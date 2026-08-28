// Package store 负责关系型存储的驱动选择、建表与方言差异。
//
// 引入原因：原实现把 MySQL 硬编码进 admin 包的 SQL 字面量（两处
// ON DUPLICATE KEY UPDATE），导致「换存储」必须改业务代码。本包把
// 差异收敛到一处，admin 只依赖 Dialect 暴露的语义。
//
// 默认驱动改为 SQLite（modernc.org/sqlite，纯 Go 实现，
// 可在 CGO_ENABLED=0 下静态链接，保住 Dockerfile 现有的构建形态）。
// 选择依据：三张表（biz_config / audit_log / runtime_config）总量小，
// 且业务流量路径完全不查关系库 —— 网关读配置走 Redis 快照。
// 关系库只承担「配置写入的 SoT」与「审计问责」，单写者足够。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// Kind 存储类型
type Kind string

const (
	// KindSQLite 纯 Go SQLite，默认
	KindSQLite Kind = "sqlite"
	// KindMySQL 外部 MySQL，用于已有集群复用
	KindMySQL Kind = "mysql"
)

// DB 存储句柄，附带方言信息
type DB struct {
	*sql.DB
	Kind Kind
}

// Open 按 DSN 前缀推断驱动并打开连接。
//
// 连接池参数按驱动区分，这不是调优偏好而是正确性要求：
// SQLite 在 WAL 模式下允许多读单写，但并发写会撞 SQLITE_BUSY。
// 把写连接限制为 1 是最可靠的规避方式 —— 由 database/sql 层排队，
// 而不是依赖 busy_timeout 自旋重试。
func Open(dsn string) (*DB, error) {
	kind, driver, target, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open(driver, target)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", kind, err)
	}

	switch kind {
	case KindSQLite:
		// 单连接：WAL 下写必须串行。读也走这条连接，
		// 但读全部发生在管理面（低频），不构成瓶颈。
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		// SQLite 是进程内库，连接无需回收
		db.SetConnMaxLifetime(0)
	case KindMySQL:
		db.SetMaxOpenConns(16)
		db.SetMaxIdleConns(8)
		db.SetConnMaxLifetime(30 * time.Minute)
	}

	return &DB{DB: db, Kind: kind}, nil
}

// parseDSN 解析 DSN，返回存储类型、驱动名与驱动实际接受的目标串。
//
// 支持形式：
//   - ""                          → 默认 SQLite，落盘 ./data/unirate.db
//   - "sqlite:///abs/path.db"     → SQLite 指定路径
//   - "/abs/path.db"、"./rel.db"  → 裸路径按 SQLite 处理
//   - "file:...?_pragma=..."      → 直接透传给 SQLite 驱动
//   - "user:pass@tcp(host)/db"    → MySQL
//   - "mysql://..."               → MySQL（去掉 scheme 后透传）
func parseDSN(dsn string) (Kind, string, string, error) {
	d := strings.TrimSpace(dsn)

	if d == "" {
		return KindSQLite, "sqlite", sqliteTarget(defaultDBPath()), nil
	}
	if after, ok := strings.CutPrefix(d, "sqlite://"); ok {
		p := after
		if p == "" {
			return "", "", "", fmt.Errorf("sqlite dsn missing path: %q", dsn)
		}
		return KindSQLite, "sqlite", sqliteTarget(p), nil
	}
	if strings.HasPrefix(d, "file:") {
		// 已是驱动原生形式，调用方自行负责 pragma
		return KindSQLite, "sqlite", d, nil
	}
	if after, ok := strings.CutPrefix(d, "mysql://"); ok {
		return KindMySQL, "mysql", after, nil
	}
	// 裸路径 → SQLite；含 @ 或 tcp( → MySQL
	if strings.Contains(d, "@tcp(") || strings.Contains(d, "@unix(") {
		return KindMySQL, "mysql", d, nil
	}
	if strings.HasPrefix(d, "/") || strings.HasPrefix(d, "./") ||
		strings.HasSuffix(d, ".db") || strings.HasSuffix(d, ".sqlite") {
		return KindSQLite, "sqlite", sqliteTarget(d), nil
	}
	return "", "", "", fmt.Errorf("unrecognized DSN form: %q "+
		"(use sqlite:///path.db, /path.db, or user:pass@tcp(host:3306)/db)", dsn)
}

// defaultDBPath 默认落盘位置。
// 用相对路径而非 /var/lib，是为了让本地 go run 与容器内 volume 挂载都能直接工作。
func defaultDBPath() string {
	if p := os.Getenv("SQLITE_PATH"); p != "" {
		return p
	}
	return filepath.Join("data", "unirate.db")
}

// sqliteTarget 构造带必要 pragma 的 SQLite DSN。
//
// 三个 pragma 都不是可选优化：
//   - journal_mode=WAL：读不阻塞写、写不阻塞读。没有它，
//     管理面的一次审计查询会阻塞配置写入。
//   - busy_timeout=5000：即使已限制单写连接，checkpoint 期间仍可能短暂忙。
//   - foreign_keys=on：SQLite 默认关闭外键约束，静默接受脏数据。
//   - synchronous=NORMAL：WAL 下 NORMAL 已能保证进程崩溃不丢已提交事务
//     （仅 OS 崩溃可能丢最后几个事务），比 FULL 少一次 fsync。
//     审计日志可接受这个权衡；配置写入的 SoT 语义由 Redis 快照兜底。
func sqliteTarget(path string) string {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o750)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(on)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return "file:" + path + "?" + q.Encode()
}

// Ping 带超时的连通性检查
func (db *DB) Ping(ctx context.Context) error {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return db.PingContext(c)
}

// writeTxTimeout 是单次配置写事务的上限。
//
// SQLite 池只有 1 条连接（见 Open），因此「拿不到连接」不是排队变慢，
// 而是永久阻塞在 database/sql 的 conn 等待上：既不返回错误，也不释放
// 那条唯一连接，整个管理面随之全挂且无任何日志。deadline 是唯一能把
// 这类故障从静默死锁降级为可观测 5xx 的手段。
const writeTxTimeout = 5 * time.Second

// BeginWrite 开启一个带 deadline 的写事务，返回的 cancel 必须被调用。
//
// 所有配置写路径都应走这里而不是裸 BeginTx —— 约束集中在一处，
// 新增写路径就不会各自漏掉超时。
func (db *DB) BeginWrite(ctx context.Context) (*sql.Tx, context.Context, context.CancelFunc, error) {
	c, cancel := context.WithTimeout(ctx, writeTxTimeout)
	tx, err := db.BeginTx(c, nil)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return tx, c, cancel, nil
}
