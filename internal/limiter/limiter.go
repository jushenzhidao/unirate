package limiter

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/batch_check.lua
var batchCheckSrc string

//go:embed scripts/release_conc.lua
var releaseConcSrc string

//go:embed scripts/token_ledger.lua
var tokenLedgerSrc string

var (
	batchCheckScript  = redis.NewScript(batchCheckSrc)
	releaseConcScript = redis.NewScript(releaseConcSrc)
	tokenLedgerScript = redis.NewScript(tokenLedgerSrc)
)

// ErrRedisDown Redis 不可用
var ErrRedisDown = errors.New("redis unavailable")

// FailMode Redis 故障时的降级策略（对应评审 P1-10）
//
// 原设计「Redis 故障 → L3/L4 一律 Fail-Open」的问题：
// L4 承载 metric=token 的预算控制，Redis 挂掉期间所有 Token 预算失效，
// 对接大模型上游时这等同于「烧钱开关」，且事后无法补账。
// 因此按 metric 分治：request 可 Fail-Open，token 必须 Fail-Close 或本地保守配额。
type FailMode string

const (
	FailOpen       FailMode = "open"        // 放行（适用于 metric=request）
	FailClose      FailMode = "close"       // 拒绝（适用于 metric=token 的严格预算）
	FailLocalQuota FailMode = "local_quota" // 降级为本地保守配额 = 总量/实例数
)

// Decision 限流决策结果
type Decision struct {
	Allowed    bool
	RuleName   string
	RuleID     int64
	Dimension  string
	RetryAfter time.Duration
	Degraded   bool // 是否在降级模式下作出的决策
}

// concHold 记录本请求持有的并发 Key，用于响应结束时精确释放
type concHold struct {
	keys  []string
	reqID string
}

// Limiter 限流器
type Limiter struct {
	rdb      redis.UniversalClient
	tzOffset int64
	// instances 集群实例数，用于 local_quota 降级估算。
	// 原子读写：它是 Tier 1 可热更新项（扩缩容后需同步），
	// 而降级路径可能与配置更新并发执行。
	instances atomic.Int64
	local     *localFallback
	timeout   time.Duration
	breaker   *breaker
}

// SetInstances 热更新集群实例数。
//
// 这项直接决定降级期间的本地保守配额（总量 ÷ 实例数）：
// 设得比真实副本数小 → 降级时各实例配额之和超过总配额，出现超卖；
// 设得过大 → 降级时过度拒绝。所以它必须能随扩缩容同步，不能只在启动时读一次。
// 非正值忽略，避免除零。
func (l *Limiter) SetInstances(n int) {
	if n <= 0 {
		return
	}
	l.instances.Store(int64(n))
}

// Instances 当前实例数
func (l *Limiter) Instances() int { return int(l.instances.Load()) }

// localQuota 计算降级期间的单实例保守配额 = 总配额 ÷ 实例数。
//
// 抽成单一函数是因为这段逻辑在 Check 降级与 TokenAdmit 降级两处都要用，
// 两份实现一旦漂移就会出现「请求维度和 token 维度用了不同实例数」的
// 沉默不一致 —— 这类错误不报警、只是配额算错。
// 下界钳到 1：配额为 0 会让降级期间全量拒绝，比超卖更糟（服务直接不可用）。
func (l *Limiter) localQuota(limit int64) int64 {
	inst := l.instances.Load()
	if inst < 1 {
		inst = 1
	}
	q := limit / inst
	if q < 1 {
		q = 1
	}
	return q
}

// BreakerStats 暴露 Redis 熔断器统计
func (l *Limiter) BreakerStats() (errors, trips int64, open bool) {
	return l.breaker.Stats()
}

// Options 限流器配置
type Options struct {
	TZOffsetSeconds int64
	Instances       int
	RedisTimeout    time.Duration
}

