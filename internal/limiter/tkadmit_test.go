package limiter

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Token 预算准入合并进 batch_check 后的定向测试（ADR-003）。
//
// 针对的是「合并」这一改动最可能引入的沉默逻辑错误：
// 判定看起来对、测试也绿，但配额悄悄没扣或多扣。

func tkRule(t *testing.T, name string, limit int64) *Rule {
	return mkRule(t, &Rule{
		Name: name, Type: TypeRate, Metric: MetricToken,
		Dimensions: []string{DimBiz, DimToken}, Window: "1h", Limit: limit,
		Algorithm: AlgFixedWindow,
	})
}

func tokenKeyOf(l *Limiter, r *Rule, m *Meta) string {
	b := WindowBoundary(time.Now(), r.WindowSeconds(), l.tzOffset, r.IsNaturalWindow())
	return TokenLedgerKey(r.Dimensions, m.dimValues(r.Dimensions), r.WindowSeconds(), b)
}

// TestTkAdmitBlocksWhenExhausted 预算耗尽时 Check 必须拒绝。
//
// 这条断言原先由 TokenAdmit 保证（走独立往返）。合并后责任转移到
// batch_check 的 Phase 1，必须重新证明它仍成立 —— 否则 Token 预算形同虚设，
// 对接大模型上游时等同「烧钱开关」。
func TestTkAdmitBlocksWhenExhausted(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := tkRule(t, "tk-budget", 1000)
	rules := []*Rule{r}
	m := meta("tk-1")

	d, _, err := l.Check(ctx, rules, m, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !d.Allowed {
		t.Fatalf("预算未消耗时应放行，实际被 %q 拒绝", d.RuleName)
	}

	// 关键：放行后账本不得有任何增加。
	// Token 属事后计量，准入期只读；若 Phase 2 误写就会双重计费。
	key := tokenKeyOf(l, r, m)
	if n, err := rdb.Exists(ctx, key).Result(); err == nil && n != 0 {
		v, _ := rdb.Get(ctx, key).Result()
		t.Fatalf("准入不得写账本，但 %s 已存在（值=%q）", key, v)
	}

	if err := l.TokenReserve(ctx, rules, m, 1000); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	d, _, err = l.Check(ctx, rules, m, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if d.Allowed {
		t.Fatal("预算耗尽后仍放行 —— Token 预算失效")
	}
	if d.RuleName != "tk-budget" {
		t.Fatalf("拒绝规则名应为 tk-budget，实际 %q", d.RuleName)
	}
}

// TestTkAdmitDoesNotBreakLaterRuleCommit 是本次改动最关键的回归测试。
//
// batch_check.lua 的 Phase 2 用 ipairs 遍历 plans，而 ipairs 遇到第一个 nil 即停止。
// tkadmit 是「只读不写」的规则，若它不在 plans 中占位就会留下空洞，
// 导致**其后所有规则的提交被静默跳过** —— 判定通过但配额没扣，
// 即 P0-1 配额放大的复现，且不报错、不崩溃。
//
// 因此这里刻意把 token 规则排在 fixed 规则**之前**：
// 若占位逻辑被误删，本测试会因「fixed 规则永不超限」而失败。
func TestTkAdmitDoesNotBreakLaterRuleCommit(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	tk := tkRule(t, "tk-first", 1000000)
	fixed := mkRule(t, &Rule{
		Name: "qps-after-token", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimBiz}, Window: "60s", Limit: 3,
		Algorithm: AlgFixedWindow,
	})
	// 顺序至关重要：token 在前，fixed 在后
	rules := []*Rule{tk, fixed}

	allowed := 0
	for i := 0; i < 10; i++ {
		d, _, err := l.Check(ctx, rules, meta(fmt.Sprintf("ord-%d", i)), 0)
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if d.Allowed {
			allowed++
		}
	}

	// fixed 规则 limit=3，必须恰好放行 3 个。
	// 若 plans 出现空洞导致 INCRBY 被跳过，这里会放行 10 个。
	if allowed != 3 {
		t.Fatalf("token 规则在前时后续规则的提交被跳过：放行 %d 个，期望恰好 3 个"+
			"（plans 空洞使 Phase 2 提前终止，等同 P0-1 配额放大）", allowed)
	}
}

// TestTkAdmitRejectDoesNotConsumeOtherQuota 预算耗尽拒绝时，
// 其他规则的计数器不得被污染 —— 两阶段不变量对 tkadmit 同样必须成立。
func TestTkAdmitRejectDoesNotConsumeOtherQuota(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	tk := tkRule(t, "tk-blocked", 10)
	fixed := mkRule(t, &Rule{
		Name: "qps-guard", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimBiz}, Window: "60s", Limit: 100,
		Algorithm: AlgFixedWindow,
	})
	m := meta("pollute-1")

	if err := l.TokenReserve(ctx, []*Rule{tk}, m, 10); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	// tkadmit 排在后面，确保 fixed 规则已在 Phase 1 通过试算
	d, _, err := l.Check(ctx, []*Rule{fixed, tk}, m, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if d.Allowed {
		t.Fatal("token 预算耗尽应拒绝")
	}

	b := WindowBoundary(time.Now(), fixed.WindowSeconds(), l.tzOffset, fixed.IsNaturalWindow())
	fkey := RateKey(fixed.Dimensions, m.dimValues(fixed.Dimensions), fixed.WindowSeconds(), b)
	if n, err := rdb.Exists(ctx, fkey).Result(); err == nil && n != 0 {
		v, _ := rdb.Get(ctx, fkey).Result()
		t.Fatalf("被拒请求污染了其他规则计数器：%s = %q（两阶段不变量被破坏）", fkey, v)
	}
}

// TestTkAdmitSemanticsMatchLedgerAdmit 合并实现与原 TokenAdmit 必须逐位等价。
//
// 两者都用 used >= limit 而非 used + cost > limit ——
// 准入期不知道本次会消耗多少 token，语义是「窗口预算已用完则不放新请求」。
// 边界值 used == limit-1 放行、used == limit 拒绝，两条路径必须一致。
func TestTkAdmitSemanticsMatchLedgerAdmit(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := tkRule(t, "tk-boundary", 100)
	rules := []*Rule{r}
	m := meta("bound-1")

	// used = 99（limit-1）：两条路径都应放行
	if err := l.TokenReserve(ctx, rules, m, 99); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	viaCheck, _, err := l.Check(ctx, rules, m, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	viaLedger, err := l.TokenAdmit(ctx, rules, m)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if !viaCheck.Allowed || !viaLedger.Allowed {
		t.Fatalf("used=99 limit=100 应放行：Check=%v TokenAdmit=%v",
			viaCheck.Allowed, viaLedger.Allowed)
	}

	// used = 100（== limit）：两条路径都应拒绝
	if err := l.TokenReserve(ctx, rules, m, 1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	viaCheck, _, err = l.Check(ctx, rules, m, 0)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	viaLedger, err = l.TokenAdmit(ctx, rules, m)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if viaCheck.Allowed || viaLedger.Allowed {
		t.Fatalf("used=100 limit=100 应拒绝：Check=%v TokenAdmit=%v"+
			"（两条路径语义必须逐位一致）", viaCheck.Allowed, viaLedger.Allowed)
	}
}
