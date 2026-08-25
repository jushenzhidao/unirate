package admin

import (
	"net/http"
	"strings"

	"github.com/unirate/gateway/internal/adminui"
)

// 内嵌管理控制台的静态挂载。
//
// 鉴权边界的取舍（这是本文件唯一需要想清楚的事）：
//
// 控制台是纯静态资产 + 前端 hash 路由。登录页本身**必须允许未鉴权访问** ——
// 否则用户没有任何界面可以输入令牌，等于把管理面锁死。
// 但这不构成信息泄露：这些文件在编译期固定、对所有部署完全一致，
// 不含令牌、不含配置、不含任何运行时数据；它们能读到的一切都还要过
// /admin/* 的 s.auth 才拿得到。反过来说，把静态壳也塞进 s.auth 才是错的：
// 浏览器不会为 <link>/<script> 自动带 Bearer 头，页面必然加载失败。
//
// 因此边界划在「数据」而不是「壳」上：
//   - 静态资产（/、*.html、*.css、*.js、*.svg）：不鉴权，只读，无数据；
//   - 全部 /admin/* 数据端点：一律走既有 s.auth 中间件链，一个不漏。
//
// 另外刻意不动 server.go 里 auth → allowMethods → 依赖守卫 的顺序：
// auth 在最外层是为了让未鉴权者无法通过 401 与 503 的状态码差异探测
// 后端依赖状态（见 server_test.go 的 TestUnauthenticatedProbeCannotDetectBackendState）。
// 本文件只新增一个不冲突的兜底路由，不重排任何既有链路。
func (s *Server) mountUI(mux *http.ServeMux) {
	h, err := adminui.NewHandler()
	if err != nil {
		// 资产在编译期内嵌，读不出来说明构建产物损坏。
		// 此时不影响 API 可用性，因此只告警不阻断启动 ——
		// 管理面的核心职责是 API，控制台是它的一层皮。
		s.log.Error("admin ui assets unavailable, console disabled", "err", err)
		return
	}
	// 注册在 "/" 上：ServeMux 的最长前缀匹配保证已注册的 /admin/* 端点仍命中
	// 各自更具体的模式，不会被这里截走。
	//
	// 但 "/" 会兜住**未注册**的 /admin/xxx（如拼错的端点），若直接交给静态
	// 处理器，它们会拿到 200 + index.html。这会掩盖拼写错误，让调用方以为
	// 端点存在只是返回了怪东西。API 命名空间下的未知路径必须是 404。
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/") || r.URL.Path == "/admin" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such admin endpoint"})
			return
		}
		h.ServeHTTP(w, r)
	}))
	s.log.Info("admin console mounted", "path", "/")
}
