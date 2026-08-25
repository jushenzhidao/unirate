package limiter

import (
	"fmt"
	"strconv"
	"strings"
)

// Algorithm 限流算法
type Algorithm string

const (
	AlgFixedWindow   Algorithm = "fixed_window"
	AlgSlidingWindow Algorithm = "sliding_window"
	AlgTokenBucket   Algorithm = "token_bucket"
)

// RuleType 规则类型
type RuleType string

const (
	TypeRate        RuleType = "rate"
	TypeConcurrency RuleType = "concurrency"
)

// Metric 计量对象
type Metric string

const (
	MetricRequest Metric = "request"
	MetricToken   Metric = "token"
)

// 预定义维度
const (
	DimGlobal = "global"
	DimBiz    = "biz"
	DimPath   = "path"
	DimToken  = "token"
	DimIP     = "ip"
	DimMethod = "method"
)

var validDims = map[string]bool{
	DimGlobal: true, DimBiz: true, DimPath: true,
	DimToken: true, DimIP: true, DimMethod: true,
}

// Rule 一条频控规则
type Rule struct {
	ID         int64     `json:"id" yaml:"id"`
	Name       string    `json:"name" yaml:"name"`
	Type       RuleType  `json:"type" yaml:"type"`
	Metric     Metric    `json:"metric" yaml:"metric"`
	Dimensions []string  `json:"dimensions" yaml:"dimensions"`
	Window     string    `json:"window" yaml:"window"`
	Limit      int64     `json:"limit" yaml:"limit"`
	Algorithm  Algorithm `json:"algorithm" yaml:"algorithm"`
	Burst      int64     `json:"burst" yaml:"burst"`
	Watermark  int       `json:"watermark" yaml:"watermark"`
	MaxConc    int64     `json:"max_concurrent" yaml:"max_concurrent"`
	TimeoutSec int64     `json:"timeout" yaml:"timeout"`
	Enabled    *bool     `json:"enabled" yaml:"enabled"`

	winSec  int64 // 解析后的窗口秒数
	natural bool  // 是否为自然日/周语义（受业务时区影响）
}

// WindowSeconds 返回解析后的窗口秒数
func (r *Rule) WindowSeconds() int64 { return r.winSec }

// IsNaturalWindow 返回该窗口是否按业务时区自然对齐
func (r *Rule) IsNaturalWindow() bool { return r.natural }

// IsEnabled 默认启用
func (r *Rule) IsEnabled() bool { return r.Enabled == nil || *r.Enabled }

// ParseWindow 解析窗口表达式，如 1s / 5m / 1h / 1d / 2w
func ParseWindow(w string) (sec int64, natural bool, err error) {
	if len(w) < 2 {
		return 0, false, fmt.Errorf("invalid window %q", w)
	}
	unit := w[len(w)-1]
	n, err := strconv.ParseInt(w[:len(w)-1], 10, 64)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("invalid window %q", w)
	}
	switch unit {
	case 's':
		return n, false, nil
	case 'm':
		return n * 60, false, nil
	case 'h':
		return n * 3600, false, nil
	case 'd':
		return n * 86400, true, nil
	case 'w':
		return n * 604800, true, nil
	default:
		return 0, false, fmt.Errorf("invalid window unit %q", string(unit))
	}
}

// Validate 校验并规范化规则。
// 这里承担「配置错误必须在加载期暴露」的职责 —— 运行期再报错就晚了。
func (r *Rule) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("rule name required")
	}
	if len(r.Dimensions) == 0 {
		return fmt.Errorf("rule %q: dimensions required", r.Name)
	}
	seen := map[string]bool{}
	for i, d := range r.Dimensions {
		d = strings.ToLower(strings.TrimSpace(d))
		if !validDims[d] {
			return fmt.Errorf("rule %q: unknown dimension %q", r.Name, d)
		}
		if seen[d] {
			return fmt.Errorf("rule %q: duplicated dimension %q", r.Name, d)
		}
		// global 是「无维度」语义，与任何具体维度组合都自相矛盾
		if d == DimGlobal && len(r.Dimensions) > 1 {
			return fmt.Errorf("rule %q: dimension 'global' cannot combine with others", r.Name)
		}
		seen[d] = true
		r.Dimensions[i] = d
	}

	switch r.Type {
	case TypeConcurrency:
		if r.MaxConc <= 0 {
			return fmt.Errorf("rule %q: max_concurrent must be > 0", r.Name)
		}
		if r.TimeoutSec <= 0 {
			r.TimeoutSec = 120 // 与 Spec §2.5.3 默认值一致
		}
		return nil

	case TypeRate:
		if r.Limit <= 0 {
			return fmt.Errorf("rule %q: limit must be > 0", r.Name)
		}
		sec, natural, err := ParseWindow(r.Window)
		if err != nil {
			return fmt.Errorf("rule %q: %w", r.Name, err)
		}
		r.winSec, r.natural = sec, natural

		if r.Metric == "" {
			r.Metric = MetricRequest
		}
		if r.Metric != MetricRequest && r.Metric != MetricToken {
			return fmt.Errorf("rule %q: unknown metric %q", r.Name, r.Metric)
		}
		if r.Algorithm == "" {
			r.Algorithm = AlgFixedWindow
		}

		// 对应评审 P0-2：令牌桶是持久速率桶，与「长窗口 Token 配额」语义不兼容。
		// Token 预算属于「窗口内总量」语义，必须用固定/滑动窗口表达。
		if r.Algorithm == AlgTokenBucket {
			if r.Metric == MetricToken {
				return fmt.Errorf(
					"rule %q: token_bucket cannot be used with metric=token; "+
						"token budget is a windowed-quota semantic, use fixed_window or sliding_window", r.Name)
			}
			if r.Burst <= 0 {
				r.Burst = r.Limit
			}
			if r.Burst < r.Limit/r.winSec {
				return fmt.Errorf("rule %q: burst too small for rate", r.Name)
			}
		}

		// 滑动窗口用 ZSet 存储，成员数等于窗口内请求数，超大 limit 会撑爆内存
		if r.Algorithm == AlgSlidingWindow && r.Limit > 100000 {
			return fmt.Errorf(
				"rule %q: sliding_window with limit > 100000 costs excessive memory; use fixed_window", r.Name)
		}
		if r.Watermark <= 0 || r.Watermark > 100 {
			r.Watermark = 80
		}
		return nil

	default:
		return fmt.Errorf("rule %q: unknown type %q", r.Name, r.Type)
	}
}

// TokenBucketRate 返回令牌桶每秒填充速率
func (r *Rule) TokenBucketRate() float64 {
	if r.winSec <= 0 {
		return float64(r.Limit)
	}
	return float64(r.Limit) / float64(r.winSec)
}

// DimKey 返回维度组合的稳定标识
func (r *Rule) DimKey() string { return strings.Join(r.Dimensions, ".") }
