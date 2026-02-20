package slogbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/slogtest"
	"time"
)

func TestNew_PanicsOnZeroSize(t *testing.T) {
	for _, size := range []int{0, -1, -100} {
		t.Run("", func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("New(%d, nil) did not panic", size)
				}
			}()
			New(size, nil)
		})
	}
}

func TestHandle_BasicWrite(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("hello")

	if h.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", h.Len())
	}
	recs := h.Records()
	if recs[0].Message != "hello" {
		t.Errorf("Message = %q, want %q", recs[0].Message, "hello")
	}
}

func TestHandle_WrapAround(t *testing.T) {
	h := New(3, nil)
	logger := slog.New(h)

	for i := range 5 {
		logger.Info("msg", "i", i)
	}

	if h.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", h.Len())
	}
	recs := h.Records()
	// Oldest surviving record should have i=2.
	for j, want := range []int64{2, 3, 4} {
		got := recs[j].NumAttrs()
		if got == 0 {
			t.Fatalf("record %d has no attrs", j)
		}
		var val int64
		recs[j].Attrs(func(a slog.Attr) bool {
			if a.Key == "i" {
				val = a.Value.Int64()
			}
			return true
		})
		if val != want {
			t.Errorf("record %d: i = %d, want %d", j, val, want)
		}
	}
}

func TestEnabled_DefaultLevel(t *testing.T) {
	h := New(10, nil)
	ctx := t.Context()

	if h.Enabled(ctx, slog.LevelDebug) {
		t.Error("should not be enabled for Debug")
	}
	if !h.Enabled(ctx, slog.LevelInfo) {
		t.Error("should be enabled for Info")
	}
	if !h.Enabled(ctx, slog.LevelWarn) {
		t.Error("should be enabled for Warn")
	}
}

func TestEnabled_CustomLevel(t *testing.T) {
	h := New(10, &Options{Level: slog.LevelWarn})
	ctx := t.Context()

	if h.Enabled(ctx, slog.LevelInfo) {
		t.Error("should not be enabled for Info")
	}
	if !h.Enabled(ctx, slog.LevelWarn) {
		t.Error("should be enabled for Warn")
	}
}

func TestWithAttrs(t *testing.T) {
	h := New(10, nil)
	child := h.WithAttrs([]slog.Attr{slog.String("service", "api")})
	logger := slog.New(child)
	logger.Info("request")

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("Len() = %d, want 1", len(recs))
	}
	var found bool
	recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "service" && a.Value.String() == "api" {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Error("expected service=api attr not found")
	}
}

func TestWithGroup(t *testing.T) {
	h := New(10, nil)
	child := h.WithGroup("req").WithAttrs([]slog.Attr{slog.String("method", "GET")})
	logger := slog.New(child)
	logger.Info("hit")

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("Len() = %d, want 1", len(recs))
	}

	// The attr should be nested: req.method = "GET"
	var foundGroup bool
	recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "req" && a.Value.Kind() == slog.KindGroup {
			for _, ga := range a.Value.Group() {
				if ga.Key == "method" && ga.Value.String() == "GET" {
					foundGroup = true
				}
			}
		}
		return true
	})
	if !foundGroup {
		t.Error("expected req.method=GET group attr not found")
	}
}

func TestWithAttrs_SharedBuffer(t *testing.T) {
	h := New(10, nil)
	child1 := h.WithAttrs([]slog.Attr{slog.String("from", "child1")})
	child2 := h.WithAttrs([]slog.Attr{slog.String("from", "child2")})

	slog.New(child1).Info("one")
	slog.New(child2).Info("two")
	slog.New(h).Info("three")

	if h.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", h.Len())
	}
	recs := h.Records()
	msgs := make([]string, len(recs))
	for i, r := range recs {
		msgs[i] = r.Message
	}
	want := []string{"one", "two", "three"}
	if !slices.Equal(msgs, want) {
		t.Errorf("messages = %v, want %v", msgs, want)
	}
}

func TestAll_Iterator(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	for i := range 5 {
		logger.Info("msg", "i", i)
	}

	// Collect via iterator.
	var count int
	for range h.All() {
		count++
	}
	if count != 5 {
		t.Errorf("iterator yielded %d records, want 5", count)
	}

	// Early break.
	count = 0
	for range h.All() {
		count++
		if count == 2 {
			break
		}
	}
	if count != 2 {
		t.Errorf("early break: count = %d, want 2", count)
	}
}

