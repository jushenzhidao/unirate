package admin

import (
	"errors"
	"strings"
	"testing"
)

// TestRejectsRepositoryPlaceholderTokens 锁定一个**实测确认可利用**的缺陷。
//
// 缺陷现场：docker-compose.yml 曾给 ADMIN_TOKEN 配默认值
// `change-me-admin-token-32chars-min`，实测：
//
//	curl :29090/admin/snapshot -H "Authorization: Bearer change-me-admin-token-32chars-min"
//	→ HTTP 200
//
// 该值公开写在仓库里，等于管理面无鉴权。
//
// 为什么原有校验拦不住：
//   - ErrNoToken 只防「忘记设置」，这里恰恰是「设置了」；
//   - `len >= 16` 对 33 字符的占位值完全无效。
//
// 长度是必要条件、不是充分条件。这个测试守的是「公开可知的值一律拒绝」。
func TestRejectsRepositoryPlaceholderTokens(t *testing.T) {
	// 全部来自 docker-compose.yml / .env.example 的历史占位值。
	// 每一个都足够长，都能通过任何纯长度检查。
	placeholders := []string{
		"change-me-admin-token-32chars-min",
		"CHANGE-ME-ADMIN-TOKEN-32CHARS-MIN", // 大写变体同样可猜测
		"change-me-something-else-long-enough",
		"changeme-admin-token-long-enough-x",
		"<RUN make init TO GENERATE>-padding-to-32",
		"unirate_redis_pass_padded_to_32_chars",
		"unirate-pass-padded-out-to-32-chars!",
		"placeholder-token-long-enough-here",
		"example-admin-token-long-enough-ok",
		"your-token-goes-here-long-enough-1",
		"insecure-admin-token-long-enough-1",
	}
	for _, tok := range placeholders {
		if len(tok) < MinTokenLen {
			t.Fatalf("测试数据 %q 本身不足 %d 字符，无法证明「长度够但仍被拒」", tok, MinTokenLen)
		}
		err := ValidateToken(tok)
		if err == nil {
			t.Errorf("占位值 %q 必须被拒绝 —— 它公开在仓库中，等于管理面无鉴权", tok)
			continue
		}
		// 失败信息必须可操作：只说 "too short" 会让部署者随手凑长度，
		// 弱值问题原地复现
		if !strings.Contains(err.Error(), "openssl rand") {
			t.Errorf("拒绝 %q 时未给出生成命令，错误信息不可操作: %v", tok, err)
		}
	}
}

// TestTokenLengthThreshold 阈值从 16 提到 32。
//
// 管理面令牌是长期凭证，泄露即等同于改写全局限流规则的权限，
// 应按长期密钥而非会话令牌取标准。`openssl rand -base64 24` 正好产出 32 字符。
func TestTokenLengthThreshold(t *testing.T) {
	// 31 字符必须拒（证明阈值真的是 32，不是 16）
	tok31 := "Xk7qP2mLd9vRt4wZbN6yHc3jF8sQa1e"
	if len(tok31) != 31 {
		t.Fatalf("测试数据长度应为 31，实际 %d", len(tok31))
	}
	if err := ValidateToken(tok31); err == nil {
		t.Error("31 字符令牌必须被拒绝（阈值为 32）")
	}

	// 恰好 32 字符的强随机值必须过
	tok32 := tok31 + "K"
	if len(tok32) != MinTokenLen {
		t.Fatalf("测试数据长度应为 %d，实际 %d", MinTokenLen, len(tok32))
	}
	if err := ValidateToken(tok32); err != nil {
		t.Errorf("32 字符强随机令牌被误拒: %v", err)
	}
}

// TestRejectsZeroEntropyToken 单字符重复能轻松凑够长度却毫无熵。
// 若只查长度，"aaaa...a" 会通过，反而给人一种"已经安全了"的错觉。
func TestRejectsZeroEntropyToken(t *testing.T) {
	for _, tok := range []string{
		strings.Repeat("a", 40),
		strings.Repeat("X", 32),
		strings.Repeat("0", 64),
	} {
		if err := ValidateToken(tok); err == nil {
			t.Errorf("单字符重复令牌 %q… 必须被拒绝（零实际熵）", tok[:8])
		}
	}
}

// TestEmptyAndBlankToken 空值与纯空白都算未配置
func TestEmptyAndBlankToken(t *testing.T) {
	for _, tok := range []string{"", " ", "\t\n", strings.Repeat(" ", 40)} {
		if err := ValidateToken(tok); err == nil {
			t.Errorf("空/空白令牌 %q 必须被拒绝", tok)
		}
	}
	// 空值必须仍返回 ErrNoToken，main.go 与既有测试依赖该哨兵语义
	if err := ValidateToken(""); !errors.Is(err, ErrNoToken) {
		t.Errorf("空令牌应返回 ErrNoToken 哨兵，实际 %v", err)
	}
}

// TestAcceptsStrongGeneratedToken 校验不能过严：
// `openssl rand -base64 24` 的真实输出形态必须能通过，
// 否则 make init 生成的凭证反而无法启动。
func TestAcceptsStrongGeneratedToken(t *testing.T) {
	// 形如 openssl rand -base64 24 的输出（含 base64 字符集）
	strong := []string{
		"kJ8mQ2xW5nL7pR3vT9yB4cF6hD1sA0zE",
		"Zm9vYmFyYmF6cXV1eHF1dXhmb28xMjM0",
		"aB3/dE5+gH7iJ9kL1mN3oP5qR7sT9uV=",
		"7Kp2Wq9Xm4Ln8Rt3Vy6Bc1Fh5Ds0Az2Ej4Gk",
	}
	for _, tok := range strong {
		if err := ValidateToken(tok); err != nil {
			t.Errorf("强随机令牌 %q 被误拒: %v —— 校验过严会让 make init 产出不可用凭证", tok, err)
		}
	}
}

// TestNewRejectsWeakToken 校验接到 New() 上，而不是只有独立函数能拒。
// 这是防「校验函数写了但没接进构造路径」的沉默失效。
func TestNewRejectsWeakToken(t *testing.T) {
	weak := []string{
		"change-me-admin-token-32chars-min",
		"short",
		"",
		strings.Repeat("a", 40),
	}
	for _, tok := range weak {
		if _, err := New(nil, nil, quietLogger(), Options{Addr: ":0", Token: tok}); err == nil {
			t.Errorf("New() 必须拒绝弱令牌 %q —— 校验未接入构造路径", tok)
		}
	}
}
