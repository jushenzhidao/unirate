package config

import (
	"os"
	"testing"
	"time"
)

// TestPolicyBoundsRejectOutOfRange 上下限来自 CONFIG-TIERING.md 的护栏列，
// 每条都对应一种具体的翻车方式，因此边界必须精确 —— 不是「大概那个量级」。
//
// 这里同时测「刚好越界必须拒」和「刚好在界内必须收」，
// 只测其中一侧无法区分 `<` 与 `<=` 写错（典型的沉默逻辑错误高发点）。
func TestPolicyBoundsRejectOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		valid bool
		why   string
	}{
		// token_flush_interval: 100ms ~ 10s
		{"刷盘间隔低于下限", KeyTokenFlushInterval, "99ms", false, "过小则 Redis 写入压力陡增"},
		{"刷盘间隔等于下限", KeyTokenFlushInterval, "100ms", true, "边界含入"},
		{"刷盘间隔等于上限", KeyTokenFlushInterval, "10s", true, "边界含入"},
		{"刷盘间隔超过上限", KeyTokenFlushInterval, "10.001s", false, "超卖窗口过宽"},
		{"刷盘间隔为零", KeyTokenFlushInterval, "0s", false, "零会导致 ticker panic 或忙轮询"},
		{"刷盘间隔为负", KeyTokenFlushInterval, "-1s", false, "负时长无意义"},

		// upstream_timeout: 1s ~ 600s
		{"上游超时低于下限", KeyUpstreamTimeout, "999ms", false, "过小会误杀正常请求"},
		{"上游超时等于下限", KeyUpstreamTimeout, "1s", true, ""},
		{"上游超时等于上限", KeyUpstreamTimeout, "600s", true, ""},
		{"上游超时超过上限", KeyUpstreamTimeout, "601s", false, "连接长期占用"},

		// config_poll_interval: 5s ~ 300s
		{"轮询间隔低于下限", KeyConfigPollInterval, "4s", false, "过频增加 Redis 负载"},
		{"轮询间隔等于下限", KeyConfigPollInterval, "5s", true, ""},
		{"轮询间隔等于上限", KeyConfigPollInterval, "300s", true, ""},
		{"轮询间隔超过上限", KeyConfigPollInterval, "301s", false, "配置生效延迟过大"},

		// max_request_body_mb: 1 ~ 256
		{"请求体上限为零", KeyMaxRequestBodyMB, "0", false, "零会拒绝所有带体请求"},
		{"请求体上限为负", KeyMaxRequestBodyMB, "-1", false, "负值无意义"},
		{"请求体上限等于下限", KeyMaxRequestBodyMB, "1", true, ""},
		{"请求体上限等于上限", KeyMaxRequestBodyMB, "256", true, ""},
		{"请求体上限超过上限", KeyMaxRequestBodyMB, "257", false, "内存耗尽风险"},

		// instances: >= 1
		{"实例数为零", KeyInstances, "0", false, "会导致降级配额计算除零"},
		{"实例数为负", KeyInstances, "-3", false, "负值无意义"},
		{"实例数为一", KeyInstances, "1", true, ""},
		{"实例数合理值", KeyInstances, "8", true, ""},

		// log_level 枚举
		{"日志级别非法", KeyLogLevel, "verbose", false, "仅 4 个枚举值"},
		{"日志级别 debug", KeyLogLevel, "debug", true, ""},
		{"日志级别大写", KeyLogLevel, "DEBUG", true, "大小写不敏感"},
		{"日志级别 trace", KeyLogLevel, "trace", false, "slog 无 trace 级别"},

		// bool
		{"布尔非法值", KeyExposeRuleName, "maybe", false, ""},
		{"布尔 false", KeyExposeRuleName, "false", true, ""},
		{"布尔 off", KeyExposeRuleName, "off", true, ""},

		// 时长必须带单位
		{"时长缺单位", KeyUpstreamTimeout, "30", false, "裸数字会被误解析"},
		{"时长乱码", KeyTokenFlushInterval, "abc", false, ""},

		// 未知键
		{"未知配置键", "totally_unknown_key", "1", false, "白名单外一律拒绝"},
		// CONFIG-TIERING.md「明确不搬」的项不得出现在白名单里
		{"SSRF 开关不可通过页面设置", "allow_header_upstream", "true", false, "SSRF 防线不交给一个令牌"},
		{"CIDR 白名单不可通过页面设置", "admin_allow_cidrs", "0.0.0.0/0", false, "会自毁第二重防线"},
		{"XFF 跳数不可通过页面设置", "trusted_proxy_hops", "3", false, "误配导致限流维度整体错乱"},
	}

	for _, tc := range cases {
		problems := ValidatePolicyOverrides(map[string]string{tc.key: tc.value})
		got := len(problems) == 0
		if got != tc.valid {
			t.Errorf("%s (%s=%s): 期望 valid=%v，实际 %v（problems=%v）%s",
				tc.name, tc.key, tc.value, tc.valid, got, problems, tc.why)
		}
	}
}

