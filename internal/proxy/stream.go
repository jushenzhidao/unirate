package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"
)

// SSE 流式透传（对应 Spec §2.6 与 §4.2）
//
// 核心约束：零缓冲。任何形式的批量攒帧都会破坏流式体验，
// 而 Go 的 http.ResponseWriter 默认带缓冲，必须每帧显式 Flush。
//
// 同时需要在透传过程中旁路解析 token 用量 —— 关键点是
// 「先转发、后解析」：解析逻辑的任何耗时或 panic 都不能影响客户端收帧。

// FrameSink 旁路帧处理器
type FrameSink interface {
	// OnData 收到一条 SSE data 行（已剥离 "data: " 前缀）
	OnData(data []byte)
	// OnFlushTick 到达刷盘间隔
	OnFlushTick()
	// OnEnd 流结束
	OnEnd()
}

// sseCopyResult 流复制结果
type sseCopyResult struct {
	Bytes  int64
	Frames int64
	Err    error

	// TTFT 从 copySSE 开始到**首个 data 行**被转发的耗时。
	//
	// 取 data 行而非响应头或首个任意行，是因为只有 data 才承载模型输出：
	// 上游常先发 `event:`/`:ping` 心跳或 role 声明帧，以那些为基准会
	// 系统性低估 TTFT，得到一个好看但无意义的数字。
	//
	// 零值表示整个流没有 data 行（如上游立即报错），调用方须据此跳过上报，
	// 否则会把 0 混进直方图，把 P50 拉向 0。
	TTFT time.Duration
}

// isSSE 判断响应是否为事件流
func isSSE(h http.Header) bool {
	ct := h.Get("Content-Type")
	return len(ct) >= 17 && (ct[:17] == "text/event-stream" || bytes.HasPrefix([]byte(ct), []byte("text/event-stream")))
}

// copySSE 以零缓冲方式转发 SSE 并旁路计量。
//
// 实现要点：
//  1. 用 bufio.Reader 按行读上游（上游侧缓冲不影响下游及时性）；
//  2. 每读到一个完整帧立即写下游并 Flush；
//  3. data 行内容交给 sink 异步累加，sink 内部不得阻塞；
//  4. 按 flushInterval 触发增量刷盘（把 Token 超卖窗口从「整个 SSE 时长」压到秒级）。
func copySSE(w http.ResponseWriter, body io.Reader, sink FrameSink, flushInterval time.Duration) sseCopyResult {
	flusher, canFlush := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 16*1024)

	var res sseCopyResult
	streamStart := time.Now()
	lastFlush := streamStart

	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			n, werr := w.Write(line)
			res.Bytes += int64(n)
			if werr != nil {
				// 客户端断开：正常终止，不算上游错误
				res.Err = werr
				break
			}

			// SSE 帧以空行结束，此时必须 Flush 才能让客户端立即收到
			if canFlush && isFrameEnd(line) {
				flusher.Flush()
				res.Frames++
			}

			data, isData := stripDataPrefix(line)
			// TTFT 在 Write 之后取样，度量的是「客户端已可见首字」而非
			// 「网关已收到首字」—— 后者会漏掉下游写入本身的耗时。
			if isData && res.TTFT == 0 {
				res.TTFT = time.Since(streamStart)
			}

			if sink != nil {
				if isData {
					sink.OnData(data)
				}
				if flushInterval > 0 && time.Since(lastFlush) >= flushInterval {
					sink.OnFlushTick()
					lastFlush = time.Now()
				}
			}
		}

		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				// 单行超过缓冲区：继续读剩余部分，不丢数据
				continue
			}
			if !errors.Is(err, io.EOF) {
				res.Err = err
			}
			break
		}
	}

	if canFlush {
		flusher.Flush()
	}
	if sink != nil {
		sink.OnEnd()
	}
	return res
}

// isFrameEnd 判断该行是否为帧分隔空行
func isFrameEnd(line []byte) bool {
	t := bytes.TrimRight(line, "\r\n")
	return len(t) == 0
}

var dataPrefix = []byte("data:")

// stripDataPrefix 剥离 "data:" 前缀
func stripDataPrefix(line []byte) ([]byte, bool) {
	t := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(t, dataPrefix) {
		return nil, false
	}
	return bytes.TrimSpace(t[len(dataPrefix):]), true
}
