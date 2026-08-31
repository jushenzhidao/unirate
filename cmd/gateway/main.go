package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/unirate/gateway/internal/admin"
	"github.com/unirate/gateway/internal/config"
	"github.com/unirate/gateway/internal/limiter"
	"github.com/unirate/gateway/internal/meta"
	"github.com/unirate/gateway/internal/obs"
	"github.com/unirate/gateway/internal/proxy"
	"github.com/unirate/gateway/internal/store"
	"github.com/unirate/gateway/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	rt := config.LoadRuntime()
	log, logLevel := newLogger(rt.LogLevel)

	log.Info("starting unirate gateway",
		"version", rt.Version,
		"proxy", rt.ProxyAddr, "admin", rt.AdminAddr, "obs", rt.ObsAddr,
		"instances", rt.Instances)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Logfire 直接推送（可选）
	var logfireShutdown func(context.Context) error
	if token := os.Getenv("LOGFIRE_TOKEN"); token != "" {
		lfCfg := obs.LogfireConfig{
			Token:    token,
			Endpoint: os.Getenv("LOGFIRE_ENDPOINT"),
			Interval: 15 * time.Second,
			Env:      os.Getenv("DEPLOY_ENV"),
		}
		if v := os.Getenv("OTEL_SCRAPE_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				lfCfg.Interval = d
			}
		}
		var err error
		logfireShutdown, err = obs.InitLogfireExporter(lfCfg, log)
		if err != nil {
			log.Error("failed to initialize logfire exporter; metrics will only be available via /metrics", "err", err)
		} else {
			log.Info("logfire direct push enabled", "interval", lfCfg.Interval)
		}
	}

	rdb := newRedis(rt)
	defer func() { _ = rdb.Close() }()

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		pingCancel()
		// Redis 不可用不阻止启动 —— 网关有降级路径，
		// 但必须显式告警，避免「静默降级」导致限流形同虚设而无人知晓
		log.Error("redis unreachable at startup; gateway will run in degraded mode", "err", err)
	} else {
		pingCancel()
		log.Info("redis connected", "addrs", rt.RedisAddrs)
	}

	// SQLite 嵌入进程，无外部依赖。DSN 为空时落盘到默认路径
	// （data/unirate.db），保证默认部署即具备配置写入能力。
	sdb, err := store.Open(rt.StoreDSN)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = sdb.Close() }()

	// SQLite 打不开通常是目录权限或磁盘满，属于必须立刻暴露的部署错误：
	// 库在进程内，没有「稍后重连」这一说，继续启动只会让管理面
	// 在第一次写入时才报错，且错误现场已丢失。
	if err := sdb.Ping(ctx); err != nil {
		return fmt.Errorf("sqlite unusable (check volume permissions): %w", err)
	}
	log.Info("store connected", "path", rt.StoreDSN)

	// 幂等建表。首次启动必须建表，否则管理面第一次写入即失败。
	migCtx, migCancel := context.WithTimeout(ctx, 30*time.Second)
	err = sdb.Migrate(migCtx)
	migCancel()
	if err != nil {
		return fmt.Errorf("schema migrate: %w", err)
	}

	// 种子数据加载。仅当 SEED_SQL_DIR 显式设置时生效 ——
	// 生产编排不设置该变量，测试编排挂载 deploy/seed。
	//
	// 失败必须致命：种子加载不了意味着 e2e 断言的前置数据缺失，
	// 静默跳过会让失败表现为「限流逻辑不对」，排查成本极高。
	if dir := os.Getenv(store.SeedDirEnv); dir != "" {
		seedCtx, seedCancel := context.WithTimeout(ctx, 30*time.Second)
		err := sdb.LoadSeeds(seedCtx, dir)
		seedCancel()
		if err != nil {
			return fmt.Errorf("load seeds from %s: %w", dir, err)
		}
		log.Info("seed data loaded", "dir", dir)
	}

	db := sdb.DB

	cfgStore := config.NewStore(db, rdb, log)
	defer cfgStore.Close()

	// 环境变量层作为 Tier 1 策略的 base，必须在 Bootstrap 之前注入 ——
	// 否则首次解析会以内置默认值为 base，环境变量在启动窗口内被忽略。
	cfgStore.SetPolicyBase(config.PolicyFromEnv(rt))

	health := obs.NewHealth(rt.Version)
	metrics := obs.NewMetrics()

	bootCtx, bootCancel := context.WithTimeout(ctx, 15*time.Second)
	bootErr := cfgStore.Bootstrap(bootCtx)
	bootCancel()
	if bootErr != nil {
		// 无配置无法路由，此时 /ready 保持 503，由编排层决定是否重启，
		// 但进程继续存活等待配置就绪（配置中心可能只是短暂不可用）
		log.Error("config bootstrap failed; staying not-ready until config arrives", "err", bootErr)
	} else {
		health.MarkReady()
	}
	cfgStore.Watch(ctx, rt.ConfigPollInterval)

	lim := limiter.New(rdb, limiter.Options{
		TZOffsetSeconds: rt.TZOffsetSeconds,
		Instances:       rt.Instances,
		RedisTimeout:    rt.RedisTimeout,
	})

	go runtimeStateLoop(ctx, cfgStore, lim, health, metrics, log)

	policy := upstream.DefaultPolicy()
	policy.AllowHeaderOverride = rt.AllowHeaderUpstream
	policy.HostAllowlist = rt.UpstreamAllowlist
	resolver := upstream.New(cfgStore, policy, "", "BIZ_")
	if rt.AllowHeaderUpstream {
		log.Warn("ALLOW_HEADER_UPSTREAM is enabled; " +
			"clients can direct traffic via X-Upstream-Base-URL (private targets are still blocked)")
	}

	mc := meta.DefaultConfig()
	mc.TrustedProxyHops = rt.TrustedProxyHops
	mc.RealIPHeader = rt.RealIPHeader
	mc.TokenHeaders = rt.TokenHeaders
	if rt.TrustedProxyHops == 0 && rt.RealIPHeader == "" {
		log.Info("XFF is not trusted (TRUSTED_PROXY_HOPS=0); ip dimension uses TCP peer address")
	}

	popt := proxy.DefaultOptions()
	popt.MetaConfig = mc
	popt.UpstreamTimeout = rt.UpstreamTimeout
	popt.TokenFlushEvery = rt.TokenFlushEvery
	popt.MaxRequestBody = rt.MaxRequestBody
	popt.Instances = rt.Instances
	popt.ExposeRuleName = rt.ExposeRuleName

	ph := proxy.New(lim, cfgStore, resolver, metrics, log, popt)

	// Tier 1 运行策略热更新：一处订阅，扇出到三个消费者。
	// 注册时会立即回调一次当前值，因此启动即包含页面已有的覆盖项 ——
	// 不需要在这里手动 apply 一遍，也就不会出现两处逻辑不一致。
	cfgStore.OnPolicyChange(func(p *config.Policy) {
		ph.ApplyPolicy(p)
		lim.SetInstances(p.Instances)
		logLevel.Set(parseLevel(p.LogLevel))
	})

	health.BindRedis(func(c context.Context) error { return rdb.Ping(c).Err() })
	health.BindConfig(func() (int64, bool, int) {
		s := cfgStore.Current()
		return s.Version, cfgStore.Degraded(), len(s.Bizs)
	})
	health.BindBreaker(lim.BreakerStats)

	// 三个监听端口职责分离：
	//   ProxyAddr 业务流量（对外）
	//   ObsAddr   指标与探针（仅内网/监控系统）
	//   AdminAddr 管理面（仅内网 + 鉴权）
	proxySrv := &http.Server{
		Addr:              rt.ProxyAddr,
		Handler:           ph,
		ReadHeaderTimeout: 15 * time.Second,
		// 不设 WriteTimeout：SSE 长连接会被它硬切断
		IdleTimeout: 120 * time.Second,
	}

	obsMux := http.NewServeMux()
	obsMux.Handle("/metrics", metrics.Handler())
	obsMux.Handle("/live", health.LiveHandler())
	obsMux.Handle("/ready", health.ReadyHandler())
	obsMux.Handle("/health", health.HealthHandler())
	obsSrv := &http.Server{
		Addr: rt.ObsAddr, Handler: obsMux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	errCh := make(chan error, 3)

	go func() {
		log.Info("proxy listening", "addr", rt.ProxyAddr)
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	go func() {
		log.Info("observability listening", "addr", rt.ObsAddr)
		if err := obsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("obs server: %w", err)
		}
	}()

	// 管理面不再有条件启用：存储始终可用，配置写入能力是网关的基本功能。
	var adminSrv *admin.Server
	{
		var err error
		adminSrv, err = admin.New(sdb, cfgStore, log, admin.Options{
			Addr:       rt.AdminAddr,
			Token:      rt.AdminToken,
			AllowCIDRs: rt.AdminAllowCIDRs,
			// 与 obs 端口共用同一份内存指标：控制台看板经受鉴权的
			// /admin/metrics 同源取数，obs 端口继续专供 Prometheus 抓取。
			Metrics: metrics,
		})
		if err != nil {
			// 拒绝以无鉴权状态启动管理面，这是硬约束
			return fmt.Errorf("admin api: %w", err)
		}
		go func() {
			if err := adminSrv.Start(); err != nil {
				errCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// 优雅退出：先让 /ready 转 503 使 SLB 摘流，再等在途请求结束
	health.MarkShuttingDown()
	time.Sleep(2 * time.Second)

	sctx, scancel := context.WithTimeout(context.Background(), rt.ShutdownGrace)
	defer scancel()

	if adminSrv != nil {
		_ = adminSrv.Shutdown(sctx)
	}
	_ = obsSrv.Shutdown(sctx)
	if err := proxySrv.Shutdown(sctx); err != nil {
		log.Warn("proxy shutdown timed out; some in-flight requests were cut", "err", err)
	}

	// Flush 最后一批指标到 Logfire
	if logfireShutdown != nil {
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := logfireShutdown(flushCtx); err != nil {
			log.Warn("logfire shutdown incomplete; last batch may be lost", "err", err)
		}
		flushCancel()
	}

	log.Info("gateway stopped")
	return nil
}

// runtimeStateLoop 周期性把内部状态同步到指标。
//
// 这里采集的是「网关自身是否健康」而非业务流量指标，
// 其中 redis_breaker_open 是最关键的一条 —— 它区分
// 「限流正在精确生效」与「限流已静默失效」，必须始终可观测，
// 哪怕从未发生过故障也要输出 0，否则告警规则 (absent) 无法区分
// 「指标缺失」和「一切正常」。
func runtimeStateLoop(ctx context.Context, cfgStore *config.Store, lim *limiter.Limiter,
	health *obs.Health, m *obs.Metrics, log *slog.Logger) {

	sync := func() {
		snap := cfgStore.Current()
		m.ConfigVersion.Set(snap.Version)

		_, _, open := lim.BreakerStats()
		var v int64
		if open {
			v = 1
		}
		m.BreakerOpen.Set(v)
	}

	// 启动即输出一次，避免抓取窗口内指标为空
	sync()

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	marked := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sync()
			if !marked && len(cfgStore.Current().Bizs) > 0 {
				health.MarkReady()
				marked = true
				log.Info("config became available; marked ready", "version", cfgStore.Current().Version)
			}
		}
	}
}

func newRedis(rt *config.Runtime) redis.UniversalClient {
	return redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    rt.RedisAddrs,
		Password: rt.RedisPassword,
		DB:       rt.RedisDB,
		// 连接池必须足够大：压测证明池不足会造成命令排队超时，
		// 进而被误判为 Redis 故障（详见 limiter/breaker.go）
		PoolSize:        rt.RedisPoolSize,
		MinIdleConns:    16,
		PoolTimeout:     time.Second,
		ConnMaxIdleTime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
		MaxRetries:      2,
	})
}

// newLogger 构造日志器，并返回可热更新级别的 LevelVar。
//
// slog.LevelVar 本身是并发安全的，把它交出去就能在运行期改级别 ——
// 这是 LOG_LEVEL 归入 Tier 1 的前提：线上排障需要临时开 debug，
// 而重启会丢掉正在复现的现场。
func newLogger(level string) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	lv.Set(parseLevel(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})), lv
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