// New 创建限流器
func New(rdb redis.UniversalClient, opt Options) *Limiter {
	if opt.Instances <= 0 {
		opt.Instances = 1
	}
	if opt.RedisTimeout <= 0 {
		// 200ms：需覆盖 Lua 脚本执行 + 高并发下的连接池排队。
		// 50ms 在压测中被证明过于激进，会把正常抖动误判为故障。
		opt.RedisTimeout = 200 * time.Millisecond
	}
	l := &Limiter{
		rdb:      rdb,
		tzOffset: opt.TZOffsetSeconds,
		local:    newLocalFallback(),
		timeout:  opt.RedisTimeout,
		breaker:  newBreaker(),
	}
	l.instances.Store(int64(opt.Instances))
	return l
}

// luaRule 传给 Lua 脚本的规则描述
type luaRule struct {
	K      int     `json:"k"`
	T      string  `json:"t"`
	Limit  int64   `json:"limit"`
	Cost   int64   `json:"cost"`
	Expire int64   `json:"expire,omitempty"`
	Window int64   `json:"window,omitempty"`
	Rate   float64 `json:"rate,omitempty"`
	Burst  int64   `json:"burst,omitempty"`
	TTL    int64   `json:"ttl,omitempty"`
}

// Meta 请求元数据，用于维度取值
type Meta struct {
	Biz       string
	Path      string
	TokenHash string
	IP        string
	Method    string
	RequestID string
}

// dimValues 按规则维度提取取值
func (m *Meta) dimValues(dims []string) []string {
	vals := make([]string, 0, len(dims))
	for _, d := range dims {
		switch d {
		case DimGlobal:
			vals = append(vals, "_")
		case DimBiz:
			vals = append(vals, m.Biz)
		case DimPath:
			vals = append(vals, m.Path)
		case DimToken:
			vals = append(vals, m.TokenHash)
		case DimIP:
			vals = append(vals, m.IP)
		case DimMethod:
			vals = append(vals, m.Method)
		}
	}
	return vals
}

