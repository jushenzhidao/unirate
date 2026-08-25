// unirate 压测程序（ADR-008 的可执行实现）
//
// 为何自写而非 k6：k6 官方镜像在本项目的网络环境下拉取失败（实测持续重试超时），
// 方案不可执行。自写 Go 程序的额外好处：
//   - 零第三方依赖，离线内网可直接 `go run`
//   - 能精确实现 ADR-008 要求的预热、多轮中位数、双侧 CPU 归因逻辑
//   - 与被测服务同语言，便于复用 SSE 语义判断（首帧识别、帧计数）
//
// 用法（在仓库根执行）：
//
//	docker run --rm --network unirate_unirate \
//	  -v "$PWD":/src -w /src -v unirate-gomod:/go/pkg/mod \
//	  -e GOFLAGS=-mod=mod -e GOPROXY=https://goproxy.cn,direct \
//	  golang:1.22-alpine go run ./test/perf/loadgen -scenario=A -conc=256
//
// 注意：本目录是**独立 module**，上面的 `go run ./test/perf/loadgen`
// 需在仓库根以 module-aware 模式跑会失败（不属于主 module）。
// 正确方式见 test/perf/run.sh —— 它 cd 进本目录后再执行。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type config struct {
	ProxyURL   string
	ObsURL     string
	AdminURL   string
	AdminToken string
	RedisAddr  string
	RedisPass  string
	// UpstreamBase 场景 B 专属 biz 指向的上游（compose 内的 mock）
	UpstreamBase string

	Scenario string
	Concs    []int
	Duration time.Duration
	Warmup   time.Duration
	Rounds   int
	Limit    int64
	Chunks   int
	DelayMS  int
	CPUFile  string
	OutFile  string
	Label    string
	// BaselineTTFB 优化前同并发档的 TTFB p50（ms），用于场景 C 做相对对比。
	// 为 0 时只记录不判失败（详见 report.go 中的判据说明）。
	BaselineTTFB float64
}

type harness struct {
	cfg         config
	client      *http.Client
	lastElapsed time.Duration
	// windows 记录每轮正式采集的起止 epoch 秒，用于过滤容器 CPU 样本，
	// 使归因只基于真实负载期而非预热/空闲期
	windows []window
}

func main() {
	cfg := parseFlags()
	h := newHarness(cfg)

	if err := h.healthCheck(); err != nil {
		fmt.Fprintf(os.Stderr, "\n健康检查失败，拒绝产出垃圾数据：%v\n", err)
		os.Exit(1)
	}

	report, err := h.run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n压测失败：%v\n", err)
		os.Exit(1)
	}

	h.printReport(report)
	if cfg.OutFile != "" {
		if err := writeJSON(cfg.OutFile, report); err != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败：%v\n", cfg.OutFile, err)
			os.Exit(1)
		}
		fmt.Printf("\n基线已固化：%s\n", cfg.OutFile)
	}
	if !report.Verdict.Pass {
		os.Exit(2) // 硬断言失败 → 非零退出，便于 CI 拦截
	}
}

func parseFlags() config {
	var cfg config
	var concs string
	flag.StringVar(&cfg.ProxyURL, "proxy", env("PERF_PROXY", "http://gateway:8080"), "网关业务端口")
	flag.StringVar(&cfg.ObsURL, "obs", env("PERF_OBS", "http://gateway:9091"), "网关 obs 端口")
	flag.StringVar(&cfg.AdminURL, "admin", env("PERF_ADMIN", "http://gateway:9090"), "网关 admin 端口")
	flag.StringVar(&cfg.AdminToken, "admin-token", os.Getenv("ADMIN_TOKEN"), "admin Bearer token")
	flag.StringVar(&cfg.RedisAddr, "redis", env("REDIS_ADDR", "redis:6379"), "Redis 地址")
	flag.StringVar(&cfg.RedisPass, "redis-pass", os.Getenv("REDIS_PASSWORD"), "Redis 密码")
	flag.StringVar(&cfg.UpstreamBase, "upstream", env("PERF_UPSTREAM", "http://mock-upstream:9000"),
		"场景 B 专属 biz 的上游地址")

	flag.StringVar(&cfg.Scenario, "scenario", "A", "场景 A|B|C|D")
	flag.StringVar(&concs, "conc", "", "并发档，逗号分隔（默认按场景取 ADR-008 定义值）")
	flag.DurationVar(&cfg.Duration, "duration", 20*time.Second, "单轮采集时长")
	flag.DurationVar(&cfg.Warmup, "warmup", 30*time.Second, "预热时长（ADR-008 要求 30s）")
	flag.IntVar(&cfg.Rounds, "rounds", 5, "轮数（ADR-008 要求 5 轮取中位数）")
	flag.Int64Var(&cfg.Limit, "limit", 50, "场景 B 的配额上限")
	flag.IntVar(&cfg.Chunks, "chunks", 120, "场景 C 每流帧数")
	flag.IntVar(&cfg.DelayMS, "delay-ms", 20, "场景 C 上游帧间隔")
	flag.StringVar(&cfg.CPUFile, "cpufile", os.Getenv("PERF_CPUFILE"), "外层 shell 采集的容器 CPU 样本文件")
	flag.StringVar(&cfg.OutFile, "out", "", "基线 JSON 输出路径")
	flag.StringVar(&cfg.Label, "label", "", "本次运行标签（如 before-opt / after-opt）")
	flag.Float64Var(&cfg.BaselineTTFB, "baseline-ttfb", 0,
		"场景 C 对比基线：优化前同并发档的 TTFB p50（ms）；为 0 时只记录不判失败")
	flag.Parse()

	cfg.Scenario = strings.ToUpper(strings.TrimSpace(cfg.Scenario))
	cfg.Concs = parseConcs(concs, cfg.Scenario)
	return cfg
}

