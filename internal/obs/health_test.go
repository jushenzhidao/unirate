package obs

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLivenessIgnoresDependencies 验证评审 P1-11 的核心修正。
//
// 原 Spec 把 /health 定义为「包含 Redis 连通性」，而 §2.7 又声明
// 「Redis 故障时继续服务」。两者冲突的后果：Redis 抖动 → 探针失败
// → K8s 重启所有 Pod / SLB 摘掉所有实例 → 把优雅降级变成全站不可用。
//
// 正确语义：/live 绝不检查任何外部依赖。
func TestLivenessIgnoresDependencies(t *testing.T) {
	h := NewHealth("test")
	// 注入一个永远失败的 Redis 探测
	h.BindRedis(func(context.Context) error { return errors.New("connection refused") })

	w := httptest.NewRecorder()
	h.LiveHandler().ServeHTTP(w, httptest.NewRequest("GET", "/live", nil))
	if w.Code != 200 {
		t.Fatalf("/live must stay 200 even when redis is down, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "alive") {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

// TestReadinessOnlyChecksConfig /ready 只关心「缺了就无法工作」的依赖
func TestReadinessOnlyChecksConfig(t *testing.T) {
	h := NewHealth("test")
	h.BindRedis(func(context.Context) error { return errors.New("down") })

	// 配置未加载 → 不可接收流量
	w := httptest.NewRecorder()
	h.ReadyHandler().ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 503 {
		t.Fatalf("expected 503 before config loaded, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "not_ready") {
		t.Errorf("body should explain the reason: %s", w.Body.String())
	}

	// 配置就绪后必须放行，即使 Redis 仍然故障 —— 网关有降级路径
	h.MarkReady()
	w = httptest.NewRecorder()
	h.ReadyHandler().ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 200 {
		t.Fatalf("ready must be 200 once config is loaded (redis has a degraded path), got %d", w.Code)
	}
}

// TestShutdownFlipsReadyFirst 优雅退出必须先让 /ready 转 503，
// 使 SLB 提前摘流，避免在途请求被硬切断
func TestShutdownFlipsReadyFirst(t *testing.T) {
	h := NewHealth("test")
	h.MarkReady()
	h.MarkShuttingDown()

	w := httptest.NewRecorder()
	h.ReadyHandler().ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	if w.Code != 503 {
		t.Errorf("/ready must be 503 while shutting down, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "shutting_down") {
		t.Errorf("body: %s", w.Body.String())
	}

	// 但 /live 必须仍为 200，直到进程真正退出
	w = httptest.NewRecorder()
	h.LiveHandler().ServeHTTP(w, httptest.NewRequest("GET", "/live", nil))
	if w.Code != 200 {
		t.Errorf("/live must stay 200 during graceful shutdown, got %d", w.Code)
	}
}

// TestHealthAlwaysReturns200 /health 是排障视图，绝不参与自动摘流决策，
// 因此任何情况都返回 200，用 body 里的 status 字段表达降级
func TestHealthAlwaysReturns200(t *testing.T) {
	h := NewHealth("v1.2.3")
	h.BindRedis(func(context.Context) error { return errors.New("redis exploded") })
	h.BindConfig(func() (int64, bool, int) { return 7, true, 2 })
	h.BindBreaker(func() (int64, int64, bool) { return 99, 3, true })

	w := httptest.NewRecorder()
	h.HealthHandler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))

	if w.Code != 200 {
		t.Fatalf("/health must always be 200 (it is a diagnostic view), got %d", w.Code)
	}
	body := w.Body.String()
	// 必须诚实反映降级
	if !strings.Contains(body, `"status":"degraded"`) {
		t.Errorf("degraded state must be reported in body: %s", body)
	}
	for _, want := range []string{"redis", "config", "redis_breaker", "v1.2.3", "uptime_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in health report: %s", want, body)
		}
	}
}

func TestHealthOKWhenAllGood(t *testing.T) {
	h := NewHealth("v1")
	h.MarkReady()
	h.BindRedis(func(context.Context) error { return nil })
	h.BindConfig(func() (int64, bool, int) { return 5, false, 3 })
	h.BindBreaker(func() (int64, int64, bool) { return 0, 0, false })

	w := httptest.NewRecorder()
	h.HealthHandler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	body := w.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("expected ok status: %s", body)
	}
	if !strings.Contains(body, `"ready":true`) {
		t.Errorf("expected ready true: %s", body)
	}
}
