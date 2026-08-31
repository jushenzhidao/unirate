package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// mock 上游：用于 e2e 验收，模拟 OpenAI 兼容接口的关键行为。
//
// 覆盖场景：
//   /v1/chat/completions        非流式 JSON，返回 usage
//   /v1/chat/completions?stream=1  SSE 流式，末帧返回精确 usage
//   /slow?ms=N                  慢响应，验证上游超时
//   /status/{code}              指定状态码
//   /echo                       回显请求头，验证透传与逐跳头剥离
//   /hang                       长时间不响应，验证并发额度释放

var reqCount atomic.Int64

func main() {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":9000"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", chat)
	mux.HandleFunc("/slow", slow)
	mux.HandleFunc("/status/", status)
	mux.HandleFunc("/echo", echo)
	mux.HandleFunc("/hang", hang)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"served":%d}`, reqCount.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "path": r.URL.Path, "method": r.Method,
			"served": reqCount.Load(),
		})
	})

	// addr 来自 LISTEN_ADDR 环境变量（部署期输入），不来自任何请求数据；
	// 且本程序仅用于 e2e，不进生产镜像。
	// #nosec G706 -- addr 源自环境变量而非请求，无日志注入面
	log.Printf("mock upstream listening on %s", addr)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func chat(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)
	if r.URL.Query().Get("stream") == "1" || r.Header.Get("Accept") == "text/event-stream" {
		streamChat(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     "chatcmpl-mock",
		"object": "chat.completion",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": "hello from mock upstream"},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20,
		},
	})
}

// streamChat 逐帧输出并在末帧给出精确 usage，用于验证「预扣→核销退差」链路
func streamChat(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	n := 10
	if v := r.URL.Query().Get("chunks"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 && k <= 500 {
			n = k
		}
	}
	delay := 20 * time.Millisecond
	if v := r.URL.Query().Get("delay_ms"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 && k <= 2000 {
			delay = time.Duration(k) * time.Millisecond
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	for i := 0; i < n; i++ {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		frame := map[string]any{
			"id":      "chatcmpl-mock",
			"object":  "chat.completion.chunk",
			"choices": []map[string]any{{"index": 0, "delta": map[string]string{"content": "字块"}}},
		}
		b, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		time.Sleep(delay)
	}

	// 末帧携带精确 usage。故意让它明显小于「估算×1.2」，
	// 这样 e2e 可以观测到退差是否真的发生。
	final := map[string]any{
		"id": "chatcmpl-mock", "object": "chat.completion.chunk",
		"choices": []map[string]any{},
		"usage": map[string]int{
			"prompt_tokens": 5, "completion_tokens": n, "total_tokens": 5 + n,
		},
	}
	b, _ := json.Marshal(final)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func slow(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)
	ms := 1000
	if v := r.URL.Query().Get("ms"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k >= 0 {
			ms = k
		}
	}
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"slept_ms":%d}`, ms)
	case <-r.Context().Done():
	}
}

func status(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)
	code := 200
	if s := r.URL.Path[len("/status/"):]; s != "" {
		if k, err := strconv.Atoi(s); err == nil && k >= 100 && k < 600 {
			code = k
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"status":%d}`, code)
}

func echo(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)
	hdrs := map[string]string{}
	for k, v := range r.Header {
		hdrs[k] = v[0]
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path": r.URL.Path, "query": r.URL.RawQuery,
		"method": r.Method, "headers": hdrs,
	})
}

func hang(w http.ResponseWriter, r *http.Request) {
	reqCount.Add(1)
	<-r.Context().Done()
}
