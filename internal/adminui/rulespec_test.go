package adminui

// 前端规则常量与后端权威定义的一致性护栏。
//
// 为什么不执行 JS：本包零构建、零 node 依赖，引入 JS 运行时只为跑断言，
// 代价远大于收益。这里的做法是从资产源码正则抽出字面量，与 Go 侧的
// **真函数 / 真变量** 比对 —— 拿 limiter.ParseWindow 当预言机，
// 比任何在 JS 侧手写的期望值都强：后端改了语义，这里立刻红。
//
// 直接动因是两个真实发生过的字符级偏差：
//   - 设计稿把 Key 分隔符写成全宽 '│'（U+2502）。它不在 TestNoEmojiInAssets
//     的扫描区间内，人工比对也几乎看不出与半角 '|' 的差别。
//   - 设计稿把 global 维度的空值写成 '-'，后端实际是 '_'（key.go:40）。
//
// 这类偏差不会让任何测试变红、不会报错，只会让前端预览的 Key 与线上 Redis 里
// 真实存在的 Key 不一致 —— 而这个预览的全部价值就是可复制去 Redis 里查。

import (
	"regexp"
	"strings"
	"testing"

	"github.com/unirate/gateway/internal/limiter"
)

// asset 取出一个已嵌入的资产源码，顺带断言它真的进了二进制。
func asset(t *testing.T, h *Handler, name string) string {
	t.Helper()
	body, ok := h.files[name]
	if !ok {
		t.Fatalf("资产 %q 未被嵌入", name)
	}
	return string(body)
}

// jsStringArray 抽出形如 var NAME = ['a', 'b']; 的字符串数组字面量。
// 先剥注释：注释里会出现示例数组，抽错了会得到假绿。
func jsStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	code := stripJSComments(src)
	re := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]`)
	m := re.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("未在资产中找到数组常量 %s", name)
	}
	var out []string
	for _, item := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, item[1])
	}
	if len(out) == 0 {
		t.Fatalf("常量 %s 抽出了 0 个元素，正则可能与源码格式脱节", name)
	}
	return out
}

// jsObjectValues 抽出形如 { v: 'rate', t: '...' } 序列中的所有 v 值，
// 用于 TYPES / METRICS / ALGOS 这类 {v,t} 结构。
func jsObjectValues(t *testing.T, src, name string) []string {
	t.Helper()
	code := stripJSComments(src)
	re := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\n\s*\];`)
	m := re.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("未在资产中找到对象数组常量 %s", name)
	}
	var out []string
	for _, item := range regexp.MustCompile(`v:\s*'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, item[1])
	}
	if len(out) == 0 {
		t.Fatalf("常量 %s 抽出了 0 个 v 字段", name)
	}
	return out
}

// jsStringField 抽出形如 key: 'value' 的单个字符串字段。
func jsStringField(t *testing.T, src, key string) string {
	t.Helper()
	code := stripJSComments(src)
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `:\s*'((?:[^'\\]|\\.)*)'`)
	m := re.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("未在资产中找到字符串字段 %s", key)
	}
	return m[1]
}

// jsNumberField 抽出形如 key: 48 的单个数字字段（返回原文，避免精度歧义）。
func jsNumberField(t *testing.T, src, key string) string {
	t.Helper()
	code := stripJSComments(src)
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `:\s*(-?\d+)`)
	m := re.FindStringSubmatch(code)
	if m == nil {
		t.Fatalf("未在资产中找到数字字段 %s", key)
	}
	return m[1]
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, v := range a {
		seen[v]++
	}
	for _, v := range b {
		seen[v]--
		if seen[v] < 0 {
			return false
		}
	}
	return true
}

// TestRuleSpecWindowUnits 单位表必须与 limiter.ParseWindow 逐位一致。
//
// 断言方式是对每个单位真的调一次后端函数，而不是比对一张手写期望表 ——
// 手写表会跟着 JS 一起错。natural 同样必须比：它决定 Key 的 boundary
// 是否按业务时区偏移，算错会让预览出的 Key 在 Redis 里根本不存在。
func TestRuleSpecWindowUnits(t *testing.T) {
	h := newH(t)
	code := stripJSComments(asset(t, h, "rule-spec.js"))

	unitRe := regexp.MustCompile(`(?s)\bUNIT\s*=\s*\{(.*?)\}`)
	m := unitRe.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("未找到 UNIT 单位表")
	}
	pairs := regexp.MustCompile(`([a-z]):\s*(\d+)`).FindAllStringSubmatch(m[1], -1)
	if len(pairs) == 0 {
		t.Fatal("UNIT 表抽出了 0 个单位")
	}

	naturalJS := map[string]bool{}
	for _, u := range jsStringArray(t, asset(t, h, "rule-spec.js"), "NATURAL_UNITS") {
		naturalJS[u] = true
	}

	for _, p := range pairs {
		unit, secStr := p[1], p[2]
		sec, natural, err := limiter.ParseWindow("1" + unit)
		if err != nil {
			t.Errorf("单位 %q 在前端存在，但后端 ParseWindow 拒绝了 %q：%v", unit, "1"+unit, err)
			continue
		}
		if got := strings.TrimSpace(secStr); got != itoa(sec) {
			t.Errorf("单位 %q 的秒数不一致：前端 %s，后端 %d", unit, got, sec)
		}
		if naturalJS[unit] != natural {
			t.Errorf("单位 %q 的 natural 不一致：前端 %v，后端 %v", unit, naturalJS[unit], natural)
		}
	}

	// 反向：后端认的单位前端不能漏，否则合法窗口在前端被判非法
	for _, unit := range []string{"s", "m", "h", "d", "w"} {
		if !regexp.MustCompile(`\b` + unit + `:\s*\d+`).MatchString(m[1]) {
			t.Errorf("后端支持单位 %q，但前端 UNIT 表缺失", unit)
		}
	}
}

