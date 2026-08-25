package limiter

import (
	"sync"
	"time"
)

// localFallback 本地状态，承担两个职责：
//
//  1. blocked/block —— 「已知超限」负缓存。仅用于短窗口规则的快速拒绝，
//     它只会让判定更严格，绝不放宽配额，因此不违反集群语义（评审 P0-1 的约束）。
//  2. allowQuota   —— Redis 不可用时的本地保守配额（评审 P1-10 的 local_quota 降级）。
//
// 刻意不在此实现「本地令牌桶做主判定」—— 那正是评审 P0-1 指出的
// 「N 实例各持一桶导致实际配额放大 N 倍」的根因。
type localFallback struct {
	mu     sync.RWMutex
	blocks map[string]time.Time
	quotas map[string]*quotaEntry
	lastGC time.Time
}

type quotaEntry struct {
	count   int64
	resetAt time.Time
}

func newLocalFallback() *localFallback {
	return &localFallback{
		blocks: make(map[string]time.Time),
		quotas: make(map[string]*quotaEntry),
		lastGC: time.Now(),
	}
}

func (l *localFallback) blocked(key string, now time.Time) (time.Time, bool) {
	l.mu.RLock()
	until, ok := l.blocks[key]
	l.mu.RUnlock()
	if !ok || now.After(until) {
		return time.Time{}, false
	}
	return until, true
}

func (l *localFallback) block(key string, until time.Time) {
	l.mu.Lock()
	l.blocks[key] = until
	l.gcLocked(time.Now())
	l.mu.Unlock()
}

// allowQuota 本地保守配额判定，仅在 Redis 降级期间使用
func (l *localFallback) allowQuota(key string, limit int64, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.quotas[key]
	if !ok || now.After(e.resetAt) {
		e = &quotaEntry{count: 0, resetAt: now.Add(window)}
		l.quotas[key] = e
	}
	if e.count >= limit {
		return false
	}
	e.count++
	l.gcLocked(now)
	return true
}

// gcLocked 惰性清理过期条目，避免 map 无界增长导致内存泄漏
func (l *localFallback) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < 30*time.Second {
		return
	}
	l.lastGC = now
	for k, v := range l.blocks {
		if now.After(v) {
			delete(l.blocks, k)
		}
	}
	for k, v := range l.quotas {
		if now.After(v.resetAt) {
			delete(l.quotas, k)
		}
	}
}
