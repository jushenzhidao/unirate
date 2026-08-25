package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Tier 1 配置项的元数据与上下限。
//
// 上下限直接抄自 docs/decisions/CONFIG-TIERING.md 的「风险与护栏」列 ——
// 这些不是随手拍的数字，每一条都对应一种具体的翻车方式，注释里写明。
//
// 校验必须在写入前完成（与 /admin/rules/validate 同一设计思路）：
// 一旦非法值进了 SoT，它会被发布到所有实例，此时再拒绝已经晚了。

// PolicySpec 单个配置项的规格说明
type PolicySpec struct {
	Key string
	// Kind 供前端选控件：bool / duration / int / enum
	Kind string
	// Min/Max 对 duration 是纳秒，对 int 是数值本身；Kind 为 bool/enum 时忽略
	Min, Max int64
	// Enum 仅 Kind=enum 时有效
	Enum []string
	// Desc 面向运维的说明
	Desc string
	// Warn 高风险提示。前端需在用户改动该项时显性展示。
	Warn string
	// apply 解析原始字符串并写入 Policy，含上下限校验
	apply func(*Policy, string) error
}

// durBound 生成带上下限的时长解析器
func durBound(key string, min, max time.Duration, set func(*Policy, Dur)) func(*Policy, string) error {
	return func(p *Policy, raw string) error {
		v, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid duration (want e.g. %q): %w", min.String(), err)
		}
		if v < min || v > max {
			return fmt.Errorf("%s must be within [%s, %s], got %s", key, min, max, v)
		}
		set(p, Dur(v))
		return nil
	}
}

// intBound 生成带上下限的整数解析器
func intBound(key string, min, max int64, set func(*Policy, int)) func(*Policy, string) error {
	return func(p *Policy, raw string) error {
		n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		if n < min || n > max {
			return fmt.Errorf("%s must be within [%d, %d], got %d", key, min, max, n)
		}
		set(p, int(n))
		return nil
	}
}

func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q (want true/false)", raw)
}

// LogLevels 允许的日志级别
var LogLevels = []string{"debug", "info", "warn", "error"}

var policySpecs = map[string]*PolicySpec{
	KeyExposeRuleName: {
		Key: KeyExposeRuleName, Kind: "bool",
		Desc: "429 响应是否携带 X-RateLimit-Rule / 规则名。外网部署建议关闭，内网排障建议开启。",
		apply: func(p *Policy, raw string) error {
			v, err := parseBool(raw)
			if err != nil {
				return err
			}
			p.ExposeRuleName = v
			return nil
		},
	},
	KeyUpstreamTimeout: {
		Key: KeyUpstreamTimeout, Kind: "duration",
		Min: int64(time.Second), Max: int64(600 * time.Second),
		Desc: "非流式请求的上游整体超时。SSE 不受此约束（流式不设整体超时，靠 idle 与 ctx 控制）。",
		apply: durBound(KeyUpstreamTimeout, time.Second, 600*time.Second,
			func(p *Policy, d Dur) { p.UpstreamTimeout = d }),
	},
	KeyTokenFlushInterval: {
		Key: KeyTokenFlushInterval, Kind: "duration",
		Min: int64(100 * time.Millisecond), Max: int64(10 * time.Second),
		Desc: "SSE 期间 Token 增量刷盘间隔，直接决定 Token 超卖窗口宽度。",
		Warn: "调小会显著增加 Redis 写入压力；调大会拉宽超卖窗口（最坏情况多花该间隔内的 token）。",
		apply: durBound(KeyTokenFlushInterval, 100*time.Millisecond, 10*time.Second,
			func(p *Policy, d Dur) { p.TokenFlushInterval = d }),
	},
	KeyMaxRequestBodyMB: {
		Key: KeyMaxRequestBodyMB, Kind: "int", Min: 1, Max: 256,
		Desc: "单请求体上限（MB）。超过即返回 413。",
		Warn: "上限 256MB 是防内存耗尽的硬护栏，不是建议值；大 body 场景请评估实例内存。",
		apply: intBound(KeyMaxRequestBodyMB, 1, 256,
			func(p *Policy, n int) { p.MaxRequestBodyMB = n }),
	},
	KeyConfigPollInterval: {
		Key: KeyConfigPollInterval, Kind: "duration",
		Min: int64(5 * time.Second), Max: int64(300 * time.Second),
		Desc: "配置变更的兜底轮询间隔。Pub/Sub 负责秒级生效，此项防消息丢失导致长期用旧配置。",
		apply: durBound(KeyConfigPollInterval, 5*time.Second, 300*time.Second,
			func(p *Policy, d Dur) { p.ConfigPollInterval = d }),
	},
	KeyLogLevel: {
		Key: KeyLogLevel, Kind: "enum", Enum: LogLevels,
		Desc: "日志级别。线上排障可临时开 debug，无需重启（重启会丢失现场）。",
		Warn: "debug 在高流量下日志量激增，可能打满磁盘或拖慢 IO，排障结束请调回 info。",
		apply: func(p *Policy, raw string) error {
			v := strings.ToLower(strings.TrimSpace(raw))
			for _, l := range LogLevels {
				if v == l {
					p.LogLevel = v
					return nil
				}
			}
			return fmt.Errorf("log_level must be one of %s", strings.Join(LogLevels, "/"))
		},
	},
	KeyInstances: {
		Key: KeyInstances, Kind: "int", Min: 1, Max: 1024,
		Desc: "集群实例数。仅在 Redis 故障降级时用于本地保守配额估算：每实例配额 = 总配额 ÷ 实例数。",
		Warn: "误配直接影响降级期间的限流精度：设得比真实实例数小会导致降级时超卖（各实例配额之和大于总配额）；" +
			"设得过大会导致降级时过度拒绝。请与实际副本数保持一致，扩缩容后同步更新。",
		apply: intBound(KeyInstances, 1, 1024,
			func(p *Policy, n int) { p.Instances = n }),
	},
}

