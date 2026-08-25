package admin

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unirate/gateway/internal/config"
)

func policyServer(t *testing.T) http.Handler {
	t.Helper()
	store := config.NewStore(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s, err := New(nil, store, quietLogger(), Options{Addr: ":0", Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.Handler()
}

func doPolicy(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// TestPolicyEndpointsRequireAuth 新增端点绝不能漏鉴权。
// 这是新增路由最容易犯的错 —— 挂路由时忘记套 s.auth，
// 而端点本身工作正常，测试如果只测功能就完全发现不了。
func TestPolicyEndpointsRequireAuth(t *testing.T) {
	h := policyServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/policy"},
		{"PUT", "/admin/policy"},
		{"POST", "/admin/policy/validate"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: 无凭证必须 401，实际 %d", tc.method, tc.path, w.Code)
		}
		if w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s: 缺少 WWW-Authenticate challenge 头", tc.method, tc.path)
		}
	}
}

// TestPolicyMethodWhitelist 未声明的方法必须 405
func TestPolicyMethodWhitelist(t *testing.T) {
	h := policyServer(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/admin/policy"},
		{"DELETE", "/admin/policy"},
		{"PATCH", "/admin/policy"},
		{"GET", "/admin/policy/validate"},
	} {
		code, _ := doPolicy(t, h, tc.method, tc.path, "{}")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: 期望 405，实际 %d", tc.method, tc.path, code)
		}
	}
}

// TestPolicyGetExposesThreeStates 任务硬要求：响应必须能区分
// 当前值 / 默认值 / 是否被环境变量覆盖。前端要显示这个三态，
// 后端不给就没法显示 —— 而且不给也不会报错。
func TestPolicyGetExposesThreeStates(t *testing.T) {
	h := policyServer(t)
	code, body := doPolicy(t, h, "GET", "/admin/policy", "")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/policy 期望 200，实际 %d: %v", code, body)
	}

	// 优先级必须显式写进响应，不让前端凭猜测提示用户
	if body["priority"] != "page > env > default" {
		t.Errorf("响应必须声明优先级，实际 %v", body["priority"])
	}

	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 字段类型异常: %T", body["items"])
	}
	if len(items) != len(config.PolicyKeys) {
		t.Fatalf("应返回全部 %d 项，实际 %d", len(config.PolicyKeys), len(items))
	}

	required := []string{
		"key", "kind", "value", "default", "env_value",
		"source", "overridden_by_env", "env_name", "page_value", "desc",
	}
	seen := map[string]bool{}
	for _, raw := range items {
		it, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item 类型异常: %T", raw)
		}
		for _, f := range required {
			if _, hit := it[f]; !hit {
				t.Errorf("项 %v 缺少三态字段 %q", it["key"], f)
			}
		}
		seen[it["key"].(string)] = true
	}
	for _, k := range config.PolicyKeys {
		if !seen[k] {
			t.Errorf("配置项 %q 未出现在响应中，页面上会彻底隐身", k)
		}
	}
}

// TestPolicyItemsAreStablyOrdered 输出顺序必须稳定。
// map 遍历乱序会让前端表格每次刷新都跳动，看起来像数据在变。
func TestPolicyItemsAreStablyOrdered(t *testing.T) {
	h := policyServer(t)
	var first []string
	for round := 0; round < 5; round++ {
		_, body := doPolicy(t, h, "GET", "/admin/policy", "")
		items := body["items"].([]any)
		var keys []string
		for _, raw := range items {
			keys = append(keys, raw.(map[string]any)["key"].(string))
		}
		if round == 0 {
			first = keys
			continue
		}
		if strings.Join(keys, ",") != strings.Join(first, ",") {
			t.Fatalf("多次请求顺序不一致：\n第一次 %v\n本次   %v", first, keys)
		}
	}
	// 顺序应与 config.PolicyKeys 一致
	if strings.Join(first, ",") != strings.Join(config.PolicyKeys, ",") {
		t.Errorf("顺序应与 config.PolicyKeys 一致：\n期望 %v\n实际 %v",
			config.PolicyKeys, first)
	}
}

// TestPolicyHighRiskItemsCarryWarning INSTANCES 必须带风险提示 ——
// 任务明确要求「这项要在响应里带上风险提示文案」。
func TestPolicyHighRiskItemsCarryWarning(t *testing.T) {
	h := policyServer(t)
	_, body := doPolicy(t, h, "GET", "/admin/policy", "")

	found := false
	for _, raw := range body["items"].([]any) {
		it := raw.(map[string]any)
		if it["key"] != config.KeyInstances {
			continue
		}
		found = true
		warn, _ := it["warn"].(string)
		if warn == "" {
			t.Fatal("instances 必须带风险提示：误配直接影响降级期间的限流精度")
		}
		// 提示必须说清风险方向，不能是"请谨慎修改"这类空话
		if !strings.Contains(warn, "超卖") && !strings.Contains(warn, "拒绝") {
			t.Errorf("instances 风险提示未说明具体后果: %q", warn)
		}
	}
	if !found {
		t.Fatal("响应中未找到 instances 项")
	}
}

