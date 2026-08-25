package upstream

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// TestSSRFBlocked 验证评审 P0-3 的 SSRF 防护落地。
// 原 Spec 只写「忽略或返回 400」而未定义「合法」，这里逐项锁死。
func TestSSRFBlocked(t *testing.T) {
	p := DefaultPolicy()
	p.AllowHeaderOverride = true

	// 来自请求头的地址必须禁止指向内网/云元数据服务
	blocked := []string{
		"http://127.0.0.1:8080",
		"http://localhost:3000",
		"http://169.254.169.254/latest/meta-data/", // AWS/阿里云元数据，SSRF 首要目标
		"http://10.1.2.3",
		"http://192.168.1.1",
		"http://172.16.0.1",
		"http://[::1]:8080",
		"http://100.64.0.1", // CGNAT
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO", // Redis 协议走私
		"http://redis:6379",             // 容器服务名同样视为内网
		"://nohost",
	}
	for _, u := range blocked {
		if err := p.Validate(u, true); err == nil {
			t.Errorf("expected %q to be blocked from header source, got nil", u)
		} else if !errors.Is(err, ErrUpstreamBlocked) {
			t.Errorf("%q: expected ErrUpstreamBlocked, got %v", u, err)
		}
	}

	// 公网地址允许（DNS 打桩，避免测试依赖外网）
	restore := stubDNS(map[string][]net.IP{
		"api.openai.com": {net.ParseIP("104.18.6.192")},
	})
	defer restore()

	for _, u := range []string{"https://api.openai.com", "http://93.184.216.34"} {
		if err := p.Validate(u, true); err != nil {
			t.Errorf("expected %q allowed, got %v", u, err)
		}
	}
}

func stubDNS(table map[string][]net.IP) func() {
	old := lookupIP
	lookupIP = func(host string) ([]net.IP, error) {
		if ips, ok := table[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	return func() { lookupIP = old }
}

// TestDNSRebindingBlocked 防 DNS rebinding：域名看起来正常，但解析到内网必须拒绝。
// 只做字符串黑名单的实现会在这里失守。
func TestDNSRebindingBlocked(t *testing.T) {
	restore := stubDNS(map[string][]net.IP{
		"rebind.evil.com": {net.ParseIP("127.0.0.1")},
		// 混合返回：一条公网 + 一条内网，仍必须拒绝
		"mixed.evil.com":    {net.ParseIP("1.1.1.1"), net.ParseIP("169.254.169.254")},
		"clean.example.com": {net.ParseIP("93.184.216.34")},
	})
	defer restore()

	p := DefaultPolicy()
	p.AllowHeaderOverride = true

	for _, u := range []string{"http://rebind.evil.com", "http://mixed.evil.com"} {
		if err := p.Validate(u, true); err == nil {
			t.Errorf("expected %q blocked by DNS resolution check", u)
		}
	}
	if err := p.Validate("http://clean.example.com", true); err != nil {
		t.Errorf("public host must pass, got %v", err)
	}
	// 解析失败按内网处理：宁可拒绝也不放过
	if err := p.Validate("http://unresolvable.invalid", true); err == nil {
		t.Error("unresolvable host must be blocked when coming from header")
	}
}

// TestAllowlistOverridesEverything 白名单模式下只认列表内主机
func TestAllowlistOverridesEverything(t *testing.T) {
	p := DefaultPolicy()
	p.AllowHeaderOverride = true
	p.HostAllowlist = []string{"api.openai.com", "*.internal.corp"}

	ok := []string{
		"https://api.openai.com/v1",
		"http://svc-a.internal.corp:8080",
	}
	for _, u := range ok {
		if err := p.Validate(u, true); err != nil {
			t.Errorf("expected %q allowed, got %v", u, err)
		}
	}
	for _, u := range []string{"https://evil.com", "http://api.openai.com.evil.com"} {
		if err := p.Validate(u, true); err == nil {
			t.Errorf("expected %q blocked by allowlist", u)
		}
	}
}

// TestHeaderOverrideDisabledByDefault 默认必须关闭请求头覆盖上游
func TestHeaderOverrideDisabledByDefault(t *testing.T) {
	if DefaultPolicy().AllowHeaderOverride {
		t.Fatal("AllowHeaderOverride must default to false")
	}

	r := New(nil, DefaultPolicy(), "", "")
	if _, err := r.Resolve("nobiz", "https://evil.com"); !errors.Is(err, ErrNoUpstream) {
		t.Fatalf("header upstream must be ignored when disabled, got %v", err)
	}
}

type fakeSrc struct {
	base  string
	strip bool
	ok    bool
}

func (f fakeSrc) Upstream(string) (string, bool, bool) { return f.base, f.strip, f.ok }

// TestResolvePriority P1 配置面板优先于 P2 请求头
func TestResolvePriority(t *testing.T) {
	pol := DefaultPolicy()
	pol.AllowHeaderOverride = true
	r := New(fakeSrc{base: "http://mock-upstream:9000/", strip: true, ok: true}, pol, "", "")

	tg, err := r.Resolve("order", "https://api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Source != "config" {
		t.Fatalf("expected config source to win, got %q", tg.Source)
	}
	if strings.HasSuffix(tg.BaseURL, "/") {
		t.Fatalf("trailing slash must be trimmed, got %q", tg.BaseURL)
	}
	if !tg.StripPathPrefix {
		t.Fatal("strip flag lost")
	}
}
