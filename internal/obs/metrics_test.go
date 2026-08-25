package obs

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestMetricsExpositionFormat 指标输出必须严格符合 Prometheus 文本格式。
// 格式错误会让 Prometheus 静默丢弃整个抓取结果 —— 监控看似正常实则无数据。
func TestMetricsExpositionFormat(t *testing.T) {
	m := NewMetrics()
	m.ReqTotal.Inc("demo", "pass", "200")
	m.ReqTotal.Inc("demo", "reject", "429")
	m.Rejected.Inc("demo", "ip-qps", "biz.ip")
	m.Latency.Observe(0.012, "demo", "pass")
	m.ConcInFlight.Set(3, "demo")
	m.BreakerOpen.Set(0)
	m.ConfigVersion.Set(42)

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/plain") || !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("wrong Content-Type: %q", ct)
	}

	body := w.Body.String()

	// 每个指标族必须有 HELP 与 TYPE
	for _, name := range []string{
		"unirate_requests_total", "unirate_rejected_total",
		"unirate_request_duration_seconds", "unirate_concurrency_in_flight",
		"unirate_redis_breaker_open", "unirate_config_version",
	} {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("missing HELP for %s", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("missing TYPE for %s", name)
		}
	}

	// 无标签 gauge 必须始终输出（哪怕值为 0），否则告警的 absent() 无法区分
	// 「指标缺失」与「一切正常」
	if !strings.Contains(body, "unirate_redis_breaker_open 0\n") {
		t.Error("zero-valued gauge must still be exposed")
	}
	if !strings.Contains(body, "unirate_config_version 42\n") {
		t.Error("config_version not exposed correctly")
	}

	// 带标签样本格式
	if !strings.Contains(body, `unirate_requests_total{biz="demo",decision="pass",code="200"} 1`) {
		t.Errorf("labeled counter format wrong:\n%s", extract(body, "unirate_requests_total"))
	}

	// 直方图必须含 _bucket / _sum / _count 且有 +Inf 桶
	if !strings.Contains(body, `unirate_request_duration_seconds_bucket{biz="demo",decision="pass",le="+Inf"}`) {
		t.Error("histogram missing +Inf bucket")
	}
	if !strings.Contains(body, "unirate_request_duration_seconds_sum{") ||
		!strings.Contains(body, "unirate_request_duration_seconds_count{") {
		t.Error("histogram missing _sum/_count")
	}

	// 不得出现空行内的裸标签或未闭合大括号
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, "{") != strings.Count(line, "}") {
			t.Errorf("unbalanced braces: %q", line)
		}
		if !strings.Contains(line, " ") {
			t.Errorf("sample without value: %q", line)
		}
	}
}

func extract(body, prefix string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// TestHistogramBucketsCumulative Prometheus 直方图桶必须是累积的
func TestHistogramBucketsCumulative(t *testing.T) {
	m := NewMetrics()
	for _, v := range []float64{0.0005, 0.003, 0.02, 0.4, 8, 100} {
		m.Latency.Observe(v, "b", "pass")
	}
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))

	var prev int64 = -1
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "unirate_request_duration_seconds_bucket") {
			continue
		}
		f := strings.Fields(line)
		var v int64
		for _, ch := range f[len(f)-1] {
			if ch >= '0' && ch <= '9' {
				v = v*10 + int64(ch-'0')
			}
		}
		if v < prev {
			t.Fatalf("buckets must be cumulative, got %d after %d in %q", v, prev, line)
		}
		prev = v
	}
	// +Inf 桶必须等于总观测数
	if prev != 6 {
		t.Errorf("+Inf bucket should equal total count 6, got %d", prev)
	}
}

// TestMetricsConcurrentSafe 指标写入必须并发安全（网关是高并发场景）
func TestMetricsConcurrentSafe(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.ReqTotal.Inc("biz", "pass", "200")
				m.Latency.Observe(0.01, "biz", "pass")
				m.ConcInFlight.Add(1, "biz")
				m.ConcInFlight.Add(-1, "biz")
			}
		}(i)
	}
	// 并发读取不得 panic 或撕裂
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				w := httptest.NewRecorder()
				m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
			}
		}()
	}
	wg.Wait()

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `unirate_requests_total{biz="biz",decision="pass",code="200"} 5000`) {
		t.Errorf("lost counter increments:\n%s", extract(w.Body.String(), "unirate_requests_total"))
	}
	if !strings.Contains(w.Body.String(), `unirate_concurrency_in_flight{biz="biz"} 0`) {
		t.Error("gauge did not return to zero")
	}
}

// TestLabelEscaping 标签值中的特殊字符必须转义，否则会破坏抓取
func TestLabelEscaping(t *testing.T) {
	m := NewMetrics()
	m.Rejected.Inc("demo", `rule"with\quote`, "biz")
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if strings.Contains(body, `rule"with`) && !strings.Contains(body, `rule\"with`) {
		t.Errorf("quotes must be escaped:\n%s", extract(body, "unirate_rejected_total"))
	}
}
