package proxy

import (
	"sync"
	"testing"
	"time"

	"github.com/unirate/gateway/internal/config"
	"github.com/unirate/gateway/internal/meta"
)

func newBareHandler(opt Options) *Handler {
	h := &Handler{}
	h.opt.Store(&opt)
	return h
}

// TestApplyPolicyUpdatesTier1Fields Tier 1 项必须真的被写进生效配置。
//
// 这是本次改动的核心断言：ApplyPolicy 少赋一个字段不会报错、不会崩，
// 只是那一项在页面上改了没反应 —— 典型的沉默失效。
func TestApplyPolicyUpdatesTier1Fields(t *testing.T) {
	h := newBareHandler(DefaultOptions())

	p := config.DefaultPolicy()
	p.UpstreamTimeout = config.Dur(7 * time.Second)
	p.TokenFlushInterval = config.Dur(250 * time.Millisecond)
	p.MaxRequestBodyMB = 8
	p.ExposeRuleName = false
	p.Instances = 5

	h.ApplyPolicy(p)
	got := h.Options()

	if got.UpstreamTimeout != 7*time.Second {
		t.Errorf("upstream_timeout 未生效：期望 7s，实际 %s", got.UpstreamTimeout)
	}
	if got.TokenFlushEvery != 250*time.Millisecond {
		t.Errorf("token_flush_interval 未生效：期望 250ms，实际 %s", got.TokenFlushEvery)
	}
	if got.MaxRequestBody != 8<<20 {
		t.Errorf("max_request_body_mb 未生效或换算错误：期望 %d 字节，实际 %d", 8<<20, got.MaxRequestBody)
	}
	if got.ExposeRuleName {
		t.Error("expose_rule_name=false 未生效")
	}
	if got.Instances != 5 {
		t.Errorf("instances 未生效：期望 5，实际 %d", got.Instances)
	}
}

// TestApplyPolicyPreservesTier0Fields ApplyPolicy 只能碰 Tier 1 项。
//
// MetaConfig 决定 IP / token 维度如何提取，属 CONFIG-TIERING.md
// 「明确不搬」的项；被热更新路径覆盖会导致限流维度整体错乱，
// 而且不会有任何报错 —— 只是所有请求突然被算到同一个 IP 上。
func TestApplyPolicyPreservesTier0Fields(t *testing.T) {
	opt := DefaultOptions()
	opt.MetaConfig.TrustedProxyHops = 3
	opt.MetaConfig.RealIPHeader = "X-Real-Ip"
	opt.MetaConfig.TokenHeaders = []string{"X-Custom-Key"}
	opt.SSEIdleTimeout = 42 * time.Second

	h := newBareHandler(opt)
	h.ApplyPolicy(config.DefaultPolicy())
	got := h.Options()

	if got.MetaConfig.TrustedProxyHops != 3 {
		t.Errorf("TrustedProxyHops 被热更新篡改：期望 3，实际 %d", got.MetaConfig.TrustedProxyHops)
	}
	if got.MetaConfig.RealIPHeader != "X-Real-Ip" {
		t.Errorf("RealIPHeader 被热更新篡改：实际 %q", got.MetaConfig.RealIPHeader)
	}
	if len(got.MetaConfig.TokenHeaders) != 1 || got.MetaConfig.TokenHeaders[0] != "X-Custom-Key" {
		t.Errorf("TokenHeaders 被热更新篡改：实际 %v", got.MetaConfig.TokenHeaders)
	}
	if got.SSEIdleTimeout != 42*time.Second {
		t.Errorf("SSEIdleTimeout 不在 Tier 1 范围，不应被改：实际 %s", got.SSEIdleTimeout)
	}
}

// TestApplyPolicyNilIsNoop nil 策略不得清空既有配置。
// 订阅回调在极端时序下可能拿到 nil，此时保持原值远好过归零 ——
// 归零意味着 MaxRequestBody=0（拒绝所有带体请求）。
func TestApplyPolicyNilIsNoop(t *testing.T) {
	opt := DefaultOptions()
	opt.UpstreamTimeout = 11 * time.Second
	h := newBareHandler(opt)

	h.ApplyPolicy(nil)

	if h.Options().UpstreamTimeout != 11*time.Second {
		t.Errorf("nil 策略必须是 no-op，实际把超时改成了 %s", h.Options().UpstreamTimeout)
	}
}

// TestOptionsSnapshotIsCopy Options() 返回的是副本，改它不影响生效值。
// 若返回的是共享指针内容，调用方无意的修改会绕过原子替换、
// 让并发读到撕裂状态。
func TestOptionsSnapshotIsCopy(t *testing.T) {
	h := newBareHandler(DefaultOptions())

	snap := h.Options()
	snap.UpstreamTimeout = 999 * time.Second
	snap.ExposeRuleName = false

	if h.Options().UpstreamTimeout == 999*time.Second {
		t.Error("Options() 返回值被外部修改后影响了生效配置")
	}
	if !h.Options().ExposeRuleName {
		t.Error("Options() 返回值被外部修改后影响了生效配置")
	}
}

// TestConcurrentPolicyUpdateAndRead 并发热更新与读取不得数据竞争，
// 且任一时刻读到的必须是**某一次完整替换**的结果，不能是字段混合。
//
// 这条断言比"不 panic"更强：整份替换的意义就在于不出现
// "新超时 + 旧体积上限"这种从未被任何人配置过的组合。
// 需配合 -race 运行。
func TestConcurrentPolicyUpdateAndRead(t *testing.T) {
	h := newBareHandler(DefaultOptions())

	// 两组自洽配置：超时与体积上限成对变化，便于检测撕裂。
	// 合法组合只有这两对 —— 加上初始默认值 60s/32MB 共三种。
	pA := config.DefaultPolicy()
	pA.UpstreamTimeout = config.Dur(10 * time.Second)
	pA.MaxRequestBodyMB = 10

	pB := config.DefaultPolicy()
	pB.UpstreamTimeout = config.Dur(20 * time.Second)
	pB.MaxRequestBodyMB = 20

	valid := map[[2]int]bool{
		{60, 32}: true, // 初始默认值（首次 ApplyPolicy 之前可能被读到）
		{10, 10}: true,
		{20, 20}: true,
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				h.ApplyPolicy(pA)
			} else {
				h.ApplyPolicy(pB)
			}
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				o := h.Options()
				secs := int(o.UpstreamTimeout / time.Second)
				mb := int(o.MaxRequestBody >> 20)
				// 每次读到的必须是某一次完整替换的结果。
				// 出现 10s+20MB 这种从未被配置过的组合，说明发生了逐字段撕裂。
				if !valid[[2]int{secs, mb}] {
					t.Errorf("配置撕裂：upstream_timeout=%ds 配 max_request_body=%dMB，"+
						"该组合从未被写入过", secs, mb)
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestExposeRuleNameGovernsHeaderLeak expose_rule_name 是外网部署的信息泄露开关，
// 热更新后必须立刻改变 429 响应形态 —— 否则"关了还在泄露"。
func TestExposeRuleNameGovernsHeaderLeak(t *testing.T) {
	opt := DefaultOptions()
	opt.MetaConfig = meta.DefaultConfig()

	h := newBareHandler(opt)
	if !h.Options().ExposeRuleName {
		t.Fatal("默认应为 true（内网排障友好）")
	}

	p := config.DefaultPolicy()
	p.ExposeRuleName = false
	h.ApplyPolicy(p)

	if h.Options().ExposeRuleName {
		t.Error("关闭 expose_rule_name 后生效配置仍为 true，429 响应会继续泄露规则名")
	}
}
