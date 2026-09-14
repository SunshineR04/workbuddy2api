// stats.go — 网关请求统计（按模型）的终端视图，数据源 GET /v1/stats。
//
// 用法:
//
//	go run ./cmd/stats              # 一次性快照（人类可读）
//	go run ./cmd/stats -json        # JSON 透传（供脚本消费）
//	go run ./cmd/stats -watch 5s    # 持续刷新（Ctrl+C 退出）
//
// 为什么是这个形态：网关（server）才是所有流量的必经点，统计在网关侧采集；
// 本工具只做渲染，因此无状态、无依赖、用完即退 —— 不像常驻面板那样占内存。
//
// 配置解析与网关侧其它工具一致：读 config.json 的 listen（取端口）与 api_key；
// 可用 WB2A_URL 直接指定地址、WB2A_CONFIG 指定配置文件路径。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// modelStat 对应网关 /v1/stats 的单行统计（total 行与 models 元素同构）。
//
// 字段全部用值类型 + 零值兜底：网关在不同版本下可能缺字段（如未采集 usage 时
// tokens 全为 0），缺字段应显示为 0 而不是让整表崩掉。
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
		watch    = flag.Duration("watch", 0, "持续刷新间隔（如 5s）；0 = 只取一次快照")
		timeout  = flag.Duration("timeout", 15*time.Second, "HTTP 请求超时")
		sortKey  = flag.String("sort", "requests", "按模型排序字段：requests|ttfb|tokens|credit")
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

	baseURL, apiKey, err := resolveGateway()
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析网关地址失败: %v\n", err)
		os.Exit(1)
	}

	if *watch > 0 {
		// 首帧清屏，后续每帧回到左上角覆盖 —— 避免 watch 模式下滚动刷屏。
		for {
			fmt.Print("\033[H\033[2J")
			if err := renderOnce(baseURL, apiKey, *timeout, *jsonOut, *sortKey); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ %v\n", err)
			}
			fmt.Printf("\n刷新间隔 %s · Ctrl+C 退出\n", *watch)
			time.Sleep(*watch)
		}
	}

	if err := renderOnce(baseURL, apiKey, *timeout, *jsonOut, *sortKey); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
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

func renderOnce(baseURL, apiKey string, timeout time.Duration, jsonOut bool, sortKey string) error {
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

	if !st.Enabled {
		msg := st.Message
		if msg == "" {
			msg = "请在网关配置中设置 server.metrics_enabled=true"
		}
		fmt.Printf("⚠ 网关未启用请求统计\n  %s\n", msg)
		return nil
	}

	printReport(st, sortKey)
	return nil
}

// printReport 渲染人类可读快照。
func printReport(st *statsResponse, sortKey string) {
	t := st.Total

	fmt.Printf("📈 网关请求统计")
	if st.UptimeSec > 0 {
		fmt.Printf("          运行 %s", humanDuration(time.Duration(st.UptimeSec)*time.Second))
	}
	fmt.Println()
	fmt.Println(strings.Repeat("─", 66))

	// ── 请求量 ──
	successRate := 100.0
	if t.Requests > 0 {
		successRate = float64(t.Success) / float64(t.Requests) * 100
	}
	fmt.Printf("%-10s %-14s %-12s %s\n", "总请求", fmtInt(t.Requests),
		fmt.Sprintf("成功 %.1f%%", successRate), fmt.Sprintf("流式 %s", fmtInt(t.Streaming)))
	if t.Failed > 0 {
		fmt.Printf("%-10s %s\n", "失败", fmtInt(t.Failed))
	}

	// ── 性能 ── 首字与耗时是"有观测才有意义"的指标：无 usage 的请求不计入，
	// 全为 0 时不必展示空行。
	if t.AvgTTFBMS > 0 || t.AvgLatencyMS > 0 {
		fmt.Printf("%-10s %-14s %-12s %s\n", "平均首字", fmtMillis(t.AvgTTFBMS),
			fmt.Sprintf("耗时 %s", fmtMillis(t.AvgLatencyMS)),
			fmt.Sprintf("吞吐 %s", fmtRate(t.TokensPerSec)))
	}

	// ── token 与缓存 ──
	if t.TotalTokens > 0 {
		fmt.Printf("%-10s %-14s %s\n", "输入/输出",
			fmt.Sprintf("%s / %s", fmtTokens(t.PromptTokens), fmtTokens(t.CompletionTokens)),
			fmt.Sprintf("合计 %s", fmtTokens(t.TotalTokens)))
		fmt.Printf("%-10s %.1f%%%s\n", "缓存命中", t.CacheHitRate*100,
			fmt.Sprintf("        (命中 %s / 未命中 %s)", fmtTokens(t.CacheHitTokens), fmtTokens(t.CacheMissTokens)))
	}

	// ── 成本 ── 扣费单位是账号积分，非货币。
	if t.Credit > 0 || t.CreditPerReq > 0 {
		fmt.Printf("%-10s %-14s %s\n", "累计扣费",
			fmt.Sprintf("%.2f 积分", t.Credit),
			fmt.Sprintf("每请求 %.4f", t.CreditPerReq))
	}

	// ── 按模型 ──
	if len(st.Models) > 1 {
		fmt.Println(strings.Repeat("─", 66))
		fmt.Println("按模型")
		printModelTable(st.Models, sortKey)
	}
}

// printModelTable 渲染按模型明细表。
func printModelTable(models []modelStat, sortKey string) {
	rows := make([]modelStat, len(models))
	copy(rows, models)
	sortModels(rows, sortKey)

	fmt.Printf("  %-24s %7s %8s %10s %9s %10s\n", "模型", "请求", "首字", "吞吐", "缓存命中", "扣费")
	for _, m := range rows {
		name := m.Model
		if len(name) > 24 {
			name = name[:23] + "…"
		}
		ttfb := "-"
		if m.AvgTTFBMS > 0 {
			ttfb = fmtMillis(m.AvgTTFBMS)
		}
		tps := "-"
		if m.TokensPerSec > 0 {
			tps = fmtRate(m.TokensPerSec)
		}
		hit := "-"
		if m.CacheHitTokens+m.CacheMissTokens > 0 {
			hit = fmt.Sprintf("%.1f%%", m.CacheHitRate*100)
		}
		fmt.Printf("  %-24s %7s %8s %10s %9s %10s\n", name,
			fmtInt(m.Requests), ttfb, tps, hit, fmt.Sprintf("%.2f", m.Credit))
	}
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
