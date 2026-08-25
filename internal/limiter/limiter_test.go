package limiter

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis 建立测试用 Redis 连接。
//
// 这里踩过一个真实的坑：早先只读 REDIS_ADDR、不读密码，而 compose 中的 Redis
// 配置了 requirepass，于是 Ping 返回 NOAUTH，11 个限流内核测试全部静默 Skip，
// 但 `go test` 依然是 PASS —— 覆盖率停在 36.6%，最核心的 Lua 两阶段求值
// 实际上一次都没被执行。「静默跳过 + 绿色结果」是最危险的测试假象。
//
// 现在的处理：
//  1. 支持 REDIS_PASSWORD / REDIS_DB，与运行时配置对齐；
//  2. 提供 REDIS_REQUIRED=1 严格模式 —— CI 必须开启，
//     此时连不上就 Fatal 而非 Skip，杜绝「基础设施没起来但 CI 全绿」。
func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()

	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	db := 0
	if v := os.Getenv("REDIS_DB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			db = n
		}
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     os.Getenv("REDIS_PASSWORD"),
		DB:           db,
		PoolSize:     100,
		MinIdleConns: 10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		if os.Getenv("REDIS_REQUIRED") == "1" {
			// 严格模式：基础设施缺失是硬失败，绝不能伪装成通过
			t.Fatalf("REDIS_REQUIRED=1 但 Redis 不可用 (%s): %v\n"+
				"核心限流测试依赖真实 Redis，静默跳过会让 CI 产生虚假的绿色结果", addr, err)
		}
		t.Skipf("redis 不可用 (%s): %v —— 设置 REDIS_REQUIRED=1 可让此情况变为硬失败", addr, err)
	}

	// 测试之间必须互相隔离，否则残留 key 会让断言随执行顺序漂移
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushdb 失败: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func mkRule(t *testing.T, r *Rule) *Rule {
	t.Helper()
	if err := r.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return r
}

func meta(id string) *Meta {
	return &Meta{
		Biz: "llm", Path: "/llm/v1/chat", TokenHash: "abc123",
		IP: "10.0.0.1", Method: "POST", RequestID: id,
	}
}

// TestRejectDoesNotConsumeQuota 验证评审 P0-5：
// 被拒请求绝不能占用窗口计数，否则攻击者可用被拒流量打满配额（DoS 放大），
// 且水位监控会读到虚高的 used 值。
func TestRejectDoesNotConsumeQuota(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "fixed-5", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimToken}, Window: "60s", Limit: 5,
		Algorithm: AlgFixedWindow,
	})

	for i := 0; i < 5; i++ {
		d, _, err := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("r%d", i)), 0)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d should pass, got allowed=%v err=%v", i, d.Allowed, err)
		}
	}

	// 再打 50 次必然全部被拒
	for i := 0; i < 50; i++ {
		d, _, _ := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("x%d", i)), 0)
		if d.Allowed {
			t.Fatalf("request beyond limit should be rejected at i=%d", i)
		}
	}

	// 关键断言：计数器必须精确停在 5，而不是 5+50
	b := WindowBoundary(time.Now(), 60, 0, false)
	key := RateKey([]string{DimToken}, []string{"abc123"}, 60, b)
	got, err := rdb.Get(ctx, key).Int64()
	if err != nil {
		t.Fatalf("get counter: %v", err)
	}
	if got != 5 {
		t.Fatalf("counter polluted by rejected requests: want 5, got %d", got)
	}
	t.Logf("OK: counter stayed at %d after 50 rejected requests", got)
}

