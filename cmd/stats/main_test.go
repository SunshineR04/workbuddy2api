package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── resolveGateway：地址与密钥解析 ──────────────────────────────────────

// TestResolveGatewayFromConfig 从 config.json 取端口与 api_key（主路径）。
func TestResolveGatewayFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"listen":"127.0.0.1:7999","api_key":"sk-abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WB2A_CONFIG", cfg)
	t.Setenv("WB2A_URL", "")

	base, key, err := resolveGateway()
	if err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	if base != "http://127.0.0.1:7999" {
		t.Errorf("baseURL = %q, want http://127.0.0.1:7999", base)
	}
	if key != "sk-abc" {
		t.Errorf("apiKey = %q, want sk-abc", key)
	}
}

// TestResolveGatewayListenForms listen 的几种写法都要能取出端口。
func TestResolveGatewayListenForms(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{`{"listen":":7863"}`, "http://127.0.0.1:7863"},
		{`{"listen":"0.0.0.0:8080"}`, "http://127.0.0.1:8080"},
		{`{"listen":"127.0.0.1:9000"}`, "http://127.0.0.1:9000"},
		{`{"listen":"bad"}`, "http://127.0.0.1:7863"}, // 无法解析 → 默认端口
		{`{}`, "http://127.0.0.1:7863"},               // 缺字段 → 默认端口
	}
	for _, c := range cases {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.json")
		_ = os.WriteFile(cfg, []byte(c.listen), 0o600)
		t.Setenv("WB2A_CONFIG", cfg)
		t.Setenv("WB2A_URL", "")

		base, _, err := resolveGateway()
		if err != nil {
			t.Fatalf("listen=%s: %v", c.listen, err)
		}
		if base != c.want {
			t.Errorf("listen=%s → %q, want %q", c.listen, base, c.want)
		}
	}
}

// TestResolveGatewayURLOverride WB2A_URL 优先于配置文件。
func TestResolveGatewayURLOverride(t *testing.T) {
	t.Setenv("WB2A_URL", "http://example.com:1234/") // 带尾斜杠应被去掉
	t.Setenv("WB2A_API_KEY", "sk-env")
	t.Setenv("WB2A_CONFIG", "/nonexistent/config.json")

	base, key, err := resolveGateway()
	if err != nil {
		t.Fatalf("resolveGateway: %v", err)
	}
	if base != "http://example.com:1234" {
		t.Errorf("baseURL = %q", base)
	}
	if key != "sk-env" {
		t.Errorf("apiKey = %q, want sk-env", key)
	}
}

// TestResolveGatewayMissingConfig 配置文件缺失且无 WB2A_URL 时报错，
// 且错误信息要指明可用 WB2A_URL（否则用户不知如何绕过）。
func TestResolveGatewayMissingConfig(t *testing.T) {
	t.Setenv("WB2A_URL", "")
	t.Setenv("WB2A_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	_, _, err := resolveGateway()
	if err == nil {
		t.Fatal("配置缺失应报错")
	}
	if !strings.Contains(err.Error(), "WB2A_URL") {
		t.Errorf("错误信息应提示 WB2A_URL，得到: %v", err)
	}
}

// TestResolveGatewayBadJSON 配置文件非法 JSON 时报错而非静默用默认值。
func TestResolveGatewayBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte(`{not json`), 0o600)
	t.Setenv("WB2A_URL", "")
	t.Setenv("WB2A_CONFIG", cfg)
	if _, _, err := resolveGateway(); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// ─── fetch：端点各状态处理 ───────────────────────────────────────────────

func TestFetchOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/stats" {
			t.Errorf("请求路径 = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"enabled":true,"uptime_sec":60,"total":{"requests":5},
		                        "models":[{"model":"m1","requests":5}]}`))
	}))
	defer srv.Close()

	st, err := fetch(srv.URL, "k", 5*time.Second)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !st.Enabled || st.Total.Requests != 5 || len(st.Models) != 1 {
		t.Errorf("解析结果异常: %+v", st)
	}
}

// TestFetchNotFound 旧版网关无该端点：错误信息要能指导用户，而不是裸 404。
func TestFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "k", 5*time.Second)
	if err == nil {
		t.Fatal("404 应报错")
	}
	if !strings.Contains(err.Error(), "/v1/stats") {
		t.Errorf("错误应点明缺失端点，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "较新版本") {
		t.Errorf("错误应给出可操作指引，得到: %v", err)
	}
}

// TestFetchUnauthorized 401 提示检查 api_key。
func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "bad", 5*time.Second)
	if err == nil {
		t.Fatal("401 应报错")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("错误应提示 api_key，得到: %v", err)
	}
}

// TestFetchServerError 500 等仍报错并带响应片段。
func TestFetchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := fetch(srv.URL, "k", 5*time.Second)
	if err == nil {
		t.Fatal("500 应报错")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("错误应含状态码，得到: %v", err)
	}
}

// TestFetchConnectionError 网关未启动时错误信息应含地址，便于判断连错对象。
func TestFetchConnectionError(t *testing.T) {
	_, err := fetch("http://127.0.0.1:1", "k", 2*time.Second)
	if err == nil {
		t.Fatal("连接失败应报错")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("错误应含目标地址，得到: %v", err)
	}
}

// ─── 渲染：字段缺失与未启用 ───────────────────────────────────────────────

// TestRenderDisabled 网关未启用统计时给出配置指引。
func TestRenderDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":false,"message":"统计未启用（server.metrics_enabled=false）"}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, false, "requests"); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	if !strings.Contains(out, "未启用") {
		t.Errorf("应提示未启用，得到: %s", out)
	}
	if !strings.Contains(out, "metrics_enabled") {
		t.Errorf("应保留网关原始说明，得到: %s", out)
	}
}

// TestRenderZeroFields 全零统计（如网关刚重启、尚无请求）不应崩溃或输出 NaN。
// 这是真实会遇到的边界：进程刚起、或统计被 reset 之后。
func TestRenderZeroFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"enabled":true,"total":{},"models":[]}`))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, false, "requests"); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	if strings.Contains(out, "NaN") || strings.Contains(out, "+Inf") {
		t.Errorf("零值不应产生 NaN/Inf，得到: %s", out)
	}
	if !strings.Contains(out, "总请求") {
		t.Errorf("应仍有基本框架，得到: %s", out)
	}
}

// TestRenderJSONPassesThrough -json 输出可被再次解析，且字段完整。
func TestRenderJSONPassesThrough(t *testing.T) {
	payload := `{"enabled":true,"uptime_sec":60,"total":{"model":"(all)","requests":7,
	            "cache_hit_rate":0.5},"models":[{"model":"a","requests":7}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		if err := renderOnce(srv.URL, "k", 5*time.Second, true, "requests"); err != nil {
			t.Errorf("renderOnce: %v", err)
		}
	})
	var got statsResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("-json 输出应为合法 JSON: %v\n%s", err, out)
	}
	if !got.Enabled || got.Total.Requests != 7 || len(got.Models) != 1 {
		t.Errorf("JSON 字段丢失: %+v", got)
	}
}

// TestSortModels 各排序键都应生效，且同值时不丢行。
func TestSortModels(t *testing.T) {
	rows := []modelStat{
		{Model: "a", Requests: 1, AvgTTFBMS: 300, TotalTokens: 10, Credit: 5},
		{Model: "b", Requests: 9, AvgTTFBMS: 100, TotalTokens: 30, Credit: 1},
		{Model: "c", Requests: 5, AvgTTFBMS: 200, TotalTokens: 20, Credit: 9},
	}
	cases := []struct{ key, want string }{
		{"requests", "b"},
		{"ttfb", "a"},
		{"tokens", "b"},
		{"credit", "c"},
		{"unknown", "b"}, // 未知键回落 requests
	}
	for _, c := range cases {
		rs := append([]modelStat(nil), rows...)
		sortModels(rs, c.key)
		if len(rs) != 3 {
			t.Fatalf("key=%s 排序后行数 = %d", c.key, len(rs))
		}
		if rs[0].Model != c.want {
			t.Errorf("key=%s 首行 = %s, want %s", c.key, rs[0].Model, c.want)
		}
	}
}

// ─── 格式化辅助 ───────────────────────────────────────────────────────────

func TestFormatHelpers(t *testing.T) {
	if got := fmtInt(1234567); got != "1,234,567" {
		t.Errorf("fmtInt = %q", got)
	}
	if got := fmtInt(999); got != "999" {
		t.Errorf("fmtInt = %q", got)
	}
	if got := fmtInt(-1234); got != "-1,234" {
		t.Errorf("fmtInt 负数 = %q", got)
	}
	if got := fmtTokens(25430343); got != "25.43M" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtTokens(865800); got != "865.8K" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtTokens(42); got != "42" {
		t.Errorf("fmtTokens = %q", got)
	}
	if got := fmtMillis(4280); got != "4.28s" {
		t.Errorf("fmtMillis = %q", got)
	}
	if got := fmtMillis(650); got != "650ms" {
		t.Errorf("fmtMillis = %q", got)
	}
	if got := fmtMillis(0); got != "-" {
		t.Errorf("fmtMillis(0) = %q，应为占位符", got)
	}
	if got := fmtRate(0); got != "-" {
		t.Errorf("fmtRate(0) = %q，应为占位符", got)
	}
	if got := humanDuration(81 * time.Minute); got != "1h21m" {
		t.Errorf("humanDuration = %q", got)
	}
	if got := humanDuration(45 * time.Second); got != "45s" {
		t.Errorf("humanDuration = %q", got)
	}
}

// captureStdout 捕获标准输出（被测函数直接 fmt.Printf）。
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
