package admin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/unirate/gateway/internal/config"
	"github.com/unirate/gateway/internal/limiter"
	"github.com/unirate/gateway/internal/obs"
)

// Admin 管理面（对应评审 P0-3 修正）
//
// 原设计的致命缺陷：Spec §4.4 把 /admin/rules（含 POST/PUT/DELETE）
// 挂在与业务代理相同的端口，而 §2.1 又规定「URL 首段即 biz」——
// 这意味着任何外部调用方都能 POST /admin/rules 改写全局限流规则，
// 且 Spec 全文没有一个字提到 Admin 鉴权。这是一个可直接提权的高危漏洞。
//
// 本实现的修正（三重隔离）：
//  1. 网络隔离：Admin 监听独立端口，compose/K8s 中不对外暴露；
//  2. 身份隔离：Bearer Token 鉴权，常量时间比较防时序侧信道；
//     未配置 Token 时拒绝启动（不允许「默认无鉴权」这种配置陷阱）；
//  3. 来源隔离：可选 CIDR 白名单，双重保险。
//
// 另外所有变更强制写审计日志（Spec 完全缺失该要求）。

// Options Admin 配置
type Options struct {
	Addr string
	// Token 必填。空值直接拒绝启动，避免误开无鉴权管理面。
	Token string
	// AllowCIDRs 可选来源白名单，如 10.0.0.0/8
	AllowCIDRs []string
	// Metrics 可选。注入后 GET /admin/metrics 直读该内存结构，
	// 供控制台看板同源受鉴权取数；不注入则该端点返回 503。
	// 放在 Options 而非 New 的位置参数，是为了不破坏既有全部调用点。
	Metrics *obs.Metrics
}

// Server Admin 服务
type Server struct {
	db    *sql.DB
	store *config.Store
	log   *slog.Logger
	opt   Options
	// metrics 供受鉴权的 /admin/metrics 直读内存指标（见 metrics.go 的取舍说明）。
	// 可为 nil：此时该端点返回 503，其余端点不受影响。
	metrics *obs.Metrics

	tokenHash [32]byte
	allow     []*net.IPNet
	srv       *http.Server
}

// ErrNoToken 未配置 Admin 鉴权令牌
var ErrNoToken = fmt.Errorf(
	"admin token is required; refusing to start an unauthenticated admin api. %s", GenerateHint)

// New 创建 Admin 服务
func New(db *sql.DB, store *config.Store, log *slog.Logger, opt Options) (*Server, error) {
	// 令牌强度校验见 token.go：非空 + ≥32 字符 + 不在弱值黑名单。
	// 光有长度检查挡不住仓库里的公开占位值 —— 那是本项目实测存在过的真实缺陷。
	if err := ValidateToken(opt.Token); err != nil {
		return nil, err
	}
	s := &Server{db: db, store: store, log: log, opt: opt, metrics: opt.Metrics}
	s.tokenHash = sha256.Sum256([]byte(opt.Token))
	for _, c := range opt.AllowCIDRs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(c); err == nil {
			s.allow = append(s.allow, n)
		} else {
			return nil, fmt.Errorf("invalid admin allow cidr %q: %w", c, err)
		}
	}
	return s, nil
}

// Handler 构造路由
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// 中间件顺序：auth 必须在最外层。
	// 若把依赖守卫放外面，未鉴权的探测者能通过 503 与 401 的差异
	// 推断出后端依赖状态，属于信息泄露。
	// 中间件链固定为：auth → allowMethods → 依赖守卫 → handler
	mux.HandleFunc("/admin/bizs",
		s.auth(allowMethods(s.requireDB(s.handleBizs), "GET", "POST", "PUT")))
	mux.HandleFunc("/admin/bizs/",
		s.auth(allowMethods(s.requireDB(s.handleBizItem), "DELETE")))
	mux.HandleFunc("/admin/reload",
		s.auth(allowMethods(s.requireStore(s.handleReload), "POST")))
	mux.HandleFunc("/admin/snapshot",
		s.auth(allowMethods(s.requireStore(s.handleSnapshot), "GET")))
	mux.HandleFunc("/admin/audit",
		s.auth(allowMethods(s.requireDB(s.handleAudit), "GET")))
	// validate 是纯函数式校验，不依赖任何外部资源
	mux.HandleFunc("/admin/rules/validate",
		s.auth(allowMethods(s.handleValidate, "POST")))
	// Tier 1 运行策略：GET 读三态（只需 store），PUT 写需要 SoT。
	// 两者共用一个 mux 项，因此守卫用 requireStore + requireDB 组合 ——
	// 顺序上 DB 守卫只包住写路径，避免 MySQL 抖动时连"看一眼配置"都做不到。
	mux.HandleFunc("/admin/policy",
		s.auth(allowMethods(s.requireStore(s.handlePolicy), "GET", "PUT")))
	mux.HandleFunc("/admin/policy/validate",
		s.auth(allowMethods(s.handlePolicyValidate, "POST")))
	// 受鉴权的指标端点，供控制台看板同源取数（取舍全文见 metrics.go 顶部）。
	// 不依赖 DB/store，只需 requireMetrics；放行 HEAD 便于探活时不拉全量正文。
	mux.HandleFunc("/admin/metrics",
		s.auth(allowMethods(s.requireMetrics(s.handleMetrics), "GET", "HEAD")))
	// 内嵌控制台的静态资产。挂在 "/" 上，刻意排在最后 ——
	// 上面所有 /admin/* 模式都更具体，ServeMux 的最长前缀匹配保证它们优先命中。
	// 鉴权边界的取舍见 ui.go 顶部注释。
	s.mountUI(mux)
	return mux
}