// TestPolicyPriorityPageOverEnv 优先级契约：页面 > 环境变量 > 默认。
//
// 这是本次改动最容易悄悄写反的地方 —— 反了不会报错、不会崩，
// 只是运维在页面上改了值却不生效（或反过来 env 永远失效）。
func TestPolicyPriorityPageOverEnv(t *testing.T) {
	def := DefaultPolicy()

	// base 模拟「环境变量把超时设成 30s」
	base := DefaultPolicy()
	base.UpstreamTimeout = Dur(30 * time.Second)
	base.LogLevel = "warn"

	// 无页面覆盖 → 应取 env 值
	eff, problems := ResolvePolicy(base, nil)
	if len(problems) != 0 {
		t.Fatalf("空覆盖不应产生 problems: %v", problems)
	}
	if eff.UpstreamTimeout.D() != 30*time.Second {
		t.Errorf("无页面覆盖时应取 env 值 30s，实际 %s", eff.UpstreamTimeout.D())
	}
	if eff.LogLevel != "warn" {
		t.Errorf("无页面覆盖时应取 env 值 warn，实际 %s", eff.LogLevel)
	}

	// 有页面覆盖 → 页面胜出
	eff, problems = ResolvePolicy(base, map[string]string{
		KeyUpstreamTimeout: "5s",
		KeyLogLevel:        "debug",
	})
	if len(problems) != 0 {
		t.Fatalf("合法覆盖不应产生 problems: %v", problems)
	}
	if eff.UpstreamTimeout.D() != 5*time.Second {
		t.Errorf("页面配置必须优先于环境变量：期望 5s，实际 %s", eff.UpstreamTimeout.D())
	}
	if eff.LogLevel != "debug" {
		t.Errorf("页面配置必须优先于环境变量：期望 debug，实际 %s", eff.LogLevel)
	}

	// 未被页面覆盖的项不受影响，仍走 env
	if eff.MaxRequestBodyMB != def.MaxRequestBodyMB {
		t.Errorf("未覆盖项应保持 env/默认值 %d，实际 %d",
			def.MaxRequestBodyMB, eff.MaxRequestBodyMB)
	}
}

// TestResolvePolicyIsolatesBadEntry 单项非法只影响该项，不得拖垮整份策略。
//
// 与 store.LoadFromMySQL 处理坏规则的策略一致：一个字段写错
// 不应让网关整体失去配置（那会造成远大于原始错误的故障）。
func TestResolvePolicyIsolatesBadEntry(t *testing.T) {
	base := DefaultPolicy()
	base.UpstreamTimeout = Dur(30 * time.Second)

	eff, problems := ResolvePolicy(base, map[string]string{
		KeyUpstreamTimeout:    "9999s", // 越界，应被丢弃
		KeyTokenFlushInterval: "500ms", // 合法，应生效
	})

	if len(problems) != 1 {
		t.Fatalf("应恰好报告 1 个问题，实际 %d: %v", len(problems), problems)
	}
	// 非法项回退到 base（env）值，不是回退到内置默认值
	if eff.UpstreamTimeout.D() != 30*time.Second {
		t.Errorf("非法项应回退到 env 值 30s，实际 %s", eff.UpstreamTimeout.D())
	}
	// 合法项照常生效，不被同批次的非法项连坐
	if eff.TokenFlushInterval.D() != 500*time.Millisecond {
		t.Errorf("同批次的合法项应正常生效，实际 %s", eff.TokenFlushInterval.D())
	}
}

// TestEnvExplicitlySet 三态展示依赖这个判定。
//
// 必须用 LookupEnv 而非「取值 != 默认值」：后者无法区分
// 「没设置」与「显式设置成了和默认值相同的值」，而页面要给这两种情况不同提示。
func TestEnvExplicitlySet(t *testing.T) {
	const k = "UNIRATE_TEST_POLICY_PROBE"

	os.Unsetenv(k)
	if EnvExplicitlySet(k) {
		t.Error("未设置的环境变量必须判为 false")
	}

	// 空字符串视为未设置 —— 与 config.env() 的取值语义一致，
	// 否则 compose 里写 FOO= 会被判为已设置，但代码实际用的是默认值
	t.Setenv(k, "")
	if EnvExplicitlySet(k) {
		t.Error("空值环境变量应判为未设置（与 env() 取值语义一致）")
	}
	t.Setenv(k, "   ")
	if EnvExplicitlySet(k) {
		t.Error("纯空白环境变量应判为未设置")
	}

	t.Setenv(k, "info")
	if !EnvExplicitlySet(k) {
		t.Error("显式设置的环境变量必须判为 true")
	}
	if EnvExplicitlySet("") {
		t.Error("空变量名必须判为 false")
	}
}

