package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/unirate/gateway/internal/config"
	"github.com/unirate/gateway/internal/limiter"
)

// 业务域（biz）配置的读写处理器。
//
// 从 server.go 拆出：原文件 544 行，同时承担「服务装配 + 中间件链 + 鉴权」
// 与「biz 这一个资源的 CRUD」两件事。前者是全局骨架、几乎不变，
// 后者随业务字段增减而频繁改动，混在一个文件里会让每次加字段都碰到鉴权代码。
// 拆分后 server.go 只剩骨架与共用工具，本文件只管 biz 资源。
//
// 事务与审计的耦合是刻意的：配置变更与审计记录同一个事务提交，
// 保证「有变更必有记录」—— 详见 audit 的注释。
// bizPayload Admin 写入的业务配置
type bizPayload struct {
	Biz             string                `json:"biz"`
	BaseURL         string                `json:"base_url"`
	StripPathPrefix bool                  `json:"path_strip_prefix"`
	Enabled         *bool                 `json:"enabled"`
	Rules           []*limiter.Rule       `json:"rules"`
	Metering        *config.TokenMetering `json:"token_metering"`
	Operator        string                `json:"operator"`
}

func (s *Server) handleBizs(w http.ResponseWriter, r *http.Request) {
	// 方法白名单已由 allowMethods 中间件保证
	if r.Method == http.MethodGet {
		s.listBizs(w, r)
		return
	}
	s.upsertBiz(w, r)
}

func (s *Server) listBizs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT biz, base_url, path_strip_prefix, enabled, rules_json, metering_json, updated_at
		 FROM biz_config ORDER BY biz`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var (
			biz, baseURL string
			strip, en    bool
			rulesJSON    sql.NullString
			meterJSON    sql.NullString
			updated      sql.NullTime
		)
		if err := rows.Scan(&biz, &baseURL, &strip, &en, &rulesJSON, &meterJSON, &updated); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		item := map[string]any{
			"biz": biz, "base_url": baseURL,
			"path_strip_prefix": strip, "enabled": en,
		}
		if rulesJSON.Valid {
			var rs []*limiter.Rule
			if json.Unmarshal([]byte(rulesJSON.String), &rs) == nil {
				item["rules"] = rs
			}
		}
		if meterJSON.Valid && meterJSON.String != "" {
			var m config.TokenMetering
			if json.Unmarshal([]byte(meterJSON.String), &m) == nil {
				item["token_metering"] = m
			}
		}
		if updated.Valid {
			item["updated_at"] = updated.Time
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

func (s *Server) upsertBiz(w http.ResponseWriter, r *http.Request) {
	var p bizPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
		return
	}
	if p.Biz == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "biz required"})
		return
	}
	// 配置校验必须在写入前完成，绝不允许非法规则进入 SoT
	for _, rule := range p.Rules {
		if err := rule.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "invalid rule: " + err.Error()})
			return
		}
	}
	if p.BaseURL == "" && p.Biz != "*" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_url required"})
		return
	}

	rulesJSON, _ := json.Marshal(p.Rules)
	var meterJSON []byte
	if p.Metering != nil {
		meterJSON, _ = json.Marshal(p.Metering)
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO biz_config (biz, base_url, path_strip_prefix, enabled, rules_json, metering_json)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   base_url=VALUES(base_url), path_strip_prefix=VALUES(path_strip_prefix),
		   enabled=VALUES(enabled), rules_json=VALUES(rules_json),
		   metering_json=VALUES(metering_json)`,
		p.Biz, p.BaseURL, p.StripPathPrefix, enabled, string(rulesJSON), nullStr(meterJSON),
	); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 审计日志与配置变更同事务提交，保证「有变更必有记录」
	if err := s.audit(r, tx, "upsert_biz", p.Biz, p.Operator, rulesJSON); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	snap, err := s.store.LoadFromMySQL(r.Context())
	if err != nil {
		// 已落 SoT，只是发布失败；网关轮询兜底最终会拉到
		s.log.Error("publish config after upsert failed", "err", err)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status": "saved_but_publish_failed", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "biz": p.Biz, "config_version": snap.Version, "rules": len(p.Rules)})
}

func (s *Server) handleBizItem(w http.ResponseWriter, r *http.Request) {
	biz := strings.TrimPrefix(r.URL.Path, "/admin/bizs/")
	if biz == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "biz required"})
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(r.Context(), `DELETE FROM biz_config WHERE biz = ?`, biz)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "biz not found"})
		return
	}
	if err := s.audit(r, tx, "delete_biz", biz, r.Header.Get("X-Operator"), nil); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	snap, _ := s.store.LoadFromMySQL(r.Context())
	ver := int64(0)
	if snap != nil {
		ver = snap.Version
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "biz": biz, "config_version": ver})
}
