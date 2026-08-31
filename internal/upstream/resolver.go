package upstream

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
)

// 上游解析与 SSRF 防护（对应评审 P0-3 后半部分）
//
// 原设计缺陷：Spec §2.7 对 X-Upstream-Base-URL 的防护只有一句
// 「忽略或返回 400」，而「合法」从未定义 —— scheme 白名单？内网 CIDR 黑名单？
// DNS rebinding？开发者无从下手，实现出来必然是个 SSRF 洞。
//
// 本实现给出完整、可执行的校验规则。

var (
	ErrNoUpstream      = errors.New("no upstream available")
	ErrUpstreamBlocked = errors.New("upstream blocked by security policy")
)

// Target 解析后的上游目标
type Target struct {
	BaseURL         string
	StripPathPrefix bool
	Source          string // config | header | env
}

// SecurityPolicy 上游地址安全策略
type SecurityPolicy struct {
	// AllowHeaderOverride 是否允许通过请求头指定上游。生产环境应关闭。
	AllowHeaderOverride bool
	// AllowedSchemes 允许的协议，默认 http/https
	AllowedSchemes []string
	// HostAllowlist 上游主机白名单（精确匹配或 *.suffix 形式）。非空时只允许列表内主机。
	HostAllowlist []string
	// AllowPrivateNetwork 是否允许上游指向内网地址。
	// 容器化部署中上游通常就在内网，因此默认允许；
	// 但当 AllowHeaderOverride 开启时，必须对 header 来源强制禁止内网，防止 SSRF 探测。
	AllowPrivateNetwork bool
}

// DefaultPolicy 安全默认值
func DefaultPolicy() SecurityPolicy {
	return SecurityPolicy{
		AllowHeaderOverride: false,
		AllowedSchemes:      []string{"http", "https"},
		AllowPrivateNetwork: true,
	}
}

// 私有与保留网段，用于 SSRF 防护
var privateBlocks []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "0.0.0.0/8", "100.64.0.0/10",
		"::1/128", "fc00::/7", "fe80::/10",
	} {
		if _, b, err := net.ParseCIDR(cidr); err == nil {
			privateBlocks = append(privateBlocks, b)
		}
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, b := range privateBlocks {
		if b.Contains(ip) {
			return true
		}
	}
	return false
}

// lookupIP 可在测试中替换，便于验证 DNS rebinding 防护
var lookupIP = net.LookupIP

// isPrivateHost 判断主机是否指向内网。
//
// 三层判定，缺一不可：
//  1. IP 字面量 → 直接查 CIDR；
//  2. 无点主机名（如容器服务名 redis、upstream）→ 容器网络内必为内网，直接拒；
//  3. FQDN → 必须解析 DNS 并检查**全部**返回地址。
//     这是防 DNS rebinding 的关键：攻击者可让 evil.com 解析到 127.0.0.1，
//     只看域名字符串是拦不住的。解析失败同样视为内网（拒绝优于放过）。
func isPrivateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	if strings.EqualFold(host, "localhost") || !strings.Contains(host, ".") {
		return true
	}
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return true
	}
	// 任一解析结果落在内网即拒绝 —— 攻击者只需一条 A 记录指向内网就能得手
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

// Validate 校验上游 URL 是否符合安全策略。
// fromHeader 标识该地址是否来自客户端可控的请求头 —— 此来源需要最严格的校验。
func (p *SecurityPolicy) Validate(raw string, fromHeader bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: malformed url", ErrUpstreamBlocked)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: missing host", ErrUpstreamBlocked)
	}

	schemes := p.AllowedSchemes
	if len(schemes) == 0 {
		schemes = []string{"http", "https"}
	}
	okScheme := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			okScheme = true
			break
		}
	}
	if !okScheme {
		return fmt.Errorf("%w: scheme %q not allowed", ErrUpstreamBlocked, u.Scheme)
	}

	host := u.Hostname()

	// 白名单优先：配置了白名单则必须命中
	if len(p.HostAllowlist) > 0 {
		if !matchAllowlist(host, p.HostAllowlist) {
			return fmt.Errorf("%w: host %q not in allowlist", ErrUpstreamBlocked, host)
		}
		return nil
	}

	// 来自请求头的地址一律禁止指向内网，防止把网关当成内网探测跳板
	if fromHeader && isPrivateHost(host) {
		return fmt.Errorf("%w: header-specified upstream must not target private network", ErrUpstreamBlocked)
	}
	if !p.AllowPrivateNetwork && isPrivateHost(host) {
		return fmt.Errorf("%w: private network not allowed", ErrUpstreamBlocked)
	}
	return nil
}

func matchAllowlist(host string, list []string) bool {
	for _, pat := range list {
		if strings.HasPrefix(pat, "*.") {
			if strings.HasSuffix(host, pat[1:]) {
				return true
			}
			continue
		}
		if strings.EqualFold(host, pat) {
			return true
		}
	}
	return false
}

// ConfigSource 配置面板来源（P1），由 config 包实现
type ConfigSource interface {
	Upstream(biz string) (baseURL string, stripPrefix bool, ok bool)
}

// Resolver 三级上游解析器
type Resolver struct {
	policy     SecurityPolicy
	headerName string
	envPrefix  string
	src        ConfigSource

	envCache sync.Map // biz -> string
}

// New 创建解析器
func New(src ConfigSource, policy SecurityPolicy, headerName, envPrefix string) *Resolver {
	if headerName == "" {
		headerName = "X-Upstream-Base-URL"
	}
	if envPrefix == "" {
		envPrefix = "BIZ_"
	}
	return &Resolver{policy: policy, headerName: headerName, envPrefix: envPrefix, src: src}
}

// HeaderName 返回上游覆盖头名称
func (r *Resolver) HeaderName() string { return r.headerName }

// Resolve 按 P1 配置面板 > P2 请求头 > P3 环境变量 的优先级解析上游。
func (r *Resolver) Resolve(biz string, headerVal string) (*Target, error) {
	// P1: 配置面板（SoT → Redis 读取层）
	if r.src != nil {
		if base, strip, ok := r.src.Upstream(biz); ok && base != "" {
			if err := r.policy.Validate(base, false); err != nil {
				return nil, err
			}
			return &Target{BaseURL: strings.TrimRight(base, "/"), StripPathPrefix: strip, Source: "config"}, nil
		}
	}

	// P2: 请求头（默认关闭；开启时按最严策略校验）
	if r.policy.AllowHeaderOverride && headerVal != "" {
		if err := r.policy.Validate(headerVal, true); err != nil {
			return nil, err
		}
		return &Target{BaseURL: strings.TrimRight(headerVal, "/"), Source: "header"}, nil
	}

	// P3: 环境变量兜底
	if v, ok := r.envCache.Load(biz); ok {
		if s, _ := v.(string); s != "" {
			return &Target{BaseURL: s, Source: "env"}, nil
		}
		return nil, ErrNoUpstream
	}
	name := r.envPrefix + strings.ToUpper(strings.ReplaceAll(biz, "-", "_"))
	val := strings.TrimSpace(os.Getenv(name))
	if val == "" {
		r.envCache.Store(biz, "")
		return nil, ErrNoUpstream
	}
	if err := r.policy.Validate(val, false); err != nil {
		r.envCache.Store(biz, "")
		return nil, err
	}
	val = strings.TrimRight(val, "/")
	r.envCache.Store(biz, val)
	return &Target{BaseURL: val, Source: "env"}, nil
}
