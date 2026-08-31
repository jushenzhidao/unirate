package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadSeeds 是 e2e demo 业务域的唯一注入路径。
// 它一旦静默失效，e2e 会以「找不到业务域」的形式报错，
// 看起来像限流逻辑出问题，排查成本极高。
// 因此这里覆盖它的四个关键语义：执行顺序、幂等、失败即报、目录缺失容忍。

func writeSeed(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write seed %s: %v", name, err)
	}
}

func TestLoadSeedsAppliesInLexicalOrder(t *testing.T) {
	db := openTemp(t)
	dir := t.TempDir()

	// 02 依赖 01 已建表。若执行顺序错乱，02 会因表不存在而失败。
	writeSeed(t, dir, "01-schema.sql", `
CREATE TABLE IF NOT EXISTS seed_probe (k TEXT PRIMARY KEY, v TEXT);
`)
	writeSeed(t, dir, "02-data.sql", `
INSERT INTO seed_probe (k, v) VALUES ('demo', 'first')
  ON CONFLICT(k) DO UPDATE SET v=excluded.v;
`)
	// 非 .sql 必须被忽略，否则挂载目录里的 README 会让启动失败
	writeSeed(t, dir, "notes.txt", "this is not sql and must be skipped")

	if err := db.LoadSeeds(context.Background(), dir); err != nil {
		t.Fatalf("LoadSeeds: %v", err)
	}

	var v string
	if err := db.QueryRow(`SELECT v FROM seed_probe WHERE k='demo'`).Scan(&v); err != nil {
		t.Fatalf("probe row missing: %v", err)
	}
	if v != "first" {
		t.Errorf("v = %q, want first", v)
	}
}

// 容器重启会重复执行种子。不幂等就会在第二次启动时崩掉。
func TestLoadSeedsIsIdempotent(t *testing.T) {
	db := openTemp(t)
	dir := t.TempDir()

	writeSeed(t, dir, "01-schema.sql", `
CREATE TABLE IF NOT EXISTS seed_probe (k TEXT PRIMARY KEY, v TEXT);
`)
	writeSeed(t, dir, "02-data.sql", `
INSERT INTO seed_probe (k, v) VALUES ('demo', 'v2')
  ON CONFLICT(k) DO UPDATE SET v=excluded.v;
`)

	for i := 0; i < 3; i++ {
		if err := db.LoadSeeds(context.Background(), dir); err != nil {
			t.Fatalf("LoadSeeds run#%d: %v", i+1, err)
		}
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM seed_probe`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("row count = %d after 3 runs, want 1", n)
	}
}

// 目录不存在是合法状态（镜像未挂载种子卷），不得报错。
// 但目录存在却有坏语句必须致命 —— 静默跳过会让验收结论失去意义。
func TestLoadSeedsMissingDirIsNotAnError(t *testing.T) {
	db := openTemp(t)
	if err := db.LoadSeeds(context.Background(), filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("missing dir should be tolerated, got %v", err)
	}
	if err := db.LoadSeeds(context.Background(), ""); err != nil {
		t.Errorf("empty dir should be tolerated, got %v", err)
	}
}

func TestLoadSeedsFailsLoudlyOnBadStatement(t *testing.T) {
	db := openTemp(t)
	dir := t.TempDir()
	writeSeed(t, dir, "01-broken.sql", `INSERT INTO table_that_does_not_exist (a) VALUES (1);`)

	err := db.LoadSeeds(context.Background(), dir)
	if err == nil {
		t.Fatal("expected error for bad statement, got nil")
	}
	// 错误必须带上文件名与语句，否则容器日志里定位不到是哪条种子
	if !strings.Contains(err.Error(), "01-broken.sql") {
		t.Errorf("error must name the file, got: %v", err)
	}
	if !strings.Contains(err.Error(), "table_that_does_not_exist") {
		t.Errorf("error must include the statement, got: %v", err)
	}
}

// 读取被 os.Root 限定在 dir 内。符号链接指向外部时应被内核拒绝，
// 而不是把外部文件当种子执行。
func TestLoadSeedsDoesNotFollowSymlinkOutOfDir(t *testing.T) {
	db := openTemp(t)
	dir := t.TempDir()
	outside := t.TempDir()

	victim := filepath.Join(outside, "outside.sql")
	if err := os.WriteFile(victim, []byte(`CREATE TABLE escaped (x INT);`), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "01-link.sql")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	err := db.LoadSeeds(context.Background(), dir)
	if err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}

	// 确认逃逸出去的语句确实没被执行
	var name string
	scanErr := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='escaped'`,
	).Scan(&name)
	if scanErr == nil {
		t.Errorf("statement from outside dir was executed (table %q created)", name)
	}
}
