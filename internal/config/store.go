package config

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/unirate/gateway/internal/limiter"
)

// 配置中心（对应评审 P0-7 修正）
//
// 原设计缺陷：§2.2 说「Redis / MySQL 配置面板」，§3.3.6 说「从 Redis/MySQL 拉取」，
// §7.1 又推荐「etcd + viper」—— 三套系统并存，谁是权威、谁是缓存没有定论，
// 双写一致性与失效广播机制全文未提。
//
// 本实现收敛为单一链路：
//
//	MySQL (Source of Truth，Admin 唯一写入口)
//	   │  写入后 bump 版本号 + Pub/Sub 广播
//	   ▼
//	Redis (读取层 + 变更通知)
//	   │  订阅失效事件；另有轮询兜底（防止 Pub/Sub 消息丢失）
//	   ▼
//	网关本地缓存 (atomic.Pointer 无锁读)
//
// etcd 已删除 —— 该场景下它与 Redis 职责完全重叠，是多余的运维负担。

const (
	redisKeySnapshot = "unirate:config:snapshot"
	redisKeyVersion  = "unirate:config:version"
	redisChanInvalid = "unirate:config:invalidate"
)

// BizConfig 单个业务域的完整配置
type BizConfig struct {
	Biz             string          `json:"biz"`
	BaseURL         string          `json:"base_url"`
	StripPathPrefix bool            `json:"path_strip_prefix"`
	Enabled         bool            `json:"enabled"`
	Rules           []*limiter.Rule `json:"rules"`
	TokenMetering   *TokenMetering  `json:"token_metering,omitempty"`
}

// TokenMetering Token 计量配置
type TokenMetering struct {
	Mode          string  `json:"mode"`           // auto | json_body | sse_estimate | header | disabled
	JSONPath      string  `json:"json_path"`      // 默认 usage.total_tokens
	HeaderName    string  `json:"header_name"`    // 模式 header 时的响应头名
	EstimateRatio float64 `json:"estimate_ratio"` // 字符→token 估算系数
	SafetyBuffer  float64 `json:"safety_buffer"`  // 预扣安全系数
}

// DefaultTokenMetering 默认计量配置
func DefaultTokenMetering() *TokenMetering {
	return &TokenMetering{
		Mode:          "auto",
		JSONPath:      "usage.total_tokens",
		HeaderName:    "X-Usage-Tokens",
		EstimateRatio: 0.4,
		SafetyBuffer:  1.2,
	}
}

// Snapshot 全量配置快照
type Snapshot struct {
	Version int64                 `json:"version"`
	Bizs    map[string]*BizConfig `json:"bizs"`
	// PolicyOverrides Tier 1 运行策略的页面覆盖项（原始字符串形式）。
	// 存原始值而非解析后的 Policy，是为了让各实例本地按
	// 「页面 > 自身环境变量 > 默认」解析 —— 见 policy_store.go 顶部说明。
	PolicyOverrides map[string]string `json:"policy_overrides,omitempty"`
	LoadedAt        time.Time         `json:"loaded_at"`
}

// Store 配置存储
type Store struct {
	db  *sql.DB
	rdb redis.UniversalClient
	log *slog.Logger

	// dbKind 仅用于日志可读性（"sqlite" / "mysql"）。
	// 空值时日志退化为中性的 "sot"，不影响任何行为。
	dbKind string

	cur atomic.Pointer[Snapshot]
	pol policyState

	// 配置中心不可用时保留最后有效配置继续服务（Spec §2.7）
	degraded atomic.Bool
	stopCh   chan struct{}
	once     sync.Once
}

// NewStore 创建配置存储
func NewStore(db *sql.DB, rdb redis.UniversalClient, log *slog.Logger) *Store {
	s := &Store{db: db, rdb: rdb, log: log, stopCh: make(chan struct{})}
	s.cur.Store(&Snapshot{Version: 0, Bizs: map[string]*BizConfig{}, LoadedAt: time.Now()})
	s.pol.base.Store(DefaultPolicy())
	s.pol.eff.Store(DefaultPolicy())
	return s
}

