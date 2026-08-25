package adminui

import (
	"regexp"
	"strings"
	"testing"
)

// 指标适配层的契约守护。
//
// 这里守的不是「代码能跑」，而是几条**语义正确性**约束 —— 它们坏掉时
// 界面照样渲染、不报任何错，只是数字变成谎话。这类沉默失效没法靠人眼
// review 稳定抓住，只能机械固定住。
//
// 之所以用 Go 测试扫 JS 源码（而不是跑 JS 单测）：本项目零构建链、
// 无 node 依赖，CI 里只有 Go 工具链。扫描能覆盖的正是最危险的那几处
// ——「有人把这段逻辑删了」，而这恰好是文本可判定的。

func metricsSrc(t *testing.T) string {
	t.Helper()
	h := newH(t)
	b, ok := h.files["metrics.js"]
	if !ok {
		t.Fatal("metrics.js 未嵌入")
	}
	return stripJSCode(string(b))
}

// TestFirstFrameStoresBaselineOnly 首帧必须只存基线、不出速率。
//
// counter 是单调累计值，首帧的绝对值不是速率。若这段被删掉，
// 看板会把「进程启动至今的总请求数」当成 QPS 显示 —— 数字大得离谱，
// 但没有任何报错。反过来若强行出图，首帧会显示 0，被读成「零流量」。
func TestFirstFrameStoresBaselineOnly(t *testing.T) {
	src := metricsSrc(t)
	// ingest 必须在无 prev 时提前 return，且 return false（表示无速率可用）
	if !regexp.MustCompile(`if\s*\(\s*!prev\s*\)\s*\{[^}]*store\.prev\s*=\s*cur;[^}]*return\s+false`).MatchString(src) {
		t.Error("首帧未做「只存基线并返回 false」处理 —— KPI 会把累计值当速率")
	}
}

// TestCounterResetDiscardsSample 计数器回退（网关重启）必须丢弃该次采样。
//
// 重启后 counter 归零，与上一帧做差得到巨大负值。若不判负，
// QPS 会出现一个负尖刺，而折线图的 y 轴会被这个负值拉爆、
// 整条曲线变成一条贴顶的直线 —— 看起来像「流量突然消失」。
func TestCounterResetDiscardsSample(t *testing.T) {
	src := metricsSrc(t)
	// 必须对四个 counter 差分同时判负
	if !regexp.MustCompile(`if\s*\(dReq\s*<\s*0\s*\|\|\s*dRej\s*<\s*0`).MatchString(src) {
		t.Error("未对 counter 差分判负 —— 网关重启会产生巨大负值尖刺")
	}
	// 判负后必须置 restarted 标志（UI 要提示「基线已重置」）并清掉 kpi
	if !strings.Contains(src, "store.restarted = true") {
		t.Error("检出计数器回退后未置 restarted 标志，界面不会提示基线重置")
	}
	if !regexp.MustCompile(`store\.restarted\s*=\s*true;[\s\S]{0,80}store\.kpi\s*=\s*null`).MatchString(src) {
		t.Error("检出回退后未把 kpi 置 null —— 会继续展示重启前的陈旧速率")
	}
	// 直方图桶同样要防负
	if !regexp.MustCompile(`d\s*<\s*0\s*\?\s*0\s*:\s*d`).MatchString(src) {
		t.Error("直方图桶差分未防负 —— 重启后分位数计算会用到负样本数")
	}
}

// TestRateRequiresDivisionByInterval 速率必须除以采样间隔。
//
// 若漏掉 /dt，"QPS" 实际是「本轮询周期的请求增量」—— 5 秒间隔下
// 数字恰好是真实 QPS 的 5 倍。这是最典型的沉默逻辑错误：
// 量级看着合理，只是系统性偏大，没人会立刻发现。
func TestRateRequiresDivisionByInterval(t *testing.T) {
	src := metricsSrc(t)
	for _, expr := range []string{"dReq / dt", "dRej / dt", "dSettled / dt"} {
		if !strings.Contains(src, expr) {
			t.Errorf("速率计算缺少 %q —— 速率未除采样间隔会系统性偏大", expr)
		}
	}
	// dt 必须由两帧时间戳算出，不能用轮询间隔常量：
	// 标签页被挂起、请求排队都会让实际间隔远大于设定值
	if !strings.Contains(src, "(cur.at - prev.at) / 1000") {
		t.Error("dt 未按两帧实际时间戳计算 —— 标签页挂起后速率会算错")
	}
	if !regexp.MustCompile(`if\s*\(dt\s*<=\s*0\)`).MatchString(src) {
		t.Error("未防 dt <= 0（时钟回拨 / 同一毫秒两次采样）会产生 Infinity")
	}
}

