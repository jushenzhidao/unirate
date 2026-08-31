package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// 种子数据加载。
//
// 为什么需要它：e2e 的 demo 业务域需要在启动时注入。
// 数据库由 gateway 进程自己创建，种子通过 SEED_SQL_DIR 环境变量指定的目录加载。
// 不补齐，e2e 会因为「找不到 demo 业务域」大面积失败，
// 而失败原因看起来像限流逻辑出错，极难定位。
//
// 设计约束：
//   - 仅在 SEED_SQL_DIR 显式设置时生效。生产不设置该变量，
//     因此这段代码在生产路径上完全不可达，不构成「测试代码进生产」。
//   - 幂等。种子文件必须自带 upsert 语义，因为容器重启会重复执行。
//   - 失败即致命。种子加载不了就意味着后续验收结论无效，
//     静默跳过比启动失败危险得多。
//   - 文件读取限定在 os.Root 作用域内。目录条目名本不含路径分隔符，
//     但用 Root 让「不可能穿越出 dir」由内核 openat 保证，
//     而不是依赖对 ReadDir 返回值的推理。

// SeedDirEnv 是种子目录的环境变量名。
const SeedDirEnv = "SEED_SQL_DIR"

// LoadSeeds 按文件名字典序执行目录下的 .sql 文件。
//
// 排序语义按字典序（如 01-schema.sql、02-data.sql），保证执行顺序可控。
//
// 目录不存在时返回 nil：允许镜像里没挂载种子卷的情况。
// 但目录存在却读不了、或某条语句执行失败，一律返回错误。
func (db *DB) LoadSeeds(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read seed dir %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open seed root %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	for _, name := range files {
		raw, err := readSeedFile(root, name)
		if err != nil {
			return fmt.Errorf("read seed %s/%s: %w", dir, name, err)
		}
		for i, stmt := range splitStatements(string(raw)) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("seed %s stmt#%d: %w\nstatement: %s",
					name, i+1, err, stmt)
			}
		}
	}
	return nil
}

// readSeedFile 在 root 作用域内读取单个种子文件。
//
// 抽成函数而非在循环里内联，是为了让 Close 随每个文件的读取结束立即生效；
// 内联写法的 defer 会堆积到 LoadSeeds 返回时才执行。
func readSeedFile(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// splitStatements 按分号切分 SQL 并剔除注释与空语句。
//
// 这是个刻意的简化实现：只处理种子文件这一种输入，不追求通用 SQL 解析。
// 已知不支持的形态：分号出现在字符串字面量或 BEGIN...END 块内部。
// 种子文件是本仓库自己维护的，不用这些形态即可 —— 若将来需要，
// 应改为让驱动逐条执行而不是在这里补一个半成品词法分析器。
func splitStatements(raw string) []string {
	var out []string
	for _, chunk := range strings.Split(raw, ";") {
		var lines []string
		for _, ln := range strings.Split(chunk, "\n") {
			t := strings.TrimSpace(ln)
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			lines = append(lines, ln)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt != "" {
			out = append(out, stmt)
		}
	}
	return out
}
