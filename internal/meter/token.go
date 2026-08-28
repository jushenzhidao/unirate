package meter

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// Token 计量（对应评审 P0-6 修正的采集侧）
//
// 计量精度取舍说明：
// Spec §7.1 提出「Go 自研 BPE 分词器」，评审 Advisory-6 建议改用现成移植库。
// 本实现采用第三条路：基于字符类型的加权估算 + 上游精确值核销。
// 理由：
//   1. 引入 tiktoken 移植库需内嵌数 MB 的 BPE 词表，且不同模型词表不同，
//      网关作为通用组件不应假设上游模型；
//   2. 估算值只用于「SSE 期间的临时预扣」，SSE 结束后一律用上游 usage 核销退差，
//      最终账本精度取决于上游而非估算器；
//   3. 上游不返回 usage 时才依赖估算，此时误差由 safety_buffer 覆盖并可通过校准因子收敛。
// 结论：分词器精度不在关键路径上，不值得付出词表体积与维护成本。

// EstimateTokens 按字符类型加权估算 token 数。
//
// 经验系数（基于 cl100k_base 的统计特征）：
//   - ASCII 字符：约 4 字符 = 1 token
//   - CJK 字符：约 1 字符 = 1 token（中文单字通常独占一个 token）
//   - 其他多字节字符：约 2 字符 = 1 token
func EstimateTokens(s string, ratio float64) int64 {
	if s == "" {
		return 0
	}
	var ascii, cjk, other int
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			ascii++
		case isCJK(r):
			cjk++
		default:
			other++
		}
	}
	est := float64(ascii)/4.0 + float64(cjk) + float64(other)/2.0
	if ratio > 0 {
		// ratio 作为校准因子微调（§2.4.6 校准机制的落点）
		est *= ratio / 0.4
	}
	if est < 1 && len(s) > 0 {
		return 1
	}
	return int64(math.Ceil(est))
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK 统一表意
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0x3040 && r <= 0x30FF) || // 日文假名
		(r >= 0xAC00 && r <= 0xD7AF) || // 韩文
		(r >= 0xFF00 && r <= 0xFFEF) // 全角
}

// Counter 线程安全的 token 累加器
type Counter struct {
	estimated atomic.Int64 // 估算累计
	exact     atomic.Int64 // 上游返回的精确值
	hasExact  atomic.Bool
	flushed   atomic.Int64 // 已刷入 Redis 的量
}

// AddEstimate 累加估算值
func (c *Counter) AddEstimate(n int64) { c.estimated.Add(n) }

// SetExact 设置上游返回的精确值
func (c *Counter) SetExact(n int64) {
	c.exact.Store(n)
	c.hasExact.Store(true)
}

// HasExact 是否拿到了精确值
func (c *Counter) HasExact() bool { return c.hasExact.Load() }

// Estimated 当前估算总量
func (c *Counter) Estimated() int64 { return c.estimated.Load() }

// Exact 精确值
func (c *Counter) Exact() int64 { return c.exact.Load() }

// Flushed 已预扣量
func (c *Counter) Flushed() int64 { return c.flushed.Load() }

// PendingFlush 返回本次需要增量刷盘的量并记账。
// buffer 为安全系数（如 1.2），仅作用于估算值。
func (c *Counter) PendingFlush(buffer float64) int64 {
	if buffer <= 0 {
		buffer = 1.0
	}
	want := int64(math.Ceil(float64(c.estimated.Load()) * buffer))
	done := c.flushed.Load()
	if want <= done {
		return 0
	}
	delta := want - done
	c.flushed.Add(delta)
	return delta
}

// Final 返回最终应记账的 token 数。
// 有上游精确值时以精确值为准，否则用估算值 × buffer。
func (c *Counter) Final(buffer float64) int64 {
	if c.hasExact.Load() {
		return c.exact.Load()
	}
	if buffer <= 0 {
		buffer = 1.0
	}
	return int64(math.Ceil(float64(c.estimated.Load()) * buffer))
}

// usagePayload 用于提取 OpenAI 兼容的 usage 字段
type usagePayload struct {
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// UsageBreakdown token 用量分项。
//
// Prompt/Completion 可能为 0 而 Total 有值：部分上游只回总量。
// 观测方必须区分「确实为 0」和「上游未提供」，否则分方向指标会凭空
// 少掉一半用量，得出「补全 token 占比 100%」这类错误结论。
type UsageBreakdown struct {
	Prompt     int64
	Completion int64
	Total      int64
	// Split 为 true 表示上游给出了可信的分项拆解。
	Split bool
}

// ExtractUsageBreakdown 与 ExtractUsage 走同一份解析结果，但保留分项。
//
// 独立成函数而非改动 ExtractUsage 签名，是为了不动已有调用点与其测试：
// 配额核对只关心总量，分项仅用于观测，两条用途不应互相牵连。
func ExtractUsageBreakdown(body []byte) (UsageBreakdown, bool) {
	var out UsageBreakdown
	if !bytes.Contains(body, []byte(`"usage"`)) {
		return out, false
	}
	var p usagePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return out, false
	}
	out.Prompt = p.Usage.PromptTokens
	out.Completion = p.Usage.CompletionTokens
	out.Split = out.Prompt > 0 || out.Completion > 0
	switch {
	case p.Usage.TotalTokens > 0:
		out.Total = p.Usage.TotalTokens
	case out.Split:
		out.Total = out.Prompt + out.Completion
	default:
		return out, false
	}
	return out, true
}

// ExtractUsage 从 JSON 响应体提取 token 用量。
// 只读取顶层 usage 字段，不解析 choices 等业务结构（保持透传语义）。
func ExtractUsage(body []byte) (int64, bool) {
	// 快速预筛：不含 "usage" 直接跳过 JSON 解析
	if !bytes.Contains(body, []byte(`"usage"`)) {
		return 0, false
	}
	var p usagePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, false
	}
	if p.Usage.TotalTokens > 0 {
		return p.Usage.TotalTokens, true
	}
	if sum := p.Usage.PromptTokens + p.Usage.CompletionTokens; sum > 0 {
		return sum, true
	}
	return 0, false
}

// sseDelta 用于提取流式增量内容
type sseDelta struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Text string `json:"text"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// ParseSSEData 解析单条 SSE data 行，返回增量文本与可能的精确用量。
func ParseSSEData(data []byte) (content string, usage int64, hasUsage bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return "", 0, false
	}
	if trimmed[0] != '{' {
		return "", 0, false
	}
	var d sseDelta
	if err := json.Unmarshal(trimmed, &d); err != nil {
		return "", 0, false
	}
	var sb strings.Builder
	for _, c := range d.Choices {
		if c.Delta.Content != "" {
			sb.WriteString(c.Delta.Content)
		}
		if c.Delta.ReasoningContent != "" {
			sb.WriteString(c.Delta.ReasoningContent)
		}
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	if d.Usage != nil {
		u := d.Usage.TotalTokens
		if u == 0 {
			u = d.Usage.PromptTokens + d.Usage.CompletionTokens
		}
		if u > 0 {
			return sb.String(), u, true
		}
	}
	return sb.String(), 0, false
}

// ParseHeaderUsage 从响应头提取用量
func ParseHeaderUsage(v string) (int64, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
