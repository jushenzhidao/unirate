package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// flushRecorder 记录每次 Flush 时已写入的字节数，用于证明"零缓冲"
type flushRecorder struct {
	mu      sync.Mutex
	buf     strings.Builder
	flushes []int // 每次 Flush 时的累计字节数
	header  http.Header
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{header: http.Header{}}
}

func (f *flushRecorder) Header() http.Header { return f.header }
func (f *flushRecorder) WriteHeader(int)     {}
func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}
func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes = append(f.flushes, f.buf.Len())
}

// TestSSEZeroBuffering 验证每个 SSE 帧结束时立即 Flush。
// 这是流式体验的硬性要求：若攒够 N 帧才 Flush，客户端会感到明显卡顿。
func TestSSEZeroBuffering(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"B\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"C\"}}]}\n\n" +
		"data: [DONE]\n\n"

	fr := newFlushRecorder()
	res := copySSE(fr, strings.NewReader(stream), nil, 0)

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	// 4 个帧 → 至少 4 次 Flush
	if res.Frames != 4 {
		t.Errorf("expected 4 frames, got %d", res.Frames)
	}
	if len(fr.flushes) < 4 {
		t.Fatalf("expected >= 4 flushes (one per frame), got %d", len(fr.flushes))
	}
	// 关键断言：Flush 点必须是递增的且第一次 Flush 远早于全部内容写完，
	// 证明数据是逐帧下发而非一次性攒完
	if fr.flushes[0] >= len(stream) {
		t.Errorf("first flush happened only after all data buffered (%d >= %d)",
			fr.flushes[0], len(stream))
	}
	for i := 1; i < len(fr.flushes); i++ {
		if fr.flushes[i] < fr.flushes[i-1] {
			t.Errorf("flush byte counts must be non-decreasing: %v", fr.flushes)
			break
		}
	}
	if got := fr.buf.String(); got != stream {
		t.Errorf("stream content altered.\nwant %q\ngot  %q", stream, got)
	}
}

// countingSink 记录旁路计量回调
type countingSink struct {
	mu    sync.Mutex
	data  [][]byte
	ticks int
	ended bool
}

func (c *countingSink) OnData(d []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(d))
	copy(cp, d)
	c.data = append(c.data, cp)
}
func (c *countingSink) OnFlushTick() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ticks++
}
func (c *countingSink) OnEnd() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
}

// TestSSESinkReceivesAllData 计量旁路必须收到每条 data 行且不影响透传
func TestSSESinkReceivesAllData(t *testing.T) {
	stream := "event: message\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		": this is a comment\n" +
		"data: {\"usage\":{\"total_tokens\":42}}\n\n" +
		"data: [DONE]\n\n"

	sink := &countingSink{}
	fr := newFlushRecorder()
	res := copySSE(fr, strings.NewReader(stream), sink, 0)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(sink.data) != 3 {
		t.Errorf("expected 3 data lines, got %d: %q", len(sink.data), sink.data)
	}
	if !sink.ended {
		t.Error("OnEnd must be called")
	}
	// 透传内容必须逐字节一致，包括 event/comment 行
	if fr.buf.String() != stream {
		t.Errorf("passthrough must be byte-exact")
	}
}

// TestSSEFlushTick 验证按间隔触发增量刷盘（Token 超卖窗口压缩的前提）
func TestSSEFlushTick(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 5; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	sink := &countingSink{}
	fr := newFlushRecorder()
	// 用极小间隔配合 slowReader 保证至少触发一次
	copySSE(fr, &slowReader{r: strings.NewReader(sb.String()), delay: 3 * time.Millisecond}, sink, time.Millisecond)
	if sink.ticks == 0 {
		t.Error("expected at least one flush tick")
	}
}

type slowReader struct {
	r     io.Reader
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	time.Sleep(s.delay)
	if len(p) > 8 {
		p = p[:8] // 强制小块读，模拟真实网络分片
	}
	return s.r.Read(p)
}

// TestSSEClientDisconnect 客户端中途断开必须干净终止且调用 OnEnd（保证记账不丢）
func TestSSEClientDisconnect(t *testing.T) {
	stream := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 100)
	sink := &countingSink{}
	res := copySSE(&brokenWriter{failAfter: 100}, strings.NewReader(stream), sink, 0)
	if res.Err == nil {
		t.Error("expected write error to be surfaced")
	}
	if !sink.ended {
		t.Error("OnEnd must be called even when the client disconnects, otherwise tokens leak unsettled")
	}
}

type brokenWriter struct {
	written   int
	failAfter int
	header    http.Header
}

func (b *brokenWriter) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}
func (b *brokenWriter) WriteHeader(int) {}
func (b *brokenWriter) Write(p []byte) (int, error) {
	b.written += len(p)
	if b.written > b.failAfter {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func TestIsSSE(t *testing.T) {
	for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8"} {
		h := http.Header{}
		h.Set("Content-Type", ct)
		if !isSSE(h) {
			t.Errorf("%q should be detected as SSE", ct)
		}
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if isSSE(h) {
		t.Error("json must not be detected as SSE")
	}
}

func TestStripDataPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"data: {\"a\":1}\n", `{"a":1}`, true},
		{"data:{\"a\":1}\r\n", `{"a":1}`, true},
		{"event: ping\n", "", false},
		{": comment\n", "", false},
		{"\n", "", false},
	}
	for _, tc := range cases {
		got, ok := stripDataPrefix([]byte(tc.in))
		if ok != tc.ok || string(got) != tc.want {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

var _ = httptest.NewRequest
