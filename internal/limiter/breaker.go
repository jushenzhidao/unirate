package limiter

import (
	"sync"
	"sync/atomic"
	"time"
)

// breaker 区分「Redis 偶发抖动」与「Redis 真正不可用」。
//
// 设计动机（压测中实测发现，评审报告未覆盖）：
//
//	评审 P1-10 只讨论了「Redis 宕机 → Fail-Open」的策略问题，但生产中更高频、
//	更隐蔽的失效路径是「Redis 没挂，只是连接池打满或偶发超时」。
//	若把单次超时等同于故障并立即 Fail-Open，则在高并发压力下限流会静默失效 ——
//	实测 500 并发打 limit=50 的规则，因超时放行导致实际通过 134 个（超卖 168%）。
//	而这恰恰发生在流量高峰，也就是最需要限流生效的时刻。
//
// 因此策略修正为：
//   - 健康状态（未熔断）下的偶发错误 → 一律按 Fail-Close 处理并打点。
//     宁可错拒少量请求，也不能在高峰期把闸门整个打开。
//   - 仅当滑动窗口内错误率持续超阈值 → 判定为真正故障，进入降级模式，
//     此时才启用按 metric 分治的 Fail-Open / local_quota 策略。
//   - 熔断后经过冷却期进入半开，探测成功即恢复。
type breaker struct {
	mu sync.Mutex

	failures  int
	successes int
	windowAt  time.Time

	openUntil time.Time
	state     atomic.Int32 // 0=closed 1=open 2=half-open

	threshold int           // 窗口内触发熔断的错误次数
	ratio     float64       // 窗口内触发熔断的错误率
	window    time.Duration // 统计窗口
	cooldown  time.Duration // 熔断冷却期

	totalErrors atomic.Int64
	totalTrips  atomic.Int64
}

const (
	stateClosed int32 = iota
	stateOpen
	stateHalfOpen
)

func newBreaker() *breaker {
	return &breaker{
		threshold: 20,
		ratio:     0.5,
		window:    5 * time.Second,
		cooldown:  2 * time.Second,
		windowAt:  time.Now(),
	}
}

// degraded 返回当前是否处于「已确认的故障降级」状态。
// 返回 false 时，调用方对 Redis 错误必须按 Fail-Close 处理。
func (b *breaker) degraded() bool {
	switch b.state.Load() {
	case stateClosed:
		return false
	case stateOpen:
		b.mu.Lock()
		defer b.mu.Unlock()
		if time.Now().After(b.openUntil) {
			b.state.Store(stateHalfOpen)
			return true // 半开期仍视为降级，允许探测请求走降级路径
		}
		return true
	default:
		return true
	}
}

func (b *breaker) onSuccess() {
	if b.state.Load() == stateClosed {
		b.mu.Lock()
		b.successes++
		b.rollLocked(time.Now())
		b.mu.Unlock()
		return
	}
	// 半开/熔断期探测成功 → 立即恢复
	b.mu.Lock()
	b.state.Store(stateClosed)
	b.failures, b.successes, b.windowAt = 0, 0, time.Now()
	b.mu.Unlock()
}

func (b *breaker) onFailure() {
	b.totalErrors.Add(1)
	now := time.Now()

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state.Load() == stateHalfOpen {
		b.state.Store(stateOpen)
		b.openUntil = now.Add(b.cooldown)
		return
	}

	b.rollLocked(now)
	b.failures++

	total := b.failures + b.successes
	if b.failures >= b.threshold && total > 0 &&
		float64(b.failures)/float64(total) >= b.ratio {
		b.state.Store(stateOpen)
		b.openUntil = now.Add(b.cooldown)
		b.totalTrips.Add(1)
		b.failures, b.successes = 0, 0
	}
}

func (b *breaker) rollLocked(now time.Time) {
	if now.Sub(b.windowAt) > b.window {
		b.failures, b.successes, b.windowAt = 0, 0, now
	}
}

// Stats 供监控暴露
func (b *breaker) Stats() (errors, trips int64, open bool) {
	return b.totalErrors.Load(), b.totalTrips.Load(), b.state.Load() != stateClosed
}
