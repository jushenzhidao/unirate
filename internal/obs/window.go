package obs

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 滚动窗口速率指标（RPM / TPM）。
//
// 为什么不直接让 Prometheus 用 rate() 算：
//  1. 控制台看板需要「首屏即有值」。基于 counter 差分的前端至少要等两个
//     采集周期（默认 10s）才能显示第一个速率，重启后同样如此。
//  2. rate() 的窗口由查询方决定，与告警阈值容易不一致；服务端固定 60s
//     窗口能让看板、告警、人工排查看到同一个数。
//
// 代价是内存里多一份 60 桶的环形数组，且窗口宽度写死为 60s。对于速率
// 展示这是可接受的取舍 —— 需要任意窗口的分析仍然走 Prometheus 的 counter。
//
// 精度说明：桶粒度为 1s，读取时会包含当前未走完的那一秒，因此瞬时值
// 天然略低于真实值（最多低 1/60）。这比"等一分钟才有数"更实用。

const windowBuckets = 60

// ringWindow 单个标签组合的 60s 滚动窗口。
//
// 无锁设计：每个桶自带 epoch（该桶所属的 Unix 秒）。写入时若发现 epoch
// 落后，说明这个桶是上一轮的陈旧数据，先 CAS 抢占清零权再累加。
// 读取时同样按 epoch 过滤，超出窗口的桶直接忽略 —— 不需要后台清理协程。
type ringWindow struct {
	counts [windowBuckets]atomic.Int64
	epochs [windowBuckets]atomic.Int64
}

func (r *ringWindow) add(n int64) {
	now := time.Now().Unix()
	i := now % windowBuckets
	// 抢占清零：只有把 epoch 从旧值换成 now 成功的那个 goroutine 负责重置。
	//
	// 必须先捕获 old 再比较，不能写成 CompareAndSwap(r.epochs[i].Load(), now)：
	// 那样两次 Load 之间若被其他 goroutine 改写，CAS 仍可能成功，导致该桶
	// 被重复清零，已累加的计数凭空消失。
	if old := r.epochs[i].Load(); old != now {
		if r.epochs[i].CompareAndSwap(old, now) {
			r.counts[i].Store(0)
		}
	}
	r.counts[i].Add(n)
}

// sum 返回窗口内总量。cutoff 之前的桶视为过期。
func (r *ringWindow) sum() int64 {
	now := time.Now().Unix()
	cutoff := now - windowBuckets + 1
	var total int64
	for i := 0; i < windowBuckets; i++ {
		if r.epochs[i].Load() >= cutoff {
			total += r.counts[i].Load()
		}
	}
	return total
}

// rateVec 一组带标签的滚动窗口
type rateVec struct {
	name string
	help string
	keys []string

	mu   sync.RWMutex
	vals map[string]*ringWindow
}

func newRateVec(name, help string, keys ...string) *rateVec {
	return &rateVec{
		name: name,
		help: help,
		keys: keys,
		vals: make(map[string]*ringWindow),
	}
}

func (v *rateVec) win(labels ...string) *ringWindow {
	k := strings.Join(labels, "\x1f")
	v.mu.RLock()
	w := v.vals[k]
	v.mu.RUnlock()
	if w != nil {
		return w
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// 双重检查：RUnlock 与 Lock 之间可能已被其他 goroutine 创建
	if w = v.vals[k]; w == nil {
		w = &ringWindow{}
		v.vals[k] = w
	}
	return w
}

// Inc 记录一次事件
func (v *rateVec) Inc(labels ...string) { v.win(labels...).add(1) }

// Add 记录一批数量
func (v *rateVec) Add(n int64, labels ...string) {
	if n != 0 {
		v.win(labels...).add(n)
	}
}

// Value 返回当前 60s 窗口内的总量，供 /admin/metrics 直接消费
func (v *rateVec) Value(labels ...string) int64 { return v.win(labels...).sum() }

// Snapshot 返回全部标签组合的当前窗口值
func (v *rateVec) Snapshot() map[string]int64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]int64, len(v.vals))
	for k, w := range v.vals {
		out[k] = w.sum()
	}
	return out
}

// write 以 gauge 类型导出。
//
// 语义上它是「每分钟速率」而非单调累计值，用 counter 类型会让
// Prometheus 侧的 rate() 得到毫无意义的"速率的速率"。
func (v *rateVec) write(sb *strings.Builder) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.vals) == 0 {
		return
	}
	full := nsPrefix + v.name
	sb.WriteString("# HELP " + full + " " + v.help + "\n")
	sb.WriteString("# TYPE " + full + " gauge\n")
	// 与 counterVec/gaugeVec 一致地排序输出：抓取端做文本 diff 时
	// 顺序抖动会产生大量伪变更。
	keys := make([]string, 0, len(v.vals))
	for k := range v.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(full)
		writeLabels(sb, v.keys, strings.Split(k, "\x1f"))
		sb.WriteString(" " + strconv.FormatInt(v.vals[k].sum(), 10) + "\n")
	}
}
