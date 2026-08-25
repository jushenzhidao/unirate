package config

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func quietStore() *Store {
	return NewStore(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestStorePolicyDefaultsBeforeBootstrap Bootstrap 之前读策略不得返回 nil。
// 网关在配置加载完成前已经开始接受请求（/live 已通），
// 此时 Policy() 返回 nil 会直接 panic 掉整个代理。
func TestStorePolicyDefaultsBeforeBootstrap(t *testing.T) {
	s := quietStore()
	p := s.Policy()
	if p == nil {
		t.Fatal("Policy() 在 Bootstrap 前不得返回 nil")
	}
	if p.MaxRequestBodyMB != 32 || p.LogLevel != "info" {
		t.Errorf("Bootstrap 前应返回内置默认值，实际 %+v", p)
	}
	if s.PolicyBase() == nil {
		t.Fatal("PolicyBase() 不得返回 nil")
	}
}

// TestSetPolicyBaseAppliesEnvLayer SetPolicyBase 必须立刻影响生效值。
// 若只存不重算，环境变量会在 Bootstrap 之前的窗口内被忽略。
func TestSetPolicyBaseAppliesEnvLayer(t *testing.T) {
	s := quietStore()

	base := DefaultPolicy()
	base.LogLevel = "error"
	base.Instances = 4
	s.SetPolicyBase(base)

	if got := s.Policy(); got.LogLevel != "error" || got.Instances != 4 {
		t.Errorf("SetPolicyBase 未立即生效：实际 log_level=%s instances=%d",
			got.LogLevel, got.Instances)
	}
	// nil 应回落到默认而非 panic
	s.SetPolicyBase(nil)
	if s.Policy().LogLevel != "info" {
		t.Errorf("nil base 应回落到默认值，实际 %s", s.Policy().LogLevel)
	}
}

// TestRefreshPolicyNotifiesOnlyOnChange 回调只在值真正变化时触发。
//
// 兜底轮询每 15s 会重新解析一次配置；若每次都通知，
// LOG_LEVEL 会被反复 Set、日志里也会反复出现 "policy updated"，
// 把真实变更淹没在噪声里。
func TestRefreshPolicyNotifiesOnlyOnChange(t *testing.T) {
	s := quietStore()

	var mu sync.Mutex
	var seen []string
	s.OnPolicyChange(func(p *Policy) {
		mu.Lock()
		seen = append(seen, p.LogLevel)
		mu.Unlock()
	})

	// 注册时立即回调一次当前值，避免调用方自己处理「注册前已变更」的竞态
	mu.Lock()
	initial := len(seen)
	mu.Unlock()
	if initial != 1 {
		t.Fatalf("注册时应立即回调一次，实际 %d 次", initial)
	}

	s.refreshPolicy(map[string]string{KeyLogLevel: "debug"})
	s.refreshPolicy(map[string]string{KeyLogLevel: "debug"}) // 同值，不应再通知
	s.refreshPolicy(map[string]string{KeyLogLevel: "warn"})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Errorf("期望 3 次回调（初始 + debug + warn），实际 %d: %v", len(seen), seen)
	}
	if seen[len(seen)-1] != "warn" {
		t.Errorf("最后一次回调应为 warn，实际 %s", seen[len(seen)-1])
	}
}

// TestPolicyOverridesReturnsCopy 返回副本，调用方改它不影响内部状态。
// admin 的 GET 会把这个 map 交出去，若是同一份引用，
// 序列化过程中的任何改动都会污染生效配置。
func TestPolicyOverridesReturnsCopy(t *testing.T) {
	s := quietStore()
	s.refreshPolicy(map[string]string{KeyLogLevel: "debug"})

	got := s.PolicyOverrides()
	got[KeyLogLevel] = "error"
	got["injected"] = "x"

	again := s.PolicyOverrides()
	if again[KeyLogLevel] != "debug" {
		t.Errorf("外部修改污染了内部覆盖项：实际 %s", again[KeyLogLevel])
	}
	if _, hit := again["injected"]; hit {
		t.Error("外部注入的键进入了内部状态")
	}
}

// TestOverrideRemovalFallsBackToEnv 删除覆盖项后必须回落到 env 层，
// **不是**回落到内置默认值。
//
// 这个区别很实际：env 把超时设成 30s、页面临时改成 5s，
// 排障结束点"重置"，应回到部署时的 30s。若回落到 60s 默认值，
// 等于悄悄改变了部署配置。
func TestOverrideRemovalFallsBackToEnv(t *testing.T) {
	s := quietStore()

	base := DefaultPolicy()
	base.UpstreamTimeout = Dur(30 * time.Second)
	s.SetPolicyBase(base)

	s.refreshPolicy(map[string]string{KeyUpstreamTimeout: "5s"})
	if s.Policy().UpstreamTimeout.D() != 5*time.Second {
		t.Fatalf("页面覆盖未生效：%s", s.Policy().UpstreamTimeout.D())
	}

	s.refreshPolicy(nil) // 覆盖被清除
	if got := s.Policy().UpstreamTimeout.D(); got != 30*time.Second {
		t.Errorf("清除覆盖后应回落到 env 值 30s，实际 %s（回落到默认值会悄悄改变部署配置）", got)
	}
}

// TestConcurrentPolicyRefreshAndRead 并发刷新与读取不得竞争（需 -race）
func TestConcurrentPolicyRefreshAndRead(t *testing.T) {
	s := quietStore()
	s.OnPolicyChange(func(*Policy) {})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		lv := []string{"debug", "info", "warn", "error"}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.refreshPolicy(map[string]string{KeyLogLevel: lv[i%len(lv)]})
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				if s.Policy() == nil {
					t.Error("并发读取时 Policy() 返回 nil")
					return
				}
				_ = s.PolicyOverrides()
			}
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestIsMissingTable 表不存在必须被识别为「无覆盖项」而非致命错误。
//
// 老部署升级上来时 runtime_config 尚未创建。此时若报错，
// 会连带让 biz_config 的加载失败（两者共用一次 LoadFromMySQL），
// 结果是「加了个新功能，老部署起不来」。
func TestIsMissingTable(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Error 1146 (42S02): Table 'unirate.runtime_config' doesn't exist", true},
		{"Table 'unirate.runtime_config' doesn't exist", true},
		{"Error 1045: Access denied for user", false},
		{"dial tcp 127.0.0.1:3306: connect: connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		var err error
		if tc.msg != "" {
			err = errString(tc.msg)
		}
		if got := isMissingTable(err); got != tc.want {
			t.Errorf("isMissingTable(%q) = %v，期望 %v", tc.msg, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