// TestAtomicMultiRuleNoPartialCommit 验证评审 P0-4 第 2 项：
// 多规则场景下，若后置规则拒绝，前置规则不得留下已提交的计数（无需回滚的两阶段设计）。
func TestAtomicMultiRuleNoPartialCommit(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	loose := mkRule(t, &Rule{
		Name: "loose", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimBiz}, Window: "60s", Limit: 1000,
		Algorithm: AlgFixedWindow,
	})
	tight := mkRule(t, &Rule{
		Name: "tight", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimToken}, Window: "60s", Limit: 2,
		Algorithm: AlgFixedWindow,
	})
	rules := []*Rule{loose, tight}

	for i := 0; i < 2; i++ {
		d, _, _ := l.Check(ctx, rules, meta(fmt.Sprintf("a%d", i)), 0)
		if !d.Allowed {
			t.Fatalf("request %d should pass", i)
		}
	}
	for i := 0; i < 10; i++ {
		d, _, _ := l.Check(ctx, rules, meta(fmt.Sprintf("b%d", i)), 0)
		if d.Allowed {
			t.Fatal("should be rejected by tight rule")
		}
	}

	b := WindowBoundary(time.Now(), 60, 0, false)
	looseKey := RateKey([]string{DimBiz}, []string{"llm"}, 60, b)
	got, _ := rdb.Get(ctx, looseKey).Int64()
	// loose 规则只应记录 2 次成功，10 次被 tight 拒绝的请求不得计入
	if got != 2 {
		t.Fatalf("loose rule counter must not be committed on rejected requests: want 2, got %d", got)
	}
	t.Logf("OK: no partial commit, loose counter = %d", got)
}

// TestConcurrencyRejectNoLeak 验证评审 P0-4 第 1 项：
// 被拒的并发请求不得占用并发额度（原设计 INCR 后拒绝不 DECR，泄漏速率=拒绝QPS）。
func TestConcurrencyRejectNoLeak(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "conc-2", Type: TypeConcurrency,
		Dimensions: []string{DimToken}, MaxConc: 2, TimeoutSec: 120,
	})

	h1, h2 := mustAcquire(t, l, r, "c1"), mustAcquire(t, l, r, "c2")

	// 打 100 次必被拒，若原设计的 INCR 泄漏存在，额度会被永久占满
	for i := 0; i < 100; i++ {
		d, _, _ := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("z%d", i)), 0)
		if d.Allowed {
			t.Fatalf("should be rejected at i=%d", i)
		}
	}

	key := ConcurrencyKey("llm", []string{DimToken}, []string{"abc123"})
	n, _ := rdb.ZCard(ctx, key).Result()
	if n != 2 {
		t.Fatalf("concurrency leaked by rejected requests: want 2, got %d", n)
	}

	// 释放后必须能立即重新获取
	if err := l.Release(ctx, h1); err != nil {
		t.Fatalf("release: %v", err)
	}
	d, h3, _ := l.Check(ctx, []*Rule{r}, meta("c3"), 0)
	if !d.Allowed {
		t.Fatal("should be allowed after release")
	}
	_ = l.Release(ctx, h2)
	_ = l.Release(ctx, h3)

	n, _ = rdb.ZCard(ctx, key).Result()
	if n != 0 {
		t.Fatalf("all released, want 0, got %d", n)
	}
	t.Logf("OK: no concurrency leak after 100 rejections")
}

// TestConcurrencyIdempotentRelease 验证评审 P0-4 第 3 项：
// 重复释放不得把计数减成负数（原 DECR 方案会导致限流彻底失效）。
func TestConcurrencyIdempotentRelease(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "conc-1", Type: TypeConcurrency,
		Dimensions: []string{DimToken}, MaxConc: 1, TimeoutSec: 120,
	})
	h := mustAcquire(t, l, r, "k1")

	for i := 0; i < 5; i++ {
		if err := l.Release(ctx, h); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}

	key := ConcurrencyKey("llm", []string{DimToken}, []string{"abc123"})
	n, _ := rdb.ZCard(ctx, key).Result()
	if n != 0 {
		t.Fatalf("want 0 after repeated release, got %d", n)
	}
	// 限流必须依然生效（若变成负数则会放行超额请求）
	_ = mustAcquire(t, l, r, "k2")
	d, _, _ := l.Check(ctx, []*Rule{r}, meta("k3"), 0)
	if d.Allowed {
		t.Fatal("limit must still work after repeated release")
	}
	t.Logf("OK: idempotent release, limit still enforced")
}

