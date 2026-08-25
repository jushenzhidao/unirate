package limiter

import (
	"context"
	"time"
)

// TokenAdmit 在请求准入阶段检查 Token 预算是否已耗尽。
//
// 语义澄清（Spec 未定义，此处定案）：
//
//	Token 消耗只有在响应结束后才可知，因此准入期无法做精确判定。
//	这里只回答「当前窗口预算是否已经用完」——已用完则拒绝新请求，
//	在途请求不受影响（不会中断已建立的 SSE 流）。
//	这一取舍意味着单窗口最多超支「并发数 × 单请求消耗」，属可接受误差；
//	若需硬上限，应在业务侧配合 max_tokens 参数约束。
func (l *Limiter) TokenAdmit(ctx context.Context, rules []*Rule, meta *Meta) (Decision, error) {
	now := time.Now()
	for _, r := range rules {
		if !r.IsEnabled() || r.Type != TypeRate || r.Metric != MetricToken {
			continue
		}
		vals := meta.dimValues(r.Dimensions)
		b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
		key := TokenLedgerKey(r.Dimensions, vals, r.winSec, b)

		rctx, cancel := context.WithTimeout(ctx, l.timeout)
		res, err := tokenLedgerScript.Run(rctx, l.rdb, []string{key}, "admit", r.Limit).Slice()
		cancel()

		if err != nil {
			l.breaker.onFailure()
			// Token 预算降级：本地保守配额，绝不 Fail-Open（评审 P1-10）
			quota := l.localQuota(r.Limit)
			if !l.local.allowQuota(key+"|admit", quota, time.Duration(r.winSec)*time.Second) {
				return Decision{
					Allowed: false, RuleName: r.Name, RuleID: r.ID,
					Dimension: r.DimKey(), RetryAfter: time.Second, Degraded: true,
				}, nil
			}
			continue
		}

		ok, _, pttl := parseResult(res)
		if !ok {
			retry := time.Duration(pttl) * time.Millisecond
			if retry <= 0 {
				retry = time.Duration(r.winSec) * time.Second
			}
			return Decision{
				Allowed: false, RuleName: r.Name, RuleID: r.ID,
				Dimension: r.DimKey(), RetryAfter: retry,
			}, nil
		}
	}
	return Decision{Allowed: true}, nil
}

// TokenReserve 增量预扣 Token。
//
// 对应评审 P0-6：原设计「本地累加、SSE 结束一次性刷 Redis」使超卖窗口等于整个 SSE 时长。
// 本实现由调用方按固定间隔（默认 1s）增量刷盘，把超卖窗口压缩到秒级。
func (l *Limiter) TokenReserve(ctx context.Context, rules []*Rule, meta *Meta, delta int64) error {
	if delta <= 0 {
		return nil
	}
	now := time.Now()
	for _, r := range rules {
		if !r.IsEnabled() || r.Type != TypeRate || r.Metric != MetricToken {
			continue
		}
		vals := meta.dimValues(r.Dimensions)
		b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
		key := TokenLedgerKey(r.Dimensions, vals, r.winSec, b)

		rctx, cancel := context.WithTimeout(ctx, l.timeout)
		_ = tokenLedgerScript.Run(rctx, l.rdb, []string{key}, "reserve", delta, r.winSec+60).Err()
		cancel()
	}
	return nil
}

// TokenSettle 用上游返回的精确用量核销预扣，退回多扣部分。
//
// 对应评审 P0-6 核心修正：原设计只有「精确值覆盖估算值」的说法而无补偿算法，
// 导致 ×1.2 安全缓冲永远只扣不退，全局预算被系统性提前 20% 耗尽。
// 此处按 (actual - reserved) 做差额修正，差值为负即退回。
func (l *Limiter) TokenSettle(ctx context.Context, rules []*Rule, meta *Meta, reserved, actual int64) error {
	if reserved == actual {
		return nil
	}
	now := time.Now()
	for _, r := range rules {
		if !r.IsEnabled() || r.Type != TypeRate || r.Metric != MetricToken {
			continue
		}
		vals := meta.dimValues(r.Dimensions)
		b := WindowBoundary(now, r.winSec, l.tzOffset, r.natural)
		key := TokenLedgerKey(r.Dimensions, vals, r.winSec, b)

		rctx, cancel := context.WithTimeout(ctx, l.timeout)
		_ = tokenLedgerScript.Run(rctx, l.rdb, []string{key},
			"settle", reserved, actual, r.winSec+60).Err()
		cancel()
	}
	return nil
}

// TokenUsage 读取当前窗口已用量，供水位监控使用
func (l *Limiter) TokenUsage(ctx context.Context, r *Rule, meta *Meta) (int64, error) {
	vals := meta.dimValues(r.Dimensions)
	b := WindowBoundary(time.Now(), r.winSec, l.tzOffset, r.natural)
	key := TokenLedgerKey(r.Dimensions, vals, r.winSec, b)
	n, err := l.rdb.Get(ctx, key).Int64()
	if err != nil {
		return 0, err
	}
	return n, nil
}
