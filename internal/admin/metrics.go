package admin

import (
	"net/http"

	"github.com/unirate/gateway/internal/obs"
)

// 受鉴权的指标端点（GET /admin/metrics）。
//
// ── 为什么要有这个端点 ───────────────────────────────────────────────
// 控制台的监控看板需要指标数据，但不能从 obs 端口（29091）取：
// 该端口无鉴权且绑 0.0.0.0，跨端口取数要在无鉴权端口开 CORS，
// 等于让任意网页读到含 biz 名 / 规则名 / 拒绝分布的内部拓扑。
// 且同一界面三个模块要令牌、监控模块裸奔，鉴权边界不成立。
// 因此看板同源取数，走与其他 /admin/* 完全一致的 auth 中间件链。
// obs 端口职责不变，继续专供 Prometheus 抓取。
//
// ── 返回形态：原样 Prometheus 文本，不做后端预聚合 ──────────────────
// 考虑过在后端算好 QPS / 拒绝率 / P99 再返回 JSON，否决理由是**正确性**，
// 不是"前端已经写好了"：
//
//	速率 = counter 一阶差分 ÷ 采样间隔，算它必须持有上一次采样值。
//	网关是多实例部署（限流的全部意义就在于跨实例共享配额），
//	把采样状态放进某个实例的内存，看板每次轮询命中不同实例就会拿到
//	基于不同基线算出的速率 —— 数字会在几个值之间跳动，且无法判断
//	哪个是对的。要做对就得把采样状态外置到 Redis 并处理并发写，
//	为了"少让前端算一次减法"引入一个有状态组件，代价与收益完全不成比例。
//
//	而差分放在浏览器里，状态天然属于"这一个看板会话"：
//	刷新间隔就是它自己的间隔，多标签页各算一份也各自自洽。
//	多实例只会让计数看起来"跳"一次，前端已有的负值丢弃 + 基线重置分支
//	正好覆盖这种情况（见 metrics.js 的 restarted 分支）。
//
// 所以选 A：透传文本。前端 metrics.js 的解析 / 差分 / 首帧基线 / 负值丢弃
// 没有一行白写 —— 那些逻辑本来就该在消费端，不该在网关里。
// 后端不返回 P99，也就不存在"给一个看起来精确却不说误差范围的数字"的问题；
// 桶边界由 obs.LatencyBounds 单点定义，前端 BUCKETS 与之对齐。
//
// ── 数据来源：直读内存，不 HTTP 自调 obs 端口 ───────────────────────
// 自调会引入一次本地网络往返 + 一次序列化 + 一次解析，还要在代码里写死
// obs 的监听地址（而它可配置），并且让 admin 的可用性依赖 obs 端口是否起来。
// 直读 obs.Metrics 已确认并发安全：每个 vec 的 write 在自身 RWMutex 读锁内
// 遍历 map，取值走 atomic.Load，与请求路径上的 Add/Set/Observe 并发无竞争
// （obs/metrics.go:39-53、57-72、174-207；说明见 obs/registry.go 的 Render 注释）。
// 唯一弱一致性是跨 vec 非同一瞬间快照，误差 ≤ 一个采样间隔内的增量，
// 与 Prometheus 抓取本身的语义相同。
//
// ── 依赖守卫 ────────────────────────────────────────────────────────
// 指标不依赖 SoT 也不依赖 config store，因此不套 requireDB / requireStore，
// 只需 requireMetrics 处理"网关以无指标模式装配"这一种情况（m == nil）。
// 中间件顺序仍是 auth → allowMethods → 依赖守卫，与既有端点一致：
// auth 在最外层，未鉴权者无法通过 401 / 503 的差异探测后端状态。

// requireMetrics 指标依赖守卫。
//
// 与 requireDB / requireStore 同构：指标集合可能未注入（例如仅跑管理面的
// 测试装配），此时返回 503 让调用方重试，而不是 nil 解引用把管理面打挂。
func (s *Server) requireMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "metrics registry not available"})
			return
		}
		next(w, r)
	}
}

// handleMetrics 返回与 obs 端口 /metrics 逐字节一致的 Prometheus 文本。
//
// 刻意不加 Cache-Control 之外的任何加工：这是一份原始观测数据，
// 任何"帮忙"的聚合都会让消费方失去自行判断误差的能力。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body := s.metrics.Render()
	h := w.Header()
	h.Set("Content-Type", obs.ContentType)
	// 指标是即时值，中间层缓存会让看板显示过期数据却毫无提示。
	h.Set("Cache-Control", "no-store")
	// 该响应含内部拓扑（biz 名 / 规则名），不应被任何嗅探式内容协商改写。
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(body))
}
