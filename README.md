# slogbox

[![CI](https://github.com/alexrios/slogbox/actions/workflows/ci.yml/badge.svg)](https://github.com/alexrios/slogbox/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/alexrios/slogbox.svg)](https://pkg.go.dev/github.com/alexrios/slogbox)
[![Go Report Card](https://goreportcard.com/badge/github.com/alexrios/slogbox)](https://goreportcard.com/report/github.com/alexrios/slogbox)

![img.png](slogbox.png)

A `slog.Handler` that keeps the last N log records in a fixed-size circular buffer.
Zero external dependencies -- stdlib only.

Primary use case: exposing recent logs via health-check or admin HTTP endpoints.
Inspired by `runtime/trace.FlightRecorder`, it can also act as a **black box recorder**
that flushes context-rich logs on error.

## Install

```bash
go get github.com/alexrios/slogbox
```

## Quick start

```go
package main

import (
	"log/slog"
	"net/http"

	"github.com/alexrios/slogbox"
)

func main() {
	rec := slogbox.New(500, nil)
	logger := slog.New(rec)
	slog.SetDefault(logger)

	http.Handle("GET /debug/logs", slogbox.HTTPHandler(rec, nil))

	slog.Info("server starting", "port", 8080)
	http.ListenAndServe(":8080", nil)
}
```

## API overview

| Function / Method | Description |
|---|---|
| `New(size, opts)` | Create a handler with buffer capacity `size` |
| `Handle(ctx, record)` | Store a record (implements `slog.Handler`); triggers flush if `FlushOn` threshold is met |
| `WithAttrs(attrs)` | Return a handler with additional attributes (shared buffer) |
| `WithGroup(name)` | Return a handler with a group prefix (shared buffer) |
| `Records()` | Snapshot of stored records, oldest to newest (respects `MaxAge`) |
| `RecordsAbove(minLevel)` | Snapshot filtered to records >= `minLevel` (respects `MaxAge`) |
| `All()` | `iter.Seq[slog.Record]` iterator over stored records (respects `MaxAge`) |
| `JSON()` | Marshal records as a JSON array (respects `MaxAge`) |
| `WriteTo(w)` | Stream records as JSON to an `io.Writer` (implements `io.WriterTo`) |
| `HTTPHandler(h, onErr)` | Ready-made `http.Handler` serving JSON logs; pass `nil` for default 500 on error |
| `Flush(ctx)` | Explicitly drain pending records to `FlushTo` (for graceful shutdown) |
| `Len()` | Number of records physically stored (ignores `MaxAge`) |
| `Capacity()` | Total buffer capacity |
| `TotalRecords()` | Monotonic count of records ever written (survives wrap-around; reset by `Clear`) |
| `PendingFlushCount()` | Number of records pending for next flush (0 if flush not configured) |
| `Clear()` | Remove all records and reset flush state |

### Options

| Field | Type | Description |
|---|---|---|
| `Level` | `slog.Leveler` | Minimum level stored (default: `INFO`) |
| `FlushOn` | `slog.Leveler` | Level that triggers flush to `FlushTo` |
| `FlushTo` | `slog.Handler` | Destination for flushed records |
| `MaxAge` | `time.Duration` | Exclude records older than this from reads; `0` = no filter |

## Black box pattern

Keep a ring buffer of recent logs and flush them to stderr when an error occurs:

```go
rec := slogbox.New(500, &slogbox.Options{
	FlushOn: slog.LevelError,
	FlushTo: slog.NewJSONHandler(os.Stderr, nil),
	MaxAge:  5 * time.Minute,
})
logger := slog.New(rec)

logger.Info("request started", "path", "/api/users")
logger.Info("db query", "rows", 42)
// ... when an error happens, all recent logs are flushed to stderr
logger.Error("query failed", "err", err)
```

Serve the ring buffer over HTTP:

```go
http.Handle("GET /debug/logs", slogbox.HTTPHandler(rec, nil))
```

Or with a custom error handler:

```go
http.Handle("GET /debug/logs", slogbox.HTTPHandler(rec, func(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("debug/logs: write error", "err", err)
}))
```

### Graceful shutdown

On process exit, records accumulated since the last level-triggered flush are
silently lost. Use `Flush` to drain them:

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
if err := rec.Flush(ctx); err != nil {
	log.Printf("flush error: %v", err)
}
```

### Observability

Monitor buffer throughput and pending flush count:

```go
fmt.Printf("total records written: %d\n", rec.TotalRecords())
fmt.Printf("pending flush: %d\n", rec.PendingFlushCount())
```

## Streaming JSON with `GOEXPERIMENT=jsonv2`

When built with `GOEXPERIMENT=jsonv2`, `WriteTo` uses `encoding/json/v2`'s
streaming `jsontext.Encoder` to write records one at a time, avoiding a single
large intermediate `[]byte` allocation. The API is identical -- the optimization
is transparent.

```bash
GOEXPERIMENT=jsonv2 go build ./...
GOEXPERIMENT=jsonv2 go test -bench=BenchmarkWriteTo -benchmem ./...
```

Without the experiment flag, `WriteTo` falls back to `encoding/json.Marshal`
(the default behavior).

## Benchmarks

Representative values on an Intel Core i9-14900K (32 threads):

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Handle | 150 | 48 | 1 |
| Handle_Parallel | 440 | 48 | 1 |
| Handle_WithFlush | 144 | 48 | 1 |
| Handle_FlushTrigger (100 records) | 32,900 | 37,568 | 101 |
| Flush (100 records) | 33,800 | 37,568 | 101 |
| Records (1000) | 138,000 | 294,912 | 1 |
| All (1000) | 161,900 | 294,984 | 4 |
| JSON (100 records, 5 attrs) | 306,500 | 143,400 | 1,904 |
| WriteTo (100 records) | 224,600 | 143,300 | 1,904 |
| WriteTo_LargeBuffer (10K records) | 25,700,000 | 16,300,000 | 190,026 |
| WithAttrs (5 attrs) | 459 | 496 | 3 |
| WithGroup | 38 | 16 | 1 |
| Records_WithMaxAge | 198,600 | 294,912 | 1 |

### WriteTo with `GOEXPERIMENT=jsonv2`

When built with the jsonv2 experiment, `WriteTo` streams records through
`jsontext.Encoder` instead of marshalling the entire array at once:

| Benchmark | default | jsonv2 | improvement |
|---|---:|---:|---|
| WriteTo (100 records, B/op) | 143,300 | 34,296 | **4x less memory** |
| WriteTo (100 records, allocs/op) | 1,904 | 35 | **54x fewer allocs** |
| WriteTo (10K records, ns/op) | 25,700,000 | 960,000 | **27x faster** |
| WriteTo (10K records, B/op) | 16,300,000 | 2,885,115 | **6x less memory** |
| WriteTo (10K records, allocs/op) | 190,026 | 35 | **5400x fewer allocs** |

## Design notes

[Building slogbox](https://alexrios.me/blog/slogbox-devlog/) — design decisions, tradeoffs, and lessons learned.

## License

[GPL-3.0](LICENSE)