func TestJSON(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("test-msg", "key", "val")

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	e := entries[0]
	if e["msg"] != "test-msg" {
		t.Errorf("msg = %v, want test-msg", e["msg"])
	}
	if e["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", e["level"])
	}
	attrs, ok := e["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("attrs not a map: %T", e["attrs"])
	}
	if attrs["key"] != "val" {
		t.Errorf("attrs[key] = %v, want val", attrs["key"])
	}
}

func TestClear(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("a")
	logger.Info("b")

	h.Clear()

	if h.Len() != 0 {
		t.Errorf("Len() after Clear = %d, want 0", h.Len())
	}
	if recs := h.Records(); len(recs) != 0 {
		t.Errorf("Records() after Clear = %d, want 0", len(recs))
	}
}

func TestCapacity(t *testing.T) {
	h := New(42, nil)
	if got := h.Capacity(); got != 42 {
		t.Errorf("Capacity() = %d, want 42", got)
	}

	// Capacity stays the same after writes.
	logger := slog.New(h)
	for range 50 {
		logger.Info("fill")
	}
	if got := h.Capacity(); got != 42 {
		t.Errorf("Capacity() after overflow = %d, want 42", got)
	}
}

// tokenValue is a LogValuer that resolves to a redacted string.
type tokenValue struct{ raw string }

func (tv tokenValue) LogValue() slog.Value {
	return slog.StringValue("REDACTED:" + tv.raw)
}

func TestJSON_LogValuer(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("auth", "token", tokenValue{raw: "secret"})

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	attrs, ok := entries[0]["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("attrs not a map: %T", entries[0]["attrs"])
	}
	got, ok := attrs["token"].(string)
	if !ok {
		t.Fatalf("token not a string: %T", attrs["token"])
	}
	want := "REDACTED:secret"
	if got != want {
		t.Errorf("token = %q, want %q", got, want)
	}
}

func TestConcurrency(t *testing.T) {
	h := New(100, nil)
	logger := slog.New(h)

	var wg sync.WaitGroup
	// Writers.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				logger.Info("concurrent", "t", time.Now().UnixNano())
			}
		}()
	}
	// Readers.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = h.Records()
				_ = h.Len()
				for range h.All() {
				}
			}
		}()
	}
	wg.Wait()

	if h.Len() != 100 {
		t.Errorf("Len() = %d, want 100", h.Len())
	}
}

func TestWithAttrs_Empty(t *testing.T) {
	h := New(10, nil)
	got := h.WithAttrs(nil)
	if got != h {
		t.Error("WithAttrs(nil) should return the same *Handler")
	}
	got = h.WithAttrs([]slog.Attr{})
	if got != h {
		t.Error("WithAttrs(empty) should return the same *Handler")
	}
}

func TestWithGroup_Empty(t *testing.T) {
	h := New(10, nil)
	got := h.WithGroup("")
	if got != h {
		t.Error("WithGroup(\"\") should return the same *Handler")
	}
}

func TestHandle_BufferSizeOne(t *testing.T) {
	h := New(1, nil)
	logger := slog.New(h)

	for i := range 5 {
		logger.Info("msg", "i", i)
	}

	if h.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", h.Len())
	}
	recs := h.Records()
	var val int64
	recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "i" {
			val = a.Value.Int64()
		}
		return true
	})
	if val != 4 {
		t.Errorf("last record i = %d, want 4", val)
	}
}

func TestWithGroup_Nested(t *testing.T) {
	h := New(10, nil)
	child := h.WithGroup("a").WithGroup("b").WithAttrs([]slog.Attr{slog.String("k", "v")})
	logger := slog.New(child)
	logger.Info("nested")

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	attrs, ok := entries[0]["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("attrs not a map: %T", entries[0]["attrs"])
	}
	aGroup, ok := attrs["a"].(map[string]any)
	if !ok {
		t.Fatalf("attrs[a] not a map: %T", attrs["a"])
	}
	bGroup, ok := aGroup["b"].(map[string]any)
	if !ok {
		t.Fatalf("attrs[a][b] not a map: %T", aGroup["b"])
	}
	if bGroup["k"] != "v" {
		t.Errorf("attrs[a][b][k] = %v, want \"v\"", bGroup["k"])
	}
}

func TestAddAttrToMap_EmptyKey(t *testing.T) {
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0)
	r.AddAttrs(slog.String("", "ignored"), slog.String("keep", "yes"))
	_ = h.Handle(ctx, r)

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	attrs, ok := entries[0]["attrs"].(map[string]any)
	if !ok {
		t.Fatalf("attrs not a map: %T", entries[0]["attrs"])
	}
	if _, exists := attrs[""]; exists {
		t.Error("empty-key attr should be omitted")
	}
	if attrs["keep"] != "yes" {
		t.Errorf("attrs[keep] = %v, want \"yes\"", attrs["keep"])
	}
}

