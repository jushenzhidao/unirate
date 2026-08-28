package meter

import "testing"

// TestExtractUsageBreakdownSSEFrame 守住 SSE 路径的分方向记账。
//
// 这条链路曾经静默失效：SSE 帧里的 usage 已被解析用于精确核销，
// 但 prompt/completion 拆解没有往指标层传，看板上分方向计数恒为空。
// 缺陷不可见的原因是总量核销完全正确，只有分项缺失。
func TestExtractUsageBreakdownSSEFrame(t *testing.T) {
	// 剥掉 "data: " 前缀后的裸 JSON —— 与 stripDataPrefix 的输出形态一致
	frame := []byte(`{"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`)

	bd, ok := ExtractUsageBreakdown(frame)
	if !ok {
		t.Fatal("SSE usage frame must be recognized")
	}
	if !bd.Split {
		t.Fatal("Split must be true when upstream gives prompt/completion")
	}
	if bd.Prompt != 7 || bd.Completion != 11 || bd.Total != 18 {
		t.Fatalf("got prompt=%d completion=%d total=%d, want 7/11/18",
			bd.Prompt, bd.Completion, bd.Total)
	}
}

// TestExtractUsageBreakdownTotalOnly 只有总量时不得伪造分项。
//
// 用估算值填充 prompt/completion 会让「补全占比」这类派生指标变成噪声，
// 所以 Split 必须为 false，调用方据此跳过分方向计数。
func TestExtractUsageBreakdownTotalOnly(t *testing.T) {
	bd, ok := ExtractUsageBreakdown([]byte(`{"usage":{"total_tokens":42}}`))
	if !ok {
		t.Fatal("total-only usage must still be extracted")
	}
	if bd.Split {
		t.Fatal("Split must be false without a trustworthy breakdown")
	}
	if bd.Total != 42 {
		t.Fatalf("total = %d, want 42", bd.Total)
	}
	if bd.Prompt != 0 || bd.Completion != 0 {
		t.Fatalf("must not synthesize breakdown, got %d/%d", bd.Prompt, bd.Completion)
	}
}

// TestExtractUsageBreakdownDerivesTotal 缺 total_tokens 时由分项求和补齐。
func TestExtractUsageBreakdownDerivesTotal(t *testing.T) {
	bd, ok := ExtractUsageBreakdown([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":9}}`))
	if !ok {
		t.Fatal("breakdown without total must be accepted")
	}
	if bd.Total != 14 {
		t.Fatalf("total = %d, want 14 derived from parts", bd.Total)
	}
}

// TestExtractUsageBreakdownRejects 非 usage 帧与空用量必须被拒绝，
// 否则每个内容帧都会触发一次无意义的指标写入。
func TestExtractUsageBreakdownRejects(t *testing.T) {
	cases := map[string][]byte{
		"content frame": []byte(`{"choices":[{"delta":{"content":"hi"}}]}`),
		"done sentinel": []byte(`[DONE]`),
		"empty usage":   []byte(`{"usage":{}}`),
		"malformed":     []byte(`{"usage":`),
	}
	for name, body := range cases {
		if _, ok := ExtractUsageBreakdown(body); ok {
			t.Errorf("%s: must not be treated as usage", name)
		}
	}
}