// SetDBKind 标注底层 SoT 的类型，仅用于日志。
// 与 NewStore 分离是为了不改动既有构造签名（多处调用与测试依赖它）。
func (s *Store) SetDBKind(kind string) { s.dbKind = kind }

// sotName 返回日志中使用的 SoT 名称。
func (s *Store) sotName() string {
	if s.dbKind == "" {
		return "sot"
	}
	return s.dbKind
}

// Current 返回当前配置快照（无锁读）
func (s *Store) Current() *Snapshot { return s.cur.Load() }

// Degraded 配置中心是否处于降级状态
func (s *Store) Degraded() bool { return s.degraded.Load() }

// Upstream 实现 upstream.ConfigSource
func (s *Store) Upstream(biz string) (string, bool, bool) {
	snap := s.cur.Load()
	c, ok := snap.Bizs[biz]
	if !ok || !c.Enabled || c.BaseURL == "" {
		return "", false, false
	}
	return c.BaseURL, c.StripPathPrefix, true
}

// Rules 返回指定 biz 生效的规则（含全局规则）
func (s *Store) Rules(biz string) []*limiter.Rule {
	snap := s.cur.Load()
	var out []*limiter.Rule
	if g, ok := snap.Bizs["*"]; ok {
		out = append(out, g.Rules...)
	}
	if c, ok := snap.Bizs[biz]; ok {
		out = append(out, c.Rules...)
	}
	return out
}

// Metering 返回 biz 的 Token 计量配置
func (s *Store) Metering(biz string) *TokenMetering {
	snap := s.cur.Load()
	if c, ok := snap.Bizs[biz]; ok && c.TokenMetering != nil {
		return c.TokenMetering
	}
	return DefaultTokenMetering()
}

