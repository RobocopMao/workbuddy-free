// cors.go 浏览器跨域（CORS）放行中间件。
//
// 为什么需要：本网关原本只服务**非浏览器**调用方（curl、各类客户端、上游 Agent），
// 从不发 Access-Control-Allow-* 头。一旦要让**浏览器里的页面**直接调 /v1
// （本地 Tauri/WebView 前端、Vite dev server 里的页面、自建网页工具），浏览器会先发
// OPTIONS 预检——net/http 的 ServeMux 对未注册 OPTIONS 的路由直接 405，且响应无任何
// CORS 头，请求一律被拦成 "TypeError: Load failed"。CORS 是**浏览器侧**策略，
// curl / Node / Go 客户端都不执行，所以此前一直没暴露。
//
// 设计取舍：
//   - 默认关闭（config 的 cors.enabled=false）→ 中间件完全不介入，既有行为零变化。
//   - 打开后默认只放行**本机来源**（localhost / 127.0.0.1 / [::1] / Tauri 的
//     tauri://localhost，任意端口），公网页面仍然被拦——避免"开了跨域"等于
//     "把带 key 的接口交给任意网页"。
//   - 未放行的来源：预检直接 403；**普通请求照常透传**（只是不加 CORS 头）。
//     不主动拒绝普通请求，是为了不误伤"偶尔带 Origin 头的非浏览器调用方"
//     （curl -H Origin: ...、反向代理、部分 SDK）——它们不看 CORS 头，加了就是回归。
//   - 放行策略每请求从 livecfg 快照读取：面板里改完立即生效，无需重启进程。
package server

import (
	"net/http"
	"strconv"
	"strings"
)

// DefaultLocalCORSOrigins 内置的"本机来源"清单（cors.enabled=true 且 cors.origins 为空时使用）。
//
// 通配只出现在端口位（`*`），主机名/协议都写死，避免"开了跨域就等于放行任意站点"。
// 每类主机都给了「带任意端口」与「不带端口」两条：浏览器在默认端口（80/443）下发出的
// Origin **不带**端口号，只写 `http://localhost`，只配 `host:*` 会漏掉这一形态。
// Tauri v2 打包后的 WebView 来源是 tauri://localhost（macOS/Linux）或
// http(s)://tauri.localhost（Windows/Android）；Vite dev server 是 http://localhost:5173。
var DefaultLocalCORSOrigins = []string{
	"http://localhost:*",
	"https://localhost:*",
	"http://localhost",
	"https://localhost",
	"http://127.0.0.1:*",
	"https://127.0.0.1:*",
	"http://127.0.0.1",
	"https://127.0.0.1",
	"http://[::1]:*",
	"https://[::1]:*",
	"http://[::1]",
	"https://[::1]",
	"tauri://localhost",
	"http://tauri.localhost",
	"https://tauri.localhost",
}

// corsAllowMethods 预检应答里声明的可用方法。
// 只有服务真实注册的方法，避免 OPTIONS 里承诺了实际 405 的方法把问题推迟到实际请求。
const corsAllowMethods = "GET, POST, OPTIONS"

// corsAllowHeadersFallback 客户端没带 Access-Control-Request-Headers 时声明的头集合。
const corsAllowHeadersFallback = "Authorization, Content-Type"

// corsMaxAge 预检结果缓存秒数。取 10 分钟：既省掉高频聊天请求的重复预检，
// 又让"面板里改了放行清单"在可接受的时间内被浏览器重新询问。
const corsMaxAge = 600

// corsPolicy 一份解析好的放行策略（每请求从快照构造，避免跨请求缓存导致配置改动不生效）。
type corsPolicy struct {
	enabled     bool
	origins     []string // 已归一化的匹配模式；空 = 用 DefaultLocalCORSOrigins
	credentials bool
}

// patterns 返回实际参与匹配的模式清单（显式配置优先，空则回落内置本机清单）。
func (p corsPolicy) patterns() []string {
	if len(p.origins) == 0 {
		return DefaultLocalCORSOrigins
	}
	return p.origins
}

// allowAll 报告策略是否为"放行任意来源"。此时浏览器要求 ACAO 必须是字面量 *
// （不能回显具体来源，也不能与 Allow-Credentials 共存；normalize 已拦住后者组合）。
func (p corsPolicy) allowAll() bool {
	if !p.enabled {
		return false
	}
	for _, pat := range p.patterns() {
		if pat == "*" {
			return true
		}
	}
	return false
}

// allows 判断请求来源是否放行。
func (p corsPolicy) allows(origin string) bool {
	if !p.enabled || !validHeaderValue(origin) {
		return false
	}
	if p.allowAll() {
		return true
	}
	o := normalizeOriginValue(origin)
	for _, pat := range p.patterns() {
		if MatchOrigin(pat, o) {
			return true
		}
	}
	return false
}

// MatchOrigin 判断单个模式是否匹配某个来源。两边都按小写比较（来源的 scheme/host
// 大小写不敏感），模式里的 `*` 可出现在任意位置（内置清单只用它匹配端口）。
//
// 先比精确值再走通配：绝大多数请求命中精确分支，省掉一次逐字符扫描。
func MatchOrigin(pattern, origin string) bool {
	p := normalizeOriginValue(pattern)
	o := normalizeOriginValue(origin)
	if p == "" || o == "" {
		return false
	}
	if p == "*" || p == o {
		return true
	}
	if !strings.Contains(p, "*") {
		return false
	}
	return globMatch(p, o)
}

