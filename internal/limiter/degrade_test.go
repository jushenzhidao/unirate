package limiter

import (
	"testing"
	"time"
)

// TestWindowBoundaryZeroWinSecDoesNotPanic 锁定一个**实测触发过的崩溃**。
//
// 现场：Redis 故障期间，网关对每个请求 panic
//
//	http: panic serving: runtime error: integer divide by zero
//	limiter.WindowBoundary(...) key.go:67
//	limiter.(*Limiter).degrade(...) limiter.go
//
// 根因：并发规则（type=concurrency）不解析 Window，其 winSec 恒为 0，
// 而 failModeOf 把它归入 FailLocalQuota；降级路径无条件调用
// WindowBoundary(now, r.winSec, ...)，函数内部 ts / winSec 直接除零。
//
// 为什么一直没被发现：这条路径只在**熔断器真正打开后**才可达。
// 熔断未打开时走 Fail-Close 分支，根本不进 degrade()。
// 演示、单测、乃至 Redis 短暂抖动都碰不到它 ——
// 只在真实故障持续到熔断打开时炸，而那正是最不能再出问题的时刻。
func TestWindowBoundaryZeroWinSecDoesNotPanic(t *testing.T) {
	now := time.Unix(1755900000, 0)
	for _, winSec := range []int64{0, -1, -3600} {
		for _, natural := range []bool{false, true} {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Errorf("winSec=%d natural=%v 发生 panic: %v", winSec, natural, rec)
					}
				}()
				if got := WindowBoundary(now, winSec, 8*3600, natural); got != 0 {
					t.Errorf("winSec=%d 应返回 0，实际 %d", winSec, got)
				}
			}()
		}
	}
}

// TestWindowBoundaryStillCorrectForValidWindows 上面的防御不得改变正常语义。
// 这是防「修 crash 时顺手把正确逻辑改坏」——
// 窗口边界算错不会报错，只会让限流窗口对不齐，属沉默逻辑错误。
func TestWindowBoundaryStillCorrectForValidWindows(t *testing.T) {
	now := time.Unix(1755900123, 0)
	cases := []struct {
		winSec  int64
		tz      int64
		natural bool
		want    int64
	}{
		{1, 0, false, 1755900123},
		{60, 0, false, 1755900120},
		{3600, 0, false, 1755900000},
		// 自然日按东八区零点对齐
		{86400, 8 * 3600, true, (1755900123+8*3600)/86400*86400 - 8*3600},
		// natural=false 时忽略时区
		{86400, 8 * 3600, false, 1755900123 / 86400 * 86400},
	}
	for _, tc := range cases {
		if got := WindowBoundary(now, tc.winSec, tc.tz, tc.natural); got != tc.want {
			t.Errorf("winSec=%d tz=%d natural=%v: 期望 %d，实际 %d",
				tc.winSec, tc.tz, tc.natural, tc.want, got)
		}
	}
}

// TestDegradeWithConcurrencyRuleDoesNotPanic 直接复现崩溃场景：
// Redis 不可用（rdb=nil）+ 规则集含并发规则 → 走 degrade 的 FailLocalQuota 分支。
//
// 这是端到端层面的守护：即使 WindowBoundary 的防御被移除，
// 只要 degrade 路径仍无条件除以 winSec，此测试就会变红。
func TestDegradeWithConcurrencyRuleDoesNotPanic(t *testing.T) {
	l := New(nil, Options{Instances: 4, TZOffsetSeconds: 8 * 3600})

	concRule := &Rule{
		ID: 1, Name: "conc-guard", Type: TypeConcurrency,
		Dimensions: []string{DimBiz}, MaxConc: 50, TimeoutSec: 120,
	}
	if err := concRule.Validate(); err != nil {
		t.Fatalf("并发规则应合法: %v", err)
	}
	// Validate 后 winSec 仍为 0 —— 这正是崩溃的前提条件
	if concRule.WindowSeconds() != 0 {
		t.Fatalf("并发规则的 winSec 应为 0（本测试的前提），实际 %d", concRule.WindowSeconds())
	}

	meta := &Meta{Biz: "demo", Path: "/v1/x", IP: "10.0.0.1", RequestID: "req-1"}

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("降级路径遇到并发规则时 panic: %v", rec)
		}
	}()

	// 反复调用：本地配额会逐步耗尽，覆盖 allow 与 reject 两条分支
	rejected := false
	for i := 0; i < 30; i++ {
		d, _, _ := l.degrade([]*Rule{concRule}, meta, nil)
		if !d.Degraded {
			t.Fatal("降级决策必须标记 Degraded=true，否则可观测性上无法区分正常与降级")
		}
		if !d.Allowed {
			rejected = true
			if d.RetryAfter <= 0 {
				t.Error("拒绝时必须给出正数 RetryAfter，客户端才知道何时重试")
			}
		}
	}
	// instances=4、MaxConc=50 → 单实例配额 12，30 次调用必然触发拒绝
	if !rejected {
		t.Error("单实例配额 50/4=12，30 次调用应至少拒绝一次；未拒绝说明配额未生效")
	}
}