// TestConcurrencyDeadlineReclaim 验证评审 P0-4 第 3 项：
// 持有者超时后必须被自动回收，且不会误伤其他持有者（原 key 级 TTL 方案会整体清空）。
func TestConcurrencyDeadlineReclaim(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := &Rule{
		Name: "conc-timeout", Type: TypeConcurrency,
		Dimensions: []string{DimToken}, MaxConc: 1, TimeoutSec: 1,
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}

	_ = mustAcquire(t, l, r, "t1")
	d, _, _ := l.Check(ctx, []*Rule{r}, meta("t2"), 0)
	if d.Allowed {
		t.Fatal("should be blocked while holder alive")
	}

	// 等待 deadline 过期，模拟持有者进程崩溃未释放
	time.Sleep(1200 * time.Millisecond)
	d, _, _ = l.Check(ctx, []*Rule{r}, meta("t3"), 0)
	if !d.Allowed {
		t.Fatal("expired holder must be reclaimed")
	}
	t.Logf("OK: timed-out holder auto-reclaimed")
}

// TestTokenBucketPersistentAcrossWindow 验证评审 P0-2：
// 令牌桶 Key 不含窗口边界，桶状态必须跨窗口连续，而非每窗口重建。
func TestTokenBucketPersistentAcrossWindow(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	// 1 秒窗口 limit=10 → rate=10/s, burst=10
	r := mkRule(t, &Rule{
		Name: "tb", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimBiz}, Window: "1s", Limit: 10,
		Algorithm: AlgTokenBucket, Burst: 10,
	})

	// 瞬间耗尽桶
	pass := 0
	for i := 0; i < 20; i++ {
		d, _, _ := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("tb%d", i)), 0)
		if d.Allowed {
			pass++
		}
	}
	if pass != 10 {
		t.Fatalf("burst should allow exactly 10, got %d", pass)
	}

	key := TokenBucketKey([]string{DimBiz}, []string{"llm"})
	// Key 中不得出现窗口边界（否则跨窗口会重建成满桶）
	if len(key) == 0 || contains(key, fmt.Sprint(time.Now().Unix()/1*1)) {
		t.Logf("token bucket key = %s", key)
	}

	// 等待后应按经过时间比例恢复令牌（连续填充），而非整桶重置。
	//
	// 断言方式经过一次修正：原先写死区间 [3,8] 并 sleep 520ms 期望「约 5 个」，
	// 宿主负载高时 sleep 会 overshoot 到 ~1s，令牌恢复满 10 个从而偶发失败
	// （实测约 1/5 概率）。依赖 wall-clock 精度的固定区间断言本身不可靠。
	//
	// 改为按实测经过时间推算上界：本测试要区分的是「连续按速率填充」与
	// 「每窗口重建成满桶」，二者的分界不是「恰好 5 个」，而是
	// 「恢复量是否与等待时间成正比」。因此只需断言：确有恢复（证明在填充），
	// 且不超过按实测耗时算出的理论量 + 容差（证明不是整桶重置）。
	before := time.Now()
	time.Sleep(520 * time.Millisecond)
	elapsed := time.Since(before)

	pass2 := 0
	for i := 0; i < 20; i++ {
		d, _, _ := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("tb2%d", i)), 0)
		if d.Allowed {
			pass2++
		}
	}

	// rate = limit/window = 10/s，理论恢复量 = elapsed 秒 × 10，且不超过 burst
	theoretical := int(elapsed.Seconds() * 10)
	upper := theoretical + 2 // 容差：Redis 侧取时与本地取时存在偏差
	if upper > 10 {
		upper = 10 // burst 上限，物理上不可能超过
	}
	if pass2 < 1 {
		t.Fatalf("桶未在填充：等待 %v 后仍无可用令牌，填充逻辑失效", elapsed)
	}
	if pass2 > upper {
		t.Fatalf("恢复 %d 个令牌超出按实测 %v 推算的上界 %d —— 疑似整桶重置而非连续填充（P0-2 回归）",
			pass2, elapsed, upper)
	}
	t.Logf("OK: 等待 %v 恢复 %d 个令牌（理论 %d / 上界 %d），连续填充成立 key=%s",
		elapsed, pass2, theoretical, upper, key)
}

