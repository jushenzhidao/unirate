package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Key 设计说明（对应评审 P1-8 修正）
//
// 原设计缺陷：
//  1. 维度值用 '_' 连接，而 path 本身含 '/' 和 '_'，biz 也允许 '_'
//     → `ratelimit:biz_path_token:order_/v1/create_a3f9b2c1` 无法反解析，
//       且不同维度组合可碰撞出同一个 Key（安全问题：A 业务吃掉 B 业务配额）
//  2. 窗口边界格式不统一：§3.4.1 用 datetime(20250823010000)，§6.4.3 用 epoch 秒
//  3. d/w 窗口按 UTC 对齐，国内业务「1 天配额」会在早 8 点重置，时区语义未声明
//
// 本实现的修正：
//  1. 分隔符统一为 '|'，且所有可变维度值经 safeVal() 编码 —— 长值或含分隔符的值取
//     SHA256 前 24 hex（96bit，评审 Advisory-7 建议的抗碰撞长度），保证值域内绝不出现 '|'
//  2. 窗口边界统一为 epoch 秒
//  3. 窗口对齐支持配置业务时区偏移，d/w 按本地零点对齐

const (
	sep       = "|"
	prefixRL  = "rl"
	prefixTB  = "tb"
	prefixCC  = "cc"
	prefixTok = "tk"
	// maxRawLen 以内的纯净值保持明文，便于线上排查；超长或含分隔符则哈希
	maxRawLen = 48
)

// safeVal 保证维度值在 Key 中不引入歧义。
// 明文可读性对排障很重要，因此只在必要时降级为哈希。
func safeVal(v string) string {
	if v == "" {
		return "_"
	}
	if len(v) <= maxRawLen && !strings.ContainsAny(v, "|/ \t\r\n") {
		return v
	}
	h := sha256.Sum256([]byte(v))
	return "h" + hex.EncodeToString(h[:])[:24]
}

// HashToken 对鉴权令牌做单向摘要，避免明文 Token 落入 Redis Key 与日志。
// 长度 24 hex = 96bit，对应评审 Advisory-7（原 16 hex=64bit 碰撞风险偏高）。
func HashToken(raw string) string {
	if raw == "" {
		return "_"
	}
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])[:24]
}

// WindowBoundary 计算窗口起点的 epoch 秒。
// tzOffset 为业务时区相对 UTC 的秒偏移（东八区 = 8*3600），
// 仅对 d/w 这类「自然日/自然周」语义的窗口生效；s/m/h 按绝对时间对齐，不受时区影响。
//
// winSec <= 0 时返回 0 而不是除零 panic。
//
// 这不是防御性冗余，而是修一个实测触发过的崩溃：
// 并发规则（type=concurrency）不解析 Window，winSec 恒为 0，
// 而降级路径曾无条件对它调用本函数 —— Redis 故障期间每个请求都
// integer divide by zero，直接 panic 掉 HTTP 连接。
//
// 防御放在做除法的这一行，而不是依赖每个调用点自查：
// 调用点有 5 处（Check / degrade / TokenAdmit / TokenReserve / TokenSettle / TokenUsage），
// 靠"每处都记得检查"来保证安全，等于把一个必然会被漏掉的义务分发出去。
func WindowBoundary(now time.Time, winSec int64, tzOffset int64, natural bool) int64 {
	if winSec <= 0 {
		return 0
	}
	ts := now.Unix()
	if natural && tzOffset != 0 {
		return ((ts+tzOffset)/winSec)*winSec - tzOffset
	}
	return (ts / winSec) * winSec
}

// RateKey 构造速率限流 Key。
// 形如 rl|biz.path|order.hxxxx|60|1755900000
func RateKey(dims, vals []string, winSec, boundary int64) string {
	var b strings.Builder
	b.WriteString(prefixRL)
	b.WriteString(sep)
	b.WriteString(strings.Join(dims, "."))
	b.WriteString(sep)
	for i, v := range vals {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(safeVal(v))
	}
	b.WriteString(sep)
	b.WriteString(strconv.FormatInt(winSec, 10))
	b.WriteString(sep)
	b.WriteString(strconv.FormatInt(boundary, 10))
	return b.String()
}

// TokenBucketKey 构造令牌桶 Key。
// 关键：不含窗口边界（对应评审 P0-2）。令牌桶是持久状态桶，
// 带 boundary 会使其每窗口重建，退化成「固定窗口 + 突发额度」，与设计意图不符。
func TokenBucketKey(dims, vals []string) string {
	var b strings.Builder
	b.WriteString(prefixTB)
	b.WriteString(sep)
	b.WriteString(strings.Join(dims, "."))
	b.WriteString(sep)
	for i, v := range vals {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(safeVal(v))
	}
	return b.String()
}

// ConcurrencyKey 构造并发控制 Key。
// 统一格式，修正评审 P0-4 第 4 项指出的 §3.4.1 与 §6.4.2 格式不一致问题。
func ConcurrencyKey(biz string, dims, vals []string) string {
	var b strings.Builder
	b.WriteString(prefixCC)
	b.WriteString(sep)
	b.WriteString(safeVal(biz))
	b.WriteString(sep)
	b.WriteString(strings.Join(dims, "."))
	b.WriteString(sep)
	for i, v := range vals {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(safeVal(v))
	}
	return b.String()
}

// TokenLedgerKey 构造 Token 账本 Key。
func TokenLedgerKey(dims, vals []string, winSec, boundary int64) string {
	var b strings.Builder
	b.WriteString(prefixTok)
	b.WriteString(sep)
	b.WriteString(strings.Join(dims, "."))
	b.WriteString(sep)
	for i, v := range vals {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(safeVal(v))
	}
	b.WriteString(sep)
	b.WriteString(strconv.FormatInt(winSec, 10))
	b.WriteString(sep)
	b.WriteString(strconv.FormatInt(boundary, 10))
	return b.String()
}
