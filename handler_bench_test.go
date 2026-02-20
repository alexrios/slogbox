package slogbox

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func BenchmarkHandle(b *testing.B) {
	h := New(1024, nil)
	ctx := context.Background()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = h.Handle(ctx, r)
	}
}

func BenchmarkHandle_Parallel(b *testing.B) {
	h := New(1024, nil)
	ctx := context.Background()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = h.Handle(ctx, r)
		}
	})
}

func BenchmarkRecords(b *testing.B) {
	h := New(1000, nil)
	ctx := context.Background()
	for range 1000 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = h.Records()
	}
}

func BenchmarkAll(b *testing.B) {
	h := New(1000, nil)
	ctx := context.Background()
	for range 1000 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		for range h.All() {
		}
	}
}

func BenchmarkJSON(b *testing.B) {
	h := New(100, nil)
	ctx := context.Background()
	for range 100 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		r.AddAttrs(
			slog.String("method", "GET"),
			slog.Int("status", 200),
			slog.Duration("latency", 42*time.Millisecond),
			slog.String("path", "/api/v1/users"),
			slog.String("ip", "10.0.0.1"),
		)
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = h.JSON()
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
	b.ResetTimer()
	for range b.N {
		_ = h.WithAttrs(attrs)
	}
}

func BenchmarkWithGroup(b *testing.B) {
	h := New(1024, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
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
	ctx := context.Background()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "benchmark", 0)
	r.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = h.Handle(ctx, r)
	}
}

func BenchmarkHandle_FlushTrigger(b *testing.B) {
	// Cost when flush fires (ERROR, ~100 records flushed).
	h := New(1024, &Options{
		FlushOn: slog.LevelError,
		FlushTo: discardHandler{},
	})
	ctx := context.Background()
	info := slog.NewRecord(time.Now(), slog.LevelInfo, "fill", 0)
	info.AddAttrs(slog.String("key", "value"))
	errRec := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	errRec.AddAttrs(slog.String("key", "value"))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		// Write 99 INFO records then 1 ERROR to trigger flush of 100.
		for range 99 {
			_ = h.Handle(ctx, info)
		}
		_ = h.Handle(ctx, errRec)
	}
}

func BenchmarkWriteTo(b *testing.B) {
	h := New(100, nil)
	ctx := context.Background()
	for range 100 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		r.AddAttrs(
			slog.String("method", "GET"),
			slog.Int("status", 200),
			slog.Duration("latency", 42*time.Millisecond),
			slog.String("path", "/api/v1/users"),
			slog.String("ip", "10.0.0.1"),
		)
		_ = h.Handle(ctx, r)
	}

	var buf bytes.Buffer
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buf.Reset()
		_, _ = h.WriteTo(&buf)
	}
}

func BenchmarkRecords_WithMaxAge(b *testing.B) {
	h := New(1000, &Options{MaxAge: 5 * time.Minute})
	ctx := context.Background()
	now := time.Now()
	for i := range 1000 {
		r := slog.NewRecord(now.Add(-time.Duration(1000-i)*time.Second), slog.LevelInfo, "fill", 0)
		r.AddAttrs(slog.String("k", "v"))
		_ = h.Handle(ctx, r)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = h.Records()
	}
}