// Check 对一组规则做原子批量限流判定。
//
// 对应评审 P0-1 修正：
//
//	原设计 L1/L2 用「本地内存令牌桶」，而架构又声明无状态多实例部署 ——
//	N 个实例各持独立桶，实际配额 = limit × N，「保护下游总量」完全落空。
//	本实现统一走 Redis 集中计数（单脚本原子求值），本地只保留「已知超限」的负缓存，
//	负缓存只会让判定更严格、绝不会放大配额，因此集群语义始终成立。
//
// 对应评审 Advisory-4：所有规则合并为一次 Redis 往返，单请求 RTT 与规则数解耦。
func (l *Limiter) Check(ctx context.Context, rules []*Rule, meta *Meta, tokenCost int64) (Decision, *concHold, error) {
	if len(rules) == 0 {
		return Decision{Allowed: true}, nil, nil
	}

	now := time.Now()
	keys := make([]string, 0, len(rules))
	lrules := make([]luaRule, 0, len(rules))
	idx := make([]*Rule, 0, len(rules))
	concKeys := make([]string, 0, 2)

	for _, r := range rules {
		if !r.IsEnabled() {
			continue
		}
		vals := meta.dimValues(r.Dimensions)

		// 本地负缓存快速拒绝：已知该 Key 在窗口内超限时直接短路，省一次 Redis 往返。
		// 注意这只会更严格，不会放宽，故不破坏集群语义。
		var lr luaRule
		var key string

		switch {
		// Token 预算准入并入本次往返（ADR-003）。
		//
		// 语义不变：仍只读检查「窗口预算是否已耗尽」，不做任何扣减 ——
		// 真实消耗在响应期由 TokenReserve/TokenSettle 记账。
		// 合并的收益是消除一次独立 Redis 往返（原 TokenAdmit 对每条 token 规则各发一次）。
		case r.Type == TypeRate && r.Metric == MetricToken:
			b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
			key = TokenLedgerKey(r.Dimensions, vals, r.winSec, b)
			lr = luaRule{T: "tkadmit", Limit: r.Limit}

		case r.Type == TypeConcurrency:
			key = ConcurrencyKey(meta.Biz, r.Dimensions, vals)
			lr = luaRule{T: "conc", Limit: r.MaxConc, TTL: r.TimeoutSec * 1000}
			concKeys = append(concKeys, key)

		case r.Algorithm == AlgTokenBucket:
			key = TokenBucketKey(r.Dimensions, vals)
			lr = luaRule{
				T:     "tb",
				Limit: r.Limit,
				Cost:  1,
				Rate:  r.TokenBucketRate(),
				Burst: r.Burst,
			}

		case r.Algorithm == AlgSlidingWindow:
			b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
			key = RateKey(r.Dimensions, vals, r.winSec, b)
			lr = luaRule{T: "sliding", Limit: r.Limit, Cost: 1, Window: r.winSec * 1000}

		default: // fixed_window
			b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
			key = RateKey(r.Dimensions, vals, r.winSec, b)
			lr = luaRule{T: "fixed", Limit: r.Limit, Cost: 1, Expire: r.winSec + 60}
		}

		if until, hit := l.local.blocked(key, now); hit {
			return Decision{
				Allowed:    false,
				RuleName:   r.Name,
				RuleID:     r.ID,
				Dimension:  r.DimKey(),
				RetryAfter: until.Sub(now),
			}, nil, nil
		}

		keys = append(keys, key)
		lr.K = len(keys)
		lrules = append(lrules, lr)
		idx = append(idx, r)
	}

	if len(keys) == 0 {
		return Decision{Allowed: true}, nil, nil
	}

	payload, err := json.Marshal(lrules)
	if err != nil {
		return Decision{}, nil, fmt.Errorf("marshal rules: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	res, err := batchCheckScript.Run(rctx, l.rdb, keys, string(payload), meta.RequestID).Slice()
	if err != nil {
		l.breaker.onFailure()
		// 关键：只有熔断器确认 Redis 真正故障时才降级放行。
		// 健康状态下的偶发超时一律 Fail-Close —— 否则高并发抖动会让限流静默失效
		// （实测 500 并发打 limit=50 因超时放行导致通过 134 个）。
		if !l.breaker.degraded() {
			r := idx[0]
			return Decision{
				Allowed:    false,
				RuleName:   r.Name,
				RuleID:     r.ID,
				Dimension:  r.DimKey(),
				RetryAfter: 100 * time.Millisecond,
				Degraded:   true,
			}, nil, nil
		}
		return l.degrade(idx, meta, err)
	}
	l.breaker.onSuccess()

	ok, failIdx, retryMs := parseResult(res)
	if ok {
		hold := &concHold{keys: concKeys, reqID: meta.RequestID}
		if len(concKeys) == 0 {
			hold = nil
		}
		return Decision{Allowed: true}, hold, nil
	}

	if failIdx < 1 || failIdx > len(idx) {
		return Decision{Allowed: false, RuleName: "unknown"}, nil, nil
	}
	r := idx[failIdx-1]
	retry := time.Duration(retryMs) * time.Millisecond

	// 短窗口超限写入本地负缓存，抵挡同 Key 的后续洪峰
	if retry > 0 && retry <= 10*time.Second {
		l.local.block(keys[failIdx-1], now.Add(retry))
	}

	return Decision{
		Allowed:    false,
		RuleName:   r.Name,
		RuleID:     r.ID,
		Dimension:  r.DimKey(),
		RetryAfter: retry,
	}, nil, nil
}

// degrade Redis 不可用时的分治降级（评审 P1-10）
func (l *Limiter) degrade(rules []*Rule, meta *Meta, cause error) (Decision, *concHold, error) {
	for _, r := range rules {
		mode := l.failModeOf(r)
		switch mode {
		case FailClose:
			return Decision{
				Allowed:    false,
				RuleName:   r.Name,
				RuleID:     r.ID,
				Dimension:  r.DimKey(),
				RetryAfter: time.Second,
				Degraded:   true,
			}, nil, nil
		case FailLocalQuota:
			key, limit, window := l.degradeQuotaParams(r, meta)
			if window <= 0 {
				// 无法构造有意义的本地窗口 → 保守拒绝。
				// 这类规则（token 预算 / 并发）走到 FailLocalQuota 就说明
				// 它关系到真金白银或下游保护，宁可拒绝也不放行。
				return Decision{
					Allowed:    false,
					RuleName:   r.Name,
					RuleID:     r.ID,
					Dimension:  r.DimKey(),
					RetryAfter: time.Second,
					Degraded:   true,
				}, nil, nil
			}
			if !l.local.allowQuota(key, l.localQuota(limit), window) {
				return Decision{
					Allowed:    false,
					RuleName:   r.Name,
					RuleID:     r.ID,
					Dimension:  r.DimKey(),
					RetryAfter: window,
					Degraded:   true,
				}, nil, nil
			}
		}
	}
	return Decision{Allowed: true, Degraded: true}, nil, nil
}

// degradeQuotaParams 为降级路径构造本地配额的 Key、上限与窗口。
//
// 修复一个会 panic 的真实缺陷：
//
//	并发规则（type=concurrency）不解析 Window，其 winSec 恒为 0，
//	而 failModeOf 把它归入 FailLocalQuota。原实现无条件调用
//	WindowBoundary(now, r.winSec, ...)，该函数内部做 ts / winSec ——
//	于是 Redis 故障 + 存在并发规则时，每个请求都 integer divide by zero，
//	panic 掉整个 HTTP 连接（客户端看到的是连接被重置，不是 5xx）。
//
// 为什么此前没被发现：panic 只在**熔断器真正打开后**才可达
//   - 熔断未开时走的是 Fail-Close 分支，根本不进 degrade()；
//   - 单测用的规则集不含并发规则，或没打到熔断阈值。
//
// 这正是「沉默逻辑错误」的典型形态：代码能编译、演示能跑、
// 只在特定故障组合下炸，而那恰恰是最不能再出问题的时刻。
//
// 并发规则用 TimeoutSec 作为本地窗口：它表达的是「一个请求最长持有多久额度」，
// 是该规则唯一具备时间语义的字段，用它做本地配额的重置周期最贴近原义。
func (l *Limiter) degradeQuotaParams(r *Rule, meta *Meta) (key string, limit int64, window time.Duration) {
	vals := meta.dimValues(r.Dimensions)

	if r.Type == TypeConcurrency {
		w := r.TimeoutSec
		if w <= 0 {
			w = 120 // 与 Rule.Validate() 的默认值一致
		}
		// 并发键不含窗口边界（并发本身无窗口语义），
		// 加 |deg 后缀避免与正常路径的并发 Key 混用
		return ConcurrencyKey(meta.Biz, r.Dimensions, vals) + "|deg",
			r.MaxConc, time.Duration(w) * time.Second
	}

	if r.winSec <= 0 {
		// 速率规则理应有窗口；走到这里说明规则未经 Validate 或数据损坏。
		// 返回 window=0 让调用方保守拒绝，而不是硬算导致除零。
		return "", r.Limit, 0
	}
	b := WindowBoundary(time.Now(), r.winSec, l.tzOffset, r.natural)
	return RateKey(r.Dimensions, vals, r.winSec, b),
		r.Limit, time.Duration(r.winSec) * time.Second
}

// failModeOf 决定单条规则的降级策略。
// metric=token 关系到真金白银的预算，默认本地保守配额；request 类默认放行。
func (l *Limiter) failModeOf(r *Rule) FailMode {
	if r.Type == TypeRate && r.Metric == MetricToken {
		return FailLocalQuota
	}
	if r.Type == TypeConcurrency {
		return FailLocalQuota
	}
	return FailOpen
}

// Release 释放本请求持有的并发额度。必须在响应结束时无条件调用。
func (l *Limiter) Release(ctx context.Context, hold *concHold) error {
	if hold == nil || len(hold.keys) == 0 {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	return releaseConcScript.Run(rctx, l.rdb, hold.keys, hold.reqID).Err()
}

func parseResult(res []interface{}) (ok bool, failIdx int, retryMs int64) {
	if len(res) < 3 {
		return false, 0, 0
	}
	toI := func(v interface{}) int64 {
		switch n := v.(type) {
		case int64:
			return n
		case float64:
			return int64(n)
		}
		return 0
	}
	return toI(res[0]) == 1, int(toI(res[1])), toI(res[2])
}