// globMatch 支持 `*`（任意长度，含空）的简单通配匹配。
// 双指针 + 回溯，O(len(p)*len(s)) 最坏、常见路径线性；正则在这里属于杀鸡用牛刀，
// 且用户配置的原始文本进不了正则引擎（避免 ReDoS / 转义疏漏）。
func globMatch(pattern, s string) bool {
	var (
		p, i int
		star = -1
		mark int
	)
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star = p
			mark = i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			// 回退到最近的 `*`，让它多吃一个字符。
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// normalizeOriginValue 归一化来源/模式：去空白、转小写、去掉尾部斜杠
// （用户常把 "http://localhost:5173/" 粘进来，浏览器发出的 Origin 没有尾斜杠）。
func normalizeOriginValue(v string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(v)), "/")
}

// validHeaderValue 拒绝含控制字符（CR/LF/TAB/NUL 等）或非 ASCII 的取值。
//
// 存在的意义是**防响应头注入**：Origin 与 Access-Control-Request-Headers 都是
// 客户端可控的原始文本，会被回显进响应头。Go 的 net/http 在写 header 时不会替我们
// 校验（只有 server 端解析请求头时才查），所以在回显前自己拦一道。
func validHeaderValue(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// validHeaderList 校验回显的请求头清单：只允许 token 字符与逗号/空格分隔。
func validHeaderList(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == ',', c == ' ', c == '.', c == '*':
		default:
			return false
		}
	}
	return true
}

// corsPolicyFor 从当前运行期快照取一份 CORS 策略。
func (h *Handler) corsPolicyFor() corsPolicy {
	s := h.loadLive()
	return corsPolicy{
		enabled:     s.CORSEnabled,
		origins:     s.CORSOrigins,
		credentials: s.CORSAllowCredentials,
	}
}

// applyCORS 在进路由前处理跨域：命中预检则就地应答并返回 true（调用方不再进 mux）。
//
// 三种情况：
//  1. 无 Origin 头 → 非浏览器跨域请求，原样放行（零行为变化）。
//  2. CORS 未启用 / 来源未放行 → 预检答 403（让浏览器给出明确的 CORS 报错，
//     而不是让它拿到一个 405/无头响应后报含糊的 Load failed）；普通请求透传。
//  3. 其它 → 补 CORS 响应头后继续；预检就地答 204。
func (h *Handler) applyCORS(w http.ResponseWriter, r *http.Request) (handled bool) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	pol := h.corsPolicyFor()

	if !pol.allows(origin) {
		// 预检必须自己答：ServeMux 没注册 OPTIONS，落下去就是 405，浏览器只会报
		// "预检响应无效"。403 + 说明文本让问题一眼可见（含"未放行该来源"）。
		if isPreflight(r) {
			if pol.enabled {
				http.Error(w, "CORS origin not allowed: "+origin, http.StatusForbidden)
			} else {
				http.Error(w, "CORS is disabled on this gateway (config: cors.enabled)", http.StatusForbidden)
			}
			return true
		}
		return false // 普通跨域请求：透传但不加头，浏览器自行拦截（不误伤非浏览器调用方）
	}

	setCORSHeaders(w, pol, origin)

	if isPreflight(r) {
		// 请求方法不在放行集合里 → 预检失败，交给浏览器报错（比默默 204 更容易排查）。
		method := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
		if method != "" && !corsMethodAllowed(method) {
			http.Error(w, "CORS method not allowed: "+method, http.StatusForbidden)
			return true
		}
		setPreflightHeaders(w, r)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// isPreflight 判定是否为 CORS 预检请求：OPTIONS + Access-Control-Request-Method。
// 两者缺一都不算（裸 OPTIONS 维持原有的 405 语义，不改变非浏览器调用方的观测）。
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions &&
		r.Header.Get("Access-Control-Request-Method") != ""
}

// corsMethodAllowed 报告某方法是否在放行集合内。
func corsMethodAllowed(m string) bool {
	for _, x := range strings.Split(corsAllowMethods, ",") {
		if strings.TrimSpace(x) == m {
			return true
		}
	}
	return false
}

// setCORSHeaders 写入放行所需的响应头（实际请求与预检共用）。
func setCORSHeaders(w http.ResponseWriter, pol corsPolicy, origin string) {
	h := w.Header()
	// 通配策略必须回字面量 *（回显具体来源在浏览器看来是"两套策略"，
	// 且与预检缓存配合时容易让人误以为配置没生效）。
	if pol.allowAll() {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		// 来源随请求变化 → 必须 Vary: Origin，否则中间缓存会把 A 站的放行头
		// 复用给 B 站（经典缓存投毒面）。
		h.Add("Vary", "Origin")
	}
	if pol.credentials && !pol.allowAll() {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
}

// setPreflightHeaders 写入预检专属响应头（方法、请求头清单、缓存时长）。
//
// 之所以与 setCORSHeaders 分开：这几个头只该出现在预检响应上，
// 混在一起容易在后续维护中把 Max-Age 之类带到普通响应上。
func setPreflightHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", corsAllowMethods)

	// 回显客户端声明的请求头清单：客户端（如带自定义头的 SDK）会不断变，
	// 写死清单迟早漏一个；回显是 CORS 的标准做法。只在校验通过时回显，
	// 否则退回固定清单（防头注入）。
	reqHeaders := r.Header.Get("Access-Control-Request-Headers")
	if validHeaderList(reqHeaders) {
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	} else {
		h.Set("Access-Control-Allow-Headers", corsAllowHeadersFallback)
	}
	h.Set("Access-Control-Max-Age", strconv.Itoa(corsMaxAge))

	// Chrome 的 Private Network Access：公网页面（https://…）请求本机地址时，
	// 预检会多带 Access-Control-Request-Private-Network: true，只有在应答里
	// 显式声明才放行。这正是"在浏览器里用本地接口"的常见场景（本地开发页面上线后
	// 仍连本机网关），所以一并支持；来源已过白名单，不额外放宽面。
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Access-Control-Request-Private-Network")), "true") {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}
}
