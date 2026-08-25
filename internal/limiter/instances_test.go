package limiter

import (
	"sync"
	"testing"
)

// TestLocalQuotaMath 降级期间的单实例保守配额 = 总配额 ÷ 实例数。
//
// 这段逻辑属于典型的沉默逻辑错误高发区：算错不报错、不崩溃，
// 只是 Redis 故障期间的限流精度悄悄失准 —— 而那正是最不希望出错的时刻。
// 因此这里逐个边界断言，不依赖"跑一遍看着对"。
func TestLocalQuotaMath(t *testing.T) {
	cases := []struct {
		name      string
		instances int
		limit     int64
		want      int64
		why       string
	}{
		{"单实例取全量配额", 1, 100, 100, ""},
		{"四实例均分", 4, 100, 25, ""},
		{"整除边界", 5, 100, 20, ""},
		{"非整除向下取整", 3, 100, 33, "向下取整偏保守，宁可少放也不超卖"},
		{"实例数大于配额时钳到 1", 200, 100, 1,
			"配额为 0 会让降级期间全量拒绝，比超卖更糟（服务直接不可用）"},
		{"配额为 1 且多实例", 10, 1, 1, "同上，下界钳到 1"},
		{"配额为 0", 4, 0, 1, "钳到 1，不得返回 0"},
	}
	for _, tc := range cases {
		l := New(nil, Options{Instances: tc.instances})
		if got := l.localQuota(tc.limit); got != tc.want {
			t.Errorf("%s: instances=%d limit=%d → 期望 %d，实际 %d  %s",
				tc.name, tc.instances, tc.limit, tc.want, got, tc.why)
		}
	}
}

// TestLocalQuotaNeverZeroOrNegative 无论实例数被设成什么，配额都不得 ≤ 0。
//
// 配额 0 意味着降级期间拒绝一切请求；负数会让 allowQuota 的比较逻辑
// 行为未定义。这两种都是比"限流不准"严重得多的故障。
func TestLocalQuotaNeverZeroOrNegative(t *testing.T) {
	l := New(nil, Options{Instances: 1})
	for _, inst := range []int{-100, -1, 0, 1, 7, 1024, 100000} {
		l.SetInstances(inst) // 非正值应被忽略
		for _, limit := range []int64{0, 1, 10, 5000, 1 << 40} {
			if q := l.localQuota(limit); q < 1 {
				t.Fatalf("instances=%d limit=%d 得到非法配额 %d", inst, limit, q)
			}
		}
	}
}

// TestNewClampsNonPositiveInstances 构造期非正实例数必须被钳到 1，
// 否则 localQuota 会除零 panic。
func TestNewClampsNonPositiveInstances(t *testing.T) {
	for _, inst := range []int{-5, 0} {
		l := New(nil, Options{Instances: inst})
		if got := l.Instances(); got != 1 {
			t.Errorf("Options.Instances=%d 应被钳到 1，实际 %d", inst, got)
		}
		// 不 panic 即通过（除零会在这里炸）
		if q := l.localQuota(100); q != 100 {
			t.Errorf("钳到 1 后应取全量配额 100，实际 %d", q)
		}
	}
}

// TestSetInstancesIgnoresNonPositive 热更新路径同样要防除零。
//
// instances 是 Tier 1 可热改项，虽然 API 层有 min=1 校验，
// 但这一层不能依赖上层校验 —— 防御要放在会除零的地方。
func TestSetInstancesIgnoresNonPositive(t *testing.T) {
	l := New(nil, Options{Instances: 8})

	for _, bad := range []int{0, -1, -999} {
		l.SetInstances(bad)
		if got := l.Instances(); got != 8 {
			t.Errorf("SetInstances(%d) 应被忽略，实例数不应从 8 变成 %d", bad, got)
		}
	}

	l.SetInstances(3)
	if got := l.Instances(); got != 3 {
		t.Errorf("合法值应生效：期望 3，实际 %d", got)
	}
	if q := l.localQuota(90); q != 30 {
		t.Errorf("热更新后配额应随之变化：期望 30，实际 %d", q)
	}
}

// TestConcurrentSetInstancesAndQuota 并发热更新与配额计算不得竞争（需 -race）。
// 降级路径可能与配置更新同时发生，这是真实存在的并发面。
func TestConcurrentSetInstancesAndQuota(t *testing.T) {
	l := New(nil, Options{Instances: 1})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			l.SetInstances(i%16 + 1)
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				if q := l.localQuota(1000); q < 1 {
					t.Errorf("并发期间出现非法配额 %d", q)
					return
				}
				_ = l.Instances()
			}
		}()
	}
	close(stop)
	wg.Wait()
}
