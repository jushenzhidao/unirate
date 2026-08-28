package proxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unirate/gateway/internal/config"
	"github.com/unirate/gateway/internal/limiter"
	"github.com/unirate/gateway/internal/meta"
	"github.com/unirate/gateway/internal/meter"
	"github.com/unirate/gateway/internal/obs"
	"github.com/unirate/gateway/internal/upstream"
)

// 逐跳头（RFC 7230 §6.1）不得转发给上游
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Options 代理配置
type Options struct {
	MetaConfig      meta.Config
	UpstreamTimeout time.Duration
	SSEIdleTimeout  time.Duration
	TokenFlushEvery time.Duration
	MaxRequestBody  int64
	Instances       int
	ExposeRuleName  bool // 429 响应是否暴露规则名（内网可开，外网建议关）
}

// DefaultOptions 默认配置
func DefaultOptions() Options {
	return Options{
		MetaConfig:      meta.DefaultConfig(),
		UpstreamTimeout: 60 * time.Second,
		SSEIdleTimeout:  300 * time.Second,
		TokenFlushEvery: time.Second,
		MaxRequestBody:  32 << 20, // 32MB
		ExposeRuleName:  true,
	}
}

// Handler 网关代理处理器
type Handler struct {
	lim *limiter.Limiter
	cfg *config.Store
	res *upstream.Resolver
	met *obs.Metrics
	log *slog.Logger
	// opt 无锁热替换。
	//
	// 原先是值类型、New() 时固化，导致 Tier 1 运行策略（超时/体积上限/
	// 刷盘间隔/规则名暴露）改动必须重启才生效。改为 atomic.Pointer 后可整份替换。
	// 复用 config.Store 已在用的同一模式（cur atomic.Pointer[Snapshot]），
	// 不另造机制。
	//
	// 关键：整份替换而非逐字段写。逐字段写会让单个请求读到
	// "新超时 + 旧体积上限" 的混合状态；整份替换保证每个请求看到的是
	// 某一时刻的一致配置。
	opt       atomic.Pointer[Options]
	client    *http.Client
	sseClient *http.Client
}

// Options 返回当前生效配置（无锁读）
func (h *Handler) Options() Options { return *h.opt.Load() }

// SetOptions 整份替换运行配置。MetaConfig 由调用方保证一致性 ——
// 它属 Tier 0（影响 IP/token 维度提取，见 CONFIG-TIERING.md「明确不搬」），
// 热更新路径不会改它。
func (h *Handler) SetOptions(opt Options) { h.opt.Store(&opt) }

// ApplyPolicy 把 Tier 1 策略投射到代理配置上。
//
// 只覆盖 Tier 1 涉及的字段，其余（MetaConfig / SSEIdleTimeout）保持原值 ——
// 那些是 Tier 0 或未纳入热更新范围的项，覆盖它们会造成静默的语义变化。
func (h *Handler) ApplyPolicy(p *config.Policy) {
	if p == nil {
		return
	}
	cur := *h.opt.Load()
	cur.UpstreamTimeout = p.UpstreamTimeout.D()
	cur.TokenFlushEvery = p.TokenFlushInterval.D()
	cur.MaxRequestBody = p.MaxRequestBodyBytes()
	cur.ExposeRuleName = p.ExposeRuleName
	cur.Instances = p.Instances
	h.opt.Store(&cur)
}