// TestQuantileReturnsBucketRange 分位数必须连所在桶区间一起返回。
//
// 分辨率上限由 bucket 边界决定，不由插值精度决定：桶区间宽度占其上界
// 50%-100%（(1s,2.5s] 那档宽 1500ms）。只返回一个数字，UI 就无从判断
// 该给几位有效数字，最终会显示 "2479.0 ms" 这种暗示 0.1ms 精度的值 ——
// 比真实分辨率高四个数量级，而 SRE 会拿它对 SLO。
func TestQuantileReturnsBucketRange(t *testing.T) {
	src := metricsSrc(t)
	if !regexp.MustCompile(`lo:\s*prevBound,\s*hi:\s*BUCKETS\[i\]`).MatchString(src) {
		t.Error("histQuantile 未返回所在桶区间 —— UI 无法表达真实分辨率")
	}
	if !strings.Contains(src, "exact: true") {
		t.Error("histQuantile 未区分「恰好命中桶边界」（无插值误差）的情况")
	}
	// 落 +Inf 桶时上界未知，必须是 Infinity 而不是最后一个有限边界，
	// 否则 "30000ms" 会被读成确定值，而真实延迟可能是它的若干倍
	if !strings.Contains(src, "hi: Infinity") {
		t.Error("超出最大桶时未把上界标为 Infinity —— 会把下界谎报成确定值")
	}
	// KPI 层必须真的消费这个区间
	kpi := stripJSComments(string(newH(t).files["monitor-kpi.js"]))
	if !strings.Contains(kpi, "p99Range") {
		t.Error("monitor-kpi.js 未消费 p99Range —— 精度信息被丢弃")
	}
	if !strings.Contains(kpi, "桶区间") {
		t.Error("KPI 卡未显示桶区间标注，用户看不到该数值的真实分辨率")
	}
}

// TestPollingPausesWhenHidden 后台标签页必须停止轮询。
// 否则一个忘记关的标签页会持续打受鉴权的指标端点。
func TestPollingPausesWhenHidden(t *testing.T) {
	src := stripJSCode(string(newH(t).files["page-monitor.js"]))
	if !strings.Contains(src, "document.hidden") {
		t.Error("未在 document.hidden 时暂停轮询")
	}
}

// TestMetricsEndpointIsSameOriginAuthenticated 指标必须走同源鉴权端点。
//
// obs 端口（29091）无鉴权且全网暴露；跨端口取数要开 CORS，
// 等于让任意网页读运行指标。这条边界一旦被「顺手改回去」就是安全回归。
func TestMetricsEndpointIsSameOriginAuthenticated(t *testing.T) {
	src := metricsSrc(t)
	// 端点路径是字符串字面量，必须用保留字符串的剥离器（见 scan_test.go 说明）
	lit := stripJSComments(string(newH(t).files["metrics.js"]))
	if !regexp.MustCompile(`ENDPOINT\s*=\s*'admin/metrics'`).MatchString(lit) {
		t.Error("指标端点不是同源相对路径 admin/metrics")
	}
	if !strings.Contains(lit, "Authorization") {
		t.Error("指标请求未注入 Bearer 凭证")
	}
	// 401 必须触发会话失效处理，否则令牌过期后看板会静默停更
	if !strings.Contains(src, "onSessionExpired") {
		t.Error("指标请求未处理 401 —— 令牌失效后看板会静默停止更新")
	}
}

// TestTopRejectRowNavigatesToRules 「拒绝率高 → 哪条规则拒的」必须一次点击到位。
// 设计师把这条定为全站最关键动线，它断掉不会报错，只是排障要多绕好几步。
func TestTopRejectRowNavigatesToRules(t *testing.T) {
	// 路由字面量在字符串里，用保留字符串的剥离器
	src := stripJSComments(string(newH(t).files["page-monitor.js"]))
	if !regexp.MustCompile(`#/rules\?biz=`).MatchString(src) {
		t.Error("Top10 行未跳转到目标 biz 的规则页")
	}
	if !strings.Contains(src, "encodeURIComponent") {
		t.Error("biz 名未做 URL 编码 —— 含特殊字符的 biz 名会拼出错误路由")
	}
}
