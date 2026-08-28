package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/unirate/gateway/internal/config"
)

// Tier 1 运行策略管理端点（见 docs/decisions/CONFIG-TIERING.md）
//
// 三个端点，职责不重叠：
//
//	GET  /admin/policy          读取三态（生效值 / 默认值 / 是否被 env 显式设置）
//	PUT  /admin/policy          写入覆盖项（写入前校验 + 同事务审计）
//	POST /admin/policy/validate 只校验不落库，供前端即时反馈
//
// 写入必须先校验：非法值一旦进 SoT 就会被发布到所有实例，那时再拒绝已经晚了。
// 这与 /admin/rules/validate 的设计思路一致。

// handlePolicy GET 读 / PUT 写
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.getPolicy(w, r)
		return
	}
	s.putPolicy(w, r)
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.policyView())
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	var p policyPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if len(p.Values) == 0 && len(p.Reset) == 0 {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "nothing to do: provide values or reset"})
		return
	}

	vals, decodeProblems := decodePolicyValues(p.Values)
	if len(decodeProblems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid config values", "problems": decodeProblems})
		return
	}
	// 校验必须在写入前完成，非法值绝不允许进 SoT
	if problems := config.ValidatePolicyOverrides(vals); len(problems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid config values", "problems": problems})
		return
	}
	// reset 的键也要校验存在性，否则拼错键名会静默无效
	resetProblems := map[string]string{}
	for _, k := range p.Reset {
		if _, ok := config.PolicySpecOf(k); !ok {
			resetProblems[k] = "unknown config key; allowed: " +
				strings.Join(config.PolicyKeys, ", ")
		}
	}
	if len(resetProblems) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid reset keys", "problems": resetProblems})
		return
	}

	// DB 守卫排在全部请求校验**之后**，与 allowMethods 排在依赖守卫之前
	// 是同一条理由：请求是否合法是请求自身的属性，与后端是否就绪无关。
	// 顺序颠倒会让一个越界值在 MySQL 抖动时得到 503，
	// 掩盖了「这个值根本不被接受」的真实原因。
	//
	// 守卫放在这里而不是中间件层：GET 只读本地快照，
	// MySQL 抖动时仍应能查看当前配置 —— 那恰恰是最需要看配置的时候。
	if s.db == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "database not available"})
		return
	}

	tx, txCtx, cancel, err := s.db.BeginWrite(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer cancel()
	defer func() { _ = tx.Rollback() }()

	for _, k := range sortedKeys(vals) {
		if _, err := tx.ExecContext(txCtx,
			`INSERT INTO runtime_config (cfg_key, cfg_value, operator)
			 VALUES (?, ?, ?)`+
				s.db.UpsertSuffix([]string{"cfg_key"}, []string{"cfg_value", "operator"}),
			k, vals[k], operatorOf(r, p.Operator),
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	for _, k := range p.Reset {
		if _, err := tx.ExecContext(txCtx,
			`DELETE FROM runtime_config WHERE cfg_key = ?`, k); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	// 审计与变更同事务：管理面无审计等于无问责
	detail, _ := json.Marshal(map[string]any{"set": vals, "reset": p.Reset})
	if err := s.audit(txCtx, r, tx, "update_policy", "_", p.Operator, detail); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	snap, err := s.store.LoadFromMySQL(r.Context())
	if err != nil {
		// 已落 SoT，只是发布失败；其它实例靠轮询兜底最终会拉到
		s.log.Error("publish policy after update failed", "err", err)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "saved_but_publish_failed", "error": err.Error()})
		return
	}
	resp := s.policyView()
	resp["status"] = "ok"
	resp["config_version"] = snap.Version
	writeJSON(w, http.StatusOK, resp)
}

// handlePolicyValidate 只校验不落库，给前端做即时反馈
func (s *Server) handlePolicyValidate(w http.ResponseWriter, r *http.Request) {
	var p policyPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	vals, problems := decodePolicyValues(p.Values)
	for k, v := range config.ValidatePolicyOverrides(vals) {
		problems[k] = v
	}
	status := http.StatusOK
	if len(problems) > 0 {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"valid": len(problems) == 0, "checked": len(p.Values), "problems": problems})
}
