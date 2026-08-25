package admin

import (
	"github.com/unirate/gateway/internal/config"
)

// Tier 1 策略的三态视图组装。
//
// 「三态」是任务的硬要求：页面必须能显示 当前值 / 默认值 / 是否被环境变量覆盖。
// 前端没法自己推断这三者 —— 它看不到进程的环境变量，也不知道内置默认值，
// 后端不给就显示不出来，而且不给也不会报错。

// policyItem 单项的三态视图
type policyItem struct {
	Key string `json:"key"`
	// Kind 供前端选控件：bool / duration / int / enum
	Kind string `json:"kind"`
	// Value 当前生效值
	Value any `json:"value"`
	// Default 内置默认值
	Default any `json:"default"`
	// EnvValue 环境变量层解析出的值（未显式设置时等于 default）
	EnvValue any `json:"env_value"`
	// Source 生效值来自哪一层：page | env | default
	Source string `json:"source"`
	// OverriddenByEnv 该项是否被环境变量显式设置。
	//
	// 语义要看清：优先级是「页面 > env」，所以 true 不代表页面改动无效，
	// 而是提示运维「本项在部署侧也被固定过，当前页面值正压着它」——
	// 一旦页面覆盖被 reset 清除，生效值会回落到这个 env 值而非内置默认值。
	OverriddenByEnv bool `json:"overridden_by_env"`
	// EnvName 对应的环境变量名，前端提示用
	EnvName string `json:"env_name"`
	// PageValue 页面覆盖的原始值；未覆盖时为 null
	PageValue *string  `json:"page_value"`
	Min       any      `json:"min,omitempty"`
	Max       any      `json:"max,omitempty"`
	Enum      []string `json:"enum,omitempty"`
	Desc      string   `json:"desc"`
	// Warn 高风险提示，前端须在用户改动该项时显性展示
	Warn string `json:"warn,omitempty"`
}

// policyView 组装三态视图。
//
// 抽成独立方法是为了让 PUT 成功后能复用同一份表示，
// 免得前端写完再发一次 GET —— 两次请求之间可能夹进别人的改动，
// 那样用户看到的"写入结果"其实是别人的。
func (s *Server) policyView() map[string]any {
	eff := s.store.Policy()
	base := s.store.PolicyBase()
	def := config.DefaultPolicy()
	overrides := s.store.PolicyOverrides()

	// 按 config.PolicyKeys 的固定顺序输出。
	// 若遍历 map，前端表格每次刷新都会跳动，看起来像数据在变。
	items := make([]policyItem, 0, len(config.PolicyKeys))
	for _, k := range config.PolicyKeys {
		sp, ok := config.PolicySpecOf(k)
		if !ok {
			continue
		}
		envName := config.PolicyEnvName[k]
		envSet := config.EnvExplicitlySet(envName)

		it := policyItem{
			Key:             k,
			Kind:            sp.Kind,
			Value:           config.PolicyValue(eff, k),
			Default:         config.PolicyValue(def, k),
			EnvValue:        config.PolicyValue(base, k),
			OverriddenByEnv: envSet,
			EnvName:         envName,
			Enum:            sp.Enum,
			Desc:            sp.Desc,
			Warn:            sp.Warn,
		}
		if raw, hit := overrides[k]; hit {
			v := raw
			it.PageValue = &v
			it.Source = "page"
		} else if envSet {
			it.Source = "env"
		} else {
			it.Source = "default"
		}
		switch sp.Kind {
		case "int":
			it.Min, it.Max = sp.Min, sp.Max
		case "duration":
			// 上下限渲染成带单位字符串，与 value 的表示保持一致，
			// 否则前端要处理"值是 1s、上限是 600000000000"这种混搭
			it.Min, it.Max = config.DurString(sp.Min), config.DurString(sp.Max)
		}
		items = append(items, it)
	}
	return map[string]any{
		"items": items,
		"count": len(items),
		// priority 显式写进响应，避免前端凭猜测提示用户
		"priority": "page > env > default",
	}
}
