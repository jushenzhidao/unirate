package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// 三层采集（ADR-008 §三）：客户端分位数 / 网关 /metrics / Redis commandstats + 容器 CPU。
//
// 容器 CPU 的取数方式说明：
// 压测程序运行在容器内，拿不到 docker socket，因此无法直接调 `docker stats`。
// 双侧 CPU 由外层 shell（run.sh）用 `docker stats --no-stream` 定时采样写入文件，
// 本程序读取该文件并并入报告。若文件不存在则标注为 unavailable ——
// 绝不伪造 CPU 数据，因为归因结论完全依赖它（这正是首版 88k 归因失准的教训）。

// gatewayMetrics 从 obs 端口拉取并解析关心的指标
type gatewayMetrics struct {
	BreakerOpen   int64   `json:"redis_breaker_open"`
	ConcInFlight  float64 `json:"concurrency_in_flight"`
	SSEActive     float64 `json:"sse_streams_active"`
	RedisErrors   float64 `json:"redis_errors_total"`
	ReqTotal      float64 `json:"requests_total"`
	RejectedTotal float64 `json:"rejected_total"`
	PoolTimeouts  float64 `json:"redis_pool_timeouts_total"`
	PoolIdle      float64 `json:"redis_pool_idle_conns"`
	PoolTotal     float64 `json:"redis_pool_total_conns"`
	Raw           int     `json:"raw_bytes"`
}

// scrapeMetrics 拉取 /metrics 并按前缀求和。
// Prometheus text format 中同名指标会有多条带不同标签的行，这里按指标名聚合。
func scrapeMetrics(obsURL string) (gatewayMetrics, error) {
	var gm gatewayMetrics
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(obsURL + "/metrics")
	if err != nil {
		return gm, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return gm, err
	}
	gm.Raw = len(body)

	sum := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		// 形如：unirate_requests_total{biz="demo",...} 42
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		name := line[:sp]
		val, err := strconv.ParseFloat(strings.TrimSpace(line[sp+1:]), 64)
		if err != nil {
			continue
		}
		if b := strings.IndexByte(name, '{'); b >= 0 {
			name = name[:b]
		}
		sum[name] += val
	}

	get := func(n string) float64 { return sum["unirate_"+n] }
	gm.BreakerOpen = int64(get("redis_breaker_open"))
	gm.ConcInFlight = get("concurrency_in_flight")
	gm.SSEActive = get("sse_streams_active")
	gm.RedisErrors = get("redis_errors_total")
	gm.ReqTotal = get("requests_total")
	gm.RejectedTotal = get("rejected_total")
	// 池指标为 ADR-004 建议新增项，可能尚未实现 —— 缺失时为 0，报告中标注
	gm.PoolTimeouts = get("redis_pool_timeouts_total")
	gm.PoolIdle = get("redis_pool_idle_conns")
	gm.PoolTotal = get("redis_pool_total_conns")
	return gm, nil
}

// redisStats 通过最小 RESP 客户端读取 INFO，避免引入 go-redis 依赖
// （压测程序须零第三方依赖以保证离线内网可跑）。
type redisStats struct {
	EvalshaCalls   int64   `json:"evalsha_calls"`
	EvalshaUsecAvg float64 `json:"evalsha_usec_per_call"`
	EvalCalls      int64   `json:"eval_calls"`
	EvalUsecAvg    float64 `json:"eval_usec_per_call"`
	UsedCPUSys     float64 `json:"used_cpu_sys"`
	UsedCPUUser    float64 `json:"used_cpu_user"`
	ConnectedCli   int64   `json:"connected_clients"`
}

func (r redisStats) cpuTotal() float64 { return r.UsedCPUSys + r.UsedCPUUser }

// parseRedisInfo 解析 INFO 输出中所需字段
func parseRedisInfo(info string) redisStats {
	var rs redisStats
	for _, ln := range strings.Split(info, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "used_cpu_sys:"):
			rs.UsedCPUSys, _ = strconv.ParseFloat(ln[len("used_cpu_sys:"):], 64)
		case strings.HasPrefix(ln, "used_cpu_user:"):
			rs.UsedCPUUser, _ = strconv.ParseFloat(ln[len("used_cpu_user:"):], 64)
		case strings.HasPrefix(ln, "connected_clients:"):
			rs.ConnectedCli, _ = strconv.ParseInt(ln[len("connected_clients:"):], 10, 64)
		case strings.HasPrefix(ln, "cmdstat_evalsha:"):
			rs.EvalshaCalls, rs.EvalshaUsecAvg = parseCmdStat(ln)
		case strings.HasPrefix(ln, "cmdstat_eval:"):
			rs.EvalCalls, rs.EvalUsecAvg = parseCmdStat(ln)
		}
	}
	return rs
}