func TestJSON_NoAttrs(t *testing.T) {
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "bare", 0)
	_ = h.Handle(ctx, r)

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if _, exists := entries[0]["attrs"]; exists {
		t.Error("attrs field should be omitted when record has no attrs")
	}
}

func TestJSON_NestedGroups(t *testing.T) {
	h := New(10, nil)
	child := h.WithGroup("a").WithGroup("b").WithGroup("c").WithAttrs([]slog.Attr{slog.Int("depth", 3)})
	logger := slog.New(child)
	logger.Info("deep")

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	attrs := entries[0]["attrs"].(map[string]any)
	a := attrs["a"].(map[string]any)
	b := a["b"].(map[string]any)
	c := b["c"].(map[string]any)
	if c["depth"] != float64(3) {
		t.Errorf("depth = %v, want 3", c["depth"])
	}
}

// --- Test helper: collectingHandler ---

// collectingHandler is a slog.Handler that appends records to a slice.
type collectingHandler struct {
	mu      sync.Mutex
	records []slog.Record
	err     error // if non-nil, Handle returns this error
}

func (ch *collectingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (ch *collectingHandler) Handle(_ context.Context, r slog.Record) error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.err != nil {
		return ch.err
	}
	ch.records = append(ch.records, r)
	return nil
}

func (ch *collectingHandler) WithAttrs([]slog.Attr) slog.Handler { return ch }
func (ch *collectingHandler) WithGroup(string) slog.Handler      { return ch }

func (ch *collectingHandler) len() int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return len(ch.records)
}

func (ch *collectingHandler) messages() []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	msgs := make([]string, len(ch.records))
	for i, r := range ch.records {
		msgs[i] = r.Message
	}
	return msgs
}

// --- Flush tests ---

func TestFlush_TriggersOnThreshold(t *testing.T) {
	collector := &collectingHandler{}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	logger.Info("one")
	logger.Warn("two")
	logger.Error("three")

	msgs := collector.messages()
	if len(msgs) != 3 {
		t.Fatalf("flush delivered %d records, want 3; got %v", len(msgs), msgs)
	}
	want := []string{"one", "two", "three"}
	if !slices.Equal(msgs, want) {
		t.Errorf("flushed messages = %v, want %v", msgs, want)
	}
}

func TestFlush_NoTriggerBelowThreshold(t *testing.T) {
	collector := &collectingHandler{}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	logger.Info("one")
	logger.Warn("two")

	if n := collector.len(); n != 0 {
		t.Errorf("flush delivered %d records, want 0", n)
	}
}

func TestFlush_BufferPreservedAfterFlush(t *testing.T) {
	collector := &collectingHandler{}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	logger.Info("one")
	logger.Error("two")

	// Records should still be in the ring after flush.
	recs := h.Records()
	if len(recs) != 2 {
		t.Fatalf("Records() = %d, want 2", len(recs))
	}
	if recs[0].Message != "one" || recs[1].Message != "two" {
		t.Errorf("Records messages = [%q, %q], want [one, two]", recs[0].Message, recs[1].Message)
	}
}

func TestFlush_Incremental(t *testing.T) {
	collector := &collectingHandler{}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	logger.Info("a")
	logger.Error("b") // flush 1: delivers a, b

	if n := collector.len(); n != 2 {
		t.Fatalf("after first flush: %d records, want 2", n)
	}

	logger.Info("c")
	logger.Error("d") // flush 2: delivers only c, d

	msgs := collector.messages()
	if len(msgs) != 4 {
		t.Fatalf("total flushed = %d, want 4", len(msgs))
	}
	// Second flush should have sent c, d (not a, b again).
	want := []string{"a", "b", "c", "d"}
	if !slices.Equal(msgs, want) {
		t.Errorf("flushed messages = %v, want %v", msgs, want)
	}
}

func TestFlush_PartialConfig(t *testing.T) {
	// Only FlushOn set (no FlushTo) → no panic, normal operation.
	h1 := New(10, &Options{FlushOn: slog.LevelError})
	logger1 := slog.New(h1)
	logger1.Error("ok")
	if h1.Len() != 1 {
		t.Errorf("FlushOn-only: Len() = %d, want 1", h1.Len())
	}

	// Only FlushTo set (no FlushOn) → no panic, normal operation.
	h2 := New(10, &Options{FlushTo: &collectingHandler{}})
	logger2 := slog.New(h2)
	logger2.Error("ok")
	if h2.Len() != 1 {
		t.Errorf("FlushTo-only: Len() = %d, want 1", h2.Len())
	}
}

