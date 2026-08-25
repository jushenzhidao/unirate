package obs

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 指标集合与文本渲染。
//
// 从 metrics.go 拆出：原文件 327 行，同时承担「三种指标类型的实现」与
// 「网关指标集合定义 + 暴露」两件事。前者是通用容器代码，后者是业务指标清单，
// 变更频率与变更原因都不同。拆分后 metrics.go 只剩容器，本文件只剩清单与渲染。
//
// Render 从 Handler 里提取为独立方法，是为了让 admin 端口的受鉴权指标端点
// 能直读同一份内存结构，而不必 HTTP 自调 obs 端口。
// 详见 internal/admin/metrics.go 的数据源说明。

// LatencyBounds 延迟直方图的桶边界（秒）。
//
// 导出是因为它是**消费方计算分位数的精度上限**：服务端与前端做线性插值时，
// 结果误差不会小于所落桶的宽度。任何展示 P99 的地方都必须能拿到这份边界，
// 否则给出的数字看起来精确却无法说明误差范围。
var LatencyBounds = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Metrics 网关全部指标
type Metrics struct {
	ReqTotal      *counterVec
	Rejected      *counterVec
	Degraded      *counterVec
	Latency       *histogramVec
	UpstreamErr   *counterVec
	TokenConsumed *counterVec
	TokenSettled  *counterVec
	ConcInFlight  *gaugeVec
	Watermark     *gaugeVec
	RedisErrors   *counterVec
	BreakerOpen   *gaugeVec
	ConfigVersion *gaugeVec
	SSEStreams    *gaugeVec

	startedAt time.Time
}

// NewMetrics 创建指标集合
func NewMetrics() *Metrics {
	return &Metrics{
		ReqTotal: newCounterVec("requests_total",
			"Total proxied requests.", "biz", "decision", "code"),
		Rejected: newCounterVec("rejected_total",
			"Requests rejected by rate limit rules.", "biz", "rule", "dimension"),
		Degraded: newCounterVec("degraded_decisions_total",
			"Decisions made while Redis was degraded.", "biz", "mode"),
		Latency: newHistogramVec("request_duration_seconds",
			"End-to-end request latency.", LatencyBounds, "biz", "decision"),
		UpstreamErr: newCounterVec("upstream_errors_total",
			"Upstream request failures.", "biz", "kind"),
		TokenConsumed: newCounterVec("tokens_reserved_total",
			"Tokens reserved (pre-charged) into the ledger.", "biz"),
		TokenSettled: newCounterVec("tokens_settled_total",
			"Tokens settled against upstream exact usage.", "biz"),
		ConcInFlight: newGaugeVec("concurrency_in_flight",
			"In-flight requests holding a concurrency slot.", "biz"),
		Watermark: newGaugeVec("rule_watermark_ratio_percent",
			"Latest observed usage ratio per rule.", "biz", "rule"),
		RedisErrors: newCounterVec("redis_errors_total",
			"Redis operation failures.", "op"),
		BreakerOpen: newGaugeVec("redis_breaker_open",
			"Whether the Redis circuit breaker is open (1) or closed (0)."),
		ConfigVersion: newGaugeVec("config_version",
			"Currently loaded config version."),
		SSEStreams: newGaugeVec("sse_streams_active",
			"Active server-sent-event streams being proxied.", "biz"),
		startedAt: time.Now(),
	}
}

// ContentType Prometheus 文本暴露格式的 MIME 类型
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Render 渲染一份完整的 Prometheus 文本快照。
//
// 并发安全：每个 vec 的 write 在自身 RWMutex 读锁内遍历 map，
// 值本身是 atomic.Int64 / atomic.Uint64，因此多个 goroutine 同时 Render、
// 或 Render 与请求路径的 Add/Set/Observe 并发都不会数据竞争。
// 唯一的弱一致性是**跨 vec 不是同一瞬间的原子快照** ——
// 例如 ReqTotal 已写完、Rejected 尚未渲染时又来一个被拒请求，
// 会导致同一份输出里两者相差一个计数。这对速率与比率的观测没有实际影响
// （误差 ≤ 单次采样间隔内的增量），也是 Prometheus 抓取本身固有的语义，
// 不值得为此引入全局锁去阻塞请求路径。
func (m *Metrics) Render() string {
	var sb strings.Builder
	sb.Grow(8192)
	m.ReqTotal.write(&sb)
	m.Rejected.write(&sb)
	m.Degraded.write(&sb)
	m.Latency.write(&sb)
	m.UpstreamErr.write(&sb)
	m.TokenConsumed.write(&sb)
	m.TokenSettled.write(&sb)
	m.ConcInFlight.write(&sb)
	m.Watermark.write(&sb)
	m.RedisErrors.write(&sb)
	m.BreakerOpen.write(&sb)
	m.ConfigVersion.write(&sb)
	m.SSEStreams.write(&sb)

	sb.WriteString("# HELP " + nsPrefix + "uptime_seconds Process uptime.\n")
	sb.WriteString("# TYPE " + nsPrefix + "uptime_seconds gauge\n")
	sb.WriteString(fmt.Sprintf("%suptime_seconds %.3f\n", nsPrefix, time.Since(m.startedAt).Seconds()))
	return sb.String()
}

// Handler 暴露 /metrics
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ContentType)
		_, _ = w.Write([]byte(m.Render()))
	})
}