// TestDurRejectsBareNumber 时长必须带单位。
//
// 若接受裸数字，{"upstream_timeout": 30} 会被 Go 当成 30 纳秒，
// 然后被下限 1s 拒掉 —— 报错说"必须 >= 1s"而用户明明写了 30，
// 排查成本极高。直接在解析层拒绝并说明格式，是更早、更清楚的失败。
func TestDurRejectsBareNumber(t *testing.T) {
	var d Dur
	if err := d.UnmarshalJSON([]byte("30")); err == nil {
		t.Error("裸数字必须被拒绝（会被误当作纳秒）")
	}
	if err := d.UnmarshalJSON([]byte(`"30s"`)); err != nil {
		t.Errorf("带单位字符串必须被接受: %v", err)
	}
	if d.D() != 30*time.Second {
		t.Errorf("期望 30s，实际 %s", d.D())
	}
}

// TestMaxRequestBodyBytesConversion MB → 字节换算。
// 单位换算写错是典型的沉默逻辑错误：不报错，只是限流阈值差 1024 倍。
func TestMaxRequestBodyBytesConversion(t *testing.T) {
	p := DefaultPolicy()
	p.MaxRequestBodyMB = 32
	if got := p.MaxRequestBodyBytes(); got != 32*1024*1024 {
		t.Errorf("32MB 应换算为 %d 字节，实际 %d", 32*1024*1024, got)
	}
	p.MaxRequestBodyMB = 1
	if got := p.MaxRequestBodyBytes(); got != 1048576 {
		t.Errorf("1MB 应换算为 1048576 字节，实际 %d", got)
	}
}

// TestPolicyKeysCoverAllSpecs 键列表与 spec 表必须完全一致。
// 漏一项会让该配置在 API 里彻底隐身：写得进 DB、却不在页面上出现，
// 也不在 GET 响应里 —— 静默失效。
func TestPolicyKeysCoverAllSpecs(t *testing.T) {
	for _, k := range PolicyKeys {
		if _, ok := PolicySpecOf(k); !ok {
			t.Errorf("PolicyKeys 含 %q 但 spec 表中不存在", k)
		}
		if _, ok := PolicyEnvName[k]; !ok {
			t.Errorf("PolicyKeys 含 %q 但未映射环境变量名", k)
		}
		if PolicyValue(DefaultPolicy(), k) == nil {
			t.Errorf("键 %q 没有可输出的默认值，API 会返回 null", k)
		}
	}
	if len(PolicyKeys) != len(policySpecs) {
		t.Errorf("PolicyKeys(%d) 与 spec 表(%d) 数量不一致，有配置项无法在 API 中出现",
			len(PolicyKeys), len(policySpecs))
	}
}

// TestDefaultPolicyMatchesDocumentedDefaults 默认值必须与
// CONFIG-TIERING.md 的默认列一致。文档与代码漂移会让运维按文档做出错误决策。
func TestDefaultPolicyMatchesDocumentedDefaults(t *testing.T) {
	d := DefaultPolicy()
	if !d.ExposeRuleName {
		t.Error("expose_rule_name 默认应为 true")
	}
	if d.UpstreamTimeout.D() != 60*time.Second {
		t.Errorf("upstream_timeout 默认应为 60s，实际 %s", d.UpstreamTimeout.D())
	}
	if d.TokenFlushInterval.D() != time.Second {
		t.Errorf("token_flush_interval 默认应为 1s，实际 %s", d.TokenFlushInterval.D())
	}
	if d.MaxRequestBodyMB != 32 {
		t.Errorf("max_request_body_mb 默认应为 32，实际 %d", d.MaxRequestBodyMB)
	}
	if d.ConfigPollInterval.D() != 15*time.Second {
		t.Errorf("config_poll_interval 默认应为 15s，实际 %s", d.ConfigPollInterval.D())
	}
	if d.LogLevel != "info" {
		t.Errorf("log_level 默认应为 info，实际 %s", d.LogLevel)
	}
	if d.Instances != 1 {
		t.Errorf("instances 默认应为 1，实际 %d", d.Instances)
	}
}

// TestHighRiskItemsCarryWarning INSTANCES 等高风险项必须带风险提示。
// 提示文案是任务明确要求的交付内容，前端依赖它做显性警示；
// 缺了不会报错，只是运维少了一道认知护栏。
func TestHighRiskItemsCarryWarning(t *testing.T) {
	for _, k := range []string{KeyInstances, KeyTokenFlushInterval, KeyLogLevel, KeyMaxRequestBodyMB} {
		sp, ok := PolicySpecOf(k)
		if !ok {
			t.Fatalf("spec 缺失: %s", k)
		}
		if sp.Warn == "" {
			t.Errorf("高风险项 %s 必须提供 Warn 文案", k)
		}
		if sp.Desc == "" {
			t.Errorf("配置项 %s 必须提供 Desc 说明", k)
		}
	}
}
