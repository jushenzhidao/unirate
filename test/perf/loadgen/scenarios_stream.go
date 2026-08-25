package main

// 流式场景（C/D）—— 与非流式场景分文件，因为它们的被测量完全不同：
// C/D 关心 TTFB、帧数、泄漏，A/B 关心 QPS 与拒绝精度。

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 场景 C：高并发 SSE 长连接
//
// 核心指标是 TTFB（首字节延迟）—— ADR-005 的硬闸门：
// 任何 TTFB 上升都说明池化引入了攒包，必须回退。
// 因此这里精确测量「请求发出 → 收到第一个 SSE 帧」的时间，
// 而非整流耗时（后者被上游 delay_ms 主导，对缓冲变化不敏感）。
// ---------------------------------------------------------------------------

func (h *harness) scenarioC(conc, chunks, delayMS int, dur time.Duration) (roundResult, error) {
	var rr roundResult
	rr.MetricsBefore, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisBefore, _ = h.redisInfo()

	url := fmt.Sprintf("%s/%s/v1/chat/completions?stream=1&chunks=%d&delay_ms=%d",
		h.cfg.ProxyURL, perfBizThroughput, chunks, delayMS)

	var ttfb latencySet
	var ttfbMu sync.Mutex
	var frames, bytes, errs int64
	codes := newCodeCounter()

	lat := h.runConcurrent(conc, dur, func(id, n int, ls *latencySet) {
		t := time.Now()
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Authorization", "Bearer perf-c-"+itoa(id))
		resp, err := h.client.Do(req)
		if err != nil {
			atomic.AddInt64(&errs, 1)
			return
		}
		codes.inc(resp.StatusCode)

		br := bufio.NewReaderSize(resp.Body, 4096)
		first := true
		for {
			line, err := br.ReadSlice('\n')
			if len(line) > 0 {
				atomic.AddInt64(&bytes, int64(len(line)))
				if first && strings.HasPrefix(string(line), "data:") {
					ttfbMu.Lock()
					ttfb.add(time.Since(t)) // 首个 data 帧到达
					ttfbMu.Unlock()
					first = false
				}
				if len(strings.TrimRight(string(line), "\r\n")) == 0 {
					atomic.AddInt64(&frames, 1)
				}
			}
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		ls.add(time.Since(t))
	})

	rr.QPS = float64(lat.count()) / h.lastElapsed.Seconds()
	rr.Lat = lat.percentiles()
	rr.TTFB = ttfb.percentiles()
	rr.Codes = codes.snapshot()
	rr.Frames = frames
	rr.Bytes = bytes
	rr.Errors = errs
	rr.Duration = h.lastElapsed
	rr.MetricsAfter, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisAfter, _ = h.redisInfo()
	return rr, nil
}

// ---------------------------------------------------------------------------
// 场景 D：混合流量（70% SSE + 30% 非流式），查内存/并发额度泄漏
// ---------------------------------------------------------------------------

func (h *harness) scenarioD(conc int, dur time.Duration) (roundResult, error) {
	var rr roundResult
	rr.MetricsBefore, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisBefore, _ = h.redisInfo()

	sseURL := h.cfg.ProxyURL + "/" + perfBizThroughput + "/v1/chat/completions?stream=1&chunks=30&delay_ms=10"
	jsonURL := h.cfg.ProxyURL + "/" + perfBizThroughput + "/v1/chat/completions"
	codes := newCodeCounter()
	var frames, bytes, errs int64

	lat := h.runConcurrent(conc, dur, func(id, n int, ls *latencySet) {
		t := time.Now()
		wantSSE := (id*7+n)%10 < 7 // 70% 流式
		var req *http.Request
		if wantSSE {
			req, _ = http.NewRequest(http.MethodGet, sseURL, nil)
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req, _ = http.NewRequest(http.MethodPost, jsonURL,
				strings.NewReader(`{"model":"mock","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer perf-d-"+itoa(id))
		resp, err := h.client.Do(req)
		if err != nil {
			atomic.AddInt64(&errs, 1)
			return
		}
		codes.inc(resp.StatusCode)
		if wantSSE {
			br := bufio.NewReaderSize(resp.Body, 4096)
			for {
				line, err := br.ReadSlice('\n')
				atomic.AddInt64(&bytes, int64(len(line)))
				if len(strings.TrimRight(string(line), "\r\n")) == 0 && len(line) > 0 {
					atomic.AddInt64(&frames, 1)
				}
				if err != nil {
					break
				}
			}
		} else {
			nb, _ := io.Copy(io.Discard, resp.Body)
			atomic.AddInt64(&bytes, nb)
		}
		resp.Body.Close()
		ls.add(time.Since(t))
	})

	rr.QPS = float64(lat.count()) / h.lastElapsed.Seconds()
	rr.Lat = lat.percentiles()
	rr.Codes = codes.snapshot()
	rr.Frames = frames
	rr.Bytes = bytes
	rr.Errors = errs
	rr.Duration = h.lastElapsed
	// SSE 流需要时间收尾，稍等再采集，否则 in_flight 尚未归零会误报泄漏
	time.Sleep(2 * time.Second)
	rr.MetricsAfter, _ = scrapeMetrics(h.cfg.ObsURL)
	rr.RedisAfter, _ = h.redisInfo()
	return rr, nil
}