// LoadFromMySQL 从 SoT 全量加载并发布到 Redis
func (s *Store) LoadFromMySQL(ctx context.Context) (*Snapshot, error) {
	if s.db == nil {
		return nil, fmt.Errorf("sot database not configured")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT biz, base_url, path_strip_prefix, enabled, rules_json, metering_json
		 FROM biz_config WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("query biz_config: %w", err)
	}
	defer rows.Close()

	snap := &Snapshot{Bizs: map[string]*BizConfig{}, LoadedAt: time.Now()}
	for rows.Next() {
		var (
			c            BizConfig
			rulesJSON    sql.NullString
			meteringJSON sql.NullString
		)
		if err := rows.Scan(&c.Biz, &c.BaseURL, &c.StripPathPrefix, &c.Enabled, &rulesJSON, &meteringJSON); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if rulesJSON.Valid && rulesJSON.String != "" {
			if err := json.Unmarshal([]byte(rulesJSON.String), &c.Rules); err != nil {
				// 单个 biz 配置损坏不应拖垮整体加载
				s.log.Error("invalid rules json, skipping biz", "biz", c.Biz, "err", err)
				continue
			}
		}
		if meteringJSON.Valid && meteringJSON.String != "" {
			var m TokenMetering
			if err := json.Unmarshal([]byte(meteringJSON.String), &m); err == nil {
				c.TokenMetering = &m
			}
		}
		// 配置校验必须在加载期完成，运行期再报错就晚了
		valid := c.Rules[:0]
		for _, r := range c.Rules {
			if err := r.Validate(); err != nil {
				s.log.Error("invalid rule, skipping", "biz", c.Biz, "rule", r.Name, "err", err)
				continue
			}
			valid = append(valid, r)
		}
		c.Rules = valid
		snap.Bizs[c.Biz] = &c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tier 1 运行策略与业务规则同一次加载、同一次发布，
	// 保证「一个 version 对应一份完整配置」，不出现规则新策略旧的撕裂状态。
	pov, err := loadPolicyOverrides(ctx, s.db)
	if err != nil {
		return nil, err
	}
	snap.PolicyOverrides = pov

	ver, err := s.rdb.Incr(ctx, redisKeyVersion).Result()
	if err != nil {
		ver = time.Now().Unix()
	}
	snap.Version = ver

	if err := s.publish(ctx, snap); err != nil {
		s.log.Warn("publish config to redis failed", "err", err)
	}
	s.cur.Store(snap)
	s.refreshPolicy(snap.PolicyOverrides)
	s.degraded.Store(false)
	return snap, nil
}

// publish 把快照写入 Redis 读取层并广播失效事件
func (s *Store) publish(ctx context.Context, snap *Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := s.rdb.Set(ctx, redisKeySnapshot, b, 0).Err(); err != nil {
		return err
	}
	return s.rdb.Publish(ctx, redisChanInvalid, snap.Version).Err()
}

// loadFromRedis 从读取层加载
func (s *Store) loadFromRedis(ctx context.Context) (*Snapshot, error) {
	b, err := s.rdb.Get(ctx, redisKeySnapshot).Bytes()
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	for _, c := range snap.Bizs {
		for _, r := range c.Rules {
			if err := r.Validate(); err != nil {
				return nil, fmt.Errorf("invalid rule %q: %w", r.Name, err)
			}
		}
	}
	snap.LoadedAt = time.Now()
	return &snap, nil
}

// Bootstrap 启动加载。优先 MySQL，失败则退到 Redis 读取层。
func (s *Store) Bootstrap(ctx context.Context) error {
	if s.db != nil {
		if _, err := s.LoadFromMySQL(ctx); err == nil {
			s.log.Info("config loaded from sot", "store", s.sotName(),
				"version", s.Current().Version, "bizs", len(s.Current().Bizs))
			return nil
		} else {
			s.log.Warn("load from sot failed, falling back to redis",
				"store", s.sotName(), "err", err)
		}
	}
	snap, err := s.loadFromRedis(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap config: %w", err)
	}
	s.apply(snap)
	s.log.Info("config loaded from redis", "version", snap.Version, "bizs", len(snap.Bizs))
	return nil
}

// apply 原子替换快照并同步 Tier 1 策略。
// 所有「拿到新快照」的路径都必须走这里，漏一处就会出现
// 规则已更新而运行策略仍是旧值的撕裂状态。
func (s *Store) apply(snap *Snapshot) {
	s.cur.Store(snap)
	s.refreshPolicy(snap.PolicyOverrides)
}

// Watch 启动配置热更新。
// Pub/Sub 负责秒级生效，轮询作为兜底 —— Pub/Sub 不保证投递，
// 仅依赖它会导致某些实例长期使用过期配置。
func (s *Store) Watch(ctx context.Context, pollInterval time.Duration) {
	if pollInterval <= 0 {
		pollInterval = 15 * time.Second
	}
	go s.subscribe(ctx)
	go s.poll(ctx, pollInterval)
}

func (s *Store) subscribe(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		default:
		}
		sub := s.rdb.Subscribe(ctx, redisChanInvalid)
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				_ = sub.Close()
				return
			case msg, ok := <-ch:
				if !ok {
					_ = sub.Close()
					time.Sleep(time.Second)
					goto reconnect
				}
				_ = msg
				if snap, err := s.loadFromRedis(ctx); err == nil {
					if snap.Version >= s.cur.Load().Version {
						s.apply(snap)
						s.log.Info("config hot-reloaded via pubsub", "version", snap.Version)
					}
				}
			}
		}
	reconnect:
	}
}

// poll 兜底轮询。
//
// 间隔本身是 Tier 1 可热更新项，因此每轮重新读取生效值并重置定时器 ——
// 若用固定 Ticker，改了 config_poll_interval 只能等重启才生效，
// 而"改了没反应"正是配置热更新最容易踩的坑。
func (s *Store) poll(ctx context.Context, interval time.Duration) {
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-t.C:
			if d := s.Policy().ConfigPollInterval.D(); d > 0 {
				interval = d
			}
			t.Reset(interval)
			ver, err := s.rdb.Get(ctx, redisKeyVersion).Int64()
			if err != nil {
				// 配置中心不可达 → 保留最后有效配置继续服务（Spec §2.7）
				s.degraded.Store(true)
				continue
			}
			s.degraded.Store(false)
			if ver > s.cur.Load().Version {
				if snap, err := s.loadFromRedis(ctx); err == nil {
					s.apply(snap)
					s.log.Info("config hot-reloaded via poll", "version", snap.Version)
				}
			}
		}
	}
}

// Close 停止后台任务
func (s *Store) Close() {
	s.once.Do(func() { close(s.stopCh) })
}
