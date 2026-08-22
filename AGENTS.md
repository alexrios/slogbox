# AGENTS.md

This file provides repository guidance for contributors and coding agents.

## Project

`slogbox` is a zero-dependency Go library providing a `slog.Handler` that keeps
the last N log records in a fixed-size circular buffer. Inspired by
`runtime/trace.FlightRecorder`, it acts as a black box recorder that can expose
recent logs over HTTP and flush context-rich logs when an error occurs.

- Module: `github.com/alexrios/slogbox`
- Minimum Go version: 1.27
- License: GPL-3.0

## Commands

```bash
mise run test                                  # default JSON v2 tests with race detector
mise run lint                                  # default JSON v2 vet
mise run test-v1                               # JSON v1 fallback tests with race detector
mise run lint-v1                               # JSON v1 fallback vet
mise run bench                                 # complete default JSON v2 benchmark suite
mise run ci                                    # GitHub Actions workflow through pinned act

go test -v -race -run TestFoo ./...            # targeted test
BENCH_CPU=8 scripts/compare-json-benchmarks.sh # alternating v1/v2 comparison with benchstat
```

Run `go fix -diff ./...` and `git diff --check` before considering a change
complete.

## Architecture

Core files:

- `handler.go`: `Handler`, options, buffer operations, flush behavior, JSON
  entry shape, slog attribute handling, and normalization of native slog kinds.
- `recorder.go`: shared `recorder`, ring snapshots, and age filtering.
- `http.go`: `HTTPHandler` and `HTTPHandlerWithOptions`, which expose records
  with either streaming or serialization-buffered JSON delivery.
- `writeto_jsonv2.go`: default `JSON` and streaming `WriteTo` using
  `encoding/json/v2` and `jsontext.Encoder`; selected by
  `goexperiment.jsonv2`.
- `writeto.go`: JSON v1 fallback using `encoding/json`; selected by
  `!goexperiment.jsonv2`, normally via `GOEXPERIMENT=nojsonv2`.

The two central types are:

- `recorder`: a shared ring buffer guarded by `sync.RWMutex`. It owns the
  preallocated records, head and count, monotonic total, flush configuration,
  flush cursor, and maximum record age.
- `Handler`: a `slog.Handler` with a pointer to the shared recorder and its own
  attribute and group state. Values returned by `WithAttrs` and `WithGroup`
  share the recorder but clone their handler-level slices.

## Design constraints

- Store `slog.Record` values, not formatted strings, so callers retain output
  flexibility.
- Resolve `LogValuer` values when handling a record so retained output reflects
  state at capture time.
- Preserve slog group nesting and inline-group behavior.
- Normalize every native `slog.Value` kind before JSON serialization. Durations
  are nanoseconds, ordinary errors are strings, and error types with supported
  JSON or text marshalers retain their custom representation. A typed nil error
  pointer is JSON null and its methods must not be called.
- Delegate arbitrary `slog.Any` values to the selected encoder. Unsupported
  values must return errors; never discard serialization errors in tests or
  benchmarks.
- JSON v2 is the Go 1.27 default. Preserve its strict UTF-8 validation, minimal
  escaping, non-deterministic map ordering, and empty collections for nil maps
  and slices. Do not promise byte compatibility with the v1 fallback.
- `WriteTo` implements `io.WriterTo`. Its returned count is the number of bytes
  actually sent to the destination. The v2 `countWriter` may consume and omit
  the encoder's final newline, but its `Write` method must still report all input
  bytes consumed according to the `io.Writer` contract.
- `HTTPJSONStream` is the zero-value default and may expose partial JSON after
  an encoder or writer error. Only the default v2 implementation avoids a full
  output buffer; the v1 fallback materializes the representation in `WriteTo`.
  `HTTPJSONBuffer` must finish serialization before the first response write.
  Never claim that buffering can recover bytes already accepted by a failing
  `http.ResponseWriter`.
- `HTTPHandlerWithOptions.OnError` receives an `HTTPError`. Treat
  `WriteAttempted`, not `BytesWritten`, as the record of whether slogbox called
  `http.ResponseWriter.Write`: the first call may commit status 200 even when it
  accepts zero bytes. A false value does not detect `WriteHeader` calls made by
  middleware, so emit a custom error response only when the callback also knows
  the writer arrived uncommitted. `OnError` replaces the default error handling;
  after an attempted write it should only observe the failure. `HTTPHandler`
  retains its legacy callback signature but passes the structured `HTTPError`.
- Snapshot under the recorder lock and perform iteration, serialization, and
  destination flushes outside the lock where the existing design permits it.
- Flush windows use at-most-once delivery: claim records under the write lock,
  deliver outside it, and never retry a claimed record after failure.
- `MaxAge` assumes non-decreasing record timestamps and applies to `Records`,
  `RecordsAbove`, `All`, `JSON`, and `WriteTo`; `Len` remains a physical count.
- The library remains stdlib-only. Combine it with other handlers externally
  instead of adding handler chaining here.

## Testing and benchmarks

- Exercise behavior in both the default mode and `GOEXPERIMENT=nojsonv2`.
- Keep mode-specific JSON semantic tests behind matching build tags.
- Exercise both HTTP JSON modes, including serialization failures before the
  first writer call, deterministic partial writer failures, short writes, error
  callbacks, status commitment with zero accepted bytes, and the structured
  `HTTPError` fields. Do not depend on an encoder's internal buffer size to
  force a flush.
- Test `JSON` and `WriteTo` structurally rather than relying on map order or
  byte-for-byte equivalence.
- Every JSON benchmark must perform a complete preflight serialization, verify
  every fixture field and the expected array length, fail on every timed
  serialization error, and verify the timed byte count.
- Use deterministic fixtures. Benchmark inputs must never depend on the wall
  clock, random data, map iteration order, or prior benchmark state.
- Name the destination writer in each `WriteTo` sub-benchmark. Do not compare
  results produced with different writer behavior.
- Compare v1 and v2 only with the same benchmark selection, Go version,
  benchtime, count, CPU affinity, `GOMAXPROCS`, and machine. Alternate mode
  order between runs and analyze at least ten samples with `benchstat`; do not
  infer a speedup when the reported difference is not statistically significant.

## Git and GitHub

Never put information about AI assistants in Git metadata. Preserve unrelated
and untracked work, and stage only files belonging to the requested change.
