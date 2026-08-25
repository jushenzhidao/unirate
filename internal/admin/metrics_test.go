package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/unirate/gateway/internal/obs"
)

// metricsServer 注入了指标集合的测试服务
func metricsServer(t *testing.T) (*Server, *obs.Metrics) {
	t.Helper()
	m := obs.NewMetrics()
	s := newTestServer(t, Options{Addr: ":0", Metrics: m})
	return s, m
}

func authGet(h http.Handler, method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestMetricsRequiresAuth 无凭证必须 401 且带 challenge 头。
// 这条与 TestAuthRequired 重复是刻意的：指标端点是最容易被当成
// 「反正只是些数字」而开成公开的端点，值得有一条专属回归。
func TestMetricsRequiresAuth(t *testing.T) {
	s, _ := metricsServer(t)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/admin/metrics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证请求期望 401，实际 %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("缺少 WWW-Authenticate challenge 头")
	}
	// 关键：401 的响应体里不得漏出任何指标内容
	if strings.Contains(w.Body.String(), "unirate_") {
		t.Errorf("未鉴权响应泄露了指标内容: %q", w.Body.String())
	}
}

// TestMetricsAuthOuterThanDependencyGuard auth 必须在 requireMetrics 外层。
// 若顺序颠倒，未鉴权者能通过 503 与 401 的差异推断出网关是否装配了指标，
// 与 server.go 既有的探测防护要求一致。
func TestMetricsAuthOuterThanDependencyGuard(t *testing.T) {
	// 故意不注入 Metrics：守卫在内层的话会先返回 503
	s := newTestServer(t, Options{Addr: ":0"})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/admin/metrics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无凭证 + 依赖缺失时必须优先返回 401，实际 %d —— auth 未在最外层", w.Code)
	}
}

// TestMetricsReturnsPrometheusText 带凭证返回可解析的 Prometheus 文本
func TestMetricsReturnsPrometheusText(t *testing.T) {
	s, m := metricsServer(t)
	m.ReqTotal.Inc("demo", "allow", "200")
	m.Rejected.Inc("demo", "qps-guard", "biz")
	m.Latency.Observe(0.012, "demo", "allow")

	w := authGet(s.Handler(), "GET", "/admin/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != obs.ContentType {
		t.Errorf("Content-Type = %q，期望 %q", ct, obs.ContentType)
	}
	// 指标是即时值，任何中间层缓存都会让看板显示过期数据却毫无提示
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q，必须含 no-store", cc)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("缺少 X-Content-Type-Options: nosniff")
	}

	body := w.Body.String()
	for _, want := range []string{
		`unirate_requests_total{biz="demo",decision="allow",code="200"} 1`,
		`unirate_rejected_total{biz="demo",rule="qps-guard",dimension="biz"} 1`,
		"unirate_request_duration_seconds_bucket",
		"# TYPE unirate_request_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("响应缺少 %q", want)
		}
	}
}

// TestMetricsMatchesObsPortByteForByte admin 端点与 obs 端口必须是同一份数据。
// 若两处渲染逻辑分叉，看板与 Prometheus 会对同一时刻给出不同结论，
// 排障时无法判断该信哪个。
func TestMetricsMatchesObsPortByteForByte(t *testing.T) {
	s, m := metricsServer(t)
	m.ReqTotal.Add(7, "demo", "allow", "200")
	m.Watermark.Set(83, "demo", "qps-guard")

	admin := authGet(s.Handler(), "GET", "/admin/metrics")

	ow := httptest.NewRecorder()
	m.Handler().ServeHTTP(ow, httptest.NewRequest("GET", "/metrics", nil))

	// uptime 每次渲染都在变，逐字节比对时必须排除该行
	strip := func(s string) string {
		var keep []string
		for _, ln := range strings.Split(s, "\n") {
			if strings.HasPrefix(ln, "unirate_uptime_seconds ") {
				continue
			}
			keep = append(keep, ln)
		}
		return strings.Join(keep, "\n")
	}
	if strip(admin.Body.String()) != strip(ow.Body.String()) {
		t.Error("admin 端点与 obs 端口输出不一致 —— 两处渲染逻辑已分叉")
	}
	if admin.Header().Get("Content-Type") != ow.Header().Get("Content-Type") {
		t.Error("两端 Content-Type 不一致")
	}
}

// TestMetricsHeadHasNoBody HEAD 只返回头，不拉全量正文
func TestMetricsHeadHasNoBody(t *testing.T) {
	s, m := metricsServer(t)
	m.ReqTotal.Inc("demo", "allow", "200")

	w := authGet(s.Handler(), "HEAD", "/admin/metrics")
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD 期望 200，实际 %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 响应不应有正文，实际 %d 字节", w.Body.Len())
	}
	if w.Header().Get("Content-Type") != obs.ContentType {
		t.Error("HEAD 响应缺少 Content-Type")
	}
}

// TestMetricsRejectsWriteMethods 指标是只读资源，写方法必须 405 且给 Allow 头
func TestMetricsRejectsWriteMethods(t *testing.T) {
	s, _ := metricsServer(t)
	h := s.Handler()
	for _, mth := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		w := authGet(h, mth, "/admin/metrics")
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 期望 405，实际 %d", mth, w.Code)
		}
		if w.Header().Get("Allow") == "" {
			t.Errorf("%s 的 405 响应缺少 Allow 头", mth)
		}
	}
}

// TestMetricsNilRegistryReturns503 未注入指标时返回 503 而不是 panic
func TestMetricsNilRegistryReturns503(t *testing.T) {
	s := newTestServer(t, Options{Addr: ":0"})
	w := authGet(s.Handler(), "GET", "/admin/metrics")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Metrics 未注入时期望 503，实际 %d", w.Code)
	}
}

// TestMetricsConcurrentReadWithWrites 并发拉取 + 并发写入无数据竞争。
// 直读内存指标结构的前提就是它线程安全，这条在 -race 下验证该前提，
// 而不是靠读代码得出的判断。
func TestMetricsConcurrentReadWithWrites(t *testing.T) {
	s, m := metricsServer(t)
	h := s.Handler()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				m.ReqTotal.Inc("demo", "allow", "200")
				m.Latency.Observe(0.03, "demo", "allow")
				m.ConcInFlight.Add(1, "demo")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if w := authGet(h, "GET", "/admin/metrics"); w.Code != http.StatusOK {
					t.Errorf("并发拉取得到 %d", w.Code)
					return
				}
			}
		}()
	}
	wg.Wait()
}