// New 创建处理器
func New(lim *limiter.Limiter, cfg *config.Store, res *upstream.Resolver,
	met *obs.Metrics, log *slog.Logger, opt Options) *Handler {

	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	newTransport := func() *http.Transport {
		return &http.Transport{
			DialContext:           dialer.DialContext,
			MaxIdleConns:          512,
			MaxIdleConnsPerHost:   128,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			// 关键：SSE 必须禁用响应压缩与响应缓冲，否则流式会被攒包
			DisableCompression:    false,
			ResponseHeaderTimeout: 60 * time.Second,
			ForceAttemptHTTP2:     true,
		}
	}

	sseTr := newTransport()
	// SSE 场景禁用压缩：gzip 会在中间层攒够一个块才输出，破坏实时性
	sseTr.DisableCompression = true
	sseTr.ResponseHeaderTimeout = 120 * time.Second

	h := &Handler{
		lim: lim, cfg: cfg, res: res, met: met, log: log,
		client:    &http.Client{Transport: newTransport()},
		sseClient: &http.Client{Transport: sseTr}, // 流式无整体超时，靠 ctx 与 idle 控制
	}
	h.opt.Store(&opt)
	return h
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// ServeHTTP 主流程
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newRequestID()
	}
	w.Header().Set("X-Request-Id", reqID)

	// 每个请求只读一次配置，全程沿用这一份。
	// 若各处分别 Load()，一个请求可能前半段用旧超时、后半段用新体积上限，
	// 出问题时无法还原当时到底是什么配置在生效。
	opt := h.opt.Load()

	rm, err := opt.MetaConfig.Extract(r)
	if err != nil {
		h.writeError(w, r, http.StatusBadRequest, "invalid_path",
			"URL must be /{biz}/{upstream_path}", reqID)
		h.met.ReqTotal.Inc("_", "bad_request", "400")
		return
	}

	m := &limiter.Meta{
		Biz:       rm.Biz,
		Path:      rm.Path,
		TokenHash: limiter.HashToken(rm.RawToken),
		IP:        rm.IP,
		Method:    rm.Method,
		RequestID: reqID,
	}

	target, err := h.res.Resolve(rm.Biz, r.Header.Get(h.res.HeaderName()))
	if err != nil {
		code, kind := http.StatusBadGateway, "no_upstream"
		if errors.Is(err, upstream.ErrUpstreamBlocked) {
			code, kind = http.StatusForbidden, "upstream_blocked"
		}
		h.writeError(w, r, code, kind, err.Error(), reqID)
		h.met.ReqTotal.Inc(rm.Biz, kind, strconv.Itoa(code))
		h.met.UpstreamErr.Inc(rm.Biz, kind)
		return
	}

	rules := h.cfg.Rules(rm.Biz)

	// 准入判定：Token 预算 + 请求维度限流 + 并发占位，**单次 Redis 往返原子完成**。
	//
	// ADR-003：原先此处是两次串行往返（先 TokenAdmit 再 Check），
	// 且 TokenAdmit 对每条 token 规则各发一次。Token 预算准入已并入
	// batch_check.lua 的 Phase 1（只读检查余额，Phase 2 对其不写入），
	// 语义与原 TokenAdmit 逐位一致，但省掉一次 RTT。
	d, hold, err := h.lim.Check(r.Context(), rules, m, 0)
	if err != nil {
		h.log.Error("limiter check failed", "err", err, "req_id", reqID)
		h.met.RedisErrors.Inc("check")
	}
	if !d.Allowed {
		h.reject(w, r, opt, d, rm.Biz, reqID, start)
		return
	}
	if hold != nil {
		h.met.ConcInFlight.Add(1, rm.Biz)
		// 无条件释放：panic / 客户端断开 / 上游超时 都必须归还额度，
		// 否则并发计数器会永久泄漏（评审 P0-4）
		defer func() {
			rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if rerr := h.lim.Release(rctx, hold); rerr != nil {
				h.log.Error("release concurrency failed", "err", rerr, "req_id", reqID)
				h.met.RedisErrors.Inc("release")
			}
			cancel()
			h.met.ConcInFlight.Add(-1, rm.Biz)
		}()
	}
	if d.Degraded {
		h.met.Degraded.Inc(rm.Biz, "allow")
	}

	h.forward(w, r, opt, m, rm, target, rules, reqID, start)
}