func TestFlush_ErrorPropagation(t *testing.T) {
	errBoom := errors.New("boom")
	collector := &collectingHandler{err: errBoom}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})

	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelError, "fail", 0)
	err := h.Handle(ctx, r)
	if !errors.Is(err, errBoom) {
		t.Errorf("Handle() error = %v, want %v", err, errBoom)
	}

	// Record should still be stored despite flush error.
	if h.Len() != 1 {
		t.Errorf("Len() = %d, want 1", h.Len())
	}
}

func TestFlush_WithAttrsChild(t *testing.T) {
	collector := &collectingHandler{}
	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	child := h.WithAttrs([]slog.Attr{slog.String("svc", "api")})
	logger := slog.New(child)

	logger.Info("hello")
	logger.Error("boom")

	if n := collector.len(); n != 2 {
		t.Fatalf("flush delivered %d records, want 2", n)
	}
}

func TestFlush_WrappedBuffer(t *testing.T) {
	collector := &collectingHandler{}
	h := New(3, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	ctx := t.Context()

	// Write 5 INFO records into a size-3 buffer (overwrites first 2).
	for i := range 5 {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
		r.AddAttrs(slog.Int("i", i))
		_ = h.Handle(ctx, r)
	}

	// Trigger flush — 6 records since lastFlush but only 3 survive in buffer.
	r := slog.NewRecord(time.Now(), slog.LevelError, "boom", 0)
	_ = h.Handle(ctx, r)

	// Buffer holds i=3, i=4, boom (i=2 was overwritten by the error). Flush sends all 3.
	msgs := collector.messages()
	if len(msgs) != 3 {
		t.Fatalf("flushed %d records, want 3 (buffer capacity); got %v", len(msgs), msgs)
	}
	if msgs[2] != "boom" {
		t.Errorf("last flushed message = %q, want %q", msgs[2], "boom")
	}
}

func TestFlush_Concurrent(t *testing.T) {
	collector := &collectingHandler{}
	h := New(1000, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				logger.Error("concurrent")
			}
		}()
	}
	wg.Wait()

	if n := collector.len(); n == 0 {
		t.Error("expected flush records from concurrent errors")
	}
}

// --- MaxAge tests ---

func TestMaxAge_FiltersOldRecords(t *testing.T) {
	h := New(100, &Options{MaxAge: time.Minute})
	ctx := t.Context()

	// Insert a record with a time in the past.
	old := slog.NewRecord(time.Now().Add(-2*time.Minute), slog.LevelInfo, "old", 0)
	_ = h.Handle(ctx, old)

	// Insert a recent record.
	recent := slog.NewRecord(time.Now(), slog.LevelInfo, "recent", 0)
	_ = h.Handle(ctx, recent)

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("Records() = %d, want 1", len(recs))
	}
	if recs[0].Message != "recent" {
		t.Errorf("message = %q, want %q", recs[0].Message, "recent")
	}
}

func TestMaxAge_ZeroMeansNoFilter(t *testing.T) {
	h := New(100, nil) // MaxAge defaults to 0.
	ctx := t.Context()

	old := slog.NewRecord(time.Now().Add(-24*time.Hour), slog.LevelInfo, "old", 0)
	_ = h.Handle(ctx, old)

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("Records() = %d, want 1", len(recs))
	}
}

func TestMaxAge_AllExpired(t *testing.T) {
	h := New(100, &Options{MaxAge: time.Nanosecond})
	ctx := t.Context()

	old := slog.NewRecord(time.Now().Add(-time.Second), slog.LevelInfo, "gone", 0)
	_ = h.Handle(ctx, old)

	recs := h.Records()
	if len(recs) != 0 {
		t.Errorf("Records() = %d, want 0", len(recs))
	}
}

func TestMaxAge_AffectsAllReadPaths(t *testing.T) {
	h := New(100, &Options{MaxAge: time.Minute})
	ctx := t.Context()

	old := slog.NewRecord(time.Now().Add(-2*time.Minute), slog.LevelInfo, "old", 0)
	_ = h.Handle(ctx, old)
	recent := slog.NewRecord(time.Now(), slog.LevelInfo, "recent", 0)
	_ = h.Handle(ctx, recent)

	// All()
	var allCount int
	for range h.All() {
		allCount++
	}
	if allCount != 1 {
		t.Errorf("All() yielded %d, want 1", allCount)
	}

	// JSON()
	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("JSON() entries = %d, want 1", len(entries))
	}

	// WriteTo()
	var buf bytes.Buffer
	_, err = h.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}
	var wtEntries []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &wtEntries); err != nil {
		t.Fatalf("WriteTo Unmarshal error: %v", err)
	}
	if len(wtEntries) != 1 {
		t.Errorf("WriteTo() entries = %d, want 1", len(wtEntries))
	}
}