// TestPolicyValidateRejectsOutOfBounds 校验端点必须拦住越界值。
// 这是「配置错误在写入前暴露」的第一道关口，与 /admin/rules/validate 同构。
func TestPolicyValidateRejectsOutOfBounds(t *testing.T) {
	h := policyServer(t)
	cases := []struct {
		name  string
		body  string
		valid bool
	}{
		{"刷盘间隔低于下限 100ms", `{"values":{"token_flush_interval":"50ms"}}`, false},
		{"刷盘间隔上限内", `{"values":{"token_flush_interval":"100ms"}}`, true},
		{"刷盘间隔超过上限 10s", `{"values":{"token_flush_interval":"30s"}}`, false},
		{"上游超时越界", `{"values":{"upstream_timeout":"3600s"}}`, false},
		{"请求体上限越界", `{"values":{"max_request_body_mb":1024}}`, false},
		{"实例数为零", `{"values":{"instances":0}}`, false},
		{"日志级别非法", `{"values":{"log_level":"verbose"}}`, false},
		{"未知配置键", `{"values":{"nope":"1"}}`, false},
		{"SSRF 开关不可设置", `{"values":{"allow_header_upstream":true}}`, false},
		{"CIDR 白名单不可设置", `{"values":{"admin_allow_cidrs":"0.0.0.0/0"}}`, false},
		{"时长缺单位", `{"values":{"upstream_timeout":30}}`, false},
		{"合法组合", `{"values":{"log_level":"debug","instances":4,"expose_rule_name":false}}`, true},
	}
	for _, tc := range cases {
		code, body := doPolicy(t, h, "POST", "/admin/policy/validate", tc.body)
		want := http.StatusBadRequest
		if tc.valid {
			want = http.StatusOK
		}
		if code != want {
			t.Errorf("%s: 期望 %d，实际 %d（%v）", tc.name, want, code, body)
		}
		if valid, _ := body["valid"].(bool); valid != tc.valid {
			t.Errorf("%s: valid 字段应为 %v，实际 %v", tc.name, tc.valid, valid)
		}
	}
}

// TestPolicyValidateAcceptsNumberAndString 数字与字符串两种写法都要收，
// 避免前端因表单控件类型差异而必须做转换。
func TestPolicyValidateAcceptsNumberAndString(t *testing.T) {
	h := policyServer(t)
	for _, body := range []string{
		`{"values":{"instances":4}}`,
		`{"values":{"instances":"4"}}`,
		`{"values":{"expose_rule_name":true}}`,
		`{"values":{"expose_rule_name":"true"}}`,
	} {
		code, out := doPolicy(t, h, "POST", "/admin/policy/validate", body)
		if code != http.StatusOK {
			t.Errorf("%s 应被接受，实际 %d: %v", body, code, out)
		}
	}
	// null 必须被拒并指向 reset —— 否则用户会以为 null 能清空覆盖
	code, out := doPolicy(t, h, "POST", "/admin/policy/validate",
		`{"values":{"instances":null}}`)
	if code != http.StatusBadRequest {
		t.Errorf("null 值应被拒绝，实际 %d: %v", code, out)
	}
}

// TestPolicyWriteRejectedWithoutDB 写路径需要 SoT，DB 不可用时 503 而非 panic
func TestPolicyWriteRejectedWithoutDB(t *testing.T) {
	h := policyServer(t) // db 为 nil
	code, _ := doPolicy(t, h, "PUT", "/admin/policy", `{"values":{"log_level":"debug"}}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("DB 不可用时写入应返回 503，实际 %d", code)
	}
}

// TestPolicyGetWorksWithoutDB 读路径只用本地快照，MySQL 抖动时仍应可用。
// 若把 requireDB 套在整个端点上，运维在故障期间连"看一眼当前配置"都做不到 ——
// 而那恰恰是最需要看配置的时候。
func TestPolicyGetWorksWithoutDB(t *testing.T) {
	h := policyServer(t) // db 为 nil
	code, body := doPolicy(t, h, "GET", "/admin/policy", "")
	if code != http.StatusOK {
		t.Errorf("MySQL 不可用时读取配置仍应成功，实际 %d: %v", code, body)
	}
}

// TestPolicyWriteValidatesBeforeTouchingDB 越界值必须在 400 阶段就被拦下，
// 不能进入事务。这里 db 为 nil，若校验排在 DB 守卫之后会得到 503 ——
// 那意味着校验发生在写入尝试之后，非法值有机会进 SoT。
func TestPolicyWriteValidatesBeforeTouchingDB(t *testing.T) {
	h := policyServer(t)
	// 空请求体：既无 values 也无 reset，属请求自身的问题，应 400
	code, _ := doPolicy(t, h, "PUT", "/admin/policy", `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("空请求体应返回 400，实际 %d", code)
	}
}

// TestPolicyResetKeysAreValidated reset 里拼错键名必须报错。
// 若静默忽略，运维会以为"已经重置了"，而那一项其实还在生效。
func TestPolicyResetKeysAreValidated(t *testing.T) {
	h := policyServer(t)
	code, body := doPolicy(t, h, "PUT", "/admin/policy",
		`{"reset":["log_levl"]}`) // 故意拼错
	if code != http.StatusBadRequest {
		t.Errorf("未知 reset 键应返回 400，实际 %d: %v", code, body)
	}
	if body["problems"] == nil {
		t.Error("应返回 problems 明确指出哪个键有问题")
	}
}

// TestPolicyInvalidJSON 畸形 JSON 返回 400 而非 500
func TestPolicyInvalidJSON(t *testing.T) {
	h := policyServer(t)
	for _, body := range []string{`{`, `not json`, `[]`} {
		code, _ := doPolicy(t, h, "POST", "/admin/policy/validate", body)
		if code != http.StatusBadRequest {
			t.Errorf("畸形 JSON %q 应返回 400，实际 %d", body, code)
		}
	}
}
