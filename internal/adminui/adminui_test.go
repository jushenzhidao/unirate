package adminui

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func newH(t *testing.T) *Handler {
	t.Helper()
	h, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func get(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// TestAssetsAreEmbedded 全部资产必须真的进了二进制。
// embed 指令写错目录时 NewHandler 不报错，只是文件数为 0 —— 界面全白，
// 而编译与 vet 都是绿的。这类静默缺失只有断言文件清单才拦得住。
func TestAssetsAreEmbedded(t *testing.T) {
	h := newH(t)
	want := []string{
		"index.html", "tokens.css", "base.css", "layout.css", "components.css",
		"overlay.css", "charts.css", "icons.svg",
		"dom.js", "api.js", "overlay.js", "charts.js", "metrics.js", "rule-fields.js",
		"monitor-kpi.js", "rules-inner-table.js", "page-login.js", "page-monitor.js",
		"page-rules.js", "page-rules-form.js", "page-biz-form.js", "page-audit.js",
		"page-config.js", "page-policy.js", "app.js",
	}
	for _, name := range want {
		body, ok := h.files[name]
		if !ok {
			t.Errorf("资产 %q 未被嵌入", name)
			continue
		}
		if len(body) == 0 {
			t.Errorf("资产 %q 为空文件", name)
		}
	}
}

// TestIndexReferencesEveryAsset index.html 必须引用每一个 css/js 资产。
//
// 拆分文件时最容易漏的一步就是忘记加 <script>/<link>：Go 测试全绿、
// 文件也确实嵌进了二进制，但浏览器从没加载它，页面在运行时才炸。
// 反向也要查：引用了不存在的文件会静默 404，同样只在运行时暴露。
func TestIndexReferencesEveryAsset(t *testing.T) {
	h := newH(t)
	index := string(h.files["index.html"])

	for name := range h.files {
		if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".css") {
			continue
		}
		if !strings.Contains(index, `"`+name+`"`) {
			t.Errorf("index.html 未引用资产 %q —— 它不会被浏览器加载", name)
		}
	}

	// 反向：index.html 引用的每个文件都必须真的存在
	refs := regexp.MustCompile(`(?:src|href)="([^"]+\.(?:js|css))"`).FindAllStringSubmatch(index, -1)
	if len(refs) == 0 {
		t.Fatal("index.html 没有引用任何 js/css")
	}
	for _, m := range refs {
		if _, ok := h.files[m[1]]; !ok {
			t.Errorf("index.html 引用了不存在的资产 %q（运行时会 404）", m[1])
		}
	}
}

// TestScriptLoadOrder 依赖必须先于消费者加载。
//
// 各模块在 IIFE 里注册全局对象，加载期不互相调用，所以顺序错了不会立刻报错 ——
// 只有等某个页面被打开、去读一个还没注册的全局对象时才炸。这类顺序约束
// 必须由测试固定住。
func TestScriptLoadOrder(t *testing.T) {
	h := newH(t)
	index := string(h.files["index.html"])
	order := map[string]int{}
	for i, m := range regexp.MustCompile(`src="([^"]+\.js)"`).FindAllStringSubmatch(index, -1) {
		order[m[1]] = i
	}

	// dom.js 提供 U（所有模块都用）；api.js 提供 API；app.js 末尾即启动，必须最后
	for _, dep := range []struct{ first, then string }{
		{"dom.js", "api.js"},
		{"dom.js", "overlay.js"},
		{"dom.js", "charts.js"},
		{"api.js", "metrics.js"},
		{"charts.js", "monitor-kpi.js"},          // MonitorKPI 用 Charts.sparkline
		{"rule-fields.js", "page-rules-form.js"}, // RulesForm 用 RuleFields
		{"overlay.js", "app.js"},                 // App 转发 Overlay.drawer
		{"page-login.js", "app.js"},              // App.bind 调 PageLogin
	} {
		a, okA := order[dep.first]
		b, okB := order[dep.then]
		if !okA || !okB {
			t.Errorf("加载顺序断言涉及未引用的文件：%s / %s", dep.first, dep.then)
			continue
		}
		if a >= b {
			t.Errorf("%s 必须先于 %s 加载（当前 %d vs %d）", dep.first, dep.then, a, b)
		}
	}

	// app.js 必须是最后一个：它在末尾直接启动应用
	last := ""
	lastIdx := -1
	for name, i := range order {
		if i > lastIdx {
			lastIdx = i
			last = name
		}
	}
	if last != "app.js" {
		t.Errorf("app.js 必须最后加载（它末尾即启动），实际最后是 %s", last)
	}
}

// TestIndexServedAtRoot 根路径必须直出控制台
func TestIndexServedAtRoot(t *testing.T) {
	h := newH(t)
	w := get(t, h, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / 期望 200，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type 应为 text/html，实际 %q", ct)
	}
	if !strings.Contains(w.Body.String(), "unirate 管理控制台") {
		t.Error("根路径未返回控制台首页")
	}
}

// TestJavaScriptContentType .js 必须是可执行的 MIME。
// 显式查表而非依赖系统 mime 注册表：Windows 上 .js 可能被注册为 text/plain，
// 那会让浏览器拒绝执行脚本、整个控制台白屏。
func TestJavaScriptContentType(t *testing.T) {
	h := newH(t)
	for _, tc := range []struct{ path, want string }{
		{"/app.js", "text/javascript"},
		{"/tokens.css", "text/css"},
		{"/icons.svg", "image/svg+xml"},
	} {
		w := get(t, h, tc.path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s 期望 200，实际 %d", tc.path, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.want) {
			t.Errorf("%s 的 Content-Type 应以 %q 开头，实际 %q", tc.path, tc.want, ct)
		}
	}
}

// TestPathTraversalRejected 任何带 "/" 或 ".." 的路径都不得读到资产目录外的内容。
// 资产是平铺的，这类请求一律落回 index，不去拼路径。
func TestPathTraversalRejected(t *testing.T) {
	h := newH(t)
	index := string(h.files["index.html"])
	for _, p := range []string{
		"/../adminui.go",
		"/../../.env",
		"/assets/../adminui.go",
		"/sub/dir/app.js",
		"/%2e%2e/adminui.go",
	} {
		w := get(t, h, p)
		body := w.Body.String()
		// 判据是「响应必须是已知资产之一」，而不是「不含某些关键字」。
		// 黑名单式断言（不含 "package adminui"）挡不住没想到的文件，
		// 白名单式断言才能真的证明读不到资产目录以外的东西。
		if w.Code == http.StatusOK && body != index {
			t.Errorf("路径 %q 返回了非 index 内容（%d 字节），可能读到了资产目录外的文件", p, len(body))
		}
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound &&
			w.Code != http.StatusMovedPermanently {
			t.Errorf("路径 %q 返回了意外状态 %d", p, w.Code)
		}
	}
}

// TestSecurityHeaders 管理面不得被嵌套、被嗅探、被搜索引擎收录
func TestSecurityHeaders(t *testing.T) {
	h := newH(t)
	w := get(t, h, "/")
	for _, tc := range []struct{ key, want string }{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "no-referrer"},
	} {
		if got := w.Header().Get(tc.key); got != tc.want {
			t.Errorf("响应头 %s 期望 %q，实际 %q", tc.key, tc.want, got)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("缺少 Content-Security-Policy")
	}
	// frame-ancestors 'none' 才是真正阻止嵌套的那条（X-Frame-Options 已过时）
	if !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP 必须含 frame-ancestors 'none'：%s", csp)
	}
	// 零外链约束：不允许任何远程源
	if strings.Contains(csp, "http://") || strings.Contains(csp, "https://") {
		t.Errorf("CSP 不得含远程源（零 CDN 约束）：%s", csp)
	}
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP 必须把 connect-src 限制在同源，禁止跨端口取数：%s", csp)
	}
}

