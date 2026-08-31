package config

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Tier 1 策略在 Store 上的读写与订阅。
//
// 与业务规则共用同一条发布链路（LoadFromDB → publish → Pub/Sub / poll），
// 因此这里没有任何独立的轮询或订阅逻辑 —— 复用是刻意的，
// 新造一套配置通道意味着两套一致性语义、两处可能不同步。
//
// Snapshot 里发布的是**覆盖项原始 map**而非解析后的 Policy。
// 原因：环境变量是每实例本地事实，各实例的 env 可以不同。
// 若发布解析结果，写入实例的 env 会随快照污染其它实例的生效值。

// PolicyListener 策略变更回调。在 Store 内部串行调用，实现方需快速返回。
type PolicyListener func(*Policy)

// policyState 挂在 Store 上的策略状态
type policyState struct {
	// base 由环境变量解析而来，属每实例本地事实，不参与 Redis 发布
	base atomic.Pointer[Policy]
	// eff 生效值（页面 > env > 默认）
	eff atomic.Pointer[Policy]
	// overrides 当前生效的页面覆盖项，供 API 做三态展示
	overrides atomic.Pointer[map[string]string]

	mu        sync.Mutex
	listeners []PolicyListener
}

// SetPolicyBase 注入环境变量解析出的 base 策略。
// 必须在 Bootstrap 之前调用；否则首次解析会以内置默认值为 base，
// 环境变量在启动窗口内被忽略。
func (s *Store) SetPolicyBase(base *Policy) {
	if base == nil {
		base = DefaultPolicy()
	}
	s.pol.base.Store(base)
	s.refreshPolicy(s.currentOverrides())
}

// Policy 返回当前生效策略（无锁读）
func (s *Store) Policy() *Policy {
	if p := s.pol.eff.Load(); p != nil {
		return p
	}
	return s.PolicyBase()
}

// PolicyBase 返回环境变量层解析结果（不含页面覆盖）
func (s *Store) PolicyBase() *Policy {
	if b := s.pol.base.Load(); b != nil {
		return b
	}
	return DefaultPolicy()
}

// PolicyOverrides 返回页面覆盖项的副本
func (s *Store) PolicyOverrides() map[string]string {
	out := map[string]string{}
	for k, v := range s.currentOverrides() {
		out[k] = v
	}
	return out
}

// OnPolicyChange 注册变更回调。注册时立即回调一次当前值，
// 免得调用方自己处理「注册前已发生过变更」这种竞态。
func (s *Store) OnPolicyChange(fn PolicyListener) {
	if fn == nil {
		return
	}
	s.pol.mu.Lock()
	s.pol.listeners = append(s.pol.listeners, fn)
	s.pol.mu.Unlock()
	fn(s.Policy())
}

func (s *Store) currentOverrides() map[string]string {
	if m := s.pol.overrides.Load(); m != nil {
		return *m
	}
	return nil
}

// refreshPolicy 用给定覆盖项重算生效值，仅在真正变化时通知监听者。
func (s *Store) refreshPolicy(overrides map[string]string) {
	eff, problems := ResolvePolicy(s.PolicyBase(), overrides)
	for _, p := range problems {
		// 单项非法只影响该项（回退到 env/默认），但必须留明确日志 ——
		// 静默忽略会让运维以为改动已生效
		s.log.Error("invalid runtime policy override, falling back", "problem", p)
	}
	cp := overrides
	s.pol.overrides.Store(&cp)

	old := s.pol.eff.Load()
	if old != nil && *old == *eff {
		return
	}
	s.pol.eff.Store(eff)

	s.pol.mu.Lock()
	ls := make([]PolicyListener, len(s.pol.listeners))
	copy(ls, s.pol.listeners)
	s.pol.mu.Unlock()
	for _, fn := range ls {
		fn(eff)
	}
	if old != nil {
		s.log.Info("runtime policy updated",
			"log_level", eff.LogLevel,
			"upstream_timeout", eff.UpstreamTimeout.D().String(),
			"token_flush_interval", eff.TokenFlushInterval.D().String(),
			"max_request_body_mb", eff.MaxRequestBodyMB,
			"config_poll_interval", eff.ConfigPollInterval.D().String(),
			"expose_rule_name", eff.ExposeRuleName,
			"instances", eff.Instances)
	}
}

// loadPolicyOverrides 从 SoT 读取覆盖项。
//
// 表不存在时返回空集而非报错：老部署升级上来时该表尚未创建，
// 此时应以 env/默认值正常服务，而不是让整个配置加载失败
// （biz_config 的规则加载与之共用一次 LoadFromDB）。
func loadPolicyOverrides(ctx context.Context, db *sql.DB) (map[string]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT cfg_key, cfg_value FROM runtime_config`)
	if err != nil {
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("query runtime_config: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("scan runtime_config: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isMissingTable 识别 SQLite 的「表不存在」错误（SQLITE_ERROR: no such table）。
//
// 用错误文本匹配而非 driver 类型断言：modernc.org/sqlite 把该错误包成
// 通用 *sqlite.Error，错误码需要额外类型依赖才能取到，而文本形态
// 由 SQLite 内核固定输出，稳定性足够。
func isMissingTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}
