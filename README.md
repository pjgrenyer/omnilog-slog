# omnilog-slog

A [`log/slog`](https://pkg.go.dev/log/slog) handler that forwards records to
[omnilog](https://github.com/pjgrenyer/omnilog) as OTLP/HTTP protobuf,
authenticated with your tenant's API key. Built on the official
[`otelslog`](https://pkg.go.dev/go.opentelemetry.io/contrib/bridges/otelslog)
bridge and [`otlploghttp`](https://pkg.go.dev/go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp)
exporter — this package just wires the two together with omnilog's endpoint
and auth convention, the same way omnilog's README shows for a plain
OpenTelemetry JS setup.

Levels, attributes, and (if the logging call carries a context with an active
span) trace/span correlation are handled by the underlying bridge — nothing
bespoke to omnilog's wire format lives here.

## Install

```bash
go get github.com/pjgrenyer/omnilog-slog
```

## Usage

```go
package main

import (
	"context"
	"log/slog"
	"os"

	omnilogslog "github.com/pjgrenyer/omnilog-slog"
)

func main() {
	ctx := context.Background()

	handler, shutdown, err := omnilogslog.NewHandler(
		ctx,
		os.Getenv("OMNILOG_URL"),     // https://<api-id>.execute-api.eu-west-2.amazonaws.com/prod
		os.Getenv("OMNILOG_API_KEY"),
		omnilogslog.WithServiceName("my-service"),
	)
	if err != nil {
		panic(err)
	}
	defer shutdown(ctx)

	logger := slog.New(handler)
	logger.Info("hello from go", "env", "prod")

	// Logging with a context that carries an active span attaches that
	// span's trace/span id to the record, so it correlates with the trace
	// in omnilog:
	// logger.InfoContext(ctx, "handled request", "route", "/health")
}
```

`OMNILOG_URL` is the base API Gateway invoke URL (no `/v1/logs` suffix —
`NewHandler` appends it), matching the convention used elsewhere in
omnilog's docs and the demo feed script.

Records are batched; call the `shutdown` func (typically via `defer`) before
the process exits so the last batch flushes.

## Why a separate repo

The main [omnilog](https://github.com/pjgrenyer/omnilog) repo is private.
Go's toolchain resolves `go get` import paths against real VCS repos, so a
package meant to be a one-line `go get` for any external service needs a
public home — keeping it inside the private repo would mean every consumer
configuring `GOPRIVATE` and git credentials just to add a log handler, which
defeats the point. This repo holds only the handler; everything else about
omnilog (ingest, storage, query, UI) stays in the private repo.

## Development

```bash
go build ./...
go vet ./...
go test ./...
```
