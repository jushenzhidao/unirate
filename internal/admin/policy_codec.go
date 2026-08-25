package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
)

// Tier 1 策略端点的请求解析与小工具。
// 与 policy.go（HTTP 处理）、policy_view.go（三态视图）分文件，各自单一职责。

// policyPayload PUT / validate 的请求体。
//
// values 用 json.RawMessage 接收再统一转成字符串，这样 {"instances": 4}
// 与 {"instances": "4"} 都能收，而时长类仍要求带单位字符串
// （见 config.Dur 的 UnmarshalJSON —— 裸数字会被当纳秒，是个坑）。
type policyPayload struct {
	Values map[string]json.RawMessage `json:"values"`
	// Reset 要清除的覆盖项键名，清除后该项回落到 env/默认
	Reset    []string `json:"reset"`
	Operator string   `json:"operator"`
}

// decodePolicyValues 把 JSON 值统一成字符串。
// 允许 4 与 "4"、true 与 "true"；时长仍必须是带单位字符串。
func decodePolicyValues(in map[string]json.RawMessage) (map[string]string, map[string]string) {
	out := make(map[string]string, len(in))
	problems := map[string]string{}
	for k, raw := range in {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			problems[k] = "invalid json value: " + err.Error()
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case float64:
			// 整数键接受裸数字；时长键接受不了，会在 spec 校验阶段以
			// "invalid duration" 报错并附带期望格式
			if t == float64(int64(t)) {
				out[k] = strconv.FormatInt(int64(t), 10)
			} else {
				problems[k] = "value must be an integer, a boolean, or a quoted string"
			}
		case nil:
			// 明确指向 reset：否则用户会以为 null 能清空覆盖，
			// 而实际上它只是个非法值
			problems[k] = "value must not be null; use \"reset\" to clear an override"
		default:
			problems[k] = "value must be an integer, a boolean, or a quoted string"
		}
	}
	return out, problems
}

// sortedKeys 固定写入顺序。
// 同一批变更在不同实例上按相同顺序执行，便于比对审计日志。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func operatorOf(r *http.Request, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := r.Header.Get("X-Operator"); v != "" {
		return v
	}
	return "unknown"
}
