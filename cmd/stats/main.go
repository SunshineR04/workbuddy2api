// stats.go — 网关请求统计（按模型）的终端视图，数据源 GET /v1/stats。
//
// 用法:
//
//	go run ./cmd/stats              # 一次性快照
//	go run ./cmd/stats -json        # JSON 透传（供脚本消费）
//	go run ./cmd/stats -watch 5s    # 原地刷新（Ctrl+C 退出）
//
// 为什么是这个形态：网关（server）才是所有流量的必经点，统计在网关侧采集；
// 本工具只做渲染 —— 无状态、无依赖、用完即退，不像常驻面板那样占内存。
//
// 配置解析与网关侧其它工具一致：读 config.json 的 listen（取端口）与 api_key；
// 可用 WB2A_URL 直接指定地址、WB2A_CONFIG 指定配置文件路径。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// modelStat 对应网关 /v1/stats 的单行统计（total 行与 models 元素同构）。
//
// 字段全部用值类型 + 零值兜底：网关在不同版本下可能缺字段（如未采集 usage 时
// tokens 全为 0），缺字段应显示为占位符而不是让整表崩掉。
type modelStat struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Failed           int64   `json:"failed"`
	Streaming        int64   `json:"streaming"`
	AvgTTFBMS        float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
	Credit           float64 `json:"credit"`
	CreditPerReq     float64 `json:"credit_per_req"`
	LastSeen         *string `json:"last_seen"`
}

// statsResponse 对应网关 /v1/stats 的完整响应。
type statsResponse struct {
	Enabled   bool        `json:"enabled"`
	Message   string      `json:"message,omitempty"`
	Since     string      `json:"since"`
	Now       string      `json:"now"`
	UptimeSec int64       `json:"uptime_sec"`
	Total     modelStat   `json:"total"`
	Models    []modelStat `json:"models"`
}

func main() {
	var (
		jsonOut  = flag.Bool("json", false, "输出原始 JSON（供脚本消费），不做格式化")
		watch    = flag.Duration("watch", 0, "原地刷新间隔（如 5s）；0 = 只取一次快照")
		timeout  = flag.Duration("timeout", 15*time.Second, "HTTP 请求超时")
		sortKey  = flag.String("sort", "requests", "按模型排序字段：requests|ttfb|tokens|credit")
		width    = flag.Int("width", 0, "按指定列宽排版（0 = 自动探测终端宽度）")
		height   = flag.Int("height", 0, "按指定行数排版（0 = 自动探测终端高度）")
		showHelp = flag.Bool("h", false, "显示帮助")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [选项]\n\n选项:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showHelp {
		flag.Usage()
		return
	}

	// -watch 与 -json 互斥：watch 会给每帧加光标控制转义，JSON 消费者无法解析；
	// 而 watch 的意义是"人看着刷新"，与机器消费本就互斥。
	if err := validateFlags(*watch, *jsonOut); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(2)
	}

	baseURL, apiKey, err := resolveGateway()
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析网关地址失败: %v\n", err)
		os.Exit(1)
	}

	// 是否具备控制台能力：Windows 需显式开启 VT，且被重定向时无法开启。
	// 不可用时 watch 回落为逐帧滚动输出（仍有完整内容，只是不覆盖）。
	tty := enableVT()

	lay := resolveLayout(*width, *height)

	if *watch > 0 {
		os.Exit(runWatch(baseURL, apiKey, *timeout, *sortKey, *watch, tty, lay))
	}

	if err := renderOnce(baseURL, apiKey, *timeout, *jsonOut, *sortKey, lay); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

// resolveLayout 决定渲染区域：显式 -width/-height 优先，否则探测终端尺寸。
//
// 探测不到（重定向/管道）时该维度保持 0，表示不限制 —— 管道场景没有"折行"
// 问题（无终端排版），应输出完整内容而不是按未知尺寸裁剪。
func resolveLayout(width, height int) layout {
	lay := layout{pinW: width > 0, pinH: height > 0, width: width, height: height}
	if lay.pinW && lay.pinH {
		return lay
	}
	cols, rows := termSize()
	if !lay.pinW {
		lay.width = usableWidth(cols)
	}
	if !lay.pinH {
		lay.height = rows
	}
	return lay
}

