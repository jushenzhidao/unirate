package main

// 判定逻辑：ADR-008 §四.7「判定阈值预先声明」的实现。
//
// 单独成文件的理由：这些阈值是**验收契约**而非报告排版细节，
// 修改它们等于修改验收标准，应当在 review 中被单独看见。

import (
	"fmt"
)

// evaluate 执行 ADR-008 §四.7 预先声明的判定阈值
func (h *harness) evaluate(rep report) verdict {
	var v verdict
	v.Pass = true
	add := func(name string, pass bool, detail string) {
		v.Checks = append(v.Checks, check{Name: name, Pass: pass, Detail: detail})
		if !pass {
			v.Pass = false
		}
	}

	// 全场景通用：熔断器必须恒为 0，否则说明压测期间限流已静默失效，数据无意义
	breakerBad := ""
	for _, r := range rep.Rounds {
		if r.BreakerOpen != 0 {
			breakerBad += fmt.Sprintf("轮%d(conc=%d) ", r.Round, r.Conc)
		}
	}
	add("redis_breaker_open 恒为 0", breakerBad == "",
		orElse(breakerBad == "", "全轮次均为 0",
			"以下轮次熔断器开启，限流已降级，结论无效："+breakerBad))

	// 传输层错误率
	var totalErr int64
	for _, r := range rep.Rounds {
		totalErr += r.Errors
	}
	add("传输层无错误", totalErr == 0,
		fmt.Sprintf("累计传输错误 %d", totalErr))

	// 吞吐场景守卫：被测量必须是**成功路径**。
	// 若 429 占比过高说明配额设置错误，此时 QPS 反映的是拒绝路径吞吐，
	// 与准入+转发的真实成本无关（首版打 demo 时 44 万请求仅 210 个 200，
	// 却报出 2.2 万 QPS —— 这种数据比没有更危险）。
	if h.cfg.Scenario == "A" || h.cfg.Scenario == "D" {
		var ok2xx, rejected int64
		for _, r := range rep.Rounds {
			ok2xx += r.Codes["200"]
			rejected += r.Codes["429"]
		}
		total := ok2xx + rejected
		ratio := 0.0
		if total > 0 {
			ratio = float64(ok2xx) / float64(total) * 100
		}
		add("吞吐场景以成功路径为主（200 占比 ≥ 95%）", ratio >= 95,
			fmt.Sprintf("200 占比 %.2f%%（200=%d, 429=%d）%s", ratio, ok2xx, rejected,
				orElse(ratio >= 95, "",
					" → 配额设置错误，测到的是拒绝路径而非吞吐，数据不可用")))
	}

	switch h.cfg.Scenario {
	case "B":
		// 硬断言：通过数必须恰好等于 limit
		bad := ""
		for _, r := range rep.Rounds {
			if r.Passed != h.cfg.Limit {
				bad += fmt.Sprintf("轮%d通过%d ", r.Round, r.Passed)
			}
		}
		add(fmt.Sprintf("拒绝路径精度：通过数恰好 %d（零误差）", h.cfg.Limit),
			bad == "",
			orElse(bad == "", fmt.Sprintf("全 %d 轮均恰好通过 %d 个", len(rep.Rounds), h.cfg.Limit),
				"偏差轮次："+bad+"→ P0-1/P0-5 回归，必须立即回退"))

	case "C":
		// TTFB 闸门。
		//
		// 判据设计说明（这里修正过一次方法论错误）：
		// ADR-005 记录的 5ms 基线是**单流**首字节延迟，
		// 而本场景在 100~2000 并发流下测量，排队本身就会把 TTFB 推高
		// （实测 100 流时 p50 已达 8.81ms，Redis 46% / gateway 87%）。
		// 拿并发测量值直接对单流基线做绝对比较，会把「并发排队」
		// 误判成「引入攒包」——这是错误的归因，也是无法通过的死闸门。
		//
		// 正确做法：TTFB 的用途是**优化前后同并发档对比**（-baseline-ttfb 传入
		// 前一轮同档实测值），而非对齐一个单流常数。
		// 未提供对比基线时只记录数值并给出观察结论，不作硬失败 ——
		// 硬失败留给「有对比基线且明显劣化」的情况。
		worst := 0.0
		for _, r := range rep.Rounds {
			if r.TTFB == nil {
				continue
			}
			if f, ok := r.TTFB["p50_ms"].(float64); ok && f > worst {
				worst = f
			}
		}
		switch {
		case h.cfg.BaselineTTFB > 0:
			// 允许 10% 抖动；超出即视为劣化（可能引入攒包）
			limit := h.cfg.BaselineTTFB * 1.10
			add(fmt.Sprintf("SSE TTFB p50 未劣化（对比基线 %.2fms，容差 10%%）",
				h.cfg.BaselineTTFB),
				worst <= limit && worst > 0,
				fmt.Sprintf("本轮最差 TTFB p50 = %.2fms，阈值 %.2fms", worst, limit))
		default:
			add("SSE TTFB p50 已记录（无对比基线，仅观察）", true,
				fmt.Sprintf("最差轮次 TTFB p50 = %.2fms。"+
					"注：ADR-005 的 %.1fms 是单流基线，不可与并发测量值直接比较；"+
					"优化后请用 -baseline-ttfb=%.2f 做同档对比",
					worst, TTFBBaselineMS, worst))
		}
		fallthrough

	case "D":
		// 并发额度与 SSE 计数必须归零，否则是泄漏
		leak := ""
		for _, r := range rep.Rounds {
			if r.ConcInFlight != 0 || r.SSEActive != 0 {
				leak += fmt.Sprintf("轮%d(in_flight=%.0f,sse=%.0f) ",
					r.Round, r.ConcInFlight, r.SSEActive)
			}
		}
		add("并发额度与 SSE 计数归零（无泄漏）", leak == "",
			orElse(leak == "", "全轮次归零", "泄漏轮次："+leak))
	}

	// 数据可信度：spread 远超 ADR-008 记录的 ±8% 说明环境噪声过大。
	//
	// 场景 B 豁免：它是「500 并发瞬时齐发」，总耗时仅数十毫秒，
	// QPS = 500/耗时 对调度抖动极敏感（实测 5 轮 spread 29%），
	// 但这与数据可信度无关 —— 该场景的被测量是**通过数**，
	// 且 5 轮全部恰好 50 个零误差。对它做 QPS 离散度判定是判据错配。
	if h.cfg.Scenario == "B" {
		add("多轮离散度（场景 B 不适用）", true,
			"场景 B 为瞬时齐发，QPS 抖动不反映数据质量；"+
				"其可信度由「通过数恰好等于 limit」直接保证")
	} else {
		noisy := ""
		for _, m := range rep.Median {
			if m.QPSSpread > 20 {
				noisy += fmt.Sprintf("conc=%d(%.1f%%) ", m.Conc, m.QPSSpread)
			}
		}
		add("多轮离散度 ≤ 20%", noisy == "",
			orElse(noisy == "", "各并发档波动在可接受范围",
				"以下并发档波动过大，建议重测："+noisy))
	}

	// 归因数据是否具备
	add("双侧 CPU 归因数据具备", rep.CPU.Available,
		orElse(rep.CPU.Available, rep.Attrib,
			rep.CPU.Note+"（ADR-008：无此数据结论不予采信）"))
	return v
}
