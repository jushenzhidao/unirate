package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 内嵌控制台挂载后的鉴权边界回归。
//
// 这组测试守的是本次改动最容易悄悄坏掉的一件事：在 "/" 上挂静态处理器
// 会不会把某个 /admin/* 数据端点的鉴权绕过去。ServeMux 的最长前缀匹配
// 理论上不会，但「理论上不会」不是验收标准 —— 一旦哪天有人改了注册顺序
// 或加了新端点忘记包 s.auth，这里必须变红。

// TestUIServedWithoutAuth 登录页必须允许未鉴权访问。
// 否则用户没有任何界面可以输入令牌，管理面等于被锁死。
func TestUIServedWithoutAuth(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, p := range []string{"/", "/index.html", "/app.js", "/tokens.css", "/icons.svg"} {
		r := httptest.NewRequest("GET", p, nil) // 刻意不带任何凭证
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s 未鉴权时期望 200（否则无法输入令牌），实际 %d", p, w.Code)
		}
	}
}

// TestStaticAssetsCarryNoSecrets 静态壳不鉴权的前提是它不含任何机密。
// 这条是上一个测试的对价：既然放开了鉴权，就必须证明放开的东西是空的。
func TestStaticAssetsCarryNoSecrets(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, p := range []string{"/", "/app.js", "/api.js", "/metrics.js"} {
		r := httptest.NewRequest("GET", p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		body := w.Body.String()
		if strings.Contains(body, testToken) {
			t.Errorf("%s 的响应体含 Admin 令牌", p)
		}
		// 资产是编译期固定的，不该出现任何运行时配置值
		for _, leak := range []string{"MYSQL_DSN", "REDIS_ADDR", "mysql://", "redis://"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s 的响应体含运行时配置 %q", p, leak)
			}
		}
	}
}

// TestAllDataEndpointsStillRequireAuth 挂了静态处理器之后，
// 每一个数据端点都必须依然鉴权。这是本次改动的核心回归。
func TestAllDataEndpointsStillRequireAuth(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/snapshot"},
		{"GET", "/admin/bizs"},
		{"POST", "/admin/bizs"},
		{"PUT", "/admin/bizs"},
		{"DELETE", "/admin/bizs/demo"},
		{"POST", "/admin/reload"},
		{"GET", "/admin/audit"},
		{"POST", "/admin/rules/validate"},
		{"GET", "/admin/policy"},
		{"PUT", "/admin/policy"},
		{"POST", "/admin/policy/validate"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 未鉴权时期望 401，实际 %d —— 静态挂载不得绕过 auth",
				tc.method, tc.path, w.Code)
		}
		// 未鉴权响应绝不能是 HTML：那意味着请求被静态处理器截走了
		if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s %s 被静态处理器截获（返回 HTML），鉴权链已被绕过", tc.method, tc.path)
		}
	}
}

// TestUnknownAdminPathIs404NotHTML 未注册的 /admin/* 必须是 404 JSON。
//
// 若交给静态处理器兜底，拼错的端点会得到 200 + index.html ——
// 调用方会以为端点存在、只是返回了怪东西，把一个拼写错误伪装成协议问题。
func TestUnknownAdminPathIs404NotHTML(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, p := range []string{"/admin/snapshott", "/admin/nope", "/admin/", "/admin"} {
		r := httptest.NewRequest("GET", p, nil)
		r.Header.Set("Authorization", "Bearer "+testToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s 期望 404，实际 %d", p, w.Code)
		}
		if strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("GET %s 返回了控制台首页，掩盖了「端点不存在」这一事实", p)
		}
	}
}

// TestUIDoesNotLeakBackendStateWhenDepsDown 依赖全挂时静态壳仍应可用。
//
// 这正是最需要打开控制台的时刻：MySQL 挂了要能进来看配置。
// 若静态资产也依赖 store/db，故障时界面直接白屏，等于故障时没有工具。
func TestUIDoesNotLeakBackendStateWhenDepsDown(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"}) // db 与 store 均为 nil
	h := s.Handler()

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("依赖未就绪时控制台仍应可加载，实际 %d", w.Code)
	}
}

// TestUIRejectsWriteMethods 静态资产只读
func TestUIRejectsWriteMethods(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	h := s.Handler()

	for _, m := range []string{"POST", "PUT", "DELETE"} {
		r := httptest.NewRequest(m, "/index.html", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /index.html 期望 405，实际 %d", m, w.Code)
		}
	}
}
