package main

import (
	"fmt"
	"time"
)

// TTFBBaselineMS 是 ADR-005 记录的 SSE 首字节基线（5ms）。
// 场景 C 的硬闸门：TTFB 高于此值即说明引入了攒包，必须回退。
const TTFBBaselineMS = 5.0

// report 完整报告，同时作为 baseline JSON 的结构
type report struct {
	Meta    reportMeta     `json:"meta"`
	Env     envDeclaration `json:"environment_declaration"`
	Rounds  []roundSummary `json:"rounds"`
	Median  []medianEntry  `json:"median"`
	CPU     cpuSample      `json:"container_cpu"`
	Attrib  string         `json:"bottleneck_attribution"`
	Verdict verdict        `json:"verdict"`
}

type reportMeta struct {
	Scenario  string    `json:"scenario"`
	Label     string    `json:"label,omitempty"`
	Concs     []int     `json:"concurrency_levels"`
	Rounds    int       `json:"rounds"`
	Duration  string    `json:"duration_per_round"`
	Warmup    string    `json:"warmup"`
	StartedAt time.Time `json:"started_at"`
	Client    string    `json:"client_runtime"`
}

// envDeclaration 环境声明 —— 必须出现在报告头部。
// 本机是开发机而非专用压测环境，绝对数字不可作生产参考，这一点必须写死在产物里，
// 避免后续有人拿这些数字去做容量承诺。
type envDeclaration struct {
	Host            string   `json:"host"`
	ClientColocated bool     `json:"client_colocated_with_target"`
	Caveats         []string `json:"caveats"`
}

type roundSummary struct {
	Round        int              `json:"round"`
	Conc         int              `json:"concurrency"`
	QPS          float64          `json:"qps"`
	Latency      map[string]any   `json:"latency"`
	TTFB         map[string]any   `json:"ttfb,omitempty"`
	Codes        map[string]int64 `json:"status_codes"`
	Errors       int64            `json:"transport_errors"`
	Passed       int64            `json:"passed,omitempty"`
	Rejected     int64            `json:"rejected,omitempty"`
	Frames       int64            `json:"sse_frames,omitempty"`
	BreakerOpen  int64            `json:"redis_breaker_open"`
	ConcInFlight float64          `json:"concurrency_in_flight_after"`
	SSEActive    float64          `json:"sse_streams_active_after"`
	EvalshaUsec  float64          `json:"redis_evalsha_usec_per_call"`
	EvalshaCalls int64            `json:"redis_evalsha_calls"`
	RedisCPUSec  float64          `json:"redis_cpu_seconds_consumed"`
	RedisCores   float64          `json:"redis_cores_used"`
}

type medianEntry struct {
	Conc       int     `json:"concurrency"`
	QPSMedian  float64 `json:"qps_median"`
	QPSSpread  float64 `json:"qps_spread_percent"`
	P50Median  string  `json:"p50_median"`
	P99Median  string  `json:"p99_median"`
	TTFBMedian string  `json:"ttfb_p99_median,omitempty"`
	RedisCores float64 `json:"redis_cores_median"`
}

type verdict struct {
	Pass   bool    `json:"pass"`
	Checks []check `json:"checks"`
}

type check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// run 执行完整的多轮压测
func (h *harness) run() (report, error) {
	rep := report{
		Meta: reportMeta{
			Scenario: h.cfg.Scenario, Label: h.cfg.Label,
			Concs: h.cfg.Concs, Rounds: h.cfg.Rounds,
			Duration: h.cfg.Duration.String(), Warmup: h.cfg.Warmup.String(),
			StartedAt: time.Now(), Client: hostEnvNote(),
		},
		Env: envDeclaration{
			Host:            "Docker Desktop 5 核 / 8GB（开发机，非专用压测环境）",
			ClientColocated: true,
			Caveats: []string{
				"压测客户端与被测服务运行在同一宿主，争抢同一批 CPU",
				"宿主同时运行多个无关容器，存在邻居噪声",
				"绝对数字不可作生产容量参考；仅用于优化前后同环境对比",
				"归因结论必须依据 container_cpu 字段，缺失时结论不成立",
			},
		},
	}

	h.printHeader(rep)

	// 吞吐类场景须用高配额专属业务域，否则测到的是 429 拒绝路径
	if h.cfg.Scenario == "A" || h.cfg.Scenario == "C" || h.cfg.Scenario == "D" {
		fmt.Printf("[准备] 创建/更新吞吐业务域 %s（与 demo 同构规则集，配额极高使限流不触发）...\n",
			perfBizThroughput)
		if err := h.ensureThroughputBiz(); err != nil {
			h.restoreGlobalGuard() // 中途失败也要还原，不留破坏
			return rep, err
		}
		// 无论后续成功或失败，都必须还原被临时抬高的全局兜底规则
		defer h.restoreGlobalGuard()
		fmt.Println("[准备] 业务域已生效")
	}

	// 场景 B 依赖专属单规则业务域，须先建好（详见 provision.go）
	if h.cfg.Scenario == "B" {
		fmt.Printf("[准备] 创建/更新专属业务域 %s（单条 fixed_window 规则 limit=%d）...\n",
			perfBizB, h.cfg.Limit)
		if err := h.ensureScenarioBBiz(h.cfg.Limit); err != nil {
			return rep, err
		}
		fmt.Println("[准备] 业务域已生效")
	}

	for _, conc := range h.cfg.Concs {
		fmt.Printf("\n===== 场景 %s / 并发 %d =====\n", h.cfg.Scenario, conc)

		// 预热：ADR-008 §四.3 强制项。EVALSHA 需已加载、连接池需达稳态。
		if h.cfg.Warmup > 0 {
			fmt.Printf("[预热] %v（数据丢弃）...\n", h.cfg.Warmup)
			if err := h.resetState(); err != nil {
				return rep, fmt.Errorf("预热前重置失败: %w", err)
			}
			if _, err := h.oneRound(conc, h.cfg.Warmup); err != nil {
				return rep, fmt.Errorf("预热失败: %w", err)
			}
		}

		for r := 1; r <= h.cfg.Rounds; r++ {
			if err := h.resetState(); err != nil {
				return rep, fmt.Errorf("第 %d 轮重置失败: %w", r, err)
			}
			wFrom := time.Now().Unix()
			rr, err := h.oneRound(conc, h.cfg.Duration)
			if err != nil {
				return rep, fmt.Errorf("第 %d 轮失败: %w", r, err)
			}
			// 记录正式采集窗口，供容器 CPU 样本过滤（剔除预热与重置期）。
			// 放宽 2s：docker stats 采样间隔约 1-2s，窗口取严会让短轮次
			// （尤其场景 B 的瞬时齐发，耗时仅数十毫秒）一条样本都取不到。
			h.windows = append(h.windows,
				window{from: wFrom - 2, to: time.Now().Unix() + 2})
			rs := summarize(r, conc, rr)
			rep.Rounds = append(rep.Rounds, rs)
			fmt.Printf("[轮 %d/%d] QPS=%-9.0f %v breaker=%d in_flight=%.0f evalsha=%.2fµs redis=%.3f核\n",
				r, h.cfg.Rounds, rs.QPS, rr.Lat, rs.BreakerOpen,
				rs.ConcInFlight, rs.EvalshaUsec, rs.RedisCores)
			if h.cfg.Scenario == "B" {
				fmt.Printf("        通过=%d 拒绝=%d（期望通过恰好 %d）\n",
					rr.Passed, rr.Rejected, h.cfg.Limit)
			}
			if h.cfg.Scenario == "C" {
				fmt.Printf("        TTFB %v\n", rr.TTFB)
			}
		}
	}

	rep.Median = computeMedians(rep.Rounds, h.cfg.Scenario)
	rep.CPU = readCPUSamples(h.cfg.CPUFile, h.windows)
	rep.Attrib = attribute(rep.CPU)
	rep.Verdict = h.evaluate(rep)
	return rep, nil
}

