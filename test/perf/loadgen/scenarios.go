package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 四个场景对应 ADR-008 §二。每个场景返回 roundResult，由 main 做多轮中位数汇总。

// roundResult 单轮结果
type roundResult struct {
	QPS           float64
	Lat           percentiles
	TTFB          percentiles // 仅场景 C
	Codes         map[int]int64
	Passed        int64 // 场景 B：200 计数
	Rejected      int64 // 场景 B：429 计数
	Errors        int64
	Bytes         int64
	Frames        int64
	Duration      time.Duration
	MetricsBefore gatewayMetrics
	MetricsAfter  gatewayMetrics
	RedisBefore   redisStats
	RedisAfter    redisStats
}

// codeCounter 并发安全的状态码计数
type codeCounter struct {
	mu sync.Mutex
	m  map[int]int64
}

func newCodeCounter() *codeCounter { return &codeCounter{m: map[int]int64{}} }

func (c *codeCounter) inc(code int) {
	c.mu.Lock()
	c.m[code]++
	c.mu.Unlock()
}

func (c *codeCounter) snapshot() map[int]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[int]int64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// 场景 A：纯 QPS 吞吐（非流式）
// ---------------------------------------------------------------------------

func (h *harness) scenarioA(conc int, dur time.Duration) (roundResult, error) {
	var rr roundResult
	rr.MetricsBefore, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisBefore, _ = h.redisInfo()

	url := h.cfg.ProxyURL + "/" + perfBizThroughput + "/v1/chat/completions"
	codes := newCodeCounter()
	var errs, bytes int64

	lat := h.runConcurrent(conc, dur, func(id, n int, ls *latencySet) {
		t := time.Now()
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"mock","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		// 每 worker 独立 token：让 biz.token 维度分散，避免所有请求撞同一个 sliding 规则 Key
		req.Header.Set("Authorization", "Bearer perf-a-"+itoa(id))
		resp, err := h.client.Do(req)
		if err != nil {
			atomic.AddInt64(&errs, 1)
			return
		}
		nb, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		atomic.AddInt64(&bytes, nb)
		codes.inc(resp.StatusCode)
		ls.add(time.Since(t))
	})

	rr.QPS = float64(lat.count()) / h.lastElapsed.Seconds()
	rr.Lat = lat.percentiles()
	rr.Codes = codes.snapshot()
	rr.Errors = errs
	rr.Bytes = bytes
	rr.Duration = h.lastElapsed
	rr.MetricsAfter, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisAfter, _ = h.redisInfo()
	return rr, nil
}

// ---------------------------------------------------------------------------
// 场景 B：拒绝路径精度 —— limit=50 打 500 并发须恰好通过 50
//
// 这不是性能指标，是限流正确性的硬断言（P0-1 配额放大 / P0-5 计数器污染的回归闸门）。
// 实现要点：
//  1. 全部 500 个请求必须落在**同一个窗口**内，否则会混合两个窗口的配额
//     使断言失去意义。做法是先 sleep 到窗口边界后再齐发（对齐 e2e run.sh 的思路）。
//  2. 用固定 token + 固定 IP 维度，确保所有请求命中同一条规则的同一个 Key。
//  3. 请求打向 mock 的 /status/200，避免上游处理耗时把请求推出窗口。
// ---------------------------------------------------------------------------

func (h *harness) scenarioB(conc int, limit int64) (roundResult, error) {
	var rr roundResult
	rr.MetricsBefore, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisBefore, _ = h.redisInfo()

	// 用专属 biz（perfb）而非 demo —— demo 的 limit=10 且叠加了
	// sliding / concurrency / token 多条规则，无法对「恰好通过 limit 个」做单一归因。
	// perfb 只挂一条 fixed_window 规则，断言才成立。
	url := h.cfg.ProxyURL + "/" + perfBizB + "/status/200"
	codes := newCodeCounter()
	var passed, rejected, errs int64

	// 对齐到下一个整秒边界，最大化「整批落在同一 1s 窗口」的概率
	now := time.Now()
	wait := time.Until(now.Truncate(time.Second).Add(time.Second))
	if wait < 150*time.Millisecond {
		wait += time.Second // 余量太小则等下一个窗口
	}
	time.Sleep(wait)

	var lat latencySet
	var latMu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start // 齐发
			t := time.Now()
			req, _ := http.NewRequest(http.MethodGet, url, nil)
			req.Header.Set("Authorization", "Bearer perf-b-fixed")
			resp, err := h.client.Do(req)
			if err != nil {
				atomic.AddInt64(&errs, 1)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes.inc(resp.StatusCode)
			switch resp.StatusCode {
			case http.StatusOK:
				atomic.AddInt64(&passed, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&rejected, 1)
			}
			latMu.Lock()
			lat.add(time.Since(t))
			latMu.Unlock()
		}(i)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	h.lastElapsed = time.Since(t0)

	rr.QPS = float64(lat.count()) / h.lastElapsed.Seconds()
	rr.Lat = lat.percentiles()
	rr.Codes = codes.snapshot()
	rr.Passed = passed
	rr.Rejected = rejected
	rr.Errors = errs
	rr.Duration = h.lastElapsed
	rr.MetricsAfter, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisAfter, _ = h.redisInfo()
	return rr, nil
}

// runConcurrent 通用并发驱动：conc 个 worker 持续压测 dur 时长。
func (h *harness) runConcurrent(conc int, dur time.Duration,
	fn func(id, n int, ls *latencySet)) *latencySet {

	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	sets := make([]*latencySet, conc)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < conc; i++ {
		sets[i] = &latencySet{}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for n := 0; ctx.Err() == nil; n++ {
				fn(id, n, sets[id])
			}
		}(i)
	}
	wg.Wait()
	h.lastElapsed = time.Since(start)

	merged := &latencySet{}
	for _, s := range sets {
		merged.merge(s)
	}
	return merged
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}