// forward 转发到上游。
// opt 由 ServeHTTP 一次性读取后透传，保证单请求内配置一致。
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, opt *Options,
	m *limiter.Meta, rm *meta.RequestMeta, target *upstream.Target,
	rules []*limiter.Rule, reqID string, start time.Time) {

	upath := rm.Path
	if target.StripPathPrefix {
		if _, rest, err := meta.ExtractBiz(rm.Path); err == nil {
			upath = rest
		}
	}
	uurl := target.BaseURL + upath
	if r.URL.RawQuery != "" {
		uurl += "?" + r.URL.RawQuery
	}

	var body io.Reader = r.Body
	if opt.MaxRequestBody > 0 {
		body = http.MaxBytesReader(w, r.Body, opt.MaxRequestBody)
	}

	// 流式请求不设整体超时（SSE 可能持续数分钟），非流式用固定超时
	ctx := r.Context()
	var cancel context.CancelFunc
	wantSSE := clientWantsStream(r)
	if !wantSSE && opt.UpstreamTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, opt.UpstreamTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, uurl, body)
	if err != nil {
		h.writeError(w, r, http.StatusBadGateway, "bad_upstream_url", err.Error(), reqID)
		h.met.UpstreamErr.Inc(rm.Biz, "bad_url")
		return
	}
	copyHeaders(req.Header, r.Header)
	for _, hh := range hopHeaders {
		req.Header.Del(hh)
	}
	req.Header.Del(h.res.HeaderName())
	req.Header.Set("X-Request-Id", reqID)
	appendVia(req.Header, r)
	req.Host = ""
	if r.ContentLength > 0 {
		req.ContentLength = r.ContentLength
	}

	client := h.client
	if wantSSE {
		client = h.sseClient
	}

	upstreamStart := time.Now()
	resp, err := client.Do(req)
	// 上游往返延迟与端到端延迟分开度量。两者之差即为网关自身开销
	// （元数据提取 + 准入判定 + Redis 往返），是判断"慢在谁"的唯一依据。
	// 流式请求这里只计到响应头，流体时长由 StreamDuration 单独承载。
	h.met.UpstreamLat.Observe(time.Since(upstreamStart).Seconds(), rm.Biz)
	if err != nil {
		kind := "upstream_error"
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			kind, code = "upstream_timeout", http.StatusGatewayTimeout
		case errors.Is(err, context.Canceled):
			// 客户端主动断开，不是上游故障
			h.met.ReqTotal.Inc(rm.Biz, "client_closed", "499")
			return
		}
		h.writeError(w, r, code, kind, err.Error(), reqID)
		h.met.UpstreamErr.Inc(rm.Biz, kind)
		h.met.ReqTotal.Inc(rm.Biz, kind, strconv.Itoa(code))
		return
	}
	defer resp.Body.Close()

	mc := h.cfg.Metering(rm.Biz)
	hasTokenRule := hasTokenMetric(rules)

	copyHeaders(w.Header(), resp.Header)
	for _, hh := range hopHeaders {
		w.Header().Del(hh)
	}
	w.Header().Set("X-Request-Id", reqID)

	if isSSE(resp.Header) {
		// SSE 必须禁止任何中间层缓冲
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)

		h.met.SSEStreams.Add(1, rm.Biz)
		streamStart := time.Now()
		sink := h.newSSESink(rm.Biz, m, rules, mc, hasTokenRule)
		res := copySSE(w, resp.Body, sink, opt.TokenFlushEvery)
		h.met.SSEStreams.Add(-1, rm.Biz)

		// TTFT 零值表示该流没有产出任何 data 行（上游立即失败或仅发心跳）。
		// 上报 0 会把直方图 P50 拉向零，掩盖真实首字延迟，故必须跳过。
		if res.TTFT > 0 {
			h.met.TTFT.Observe(res.TTFT.Seconds(), rm.Biz)
		}
		h.met.StreamDuration.Observe(time.Since(streamStart).Seconds(), rm.Biz)
		if res.Frames > 0 {
			h.met.SSEFrames.Add(res.Frames, rm.Biz)
		}

		if res.Err != nil {
			h.log.Warn("sse stream ended with error",
				"err", res.Err, "req_id", reqID, "bytes", res.Bytes, "frames", res.Frames)
		}
		h.observe(rm.Biz, "stream", resp.StatusCode, start)
		return
	}

	// 非流式：仅在需要 token 计量时读入内存，否则直接零拷贝透传
	if hasTokenRule && mc.Mode != "disabled" && isJSON(resp.Header) {
		h.forwardJSONWithMetering(w, resp, m, rm, rules, mc, reqID, start)
		return
	}

	w.WriteHeader(resp.StatusCode)
	n, _ := io.Copy(w, resp.Body)
	_ = n
	h.observe(rm.Biz, "pass", resp.StatusCode, start)
}

