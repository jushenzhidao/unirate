package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Tier 1 运行策略（见 docs/decisions/CONFIG-TIERING.md）
//
// 这一层的判据是「可安全热替换、误配影响可逆」，因此归管理页面而非环境变量。
// 持久化链路完全复用业务规则那条：
//
//	MySQL runtime_config (SoT) → Redis 快照 → 本地 atomic.Pointer
//
// 关键设计：Redis 快照里存的是**覆盖项原始 map**而不是解析后的 Policy。
// 原因是环境变量是「每实例本地事实」，不同实例的 env 可以不同；
// 若把解析结果发布出去，A 实例的 env 会污染 B 实例的生效值。
// 因此发布 override，各实例本地按 page > env > default 自行解析。
//
// 优先级：页面 > 环境变量 > 内置默认。
// 页面优先的理由：页面是运行期决策，env 是部署期决策，运行期应当能覆盖部署期。

// PolicyKey Tier 1 配置项的键名（同时是 API 字段名与 DB 主键）
const (
	KeyExposeRuleName     = "expose_rule_name"
	KeyUpstreamTimeout    = "upstream_timeout"
	KeyTokenFlushInterval = "token_flush_interval"
	KeyMaxRequestBodyMB   = "max_request_body_mb"
	KeyConfigPollInterval = "config_poll_interval"
	KeyLogLevel           = "log_level"
	KeyInstances          = "instances"
)

// PolicyEnvName 键名 → 对应环境变量名。用于「是否被环境变量显式设置」的判定。
// 表中的值是环境变量的**名字**，不含任何凭证内容。Tier 0 凭证项
// （ADMIN_TOKEN 等）按 CONFIG-TIERING.md 明确不进此白名单。
// #nosec G101 -- 映射表存的是环境变量名，非凭证值
var PolicyEnvName = map[string]string{
	KeyExposeRuleName:     "EXPOSE_RULE_NAME",
	KeyUpstreamTimeout:    "UPSTREAM_TIMEOUT",
	KeyTokenFlushInterval: "TOKEN_FLUSH_INTERVAL",
	KeyMaxRequestBodyMB:   "MAX_REQUEST_BODY_MB",
	KeyConfigPollInterval: "CONFIG_POLL_INTERVAL",
	KeyLogLevel:           "LOG_LEVEL",
	KeyInstances:          "INSTANCES",
}

// PolicyKeys 固定顺序，保证 API 输出稳定（前端表格不会因 map 遍历乱序而抖动）
var PolicyKeys = []string{
	KeyExposeRuleName,
	KeyUpstreamTimeout,
	KeyTokenFlushInterval,
	KeyMaxRequestBodyMB,
	KeyConfigPollInterval,
	KeyLogLevel,
	KeyInstances,
}

// Dur 序列化为人类可读时长字符串（"1s" 而非 1000000000）。
//
// 反序列化**只接受字符串**：若接受裸数字，API 调用方写 {"upstream_timeout": 30}
// 会被当成 30 纳秒，校验下限 1s 会拒掉它 —— 但错误信息会让人一头雾水。
// 直接拒绝数字并要求带单位，是更早、更清楚的失败。
type Dur time.Duration

// MarshalJSON 输出带单位的字符串
func (d Dur) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON 只接受 "100ms" / "1s" 这类带单位字符串
func (d *Dur) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a quoted string with unit (e.g. \"1s\", \"500ms\")")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Dur(v)
	return nil
}

// D 转回 time.Duration
func (d Dur) D() time.Duration { return time.Duration(d) }

// Policy Tier 1 运行策略的生效值
type Policy struct {
	ExposeRuleName     bool   `json:"expose_rule_name"`
	UpstreamTimeout    Dur    `json:"upstream_timeout"`
	TokenFlushInterval Dur    `json:"token_flush_interval"`
	MaxRequestBodyMB   int    `json:"max_request_body_mb"`
	ConfigPollInterval Dur    `json:"config_poll_interval"`
	LogLevel           string `json:"log_level"`
	Instances          int    `json:"instances"`
}

// DefaultPolicy 内置默认值。与 CONFIG-TIERING.md 的默认列必须一致。
func DefaultPolicy() *Policy {
	return &Policy{
		ExposeRuleName:     true,
		UpstreamTimeout:    Dur(60 * time.Second),
		TokenFlushInterval: Dur(time.Second),
		MaxRequestBodyMB:   32,
		ConfigPollInterval: Dur(15 * time.Second),
		LogLevel:           "info",
		Instances:          1,
	}
}

// MaxRequestBodyBytes 换算为字节，供 proxy 使用
func (p *Policy) MaxRequestBodyBytes() int64 { return int64(p.MaxRequestBodyMB) << 20 }

// set 按键写入，值非法时返回错误。所有校验集中在 spec 表，避免多处规则漂移。
func (p *Policy) set(key, raw string) error {
	sp, ok := policySpecs[key]
	if !ok {
		return fmt.Errorf("unknown config key %q", key)
	}
	return sp.apply(p, raw)
}

// Clone 深拷贝。Policy 全是值字段，直接复制即可；
// 保留此方法是为了让调用方的意图显式（拿到的是快照，改它不影响生效值）。
func (p *Policy) Clone() *Policy {
	c := *p
	return &c
}

// ResolvePolicy 按 页面 > 环境变量 > 默认 解析出生效策略。
//
//	base      —— 已由环境变量解析过的策略（env 未设置的项即为默认值）
//	overrides —— 来自 MySQL runtime_config 的页面覆盖项
//
// 单项非法不会拖垮整体：记入 problems 并回退到 base 的取值。
// 理由与 store.LoadFromMySQL 处理坏规则一致 —— 一个字段写错不应让网关整体失去配置。
func ResolvePolicy(base *Policy, overrides map[string]string) (*Policy, []string) {
	if base == nil {
		base = DefaultPolicy()
	}
	out := base.Clone()
	var problems []string
	for _, k := range PolicyKeys {
		raw, ok := overrides[k]
		if !ok {
			continue
		}
		probe := out.Clone()
		if err := probe.set(k, raw); err != nil {
			problems = append(problems, fmt.Sprintf("%s=%q: %v", k, raw, err))
			continue
		}
		out = probe
	}
	return out, problems
}

// PolicyFromEnv 用环境变量解析出 base 策略（未设置的项落到默认值）
func PolicyFromEnv(rt *Runtime) *Policy {
	d := DefaultPolicy()
	if rt == nil {
		return d
	}
	return &Policy{
		ExposeRuleName:     rt.ExposeRuleName,
		UpstreamTimeout:    Dur(rt.UpstreamTimeout),
		TokenFlushInterval: Dur(rt.TokenFlushEvery),
		MaxRequestBodyMB:   int(rt.MaxRequestBody >> 20),
		ConfigPollInterval: Dur(rt.ConfigPollInterval),
		LogLevel:           rt.LogLevel,
		Instances:          rt.Instances,
	}
}
