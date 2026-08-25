package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testToken 测试用令牌。
//
// 必须同时满足：≥ MinTokenLen(32) 字符、不命中弱值黑名单。
// 原值 "test-admin-token-32-chars-long!!" 以 "test" 开头 —— 阈值提到 32 后
// 长度仍然够，但它属于弱值家族。这里改的是**测试常量**，不是放宽生产校验：
// 若为了让测试通过而下调阈值或删黑名单，等于把真实缺陷重新引回来。
const testToken = "Qk7mR2xW5nL9pT3vY6bF4cH8dJ1sA0zE"

// TestRefusesToStartWithoutToken 验证评审 P0-3 的最关键约束：
// 绝不允许「默认无鉴权」的管理面存在。原 Spec 全文未提 Admin 鉴权，
// 照抄就会产出一个任何人都能改写限流规则的提权入口。
func TestRefusesToStartWithoutToken(t *testing.T) {
	for _, tok := range []string{"", "   ", "short"} {
		if _, err := New(nil, nil, quietLogger(), Options{Addr: ":0", Token: tok}); err == nil {
			t.Errorf("must refuse to start with token %q", tok)
		}
	}

	// 合法长度必须能创建
	if _, err := New(nil, nil, quietLogger(), Options{Addr: ":0", Token: testToken}); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

// TestTestTokenItselfIsStrong 守护测试常量本身。
//
// 若有人日后把 testToken 改回弱值来"让测试更好读"，
// 整个 admin 测试套件会在一个生产环境根本不允许的令牌上运行，
// 而且不会有任何测试变红 —— 这正是沉默失效。此断言让它必然变红。
func TestTestTokenItselfIsStrong(t *testing.T) {
	if len(testToken) < MinTokenLen {
		t.Fatalf("testToken 仅 %d 字符，低于生产阈值 %d", len(testToken), MinTokenLen)
	}
	if err := ValidateToken(testToken); err != nil {
		t.Fatalf("testToken 无法通过生产校验: %v —— 请改测试常量，不要放宽校验", err)
	}
}

func TestRejectsInvalidCIDR(t *testing.T) {
	_, err := New(nil, nil, quietLogger(), Options{
		Addr: ":0", Token: testToken, AllowCIDRs: []string{"not-a-cidr"},
	})
	if err == nil {
		t.Fatal("invalid CIDR must be rejected at construction, not silently ignored")
	}
}

func newTestServer(t *testing.T, opt Options) *Server {
	t.Helper()
	if opt.Token == "" {
		opt.Token = testToken
	}
	s, err := New(nil, nil, quietLogger(), opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestAuthRequired 所有 Admin 路由都必须经过鉴权，无一例外
func TestAuthRequired(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	paths := []struct {
		method, path string
	}{
		{"GET", "/admin/bizs"},
		{"POST", "/admin/bizs"},
		{"DELETE", "/admin/bizs/demo"},
		{"POST", "/admin/reload"},
		{"GET", "/admin/snapshot"},
		{"GET", "/admin/audit"},
		{"POST", "/admin/rules/validate"},
		// 指标端点含 biz 名 / 规则名 / 拒绝分布，是内部拓扑信息，
		// 与其他数据端点同等对待，不得有任何未鉴权可读的缺口。
		{"GET", "/admin/metrics"},
		{"HEAD", "/admin/metrics"},
	}
	for _, p := range paths {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(p.method, p.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: expected 401 without credentials, got %d", p.method, p.path, w.Code)
		}
		// 必须给出标准的 challenge 头
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: missing WWW-Authenticate header", p.method, p.path)
		}
	}
}

// TestWrongTokenRejected 错误令牌的各种形态都必须被拒
func TestWrongTokenRejected(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	bad := []string{
		"Bearer wrong",
		"Bearer " + testToken + "x",              // 多一个字符
		"Bearer " + testToken[:len(testToken)-1], // 少一个字符
		"Basic " + testToken,
		testToken, // 缺 Bearer 前缀且不走 X-Admin-Token
	}
	for _, auth := range bad {
		r := httptest.NewRequest("GET", "/admin/snapshot", nil)
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: expected 401, got %d", auth, w.Code)
		}
	}
}

// TestBareTokenInAuthorizationRejected 锁定一个真实存在过的鉴权绕过漏洞。
//
// 原实现用 strings.TrimPrefix(auth, "Bearer ") 取令牌，而 TrimPrefix 在前缀
// 不存在时会原样返回整个字符串 —— 于是 `Authorization: <token>`（不带认证方案）
// 会被当成合法 Bearer 令牌放行。凭证解析必须显式校验方案名。
func TestBareTokenInAuthorizationRejected(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	cases := []struct {
		name string
		auth string
	}{
		{"裸令牌无方案名", testToken},
		{"Basic 方案携带令牌", "Basic " + testToken},
		{"方案名拼错", "Bearer2 " + testToken},
		{"仅方案名无令牌", "Bearer"},
		{"方案名后为空", "Bearer "},
		{"前缀粘连无空格", "Bearer" + testToken},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/admin/snapshot", nil)
		r.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s (%q): 必须 401，实际 %d —— 存在鉴权绕过", tc.name, tc.auth, w.Code)
		}
	}
}