// PolicySpecOf 取单项规格
func PolicySpecOf(key string) (*PolicySpec, bool) {
	sp, ok := policySpecs[key]
	return sp, ok
}

// DurString 把 spec 里的纳秒上下限渲染成带单位字符串（"100ms" / "10s"）
func DurString(ns int64) string { return time.Duration(ns).String() }

// EnvExplicitlySet 判断环境变量是否被**显式设置**。
//
// 用 LookupEnv 而不是「取值 != 默认值」来判定：后者无法区分
// 「没设置」与「显式设置成了和默认值相同的值」，而这两种情况
// 在页面上要给运维不同的提示。
// 空字符串视为未设置，与 config.env() 的取值语义保持一致
// （否则 compose 里写 FOO= 会被判为已设置，但代码实际用的是默认值）。
func EnvExplicitlySet(name string) bool {
	if name == "" {
		return false
	}
	v, ok := os.LookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}

// PolicyValue 取单项的类型化值，供 API 输出。
// 时长返回带单位字符串，bool/int/enum 返回原生类型，前端可直接用。
func PolicyValue(p *Policy, key string) any {
	if p == nil {
		return nil
	}
	switch key {
	case KeyExposeRuleName:
		return p.ExposeRuleName
	case KeyUpstreamTimeout:
		return p.UpstreamTimeout.D().String()
	case KeyTokenFlushInterval:
		return p.TokenFlushInterval.D().String()
	case KeyMaxRequestBodyMB:
		return p.MaxRequestBodyMB
	case KeyConfigPollInterval:
		return p.ConfigPollInterval.D().String()
	case KeyLogLevel:
		return p.LogLevel
	case KeyInstances:
		return p.Instances
	}
	return nil
}

// ValidatePolicyOverrides 批量校验（不写入）。
// 返回按输入键排序无关的 problems 映射，供 /admin/policy/validate 与写入前门禁共用。
func ValidatePolicyOverrides(overrides map[string]string) map[string]string {
	problems := map[string]string{}
	for k, raw := range overrides {
		sp, ok := policySpecs[k]
		if !ok {
			problems[k] = fmt.Sprintf("unknown config key; allowed: %s", strings.Join(PolicyKeys, ", "))
			continue
		}
		probe := DefaultPolicy()
		if err := sp.apply(probe, raw); err != nil {
			problems[k] = err.Error()
		}
	}
	return problems
}
