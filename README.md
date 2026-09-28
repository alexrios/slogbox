# slogbox

[![CI](https://github.com/alexrios/slogbox/actions/workflows/ci.yml/badge.svg)](https://github.com/alexrios/slogbox/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/alexrios/slogbox.svg)](https://pkg.go.dev/github.com/alexrios/slogbox)
[![Go Report Card](https://goreportcard.com/badge/github.com/alexrios/slogbox)](https://goreportcard.com/report/github.com/alexrios/slogbox)

![img.png](slogbox.png)

A `slog.Handler` that keeps the last N log records in a fixed-size circular buffer.
Zero external dependencies -- stdlib only.

Requires Go 1.27 or later.

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
| `WriteTo(w)` | Write JSON to an `io.Writer` (implements `io.WriterTo`); streams in default v2 builds and may leave partial output |
| `HTTPHandler(h, onErr)` | Ready-made `WriteTo`-based `http.Handler`; partial JSON is allowed by default |
| `HTTPHandlerWithOptions(h, opts)` | HTTP handler with explicit streaming or serialization-buffered delivery |
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
	var failure *slogbox.HTTPError
	if errors.As(err, &failure) && !failure.WriteAttempted {
		// Safe here because this handler receives an uncommitted writer directly.
		http.Error(w, "logs unavailable", http.StatusInternalServerError)
		return
	}
	slog.Error("debug/logs: response write failed", "err", err)
}))
```

### Partial JSON over HTTP

`HTTPHandler` uses `HTTPJSONStream`, the zero-value default. With the default
JSON v2 implementation, this keeps the full JSON response out of memory. An
encoder or network error may happen after the response has started, so the
client can receive partial JSON, usually with an already-committed `200` status.
`OnError` receives an `HTTPError` with the underlying error, whether slogbox
attempted a response write, and the exact number of bytes accepted by the
writer. Configuring `OnError` replaces the default error response completely:

```go
http.Handle("GET /debug/logs", slogbox.HTTPHandlerWithOptions(rec, &slogbox.HTTPHandlerOptions{
	JSONMode: slogbox.HTTPJSONStream, // optional: this is the default
	OnError: func(w http.ResponseWriter, r *http.Request, failure *slogbox.HTTPError) {
		if !failure.WriteAttempted {
			// Safe only when no earlier middleware committed the writer.
			http.Error(w, "logs unavailable", http.StatusInternalServerError)
			return
		}
		slog.Error("debug/logs: response write failed",
			"err", failure.Err,
			"write_attempted", failure.WriteAttempted,
			"bytes_written", failure.BytesWritten,
		)
	},
}))
```

Choose `HTTPJSONBuffer` when a serialization error must be detected before any
JSON bytes are committed:

```go
http.Handle("GET /debug/logs", slogbox.HTTPHandlerWithOptions(rec, &slogbox.HTTPHandlerOptions{
	JSONMode: slogbox.HTTPJSONBuffer,
	OnError: func(w http.ResponseWriter, r *http.Request, failure *slogbox.HTTPError) {
		if !failure.WriteAttempted {
			http.Error(w, "logs unavailable", http.StatusInternalServerError)
			return
		}
		slog.Error("debug/logs: response write failed",
			"err", failure.Err,
			"bytes_written", failure.BytesWritten,
		)
	},
}))
```

| Mode | Serialization behavior | Tradeoff |
|---|---|---|
| `HTTPJSONStream` | Calls `WriteTo`; encoder and writer errors can leave partial JSON | Default; avoids a full response buffer under JSON v2 |
| `HTTPJSONBuffer` | Calls `JSON` completely before the first response write | Serialization-atomic; allocates the full response |

Buffering cannot undo a partial network write: if the `http.ResponseWriter`
accepts some bytes and then fails, either mode may leave a partial response.
`WriteAttempted` is independent from `BytesWritten`: the first call made by
slogbox to `http.ResponseWriter.Write` normally commits an implicit `200` even
if it then returns zero bytes and an error. When it is true, the callback should
only observe the failure. When it is false, a custom error response is safe only
if the callback knows that middleware did not previously call `WriteHeader`.
The field describes slogbox activity; it does not detect an already-committed
writer. With no `OnError`, slogbox attempts a default `500` only before its own
first write attempt.

`HTTPError` implements `error` and unwraps `Err`, so `errors.Is` and `errors.As`
continue to work with the underlying failure. The legacy `HTTPHandler` callback
also receives a `*HTTPError` through its `error` parameter. Direct comparison
with the original error is not guaranteed; use `errors.Is` or `errors.As`.
At the lower level the same choice is available directly: call `WriteTo` for
streaming under JSON v2, or call `JSON` and write the returned bytes only after
it succeeds. The JSON v1 fallback materializes the full representation in both
methods.

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

## JSON output

Go 1.27 made `encoding/json/v2` the default implementation. Accordingly,
`JSON`, `WriteTo`, `HTTPHandler`, and `HTTPHandlerWithOptions` use JSON v2
semantics by default. `WriteTo` streams records through `jsontext.Encoder`
instead of allocating one output buffer for the full array.

Before encoding, slogbox normalizes the native `slog.Value` kinds. Durations are
JSON numbers containing nanoseconds, ordinary errors are strings, times retain
their standard JSON representation, and groups remain nested objects. Error
types with their own JSON or text marshaler keep that behavior. Other values
stored with `slog.Any` are delegated to the selected JSON encoder and may return
an error when their type is unsupported. An error containing a typed nil pointer
is encoded as JSON `null` without calling its methods, matching the standard
encoders.

The default output follows native JSON v2 behavior, including strict UTF-8
validation, non-deterministic map order, minimal escaping, and empty objects or
arrays for nil maps or slices. These choices mean that JSON v1 and v2 output is
not guaranteed to be byte-for-byte identical; the public guarantee is the
documented JSON structure.

While Go supports its temporary migration opt-out, build with
`GOEXPERIMENT=nojsonv2` to restore the `encoding/json` v1 implementation and
semantics. In this fallback, `WriteTo` buffers the complete JSON representation
before its single destination write:

```bash
GOEXPERIMENT=nojsonv2 go build ./...
GOEXPERIMENT=nojsonv2 go test -race ./...
```

See the [Go 1.27 release notes](https://go.dev/doc/go1.27#new-encoding-json-v2-and-encoding-json-jsontext-packages)
and the official [JSON v2 migration guide](https://pkg.go.dev/encoding/json#hdr-Migrating_to_v2)
for the toolchain behavior and complete semantic differences.

## Benchmarks

The JSON benchmark harness uses fixed timestamps and attributes, exercises
1, 100, and 10,000 records, and names the destination used by each `WriteTo`
case. Before timing, it validates every serialized field and the complete array.
During every measured iteration, it checks both the error and exact byte count;
the final timed buffer is validated again after the timer stops.

For the comparison below, each mode was sampled ten times with Go 1.27.0 on an
Intel Core i9-14900K. Runs alternated between v1 and v2, were pinned to the same
P-core, used `GOMAXPROCS=1`, and ran for one second per sub-benchmark. `benchstat`
computed the medians, 95% confidence intervals, and Mann-Whitney U significance
test at alpha 0.05.

Run the JSON benchmarks for each mode directly:

```bash
GOEXPERIMENT=jsonv2 go test -run='^$' -bench='^Benchmark(JSON|WriteTo)$' -benchmem -count=10 ./...
GOEXPERIMENT=nojsonv2 go test -run='^$' -bench='^Benchmark(JSON|WriteTo)$' -benchmem -count=10 ./...
```

### JSON v2 compared with the JSON v1 fallback

| Benchmark | JSON v1 | JSON v2 | v2 latency |
|---|---:|---:|---:|
| JSON, 1 record | 1.442 µs ±2% | 1.516 µs ±1% | 5.1% slower (`p<0.001`) |
| JSON, 100 records | 122.1 µs ±0% | 127.1 µs ±0% | 4.1% slower (`p=0.002`) |
| JSON, 10K records | 12.90 ms ±11% | 15.83 ms ±1% | 22.7% slower (`p=0.002`) |
| WriteTo discard, 1 record | 1.440 µs ±6% | 1.927 µs ±3% | 33.8% slower (`p<0.001`) |
| WriteTo reused buffer, 1 record | 1.437 µs ±2% | 1.924 µs ±1% | 33.9% slower (`p<0.001`) |
| WriteTo discard, 100 records | 123.9 µs ±1% | 138.7 µs ±3% | 12.0% slower (`p<0.001`) |
| WriteTo reused buffer, 100 records | 125.3 µs ±0% | 138.9 µs ±11% | 10.9% slower (`p<0.001`) |
| WriteTo discard, 10K records | 13.03 ms ±4% | 14.93 ms ±5% | 14.6% slower (`p<0.001`) |
| WriteTo reused buffer, 10K records | 14.36 ms ±4% | 14.75 ms ±1% | no significant difference (`p=0.089`) |

The reliable result is a memory/latency tradeoff, not a general speedup. For
100 and 10,000 records, v2 used 28.5% to 35.0% less memory in `WriteTo` and
9.6% to 10.5% fewer allocations, but it did not serialize faster. For a single
record, streaming overhead dominated: v2 used 28.2% more memory and 43.5% more
allocations. The non-streaming `JSON` method consistently used about 20% to 22%
less memory and 9% to 16% fewer allocations, also without a latency gain.

## Design notes

[Building slogbox](https://alexrios.me/blog/slogbox-devlog/) — design decisions, tradeoffs, and lessons learned.

## License

[GPL-3.0](LICENSE)
