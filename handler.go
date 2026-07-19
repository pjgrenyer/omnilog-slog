// Package omnilogslog provides a log/slog handler that forwards records to
// an omnilog tenant's ingest endpoint as OTLP/HTTP protobuf, authenticated
// with the tenant's API key.
package omnilogslog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// scopeName identifies this package as the OpenTelemetry instrumentation
// scope for every log record it emits (ScopeLogs.Scope.Name on the wire).
// It is conventionally the package path of the code doing the logging, not
// the end user's service name.
const scopeName = "github.com/pjgrenyer/omnilog-slog"

type config struct {
	serviceName   string
	minLevel      slog.Level
	errorHandler  func(error)
	allowInsecure bool
}

// Option configures NewHandler.
type Option func(*config)

// WithServiceName sets the OTel resource's service.name attribute, which
// omnilog attaches to every log row sent through the handler. Defaults to
// "unknown-service" if not set.
func WithServiceName(name string) Option {
	return func(c *config) { c.serviceName = name }
}

// WithMinLevel sets the minimum slog.Level exported to omnilog; records
// below this level are dropped before they reach the batching/export
// pipeline. Defaults to slog.LevelDebug, meaning every record is exported.
func WithMinLevel(level slog.Level) Option {
	return func(c *config) { c.minLevel = level }
}

// WithErrorHandler registers fn to be called whenever a batch of records
// fails to export, for example because omnilog rejects the API key or a
// network error occurs. Export happens asynchronously after the logging
// call returns, so without this option such failures are only visible as
// stderr noise (or via the last flush's error from shutdown).
//
// fn is installed as the process-wide OpenTelemetry error handler via
// otel.SetErrorHandler, so it will also receive errors from any other OTel
// components configured in the same process.
func WithErrorHandler(fn func(error)) Option {
	return func(c *config) { c.errorHandler = fn }
}

// WithAllowInsecureHTTP allows baseURL to use the http:// scheme. Without
// this option, NewHandler rejects a non-https baseURL, since the API key is
// sent as a plain header with no other protection against eavesdropping.
// This is intended for pointing at a local collector during development.
func WithAllowInsecureHTTP() Option {
	return func(c *config) { c.allowInsecure = true }
}

// NewHandler builds an slog.Handler that batches records and forwards them
// to baseURL+"/v1/logs" as OTLP/HTTP protobuf, authenticated with apiKey via
// the x-api-key header. baseURL is the omnilog API Gateway invoke URL
// (without a trailing "/v1/logs"), e.g. the OMNILOG_URL convention used
// elsewhere in omnilog docs. A trailing slash on baseURL is tolerated.
//
// baseURL must be an absolute https:// URL unless WithAllowInsecureHTTP is
// given. The exporter's HTTP client is also configured to never follow
// redirects, since Go's http.Client does not strip custom headers such as
// the API key when following a cross-origin redirect.
//
// If the handler is called with a context carrying an active OpenTelemetry
// span, the resulting log record's TraceID/SpanID are populated
// automatically so the log correlates with that trace.
//
// Export is asynchronous and best-effort: if omnilog rejects every batch
// (for example because of a bad API key), the application is not notified
// beyond stderr noise unless WithErrorHandler is supplied; shutdown's
// returned error only reflects the final flush.
//
// The returned shutdown func flushes any batched records and must be called
// before the process exits, typically via defer.
func NewHandler(ctx context.Context, baseURL, apiKey string, opts ...Option) (handler slog.Handler, shutdown func(context.Context) error, err error) {
	cfg := config{serviceName: "unknown-service", minLevel: slog.LevelDebug}
	for _, opt := range opts {
		opt(&cfg)
	}

	baseURL, err = normalizeBaseURL(baseURL, cfg.allowInsecure)
	if err != nil {
		return nil, nil, fmt.Errorf("omnilogslog: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(cfg.serviceName)))
	if err != nil {
		return nil, nil, fmt.Errorf("omnilogslog: build resource: %w", err)
	}

	httpClient := &http.Client{
		// Never follow redirects: the default client would replay the
		// x-api-key header against whatever host the redirect points to.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	exporter, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(baseURL+"/v1/logs"),
		otlploghttp.WithHeaders(map[string]string{"x-api-key": apiKey}),
		otlploghttp.WithHTTPClient(httpClient),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("omnilogslog: build exporter: %w", err)
	}

	if cfg.errorHandler != nil {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(cfg.errorHandler))
	}

	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(&levelFilterProcessor{
			Processor:   sdklog.NewBatchProcessor(exporter),
			minSeverity: toSeverity(cfg.minLevel),
		}),
		sdklog.WithResource(res),
	)

	handler = otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(provider))
	return handler, provider.Shutdown, nil
}

// normalizeBaseURL validates baseURL and strips a trailing slash so
// baseURL+"/v1/logs" never produces a double slash.
func normalizeBaseURL(raw string, allowInsecure bool) (string, error) {
	if raw == "" {
		return "", errors.New("baseURL must not be empty")
	}

	trimmed := strings.TrimSuffix(raw, "/")

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse baseURL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("baseURL %q must be an absolute URL, e.g. https://host", raw)
	}
	if u.Scheme != "https" && !allowInsecure {
		return "", fmt.Errorf("baseURL %q must use https (got %q); pass WithAllowInsecureHTTP to allow http for local development", raw, u.Scheme)
	}
	return trimmed, nil
}

// sevOffset mirrors the slog.Level-to-log.Severity conversion otelslog uses
// internally, so WithMinLevel's threshold lines up with the severities
// otelslog assigns to exported records.
const sevOffset = slog.Level(otellog.SeverityDebug) - slog.LevelDebug

func toSeverity(level slog.Level) otellog.Severity {
	return otellog.Severity(level + sevOffset)
}

// levelFilterProcessor wraps a Processor and drops records below
// minSeverity before they reach it. sdklog.BatchProcessor's own Enabled
// always returns true regardless of severity, so without this wrapper
// every level (including Debug) is exported unconditionally.
type levelFilterProcessor struct {
	sdklog.Processor
	minSeverity otellog.Severity
}

func (p *levelFilterProcessor) Enabled(ctx context.Context, param sdklog.EnabledParameters) bool {
	return param.Severity >= p.minSeverity
}

func (p *levelFilterProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	if record.Severity() < p.minSeverity {
		return nil
	}
	return p.Processor.OnEmit(ctx, record)
}
