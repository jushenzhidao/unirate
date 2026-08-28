package obs

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateVecAccumulates(t *testing.T) {
	v := newRateVec("t_rpm", "test.", "biz")
	for i := 0; i < 7; i++ {
		v.Inc("a")
	}
	v.Add(35, "b")

	if got := v.Value("a"); got != 7 {
		t.Fatalf("biz=a want 7, got %d", got)
	}
	if got := v.Value("b"); got != 35 {
		t.Fatalf("biz=b want 35, got %d", got)
	}
	if got := v.Value("never-seen"); got != 0 {
		t.Fatalf("unseen label want 0, got %d", got)
	}
}

// 并发写入不得丢计数。
//
// 这是本实现最容易出错的地方：桶的「抢占清零」若写成两次 Load 之间
// 做 CAS，多个 goroutine 可能同时清零同一个桶，计数凭空消失。
// -race 下跑此用例可同时验证无数据竞争。
func TestRateVecConcurrentNoLoss(t *testing.T) {
	v := newRateVec("t_concurrent", "test.", "biz")
	const (
		goroutines = 64
		perG       = 500
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				v.Inc("hot")
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perG)
	if got := v.Value("hot"); got != want {
		t.Fatalf("lost counts under concurrency: want %d, got %d", want, got)
	}
}

// 窗口内的桶不因时间推进而误判过期。
// 直接操纵 epoch 模拟「上一分钟的陈旧桶」，避免让测试等待 60 秒。
func TestRingWindowExpiresStaleBuckets(t *testing.T) {
	var r ringWindow
	now := time.Now().Unix()

	// 一个落在窗口内的桶
	fresh := now % windowBuckets
	r.epochs[fresh].Store(now)
	r.counts[fresh].Store(11)

	// 一个 epoch 恰好超出窗口的桶：值必须被忽略
	staleIdx := (now + 5) % windowBuckets
	r.epochs[staleIdx].Store(now - windowBuckets)
	r.counts[staleIdx].Store(9999)

	if got := r.sum(); got != 11 {
		t.Fatalf("stale bucket leaked into window: want 11, got %d", got)
	}
}

// 陈旧桶被复用时必须先清零，而不是在旧值上继续累加。
func TestRingWindowResetsOnReuse(t *testing.T) {
	var r ringWindow
	now := time.Now().Unix()
	i := now % windowBuckets
	// 伪造一个上一轮遗留的同槽位计数
	r.epochs[i].Store(now - windowBuckets)
	r.counts[i].Store(500)

	r.add(3)

	if got := r.counts[i].Load(); got != 3 {
		t.Fatalf("reused bucket kept stale value: want 3, got %d", got)
	}
}

// rateVec 必须以 gauge 导出。
// 用 counter 会让 Prometheus 的 rate() 计算"速率的速率"，得到无意义的值。
func TestRateVecExportsAsGauge(t *testing.T) {
	v := newRateVec("t_export", "Test rate.", "biz")
	v.Add(4, "acme")

	var sb strings.Builder
	v.write(&sb)
	out := sb.String()

	if !strings.Contains(out, "# TYPE unirate_t_export gauge") {
		t.Fatalf("rate metric must be a gauge, got:\n%s", out)
	}
	if !strings.Contains(out, `unirate_t_export{biz="acme"} 4`) {
		t.Fatalf("missing labelled sample, got:\n%s", out)
	}
}

// 空 vec 不应输出任何行（含 HELP/TYPE）。
// 否则 /metrics 会出现一堆无样本的孤立元数据。
func TestRateVecEmptyWritesNothing(t *testing.T) {
	v := newRateVec("t_empty", "Test.", "biz")
	var sb strings.Builder
	v.write(&sb)
	if sb.Len() != 0 {
		t.Fatalf("empty vec should emit nothing, got: %q", sb.String())
	}
}

// Render 与请求路径并发时不得竞争（配合 -race）。
func TestRenderConcurrentWithWrites(t *testing.T) {
	m := NewMetrics()
	var stop atomic.Bool
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			m.RPM.Inc("acme")
			m.TPM.Add(120, "acme")
			m.TTFT.Observe(0.42, "acme")
			m.TokensByKind.Add(30, "acme", "prompt")
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			m.SSEFrames.Inc("acme")
			m.StreamDuration.Observe(3.5, "acme")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = m.Render()
		}
		stop.Store(true)
	}()
	wg.Wait()

	out := m.Render()
	for _, want := range []string{
		"unirate_requests_per_minute",
		"unirate_tokens_per_minute",
		"unirate_ttft_seconds",
		"unirate_tokens_by_kind_total",
		"unirate_sse_frames_total",
		"unirate_stream_duration_seconds",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %s", want)
		}
	}
}