// TestWriteMethodsRejected 静态资产只读
func TestWriteMethodsRejected(t *testing.T) {
	h := newH(t)
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(m, "/", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / 期望 405，实际 %d", m, w.Code)
		}
	}
}

// TestNoEmojiInAssets P0 硬规则：图标只能来自 sprite，不得用 emoji 作功能图标。
// 这条必须由测试守住而不是靠人肉扫描 —— 人会忘，CI 不会。
func TestNoEmojiInAssets(t *testing.T) {
	h := newH(t)
	// 范围与项目约定的扫描口径一致：表情符号、杂项符号、装饰符号、变体选择符。
	// 刻意不含箭头区（U+2190-21FF）—— 「版本 3 → 4」里的箭头是中文排版里的
	// 连接符，不是功能图标；把它判违规只会逼人把可读的文案改难读。
	emoji := regexp.MustCompile(`[\x{1F300}-\x{1F9FF}\x{2600}-\x{27BF}\x{FE00}-\x{FE0F}]`)
	for name, body := range h.files {
		if m := emoji.FindString(string(body)); m != "" {
			t.Errorf("资产 %s 含 emoji %q —— 图标必须用 icons.svg 的 symbol", name, m)
		}
	}
}

// TestNoHardcodedColors P0 硬规则：颜色一律走 CSS 变量，仅 #fff / #000 例外。
// tokens.css 是唯一的字面值定义处（设计契约的 A1 层），故排除在外。
func TestNoHardcodedColors(t *testing.T) {
	h := newH(t)
	hex := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	allowed := map[string]bool{"#fff": true, "#ffffff": true, "#000": true, "#000000": true}
	for name, body := range h.files {
		if name == "tokens.css" || name == "icons.svg" {
			continue // A1 primitive 层允许字面值；sprite 内无颜色（stroke 走 currentColor）
		}
		src := string(body)
		if strings.HasSuffix(name, ".css") {
			src = stripCSSComments(src)
		} else if strings.HasSuffix(name, ".js") {
			src = stripJSCode(src)
		}
		for _, m := range hex.FindAllString(src, -1) {
			if !allowed[strings.ToLower(m)] {
				t.Errorf("资产 %s 出现硬编码颜色 %s —— 必须改用 var(--c-*)", name, m)
			}
		}
	}
}

