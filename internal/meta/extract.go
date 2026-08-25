package meta

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// 元数据提取（对应评审 P1-9 修正）
//
// 原设计缺陷：
//  1. token 维度只说「从请求头提取」—— Authorization Bearer？X-Api-Key？多 header 优先级？
//     未定义，开发者只能猜，不同实例行为可能不一致。
//  2. ip 维度直接信任 X-Forwarded-For。XFF 完全由客户端可控，
//     攻击者每个请求随机一个 XFF 即可完全绕过 IP 限流 —— IP 维度形同虚设。

// Config 元数据提取配置
type Config struct {
	// TokenHeaders 按优先级排列的令牌来源头，先命中先用
	TokenHeaders []string
	// TokenPrefixes 需要剥离的前缀（大小写不敏感），如 "Bearer "
	TokenPrefixes []string
	// TrustedProxyHops 可信代理层数。
	// 网关前置了 N 层可信代理（SLB/Nginx）时设为 N，从 XFF 最右侧向左数第 N 个值为真实客户端 IP。
	// 设为 0 表示完全不信任 XFF，直接用 TCP 对端地址（直接暴露公网时的安全默认值）。
	TrustedProxyHops int
	// RealIPHeader 若前置代理写入了可信的真实 IP 头（如 X-Real-IP），可指定。优先级高于 XFF。
	RealIPHeader string
}

// DefaultConfig 安全默认值：不信任任何 XFF
func DefaultConfig() Config {
	return Config{
		TokenHeaders:     []string{"Authorization", "X-Api-Key"},
		TokenPrefixes:    []string{"Bearer ", "Token "},
		TrustedProxyHops: 0,
	}
}

// RequestMeta 从请求提取的元数据
type RequestMeta struct {
	Biz      string
	Path     string
	RawToken string
	IP       string
	Method   string
}

// ErrInvalidBiz URL 格式错误
var ErrInvalidBiz = fmt.Errorf("invalid biz format")

// ExtractBiz 从路径提取业务域标识。
//
// 规则（Spec §2.1）：
//   - 取 Path 第一个 segment
//   - 只允许 [a-zA-Z0-9-]，统一转小写
//   - 空 biz 返回错误
//
// 注意：此处收紧了 Spec 的字符集 —— 不再允许下划线。
// 原因见 limiter/key.go：下划线曾是 Key 分隔符，允许 biz 含下划线会引入歧义。
// 虽然新 Key 方案已用 '|' 分隔且做了安全编码，但保持 biz 字符集干净仍有价值：
// biz 会出现在日志、指标标签、Redis Key 中，越简单越不容易出问题。
func ExtractBiz(path string) (biz string, rest string, err error) {
	p := strings.TrimPrefix(path, "/")
	if p == "" {
		return "", "", ErrInvalidBiz
	}
	i := strings.IndexByte(p, '/')
	if i < 0 {
		biz, rest = p, "/"
	} else {
		biz, rest = p[:i], p[i:]
	}
	if biz == "" {
		return "", "", ErrInvalidBiz
	}
	biz = strings.ToLower(biz)
	for i := 0; i < len(biz); i++ {
		c := biz[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", "", ErrInvalidBiz
		}
	}
	if len(biz) > 64 {
		return "", "", ErrInvalidBiz
	}
	return biz, rest, nil
}

// ExtractToken 按配置的优先级提取鉴权令牌原文。
// 网关不校验令牌有效性（零鉴权侵入），仅将其作为限流维度。
func (c *Config) ExtractToken(r *http.Request) string {
	for _, h := range c.TokenHeaders {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		for _, p := range c.TokenPrefixes {
			if len(v) >= len(p) && strings.EqualFold(v[:len(p)], p) {
				v = strings.TrimSpace(v[len(p):])
				break
			}
		}
		if v != "" {
			return v
		}
	}
	return ""
}

// ExtractIP 提取可信的客户端 IP。
//
// 核心安全约束：XFF 是客户端可写的，只有「已知前置了 N 层可信代理」时，
// 从右往左数第 N 个值才是可信的 —— 更左侧的值都可能是客户端伪造的。
//
// 示例：XFF: "1.1.1.1, 2.2.2.2, 3.3.3.3"，前置 1 层可信代理（hops=1）
//
//	3.3.3.3 是可信代理写入的、它看到的对端 → 取 3.3.3.3
//	1.1.1.1 和 2.2.2.2 可能全是客户端伪造的 → 必须忽略
func (c *Config) ExtractIP(r *http.Request) string {
	if c.RealIPHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(c.RealIPHeader)); v != "" {
			if ip := parseIP(v); ip != "" {
				return ip
			}
		}
	}

	if c.TrustedProxyHops > 0 {
		xff := r.Header.Get("X-Forwarded-For")
		if xff != "" {
			parts := strings.Split(xff, ",")
			// 从右往左数第 TrustedProxyHops 个
			idx := len(parts) - c.TrustedProxyHops
			if idx >= 0 && idx < len(parts) {
				if ip := parseIP(strings.TrimSpace(parts[idx])); ip != "" {
					return ip
				}
			}
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func parseIP(s string) string {
	if ip := net.ParseIP(s); ip != nil {
		return ip.String()
	}
	return ""
}

// Extract 提取全部元数据
func (c *Config) Extract(r *http.Request) (*RequestMeta, error) {
	biz, _, err := ExtractBiz(r.URL.Path)
	if err != nil {
		return nil, err
	}
	return &RequestMeta{
		Biz:      biz,
		Path:     r.URL.Path,
		RawToken: c.ExtractToken(r),
		IP:       c.ExtractIP(r),
		Method:   r.Method,
	}, nil
}