func parseConcs(s, scenario string) []int {
	if s = strings.TrimSpace(s); s != "" {
		var out []int
		for _, p := range strings.Split(s, ",") {
			if n := atoiSafe(strings.TrimSpace(p)); n > 0 {
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// ADR-008 §二 定义的默认并发档
	switch scenario {
	case "A":
		return []int{64, 256, 512, 1024}
	case "B":
		return []int{500} // 固定 500 并发打 limit=50
	case "C":
		return []int{100, 500, 1000, 2000}
	default:
		return []int{256}
	}
}

func newHarness(cfg config) *harness {
	// 客户端连接池必须远大于最高并发，否则测的是客户端瓶颈。
	// 这是压测程序最容易犯的错：默认 Transport 的 MaxIdleConnsPerHost=2，
	// 会让 2000 并发退化为 2 条连接上的排队。
	maxConc := 0
	for _, c := range cfg.Concs {
		if c > maxConc {
			maxConc = c
		}
	}
	pool := maxConc * 2
	if pool < 256 {
		pool = 256
	}
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 60 * time.Second}).DialContext,
		MaxIdleConns:        pool,
		MaxIdleConnsPerHost: pool,
		MaxConnsPerHost:     0, // 不限制，避免成为人为瓶颈
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,  // SSE 场景必须；非流式也无需压缩，减少客户端 CPU
		ForceAttemptHTTP2:   false, // 固定 HTTP/1.1，避免 h2 多路复用掩盖并发行为
	}
	return &harness{
		cfg:    cfg,
		client: &http.Client{Transport: tr, Timeout: 5 * time.Minute},
	}
}

// healthCheck 开跑前的前置校验（ADR-008 要求）：
// /ready 必须 200 且 Redis 可达，否则直接退出不产出数据。
func (h *harness) healthCheck() error {
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(h.cfg.ObsURL + "/ready")
	if err != nil {
		return fmt.Errorf("网关 obs 不可达 %s: %w", h.cfg.ObsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("网关 /ready 返回 %d（期望 200）—— 配置未就绪，压测无意义", resp.StatusCode)
	}

	rc, err := h.dialRedis()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := rc.ping(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	// 冒烟：确认业务路径真的通，避免 500 个 502 被当成压测结果
	sr, err := cli.Get(h.cfg.ProxyURL + "/demo/status/200")
	if err != nil {
		return fmt.Errorf("业务端口不可达: %w", err)
	}
	defer sr.Body.Close()
	if sr.StatusCode != http.StatusOK && sr.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("业务冒烟返回 %d（期望 200/429）—— 上游或配置有问题", sr.StatusCode)
	}
	return nil
}

func (h *harness) dialRedis() (*respConn, error) {
	return dialRedis(h.cfg.RedisAddr, h.cfg.RedisPass, 5*time.Second)
}

func (h *harness) redisInfo() (redisStats, error) {
	rc, err := h.dialRedis()
	if err != nil {
		return redisStats{}, err
	}
	defer rc.Close()
	return rc.info("cpu")
}

// resetState 每轮前重置（ADR-008 §四.2）：
// FLUSHDB 清限流计数器 + CONFIG RESETSTAT 清命令统计 + reload 让配置回到 Redis。
func (h *harness) resetState() error {
	rc, err := h.dialRedis()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := rc.flushDB(); err != nil {
		return fmt.Errorf("flushdb: %w", err)
	}
	if err := rc.resetStat(); err != nil {
		return fmt.Errorf("config resetstat: %w", err)
	}
	// FLUSHDB 连带清掉了 Redis 里的配置快照，触发 admin reload 从 MySQL 重新发布，
	// 否则新起的实例或轮询会读到空配置
	if h.cfg.AdminToken != "" {
		req, _ := http.NewRequest(http.MethodPost, h.cfg.AdminURL+"/admin/reload", nil)
		req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
		if resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
		}
	}
	time.Sleep(500 * time.Millisecond) // 让配置广播落地
	return nil
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func hostEnvNote() string {
	return fmt.Sprintf("GOMAXPROCS=%d NumCPU=%d GOOS=%s GOARCH=%s",
		runtime.GOMAXPROCS(0), runtime.NumCPU(), runtime.GOOS, runtime.GOARCH)
}
