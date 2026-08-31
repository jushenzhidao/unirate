package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		want    Kind
		wantErr bool
		// contains 校验传给驱动的目标串包含该子串
		contains string
	}{
		{name: "empty falls back to sqlite", dsn: "", want: KindSQLite, contains: "unirate.db"},
		{name: "sqlite scheme", dsn: "sqlite:///tmp/a.db", want: KindSQLite, contains: "/tmp/a.db"},
		{name: "bare abs path", dsn: "/tmp/b.db", want: KindSQLite, contains: "/tmp/b.db"},
		{name: "bare rel path", dsn: "./c.db", want: KindSQLite, contains: "c.db"},
		{name: "file form passthrough", dsn: "file:/tmp/d.db?_pragma=busy_timeout(1)", want: KindSQLite, contains: "busy_timeout(1)"},
		{name: "mysql tcp form", dsn: "u:p@tcp(127.0.0.1:3306)/unirate", want: KindMySQL, contains: "@tcp("},
		{name: "mysql scheme stripped", dsn: "mysql://u:p@tcp(h:3306)/db", want: KindMySQL, contains: "u:p@tcp(h:3306)/db"},
		{name: "mysql unix socket", dsn: "u:p@unix(/tmp/m.sock)/db", want: KindMySQL, contains: "@unix("},
		{name: "garbage rejected", dsn: "postgres://x", wantErr: true},
		{name: "sqlite scheme without path", dsn: "sqlite://", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, driver, target, err := parseDSN(c.dsn)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got kind=%s target=%s", c.dsn, kind, target)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != c.want {
				t.Errorf("kind = %s, want %s", kind, c.want)
			}
			if driver != string(c.want) {
				t.Errorf("driver = %s, want %s", driver, c.want)
			}
			if !strings.Contains(target, c.contains) {
				t.Errorf("target %q does not contain %q", target, c.contains)
			}
		})
	}
}

// sqlite 目标串必须携带全部四个 pragma。漏掉 WAL 会让管理面的
// 审计查询阻塞配置写入，属于静默的性能故障，必须由测试锁定。
func TestSQLiteTargetCarriesPragmas(t *testing.T) {
	got := sqliteTarget(filepath.Join(t.TempDir(), "x.db"))
	for _, p := range []string{
		"journal_mode%28WAL%29",
		"busy_timeout%285000%29",
		"foreign_keys%28on%29",
		"synchronous%28NORMAL%29",
	} {
		if !strings.Contains(got, p) {
			t.Errorf("target %q missing pragma %q", got, p)
		}
	}
}

func TestUpsertSuffixDialects(t *testing.T) {
	keys := []string{"cfg_key"}
	upd := []string{"cfg_value", "operator"}

	sq := (&DB{Kind: KindSQLite}).UpsertSuffix(keys, upd)
	if !strings.Contains(sq, "ON CONFLICT(cfg_key) DO UPDATE SET") ||
		!strings.Contains(sq, "cfg_value=excluded.cfg_value") ||
		!strings.Contains(sq, "operator=excluded.operator") {
		t.Errorf("sqlite suffix wrong: %s", sq)
	}

	my := (&DB{Kind: KindMySQL}).UpsertSuffix(keys, upd)
	if !strings.Contains(my, "ON DUPLICATE KEY UPDATE") ||
		!strings.Contains(my, "cfg_value=VALUES(cfg_value)") {
		t.Errorf("mysql suffix wrong: %s", my)
	}
	// MySQL 不接受冲突目标声明，出现即是语法错误
	if strings.Contains(my, "ON CONFLICT") {
		t.Errorf("mysql suffix must not declare conflict target: %s", my)
	}
}

// 列名白名单是 UpsertSuffix 拼接 SQL 的安全前提。
// 这道校验没有覆盖，等于安全论证只写在注释里。
func TestUpsertSuffixRejectsUnsafeIdentifiers(t *testing.T) {
	payloads := []string{
		"cfg_value=1; DROP TABLE biz_config--",
		"cfg_value`",
		"cfg value",
		"CfgValue",
		"1col",
		"",
		"cfg_value)",
	}
	for _, bad := range payloads {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("expected panic for identifier %q", bad)
				}
			}()
			_ = (&DB{Kind: KindSQLite}).UpsertSuffix([]string{"cfg_key"}, []string{bad})
		})
	}

	// 合法标识符不得误伤
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("unexpected panic for valid identifiers: %v", r)
			}
		}()
		_ = (&DB{Kind: KindSQLite}).UpsertSuffix(
			[]string{"cfg_key"},
			[]string{"cfg_value", "operator", "path_strip_prefix", "rules_json", "v2_col"},
		)
	}()
}

// 端到端验证：建表 → upsert 覆盖 → WAL 生效。
// 这三点是 SQLite 替换 MySQL 的全部前提假设。
func TestMigrateAndUpsertRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 幂等性：重复迁移不得报错
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate not idempotent: %v", err)
	}

	var jm string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(jm, "wal") {
		t.Fatalf("journal_mode = %q, want wal", jm)
	}

	q := `INSERT INTO runtime_config (cfg_key, cfg_value, operator) VALUES (?, ?, ?)` +
		db.UpsertSuffix([]string{"cfg_key"}, []string{"cfg_value", "operator"})

	if _, err := db.ExecContext(ctx, q, "log_level", "info", "alice"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, q, "log_level", "debug", "bob"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var val, op string
	if err := db.QueryRowContext(ctx,
		`SELECT cfg_value, operator FROM runtime_config WHERE cfg_key = ?`,
		"log_level").Scan(&val, &op); err != nil {
		t.Fatalf("select: %v", err)
	}
	if val != "debug" || op != "bob" {
		t.Errorf("got (%s, %s), want (debug, bob)", val, op)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runtime_config`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d, want 1 (upsert must not insert a duplicate)", n)
	}
}

// 审计与配置变更同事务提交，一次 WAL fsync 覆盖两条写入。
// 该测试守住这个性质：审计行必须与配置行同时可见，
// 不允许被改造成异步写入 —— 那会让「改了规则但没记录」成为可能。
func TestAuditSharesTransactionWithConfigWrite(t *testing.T) {
	db := openTemp(t)
	// 超时兜底：SQLite 池只有 1 条连接，任何「持有事务时再向池取连接」
	// 都会永久死锁而非报错。加 ctx deadline 让这类回归 5 秒内失败。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO biz_config (biz, base_url, rules_json) VALUES (?, ?, ?)`+
			db.UpsertSuffix([]string{"biz"}, []string{"base_url", "rules_json"}),
		"b1", "http://u", "[]"); err != nil {
		t.Fatalf("upsert biz: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (action, biz, operator) VALUES (?, ?, ?)`,
		"upsert_biz", "b1", "tester"); err != nil {
		t.Fatalf("insert audit: %v", err)
	}

	// 两条写入必须在同一事务内可见 —— 这是「同事务」的可测断言。
	// 注意不能用 db.QueryRow 验证「提交前对外不可见」：单连接池下
	// 那会死锁，且隔离性本身由 SQLite 保证，不是本包的职责。
	var inTx int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&inTx); err != nil {
		t.Fatalf("in-tx count: %v", err)
	}
	if inTx != 1 {
		t.Errorf("audit rows inside tx = %d, want 1", inTx)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var cfgN, auditN int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM biz_config`).Scan(&cfgN); err != nil {
		t.Fatalf("cfg count: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&auditN); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if cfgN != 1 || auditN != 1 {
		t.Errorf("after commit cfg=%d audit=%d, want 1/1", cfgN, auditN)
	}
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
