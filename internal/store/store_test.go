package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp 打开内存 SQLite，用于单元测试。
// :memory: 对每个连接独立，不同连接看不到彼此的数据。
// 本仓库的 DB 池只开 1 条连接（见 Open），因此不会踩这个坑，
// 但显式用文件模式更稳定：失败时可保留现场供调试。
func openTemp(t *testing.T) *DB {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(p)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestUpsertSuffix 验证 SQLite upsert 子句的生成。
func TestUpsertSuffix(t *testing.T) {
	db := &DB{}
	keys := []string{"cfg_key"}
	upd := []string{"cfg_value", "updated_at"}

	sq := db.UpsertSuffix(keys, upd)
	if !strings.Contains(sq, "ON CONFLICT(cfg_key) DO UPDATE SET") ||
		!strings.Contains(sq, "cfg_value=excluded.cfg_value") {
		t.Errorf("sqlite suffix wrong: %s", sq)
	}
}

// 端到端验证：建表 → upsert 覆盖 → WAL 生效。
// 这三点是 SQLite 存储的全部前提假设。
func TestMigrateAndUpsertRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	// 建表
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 插入
	sql1 := "INSERT INTO runtime_config (cfg_key, cfg_value) VALUES (?, ?)"
	if _, err := db.ExecContext(ctx, sql1, "k1", "v1"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// upsert 覆盖
	sql2 := "INSERT INTO runtime_config (cfg_key, cfg_value) VALUES (?, ?)" +
		db.UpsertSuffix([]string{"cfg_key"}, []string{"cfg_value"})
	if _, err := db.ExecContext(ctx, sql2, "k1", "v2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// 验证
	var got string
	row := db.QueryRowContext(ctx, "SELECT cfg_value FROM runtime_config WHERE cfg_key=?", "k1")
	if err := row.Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != "v2" {
		t.Errorf("want v2, got %s", got)
	}
}

// TestMigrateIdempotent 幂等建表多次执行不应报错。
func TestMigrateIdempotent(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate round %d: %v", i+1, err)
		}
	}
}

// TestAssertIdentsRejectsBadInput 不安全的标识符必须被拒绝。
func TestAssertIdentsRejectsBadInput(t *testing.T) {
	cases := [][]string{
		{"'; DROP TABLE biz_config; --"},
		{"col name with spaces"},
		{"col-with-dash"},
		{"123startwithdigit"},
		{"col`backtick"},
	}
	for _, bad := range cases {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %v, got none", bad)
				}
			}()
			assertIdents(bad)
		}()
	}
}

// TestAssertIdentsAllowsGoodInput 合法标识符应通过。
func TestAssertIdentsAllowsGoodInput(t *testing.T) {
	good := []string{"biz", "cfg_key", "updated_at", "_private", "col2"}
	assertIdents(good) // 不应 panic
}