// validateFlags 校验参数组合。单独成函数便于测试（main 直接 os.Exit）。
func validateFlags(watch time.Duration, jsonOut bool) error {
	if watch > 0 && jsonOut {
		return fmt.Errorf("-watch 与 -json 不能同时使用（watch 用于人看，json 用于脚本消费）")
	}
	return nil
}

// resolveGateway 决定网关地址与 api_key。
//
// 优先级：WB2A_URL > config.json 的 listen/api_key。与 checkin.sh、start.ps1
// 的解析口径保持一致，避免同一台机器上两套工具的地址来源不同。
func resolveGateway() (baseURL, apiKey string, err error) {
	if u := os.Getenv("WB2A_URL"); u != "" {
		return strings.TrimRight(u, "/"), os.Getenv("WB2A_API_KEY"), nil
	}

	cfgPath := os.Getenv("WB2A_CONFIG")
	if cfgPath == "" {
		cfgPath = "config.json"
	}
	raw, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		// 无配置文件不直接失败：允许仅靠 WB2A_URL 使用；这里给出可操作的提示。
		return "", "", fmt.Errorf("读取 %s 失败（可用 WB2A_URL 直接指定网关地址）: %w", cfgPath, rerr)
	}

	var cfg struct {
		Listen string `json:"listen"`
		APIKey string `json:"api_key"`
	}
	if uerr := json.Unmarshal(raw, &cfg); uerr != nil {
		return "", "", fmt.Errorf("解析 %s 失败: %w", cfgPath, uerr)
	}

	port := 7863 // 与 config.example.json 默认一致
	if i := strings.LastIndex(cfg.Listen, ":"); i >= 0 {
		if p, perr := strconv.Atoi(strings.TrimSpace(cfg.Listen[i+1:])); perr == nil && p > 0 {
			port = p
		}
	}
	return "http://127.0.0.1:" + strconv.Itoa(port), cfg.APIKey, nil
}

