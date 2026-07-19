package omnilogslog_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	omnilogslog "github.com/pjgrenyer/omnilog-slog"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
)

func TestNewHandler_SendsRecordWithAuthHeaderAndTrace(t *testing.T) {
	reqCh := make(chan *http.Request, 1)
	bodyCh := make(chan []byte, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip.NewReader: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = io.NopCloser(gz)
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Drain headers/body onto channels before responding, so the test
		// goroutine observes exactly one export call.
		reqCh <- r
		bodyCh <- raw
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	handler, shutdown, err := omnilogslog.NewHandler(ctx, srv.URL, "test-api-key", omnilogslog.WithServiceName("my-service"))
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

	var req *http.Request
	var raw []byte
	select {
	case req = <-reqCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for export request")
	}
	raw = <-bodyCh

	if got, want := req.URL.Path, "/v1/logs"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("x-api-key"), "test-api-key"; got != want {
		t.Errorf("x-api-key header = %q, want %q", got, want)
	}

	var exportReq collogpb.ExportLogsServiceRequest
	if err := proto.Unmarshal(raw, &exportReq); err != nil {
		t.Fatalf("unmarshal ExportLogsServiceRequest: %v", err)
	}

	if len(exportReq.ResourceLogs) != 1 || len(exportReq.ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("unexpected shape: %+v", exportReq.ResourceLogs)
	}
	records := exportReq.ResourceLogs[0].ScopeLogs[0].LogRecords
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
