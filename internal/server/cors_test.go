package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// corsLive 构造一份只带 CORS 字段的运行期快照 holder（模拟面板保存后的热生效配置）。
func corsLive(enabled bool, origins []string, creds bool) *livecfg.Holder {
	return livecfg.New(livecfg.Snapshot{
		CORSEnabled:          enabled,
		CORSOrigins:          origins,
		CORSAllowCredentials: creds,
	})
}

// preflight 发一个标准的 CORS 预检请求。
func preflight(h *Handler, origin, method, headers string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestCORSPreflightAllowedByDefaultLocalList 启用后，内置"本机来源"清单里的来源能通过预检。
//
// 这是本次需求的主路径：浏览器（Tauri/WebView 前端、Vite dev server 页面）直连
// 本机网关前会先发 OPTIONS，中间件必须自己应答（ServeMux 没注册 OPTIONS）。
func TestCORSPreflightAllowedByDefaultLocalList(t *testing.T) {
	for _, origin := range []string{
		"http://localhost:5173", // Vite dev server
		"http://127.0.0.1:5173",
		"http://localhost",  // 默认 80 端口
		"tauri://localhost", // Tauri v2 打包后的 WebView 来源
		"http://tauri.localhost",
		"http://[::1]:5173",
	} {
		h := NewHandler(Config{Live: corsLive(true, nil, false)})
		rec := preflight(h, origin, "POST", "authorization,content-type")
		if rec.Code != http.StatusNoContent {
			t.Errorf("origin=%s code=%d body=%s want 204", origin, rec.Code, rec.Body)
			continue
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("origin=%s ACAO=%q want %q", origin, got, origin)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("origin=%s Allow-Methods=%q want contains POST", origin, got)
		}
		// 回显客户端声明的头（浏览器按这个清单决定是否发出实际请求）。
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type" {
			t.Errorf("origin=%s Allow-Headers=%q want echo of request headers", origin, got)
		}
		if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
			t.Errorf("origin=%s Max-Age 缺失（预检会被每个请求重复触发）", origin)
		}
		// 来源回显时必须伴随 Vary: Origin，否则中间缓存会串站。
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("origin=%s Vary=%q want contains Origin", origin, got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("origin=%s Allow-Credentials=%q want empty (未开启凭据)", origin, got)
		}
	}
}

// TestCORSPreflightPublicOriginRejected 公网来源仍被拦（默认不放行任意站点）。
func TestCORSPreflightPublicOriginRejected(t *testing.T) {
	h := NewHandler(Config{Live: corsLive(true, nil, false)})
	rec := preflight(h, "https://evil.example.com", "POST", "authorization")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO=%q want empty (未放行来源不得回 CORS 头)", got)
	}
}

// TestCORSDisabledByDefault 未启用时必须与改造前的观测完全一致：
// 预检仍被拦（403 + 说明），响应里没有任何 CORS 头，裸 OPTIONS 仍是 405。
func TestCORSDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{}) // 不注入 Live：走静态兜底值，默认全零 = 关闭

	rec := preflight(h, "http://localhost:5173", "POST", "authorization")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO=%q want empty", got)
	}
	if !strings.Contains(rec.Body.String(), "cors.enabled") {
		t.Errorf("body=%q 应提示开关位置（cors.enabled）", rec.Body.String())
	}

	// 裸 OPTIONS（无 Access-Control-Request-Method）不是预检：维持 ServeMux 的 405 语义。
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("裸 OPTIONS code=%d want 405（非浏览器调用方零回归）", rec2.Code)
	}
}

// TestCORSSimpleRequestGetsHeaders 放行来源的**实际请求**（非预检）也要带上 CORS 头，
// 否则响应对浏览器不可读（典型症状：请求成功但 JS 拿到 TypeError）。
func TestCORSSimpleRequestGetsHeaders(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Live: corsLive(true, nil, false)})

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("ACAO=%q want 回显来源", got)
	}
	// 预检专属头不应污染实际响应。
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "" {
		t.Errorf("Max-Age=%q 不该出现在实际请求响应上", got)
	}
	// 业务响应体照常返回（中间件不改动下游）。
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body 不是 JSON：%v", err)
	}
	if body["service"] != ServiceName {
		t.Errorf("service=%v want %s", body["service"], ServiceName)
	}
}

// TestCORSHeadersOn401 鉴权失败也要带 CORS 头：
// 否则浏览器只看到 CORS 错误，用户根本读不到"密钥不对"这条关键信息。
func TestCORSHeadersOn401(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{
		APIKey:      "secret",
		CORSEnabled: true,
	})
	h := NewHandler(Config{Pool: p, Live: live})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("ACAO=%q want 回显来源（401 必须对浏览器可读）", got)
	}
}

// TestCORSUnallowedSimpleRequestPassesThrough 未放行来源的**普通请求**照常处理：
// 只是不加 CORS 头（浏览器会自己拦），不主动 403——避免误伤带 Origin 头的
// 非浏览器调用方（curl -H Origin / 反向代理 / 部分 SDK）。
func TestCORSUnallowedSimpleRequestPassesThrough(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Live: corsLive(true, nil, false)})

	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200（透传不拒绝）", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO=%q want empty", got)
	}
}