func TestMaxAge_LenIgnoresMaxAge(t *testing.T) {
	h := New(100, &Options{MaxAge: time.Minute})
	ctx := t.Context()

	old := slog.NewRecord(time.Now().Add(-2*time.Minute), slog.LevelInfo, "old", 0)
	_ = h.Handle(ctx, old)
	recent := slog.NewRecord(time.Now(), slog.LevelInfo, "recent", 0)
	_ = h.Handle(ctx, recent)

	if h.Len() != 2 {
		t.Errorf("Len() = %d, want 2 (physical count)", h.Len())
	}
}

// --- WriteTo tests ---

func TestWriteTo_MatchesJSON(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("msg1", "k", "v")
	logger.Warn("msg2")

	jsonData, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var buf bytes.Buffer
	_, err = h.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}

	// Both should unmarshal to the same structure.
	var jsonEntries, wtEntries []map[string]any
	if err := json.Unmarshal(jsonData, &jsonEntries); err != nil {
		t.Fatalf("JSON Unmarshal: %v", err)
	}
	if err := json.Unmarshal(buf.Bytes(), &wtEntries); err != nil {
		t.Fatalf("WriteTo Unmarshal: %v", err)
	}
	if len(jsonEntries) != len(wtEntries) {
		t.Fatalf("entry count mismatch: JSON=%d, WriteTo=%d", len(jsonEntries), len(wtEntries))
	}
	for i := range jsonEntries {
		if jsonEntries[i]["msg"] != wtEntries[i]["msg"] {
			t.Errorf("entry %d msg mismatch: JSON=%v, WriteTo=%v", i, jsonEntries[i]["msg"], wtEntries[i]["msg"])
		}
	}
}

func TestWriteTo_EmptyBuffer(t *testing.T) {
	h := New(10, nil)
	var buf bytes.Buffer
	n, err := h.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}
	if n == 0 {
		t.Error("WriteTo() returned 0 bytes for empty buffer")
	}
	// Should be valid JSON (empty array).
	var entries []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %d, want 0", len(entries))
	}
}

func TestWriteTo_ByteCount(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("count-test", "k", "v")

	var buf bytes.Buffer
	n, err := h.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo() error: %v", err)
	}
	if int64(buf.Len()) != n {
		t.Errorf("byte count mismatch: returned %d, written %d", n, buf.Len())
	}
}

type errWriter struct{ err error }

func (ew errWriter) Write([]byte) (int, error) { return 0, ew.err }

func TestWriteTo_WriterError(t *testing.T) {
	h := New(10, nil)
	logger := slog.New(h)
	logger.Info("fail")

	errBoom := errors.New("write failed")
	_, err := h.WriteTo(errWriter{err: errBoom})
	if err == nil {
		t.Fatal("WriteTo() should return error from writer")
	}
}

// Verify Handler implements io.WriterTo.
var _ io.WriterTo = (*Handler)(nil)

func TestHandler_Slogtest(t *testing.T) {
	var h *Handler
	slogtest.Run(t, func(t *testing.T) slog.Handler {
		h = New(100, nil)
		return h
	}, func(t *testing.T) map[string]any {
		recs := h.Records()
		if len(recs) == 0 {
			return map[string]any{}
		}
		r := recs[len(recs)-1]
		m := map[string]any{
			slog.LevelKey:   r.Level,
			slog.MessageKey: r.Message,
		}
		if !r.Time.IsZero() {
			m[slog.TimeKey] = r.Time
		}
		r.Attrs(func(a slog.Attr) bool {
			recordAttrToMap(m, a)
			return true
		})
		return m
	})
}

func recordAttrToMap(m map[string]any, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		if a.Key == "" {
			// Inline group: merge attrs into parent map.
			for _, ga := range v.Group() {
				recordAttrToMap(m, ga)
			}
			return
		}
		gm := map[string]any{}
		for _, ga := range v.Group() {
			recordAttrToMap(gm, ga)
		}
		m[a.Key] = gm
	} else {
		m[a.Key] = v.Any()
	}
}
