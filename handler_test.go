package omnilogslog_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	omnilogslog "github.com/pjgrenyer/omnilog-slog"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

// capturedRequest holds only the request details a test cares about,
// extracted inside the handler while the request is still live.
type capturedRequest struct {
	path      string
	apiKeyHdr string
	body      []byte
}

// testServer starts an httptest.Server that responds with status to every
// request, and reports one capturedRequest per request received on reqCh.
// reqCount tracks the total number of requests received.
func testServer(t *testing.T, status int) (srv *httptest.Server, reqCh chan capturedRequest, reqCount *atomic.Int32) {
	t.Helper()
	reqCh = make(chan capturedRequest, 8)
	reqCount = &atomic.Int32{}

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)

		body := r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip.NewReader: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body = io.NopCloser(gz)
			defer func() {
				if err := gz.Close(); err != nil {
					t.Errorf("gz.Close: %v", err)
				}
			}()
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Extract everything the test needs now, while the request is
		// still live, rather than handing out r itself.
		reqCh <- capturedRequest{
			path:      r.URL.Path,
			apiKeyHdr: r.Header.Get("x-api-key"),
			body:      raw,
		}
		w.WriteHeader(status)
	}))
	return srv, reqCh, reqCount
}

func TestNewHandler_SendsRecordWithAuthHeaderAndTrace(t *testing.T) {
	srv, reqCh, reqCount := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key",
		omnilogslog.WithServiceName("my-service"),
		omnilogslog.WithAllowInsecureHTTP(),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	loggedCtx := trace.ContextWithSpanContext(ctx, spanCtx)

	logger := slog.New(handler)
	logger.InfoContext(loggedCtx, "hello from go", "env", "prod")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var req capturedRequest
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for export request")
	}

	if got, want := req.path, "/v1/logs"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := req.apiKeyHdr, "test-api-key"; got != want {
		t.Errorf("x-api-key header = %q, want %q", got, want)
	}
	if got := reqCount.Load(); got != 1 {
		t.Errorf("received %d export requests, want exactly 1", got)
	}

	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(req.body, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}

	if len(exportReq.ResourceLogs) != 1 || len(exportReq.ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("unexpected shape: %+v", exportReq.ResourceLogs)
	}
	scope := exportReq.ResourceLogs[0].ScopeLogs[0]
	if got, want := scope.Scope.Name, "github.com/pjgrenyer/omnilog-slog"; got != want {
		t.Errorf("scope name = %q, want %q", got, want)
	}

	records := scope.LogRecords
	if len(records) != 1 {
		t.Fatalf("got %d log records, want 1", len(records))
	}
	rec := records[0]

	if got, want := rec.Body.GetStringValue(), "hello from go"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := rec.SeverityText, "INFO"; got != want {
		t.Errorf("severity text = %q, want %q", got, want)
	}
	if !bytes.Equal(rec.TraceId, traceID[:]) {
		t.Errorf("trace id = %x, want %x", rec.TraceId, traceID[:])
	}
	if !bytes.Equal(rec.SpanId, spanID[:]) {
		t.Errorf("span id = %x, want %x", rec.SpanId, spanID[:])
	}

	var sawEnv bool
	for _, attr := range rec.Attributes {
		if attr.Key == "env" && attr.Value.GetStringValue() == "prod" {
			sawEnv = true
		}
	}
	if !sawEnv {
		t.Errorf("record missing env=prod attribute, got %+v", rec.Attributes)
	}

	var sawServiceName bool
	for _, attr := range exportReq.ResourceLogs[0].Resource.Attributes {
		if attr.Key == "service.name" && attr.Value.GetStringValue() == "my-service" {
			sawServiceName = true
		}
	}
	if !sawServiceName {
		t.Errorf("resource missing service.name=my-service, got %+v", exportReq.ResourceLogs[0].Resource.Attributes)
	}
}

func TestNewHandler_DefaultServiceName(t *testing.T) {
	srv, reqCh, _ := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key", omnilogslog.WithAllowInsecureHTTP())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	slog.New(handler).Info("hello")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	req := <-reqCh
	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(req.body, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}

	var got string
	for _, attr := range exportReq.ResourceLogs[0].Resource.Attributes {
		if attr.Key == "service.name" {
			got = attr.Value.GetStringValue()
		}
	}
	if want := "unknown-service"; got != want {
		t.Errorf("default service.name = %q, want %q", got, want)
	}
}

