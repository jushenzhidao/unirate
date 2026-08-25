package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// latencySet 收集单轮的延迟样本并计算分位数。
//
// 刻意用「全量样本 + 排序」而非流式近似（HDR 直方图等）：
// 压测轮次样本量在百万级以内，排序耗时可忽略，
// 而精确分位数避免了近似算法在尾部（p999）的误差 ——
// 本项目验收依赖 TTFB 与 p99 的绝对值，不能有算法引入的偏差。
type latencySet struct {
	samples []time.Duration
	sorted  bool
}

func (l *latencySet) add(d time.Duration) {
	l.samples = append(l.samples, d)
	l.sorted = false
}

func (l *latencySet) merge(o *latencySet) {
	l.samples = append(l.samples, o.samples...)
	l.sorted = false
}

func (l *latencySet) ensureSorted() {
	if !l.sorted {
		sort.Slice(l.samples, func(i, j int) bool { return l.samples[i] < l.samples[j] })
		l.sorted = true
	}
}

func (l *latencySet) count() int { return len(l.samples) }

// quantile 返回指定分位数。q 取 [0,1]。
func (l *latencySet) quantile(q float64) time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	l.ensureSorted()
	idx := int(math.Round(float64(len(l.samples)-1) * q))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(l.samples) {
		idx = len(l.samples) - 1
	}
	return l.samples[idx]
}

func (l *latencySet) mean() time.Duration {
	if len(l.samples) == 0 {
		return 0
	}
	var sum time.Duration
	for _, s := range l.samples {
		sum += s
	}
	return sum / time.Duration(len(l.samples))
}

// percentiles 汇总常用分位数
type percentiles struct {
	P50  time.Duration `json:"p50"`
	P90  time.Duration `json:"p90"`
	P99  time.Duration `json:"p99"`
	P999 time.Duration `json:"p999"`
	Mean time.Duration `json:"mean"`
	Max  time.Duration `json:"max"`
	N    int           `json:"n"`
}

func (l *latencySet) percentiles() percentiles {
	return percentiles{
		P50:  l.quantile(0.50),
		P90:  l.quantile(0.90),
		P99:  l.quantile(0.99),
		P999: l.quantile(0.999),
		Mean: l.mean(),
		Max:  l.quantile(1.0),
		N:    l.count(),
	}
}

// MarshalJSON 以「毫秒浮点」输出，便于 baseline 文件人工比对。
// time.Duration 默认序列化为纳秒整数，可读性差。
func (p percentiles) forJSON() map[string]any {
	ms := func(d time.Duration) float64 {
		return math.Round(float64(d.Microseconds())/10) / 100 // 保留 2 位小数（ms）
	}
	return map[string]any{
		"p50_ms": ms(p.P50), "p90_ms": ms(p.P90),
		"p99_ms": ms(p.P99), "p999_ms": ms(p.P999),
		"mean_ms": ms(p.Mean), "max_ms": ms(p.Max),
		"n": p.N,
	}
}

func (p percentiles) String() string {
	return fmt.Sprintf("p50=%-9v p90=%-9v p99=%-9v p999=%-9v",
		p.P50.Round(time.Microsecond), p.P90.Round(time.Microsecond),
		p.P99.Round(time.Microsecond), p.P999.Round(time.Microsecond))
}

// medianOf 返回若干轮次数值的中位数。
//
// ADR-008 §四.4 要求「5 轮取中位数而非最优值」——
// 取最优值会系统性高估性能，且掩盖抖动；
// 中位数对偶发的邻居容器干扰不敏感，是本机多容器环境下的正确选择。
func medianOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	c := append([]float64(nil), vals...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

func medianDur(vals []time.Duration) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), vals...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
}

// spread 返回多轮数据的离散程度（最大偏离中位数的百分比），
// 用于判断本轮数据是否可信 —— ADR-008 记录同配置波动可达 ±8%，
// 若 spread 远超该值说明环境噪声过大，结论不应采信。
func spread(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	med := medianOf(vals)
	if med == 0 {
		return 0
	}
	var worst float64
	for _, v := range vals {
		d := math.Abs(v-med) / med * 100
		if d > worst {
			worst = d
		}
	}
	return math.Round(worst*10) / 10
}

func fmtFloat(f float64, dec int) string {
	return strings.TrimRight(strings.TrimRight(
		fmt.Sprintf("%.*f", dec, f), "0"), ".")
}