// TestRuleSpecWindowPresetsParse 每个预设窗口都必须能被后端解析。
// 预设里塞一个后端拒绝的值，用户点一下就得到一个后端 400，且怪不到自己头上。
func TestRuleSpecWindowPresetsParse(t *testing.T) {
	h := newH(t)
	src := asset(t, h, "rule-spec.js")
	presets := jsStringArray(t, src, "WINDOWS")

	for _, wv := range presets {
		if _, _, err := limiter.ParseWindow(wv); err != nil {
			t.Errorf("预设窗口 %q 被后端 ParseWindow 拒绝：%v", wv, err)
		}
	}

	// 分族后的并集必须等于 WINDOWS，否则有预设永远进不了下拉
	code := stripJSComments(src)
	gm := regexp.MustCompile(`(?s)\bWINDOW_GROUPS\s*=\s*\[(.*?)\n\s*\];`).FindStringSubmatch(code)
	if gm == nil {
		t.Fatal("未找到 WINDOW_GROUPS")
	}
	var grouped []string
	for _, im := range regexp.MustCompile(`items:\s*\[([^\]]*)\]`).FindAllStringSubmatch(gm[1], -1) {
		for _, s := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(im[1], -1) {
			grouped = append(grouped, s[1])
		}
	}
	if !sameSet(grouped, presets) {
		t.Errorf("WINDOW_GROUPS 的并集与 WINDOWS 不等：分组 %v，全集 %v", grouped, presets)
	}

	// 分族判据必须是后端的 natural，不能凭「感觉时间长」分
	naturalGroup := map[string]bool{}
	for _, im := range regexp.MustCompile(`(?s)items:\s*\[([^\]]*)\]`).FindAllStringSubmatch(gm[1], -1) {
		_ = im
	}
	for _, g := range regexp.MustCompile(`(?s)\{[^{}]*label:\s*'([^']*)'[^{}]*items:\s*\[([^\]]*)\][^{}]*\}`).
		FindAllStringSubmatch(gm[1], -1) {
		isNaturalGroup := strings.Contains(g[1], "自然")
		for _, s := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(g[2], -1) {
			naturalGroup[s[1]] = isNaturalGroup
		}
	}
	for _, wv := range presets {
		_, natural, err := limiter.ParseWindow(wv)
		if err != nil {
			continue
		}
		if naturalGroup[wv] != natural {
			t.Errorf("窗口 %q 分族错误：前端归入自然族=%v，后端 natural=%v",
				wv, naturalGroup[wv], natural)
		}
	}
}

// TestRuleSpecDimensionsMatchBackend 维度白名单必须与 rule.go 的 validDims 集合相等。
// 前端少一个 → 合法配置无法在 UI 里表达；多一个 → UI 让用户选一个后端会拒的值。
func TestRuleSpecDimensionsMatchBackend(t *testing.T) {
	h := newH(t)
	js := jsStringArray(t, asset(t, h, "rule-spec.js"), "DIMS")

	// 直接用后端的常量，而不是抄一份字符串字面量
	want := []string{
		limiter.DimGlobal, limiter.DimBiz, limiter.DimPath,
		limiter.DimToken, limiter.DimIP, limiter.DimMethod,
	}
	if !sameSet(js, want) {
		t.Errorf("维度集合不一致：前端 %v，后端 %v", js, want)
	}

	// 每个维度都必须真的被 Validate 接受（validDims 是包私有，这里走公开路径验证）
	for _, d := range js {
		r := &limiter.Rule{
			Name: "t", Type: limiter.TypeRate, Dimensions: []string{d},
			Window: "1m", Limit: 10,
		}
		if err := r.Validate(); err != nil {
			t.Errorf("前端维度 %q 被后端 Validate 拒绝：%v", d, err)
		}
	}
}