// TestNoForbiddenVisualPatterns P0 硬规则：无渐变、无弹跳缓动。
// 这是控制台不是营销页，渐变与弹跳都是 AI 模板味的确定性特征。
func TestNoForbiddenVisualPatterns(t *testing.T) {
	h := newH(t)
	for name, body := range h.files {
		s := string(body)
		if strings.Contains(s, "gradient(") {
			t.Errorf("资产 %s 使用了渐变 —— 禁止渐变作视觉主体", name)
		}
		// 弹跳缓动的特征是控制点为负值
		if strings.Contains(s, "cubic-bezier(0.68") || strings.Contains(s, "cubic-bezier(.68") {
			t.Errorf("资产 %s 使用了弹跳缓动 —— 界面过渡不得弹跳", name)
		}
	}
}

// TestNoInnerHTMLForBackendData XSS 防线的机械检查。
//
// audit.detail 是用户可控 JSON、biz 名与 base_url 同样可控，是最明显的注入面。
// 全部渲染必须走 textContent / createElement。唯一允许 innerHTML 的地方是
// app.js 注入静态 sprite（同源静态文件，无用户数据参与）。
func TestNoInnerHTMLForBackendData(t *testing.T) {
	h := newH(t)
	for name, body := range h.files {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		// 只看剥掉注释与字符串后的代码：注释里说明「为什么不能用 innerHTML」
		// 不是违规，真的写了赋值才是。
		code := stripJSCode(string(body))
		// 匹配真实的赋值/读取，而非出现在文字里的词
		assign := regexp.MustCompile(`\.innerHTML\s*=`)
		hits := assign.FindAllString(code, -1)
		// app.js 里唯一允许的一处：注入编译期固定的静态 sprite（无用户数据）
		allowed := 0
		if name == "app.js" {
			allowed = 1
		}
		if len(hits) > allowed {
			t.Errorf("资产 %s 出现 %d 处 innerHTML 赋值（允许 %d）—— 后端数据必须走 textContent",
				name, len(hits), allowed)
		}
		if regexp.MustCompile(`\beval\s*\(`).MatchString(code) ||
			strings.Contains(code, "new Function(") {
			t.Errorf("资产 %s 使用了 eval / new Function", name)
		}
		// document.write 同样能注入，且无法被 CSP 的 nonce 机制约束
		if strings.Contains(code, "document.write") {
			t.Errorf("资产 %s 使用了 document.write", name)
		}
	}
}

// TestTokenNotPersistedToLocalStorage 令牌只能进 sessionStorage。
// localStorage 会让 XSS 拿到一个跨标签页长期有效的管理面凭证。
func TestTokenNotPersistedToLocalStorage(t *testing.T) {
	h := newH(t)
	api := string(h.files["api.js"])
	if !strings.Contains(api, "'sessionStorage', TOKEN_KEY") {
		t.Error("令牌未存入 sessionStorage")
	}
	// 令牌键不得与 localStorage 一起出现
	for _, line := range strings.Split(api, "\n") {
		if strings.Contains(line, "localStorage") && strings.Contains(line, "TOKEN_KEY") {
			t.Errorf("令牌被写入 localStorage：%s", strings.TrimSpace(line))
		}
	}
}

// TestNoObsPortAccess 指标不得跨端口取自 obs 端口。
// obs 端口（29091）无鉴权且全网暴露，跨端口取数等于让任意网页读运行指标。
func TestNoObsPortAccess(t *testing.T) {
	h := newH(t)
	for name, body := range h.files {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		// 同样只看代码：metrics.js 的注释里必须能写清「为什么否决 obs 端口」，
		// 那段说明是这个决策唯一的留存处，不该被检查逼走。
		code := stripJSCode(string(body))
		for _, bad := range []string{"29091", ":9091", "localhost", "127.0.0.1"} {
			if strings.Contains(code, bad) {
				t.Errorf("资产 %s 的代码引用了 %q —— 指标必须走 admin 端口同源的鉴权端点", name, bad)
			}
		}
		// 绝对 URL 会绕过同源约束，一律禁止
		if regexp.MustCompile(`fetch\s*\(\s*['"]https?://`).MatchString(code) {
			t.Errorf("资产 %s 存在跨源 fetch —— 零外链且必须同源", name)
		}
	}
}