// fetch 拉取并解析 /v1/stats。
func fetch(baseURL, apiKey string, timeout time.Duration) (*statsResponse, error) {
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/stats", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接网关失败（%s）: %w", baseURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		// 继续解析
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("网关拒绝鉴权（401）：检查 config.json 的 api_key 是否正确")
	case http.StatusNotFound:
		// 该端点是较新版本才有的能力，旧版网关没有这条路由。给出可操作的提示
		// 而不是干巴巴的 404。
		return nil, fmt.Errorf("网关 %s 未提供 /v1/stats（404）—— 该端点需要较新版本的网关", baseURL)
	default:
		return nil, fmt.Errorf("网关返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var out statsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &out, nil
}

func renderOnce(baseURL, apiKey string, timeout time.Duration, jsonOut bool, sortKey string, lay layout) error {
	st, err := fetch(baseURL, apiKey, timeout)
	if err != nil {
		return err
	}

	if jsonOut {
		raw, merr := json.Marshal(st)
		if merr != nil {
			return merr
		}
		fmt.Println(string(raw))
		return nil
	}

	for _, line := range buildFrame(st, sortKey, nil, lay) {
		fmt.Println(line)
	}
	return nil
}

// ─── 单表渲染 ─────────────────────────────────────────────────────────────
//
// 设计：汇总与明细合并为一张表 —— 每个模型一行，统计全量走"合计"行
// （仅多模型时出现；单模型时那一行本身就是汇总，重复展示没有信息量）。
// 失败列按需出现：全部成功时不占位，一旦有失败自动出现以引起注意。

// col 定义一列：表头、取值、对齐、显隐、让位次序。
//
// show 为 nil 表示恒显示；否则由 buildTable 依据整表数据决定是否纳入该列。
// truncate > 0 表示取值超过该显示宽度时截断（仅模型列需要，且窗口过窄时会
// 被进一步压低到 modelMinWidth）。
// keep 为 true 的列在窗口过窄时也不裁剪 —— 模型/请求/失败丢了表格就没意义；
// 其余列按 dropSeq 从小到大依次让位（数值越大越重要，越晚被裁）。
type col struct {
	head     string
	value    func(modelStat) string
	right    bool
	show     func(rows []modelStat) bool
	keep     bool
	truncate int
	dropSeq  int
}

// modelMinWidth 是模型列可被压缩到的下限（显示列）。
// 再窄就连 "cn:deepseek…" 这类前缀都区分不出，不如不压。
const modelMinWidth = 12

// modelNameMaxWidth 是模型列在窗口充裕时的最大宽度（显示列）。
const modelNameMaxWidth = 28

// columns 返回列定义。顺序即渲染顺序；modelBudget 为模型列的截断宽度。
//
// 失败列用 show 回调：只要任一行的失败数非 0（含合计行）才纳入，
// 避免常态下白白占 4 列宽度。
//
// keep / dropSeq 决定窗口过窄时的让位次序：keep 列永不裁剪（模型、请求、失败
// 丢了表格就失去意义），其余列按 dropSeq 从小到大依次让位 —— 序小者先走。
// 排序依据是"信息价值 ÷ 宽度"：缓存命中、输入/输出 这类既宽又偏诊断性的先让位；
// 扣费（等于钱）虽窄也留到最后。
func columns(modelBudget int) []col {
	return []col{
		{head: "模型", keep: true, truncate: modelBudget, value: func(m modelStat) string {
			return truncateWidth(m.Model, modelBudget)
		}},
		{head: "请求", right: true, keep: true, value: func(m modelStat) string { return fmtInt(m.Requests) }},
		{head: "失败", right: true, keep: true, value: func(m modelStat) string { return fmtInt(m.Failed) },
			show: func(rows []modelStat) bool {
				for _, m := range rows {
					if m.Failed > 0 {
						return true
					}
				}
				return false
			}},
		{head: "首字", right: true, dropSeq: 5, value: func(m modelStat) string { return fmtMillis(m.AvgTTFBMS) }},
		{head: "耗时", right: true, dropSeq: 4, value: func(m modelStat) string { return fmtMillis(m.AvgLatencyMS) }},
		{head: "吞吐", right: true, dropSeq: 3, value: func(m modelStat) string { return fmtRate(m.TokensPerSec) }},
		{head: "输入/输出", right: true, dropSeq: 2, value: func(m modelStat) string {
			// 无 usage 观测时（无 token 数据）显示占位符，不显示 0/0 误导。
			if m.PromptTokens == 0 && m.CompletionTokens == 0 {
				return "-"
			}
			return fmtTokens(m.PromptTokens) + "/" + fmtTokens(m.CompletionTokens)
		}},
		{head: "缓存命中", right: true, dropSeq: 1, value: func(m modelStat) string {
			if m.CacheHitTokens+m.CacheMissTokens == 0 {
				return "-"
			}
			return fmt.Sprintf("%.1f%%", m.CacheHitRate*100)
		}},
		{head: "扣费", right: true, dropSeq: 6, value: func(m modelStat) string {
			return fmt.Sprintf("%.2f", m.Credit)
		}},
	}
}

// buildTable 依据数据渲染表格主体（表头 + 分隔线 + 各行 + 可选合计行）。
//
// 分隔符全部用 ASCII（| - +），不用 ─ 之类的制表符：U+2500 的 East Asian Width
// 属性是 Ambiguous —— 终端可按 1 或 2 列渲染，于是分割线与表格宽度对不上
// （中文环境常按 2 列画）。ASCII 恒占 1 列，宽度确定、不随终端而异。
//
// 对齐靠构造而非估算：每列统一是「空格 + 内容 + 空格」，段间由 | 连接，
// 故 | 与分隔线上的 + 必然落在同一列。
//
//	行：   ' a | bb | c '
//	分隔： '--- + ---- + ---'
//
// 列宽取表头与该列所有取值中的最大显示宽度（CJK 按 2 列计），因此中文表头
// 不会把后续列挤偏。
//
// maxWidth 为可用显示宽度（<=0 表示不限）：超宽时先按 dropSeq 淘汰最不重要的列，
// 再压缩模型列，直到放得下。窗口过窄时必须裁剪 —— 一旦表格宽于窗口，终端会把
// 一行折成两行，watch 的"上移 N 行"就会错位并堆叠。
//
// maxRows 为表格可用的总行数（<=0 表示不限，含表头/分隔线/合计行）：超出时保留
// 头部若干行，其余折叠为一行"…另有 N 个模型"提示。高度同样必须约束 —— 帧高于
// 窗口时首行会被顶出可视区，watch 回不到帧首。
func buildTable(rows []modelStat, total modelStat, sortKey string, maxWidth, maxRows int) []string {
	// 排序后的明细（合计行不参与排序，永远置底）。
	body := make([]modelStat, len(rows))
	copy(body, rows)
	sortModels(body, sortKey)

	withTotal := len(body) > 1
	const totalLabel = "合计"

	// 高度裁剪：表头+分隔线+（合计时再多一条分隔线与合计行）是固定开销，
	// 余下才是明细可用的行数。折叠提示本身要占一行，故预留。
	folded := 0
	if maxRows > 0 {
		fixed := 2 // 表头 + 分隔线
		if withTotal {
			fixed += 2 // 分隔线 + 合计行
		}
		avail := maxRows - fixed
		if avail < 1 {
			avail = 1
		}
		if len(body) > avail {
			// avail 行里最后一行让给折叠提示（"…另有 N 个"），故明细留 avail-1 行；
			// 只够一行时优先显示明细本身，提示让位。
			keep := avail - 1
			if keep < 1 {
				keep = 1
			}
			folded = len(body) - keep
			body = body[:keep]
		}
	}

	// 失败列的显隐要考虑合计行：明细全 0 但合计非 0 在数学上不可能，
	// 但显式纳入可让边界（如部分模型缺数据）行为可预期。
	all := columns(modelNameMaxWidth)
	probe := append(append([]modelStat(nil), body...), total)
	if withTotal {
		probe = append(probe, total)
	}

	vis := make([]col, 0, len(all))
	for _, c := range all {
		if c.show == nil || c.show(probe) {
			vis = append(vis, c)
		}
	}

	// 让位：先按 dropSeq 淘汰非 keep 列（序小者先走），再压缩模型列。
	// 每轮重新测量 —— 淘汰一列后其余列宽可能因数据分布变化而变。
	// 迭代次数有上限，不会因测量不下而空转。
	for {
		widths := measureCols(vis, body, total, withTotal, totalLabel)
		if maxWidth <= 0 || tableWidth(widths) <= maxWidth {
			out := renderTable(vis, widths, body, total, withTotal, totalLabel, folded)
			return out
		}

		// 找 dropSeq 最小的非 keep 列淘汰。
		victim, minSeq := -1, 0
		for i, c := range vis {
			if c.keep {
				continue
			}
			if victim < 0 || c.dropSeq < minSeq {
				victim, minSeq = i, c.dropSeq
			}
		}
		if victim >= 0 {
			vis = append(vis[:victim], vis[victim+1:]...)
			continue
		}

		// 没有可淘汰的列了，只能压缩模型列（下限 modelMinWidth）。
		if vis[0].truncate > modelMinWidth {
			next := vis[0].truncate - 2
			if next < modelMinWidth {
				next = modelMinWidth
			}
			vis[0] = columns(next)[0]
			continue
		}

		// 已压到极限仍放不下：按当前宽度渲染，由调用方兜底（回落滚动输出）。
		return renderTable(vis, widths, body, total, withTotal, totalLabel, folded)
	}
}

// measureCols 计算各列的显示宽度（表头与所有取值的最大值）。
//
// 模型列额外受 truncate 约束：取值已在 columns 里按该宽度截断，故其宽度
// 自然不超过它。
func measureCols(vis []col, body []modelStat, total modelStat, withTotal bool, totalLabel string) []int {
	widths := make([]int, len(vis))
	for i, c := range vis {
		widths[i] = displayWidth(c.head)
	}
	measure := func(m modelStat) {
		for i, c := range vis {
			if w := displayWidth(c.value(m)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	for _, m := range body {
		measure(m)
	}
	if withTotal {
		measure(total)
		if w := displayWidth(totalLabel); w > widths[0] {
			widths[0] = w
		}
	}
	return widths
}

// tableWidth 返回按 widths 渲染出的表格行总宽（显示列）。
//
// 构成：行首空格 + 各列内容 + 列间 " | " + 行尾空格。与 renderTable 的
// 构造规则一一对应，故两者不会各算各的。
func tableWidth(widths []int) int {
	total := 2 // 行首、行尾各一个空格
	for _, w := range widths {
		total += w
	}
	return total + 3*(len(widths)-1) // 每两个相邻列之间 3 列
}

// renderTable 按已定列宽渲染表格。folded > 0 时在明细后插入一行折叠提示
// （说明还有多少模型未显示），使"被裁剪"这件事本身是可见的，而非静默丢数据。
func renderTable(vis []col, widths []int, body []modelStat, total modelStat, withTotal bool, totalLabel string, folded int) []string {
	// 单元格：首列左对齐（模型名），数字列右对齐。
	cell := func(s string, i int) string {
		if vis[i].right {
			return padLeft(s, widths[i])
		}
		return padRight(s, widths[i])
	}

	// 数据行：' ' + cell + ' ' 组成一段，段间以 " | " 相连，行首行尾各留一空格。
	renderRow := func(label string, m modelStat, useLabel bool) string {
		var b strings.Builder
		b.WriteByte(' ')
		for i := range vis {
			if i > 0 {
				b.WriteString(" | ")
			}
			s := vis[i].value(m)
			if i == 0 && useLabel {
				s = label
			}
			b.WriteString(cell(s, i))
		}
		b.WriteByte(' ')
		return b.String()
	}

	// 表头行：与数据行同构，保证竖线逐列同位。
	var head strings.Builder
	head.WriteByte(' ')
	for i, c := range vis {
		if i > 0 {
			head.WriteString(" | ")
		}
		head.WriteString(cell(c.head, i))
	}
	head.WriteByte(' ')

	// 分隔线：逐字符与表头/数据行对齐 ——
	// 行首空格→'-'，每段内容→'-'×列宽，两侧空格与段间 " | "→'-'，
	// 竖线位置→'+'。直接按同构规则生成，故长度必然与数据行相等。
	var sep strings.Builder
	sep.WriteString("-") // 对应表头的行首空格
	for i := range vis {
		if i > 0 {
			sep.WriteString("-+-") // 对应 " | "
		}
		sep.WriteString(strings.Repeat("-", widths[i]))
	}
	sep.WriteString("-") // 对应表头的行尾空格

	out := []string{head.String(), sep.String()}
	for _, m := range body {
		out = append(out, renderRow("", m, false))
	}
	if folded > 0 {
		// 折叠提示整行左对齐铺满，不参与列对齐 —— 它不属于数据列。
		out = append(out, fitLine(fmt.Sprintf(" …另有 %d 个模型未显示（窗口过矮，可放大或去掉 -watch）", folded), tableWidth(widths)))
	}
	if withTotal {
		out = append(out, sep.String(), renderRow(totalLabel, total, true))
	}
	return out
}

// frameRightMargin 是帧右边缘保留的空列数。
//
// 故意占满终端最后一列会踩到"待折行"（pending wrap）语义：写入末列后终端置位，
// 紧随其后的擦除/换行序列可能被当成折行，使整帧下移一格并逐帧累积 —— 正是本
// 次要修的这类错位。留一列即可规避，代价仅是少一列可用宽度。
const frameRightMargin = 1

// usableWidth 把探测到的终端宽度换算为可安全使用的排版宽度。
//
// 0（未知/不限）原样返回；过窄时保底 1 列，避免出现负数宽度。
func usableWidth(termCols int) int {
	if termCols <= 0 {
		return 0
	}
	if w := termCols - frameRightMargin; w > 0 {
		return w
	}
	return 1
}

// layout 是渲染可用的显示区域（显示列 × 行）；任一为 0 表示该维度不限。
//
// 为什么要限宽度：帧一旦宽于终端，终端会把一行折成两行，watch 的"上移 N 行"
// 就回不到帧首（实际占用行数多于逻辑行数），于是每帧都在旧内容下方再画一份 ——
// 窄窗口/分屏下的典型症状。
//
// pinW/pinH 标记该维度来自 -width/-height 而非自动探测：显式指定的值不随
// 终端窗口变化（watch 每轮重新探测时分屏拖动会改变窗口，自动值要跟着变）。
type layout struct {
	width, height int
	pinW, pinH    bool
}

// buildFrame 组装完整帧（标题 + 表格 + 尾注），返回逐行切片。
//
// watch 与一次性快照共用此函数：两者内容完全一致，只是输出方式不同
// （覆盖重绘 vs 顺序打印）。fetchErr 非 nil 时在帧内展示错误 —— watch 模式下
// 错误若直接写 stderr 会冲乱已绘制的帧，且帧行数失配会导致覆盖错位。
//
// lay 限定渲染区域：表格会先按宽度裁剪（淘汰次要列 → 压缩模型列），再按高度
// 裁剪（多余的模型行折叠为一行提示），保证产出的帧不超出终端。
func buildFrame(st *statsResponse, sortKey string, fetchErr error, lay layout) []string {
	var lines []string

	// 标题说明统计口径：这里的时间是**统计窗口**，不是进程 uptime。
	//
	// since 持久化在网关的 metrics.json 里、跨重启保留（只在 reset 或删文件时
	// 重置），因此进程重启后窗口仍从原起点续算。若写成"运行 XXh"会被误读为
	// 进程已运行多久，故同时给出窗口时长与起始时刻，避免歧义。
	title := "📈 网关请求统计"
	if st != nil && st.UptimeSec > 0 {
		win := "窗口 " + humanDuration(time.Duration(st.UptimeSec)*time.Second)
		if t, err := time.Parse(time.RFC3339, st.Since); err == nil {
			win += "（自 " + t.Local().Format("01-02 15:04") + "）"
		}
		title += " · " + win
	}
	lines = append(lines, fitLine(title, lay.width))

	// 分隔线一律用 ASCII '-'：'─'(U+2500) 的 East Asian Width 是 Ambiguous，
	// 终端可能按 2 列渲染，导致与表格对不齐。ASCII 恒占 1 列。
	//
	// 表格存在时用表格宽度（视觉上与表格连成一体）；否则退回标题宽度，
	// 保证错误/未启用/无数据这些短帧也有一条与内容相称的横线。
	var table []string
	if fetchErr == nil && st != nil && st.Enabled && len(st.Models) > 0 {
		// 高度预算：标题、分隔线、尾注各占一行；宽度受限时可能还要多一行裁剪
		// 提示，一并预留 —— 帧比窗口矮无妨，比窗口高就必然错位。
		maxRows := 0
		if lay.height > 0 {
			maxRows = lay.height - 3
			if lay.width > 0 {
				maxRows--
			}
			if maxRows < 3 {
				maxRows = 3 // 表头+分隔线+至少一行明细
			}
		}
		table = buildTable(st.Models, st.Total, sortKey, lay.width, maxRows)
	}

	ruleWidth := displayWidth(title)
	if len(table) > 0 {
		ruleWidth = displayWidth(table[0])
	}
	if lay.width > 0 && ruleWidth > lay.width {
		ruleWidth = lay.width
	}
	rule := strings.Repeat("-", ruleWidth)

	if fetchErr != nil {
		// 帧内报错：保留标题与分隔线（帧结构稳定），错误信息作为表体。
		return append(lines, rule, fitLine("⚠ "+fetchErr.Error(), lay.width))
	}

	if !st.Enabled {
		msg := st.Message
		if msg == "" {
			msg = "请在网关配置中设置 server.metrics_enabled=true"
		}
		return append(lines, rule, "⚠ 网关未启用请求统计", fitLine("  "+msg, lay.width))
	}

	if len(st.Models) == 0 {
		return append(lines, rule, fitLine("暂无数据 —— 统计窗口内没有请求记录（-json 可取原始字段）", lay.width))
	}

	lines = append(lines, rule)
	lines = append(lines, table...)
	return append(lines, fitLine("扣费单位=账号积分（非货币）· 完整字段见 -json", lay.width))
}

// fitLine 把单行文本裁到不超过 width 显示列（width<=0 表示不限）。
//
// 用于标题、尾注、错误行这些不参与列对齐的行：它们超宽同样会折行并破坏
// watch 的光标上移量，故一并裁剪。
func fitLine(s string, width int) string {
	if width <= 0 {
		return s
	}
	return truncateWidth(s, width)
}

// ─── 输出：一次性 vs 原地刷新 ─────────────────────────────────────────────

// runWatch 原地刷新循环：每帧重绘覆盖上一帧。
//
// 覆盖方式不用全屏清屏（\033[2J）—— 那会闪屏且清掉滚动缓冲。改为相对移动
// 光标回到帧首 + 逐行擦到行尾 + 帧尾清除多余旧行。tty 为 false（被重定向/
// 无控制台）时回落为顺序打印，保证管道用法可读。
//
// 尺寸每轮重新探测：终端窗口（尤其分屏）随时可能被拖动改变，用旧尺寸排版会
// 让帧再次超出可视区。
func runWatch(baseURL, apiKey string, timeout time.Duration, sortKey string, interval time.Duration, tty bool, lay layout) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if tty {
		fmt.Print("\033[?25l")       // 隐藏光标：避免其在重绘时跳动
		defer fmt.Print("\033[?25h") // 恢复光标
	}

	prevLines := 0
	overlap := true // 上一帧是否以"原地覆盖"方式绘制（决定本轮能否上移光标）
	for {
		// 拉取失败且没有可用数据时，错误进帧内展示（而非 stderr），
		// 保证帧结构的行数稳定、覆盖重绘不错位。
		st, err := fetch(baseURL, apiKey, timeout)
		var frame []string
		if err != nil {
			frame = buildFrame(&statsResponse{}, sortKey, err, lay)
		} else {
			frame = buildFrame(st, sortKey, nil, lay)
		}

		// 兜底：帧必须严格放得进窗口。若仍超出（窗口窄于最简表的固有宽度，
		// 例如只有十几列），原地覆盖必然错位 —— 此时退回滚动输出，宁可刷屏
		// 也不要内容互相覆盖成乱码。
		fits := frameFits(frame, lay)
		if tty && fits {
			rewriteFrame(frame, prevLines, overlap)
		} else {
			// 回落：顺序打印，帧间空行分隔。用 prevLines=0 通知下一帧不要上移
			// 光标 —— 本轮内容是被推上去的，位置与上一帧的记账不符。
			for _, l := range frame {
				fmt.Println(l)
			}
			fmt.Println()
			overlap = false
		}
		prevLines = len(frame)
		if tty && fits {
			overlap = true
		}

		select {
		case <-ctx.Done():
			if tty && overlap {
				// 退出前把光标移到帧尾下方，避免提示符接在帧内容后面。
				fmt.Printf("\033[%dB\n", 1)
			}
			return 0
		case <-time.After(interval):
			// 尺寸可能已变（用户拖动分屏）：重新探测，仅覆盖未被显式指定的维度。
			lay = refreshLayout(lay)
		}
	}
}

// frameFits 报告帧是否放得进渲染区域（任一无约束时该维度视为通过）。
//
// 宽度是关键：只要有一行宽于窗口，终端就会折行，帧的实际占用行数多于
// len(frame)，"上移 N 行"随即失准并导致堆叠。
func frameFits(frame []string, lay layout) bool {
	if lay.width > 0 {
		for _, l := range frame {
			if displayWidth(l) > lay.width {
				return false
			}
		}
	}
	return lay.height <= 0 || len(frame) <= lay.height
}

// refreshLayout 重新探测终端尺寸，仅更新"自动"维度（-width/-height 指定的保持不变）。
//
// 每轮都重新探测：分屏拖动会改变窗口尺寸，沿用旧尺寸排版会让帧重新超出可视区，
// 于是又回到"折行 → 光标上移失准 → 堆叠"的老路。
func refreshLayout(lay layout) layout {
	cols, rows := termSize()
	if cols > 0 && !lay.pinW {
		lay.width = usableWidth(cols)
	}
	if rows > 0 && !lay.pinH {
		lay.height = rows
	}
	return lay
}

// rewriteFrame 覆盖重绘：回到帧首 → 逐行擦除写入 → 清除多余旧行。
//
// 帧内每行以 \r\n 结尾，故写完 M 行后光标停在第 M+1 行行首。下一帧据此上移
// prevLines 行即可精确回到帧首；帧变短时多余旧行由帧尾的 \033[J 擦除。
//
// overlap 为 false 表示上一帧不是原地覆盖绘制的（走了滚动回落，内容已被
// 推入滚动缓冲），此时不能上移光标 —— 那会吃掉与此帧无关的既有输出。
func rewriteFrame(lines []string, prevLines int, overlap bool) {
	var b strings.Builder
	if overlap && prevLines > 0 {
		// 光标上移 prevLines 行，回到上一帧起点（列 0）。
		fmt.Fprintf(&b, "\033[%dA", prevLines)
	}
	b.WriteString("\r")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\033[K") // 擦到行尾：覆盖时清掉上一帧更长的残留
		b.WriteString("\r\n")
	}
	// 光标现在位于新帧下方一行的行首：此处到屏幕末尾的内容都是上一帧的残留
	// （帧变短时）或空白，一并擦除。不移动光标，下一帧的上移量才与行数吻合。
	b.WriteString("\033[J")
	_, _ = os.Stdout.WriteString(b.String())
}

// ─── CJK 宽度感知的补齐与截断 ─────────────────────────────────────────────
//
// Go 的 fmt 宽度按 rune 计数，而中文/全角字符占 2 个显示列，直接用
// %-10s 会让含中文的行比其他行窄，表头与数据逐列错位。故自行按显示宽度补齐。

// displayWidth 返回字符串在等宽终端中占用的显示列数。
//
// 判定：东亚宽度为 Wide/Fullwidth 的字符（CJK 汉字、全角标点、全角字母等）
// 占 2 列；其余（含 ASCII、Latin-1、半角片假名）占 1 列。
// 组合附加符（零宽）与 emoji 的精确宽度因终端而异，此处按 1 列近似 —— 本工具
// 只在标题用 emoji（左对齐、不参与列对齐），因此不影响表格对齐。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 返回单个 rune 的显示宽度（1 或 2）。
func runeWidth(r rune) int {
	// 零宽：组合附加符号、变体选择符。
	if r == 0x200B || (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x0300 && r <= 0x036F) {
		return 0
	}
	if isWide(r) {
		return 2
	}
	return 1
}

// isWide 报告 rune 是否属于东亚"宽/全角"区间（占 2 列）。
//
// 区间依据 Unicode East Asian Width 属性的 W 与 F 类，覆盖常用范围；
// 不追求穷尽所有生僻区块（模型名与表头都用不到），但覆盖 CJK、全角标点、
// 谚文、假名、以及常见 symbol 区块。
func isWide(r rune) bool {
	switch {
	case r < 0x1100:
		return false // ASCII 与拉丁扩展：全部窄
	}
	return (r >= 0x1100 && r <= 0x115F) || // 谚文字母
		r == 0x2329 || r == 0x232A || // 〈 〉
		(r >= 0x2E80 && r <= 0x303E) || // CJK 部首、康熙部首、CJK 符号标点
		(r >= 0x3041 && r <= 0x33FF) || // 平假名、片假名、CJK 兼容、注音
		(r >= 0x3400 && r <= 0x4DBF) || // CJK 扩展 A
		(r >= 0x4E00 && r <= 0x9FFF) || // CJK 基本区
		(r >= 0xA000 && r <= 0xA4CF) || // 彝文
		(r >= 0xAC00 && r <= 0xD7A3) || // 谚文音节
		(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容表意文字
		(r >= 0xFE30 && r <= 0xFE6F) || // CJK 兼容形式、小写变体
		(r >= 0xFF00 && r <= 0xFF60) || // 全角 ASCII
		(r >= 0xFFE0 && r <= 0xFFE6) || // 全角符号
		(r >= 0x1F300 && r <= 0x1F64F) || // 杂项符号与绘文字（近似按 2 列）
		(r >= 0x1F900 && r <= 0x1F9FF) || // 补充符号与绘文字
		(r >= 0x20000 && r <= 0x3FFFD) // CJK 扩展 B 及以后
}

// padRight 左对齐补齐到 width 显示列。
func padRight(s string, width int) string {
	if d := width - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeft 右对齐补齐到 width 显示列。
func padLeft(s string, width int) string {
	if d := width - displayWidth(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// truncateWidth 把字符串截断到不超过 max 显示列，超出加省略号。
// 中文按 2 列计，因此同一 max 下中文能放的字符数比英文少一半。
func truncateWidth(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	const ellipsis = "…" // 占 2 列
	budget := max - displayWidth(ellipsis)
	var b strings.Builder
	used := 0
	for _, r := range s {
		w := runeWidth(r)
		if used+w > budget {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + ellipsis
}

// sortModels 按 sortKey 降序排列；无法识别时退回请求数。
func sortModels(rows []modelStat, sortKey string) {
	less := func(i, j int) bool { return rows[i].Requests > rows[j].Requests }
	switch sortKey {
	case "ttfb":
		less = func(i, j int) bool { return rows[i].AvgTTFBMS > rows[j].AvgTTFBMS }
	case "tokens":
		less = func(i, j int) bool { return rows[i].TotalTokens > rows[j].TotalTokens }
	case "credit":
		less = func(i, j int) bool { return rows[i].Credit > rows[j].Credit }
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if less(i, j) {
			return true
		}
		if less(j, i) {
			return false
		}
		return rows[i].Model < rows[j].Model // 同值时按模型名稳定排列
	})
}

// ─── 格式化辅助 ───────────────────────────────────────────────────────────

// fmtInt 千分位整数。
func fmtInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtTokens token 数按量级缩写（1.2M / 865.8K），便于一屏阅读。
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// fmtMillis 毫秒转可读时长：<1s 显示毫秒，否则显示秒。
func fmtMillis(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000)
}

// fmtRate 吞吐率。
func fmtRate(v float64) string {
	if v <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f tok/s", v)
}

func humanDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