func (h *harness) oneRound(conc int, dur time.Duration) (roundResult, error) {
	switch h.cfg.Scenario {
	case "A":
		return h.scenarioA(conc, dur)
	case "B":
		return h.scenarioB(conc, h.cfg.Limit)
	case "C":
		return h.scenarioC(conc, h.cfg.Chunks, h.cfg.DelayMS, dur)
	case "D":
		return h.scenarioD(conc, dur)
	default:
		return roundResult{}, fmt.Errorf("未知场景 %q（可选 A|B|C|D）", h.cfg.Scenario)
	}
}

func summarize(round, conc int, rr roundResult) roundSummary {
	codes := map[string]int64{}
	for k, v := range rr.Codes {
		codes[fmt.Sprintf("%d", k)] = v
	}
	cpuDelta := rr.RedisAfter.cpuTotal() - rr.RedisBefore.cpuTotal()
	cores := 0.0
	if rr.Duration > 0 {
		cores = round3(cpuDelta / rr.Duration.Seconds())
	}
	rs := roundSummary{
		Round: round, Conc: conc, QPS: rr.QPS,
		Latency: rr.Lat.forJSON(),
		Codes:   codes, Errors: rr.Errors,
		Passed: rr.Passed, Rejected: rr.Rejected, Frames: rr.Frames,
		BreakerOpen:  rr.MetricsAfter.BreakerOpen,
		ConcInFlight: rr.MetricsAfter.ConcInFlight,
		SSEActive:    rr.MetricsAfter.SSEActive,
		EvalshaUsec:  rr.RedisAfter.EvalshaUsecAvg,
		EvalshaCalls: rr.RedisAfter.EvalshaCalls,
		RedisCPUSec:  round3(cpuDelta),
		RedisCores:   cores,
	}
	if rr.TTFB.N > 0 {
		rs.TTFB = rr.TTFB.forJSON()
	}
	return rs
}

func computeMedians(rounds []roundSummary, scenario string) []medianEntry {
	byConc := map[int][]roundSummary{}
	var order []int
	for _, r := range rounds {
		if _, ok := byConc[r.Conc]; !ok {
			order = append(order, r.Conc)
		}
		byConc[r.Conc] = append(byConc[r.Conc], r)
	}
	var out []medianEntry
	for _, c := range order {
		rs := byConc[c]
		var qps, cores []float64
		var p50s, p99s, ttfbs []time.Duration
		for _, r := range rs {
			qps = append(qps, r.QPS)
			cores = append(cores, r.RedisCores)
			p50s = append(p50s, msToDur(r.Latency["p50_ms"]))
			p99s = append(p99s, msToDur(r.Latency["p99_ms"]))
			if r.TTFB != nil {
				ttfbs = append(ttfbs, msToDur(r.TTFB["p99_ms"]))
			}
		}
		me := medianEntry{
			Conc: c, QPSMedian: round2(medianOf(qps)), QPSSpread: spread(qps),
			P50Median: medianDur(p50s).String(), P99Median: medianDur(p99s).String(),
			RedisCores: round3(medianOf(cores)),
		}
		if len(ttfbs) > 0 {
			me.TTFBMedian = medianDur(ttfbs).String()
		}
		out = append(out, me)
	}
	return out
}

func msToDur(v any) time.Duration {
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return time.Duration(f * float64(time.Millisecond))
}

func orElse(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}
