// Package store 负责 SQLite 存储的封装。
//
// 设计决策：使用 SQLite 作为唯一存储后端，理由：
//   - 三张表（biz_config / audit_log / runtime_config）总量小
//   - 业务流量路径完全不查关系库 —— 网关读配置走 Redis 快照
//   - 关系库只承担「配置写入的 SoT」与「审计问责」，单写者足够
//   - 纯 Go 实现（modernc.org/sqlite），可在 CGO_ENABLED=0 下静态链接
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无需 CGO
)

// DB 存储句柄
type DB struct {
	*sql.DB
}

// Open 打开 SQLite 连接。
//
// 连接池参数：SQLite 在 WAL 模式下允许多读单写，但并发写会撞 SQLITE_BUSY。
// 把写连接限制为 1 是最可靠的规避方式 —— 由 database/sql 层排队，
// 而不是依赖 busy_timeout 自旋重试。
func Open(path string) (*DB, error) {
	if path == "" {
		path = defaultDBPath()
	}

	target := sqliteTarget(path)
	db, err := sql.Open("sqlite", target)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// 单连接：WAL 下写必须串行。读也走这条连接，
	// 但读全部发生在管理面（低频），不构成瓶颈。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// SQLite 是进程内库，连接无需回收
	db.SetConnMaxLifetime(0)

	return &DB{DB: db}, nil
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
	return fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)", path)
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
