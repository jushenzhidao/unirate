package adminui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/unirate/gateway/internal/limiter"
)

// RULE_ERR_MAP 双向门禁。
//
// Go 测不了 JS，所以从 api.js 抽出 RULE_ERR_MAP 的正则字面量，在 Go 侧编译成
// regexp 再比对。真值来源是 limiter.Rule.Validate() 的实际返回值 —— 每条 case
// 都真的调 Validate() 拿 error 字符串，而不是在测试里手抄一份期望文案。
// 手抄的那份会和映射表一起错，等于自己给自己发合格证；更糟的是后端改文案时
// 测试依然全绿，映射表静默失效。
//
// 这条测试是在修一个已经发生的腐化：原映射表 16 条规则里 15 条匹配不到任何
// 真实错误，14 条后端错误只有 1 条能翻出中文。三类互不相同的根因：
//
//  1. `^` 锚定失效 —— 除 rule.go:108 的 name 判空外，每条错误都被
//     `fmt.Errorf("rule %q: ...")` 包装，实际到达前端的是
//     `rule "api-limit": limit must be > 0`，开头是 `rule "` 而不是 `limit`。
//     `^limit must be` 这类正则永远匹配不到。
//  2. 词形不符 —— 表里写 `combined`，后端实际是 `combine`；表里写
//     `rule name is required`，后端实际是 `rule name required`。
//  3. 结构上不可达 —— 表里有 `^watermark` 和 `^timeout`，而这两个字段在后端
//     是静默规范化（rule.go:135-137、:180-183），永远不会产生 error。
//
// 三类根因的共同点是「写错了不报错」：映射不中就回落到显示英文原文，页面照样
// 渲染，没有任何一处会失败。这类静默失效必须由测试兜住。
//
// 反向断言（死规则检测）同样重要：一条匹配不到任何真实错误的正则，会让下一个
// 人以为某个后端错误已经覆盖了，从而不去补真正缺失的那条。

// 正向：Validate() 的每条真实错误都必须能被映射表命中。
// 覆盖 rule.go:104-190 的 13 条阻断校验（窗口一条拆格式/单位两个 case）。
func TestRuleErrMapCoversEveryBackendError(t *testing.T) {
	pats := ruleErrPatterns(t)
	hit := make([]bool, len(pats))

	for _, c := range ruleErrCases() {
		r := c.rule
		err := r.Validate()
		if err == nil {
			t.Errorf("%s：期望 Validate() 返回 error，实际通过 —— "+
				"这条 case 已失去意义，需要按后端当前语义重写", c.label)
			continue
		}
		msg := err.Error()
		matched := -1
		for i, p := range pats {
			if p.MatchString(msg) {
				matched = i
				hit[i] = true
				break
			}
		}
		if matched < 0 {
			t.Errorf("%s：后端错误 %q 在 RULE_ERR_MAP 中无匹配 —— "+
				"用户会看到英文原文。注意后端错误带 `rule \"名字\": ` 前缀，"+
				"正则不能用 ^ 锚定业务文案", c.label, msg)
		}
	}

	// 反向：不允许死规则。
	for i, p := range pats {
		if !hit[i] {
			t.Errorf("RULE_ERR_MAP 第 %d 条 %s 匹配不到任何真实后端错误 —— "+
				"死规则会让人误以为该错误已覆盖，应删除或按真实文案修正",
				i+1, p.String())
		}
	}
}