// TestInvalidAuthDoesNotFallBackToCustomHeader Authorization 存在但非法时，
// 不允许回退到 X-Admin-Token —— 否则等于白送攻击者一次额外尝试机会。
func TestInvalidAuthDoesNotFallBackToCustomHeader(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	r := httptest.NewRequest("GET", "/admin/snapshot", nil)
	r.Header.Set("Authorization", "Basic bogus")
	r.Header.Set("X-Admin-Token", testToken) // 正确令牌放在备用头
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("Authorization 非法时不得回退到 X-Admin-Token，实际 %d", w.Code)
	}
}

// TestBearerSchemeCaseInsensitive RFC 7235 规定方案名大小写不敏感
func TestBearerSchemeCaseInsensitive(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		r := httptest.NewRequest("GET", "/admin/snapshot", nil)
		r.Header.Set("Authorization", scheme+" "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusUnauthorized {
			t.Errorf("方案名 %q 必须被接受（RFC 7235 大小写不敏感）", scheme)
		}
	}
}

// TestNilDependenciesReturn503NotPanic 依赖未就绪时返回 503，绝不 panic。
// 配置 Bootstrap 完成前管理面就可能收到请求，nil 解引用会打挂整个管理面。
func TestNilDependenciesReturn503NotPanic(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"}) // db 与 store 均为 nil
	h := s.Handler()

	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/snapshot"},
		{"GET", "/admin/bizs"},
		{"POST", "/admin/bizs"},
		{"DELETE", "/admin/bizs/demo"},
		{"POST", "/admin/reload"},
		{"GET", "/admin/audit"},
		// Metrics 未注入时同样走守卫返回 503，而不是 nil 解引用
		{"GET", "/admin/metrics"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()

		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("%s %s: 依赖为 nil 时发生 panic: %v", tc.method, tc.path, rec)
				}
			}()
			h.ServeHTTP(w, r)
		}()

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s: 期望 503，实际 %d", tc.method, tc.path, w.Code)
		}
	}
}

// TestUnauthenticatedProbeCannotDetectBackendState 未鉴权者不应通过状态码差异
// 推断后端依赖状态 —— auth 必须在依赖守卫之外层
func TestUnauthenticatedProbeCannotDetectBackendState(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	r := httptest.NewRequest("GET", "/admin/snapshot", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证请求必须得到 401（而非暴露依赖状态的 503），实际 %d", w.Code)
	}
}

// TestTokenAcceptedViaBothHeaders Bearer 与 X-Admin-Token 都应支持
func TestTokenAcceptedViaBothHeaders(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, set := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testToken) },
		func(r *http.Request) { r.Header.Set("X-Admin-Token", testToken) },
	} {
		r := httptest.NewRequest("POST", "/admin/rules/validate",
			strings.NewReader(`[{"name":"ok","type":"rate","metric":"request","dimensions":["biz"],"window":"1s","limit":10}]`))
		set(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusUnauthorized {
			t.Error("valid token was rejected")
		}
	}
}

