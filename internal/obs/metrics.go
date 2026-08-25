package obs

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// 可观测性（对应 Spec §5 与评审 P1-11）
//
// 实现取舍：不引入 prometheus/client_golang。
// 理由：本组件只需 counter/gauge/histogram 三类基础指标，标签基数受控（biz/rule/decision），
// 自实现约 200 行且零依赖，可显著缩小镜像体积与供应链面。
// 输出严格遵循 Prometheus text exposition format v0.0.4，可被任意 Prometheus 抓取。
//
// 关键约束：标签基数必须受控。path 维度不进指标标签 ——
// 原 Spec §5.1 的 ratelimit_rejected_total{rule,dimension} 中 dimension 若含 path 取值，
// 高基数会打爆 Prometheus。此处 dimension 只暴露「维度名组合」（如 biz.path），不含取值。

const nsPrefix = "unirate_"

type counterVec struct {
	name   string
	help   string
	labels []string
	mu     sync.RWMutex
	vals   map[string]*atomic.Int64
}

func newCounterVec(name, help string, labels ...string) *counterVec {
	return &counterVec{name: name, help: help, labels: labels, vals: map[string]*atomic.Int64{}}
}

func (c *counterVec) Add(n int64, lv ...string) {
	k := strings.Join(lv, "\x1f")
	c.mu.RLock()
	v, ok := c.vals[k]
	c.mu.RUnlock()
	if !ok {
		c.mu.Lock()
		if v, ok = c.vals[k]; !ok {
			v = &atomic.Int64{}
			c.vals[k] = v
		}
		c.mu.Unlock()
	}
	v.Add(n)
}

func (c *counterVec) Inc(lv ...string) { c.Add(1, lv...) }

func (c *counterVec) write(sb *strings.Builder) {
	sb.WriteString("# HELP " + nsPrefix + c.name + " " + c.help + "\n")
	sb.WriteString("# TYPE " + nsPrefix + c.name + " counter\n")
	c.mu.RLock()
	keys := make([]string, 0, len(c.vals))
	for k := range c.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(nsPrefix + c.name)
		writeLabels(sb, c.labels, strings.Split(k, "\x1f"))
		sb.WriteString(" " + strconv.FormatInt(c.vals[k].Load(), 10) + "\n")
	}
	c.mu.RUnlock()
}

type gaugeVec struct {
	name   string
	help   string
	labels []string
	mu     sync.RWMutex
	vals   map[string]*atomic.Int64
}

func newGaugeVec(name, help string, labels ...string) *gaugeVec {
	return &gaugeVec{name: name, help: help, labels: labels, vals: map[string]*atomic.Int64{}}
}

func (g *gaugeVec) Set(n int64, lv ...string) {
	k := strings.Join(lv, "\x1f")
	g.mu.RLock()
	v, ok := g.vals[k]
	g.mu.RUnlock()
	if !ok {
		g.mu.Lock()
		if v, ok = g.vals[k]; !ok {
			v = &atomic.Int64{}
			g.vals[k] = v
		}
		g.mu.Unlock()
	}
	v.Store(n)
}

func (g *gaugeVec) Add(n int64, lv ...string) {
	k := strings.Join(lv, "\x1f")
	g.mu.RLock()
	v, ok := g.vals[k]
	g.mu.RUnlock()
	if !ok {
		g.mu.Lock()
		if v, ok = g.vals[k]; !ok {
			v = &atomic.Int64{}
			g.vals[k] = v
		}
		g.mu.Unlock()
	}
	v.Add(n)
}

func (g *gaugeVec) write(sb *strings.Builder) {
	sb.WriteString("# HELP " + nsPrefix + g.name + " " + g.help + "\n")
	sb.WriteString("# TYPE " + nsPrefix + g.name + " gauge\n")
	g.mu.RLock()
	keys := make([]string, 0, len(g.vals))
	for k := range g.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(nsPrefix + g.name)
		writeLabels(sb, g.labels, strings.Split(k, "\x1f"))
		sb.WriteString(" " + strconv.FormatInt(g.vals[k].Load(), 10) + "\n")
	}
	g.mu.RUnlock()
}

// histogramVec 固定分桶直方图（秒）
type histogramVec struct {
	name   string
	help   string
	labels []string
	bounds []float64
	mu     sync.RWMutex
	vals   map[string]*histBuckets
}

type histBuckets struct {
	counts []atomic.Int64
	sum    atomic.Uint64 // float64 bits 累加需 CAS，这里用微秒整数避免浮点原子操作
	total  atomic.Int64
}

func newHistogramVec(name, help string, bounds []float64, labels ...string) *histogramVec {
	return &histogramVec{name: name, help: help, labels: labels, bounds: bounds, vals: map[string]*histBuckets{}}
}

func (h *histogramVec) Observe(sec float64, lv ...string) {
	k := strings.Join(lv, "\x1f")
	h.mu.RLock()
	b, ok := h.vals[k]
	h.mu.RUnlock()
	if !ok {
		h.mu.Lock()
		if b, ok = h.vals[k]; !ok {
			b = &histBuckets{counts: make([]atomic.Int64, len(h.bounds)+1)}
			h.vals[k] = b
		}
		h.mu.Unlock()
	}
	i := sort.SearchFloat64s(h.bounds, sec)
	b.counts[i].Add(1)
	b.total.Add(1)
	b.sum.Add(uint64(sec * 1e6))
}

func (h *histogramVec) write(sb *strings.Builder) {
	sb.WriteString("# HELP " + nsPrefix + h.name + " " + h.help + "\n")
	sb.WriteString("# TYPE " + nsPrefix + h.name + " histogram\n")
	h.mu.RLock()
	keys := make([]string, 0, len(h.vals))
	for k := range h.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := h.vals[k]
		lv := strings.Split(k, "\x1f")
		var cum int64
		for i, bound := range h.bounds {
			cum += b.counts[i].Load()
			sb.WriteString(nsPrefix + h.name + "_bucket")
			writeLabelsExtra(sb, h.labels, lv, "le", strconv.FormatFloat(bound, 'g', -1, 64))
			sb.WriteString(" " + strconv.FormatInt(cum, 10) + "\n")
		}
		cum += b.counts[len(h.bounds)].Load()
		sb.WriteString(nsPrefix + h.name + "_bucket")
		writeLabelsExtra(sb, h.labels, lv, "le", "+Inf")
		sb.WriteString(" " + strconv.FormatInt(cum, 10) + "\n")

		sb.WriteString(nsPrefix + h.name + "_sum")
		writeLabels(sb, h.labels, lv)
		sb.WriteString(" " + strconv.FormatFloat(float64(b.sum.Load())/1e6, 'f', 6, 64) + "\n")

		sb.WriteString(nsPrefix + h.name + "_count")
		writeLabels(sb, h.labels, lv)
		sb.WriteString(" " + strconv.FormatInt(b.total.Load(), 10) + "\n")
	}
	h.mu.RUnlock()
}

func escapeLabel(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

func writeLabels(sb *strings.Builder, names, vals []string) {
	if len(names) == 0 {
		return
	}
	sb.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			sb.WriteByte(',')
		}
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		sb.WriteString(n + `="` + escapeLabel(v) + `"`)
	}
	sb.WriteByte('}')
}

func writeLabelsExtra(sb *strings.Builder, names, vals []string, xn, xv string) {
	sb.WriteByte('{')
	for i, n := range names {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		sb.WriteString(n + `="` + escapeLabel(v) + `",`)
	}
	sb.WriteString(xn + `="` + xv + `"}`)
}

// Metrics 集合本体、渲染与 /metrics 处理器见 registry.go。