// TestRuleSpecEnumsMatchBackend 类型 / 算法 / 计量三组枚举与 rule.go 常量比对。
func TestRuleSpecEnumsMatchBackend(t *testing.T) {
	h := newH(t)
	src := asset(t, h, "rule-spec.js")

	for _, tc := range []struct {
		name string
		want []string
	}{
		{"TYPES", []string{string(limiter.TypeRate), string(limiter.TypeConcurrency)}},
		{"METRICS", []string{string(limiter.MetricRequest), string(limiter.MetricToken)}},
		{"ALGOS", []string{
			string(limiter.AlgFixedWindow),
			string(limiter.AlgSlidingWindow),
			string(limiter.AlgTokenBucket),
		}},
	} {
		got := jsObjectValues(t, src, tc.name)
		if !sameSet(got, tc.want) {
			t.Errorf("%s 枚举不一致：前端 %v，后端 %v", tc.name, got, tc.want)
		}
	}
}

// TestRuleSpecKeyFormatMatchBackend Key 构造常量逐位比对。
//
// 用真函数产出的 Key 反解出分隔符与前缀，而不是从 key.go 抄字面量：
// sep / prefix 都是包私有常量，抄一份就等于多一个会漂移的副本。
func TestRuleSpecKeyFormatMatchBackend(t *testing.T) {
	h := newH(t)
	src := asset(t, h, "rule-spec.js")

	// rl|biz.path|order.v1|60|1755900000
	rate := limiter.RateKey([]string{"biz", "path"}, []string{"order", "v1"}, 60, 1755900000)
	parts := strings.Split(rate, jsStringField(t, src, "sep"))
	if len(parts) != 5 {
		t.Fatalf("用前端 sep 切分后端 RateKey 得到 %d 段（期望 5）：%q —— 分隔符不一致（提防全宽 U+2502）",
			len(parts), rate)
	}
	if got, want := jsStringField(t, src, "prefixRate"), parts[0]; got != want {
		t.Errorf("速率 Key 前缀不一致：前端 %q，后端 %q", got, want)
	}
	if got, want := jsStringField(t, src, "dimJoin"), "."; !strings.Contains(parts[1], want) ||
		got != want {
		t.Errorf("维度连接符不一致：前端 %q，后端实际 %q", got, want)
	}

	tb := limiter.TokenBucketKey([]string{"biz"}, []string{"order"})
	tbParts := strings.Split(tb, jsStringField(t, src, "sep"))
	if len(tbParts) != 3 {
		t.Errorf("TokenBucketKey 应为三段（不含窗口边界），实际 %q", tb)
	}
	if got := jsStringField(t, src, "prefixTokenBucket"); got != tbParts[0] {
		t.Errorf("令牌桶 Key 前缀不一致：前端 %q，后端 %q", got, tbParts[0])
	}

	cc := limiter.ConcurrencyKey("order", []string{"biz"}, []string{"order"})
	ccParts := strings.Split(cc, jsStringField(t, src, "sep"))
	if len(ccParts) != 4 {
		t.Errorf("ConcurrencyKey 应为四段（首段 biz），实际 %q", cc)
	}
	if got := jsStringField(t, src, "prefixConcurrency"); got != ccParts[0] {
		t.Errorf("并发 Key 前缀不一致：前端 %q，后端 %q", got, ccParts[0])
	}

	tk := limiter.TokenLedgerKey([]string{"biz"}, []string{"order"}, 60, 1755900000)
	tkParts := strings.Split(tk, jsStringField(t, src, "sep"))
	if len(tkParts) != 5 {
		t.Errorf("TokenLedgerKey 应为五段，实际 %q", tk)
	}
	if got := jsStringField(t, src, "prefixTokenLedger"); got != tkParts[0] {
		t.Errorf("Token 账本 Key 前缀不一致：前端 %q，后端 %q", got, tkParts[0])
	}

	// global 的空值：设计稿曾写成 '-'，后端是 '_'
	empty := limiter.RateKey([]string{"global"}, []string{""}, 60, 0)
	emptyVal := strings.Split(empty, jsStringField(t, src, "sep"))[2]
	if got := jsStringField(t, src, "emptyVal"); got != emptyVal {
		t.Errorf("空维度值不一致：前端 %q，后端 %q", got, emptyVal)
	}

	// 哈希降级：长值形态为 hashPrefix + hashHexLen 个 hex
	long := strings.Repeat("x", 64)
	hashed := strings.Split(
		limiter.RateKey([]string{"path"}, []string{long}, 60, 0),
		jsStringField(t, src, "sep"))[2]
	wantPrefix := jsStringField(t, src, "hashPrefix")
	if !strings.HasPrefix(hashed, wantPrefix) {
		t.Errorf("哈希前缀不一致：前端 %q，后端产出 %q", wantPrefix, hashed)
	}
	if got, want := jsNumberField(t, src, "hashHexLen"), itoa(int64(len(hashed)-len(wantPrefix))); got != want {
		t.Errorf("哈希 hex 长度不一致：前端 %s，后端产出 %s", got, want)
	}

	// maxRawLen 边界：正好 48 保持明文，49 触发哈希
	maxRaw := jsNumberField(t, src, "maxRawLen")
	n := atoi(t, maxRaw)
	keep := strings.Split(
		limiter.RateKey([]string{"path"}, []string{strings.Repeat("a", n)}, 60, 0),
		jsStringField(t, src, "sep"))[2]
	if keep != strings.Repeat("a", n) {
		t.Errorf("长度 %d 的值应保持明文，后端却降级为 %q —— 前端 maxRawLen 偏大", n, keep)
	}
	over := strings.Split(
		limiter.RateKey([]string{"path"}, []string{strings.Repeat("a", n+1)}, 60, 0),
		jsStringField(t, src, "sep"))[2]
	if over == strings.Repeat("a", n+1) {
		t.Errorf("长度 %d 的值后端仍保持明文 —— 前端 maxRawLen 偏小", n+1)
	}
}