func TestNewHandler_ResourceAttributes(t *testing.T) {
	srv, reqCh, _ := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key",
		omnilogslog.WithServiceName("my-service"),
		omnilogslog.WithResourceAttributes(map[string]string{
			"env":    "prod",
			"region": "eu-west-2",
		}),
		omnilogslog.WithAllowInsecureHTTP(),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	slog.New(handler).Info("hello")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	req := <-reqCh
	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(req.body, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}

	got := map[string]string{}
	for _, attr := range exportReq.ResourceLogs[0].Resource.Attributes {
		got[attr.Key] = attr.Value.GetStringValue()
	}
	want := map[string]string{"service.name": "my-service", "env": "prod", "region": "eu-west-2"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("resource attribute %q = %q, want %q (all: %+v)", k, got[k], v, got)
		}
	}
}

func TestNewHandler_ResourceAttributesServiceNameKeyOverridesWithServiceName(t *testing.T) {
	srv, reqCh, _ := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key",
		omnilogslog.WithServiceName("from-with-service-name"),
		omnilogslog.WithResourceAttributes(map[string]string{"service.name": "from-resource-attrs"}),
		omnilogslog.WithAllowInsecureHTTP(),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	slog.New(handler).Info("hello")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	req := <-reqCh
	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(req.body, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}

	var got string
	for _, attr := range exportReq.ResourceLogs[0].Resource.Attributes {
		if attr.Key == "service.name" {
			got = attr.Value.GetStringValue()
		}
	}
	if want := "from-resource-attrs"; got != want {
		t.Errorf("service.name = %q, want %q (WithResourceAttributes should win)", got, want)
	}
}

func TestNewHandler_TrailingSlashBaseURL(t *testing.T) {
	srv, reqCh, _ := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL+"/", "test-api-key", omnilogslog.WithAllowInsecureHTTP())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	slog.New(handler).Info("hello")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	req := <-reqCh
	if got, want := req.path, "/v1/logs"; got != want {
		t.Errorf("path = %q, want %q (trailing slash on baseURL should not produce a double slash)", got, want)
	}
}

func TestNewHandler_RejectsEmptyBaseURL(t *testing.T) {
	_, _, err := omnilogslog.NewHandler(context.Background(), "", "test-api-key")
	if err == nil {
		t.Fatal("NewHandler: expected error for empty baseURL, got nil")
	}
}

func TestNewHandler_RejectsUnparseableBaseURL(t *testing.T) {
	_, _, err := omnilogslog.NewHandler(context.Background(), "://missing-scheme", "test-api-key")
	if err == nil {
		t.Fatal("NewHandler: expected error for unparseable baseURL, got nil")
	}
}

func TestNewHandler_RejectsBaseURLWithoutSchemeOrHost(t *testing.T) {
	_, _, err := omnilogslog.NewHandler(context.Background(), "not-a-url-at-all", "test-api-key")
	if err == nil {
		t.Fatal("NewHandler: expected error for baseURL without scheme/host, got nil")
	}
}

func TestNewHandler_RejectsNonHTTPSBaseURL(t *testing.T) {
	_, _, err := omnilogslog.NewHandler(context.Background(), "http://example.com", "test-api-key")
	if err == nil {
		t.Fatal("NewHandler: expected error for http:// baseURL without WithAllowInsecureHTTP, got nil")
	}
}

func TestNewHandler_ShutdownSurfacesExportFailure(t *testing.T) {
	// 400 is not one of otlploghttp's retryable statuses, so the batch
	// processor's flush-on-shutdown fails immediately instead of retrying
	// for the default ~1 minute backoff budget.
	srv, _, reqCount := testServer(t, http.StatusBadRequest)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key", omnilogslog.WithAllowInsecureHTTP())
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	slog.New(handler).Info("hello")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err == nil {
		t.Fatal("shutdown: expected error when the server rejects the batch, got nil")
	}
	if reqCount.Load() == 0 {
		t.Error("server never received the export request")
	}
}

func TestNewHandler_MinLevelFiltersBelowThreshold(t *testing.T) {
	srv, reqCh, reqCount := testServer(t, http.StatusOK)
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key",
		omnilogslog.WithAllowInsecureHTTP(),
		omnilogslog.WithMinLevel(slog.LevelWarn),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	logger := slog.New(handler)
	logger.Info("dropped: below min level")
	logger.Warn("kept: at min level")

	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := reqCount.Load(); got != 1 {
		t.Fatalf("received %d export requests, want exactly 1 (the Info record should have been filtered)", got)
	}

	req := <-reqCh
	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(req.body, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}
	records := exportReq.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(records) != 1 {
		t.Fatalf("got %d log records, want 1", len(records))
	}
	if got, want := records[0].Body.GetStringValue(), "kept: at min level"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func ExampleNewHandler() {
	ctx := context.Background()

	handler, shutdown, err := omnilogslog.NewHandler(
		ctx,
		"https://api-id.execute-api.eu-west-2.amazonaws.com/prod",
		"omnilog-api-key",
		omnilogslog.WithServiceName("my-service"),
	)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			panic(err)
		}
	}()

	logger := slog.New(handler)
	logger.Info("hello from go", "env", "prod")
}