func parseCmdStat(line string) (calls int64, usecPerCall float64) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return
	}
	for _, f := range strings.Split(line[i+1:], ",") {
		kv := strings.SplitN(strings.TrimSpace(f), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "calls":
			calls, _ = strconv.ParseInt(kv[1], 10, 64)
		case "usec_per_call":
			usecPerCall, _ = strconv.ParseFloat(kv[1], 64)
		}
	}
	return
}

// cpuSample 外层 shell 采集的容器 CPU 样本
type cpuSample struct {
	Available bool    `json:"available"`
	RedisPct  float64 `json:"redis_cpu_percent"`
	GwPct     float64 `json:"gateway_cpu_percent"`
	Samples   int     `json:"samples"`
	Note      string  `json:"note,omitempty"`
}

// window 表示一段正式采集窗口（预热与重置期不计入）
type window struct{ from, to int64 }

// readCPUSamples 读取 shell 写入的 CPU 采样文件。
//
// 格式（每行）：<epoch秒> <container-name> <cpu-percent>
// 只统计落在 windows 内的样本 —— 预热期与轮间重置期系统性偏空闲，
// 计入会稀释均值并低估负载侧 CPU，使归因失真。
// windows 为空时退化为「全量统计」并在 Note 中标注。
// 文件缺失时返回 Available=false，绝不伪造数据。
func readCPUSamples(path string, windows []window) cpuSample {
	cs := cpuSample{}
	if path == "" {
		cs.Note = "未指定 -cpufile：双侧 CPU 未采集，归因结论不成立"
		return cs
	}
	f, err := os.Open(path)
	if err != nil {
		cs.Note = fmt.Sprintf("CPU 采样文件不可读(%v)：归因结论不成立", err)
		return cs
	}
	defer f.Close()

	inWindow := func(ts int64) bool {
		if len(windows) == 0 {
			return true
		}
		for _, w := range windows {
			if ts >= w.from && ts <= w.to {
				return true
			}
		}
		return false
	}

	var rSum, gSum float64
	var rN, gN, skipped int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 3 {
			continue // 旧格式或半行，跳过
		}
		ts, err := strconv.ParseInt(fs[0], 10, 64)
		if err != nil {
			continue
		}
		if !inWindow(ts) {
			skipped++
			continue
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(fs[2], "%"), 64)
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(fs[1], "redis"):
			rSum += pct
			rN++
		case strings.Contains(fs[1], "gateway"):
			gSum += pct
			gN++
		}
	}
	if rN == 0 && gN == 0 {
		cs.Note = fmt.Sprintf(
			"采集窗口内无 CPU 样本（窗口外丢弃 %d 条）：归因结论不成立。"+
				"可能原因：单轮时长过短，采样间隔（约 2s）来不及取到样本", skipped)
		return cs
	}
	cs.Available = true
	cs.Samples = rN
	if rN > 0 {
		cs.RedisPct = round2(rSum / float64(rN))
	}
	if gN > 0 {
		cs.GwPct = round2(gSum / float64(gN))
	}
	if len(windows) == 0 {
		cs.Note = "未提供采集窗口，统计了全量样本（含预热/空闲期，均值可能被稀释）"
	}
	return cs
}

// attribute 给出瓶颈归因结论。
//
// 这是 ADR-008 §五 定的强制判据 —— 首版把「简化脚本的 88k 平台期」
// 误读为「Redis 饱和」，根因就是只看 QPS 不看 CPU 归属。
func attribute(cs cpuSample) string {
	if !cs.Available {
		return "无法归因：缺双侧 CPU 数据（按 ADR-008，此时压测结论不予采信）"
	}
	const full = 80.0 // 单核跑满阈值（%）
	switch {
	case cs.RedisPct >= full:
		return fmt.Sprintf("瓶颈在 Redis 单线程（Redis %.1f%% 已跑满单核，gateway %.1f%%）",
			cs.RedisPct, cs.GwPct)
	case cs.GwPct >= full*3: // gateway 可用多核，阈值按多核放大
		return fmt.Sprintf("瓶颈在 gateway（gateway %.1f%%，Redis 仅 %.1f%%）",
			cs.GwPct, cs.RedisPct)
	default:
		return fmt.Sprintf("两侧均未饱和（Redis %.1f%%，gateway %.1f%%）"+
			"→ 瓶颈可能在压测客户端或网络；须加客户端资源重测，本轮数据不可用于容量结论",
			cs.RedisPct, cs.GwPct)
	}
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