// Start 启动独立监听
func (s *Server) Start() error {
	s.srv = &http.Server{
		Addr:              s.opt.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	s.log.Info("admin api listening", "addr", s.opt.Addr, "cidr_allowlist", len(s.allow))
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown 优雅关闭
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// auth 鉴权中间件
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.allow) > 0 && !s.ipAllowed(r) {
			s.log.Warn("admin access denied by cidr", "remote", r.RemoteAddr, "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "source not allowed"})
			return
		}
		got := sha256.Sum256([]byte(extractToken(r)))
		// 常量时间比较：防止通过响应时间差逐字节爆破令牌
		if subtle.ConstantTimeCompare(got[:], s.tokenHash[:]) != 1 {
			s.log.Warn("admin auth failed", "remote", r.RemoteAddr, "path", r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Bearer realm="unirate-admin"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// allowMethods 方法校验中间件。
//
// 必须排在依赖守卫之前：请求方法是否合法是请求自身的属性，
// 与后端依赖是否就绪无关。若顺序颠倒，一个 PATCH 请求在 DB 未就绪时
// 会得到 503，掩盖了「这个方法根本不被支持」的真实原因。
func allowMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range methods {
			if r.Method == m {
				next(w, r)
				return
			}
		}
		w.Header().Set("Allow", strings.Join(methods, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// requireStore / requireDB 依赖守卫。
// 管理面在配置尚未 Bootstrap 完成、或 MySQL 不可达时仍可能收到请求，
// 此时应返回 503 让调用方重试，而不是 nil 解引用把整个管理面打挂。
func (s *Server) requireStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.store == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "config store not ready"})
			return
		}
		next(w, r)
	}
}

func (s *Server) requireDB(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.db == nil {
			writeJSON(w, http.StatusServiceUnavailable,
				map[string]string{"error": "database not available"})
			return
		}
		next(w, r)
	}
}

// extractToken 严格解析凭证。
//
// 这里曾经有一个真实漏洞：strings.TrimPrefix(auth, "Bearer ") 在前缀不存在时
// 会原样返回整个头部值，于是畸形请求 `Authorization: <token>`（不带任何认证方案）
// 会被当作合法 Bearer 令牌放行。凭证解析必须显式校验方案名，不能靠 TrimPrefix
// 的「碰巧截掉了」来判定。
//
// 规则：
//   - Authorization 头必须严格形如 `Bearer <token>`，方案名大小写不敏感（RFC 7235）；
//   - 其他方案（Basic 等）或缺失方案名一律视为无凭证；
//   - 仅当 Authorization 头完全缺失时，才回退到 X-Admin-Token。
//     若 Authorization 存在但非法，不允许回退 —— 否则等于给攻击者多一次尝试机会。
func extractToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return strings.TrimSpace(r.Header.Get("X-Admin-Token"))
	}
	scheme, rest, found := strings.Cut(auth, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func (s *Server) ipAllowed(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range s.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.LoadFromMySQL(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.log.Info("config reloaded via admin", "version", snap.Version, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "reloaded", "config_version": snap.Version, "bizs": len(snap.Bizs)})
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Current())
}

// handleValidate 规则试算：不落库，只校验。便于 CI 与前端做即时反馈。
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var rules []*limiter.Rule
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rules); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	problems := []map[string]string{}
	for i, rule := range rules {
		if err := rule.Validate(); err != nil {
			problems = append(problems, map[string]string{
				"index": fmt.Sprint(i), "name": rule.Name, "error": err.Error()})
		}
	}
	status := http.StatusOK
	if len(problems) > 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"valid": len(problems) == 0, "checked": len(rules), "problems": problems})
}