// TestDegradeTokenRuleUsesWindowQuota token 预算规则的降级仍应按窗口配额判定，
// 不能被并发规则的特殊处理带偏。
func TestDegradeTokenRuleUsesWindowQuota(t *testing.T) {
	l := New(nil, Options{Instances: 2, TZOffsetSeconds: 8 * 3600})

	tokRule := &Rule{
		ID: 2, Name: "token-budget", Type: TypeRate, Metric: MetricToken,
		Dimensions: []string{DimBiz, DimToken}, Window: "1h", Limit: 100,
		Algorithm: AlgFixedWindow,
	}
	if err := tokRule.Validate(); err != nil {
		t.Fatalf("token 规则应合法: %v", err)
	}

	meta := &Meta{Biz: "demo", TokenHash: "abc", RequestID: "req-2"}

	key, limit, window := l.degradeQuotaParams(tokRule, meta)
	if key == "" {
		t.Error("速率规则应产出有效的配额 Key")
	}
	if limit != 100 {
		t.Errorf("配额上限应取规则 Limit=100，实际 %d", limit)
	}
	if window != time.Hour {
		t.Errorf("窗口应为 1h，实际 %s", window)
	}
}

// TestDegradeQuotaParamsRejectsBrokenRateRule winSec 为 0 的速率规则
// （规则损坏或未经 Validate）必须返回 window=0，让调用方保守拒绝，
// 而不是硬算出一个错误的窗口继续放行。
func TestDegradeQuotaParamsRejectsBrokenRateRule(t *testing.T) {
	l := New(nil, Options{Instances: 1})
	broken := &Rule{
		ID: 3, Name: "broken", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimBiz}, Limit: 10,
		// 故意不调 Validate，winSec 保持 0
	}
	meta := &Meta{Biz: "demo", RequestID: "req-3"}

	_, _, window := l.degradeQuotaParams(broken, meta)
	if window != 0 {
		t.Errorf("损坏的速率规则应返回 window=0 以触发保守拒绝，实际 %s", window)
	}

	// degrade 必须因此拒绝，且不 panic
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("损坏规则导致 panic: %v", rec)
		}
	}()
	// 该规则的 failMode 是 FailOpen（request 类），改成 token 类才走 LocalQuota
	broken.Metric = MetricToken
	d, _, _ := l.degrade([]*Rule{broken}, meta, nil)
	if d.Allowed {
		t.Error("无法构造有效本地窗口时，token 预算规则必须保守拒绝而非放行")
	}
}

// TestDegradeConcurrencyKeyIsolatedFromNormalPath 降级用的并发 Key
// 必须与正常路径的 Key 区分开。共用会让降级期间的本地计数
// 与 Redis 恢复后的集中计数相互干扰。
func TestDegradeConcurrencyKeyIsolatedFromNormalPath(t *testing.T) {
	l := New(nil, Options{Instances: 1})
	r := &Rule{
		ID: 4, Name: "c", Type: TypeConcurrency,
		Dimensions: []string{DimBiz}, MaxConc: 10, TimeoutSec: 60,
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	meta := &Meta{Biz: "demo", RequestID: "r"}

	degKey, _, _ := l.degradeQuotaParams(r, meta)
	normalKey := ConcurrencyKey(meta.Biz, r.Dimensions, meta.dimValues(r.Dimensions))

	if degKey == normalKey {
		t.Error("降级 Key 与正常路径 Key 相同，两套计数会相互干扰")
	}
}

// TestDegradeConcurrencyDefaultsTimeout TimeoutSec 缺失时应回落到 120s，
// 而不是产生 0 窗口（0 窗口会让配额条目立即过期，等于完全不限流）。
func TestDegradeConcurrencyDefaultsTimeout(t *testing.T) {
	l := New(nil, Options{Instances: 1})
	r := &Rule{
		ID: 5, Name: "c2", Type: TypeConcurrency,
		Dimensions: []string{DimBiz}, MaxConc: 10, TimeoutSec: 0,
	}
	meta := &Meta{Biz: "demo", RequestID: "r"}

	_, _, window := l.degradeQuotaParams(r, meta)
	if window != 120*time.Second {
		t.Errorf("TimeoutSec=0 应回落到 120s，实际 %s（0 窗口等于不限流）", window)
	}
}
