package slogbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"
)

func BenchmarkHandle(b *testing.B) {
	h := New(1024, nil)
	ctx := b.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, r)
	}
}

func BenchmarkHandle_Parallel(b *testing.B) {
	h := New(1024, nil)
	ctx := b.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() { // b.Loop() is not applicable inside RunParallel; pb.Next() is correct here
			_ = h.Handle(ctx, r)
		}
	})
}

func BenchmarkRecords(b *testing.B) {
	h := New(1000, nil)
	ctx := b.Context()
	for range 1000 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = h.Records()
	}
}

func BenchmarkAll(b *testing.B) {
	h := New(1000, nil)
	ctx := b.Context()
	for range 1000 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	for b.Loop() {
		for range h.All() {
		}
	}
}

func BenchmarkJSON(b *testing.B) {
	for _, records := range jsonBenchmarkSizes {
		b.Run("records="+strconv.Itoa(records), func(b *testing.B) {
			h := newJSONBenchmarkHandler(b, records)
			preflight, err := h.JSON()
			if err != nil {
				b.Fatalf("JSON preflight: %v", err)
			}
			validateBenchmarkJSON(b, preflight, records)
			wantBytes := len(preflight)

			b.SetBytes(int64(wantBytes))
			b.ReportAllocs()
			var output []byte
			for b.Loop() {
				output, err = h.JSON()
				if err != nil {
					b.Fatalf("JSON: %v", err)
				}
				if len(output) != wantBytes {
					b.Fatalf("JSON wrote %d bytes, want %d", len(output), wantBytes)
				}
			}
			validateBenchmarkJSON(b, output, records)
		})
	}
}

func BenchmarkWithAttrs(b *testing.B) {
	h := New(1024, nil)
	attrs := []slog.Attr{
		slog.String("service", "api"),
		slog.String("version", "1.0"),
		slog.Int("port", 8080),
		slog.Bool("debug", false),
		slog.String("env", "prod"),
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = h.WithAttrs(attrs)
	}
}

func BenchmarkWithGroup(b *testing.B) {
	h := New(1024, nil)

	b.ReportAllocs()
	for b.Loop() {
		_ = h.WithGroup("request")
	}
}

// discardHandler is a no-op slog.Handler for benchmarking flush overhead.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (discardHandler) WithAttrs([]slog.Attr) slog.Handler        { return discardHandler{} }
func (discardHandler) WithGroup(string) slog.Handler             { return discardHandler{} }

func BenchmarkHandle_WithFlush(b *testing.B) {
	// Measure overhead of flush check on the hot path (INFO, no trigger).
	h := New(1024, &Options{
		FlushOn: slog.LevelError,
		FlushTo: discardHandler{},
	})
	ctx := b.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, r)
	}
}

func BenchmarkHandle_FlushTrigger(b *testing.B) {
	// Cost when flush fires (ERROR, ~100 records flushed).
	h := New(1024, &Options{
		FlushOn: slog.LevelError,
		FlushTo: discardHandler{},
	})
	ctx := b.Context()
	info := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
	info.AddAttrs(slog.String("key", "value"))
	errRec := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	errRec.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	for b.Loop() {
		// Write 99 INFO records then 1 ERROR to trigger flush of 100.
		for range 99 {
			_ = h.Handle(ctx, info)
		}
		_ = h.Handle(ctx, errRec)
	}
}