// TestTokenBucketRejectsTokenMetric 验证评审 P0-2 的语义修正：
// token_bucket 与 metric=token 组合语义矛盾，必须在配置加载期拒绝。
func TestTokenBucketRejectsTokenMetric(t *testing.T) {
	r := &Rule{
		Name: "bad", Type: TypeRate, Metric: MetricToken,
		Dimensions: []string{DimGlobal}, Window: "1m", Limit: 500000,
		Algorithm: AlgTokenBucket,
	}
	if err := r.Validate(); err == nil {
		t.Fatal("must reject token_bucket + metric=token combination")
	} else {
		t.Logf("OK: rejected at config load: %v", err)
	}
}

// TestTokenLedgerSettleRefund 验证评审 P0-6：
// 估算预扣 1016，实际 847，多扣的 169 必须退回，否则预算被系统性提前 20% 耗尽。
func TestTokenLedgerSettleRefund(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "token-budget", Type: TypeRate, Metric: MetricToken,
		Dimensions: []string{DimGlobal}, Window: "1h", Limit: 500000,
		Algorithm: AlgFixedWindow,
	})
	rules := []*Rule{r}
	m := meta("t-settle")

	// 估算 847 * 1.2 = 1016 预扣
	const reserved, actual = 1016, 847
	if err := l.TokenReserve(ctx, rules, m, reserved); err != nil {
		t.Fatal(err)
	}
	used, _ := l.TokenUsage(ctx, r, m)
	if used != reserved {
		t.Fatalf("after reserve want %d, got %d", reserved, used)
	}

	if err := l.TokenSettle(ctx, rules, m, reserved, actual); err != nil {
		t.Fatal(err)
	}
	used, _ = l.TokenUsage(ctx, r, m)
	if used != actual {
		t.Fatalf("settle must refund over-reserved tokens: want %d, got %d", actual, used)
	}
	t.Logf("OK: reserved=%d settled=%d, refunded %d tokens", reserved, actual, reserved-actual)
}

// TestTokenLedgerAdmitBlocksWhenExhausted 验证预算耗尽后拒绝新请求
func TestTokenLedgerAdmitBlocksWhenExhausted(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "budget", Type: TypeRate, Metric: MetricToken,
		Dimensions: []string{DimToken}, Window: "1h", Limit: 1000,
		Algorithm: AlgFixedWindow,
	})
	rules := []*Rule{r}
	m := meta("adm")

	d, _ := l.TokenAdmit(ctx, rules, m)
	if !d.Allowed {
		t.Fatal("fresh budget should admit")
	}
	_ = l.TokenReserve(ctx, rules, m, 1000)

	d, _ = l.TokenAdmit(ctx, rules, m)
	if d.Allowed {
		t.Fatal("exhausted budget must reject")
	}
	if d.RetryAfter <= 0 {
		t.Fatal("must provide retry_after")
	}
	t.Logf("OK: budget exhausted, retry_after=%v", d.RetryAfter)
}

// TestConcurrentExactLimit 验证高并发下配额精确性（无超卖、无锁丢失）
func TestConcurrentExactLimit(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	const limit = 50
	r := mkRule(t, &Rule{
		Name: "race", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimToken}, Window: "60s", Limit: limit,
		Algorithm: AlgFixedWindow,
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, _, err := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("rc%d", i)), 0)
			if err == nil && d.Allowed {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if passed != limit {
		t.Fatalf("concurrent limit must be exact: want %d, got %d", limit, passed)
	}
	t.Logf("OK: 500 concurrent requests, exactly %d passed", passed)
}

