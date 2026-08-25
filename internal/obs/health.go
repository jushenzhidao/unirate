package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// 健康检查（对应评审 P1-11 修正）
//
// 原设计缺陷：Spec §2.7 声明「Redis 故障时 Fail-Open 继续服务」，
// §5.3 又把 /health 定义为「包含 Redis 连通性」。两者直接冲突 ——
// Redis 挂掉时网关明明还能正常代理，但 /health 返回 503 会让 K8s/SLB
// 把所有实例摘掉，把「优雅降级」变成「全站不可用」。
//
// 修正：按 Kubernetes 探针语义严格分离三个端点。
//
//	/live   进程存活。只要 HTTP server 能响应就返回 200。
//	        绝不检查任何外部依赖 —— 否则依赖抖动会触发无意义的容器重启。
//	/ready  是否应接收新流量。只检查「缺了就无法工作」的依赖：
//	        配置必须已加载（无配置无法路由）。Redis 不在其中 —— 它有降级路径。
//	/health 人类可读的完整状态聚合，含 Redis / 配置 / 熔断器明细。
//	        仅用于排障与监控看板，不接入任何自动化摘流决策。
type Health struct {
	ready    atomic.Bool
	shutdown atomic.Bool

	redisPing   func(context.Context) error
	configState func() (version int64, degraded bool, bizCount int)
	breaker     func() (errs, trips int64, open bool)

	version   string
	startedAt time.Time
}

// NewHealth 创建健康检查器
func NewHealth(version string) *Health {
	return &Health{version: version, startedAt: time.Now()}
}

// BindRedis 注入 Redis 连通性探测
func (h *Health) BindRedis(f func(context.Context) error) { h.redisPing = f }

// BindConfig 注入配置状态查询
func (h *Health) BindConfig(f func() (int64, bool, int)) { h.configState = f }

// BindBreaker 注入熔断器状态查询
func (h *Health) BindBreaker(f func() (int64, int64, bool)) { h.breaker = f }

// MarkReady 标记已可接收流量（配置加载成功后调用）
func (h *Health) MarkReady() { h.ready.Store(true) }

// MarkShuttingDown 进入优雅退出，/ready 立即转 503 以便 SLB 提前摘流
func (h *Health) MarkShuttingDown() { h.shutdown.Store(true) }

// LiveHandler 进程存活探针
func (h *Health) LiveHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
}

// ReadyHandler 流量准入探针
func (h *Health) ReadyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case h.shutdown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"shutting_down"}`))
		case !h.ready.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not_ready","reason":"config not loaded"}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready"}`))
		}
	})
}

type healthReport struct {
	Status     string         `json:"status"`
	Version    string         `json:"version"`
	UptimeSec  float64        `json:"uptime_seconds"`
	Ready      bool           `json:"ready"`
	Components map[string]any `json:"components"`
}

// HealthHandler 完整状态聚合，仅供排障，不参与自动摘流
func (h *Health) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep := healthReport{
			Status:     "ok",
			Version:    h.version,
			UptimeSec:  time.Since(h.startedAt).Seconds(),
			Ready:      h.ready.Load(),
			Components: map[string]any{},
		}

		if h.redisPing != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
			err := h.redisPing(ctx)
			cancel()
			if err != nil {
				// Redis 异常只降级为 degraded，绝不返回非 200 —— 见文件头说明
				rep.Components["redis"] = map[string]any{"ok": false, "error": err.Error()}
				rep.Status = "degraded"
			} else {
				rep.Components["redis"] = map[string]any{"ok": true}
			}
		}

		if h.configState != nil {
			ver, degraded, n := h.configState()
			rep.Components["config"] = map[string]any{
				"version": ver, "degraded": degraded, "biz_count": n,
			}
			if degraded {
				rep.Status = "degraded"
			}
		}

		if h.breaker != nil {
			errs, trips, open := h.breaker()
			rep.Components["redis_breaker"] = map[string]any{
				"errors": errs, "trips": trips, "open": open,
			}
			if open {
				rep.Status = "degraded"
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(rep)
	})
}
