package meter

import "testing"

// TestSettleRefundsOverCharge 验证评审 P0-6 的核心修正。
//
// 原设计：估算值 ×1.2 预扣，拿到精确值后「覆盖」但无补偿算法
//
//	→ 缓冲部分永远只扣不退，全局预算被系统性提前约 20% 耗尽。
//
// 本实现：预扣量与精确量做差额结算，多扣必退。
func TestSettleRefundsOverCharge(t *testing.T) {
	var c Counter
	c.AddEstimate(1000)

	const buffer = 1.2
	reserved := c.PendingFlush(buffer)
	if reserved != 1200 {
		t.Fatalf("expected 1200 reserved with 1.2 buffer, got %d", reserved)
	}

	// 上游返回精确值 950 —— 比预扣少 250
	c.SetExact(950)
	final := c.Final(buffer)
	if final != 950 {
		t.Fatalf("exact usage must win over estimate, got %d", final)
	}

	diff := final - c.Flushed()
	if diff != -250 {
		t.Fatalf("expected -250 to be refunded, got %d", diff)
	}
}

// TestPendingFlushIsIncremental 增量刷盘不得重复计费
func TestPendingFlushIsIncremental(t *testing.T) {
	var c Counter
	c.AddEstimate(100)
	first := c.PendingFlush(1.0)
	if first != 100 {
		t.Fatalf("first flush: want 100, got %d", first)
	}
	// 无新增时不得再刷
	if again := c.PendingFlush(1.0); again != 0 {
		t.Fatalf("no new tokens must yield 0, got %d", again)
	}
	c.AddEstimate(50)
	if delta := c.PendingFlush(1.0); delta != 50 {
		t.Fatalf("incremental flush: want 50, got %d", delta)
	}
	if total := c.Flushed(); total != 150 {
		t.Fatalf("total flushed: want 150, got %d", total)
	}
}

func TestExtractUsage(t *testing.T) {
	cases := []struct {
		body string
		want int64
		ok   bool
	}{
		{`{"usage":{"total_tokens":123}}`, 123, true},
		{`{"usage":{"prompt_tokens":10,"completion_tokens":20}}`, 30, true},
		{`{"choices":[]}`, 0, false},
		{`not json`, 0, false},
		{`{"usage":{}}`, 0, false},
		{`{"id":"x","usage":{"total_tokens":7},"model":"gpt"}`, 7, true},
	}
	for _, tc := range cases {
		got, ok := ExtractUsage([]byte(tc.body))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseSSEData(t *testing.T) {
	content, usage, has := ParseSSEData([]byte(`{"choices":[{"delta":{"content":"你好"}}]}`))
	if content != "你好" || has || usage != 0 {
		t.Errorf("got (%q,%d,%v)", content, usage, has)
	}

	// reasoning_content 也必须计入（推理模型会产生大量此类 token）
	content, _, _ = ParseSSEData([]byte(`{"choices":[{"delta":{"reasoning_content":"think"}}]}`))
	if content != "think" {
		t.Errorf("reasoning_content must be counted, got %q", content)
	}

	// 末帧携带精确用量
	_, usage, has = ParseSSEData([]byte(`{"choices":[],"usage":{"total_tokens":88}}`))
	if !has || usage != 88 {
		t.Errorf("usage frame: got (%d,%v)", usage, has)
	}

	// [DONE] 与非 JSON 必须安全忽略
	for _, s := range []string{"[DONE]", "", "garbage", "  "} {
		if c, u, h := ParseSSEData([]byte(s)); c != "" || u != 0 || h {
			t.Errorf("%q should be ignored, got (%q,%d,%v)", s, c, u, h)
		}
	}

	// 兼容 completions 协议的 text 字段
	content, _, _ = ParseSSEData([]byte(`{"choices":[{"text":"legacy"}]}`))
	if content != "legacy" {
		t.Errorf("text field must be counted, got %q", content)
	}
}

func TestEstimateTokens(t *testing.T) {
	// CJK 约 1 字符 1 token，显著高于 ASCII 密度
	cjk := EstimateTokens("你好世界你好世界", 0)
	ascii := EstimateTokens("hello world hello", 0)
	if cjk <= ascii {
		t.Errorf("CJK should estimate higher per-char than ASCII: cjk=%d ascii=%d", cjk, ascii)
	}
	if EstimateTokens("", 0) != 0 {
		t.Error("empty string must be 0")
	}
	// 非空串至少 1 token，避免累加时被抹平
	if EstimateTokens("a", 0) < 1 {
		t.Error("non-empty string must be at least 1 token")
	}
}

func TestParseHeaderUsage(t *testing.T) {
	if n, ok := ParseHeaderUsage(" 42 "); !ok || n != 42 {
		t.Errorf("got (%d,%v)", n, ok)
	}
	for _, s := range []string{"", "abc", "-1", "0"} {
		if _, ok := ParseHeaderUsage(s); ok {
			t.Errorf("%q must be rejected", s)
		}
	}
}

// TestFinalWithoutExact 上游不返回 usage 时用估算×buffer 兜底
func TestFinalWithoutExact(t *testing.T) {
	var c Counter
	c.AddEstimate(100)
	if got := c.Final(1.2); got != 120 {
		t.Fatalf("want 120, got %d", got)
	}
	if c.HasExact() {
		t.Error("HasExact must be false")
	}
}