// TestCORSWildcardOrigin "*" 时 ACAO 必须是字面量 *，且不得带 Allow-Credentials
// （浏览器规范禁止二者共存）。
func TestCORSWildcardOrigin(t *testing.T) {
	h := NewHandler(Config{Live: corsLive(true, []string{"*"}, false)})

	rec := preflight(h, "https://any.example.com", "POST", "authorization")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s want 204", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("ACAO=%q want literal *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Allow-Credentials=%q want empty（与 * 互斥）", got)
	}
}

// TestCORSCustomOriginsAndCredentials 显式来源清单 + 凭据开关：精确匹配、回显来源并声明凭据。
func TestCORSCustomOriginsAndCredentials(t *testing.T) {
	live := corsLive(true, []string{"http://localhost:5173", "app://my-editor"}, true)
	h := NewHandler(Config{Live: live})

	rec := preflight(h, "app://my-editor", "POST", "authorization")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d body=%s want 204", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "app://my-editor" {
		t.Errorf("ACAO=%q want 回显来源", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials=%q want true", got)
	}

	// 清单外的来源（哪怕是同主机的其它端口）不放行。
	rec2 := preflight(h, "http://localhost:5174", "POST", "")
	if rec2.Code != http.StatusForbidden {
		t.Errorf("清单外来源 code=%d want 403", rec2.Code)
	}
}

// TestCORSRejectsHeaderInjection Origin 是客户端可控文本且会被回显到响应头，
// 必须拦掉含 CR/LF 的取值（防响应头注入）。
func TestCORSRejectsHeaderInjection(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Live: corsLive(true, nil, false)})

	bad := "http://localhost:5173\r\nX-Injected: 1"
	req := httptest.NewRequest("GET", "/healthz", nil)
	req.Header.Set("Origin", bad)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // 不 panic、不回显
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO=%q want empty（非法 Origin 不得回显）", got)
	}
	if got := rec.Header().Get("X-Injected"); got != "" {
		t.Fatalf("注入头落地了：X-Injected=%q", got)
	}

	// 预检侧的请求头清单同样只在校验通过时回显。
	rec2 := preflight(h, "http://localhost:5173", "POST", "authorization\r\nX-Bad: 1")
	if got := rec2.Header().Get("Access-Control-Allow-Headers"); strings.Contains(got, "X-Bad") {
		t.Fatalf("Allow-Headers=%q 不该回显非法清单", got)
	} else if got != corsAllowHeadersFallback {
		t.Errorf("Allow-Headers=%q want 回落 %q", got, corsAllowHeadersFallback)
	}
}

// TestCORSPreflightRejectsUnsupportedMethod 预检声明的方法不在放行集合时直接 403，
// 而不是 204 之后在实际请求上才失败。
func TestCORSPreflightRejectsUnsupportedMethod(t *testing.T) {
	h := NewHandler(Config{Live: corsLive(true, nil, false)})
	rec := preflight(h, "http://localhost:5173", "DELETE", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d want 403", rec.Code)
	}
}

// TestCORSPrivateNetworkPreflight Chrome 的 Private Network Access：公网页面请求本机地址时
// 预检多带该头，只有显式应答才放行（"在浏览器里用本地接口"的常见形态）。
func TestCORSPrivateNetworkPreflight(t *testing.T) {
	h := NewHandler(Config{Live: corsLive(true, nil, false)})
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Private-Network"); got != "true" {
		t.Errorf("Allow-Private-Network=%q want true", got)
	}
}

// TestCORSConfigHotReload 面板改完配置立即生效：同一个 handler 换快照后行为随之改变，
// 不需要重启进程。
func TestCORSConfigHotReload(t *testing.T) {
	live := corsLive(false, nil, false)
	h := NewHandler(Config{Live: live})

	if rec := preflight(h, "http://localhost:5173", "POST", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("关闭态 code=%d want 403", rec.Code)
	}

	live.Store(livecfg.Snapshot{CORSEnabled: true, CORSOrigins: []string{"http://localhost:5173"}})
	if rec := preflight(h, "http://localhost:5173", "POST", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("热生效后 code=%d want 204", rec.Code)
	}
}

func TestMatchOrigin(t *testing.T) {
	cases := []struct {
		pattern, origin string
		want            bool
	}{
		{"http://localhost:*", "http://localhost:5173", true},
		{"http://localhost:*", "http://localhost", false}, // 模式含冒号，无端口不匹配
		{"http://localhost:5173", "http://localhost:5173", true},
		{"http://localhost:5173", "http://localhost:5174", false},
		{"HTTP://LocalHost:5173", "http://localhost:5173", true},  // 大小写不敏感
		{"http://localhost:5173/", "http://localhost:5173", true}, // 尾斜杠归一化
		{"tauri://localhost", "tauri://localhost", true},
		{"http://127.0.0.1:*", "http://127.0.0.1:3000", true},
		{"http://127.0.0.1:*", "http://127.0.0.1.evil.com:3000", false}, // 前缀不得越界
		{"*", "https://anything.example", true},
		{"http://*.example.com:*", "http://a.example.com:8080", true},
		{"", "http://localhost", false},
		{"http://localhost", "", false},
	}
	for _, c := range cases {
		if got := MatchOrigin(c.pattern, c.origin); got != c.want {
			t.Errorf("MatchOrigin(%q, %q)=%v want %v", c.pattern, c.origin, got, c.want)
		}
	}
}