// forwardJSONWithMetering 非流式 JSON 响应：提取 usage 并记账
func (h *Handler) forwardJSONWithMetering(w http.ResponseWriter, resp *http.Response,
	m *limiter.Meta, rm *meta.RequestMeta, rules []*limiter.Rule,
	mc *config.TokenMetering, reqID string, start time.Time) {

	// 限制读取上限，防止超大响应打爆内存
	const maxMeterBody = 8 << 20
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxMeterBody))
	if err != nil {
		h.log.Warn("read upstream body failed", "err", err, "req_id", reqID)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(buf)
	// 若响应被截断，剩余部分继续透传（不参与计量）
	if len(buf) == maxMeterBody {
		_, _ = io.Copy(w, resp.Body)
	}

	var used int64
	var ok bool
	if mc.Mode == "header" || mc.Mode == "auto" {
		if v, hit := meter.ParseHeaderUsage(resp.Header.Get(mc.HeaderName)); hit {
			used, ok = v, true
		}
	}
	if !ok && (mc.Mode == "json_body" || mc.Mode == "auto") {
		var bd meter.UsageBreakdown
		if bd, ok = meter.ExtractUsageBreakdown(buf); ok {
			used = bd.Total
			h.countTokenKinds(rm.Biz, bd)
		}
	}
	if !ok && mc.Mode != "disabled" {
		// 上游未返回用量，退化为估算（带安全缓冲）
		used = meter.EstimateTokens(string(buf), mc.EstimateRatio)
		if mc.SafetyBuffer > 1 {
			used = int64(float64(used) * mc.SafetyBuffer)
		}
	}

	if used > 0 {
		bctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := h.lim.TokenReserve(bctx, rules, m, used); err != nil {
			h.met.RedisErrors.Inc("token_reserve")
		}
		cancel()
		h.countTokens(rm.Biz, used)
	}
	h.observe(rm.Biz, "pass", resp.StatusCode, start)
}

// sseSink 实现 FrameSink，负责 SSE 期间的 token 增量记账
type sseSink struct {
	h     *Handler
	biz   string
	m     *limiter.Meta
	rules []*limiter.Rule
	mc    *config.TokenMetering
	on    bool

	mu sync.Mutex
	c  meter.Counter
}

func (h *Handler) newSSESink(biz string, m *limiter.Meta,
	rules []*limiter.Rule, mc *config.TokenMetering, on bool) FrameSink {
	if !on || mc.Mode == "disabled" {
		return nil
	}
	return &sseSink{h: h, biz: biz, m: m, rules: rules, mc: mc, on: on}
}

func (s *sseSink) OnData(data []byte) {
	content, usage, hasUsage := meter.ParseSSEData(data)
	if hasUsage {
		s.c.SetExact(usage)
		// SSE 是 LLM 流量的主形态，分项仅在 usage 帧出现一次；
		// 漏掉这里会让 tokens_by_kind 在流式场景永久为空。
		if bd, ok := meter.ExtractUsageBreakdown(data); ok {
			s.h.countTokenKinds(s.biz, bd)
		}
	}
	if content != "" {
		s.c.AddEstimate(meter.EstimateTokens(content, s.mc.EstimateRatio))
	}
}

// OnFlushTick 增量刷盘。把超卖窗口从「整个 SSE 时长」压缩到刷盘间隔（默认 1s）。
func (s *sseSink) OnFlushTick() {
	s.mu.Lock()
	delta := s.c.PendingFlush(s.mc.SafetyBuffer)
	s.mu.Unlock()
	if delta <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.h.lim.TokenReserve(ctx, s.rules, s.m, delta); err != nil {
		s.h.met.RedisErrors.Inc("token_reserve")
		return
	}
	s.h.countTokens(s.biz, delta)
}

