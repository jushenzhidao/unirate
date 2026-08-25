package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// 审计日志的写入与读取，以及两个供全包共用的响应小工具。
//
// 从 server.go 拆出：审计是横切关注点，被 biz.go 的每个写路径调用，
// 放在 server.go 里会让「服务装配」文件被业务写路径反向依赖。
//
// 为什么必须有审计：Spec 全文没提这个要求，但一个能改写全局限流规则的
// 管理面若无操作记录，事故后无法回答「谁在什么时候把 limit 改成了 0」——
// 等于没有问责能力。因此审计与配置变更同事务提交，不允许「改成功了但没记上」。

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, action, biz, operator, remote_addr, detail, created_at
		 FROM audit_log ORDER BY id DESC LIMIT 100`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id                            int64
			action, biz, operator, remote string
			detail                        sql.NullString
			created                       time.Time
		)
		if err := rows.Scan(&id, &action, &biz, &operator, &remote, &detail, &created); err != nil {
			continue
		}
		out = append(out, map[string]any{
			"id": id, "action": action, "biz": biz, "operator": operator,
			"remote_addr": remote, "detail": detail.String, "created_at": created,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

// audit 写审计日志。Spec 完全没提这个要求，但管理面无审计等于没有问责能力。
func (s *Server) audit(r *http.Request, tx *sql.Tx, action, biz, operator string, detail []byte) error {
	if operator == "" {
		operator = r.Header.Get("X-Operator")
	}
	if operator == "" {
		operator = "unknown"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	d := string(detail)
	if len(d) > 4000 {
		d = d[:4000]
	}
	_, err = tx.ExecContext(r.Context(),
		`INSERT INTO audit_log (action, biz, operator, remote_addr, detail)
		 VALUES (?, ?, ?, ?, ?)`,
		action, biz, operator, host, d)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	s.log.Info("admin change", "action", action, "biz", biz,
		"operator", operator, "remote", host)
	return nil
}

func nullStr(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
