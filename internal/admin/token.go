package admin

import (
	"fmt"
	"strings"
)

// Admin 令牌强度校验。
//
// 背景（实测确认的真实缺陷）：docker-compose.yml 曾给 ADMIN_TOKEN 配默认值
// `change-me-admin-token-32chars-min`，用它可直接访问管理面（HTTP 200）。
// 该值公开写在仓库里，等于管理面无鉴权。
//
// 原有的两道校验都拦不住它：
//   - ErrNoToken 只防「忘记设置」，而这里恰恰是「设置了」；
//   - `len >= 16` 对 33 字符的占位值毫无作用。
//
// 结论：长度检查是必要条件、不是充分条件。占位值比空值更危险，
// 因为它看起来像"已经配好了"，没人会去复查。
// 因此必须增加**弱值黑名单**这一维度 —— 校验的是「这个值是否公开可知」，
// 而不只是「这个值够不够长」。

// MinTokenLen 令牌最小长度。
//
// 从 16 提到 32：16 字符对应约 96 bit（Base64）熵，看似够，
// 但管理面令牌是长期凭证、且一旦泄露即等同于改写全局限流规则的权限，
// 应当按长期密钥而非会话令牌的标准取值。
// `openssl rand -base64 24` 产出 32 字符，正好对齐。
const MinTokenLen = 32

// GenerateHint 校验失败时给出的可操作修复指引。
//
// 只说 "token too short" 是没用的 —— 部署者下一步动作是什么并不明确，
// 大概率就是随手补几个字符凑长度，弱值问题原地复现。
// 必须直接给出生成命令。
const GenerateHint = "generate one with: openssl rand -base64 24   " +
	"(or run `make init` to create a .env with strong random credentials)"

// weakExact 完整匹配即拒绝的弱值。
//
// 含 .env.example 中出现过的全部占位值 —— 这些字符串已随仓库公开，
// 任何一个被用作真实凭证都等同于无鉴权。
var weakExact = map[string]struct{}{
	// .env.example / docker-compose.yml 历史占位值
	"change-me-admin-token-32chars-min": {},
	"unirate_redis_pass":                {},
	"unirate_pass":                      {},
	"unirate_root_pass":                 {},
	"<run make init to generate>":       {},
	// 通用弱值
	"admin":    {},
	"password": {},
	"test":     {},
	"123456":   {},
	"changeme": {},
	"secret":   {},
	"token":    {},
	"default":  {},
}

// weakContains 子串匹配即拒绝。
//
// 为什么前缀匹配不够：占位值常被人「补几个字符凑长度」，
// 例如 `<RUN make init TO GENERATE>-padding` —— 前缀变了但值依然公开可知。
// 这类模板标记只要出现在任意位置，就说明这个值是从模板派生的。
var weakContains = []string{
	"run make init",
	"to generate",
	"change-me",
	"changeme",
	"change_me",
	"replace-me",
	"replaceme",
	"placeholder",
	"your-token",
	"your_token",
	"admin-token",
	"admin_token",
	"unirate_pass",
	"unirate_redis_pass",
	"unirate_root_pass",
}

// weakPrefix 前缀匹配即拒绝。
// 覆盖 change-me / changeme 家族的各种变体，不必逐一穷举。
var weakPrefix = []string{
	"change-me",
	"changeme",
	"change_me",
	"replace-me",
	"replaceme",
	"your-token",
	"your_token",
	"example",
	"placeholder",
	"insecure",
	"unirate_",
	"unirate-",
}

// ValidateToken 校验 Admin 令牌强度。
//
// 三道检查，缺一不可：
//  1. 非空 —— 防「忘记设置」；
//  2. 长度 ≥ MinTokenLen —— 防暴力猜测；
//  3. 不是公开可知的弱值 —— 防「用了仓库里的占位值」。
//
// 大小写不敏感比对：ADMIN 与 admin 的可猜测性没有区别。
func ValidateToken(tok string) error {
	t := strings.TrimSpace(tok)
	if t == "" {
		return ErrNoToken
	}
	if len(t) < MinTokenLen {
		return fmt.Errorf("admin token too short: got %d chars, need >= %d; %s",
			len(t), MinTokenLen, GenerateHint)
	}
	low := strings.ToLower(t)
	if _, hit := weakExact[low]; hit {
		return fmt.Errorf("admin token %q is a well-known placeholder published in this "+
			"repository; using it leaves the admin api effectively unauthenticated. %s",
			t, GenerateHint)
	}
	for _, p := range weakPrefix {
		if strings.HasPrefix(low, p) {
			return fmt.Errorf("admin token starts with weak placeholder prefix %q; "+
				"placeholder-derived tokens are guessable. %s", p, GenerateHint)
		}
	}
	// 子串检查排在前缀之后：命中前缀时报「前缀」更贴近用户实际写的东西，
	// 报错更好懂
	for _, c := range weakContains {
		if strings.Contains(low, c) {
			return fmt.Errorf("admin token contains placeholder marker %q; padding a "+
				"published placeholder does not make it secret. %s", c, GenerateHint)
		}
	}
	// 单字符重复（"aaaa..."）能轻松凑够长度却毫无熵，必须拦住 ——
	// 否则上面的长度检查会给人一种"已经安全了"的错觉。
	if isSingleRune(t) {
		return fmt.Errorf("admin token consists of a single repeated character and has "+
			"no practical entropy. %s", GenerateHint)
	}
	return nil
}

func isSingleRune(s string) bool {
	rs := []rune(s)
	if len(rs) < 2 {
		return true
	}
	for _, r := range rs[1:] {
		if r != rs[0] {
			return false
		}
	}
	return true
}