// TestRuleSpecNormalizationDefaults 规范化默认值与 rule.go 的落库值一致。
// UI 承诺「留空按 N 落库」，这个 N 错了就是对用户说了假话。
func TestRuleSpecNormalizationDefaults(t *testing.T) {
	h := newH(t)
	src := asset(t, h, "rule-spec.js")

	// watermark 越界 → 80
	r := &limiter.Rule{
		Name: "t", Type: limiter.TypeRate, Dimensions: []string{"biz"},
		Window: "1m", Limit: 10, Watermark: 0,
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := jsNumberField(t, src, "watermark"); got != itoa(int64(r.Watermark)) {
		t.Errorf("watermark 默认值不一致：前端 %s，后端 %d", got, r.Watermark)
	}

	// timeout <= 0 → 120（仅并发分支）
	c := &limiter.Rule{
		Name: "t", Type: limiter.TypeConcurrency, Dimensions: []string{"biz"},
		MaxConc: 5, TimeoutSec: 0,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := jsNumberField(t, src, "timeoutSec"); got != itoa(c.TimeoutSec) {
		t.Errorf("timeout 默认值不一致：前端 %s，后端 %d", got, c.TimeoutSec)
	}

	// burst <= 0 → limit
	b := &limiter.Rule{
		Name: "t", Type: limiter.TypeRate, Dimensions: []string{"biz"},
		Window: "1m", Limit: 600, Algorithm: limiter.AlgTokenBucket, Burst: 0,
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if b.Burst != 600 {
		t.Errorf("burst<=0 应规范化为 limit，实际 %d", b.Burst)
	}

	// 空 metric / 空 algorithm 的落库值
	e := &limiter.Rule{
		Name: "t", Type: limiter.TypeRate, Dimensions: []string{"biz"},
		Window: "1m", Limit: 10,
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := jsStringField(t, src, "metric"); got != string(e.Metric) {
		t.Errorf("metric 默认值不一致：前端 %q，后端 %q", got, e.Metric)
	}
	if got := jsStringField(t, src, "algorithm"); got != string(e.Algorithm) {
		t.Errorf("algorithm 默认值不一致：前端 %q，后端 %q", got, e.Algorithm)
	}

	// 滑动窗口上限：前端常量必须正好是后端的临界点
	maxStr := jsNumberField(t, src, "slidingLimitMax")
	max := int64(atoi(t, maxStr))
	ok := &limiter.Rule{
		Name: "t", Type: limiter.TypeRate, Dimensions: []string{"biz"},
		Window: "1m", Limit: max, Algorithm: limiter.AlgSlidingWindow,
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("limit=%d（前端上限）应被后端接受，实际拒绝：%v", max, err)
	}
	bad := &limiter.Rule{
		Name: "t", Type: limiter.TypeRate, Dimensions: []string{"biz"},
		Window: "1m", Limit: max + 1, Algorithm: limiter.AlgSlidingWindow,
	}
	if err := bad.Validate(); err == nil {
		t.Errorf("limit=%d 应被后端拒绝，实际通过 —— 前端上限偏小", max+1)
	}
}

// TestNoFullWidthSeparatorInAssets 全宽竖线 U+2502 专项门禁。
//
// 单独一条测试而不是并入 emoji 检查：emoji 的扫描区间刻意不含制表符区，
// 扩大那个区间会牵连无关字符。这里只钉死这一个字符，因为它是唯一一个
// 「看起来对、实际让预览出的 Key 全错」的字符，且设计稿里真的出现过。
func TestNoFullWidthSeparatorInAssets(t *testing.T) {
	h := newH(t)
	for name, body := range h.files {
		if i := strings.IndexRune(string(body), '\u2502'); i >= 0 {
			t.Errorf("资产 %s 第 %d 字节处出现全宽竖线 U+2502 —— 分隔符必须是 ASCII 半角 '|'",
				name, i)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		return "-" + string(buf)
	}
	return string(buf)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("无法解析数字 %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ---------------------------------------------------------------------------
// 以下是校验器与预览层的**结构性**护栏。
//
// 它们不执行 JS，检测的是「源码里必须/不得出现某个模式」。这类断言看起来
// 粗糙，但针对的是我在实现期真实踩过、且只能靠源码形态拦住的三个陷阱 ——
// 它们的共同点是：写错了照样跑得通，只是结论悄悄错了。
// ---------------------------------------------------------------------------

// TestBurstFloorUsesIntegerTruncation burst 下限必须用整除截断。
//
// 后端 rule.go:171 是 `r.Burst < r.Limit/r.winSec`，Go 的 int64 相除直接截断。
// JS 里除法得到浮点，必须显式 Math.floor。用 ceil 或 toFixed 会让前端在
// limit 不能被 winSec 整除时比后端更严 —— 拦住后端本会接受的配置，
// 而症状是「界面说 burst 太小，但同样的值线上明明在跑」。
func TestBurstFloorUsesIntegerTruncation(t *testing.T) {
	src := asset(t, newH(t), "rule-validate.js")

	if !strings.Contains(src, "Math.floor(limit / winSec)") {
		t.Error("rule-validate.js 的 burstFloor 必须用 Math.floor(limit / winSec)：" +
			"后端 rule.go:171 是 int64 整除，会截断小数")
	}
	code := stripJSComments(src)
	for _, bad := range []string{"Math.ceil(limit", "Math.round(limit", "toFixed"} {
		if strings.Contains(code, bad) {
			t.Errorf("rule-validate.js 出现 %q —— burst 下限只能向下取整，"+
				"其他取整方式会让前端比后端更严", bad)
		}
	}

	// 用后端真值反向确认「截断」这件事本身在边界上是有效的：
	// limit=3601 / winSec=3600 截断为 1，burst=1 必须被后端接受。
	// 若前端用 ceil 会得到 2 从而误判 burst=1 非法。
	r := limiter.Rule{
		Name: "t", Type: "rate", Metric: "request", Algorithm: "token_bucket",
		Dimensions: []string{"biz"}, Window: "1h", Limit: 3601, Burst: 1,
	}
	if err := r.Validate(); err != nil {
		t.Errorf("后端应接受 limit=3601 window=1h burst=1（3601/3600 截断为 1），实际拒绝：%v", err)
	}
}

// TestBurstFloorGuardsZeroWindow burst 下限必须在窗口无效时返回「跳过」。
//
// JS 里 10/0 === Infinity，而 1 < Infinity 为真。若不显式拦 winSec<=0，
// 窗口填 'abc' 时前端会报「突发容量过小」，而后端报的是「窗口格式非法」——
// 用户按前端提示去改 burst，永远改不对。
//
// 前置条件不满足时正确的结论是「跳过」：既不是通过（那会漏掉真问题），
// 也不是失败（那会指错方向）。
func TestBurstFloorGuardsZeroWindow(t *testing.T) {
	src := asset(t, newH(t), "rule-validate.js")

	fn := funcBody(t, src, "burstFloor")
	if !strings.Contains(fn, "winSec <= 0") {
		t.Error("burstFloor 必须显式判 winSec <= 0 并返回 null：" +
			"JS 中 limit/0 为 Infinity，任何有限的 burst 都会小于它，导致误报")
	}
	if !strings.Contains(fn, "return null") {
		t.Error("burstFloor 在前置条件不满足时必须返回 null（表示无法判定），" +
			"不能返回一个数 —— 返回数会被下游当成真实下限")
	}

	// 后端行为确认：窗口非法时报的是窗口，不是 burst
	r := limiter.Rule{
		Name: "t", Type: "rate", Metric: "request", Algorithm: "token_bucket",
		Dimensions: []string{"biz"}, Window: "abc", Limit: 3600, Burst: 1,
	}
	err := r.Validate()
	if err == nil {
		t.Fatal("后端应拒绝 window=abc")
	}
	if strings.Contains(err.Error(), "burst") {
		t.Errorf("后端对 window=abc 报的应是窗口问题而非 burst，实际：%v", err)
	}
}

// TestBurstLowerBoundRequiresPositiveBurst burst<=0 不得走下限判定。
//
// 后端 rule.go:168 先把 burst<=0 规范化成 Limit，:171 才比下限。所以
// burst=0 永远不会触发「过小」。前端若漏掉 burst>0 这个前提，会报出一个
// 后端根本不存在的错误 —— 而默认值恰好就是 0，等于每条新建的令牌桶规则
// 一打开就是红的。
func TestBurstLowerBoundRequiresPositiveBurst(t *testing.T) {
	// 后端确认：burst=0 + 一个远小于下限的值，都不该报 burst 过小
	for _, burst := range []int64{0, -1} {
		r := limiter.Rule{
			Name: "t", Type: "rate", Metric: "request", Algorithm: "token_bucket",
			Dimensions: []string{"biz"}, Window: "1s", Limit: 5000, Burst: burst,
		}
		if err := r.Validate(); err != nil {
			t.Errorf("burst=%d 应被规范化为 limit 而非报错，实际：%v", burst, err)
		}
	}
	// 而 burst=1 且下限为 5000 时后端确实会拒 —— 证明上面不是因为整条规则被跳过
	r := limiter.Rule{
		Name: "t", Type: "rate", Metric: "request", Algorithm: "token_bucket",
		Dimensions: []string{"biz"}, Window: "1s", Limit: 5000, Burst: 1,
	}
	if err := r.Validate(); err == nil {
		t.Fatal("burst=1 且下限 5000 时后端应拒绝，否则本测试的对照组无效")
	}

	src := asset(t, newH(t), "rule-validate.js")
	if !strings.Contains(src, "burst <= 0") {
		t.Error("rule-validate.js 必须显式区分 burst<=0（规范化）与 burst>0（比下限）：" +
			"后端 rule.go:168 先规范化后比较，漏掉会误报一个不存在的错误")
	}
}

// TestConcurrencyShortCircuits concurrency 分支必须整体短路。
//
// 后端 rule.go:129-137 检查完 MaxConc 即 return nil，
// limit / window / metric / algorithm 四项**完全不校验**。
// 前端若无条件校验，会拦住后端本会接受的配置。
func TestConcurrencyShortCircuits(t *testing.T) {
	// 先用后端真函数确认这个短路是真的：四项全部非法但仍通过
	r := limiter.Rule{
		Name: "t", Type: "concurrency", MaxConc: 5,
		Dimensions: []string{"biz"},
		Limit:      -5, Window: "1.5h", Metric: "bogus", Algorithm: "bogus",
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("后端应完全不校验 concurrency 的 limit/window/metric/algorithm，实际：%v", err)
	}

	src := asset(t, newH(t), "rule-validate.js")
	fn := funcBody(t, src, "validate")

	// 短路必须发生在 window/limit 校验之前，否则等于没短路
	iConc := strings.Index(fn, "'concurrency'")
	if iConc < 0 {
		t.Fatal("validate 中找不到 concurrency 分支")
	}
	iReturn := -1
	for _, form := range []string{"return finish(", "return {"} {
		if i := strings.Index(fn[iConc:], form); i >= 0 {
			iReturn = iConc + i
			break
		}
	}
	if iReturn < 0 {
		t.Fatal("concurrency 分支必须提前 return，不能与 rate 校验共用后续流程")
	}

	for _, later := range []string{"parseWindow", "burstFloor", "slidingLimitMax"} {
		if i := strings.Index(fn, later); i >= 0 && i < iReturn {
			t.Errorf("%q 出现在 concurrency 分支 return 之前（偏移 %d < %d）："+
				"后端在该类型下根本不执行这些检查", later, i, iReturn)
		}
	}

	// 跳过项必须被显式记录，否则「不校验」会被展示成「校验通过」
	if !strings.Contains(fn[iConc:iReturn], "skipped.push") {
		t.Error("concurrency 分支必须把 limit/window/metric/algorithm 记入 skipped：" +
			"不校验与校验通过是两回事，后者会让人以为填错也无所谓")
	}
}

// TestPreviewBranchOrderMatchesLimiter 键预览的分支顺序必须与 limiter.go 一致。
//
// limiter.go:226 的 `Type==rate && Metric==token` 早于 :236 的
// `Algorithm==token_bucket`。顺序调换后，metric=token+token_bucket 的规则
// 会预览出 tb 键，而线上真实存在的是 tk 键 —— 运维拿它去 SCAN 一个都查不到。
func TestPreviewBranchOrderMatchesLimiter(t *testing.T) {
	src := asset(t, newH(t), "rule-preview.js")
	fn := funcBody(t, src, "branchOf")

	iTk := strings.Index(fn, "'tk'")
	iCc := strings.Index(fn, "'cc'")
	iTb := strings.Index(fn, "'tb'")
	iSliding := strings.Index(fn, "'rl_sliding'")
	for name, i := range map[string]int{"tk": iTk, "cc": iCc, "tb": iTb, "rl_sliding": iSliding} {
		if i < 0 {
			t.Fatalf("branchOf 中找不到分支 %q", name)
		}
	}
	if !(iTk < iCc && iCc < iTb && iTb < iSliding) {
		t.Errorf("branchOf 分支顺序必须是 tk → cc → tb → rl_sliding → rl_fixed"+
			"（照抄 limiter.go:220-256 的 switch），实际偏移 tk=%d cc=%d tb=%d sliding=%d",
			iTk, iCc, iTb, iSliding)
	}

	// tb 与 cc 分支不含窗口边界段（limiter.go 没给它们算 WindowBoundary）
	for _, id := range []string{"'tb'", "'cc'"} {
		i := strings.Index(fn, id)
		seg := fn[i:min(i+120, len(fn))]
		if !strings.Contains(seg, "boundary: false") {
			t.Errorf("分支 %s 必须标记 boundary: false —— "+
				"后端对该分支不调用 WindowBoundary，多一段会让键查不到", id)
		}
	}
}

// TestPreviewNeverGuessesTimezone 自然窗口的边界值不得被推算。
//
// 业务时区偏移来自网关进程的 TZ_OFFSET_SECONDS，前端无合法途径获取
// （无 policy key，snapshot 刻意不含该字段）。显示一个用浏览器本地时区
// 推算出来的数字，比不显示更糟：它看着像真的，复制去 Redis 查不到。
func TestPreviewNeverGuessesTimezone(t *testing.T) {
	src := asset(t, newH(t), "rule-preview.js")
	fn := funcBody(t, src, "boundarySeg")

	// 函数体按 `!win.natural` 的守卫拆成两段：守卫内是滚动窗口（算真值），
	// 守卫之后是自然窗口（必须占位）。
	iGuard := strings.Index(fn, "!win.natural")
	if iGuard < 0 {
		t.Fatal("boundarySeg 中找不到 !win.natural 守卫")
	}
	iGuardEnd := strings.Index(fn[iGuard:], "}")
	if iGuardEnd < 0 {
		t.Fatal("boundarySeg 的 !win.natural 守卫块未闭合")
	}
	rolling := fn[iGuard : iGuard+iGuardEnd] // 滚动窗口分支
	natural := fn[iGuard+iGuardEnd:]         // 自然窗口分支

	if !strings.Contains(natural, "PLACEHOLDER") {
		t.Error("boundarySeg 的 natural 分支必须返回占位常量：" +
			"业务时区偏移在前端不可知，推算出的数字是错的")
	}
	if strings.Contains(natural, "Date.now()") || strings.Contains(natural, "getTimezoneOffset") {
		t.Errorf("boundarySeg 的 natural 分支不得读取当前时间或浏览器时区："+
			"浏览器时区与网关的 TZ_OFFSET_SECONDS 无关，用它推算等于编造。实际：%s", natural)
	}

	// 反面：滚动窗口分支必须给真值（这是预览可复制性的全部依据）
	if !strings.Contains(rolling, "win.sec") {
		t.Errorf("boundarySeg 的非 natural 分支必须用 win.sec 算出真实边界："+
			"滚动窗口与时区无关，前端算得出与后端逐位一致的值。实际：%s", rolling)
	}
	// 且必须向下取整对齐，不能用 ceil/round —— 后端 key.go:76 是整除
	if !strings.Contains(rolling, "Math.floor") {
		t.Errorf("滚动窗口边界必须用 Math.floor 对齐（后端 key.go:76 是整除），实际：%s", rolling)
	}

	// 占位符必须是半角尖括号包裹，与其他运行期取值记号一致，
	// 且绝不能长得像数字（否则会被误当成可复制的真值）
	for _, name := range []string{"PLACEHOLDER_D", "PLACEHOLDER_W"} {
		v := jsVarString(t, src, name)
		if !strings.HasPrefix(v, "<") || !strings.HasSuffix(v, ">") {
			t.Errorf("%s = %q 必须用半角尖括号包裹，以区别于可复制的真值", name, v)
		}
		if regexp.MustCompile(`^<?[0-9]+>?$`).MatchString(v) {
			t.Errorf("%s = %q 不能长得像数字，否则会被误当成可复制的真实边界值", name, v)
		}
	}
}

// TestFormDefersToLocalValidation 表单必须先本地判定再决定是否请求后端。
//
// 本地校验是纯函数、微秒级；后端是网络往返。若无条件发请求，「校验中」的
// 转圈会盖住已经算出来的本地结论，用户白等 400ms 看到同一个答案。
func TestFormDefersToLocalValidation(t *testing.T) {
	src := asset(t, newH(t), "page-rules-form.js")

	if !strings.Contains(src, "RuleValidate") && !strings.Contains(src, "V.validate") {
		t.Error("page-rules-form.js 必须调用本地校验器 RuleValidate.validate")
	}
	if !strings.Contains(src, "localBlocked") {
		t.Error("page-rules-form.js 需要一个本地结论判定（localBlocked），" +
			"用于在本地不通过时跳过后端请求")
	}
	// 本地不通过时必须取消待发请求，否则 debounce 里排队的那次还会发出去
	if !strings.Contains(src, "clearTimeout") {
		t.Error("本地不通过时必须 clearTimeout 取消已排队的后端校验")
	}
	// 漂移判定必须喂未翻译的原文：translateRuleError 的输出无法做模式匹配
	iCovered := strings.Index(src, "backendErrorCovered")
	if iCovered < 0 {
		t.Error("page-rules-form.js 必须用 backendErrorCovered 做镜像漂移判定：" +
			"后端报错而本地放行时，需要显式告警，否则前端校验落后于 rule.go 无人察觉")
	} else {
		// 取该调用所在行，确认参数不是 translate 过的
		lineStart := strings.LastIndex(src[:iCovered], "\n") + 1
		lineEnd := iCovered + strings.Index(src[iCovered:], "\n")
		line := src[lineStart:lineEnd]
		if strings.Contains(line, "translateRuleError") {
			t.Errorf("backendErrorCovered 必须接收未翻译的后端原文，实际：%s", line)
		}
	}
}

// TestFormDoesNotRebuildOnInput 输入时不得重建字段区。
//
// 每敲一个字符就重建 DOM 会让输入框失焦、输入法候选被打断 —— 一个连
// 「窗口填 abc」都打不完的表单，校验做得再准也没用。
func TestFormDoesNotRebuildOnInput(t *testing.T) {
	src := asset(t, newH(t), "page-rules-form.js")

	fn := funcBody(t, src, "revalidate")
	for _, bad := range []string{"rebuild()", "U.clear(fields)", "U.clear(body)"} {
		if strings.Contains(fn, bad) {
			t.Errorf("revalidate（输入时调用）中出现 %q —— "+
				"重建字段区会让正在输入的控件失焦", bad)
		}
	}
	// 而 set（切类型/切算法，会改变字段可见性）必须重建
	setFn := funcBody(t, src, "set")
	if setFn != "" && !strings.Contains(setFn, "rebuild") {
		t.Error("ctx.set 改变的是影响字段可见性的值，必须重建字段区")
	}
}

// funcBody 抽出一个 JS 函数的函数体（到同缩进的 '}' 为止）。
//
// 用括号配对而非正则：函数体里必然含嵌套的 {}，正则要么贪婪吃掉后面的函数、
// 要么在第一个内层 '}' 就截断，两种错法都会让断言看似通过。
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	pat := regexp.MustCompile(`(?m)^\s*(?:function\s+` + regexp.QuoteMeta(name) +
		`\s*\(|` + regexp.QuoteMeta(name) + `:\s*function\s*\()`)
	loc := pat.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("在资产中找不到函数 %q", name)
	}
	open := strings.Index(src[loc[1]:], "{")
	if open < 0 {
		t.Fatalf("函数 %q 后找不到 '{'", name)
	}
	start := loc[1] + open
	depth := 0
	inStr := byte(0)
	for i := start; i < len(src); i++ {
		c := src[i]
		if inStr != 0 {
			if c == '\\' {
				i++
			} else if c == inStr {
				inStr = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			inStr = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("函数 %q 的括号未配平", name)
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// jsVarString 抽出 `var NAME = '...'` 形式的字符串常量。
// jsStringField 只认对象字面量里的 `key: '...'`，模块级 var 抽不到。
func jsVarString(t *testing.T, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(name) +
		`\s*=\s*'([^']*)'`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("未在资产中找到 var %s 的字符串定义", name)
	}
	return m[1]
}

// 剥注释复用同包 scan_test.go 已有的 stripJSComments（只剥注释、保留字符串
// 字面量），语义与本文件所需完全一致，不再另立一份 —— 同包重名会直接编译失败。
// 为什么禁用词扫描必须先剥注释，见那边的函数注释：本仓库刻意在注释里写明
// 「不可用 ceil 或 toFixed」这类反面模式，那是文档而非缺陷；不剥注释会把这类
// 记录判成违规，逼后来者删掉最该留下的东西。

// TestNameCheckDoesNotTrim 钉死 name 必填校验**不得** trim 后判空。
//
// 来源是 qa-diff 的 140 组差分测试里 7 条误报（单/双空格、tab、换行、U+3000、
// 空白 + concurrency 分支）：后端 rule.go:107 是 `r.Name == ""`，不 trim，
// 纯空白 name 会被放行并真的落库。前端若 trim 后判空，就拦住了后端会接受的
// 配置 —— 这正是「前端比后端更严」那一类，症状是「界面说不行但线上明明在跑」，
// 比漏报难查得多。
//
// 这条测试盯的是「宁松不紧」这个方向性约束，不是某个具体文案。谁要是觉得
// 「空白名字显然该拦」而顺手加上 trim，会在这里被挡下来 —— 该拦的地方是后端，
// 前端镜像层无权比权威源更严。
func TestNameCheckDoesNotTrim(t *testing.T) {
	// 先确认后端确实不 trim，避免这条测试在后端改了语义后仍然「正确地」错着
	for _, blank := range []string{" ", "  ", "\t", "\n", "\u3000"} {
		r := &limiter.Rule{
			Name: blank, Type: "rate", Dimensions: []string{"biz"},
			Limit: 100, Window: "1m",
		}
		if err := r.Validate(); err != nil {
			t.Fatalf("后端语义已变：name=%q 现在被拒（%v）。"+
				"若后端开始 trim，前端镜像也要同步放开这条断言", blank, err)
		}
	}

	src := stripJSComments(asset(t, newH(t), "rule-validate.js"))
	body := funcBody(t, src, "validate")

	// name 判空所在的那几行不得出现 trim
	for _, ln := range strings.Split(body, "\n") {
		if !strings.Contains(ln, "name") {
			continue
		}
		if strings.Contains(ln, "trim") || strings.Contains(ln, "Trim") {
			t.Errorf("name 校验用了 trim：%s\n"+
				"后端 rule.go:107 是 r.Name == \"\" 不 trim，"+
				"trim 后判空会拦住后端接受的纯空白 name（qa-diff 差分测试 7 条误报）",
				strings.TrimSpace(ln))
		}
	}

	// 正向：必须存在对真空串的判定，否则「不 trim」退化成「根本不校验」
	if !strings.Contains(body, "name_required") {
		t.Error("name 必填校验缺失：不 trim 不等于不校验，真空串仍须阻断")
	}
}
