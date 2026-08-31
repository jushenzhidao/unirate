package obs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// LogfireConfig 直接推送到 Logfire 的配置
type LogfireConfig struct {
	Token    string        // Logfire write token (required)
	Endpoint string        // Logfire OTLP endpoint
	Interval time.Duration // 推送间隔
	Env      string        // 环境标签
}

// InitLogfireExporter 初始化 Logfire 直接推送。
//
// 设计要点：
// 1. 零侵入热路径：OTel SDK 的 aggregation 运行在独立 goroutine，不阻塞业务请求
// 2. 兼容现有指标：保留 /metrics 端点供本地调试与兼容既有监控
// 3. 自动桥接：从自研 atomic.Int64 指标自动转换为 OTel Instrument
//
// 返回 shutdown 函数供 main 退出时调用，确保最后一批指标被推送。
// 若初始化失败则返回 error，此时不影响 /metrics 端点的暴露 ——
// 指标仍在内存中累加，只是不会推送到 Logfire。
func InitLogfireExporter(cfg LogfireConfig, log *slog.Logger) (shutdown func(context.Context) error, err error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("LOGFIRE_TOKEN is required")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://logfire-api.pydantic.dev/v1/metrics"
	}
	if cfg.Interval == 0 {
		cfg.Interval = 15 * time.Second
	}

	ctx := context.Background()

	// Resource attributes
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("unirate"),
			semconv.ServiceVersion(os.Getenv("VERSION")),
			semconv.DeploymentEnvironment(cfg.Env),
		),
		resource.WithHost(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, fmt.Errorf("resource.New: %w", err)
	}

	// OTLP HTTP exporter
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpoint(cfg.Endpoint),
		otlpmetrichttp.WithHeaders(map[string]string{
			"Authorization": cfg.Token,
		}),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithTimeout(30*time.Second),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
			Enabled:         true,
			InitialInterval: 5 * time.Second,
			MaxInterval:     60 * time.Second,
			MaxElapsedTime:  5 * time.Minute,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("otlpmetrichttp.New: %w", err)
	}

	// 过滤掉进程自身噪音指标（与 deploy/otel/collector.yaml 的 filter 对齐）
	view := sdkmetric.NewView(
		sdkmetric.Instrument{Scope: instrumentation.Scope{Name: "otelcol"}},
		sdkmetric.Stream{Aggregation: sdkmetric.AggregationDrop{}},
	)

	// MeterProvider with periodic reader
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter,
			sdkmetric.WithInterval(cfg.Interval),
		)),
		sdkmetric.WithView(view),
	)

	// 设为全局 MeterProvider，供后续桥接使用
	otel.SetMeterProvider(provider)

	log.Info("logfire exporter initialized",
		"endpoint", cfg.Endpoint,
		"interval", cfg.Interval,
		"env", cfg.Env,
	)

	return provider.Shutdown, nil
}
