package adminui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/unirate/gateway/internal/obs"
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
	if !regexp.MustCompile(`lo:\s*prevBound,\s*hi:\s*bk\[i\]`).MatchString(src) {
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

// TestHistogramBucketLabelsMatchBackend le 标签的字符串形式必须跨语言一致。
//
// 后端用 strconv.FormatFloat(bound, 'g', -1, 64) 渲染 le 标签，前端用
// String(b) 生成查找键。两者恰好一致（都给最短往返表示），但这是巧合而非
// 契约：换成 %v、%.1f 或在 JS 侧用 toFixed，键就匹配不上了。
//
// 匹配不上的后果是 cumulativeOf 全取 0 → histQuantile 因 total<=0 返回
// null → KPI 卡显示「—」。整条链路不抛任何错误，看起来就像「没有流量」。
// 这类沉默失效正是本文件存在的理由，所以在这里把它钉死。
func TestHistogramBucketLabelsMatchBackend(t *testing.T) {
	// 直接取后端真实边界，而不是手写样本 —— 后端加桶时这条测试要自动覆盖到
	var bounds []float64
	bounds = append(bounds, obs.LatencyBounds...)
	bounds = append(bounds, obs.TTFTBounds...)
	for _, b := range bounds {
		got := strconv.FormatFloat(b, 'g', -1, 64)
		// JS String(Number) 的等价形式：无小数则无 .0，无指数（这些量级下）
		want := jsNumberString(b)
		if got != want {
			t.Errorf("le 标签跨语言不一致: Go=%q JS=%q（前端会静默取 0 → KPI 显示「—」）", got, want)
		}
	}
	// 前端桶边界字面量必须与后端逐值一致。后端改了桶而前端没跟，
	// cumulativeOf 对不上的那几档直接取 0，分位数偏低但仍是个「合理」数字。
	lit := stripJSComments(string(newH(t).files["metrics.js"]))
	assertBucketLiteral(t, lit, "TTFT_BUCKETS", obs.TTFTBounds)
	assertBucketLiteral(t, lit, "BUCKETS", obs.LatencyBounds)

	// 前端必须为 TTFT 显式传入 TTFT_BUCKETS：桶边界与指标不匹配是静默错误
	src := metricsSrc(t)
	if !regexp.MustCompile(`(?i)histQuantile\([^)]*ttft[^)]*TTFT_BUCKETS`).MatchString(src) {
		t.Error("TTFT 分位数未显式传 TTFT_BUCKETS —— 会用端到端桶边界解析，结果静默错误")
	}
	if !strings.Contains(src, "bounds || BUCKETS") {
		t.Error("histQuantile 未保留 bounds 默认值 —— 既有调用点会拿到 undefined 桶")
	}
}

// assertBucketLiteral 校验 JS 里的桶边界数组字面量与后端 bounds 逐值一致。
func assertBucketLiteral(t *testing.T, src, name string, want []float64) {
	t.Helper()
	// 匹配 var NAME = [ ... ];  —— 用 \b 避免 BUCKETS 误匹配 TTFT_BUCKETS
	re := regexp.MustCompile(`(?:^|[^_\w])` + name + `\s*=\s*\[([^\]]*)\]`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("未在 metrics.js 找到 %s 的数组字面量定义", name)
	}
	var got []string
	for _, p := range strings.Split(m[1], ",") {
		if p = strings.TrimSpace(p); p != "" {
			got = append(got, p)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s 桶数量与后端不一致: JS=%d 后端=%d（对不上的档位会静默取 0）",
			name, len(got), len(want))
	}
	for i, w := range want {
		if exp := strconv.FormatFloat(w, 'g', -1, 64); got[i] != exp {
			t.Errorf("%s[%d] 与后端不一致: JS=%q 后端=%q", name, i, got[i], exp)
		}
	}
}

// jsNumberString 复现 JS String(Number) 在测试所涉量级下的输出。
func jsNumberString(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	// Go 的 'g' 在 |exp| 较大时用 e 记法，JS 阈值不同；本用例区间内两者一致，
	// 若未来加入极端量级的桶边界，这里会先失败，提示需要重新对齐格式。
	if strings.ContainsAny(s, "eE") {
		return "«需要重新对齐: " + s + "»"
	}
	return s
}

// TestGaugeMetricsAreNotDifferentiated RPM/TPM 是 gauge，禁止参与差分。
//
// 后端已在滚动窗口内算好「每分钟」值，前端再对它做 (cur-prev)/dt 会得到
// 「每分钟值的变化速率」—— 稳态流量下恒为 0，看板会显示「零流量」。
// 反过来若把 counter 当 gauge 直接显示，会把进程累计值当成瞬时值。
// 两个方向都不报错，只是数字变成谎话。
func TestGaugeMetricsAreNotDifferentiated(t *testing.T) {
	src := metricsSrc(t)
	// rpm/tpm 必须直取当前值，不得出现 dRpm / dt 这类差分
	if regexp.MustCompile(`d(Rpm|Tpm|RPM|TPM)\s*/\s*dt`).MatchString(src) {
		t.Error("对 RPM/TPM gauge 做了速率差分 —— 稳态流量下会恒显示 0")
	}
	if !regexp.MustCompile(`rpm:\s*cur\.rpm`).MatchString(src) {
		t.Error("RPM 未直取 gauge 当前值")
	}
	if !regexp.MustCompile(`tpm:\s*cur\.tpm`).MatchString(src) {
		t.Error("TPM 未直取 gauge 当前值")
	}
	// 新增的 sseFrames / ttftCount 都是 counter，必须纳入重启检测，
	// 否则重启后 framesPerSec 出负尖刺、TTFT 用到负样本数
	if !strings.Contains(src, "cur.sseFrames < prev.sseFrames") {
		t.Error("sseFrames counter 未纳入重启检测 —— 网关重启后帧率会出现负尖刺")
	}
	if !strings.Contains(src, "cur.ttftCount < prev.ttftCount") {
		t.Error("ttftCount counter 未纳入重启检测 —— TTFT 分位数会用到负样本数")
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

// TestTrafficPanelBarScaleMatchesSortKey 流量面板的条形基准必须与排序键一致。
//
// RPM 与 TPM 差两三个数量级。面板按 rpm 降序取 Top10，条形宽度也必须以
// rows[0].rpm 为 peak。若有人把排序换成 tpm 却漏改 peak（或反之），
// 首行不再是最长条、宽度与数值失去对应关系 —— 图形在说谎，但不报任何错。
func TestTrafficPanelBarScaleMatchesSortKey(t *testing.T) {
	src := stripJSCode(string(newH(t).files["page-monitor.js"]))
	if !regexp.MustCompile(`rows\.sort\([\s\S]{0,40}?return\s+b\.rpm\s*-\s*a\.rpm`).MatchString(src) {
		t.Error("流量面板未按 rpm 降序排序")
	}
	if !regexp.MustCompile(`peak\s*=\s*rows\[0\]\.rpm`).MatchString(src) {
		t.Error("条形 peak 未取 rows[0].rpm —— 与排序键不一致时条形宽度失去意义")
	}
	if !regexp.MustCompile(`e\.rpm\s*/\s*peak`).MatchString(src) {
		t.Error("条形宽度未以 rpm/peak 计算")
	}
}