func BenchmarkWriteTo(b *testing.B) {
	for _, records := range jsonBenchmarkSizes {
		b.Run("records="+strconv.Itoa(records), func(b *testing.B) {
			h := newJSONBenchmarkHandler(b, records)
			var preflight bytes.Buffer
			written, err := h.WriteTo(&preflight)
			if err != nil {
				b.Fatalf("WriteTo preflight: %v", err)
			}
			if written != int64(preflight.Len()) {
				b.Fatalf("WriteTo preflight count = %d, wrote %d bytes", written, preflight.Len())
			}
			validateBenchmarkJSON(b, preflight.Bytes(), records)
			wantBytes := preflight.Len()

			b.Run("writer=discard", func(b *testing.B) {
				b.SetBytes(int64(wantBytes))
				b.ReportAllocs()
				for b.Loop() {
					written, err := h.WriteTo(io.Discard)
					if err != nil {
						b.Fatalf("WriteTo: %v", err)
					}
					if written != int64(wantBytes) {
						b.Fatalf("WriteTo count = %d, want %d", written, wantBytes)
					}
				}
			})

			b.Run("writer=buffer_reused", func(b *testing.B) {
				var output bytes.Buffer
				output.Grow(wantBytes)
				b.SetBytes(int64(wantBytes))
				b.ReportAllocs()
				for b.Loop() {
					output.Reset()
					written, err := h.WriteTo(&output)
					if err != nil {
						b.Fatalf("WriteTo: %v", err)
					}
					if written != int64(wantBytes) || output.Len() != wantBytes {
						b.Fatalf("WriteTo returned %d and wrote %d bytes, want %d", written, output.Len(), wantBytes)
					}
				}
				validateBenchmarkJSON(b, output.Bytes(), records)
			})
		})
	}
}

var jsonBenchmarkSizes = [...]int{1, 100, 10000}

var jsonBenchmarkTime = time.Date(2026, time.August, 22, 12, 34, 56, 789, time.UTC)

func newJSONBenchmarkHandler(b *testing.B, records int) *Handler {
	b.Helper()
	h := New(max(1, records), nil)
	for range records {
		r := slog.NewRecord(jsonBenchmarkTime, slog.LevelInfo, "msg", 0)
		r.AddAttrs(
			slog.String("method", "GET"),
			slog.Int("status", 200),
			slog.Duration("latency", 42*time.Millisecond),
			slog.String("path", "/api/v1/users"),
			slog.String("ip", "10.0.0.1"),
		)
		if err := h.Handle(b.Context(), r); err != nil {
			b.Fatalf("Handle fixture record: %v", err)
		}
	}
	return h
}

type jsonBenchmarkEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"msg"`
	Attrs   struct {
		Method  string `json:"method"`
		Status  int64  `json:"status"`
		Latency int64  `json:"latency"`
		Path    string `json:"path"`
		IP      string `json:"ip"`
	} `json:"attrs"`
}

func validateBenchmarkJSON(b *testing.B, data []byte, wantEntries int) {
	b.Helper()
	if !json.Valid(data) {
		b.Fatalf("output is not valid JSON: %q", data)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []jsonBenchmarkEntry
	if err := dec.Decode(&entries); err != nil {
		b.Fatalf("decode benchmark output: %v", err)
	}
	if err := consumeJSONEOF(dec); err != nil {
		b.Fatalf("decode trailing benchmark output: %v", err)
	}
	if len(entries) != wantEntries {
		b.Fatalf("benchmark output has %d entries, want %d", len(entries), wantEntries)
	}
	for i, entry := range entries {
		if !entry.Time.Equal(jsonBenchmarkTime) ||
			entry.Level != slog.LevelInfo.String() ||
			entry.Message != "msg" ||
			entry.Attrs.Method != "GET" ||
			entry.Attrs.Status != 200 ||
			entry.Attrs.Latency != (42*time.Millisecond).Nanoseconds() ||
			entry.Attrs.Path != "/api/v1/users" ||
			entry.Attrs.IP != "10.0.0.1" {
			b.Fatalf("benchmark output entry %d does not match fixture: %+v", i, entry)
		}
	}
}

func consumeJSONEOF(dec *json.Decoder) error {
	var trailing any
	err := dec.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return errors.New("unexpected second JSON value")
	}
	return err
}

func BenchmarkFlush(b *testing.B) {
	h := New(1024, &Options{
		FlushOn: slog.LevelError,
		FlushTo: discardHandler{},
	})
	ctx := b.Context()
	info := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
	info.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	for b.Loop() {
		// Write 100 INFO records, then flush explicitly.
		for range 100 {
			_ = h.Handle(ctx, info)
		}
		_ = h.Flush(ctx)
	}
}

func BenchmarkRecords_WithMaxAge(b *testing.B) {
	h := New(1000, &Options{MaxAge: 5 * time.Minute})
	ctx := b.Context()
	now := time.Now()
	for i := range 1000 {
		r := slog.NewRecord(now.Add(-time.Duration(1000-i)*time.Second), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	for b.Loop() {
		_ = h.Records()
	}
}
