package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// 建表与方言差异。
//
// 设计取舍：不引入 migration 框架（golang-migrate / goose）。
// 理由：只有三张表，且明确不需要兼容旧版本 —— 幂等 CREATE TABLE IF NOT EXISTS
// 足以覆盖「全新部署」与「重启」两个场景，多一个框架就多一份供应链与
// 版本表状态机需要维护。若将来出现破坏性 schema 变更，再引入。

// identRe 限定可拼入 SQL 的标识符形态：小写字母、数字、下划线，首字符非数字。
// 本仓库所有表名列名都符合该约定。
var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// assertIdents 校验列名是否为安全标识符，不合规直接 panic。
//
// 为什么是 panic 而不是返回 error：列名在本仓库全部是源码字面量，
// 不合规只可能是编码错误，不是运行时可恢复的输入问题。让它在
// 首次调用（即启动后第一次配置写入）就炸掉，比返回 error 被
// 上层包装成 500 更容易定位。
//
// 这道校验同时是 gosec G202 的实质依据：拼入 SQL 的部分被约束为
// 标识符集合，注入载荷（引号、分号、空格、注释符）无法通过。
func assertIdents(cols []string) {
	for _, c := range cols {
		if !identRe.MatchString(c) {
			panic(fmt.Sprintf("store: unsafe SQL identifier %q", c))
		}
	}
}

// UpsertSuffix 返回 upsert 的冲突处理子句。
//
// 这是 MySQL 与 SQLite 唯一的语法分歧点。抽出为方法而非在调用点
// switch，是为了让新增 upsert 的人无法忘记处理方言。
//
// keyCols 为唯一键列（SQLite 需显式声明冲突目标，MySQL 不需要），
// updCols 为冲突时要覆盖的列。
//
// 两者均只接受源码字面量列名，由 assertIdents 强制。值一律走 ?
// 占位符，绝不拼接。
func (db *DB) UpsertSuffix(keyCols, updCols []string) string {
	assertIdents(keyCols)
	assertIdents(updCols)

	var sb strings.Builder
	switch db.Kind {
	case KindMySQL:
		sb.WriteString(" ON DUPLICATE KEY UPDATE ")
		for i, c := range updCols {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s=VALUES(%s)", c, c)
		}
	default: // SQLite
		fmt.Fprintf(&sb, " ON CONFLICT(%s) DO UPDATE SET ", strings.Join(keyCols, ", "))
		for i, c := range updCols {
			if i > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s=excluded.%s", c, c)
		}
	}
	return sb.String()
}

// Migrate 幂等建表
func (db *DB) Migrate(ctx context.Context) error {
	stmts := sqliteDDL
	if db.Kind == KindMySQL {
		stmts = mysqlDDL
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: %w\nstatement: %s", err, s)
		}
	}
	return nil
}

// sqliteDDL SQLite 建表语句。
//
// 与 MySQL 版的实质差异：
//   - AUTOINCREMENT 需配合 INTEGER PRIMARY KEY
//   - 无 JSON 类型，用 TEXT（SQLite 的 JSON1 扩展仍可查询 TEXT 中的 JSON）
//   - BOOLEAN 存为 INTEGER 0/1
//   - CURRENT_TIMESTAMP 语义一致
var sqliteDDL = []string{
	`CREATE TABLE IF NOT EXISTS biz_config (
		biz               TEXT    PRIMARY KEY,
		base_url          TEXT    NOT NULL,
		path_strip_prefix INTEGER NOT NULL DEFAULT 0,
		enabled           INTEGER NOT NULL DEFAULT 1,
		rules_json        TEXT    NOT NULL,
		metering_json     TEXT,
		created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	`CREATE TABLE IF NOT EXISTS audit_log (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		action      TEXT NOT NULL,
		biz         TEXT NOT NULL DEFAULT '',
		operator    TEXT NOT NULL DEFAULT 'unknown',
		remote_addr TEXT NOT NULL DEFAULT '',
		detail      TEXT,
		created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// 审计查询恒为「按时间倒序取最近 N 条」，覆盖索引直接服务该模式
	`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_biz ON audit_log(biz, id DESC)`,

	`CREATE TABLE IF NOT EXISTS runtime_config (
		cfg_key    TEXT PRIMARY KEY,
		cfg_value  TEXT NOT NULL,
		operator   TEXT NOT NULL DEFAULT 'unknown',
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,

	// SQLite 无 ON UPDATE CURRENT_TIMESTAMP，用触发器补齐。
	// 不补的后果：updated_at 永远是插入时间，配置变更时间无从追溯。
	`CREATE TRIGGER IF NOT EXISTS trg_biz_config_updated
		AFTER UPDATE ON biz_config FOR EACH ROW
		BEGIN
			UPDATE biz_config SET updated_at = CURRENT_TIMESTAMP WHERE biz = OLD.biz;
		END`,

	`CREATE TRIGGER IF NOT EXISTS trg_runtime_config_updated
		AFTER UPDATE ON runtime_config FOR EACH ROW
		BEGIN
			UPDATE runtime_config SET updated_at = CURRENT_TIMESTAMP WHERE cfg_key = OLD.cfg_key;
		END`,
}

// mysqlDDL MySQL 建表语句，与 deploy/mysql/init.sql 保持一致。
// 保留是为了让已有 MySQL 集群的用户可以只改 DSN 而无需改部署。
var mysqlDDL = []string{
	`CREATE TABLE IF NOT EXISTS biz_config (
		biz               VARCHAR(64)  NOT NULL PRIMARY KEY,
		base_url          VARCHAR(512) NOT NULL,
		path_strip_prefix TINYINT(1)   NOT NULL DEFAULT 0,
		enabled           TINYINT(1)   NOT NULL DEFAULT 1,
		rules_json        JSON         NOT NULL,
		metering_json     JSON         NULL,
		created_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at        TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS audit_log (
		id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
		action      VARCHAR(64)  NOT NULL,
		biz         VARCHAR(64)  NOT NULL DEFAULT '',
		operator    VARCHAR(128) NOT NULL DEFAULT 'unknown',
		remote_addr VARCHAR(64)  NOT NULL DEFAULT '',
		detail      TEXT         NULL,
		created_at  TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
		INDEX idx_audit_created (created_at),
		INDEX idx_audit_biz (biz, id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

	`CREATE TABLE IF NOT EXISTS runtime_config (
		cfg_key    VARCHAR(64)  NOT NULL PRIMARY KEY,
		cfg_value  VARCHAR(512) NOT NULL,
		operator   VARCHAR(128) NOT NULL DEFAULT 'unknown',
		updated_at TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}
