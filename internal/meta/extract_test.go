package meta

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestXFFSpoofingBlocked 验证评审 P1-9：XFF 不可信时必须回落到 TCP 对端。
// 原设计直接信任 XFF，攻击者每请求随机一个 XFF 就能完全绕过 IP 维度限流。
func TestXFFSpoofingBlocked(t *testing.T) {
	c := DefaultConfig() // TrustedProxyHops = 0
	r := httptest.NewRequest("GET", "/order/v1/create", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	if got := c.ExtractIP(r); got != "203.0.113.9" {
		t.Fatalf("XFF must be ignored when hops=0, got %q", got)
	}
}

// TestTrustedProxyHops 前置 N 层可信代理时，从右往左数第 N 个才可信
func TestTrustedProxyHops(t *testing.T) {
	cases := []struct {
		hops int
		xff  string
		want string
	}{
		// 客户端伪造了 1.1.1.1、2.2.2.2；3.3.3.3 是唯一可信代理写入的
		{1, "1.1.1.1, 2.2.2.2, 3.3.3.3", "3.3.3.3"},
		{2, "1.1.1.1, 2.2.2.2, 3.3.3.3", "2.2.2.2"},
		{3, "1.1.1.1, 2.2.2.2, 3.3.3.3", "1.1.1.1"},
		// hops 超过实际层数 → 索引越界，必须回落对端而不是取到伪造值
		{5, "1.1.1.1, 2.2.2.2", "198.51.100.7"},
		// 非法 IP 不得采纳
		{1, "not-an-ip", "198.51.100.7"},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.TrustedProxyHops = tc.hops
		r := httptest.NewRequest("GET", "/x/y", nil)
		r.RemoteAddr = "198.51.100.7:1234"
		r.Header.Set("X-Forwarded-For", tc.xff)
		if got := c.ExtractIP(r); got != tc.want {
			t.Errorf("hops=%d xff=%q: want %q, got %q", tc.hops, tc.xff, tc.want, got)
		}
	}
}

// TestTokenExtractionPriority 令牌来源与前缀剥离必须确定性（评审 P1-9 第一项）
func TestTokenExtractionPriority(t *testing.T) {
	c := DefaultConfig()

	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Authorization", "Bearer sk-abc123")
	r.Header.Set("X-Api-Key", "key-should-lose")
	if got := c.ExtractToken(r); got != "sk-abc123" {
		t.Fatalf("Authorization must win with prefix stripped, got %q", got)
	}

	// 大小写不敏感的前缀剥离
	r2 := httptest.NewRequest("GET", "/x", nil)
	r2.Header.Set("Authorization", "bearer sk-lower")
	if got := c.ExtractToken(r2); got != "sk-lower" {
		t.Fatalf("prefix match must be case-insensitive, got %q", got)
	}

	// 首选头缺失时回落次选
	r3 := httptest.NewRequest("GET", "/x", nil)
	r3.Header.Set("X-Api-Key", "key-1")
	if got := c.ExtractToken(r3); got != "key-1" {
		t.Fatalf("fallback header failed, got %q", got)
	}

	// 无凭证返回空串（匿名维度）
	if got := c.ExtractToken(httptest.NewRequest("GET", "/x", nil)); got != "" {
		t.Fatalf("expected empty token, got %q", got)
	}
}

func TestExtractBiz(t *testing.T) {
	ok := []struct{ path, biz, rest string }{
		{"/order/v1/create", "order", "/v1/create"},
		{"/Order/v1", "order", "/v1"},
		{"/order", "order", "/"},
		{"/ai-proxy/v1/chat/completions", "ai-proxy", "/v1/chat/completions"},
	}
	for _, tc := range ok {
		b, rest, err := ExtractBiz(tc.path)
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.path, err)
			continue
		}
		if b != tc.biz || rest != tc.rest {
			t.Errorf("%q: got (%q,%q), want (%q,%q)", tc.path, b, rest, tc.biz, tc.rest)
		}
	}

	// 非法 biz 必须拒绝：下划线与特殊字符会污染 Redis Key 与指标标签
	for _, p := range []string{"", "/", "//v1", "/or_der/v1", "/or.der/v1", "/or:der"} {
		if _, _, err := ExtractBiz(p); err == nil {
			t.Errorf("expected %q rejected", p)
		}
	}
}

func TestRealIPHeaderWins(t *testing.T) {
	c := DefaultConfig()
	c.RealIPHeader = "X-Real-IP"
	c.TrustedProxyHops = 1
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Real-IP", "8.8.8.8")
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := c.ExtractIP(r); got != "8.8.8.8" {
		t.Fatalf("RealIPHeader must take precedence, got %q", got)
	}
}

var _ = http.MethodGet