// TestCIDRAllowlist 来源白名单作为第二重保险
func TestCIDRAllowlist(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0", AllowCIDRs: []string{"10.0.0.0/8"}})
	h := s.Handler()

	// 白名单外：即使带正确令牌也必须拒绝
	r := httptest.NewRequest("GET", "/admin/snapshot", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("out-of-allowlist source must get 403 even with valid token, got %d", w.Code)
	}

	// 白名单内 + 正确令牌 → 放行
	r = httptest.NewRequest("GET", "/admin/snapshot", nil)
	r.RemoteAddr = "10.1.2.3:5678"
	r.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Errorf("in-allowlist source with valid token must pass, got %d", w.Code)
	}
}

// TestValidateEndpointRejectsBadRules 规则试算接口必须拦住非法配置。
// 这是「配置错误在写入前暴露」的第一道关口。
func TestValidateEndpointRejectsBadRules(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{
			// 评审 P0-2：令牌桶与 Token 预算语义不兼容
			name:  "token_bucket with metric=token",
			body:  `[{"name":"x","type":"rate","metric":"token","dimensions":["biz"],"window":"1h","limit":1000,"algorithm":"token_bucket"}]`,
			valid: false,
		},
		{
			name:  "global combined with other dims",
			body:  `[{"name":"x","type":"rate","dimensions":["global","ip"],"window":"1s","limit":10}]`,
			valid: false,
		},
		{
			name:  "unknown dimension",
			body:  `[{"name":"x","type":"rate","dimensions":["country"],"window":"1s","limit":10}]`,
			valid: false,
		},
		{
			name:  "duplicated dimension",
			body:  `[{"name":"x","type":"rate","dimensions":["biz","biz"],"window":"1s","limit":10}]`,
			valid: false,
		},
		{
			name:  "missing name",
			body:  `[{"type":"rate","dimensions":["biz"],"window":"1s","limit":10}]`,
			valid: false,
		},
		{
			name:  "invalid window unit",
			body:  `[{"name":"x","type":"rate","dimensions":["biz"],"window":"1y","limit":10}]`,
			valid: false,
		},
		{
			// 滑动窗口 limit 过大会撑爆 ZSet 内存
			name:  "sliding_window limit too large",
			body:  `[{"name":"x","type":"rate","dimensions":["biz"],"window":"1h","limit":200000,"algorithm":"sliding_window"}]`,
			valid: false,
		},
		{
			name:  "concurrency without max_concurrent",
			body:  `[{"name":"x","type":"concurrency","dimensions":["biz"]}]`,
			valid: false,
		},
		{
			name:  "valid fixed window",
			body:  `[{"name":"ok","type":"rate","metric":"request","dimensions":["biz","ip"],"window":"1m","limit":600}]`,
			valid: true,
		},
		{
			name:  "valid token budget with fixed window",
			body:  `[{"name":"ok","type":"rate","metric":"token","dimensions":["biz","token"],"window":"1h","limit":100000,"algorithm":"fixed_window"}]`,
			valid: true,
		},
		{
			name:  "valid concurrency",
			body:  `[{"name":"ok","type":"concurrency","dimensions":["biz"],"max_concurrent":50}]`,
			valid: true,
		},
	}

	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/admin/rules/validate", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		wantCode := http.StatusBadRequest
		if tc.valid {
			wantCode = http.StatusOK
		}
		if w.Code != wantCode {
			t.Errorf("%s: expected %d, got %d (%s)", tc.name, wantCode, w.Code,
				strings.TrimSpace(w.Body.String()))
		}
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/admin/bizs"},
		{"GET", "/admin/reload"},
		{"GET", "/admin/rules/validate"},
		{"GET", "/admin/bizs/demo"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: expected 405, got %d", tc.method, tc.path, w.Code)
		}
	}
}
