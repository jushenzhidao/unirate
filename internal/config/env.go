package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// 运行时配置全部来自环境变量（12-Factor），避免再引入配置文件解析层。
// 与「业务限流规则」区分开：规则是动态数据（MySQL SoT），这里是进程启动参数。

// Runtime 进程级运行配置
type Runtime struct {
	ProxyAddr string
	AdminAddr string
	ObsAddr   string

	RedisAddrs    []string
	RedisPassword string
	RedisDB       int
	RedisPoolSize int
	RedisTimeout  time.Duration

	MySQLDSN string

	AdminToken      string
	AdminAllowCIDRs []string

	TZOffsetSeconds int64
	Instances       int

	UpstreamTimeout time.Duration
	TokenFlushEvery time.Duration
	MaxRequestBody  int64

	TrustedProxyHops int
	RealIPHeader     string
	TokenHeaders     []string

	AllowHeaderUpstream bool
	UpstreamAllowlist   []string
	ExposeRuleName      bool

	ConfigPollInterval time.Duration
	ShutdownGrace      time.Duration
	LogLevel           string
	Version            string
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envList(k string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadRuntime 从环境变量加载运行配置
func LoadRuntime() *Runtime {
	return &Runtime{
		ProxyAddr: env("PROXY_ADDR", ":8080"),
		// Admin 默认只监听回环，必须显式改成 0.0.0.0 才能跨容器访问，
		// 防止「一键部署」把管理面直接暴露到公网（评审 P0-3）
		AdminAddr: env("ADMIN_ADDR", "127.0.0.1:9090"),
		ObsAddr:   env("OBS_ADDR", ":9091"),

		RedisAddrs:    envList("REDIS_ADDRS", []string{env("REDIS_ADDR", "127.0.0.1:6379")}),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		RedisDB:       envInt("REDIS_DB", 0),
		RedisPoolSize: envInt("REDIS_POOL_SIZE", 256),
		RedisTimeout:  envDur("REDIS_TIMEOUT", 200*time.Millisecond),

		MySQLDSN: os.Getenv("MYSQL_DSN"),

		AdminToken:      os.Getenv("ADMIN_TOKEN"),
		AdminAllowCIDRs: envList("ADMIN_ALLOW_CIDRS", nil),

		TZOffsetSeconds: int64(envInt("TZ_OFFSET_SECONDS", 8*3600)),
		Instances:       envInt("INSTANCES", 1),

		UpstreamTimeout: envDur("UPSTREAM_TIMEOUT", 60*time.Second),
		TokenFlushEvery: envDur("TOKEN_FLUSH_INTERVAL", time.Second),
		MaxRequestBody:  int64(envInt("MAX_REQUEST_BODY_MB", 32)) << 20,

		TrustedProxyHops: envInt("TRUSTED_PROXY_HOPS", 0),
		RealIPHeader:     env("REAL_IP_HEADER", ""),
		TokenHeaders:     envList("TOKEN_HEADERS", []string{"Authorization", "X-Api-Key"}),

		AllowHeaderUpstream: envBool("ALLOW_HEADER_UPSTREAM", false),
		UpstreamAllowlist:   envList("UPSTREAM_ALLOWLIST", nil),
		ExposeRuleName:      envBool("EXPOSE_RULE_NAME", true),

		ConfigPollInterval: envDur("CONFIG_POLL_INTERVAL", 15*time.Second),
		ShutdownGrace:      envDur("SHUTDOWN_GRACE", 30*time.Second),
		LogLevel:           env("LOG_LEVEL", "info"),
		Version:            env("VERSION", "dev"),
	}
}