// OnEnd 流结束：用上游精确 usage 核销，退回多预扣的部分（评审 P0-6）
func (s *sseSink) OnEnd() {
	s.mu.Lock()
	reserved := s.c.Flushed()
	// 先把尾部未刷的增量补上，保证 reserved 覆盖全部估算量
	if tail := s.c.PendingFlush(s.mc.SafetyBuffer); tail > 0 {
		reserved += tail
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.h.lim.TokenReserve(ctx, s.rules, s.m, tail)
		cancel()
		s.h.countTokens(s.biz, tail)
	}
	actual := s.c.Final(s.mc.SafetyBuffer)
	hasExact := s.c.HasExact()
	s.mu.Unlock()

	if !hasExact || reserved == actual {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.h.lim.TokenSettle(ctx, s.rules, s.m, reserved, actual); err != nil {
		s.h.met.RedisErrors.Inc("token_settle")
		return
	}
	s.h.met.TokenSettled.Add(actual, s.biz)
}

// reject 返回 429（Spec §4.3）
func (h *Handler) reject(w http.ResponseWriter, r *http.Request, opt *Options,
	d limiter.Decision, biz, reqID string, start time.Time) {

	retry := d.RetryAfter
	if retry < 0 {
		retry = 0
	}
	secs := int(retry.Seconds())
	if retry > 0 && secs == 0 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(retry).Unix(), 10))
	if opt.ExposeRuleName {
		w.Header().Set("X-RateLimit-Rule", d.RuleName)
	}
	if d.Degraded {
		w.Header().Set("X-RateLimit-Degraded", "1")
		h.met.Degraded.Inc(biz, "reject")
	}

	msg := "rate limit exceeded"
	if opt.ExposeRuleName {
		msg = fmt.Sprintf("rate limit exceeded: rule %q on dimension %q", d.RuleName, d.Dimension)
	}
	h.writeError(w, r, http.StatusTooManyRequests, "rate_limited", msg, reqID)

	h.met.Rejected.Inc(biz, d.RuleName, d.Dimension)
	h.observe(biz, "reject", http.StatusTooManyRequests, start)
}

type errBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	} `json:"error"`
}

func (h *Handler) writeError(w http.ResponseWriter, r *http.Request,
	status int, code, msg, reqID string) {
	var b errBody
	b.Error.Code = code
	b.Error.Message = msg
	b.Error.RequestID = reqID
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&b)
}

func (h *Handler) observe(biz, decision string, code int, start time.Time) {
	h.met.ReqTotal.Inc(biz, decision, strconv.Itoa(code))
	h.met.Latency.Observe(time.Since(start).Seconds(), biz, decision)
	// RPM 走滚动窗口，让控制台首屏即有速率值、且不受进程重启影响。
	// 这里是所有请求路径的统一收口，放在别处会漏计。
	h.met.RPM.Inc(biz)
}

// countTokens 统一的 token 观测收口。
//
// 单独提取而非在各调用点重复三行，是因为 TokenConsumed / TPM / TokensByKind
// 必须同步更新 —— 漏掉任一处都会让看板上的用量与配额核对结果对不上，
// 而这种不一致极难从现场反推。
func (h *Handler) countTokens(biz string, n int64) {
	if n <= 0 {
		return
	}
	h.met.TokenConsumed.Add(n, biz)
	h.met.TPM.Add(n, biz)
}

// countTokenKinds 记录分方向用量。仅在上游给出可信拆解时调用 ——
// 用估算值伪造分项会让「补全占比」这类派生指标变成噪声。
func (h *Handler) countTokenKinds(biz string, u meter.UsageBreakdown) {
	if !u.Split {
		return
	}
	if u.Prompt > 0 {
		h.met.TokensByKind.Add(u.Prompt, biz, "prompt")
	}
	if u.Completion > 0 {
		h.met.TokensByKind.Add(u.Completion, biz, "completion")
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func appendVia(dst http.Header, r *http.Request) {
	proto := "1.1"
	if r.ProtoMajor == 2 {
		proto = "2.0"
	}
	v := proto + " unirate"
	if old := dst.Get("Via"); old != "" {
		v = old + ", " + v
	}
	dst.Set("Via", v)
}

func clientWantsStream(r *http.Request) bool {
	// OpenAI 兼容协议用 body 里的 "stream": true 表达，但读 body 会破坏透传，
	// 因此只做保守判断：由 Accept 头或上游响应 Content-Type 决定。
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}

func isJSON(h http.Header) bool {
	return strings.Contains(h.Get("Content-Type"), "json")
}

func hasTokenMetric(rules []*limiter.Rule) bool {
	for _, r := range rules {
		if r.IsEnabled() && r.Type == limiter.TypeRate && r.Metric == limiter.MetricToken {
			return true
		}
	}
	return false
}
