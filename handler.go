// Package omnilogslog provides a log/slog handler that forwards records to
// an omnilog tenant's ingest endpoint as OTLP/HTTP protobuf, authenticated
// with the tenant's API key.
package omnilogslog

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type config struct {
	serviceName string
}

// Option configures NewHandler.
type Option func(*config)

// WithServiceName sets the OTel resource's service.name attribute, which
// omnilog attaches to every log row sent through the handler. Defaults to
// "unknown-service" if not set.
func WithServiceName(name string) Option {
	return func(c *config) { c.serviceName = name }
}

// NewHandler builds an slog.Handler that batches records and forwards them
// to baseURL+"/v1/logs" as OTLP/HTTP protobuf, authenticated with apiKey via
// the x-api-key header. baseURL is the omnilog API Gateway invoke URL
// (without a trailing "/v1/logs"), e.g. the OMNILOG_URL convention used
// elsewhere in omnilog docs.
//
// If the handler is called with a context carrying an active OpenTelemetry
// span, the resulting log record's TraceID/SpanID are populated
// automatically so the log correlates with that trace.
//
// The returned shutdown func flushes any batched records and must be called
// before the process exits, typically via defer.
func NewHandler(ctx context.Context, baseURL, apiKey string, opts ...Option) (handler slog.Handler, shutdown func(context.Context) error, err error) {
	cfg := config{serviceName: "unknown-service"}
	for _, opt := range opts {
		opt(&cfg)
	}

	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(baseURL+"/v1/logs"),
		otlploghttp.WithHeaders(map[string]string{"x-api-key": apiKey}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("omnilogslog: build exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(cfg.serviceName)))
	if err != nil {
		return nil, nil, fmt.Errorf("omnilogslog: build resource: %w", err)
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
		sdklog.WithResource(res),
	)

	handler = otelslog.NewHandler(cfg.serviceName, otelslog.WithLoggerProvider(provider))
	return handler, provider.Shutdown, nil
}
