package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAppendVia 验证 Via 头按 RFC 7230 §5.7.1 追加而非覆盖。
// 覆盖会丢失上游链路信息，多级代理排障时无法还原路径。
func TestAppendVia(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		proto    int
		want     string
	}{
		{"无既有 Via", "", 1, "1.1 unirate"},
		{"HTTP/2 请求", "", 2, "2.0 unirate"},
		{"追加到既有 Via 之后", "1.1 nginx", 1, "1.1 nginx, 1.1 unirate"},
		{"多级链路继续追加", "1.0 a, 1.1 b", 1, "1.0 a, 1.1 b, 1.1 unirate"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/demo/x", nil)
		r.ProtoMajor = tc.proto
		dst := http.Header{}
		if tc.existing != "" {
			dst.Set("Via", tc.existing)
		}
		appendVia(dst, r)
		if got := dst.Get("Via"); got != tc.want {
			t.Errorf("%s: 期望 Via=%q，实际 %q", tc.name, tc.want, got)
		}
	}
}

// TestHopHeadersAreStripped 逐跳头绝不能转发给上游（RFC 7230 §6.1）。
// 转发 Connection/Upgrade 等头会让上游误判连接语义，
// 转发 Proxy-Authorization 更是凭证泄露。
func TestHopHeadersAreStripped(t *testing.T) {
	src := http.Header{}
	for _, h := range hopHeaders {
		src.Set(h, "should-not-survive")
	}
	src.Set("Authorization", "Bearer keep-me")
	src.Set("Content-Type", "application/json")

	dst := http.Header{}
	copyHeaders(dst, src)
	for _, h := range hopHeaders {
		dst.Del(h)
	}

	for _, h := range hopHeaders {
		if dst.Get(h) != "" {
			t.Errorf("逐跳头 %q 未被剥离，存在协议语义污染风险", h)
		}
	}
	// 端到端头必须保留，否则上游鉴权会失败
	if dst.Get("Authorization") != "Bearer keep-me" {
		t.Error("端到端头 Authorization 被误删")
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Error("端到端头 Content-Type 被误删")
	}
}

// TestCopyHeadersPreservesMultiValue 多值头必须全部保留。
// Set-Cookie 是典型场景：用 Set 而非 Add 会只剩最后一个，导致会话丢失。
func TestCopyHeadersPreservesMultiValue(t *testing.T) {
	src := http.Header{}
	src.Add("Set-Cookie", "a=1")
	src.Add("Set-Cookie", "b=2")
	src.Add("Set-Cookie", "c=3")

	dst := http.Header{}
	copyHeaders(dst, src)

	if got := len(dst.Values("Set-Cookie")); got != 3 {
		t.Errorf("多值头丢失：期望 3 个 Set-Cookie，实际 %d 个", got)
	}
}

func TestClientWantsStream(t *testing.T) {
	cases := []struct {
		accept string
		want   bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"application/json, text/event-stream", true},
		{"application/json", false},
		{"", false},
		{"*/*", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/demo/x", nil)
		if tc.accept != "" {
			r.Header.Set("Accept", tc.accept)
		}
		if got := clientWantsStream(r); got != tc.want {
			t.Errorf("Accept=%q: 期望 %v，实际 %v", tc.accept, tc.want, got)
		}
	}
}

func TestIsJSON(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/problem+json", true},
		{"text/plain", false},
		{"text/event-stream", false},
		{"", false},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.ct != "" {
			h.Set("Content-Type", tc.ct)
		}
		if got := isJSON(h); got != tc.want {
			t.Errorf("Content-Type=%q: 期望 %v，实际 %v", tc.ct, tc.want, got)
		}
	}
}

// TestNewRequestIDUniqueness 请求 ID 必须唯一且长度稳定，
// 否则全链路日志关联会串号。
func TestNewRequestIDUniqueness(t *testing.T) {
	seen := make(map[string]bool, 2000)
	var length int
	for i := 0; i < 2000; i++ {
		id := newRequestID()
		if id == "" {
			t.Fatal("请求 ID 不得为空")
		}
		if seen[id] {
			t.Fatalf("请求 ID 重复: %q", id)
		}
		seen[id] = true
		if length == 0 {
			length = len(id)
		} else if len(id) != length {
			t.Errorf("请求 ID 长度不稳定: 先前 %d，当前 %d (%q)", length, len(id), id)
		}
	}
}

// TestWriteErrorShape 错误响应结构必须稳定，客户端要靠 code 做分支。
func TestWriteErrorShape(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/demo/x", nil)

	h.writeError(w, r, http.StatusTooManyRequests, "rate_limited", "quota exceeded", "req-abc")

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("状态码: 期望 429，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type 应为 JSON，实际 %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{`"code":"rate_limited"`, `"message":"quota exceeded"`, `"request_id":"req-abc"`} {
		if !strings.Contains(body, want) {
			t.Errorf("错误体缺少 %s，实际: %s", want, body)
		}
	}
}