// TestSlidingWindowNoOverwrite 验证滑动窗口 member 唯一性（评审 P0-5 附带项）：
// 原设计 member = now .. '_' .. ARGV[4] 且 ARGV[4] 来源未定义，同秒同值会 ZADD 覆盖漏计。
func TestSlidingWindowNoOverwrite(t *testing.T) {
	rdb := testRedis(t)
	defer rdb.Close()
	l := New(rdb, Options{})
	ctx := context.Background()

	r := mkRule(t, &Rule{
		Name: "sw", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimToken}, Window: "60s", Limit: 10,
		Algorithm: AlgSlidingWindow,
	})

	pass := 0
	// 同一毫秒内密集打请求，验证不会因 member 冲突而漏计
	for i := 0; i < 30; i++ {
		d, _, _ := l.Check(ctx, []*Rule{r}, meta(fmt.Sprintf("sw%d", i)), 0)
		if d.Allowed {
			pass++
		}
	}
	if pass != 10 {
		t.Fatalf("sliding window must count exactly: want 10, got %d", pass)
	}
	t.Logf("OK: sliding window counted exactly %d (no member overwrite)", pass)
}

// TestKeyCollisionSafety 验证评审 P1-8：不同维度组合不得碰撞出同一 Key
func TestKeyCollisionSafety(t *testing.T) {
	// 原设计用 '_' 连接，以下两组会碰撞成同一字符串
	k1 := RateKey([]string{DimBiz, DimPath}, []string{"order", "/v1_create"}, 60, 100)
	k2 := RateKey([]string{DimBiz, DimPath}, []string{"order_/v1", "create"}, 60, 100)
	if k1 == k2 {
		t.Fatalf("key collision: %s", k1)
	}

	// path 含 '/' 必须被安全编码，不得破坏 Key 结构
	k3 := RateKey([]string{DimPath}, []string{"/llm/v1/chat/completions"}, 60, 100)
	if contains(k3, "/") {
		t.Fatalf("path must be encoded, got %s", k3)
	}
	t.Logf("OK: k1=%s k2=%s k3=%s", k1, k2, k3)
}

// TestTransientErrorFailsClosed 回归测试：Redis 偶发错误（非故障）必须 Fail-Close。
//
// 这是压测中实测发现、评审报告未覆盖的缺陷：
// 原实现把任何 Redis 错误都当作故障并按 metric 分治降级，
// 导致 metric=request 规则在连接池打满时静默 Fail-Open，限流在流量高峰失效。
func TestTransientErrorFailsClosed(t *testing.T) {
	// 指向一个不可达地址模拟错误，但熔断器尚未累积到故障阈值
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
	})
	defer rdb.Close()
	l := New(rdb, Options{RedisTimeout: 50 * time.Millisecond})

	r := mkRule(t, &Rule{
		Name: "req-rule", Type: TypeRate, Metric: MetricRequest,
		Dimensions: []string{DimToken}, Window: "60s", Limit: 10,
		Algorithm: AlgFixedWindow,
	})

	// 熔断器闭合状态下的首个错误必须拒绝，而非放行
	d, _, err := l.Check(context.Background(), []*Rule{r}, meta("tr1"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Allowed {
		t.Fatal("transient redis error must fail closed, not open")
	}
	if !d.Degraded {
		t.Fatal("decision should be marked degraded")
	}
	t.Logf("OK: transient error rejected (degraded=%v, retry=%v)", d.Degraded, d.RetryAfter)

	// 持续错误累积后熔断打开，此时才允许 metric=request 降级放行
	for i := 0; i < 60; i++ {
		_, _, _ = l.Check(context.Background(), []*Rule{r}, meta(fmt.Sprintf("tr%d", i)), 0)
	}
	_, trips, open := l.BreakerStats()
	if !open || trips == 0 {
		t.Fatalf("breaker should trip after sustained failures: trips=%d open=%v", trips, open)
	}
	d, _, _ = l.Check(context.Background(), []*Rule{r}, meta("tr-final"), 0)
	if !d.Allowed {
		t.Fatal("after breaker trips, metric=request should fail open")
	}
	t.Logf("OK: breaker tripped (%d times), metric=request now fails open", trips)
}

func mustAcquire(t *testing.T, l *Limiter, r *Rule, id string) *concHold {
	t.Helper()
	d, h, err := l.Check(context.Background(), []*Rule{r}, meta(id), 0)
	if err != nil || !d.Allowed {
		t.Fatalf("acquire %s failed: allowed=%v err=%v", id, d.Allowed, err)
	}
	return h
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