// ruleErrCases 每条 case 都必须真的触发 Validate() 返回 error。
//
// 构造时注意 rule.go 的控制流：前置字段必须填到「刚好越过前面所有校验」，
// 否则会撞上更早的那条 return，测到的不是想测的那条。例如测 burst 过小
// 必须先给合法的 window 和 limit，否则会先撞上 :142 的 limit 判零。
func ruleErrCases() []struct {
	label string
	rule  limiter.Rule
} {
	return []struct {
		label string
		rule  limiter.Rule
	}{
		{"name 空", limiter.Rule{}},
		{"维度空", limiter.Rule{Name: "a", Type: limiter.TypeRate}},
		{"未知维度", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"nope"}}},
		{"维度重复", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz", "biz"}}},
		{"global 组合", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"global", "biz"}}},
		{"max_concurrent<=0", limiter.Rule{Name: "a", Type: limiter.TypeConcurrency,
			Dimensions: []string{"biz"}}},
		{"limit<=0", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricRequest, Window: "1m"}},
		{"窗口格式非法", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricRequest,
			Window: "1.5h", Limit: 10}},
		{"窗口单位非法", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricRequest,
			Window: "10x", Limit: 10}},
		{"未知 metric", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: "bogus", Window: "1m", Limit: 10}},
		{"token_bucket+token", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricToken, Window: "1m",
			Limit: 10, Algorithm: limiter.AlgTokenBucket}},
		{"burst 过小", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricRequest, Window: "1m",
			Limit: 6000, Algorithm: limiter.AlgTokenBucket, Burst: 3}},
		{"滑动窗口超限", limiter.Rule{Name: "a", Type: limiter.TypeRate,
			Dimensions: []string{"biz"}, Metric: limiter.MetricRequest, Window: "1m",
			Limit: 200001, Algorithm: limiter.AlgSlidingWindow}},
		{"未知 type", limiter.Rule{Name: "a", Type: "bogus",
			Dimensions: []string{"biz"}}},
	}
}

// ruleErrPatterns 从 api.js 抽出 RULE_ERR_MAP 的正则字面量，转成 Go 正则。
//
// 只抽 /.../ 字面量的 pattern 部分。JS 与 Go 的正则语法在这张表用到的子集
// （字面量、\d、[]、|、()）上一致，不一致的写法会在 MustCompile 处直接炸，
// 而不是静默按不同语义匹配。
func ruleErrPatterns(t *testing.T) []*regexp.Regexp {
	t.Helper()
	src := stripJSComments(asset(t, newH(t), "api.js"))
	start := strings.Index(src, "RULE_ERR_MAP")
	if start < 0 {
		t.Fatal("api.js 中未找到 RULE_ERR_MAP")
	}
	body := src[start:]
	if end := strings.Index(body, "];"); end > 0 {
		body = body[:end]
	}
	re := regexp.MustCompile(`\[\s*/((?:[^/\\\n]|\\.)+)/([a-z]*)\s*,`)
	ms := re.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		t.Fatal("RULE_ERR_MAP 中未抽到任何正则字面量 —— 表结构可能已变，护栏失效")
	}
	out := make([]*regexp.Regexp, 0, len(ms))
	for _, m := range ms {
		pat := m[1]
		if strings.Contains(m[2], "i") {
			pat = "(?i)" + pat
		}
		out = append(out, regexp.MustCompile(pat))
	}
	return out
}

// TestRuleErrMapHasNoDeadPatterns 是 TestRuleErrMapCoversEveryBackendError 的反向。
//
// 这条比正向更重要。这张表曾腐化到 16 条里 15 条命中不到任何真实后端错误，
// 根因就是只有「新加的能命中」这一个方向的保障：后端改了文案、或某条校验被
// 删掉，旧正则就静默变成死规则，而正向测试全程是绿的。没有反向门禁，这张表
// 会以完全相同的方式再烂一遍。
//
// 用例集与正向共用 ruleErrCases() —— 两处各维护一份迟早漂移。
// 错误文案一律取自真实 Validate() 返回值，不手抄：手抄的话后端改了文案，
// 这里仍然「正确地」绿着。
//
// 前提：规则相关的 4xx 错误来源仅 Validate()。若将来新增来源（重名、存储层、
// JSON 解析失败等），必须同时补进 ruleErrCases()，否则针对新来源写的正则会
// 在这里被误判成死规则。
func TestRuleErrMapHasNoDeadPatterns(t *testing.T) {
	var msgs []string
	for _, c := range ruleErrCases() {
		r := c.rule
		err := r.Validate()
		if err == nil {
			t.Fatalf("用例 %q 未触发错误 —— 该用例已失效，"+
				"两个方向的测试都会因此漏掉一条规则", c.label)
		}
		msgs = append(msgs, err.Error())
	}

	pats := ruleErrPatterns(t)
	for _, p := range pats {
		hit := false
		for _, m := range msgs {
			if p.MatchString(m) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("死规则：正则 /%s/ 命中不了任何真实后端错误。\n"+
				"两种可能：(a) 正则写错了 —— 注意曾出现 `combined` 与后端实际的 "+
				"`combine` 词形不匹配这类问题；(b) 它对应的后端校验已不存在，该删。\n"+
				"前者改正则，后者删表项。当前用例集覆盖 %d 条后端错误。",
				p.String(), len(msgs))
		}
	}
}
