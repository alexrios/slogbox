package slogbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestNew_PanicsOnNegativeMaxAge(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("New with negative MaxAge did not panic")
		}
	}()
	New(10, &Options{MaxAge: -time.Second})
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

func TestWithAttrs_LogValuerResolved(t *testing.T) {
	// Handler-level attrs passed via WithAttrs must have LogValuers resolved
	// eagerly, matching the resolution applied to record-level attrs in Handle.
	h := New(10, nil)
	child := h.WithAttrs([]slog.Attr{slog.Any("token", tokenValue{raw: "secret"})})
	logger := slog.New(child)
	logger.Info("request")

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
		t.Fatalf("token not a string: %T (%v)", attrs["token"], attrs["token"])
	}
	if got != "REDACTED:secret" {
		t.Errorf("token = %q, want %q", got, "REDACTED:secret")
	}
}

func TestWithAttrs_LogValuerResolvedInGroup(t *testing.T) {
	// LogValuer resolution must also work for attrs nested under a group.
	h := New(10, nil)
	child := h.WithGroup("auth").WithAttrs([]slog.Attr{slog.Any("token", tokenValue{raw: "key123"})})
	logger := slog.New(child)
	logger.Info("request")

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
	authGroup, ok := attrs["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth not a map: %T (%v)", attrs["auth"], attrs["auth"])
	}
	got, ok := authGroup["token"].(string)
	if !ok {
		t.Fatalf("auth.token not a string: %T (%v)", authGroup["token"], authGroup["token"])
	}
	if got != "REDACTED:key123" {
		t.Errorf("auth.token = %q, want %q", got, "REDACTED:key123")
	}
}

func TestConcurrency(t *testing.T) {
	h := New(100, nil)
	logger := slog.New(h)

	var wg sync.WaitGroup
	// Writers.
	for range 10 {
		wg.Go(func() {
			for range 100 {
				logger.Info("concurrent", "t", time.Now().UnixNano())
			}
		})
	}
	// Readers.
	for range 5 {
		wg.Go(func() {
			for range 50 {
				_ = h.Records()
				_ = h.Len()
				for range h.All() {
				}
			}
		})
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

func TestWithAttrs_AfterConsumedGroup(t *testing.T) {
	// After WithGroup("a").WithAttrs({x:1}).WithAttrs({y:2}), all handler-level
	// attrs (x and y) must be scoped inside Group("a"), matching stdlib behaviour.
	// Record-level attrs also belong inside Group("a").
	h := New(10, nil)
	ctx := t.Context()

	child := h.WithGroup("a").
		WithAttrs([]slog.Attr{slog.String("x", "1")}).
		WithAttrs([]slog.Attr{slog.String("y", "2")})

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(slog.String("z", "record"))
	if err := child.(slog.Handler).Handle(ctx, r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}

	top := map[string]any{}
	recs[0].Attrs(func(a slog.Attr) bool {
		top[a.Key] = a
		return true
	})

	// y must be inside group "a", not at the top level.
	if _, ok := top["y"]; ok {
		t.Error("y must be inside group 'a', not at top level")
	}
	aAttr, ok := top["a"]
	if !ok {
		t.Fatal("group 'a' missing")
	}
	aInner := map[string]string{}
	for _, ga := range aAttr.(slog.Attr).Value.Group() {
		aInner[ga.Key] = ga.Value.String()
	}
	if aInner["x"] != "1" {
		t.Errorf("a.x = %q, want \"1\"", aInner["x"])
	}
	if aInner["y"] != "2" {
		t.Errorf("a.y = %q, want \"2\"", aInner["y"])
	}
	if aInner["z"] != "record" {
		t.Errorf("a.z = %q, want \"record\"", aInner["z"])
	}
}

func TestMergeGroupAttrs_SiblingGroupPreserved(t *testing.T) {
	// Construct a handler whose attr slice contains two sibling groups at the
	// top level: Group("b", y=2) at index 0, Group("a", x=1) at index 1.
	// When Handle navigates into Group("a") to place record attrs, the early
	// return in mergeGroupAttrs must return the full existing slice — so
	// Group("b") must still be present in the logged record.
	h := New(10, nil)
	ctx := t.Context()

	// Add Group("b") directly as a handler-level attr (no pending group).
	h1 := h.WithAttrs([]slog.Attr{slog.Group("b", slog.String("y", "2"))})
	// h1.attrs = [Group("b",{y:2})], groups=[], groupsUsed=0

	// WithGroup("a") + WithAttrs({x:1}) nests x inside a and appends Group("a")
	// to h1.attrs, resulting in [Group("b",{y:2}), Group("a",{x:1})].
	h2 := h1.WithGroup("a").WithAttrs([]slog.Attr{slog.String("x", "1")})
	// h2.attrs = [Group("b",{y:2}), Group("a",{x:1})], groups=["a"], groupsUsed=1

	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(slog.String("w", "record"))
	if err := h2.(slog.Handler).Handle(ctx, r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}

	// Collect top-level attrs into a map.
	top := map[string]any{}
	recs[0].Attrs(func(a slog.Attr) bool {
		top[a.Key] = a
		return true
	})

	// Group "b" must survive: mergeGroupAttrs returns the full existing slice.
	bAttr, ok := top["b"]
	if !ok {
		t.Fatal("group 'b' missing from record attrs — sibling dropped by mergeGroupAttrs")
	}
	if bAttr.(slog.Attr).Value.Kind() != slog.KindGroup {
		t.Fatalf("group 'b' is not a group: %v", bAttr)
	}

	// Group "a" must contain both the handler-level attr x and the record attr w.
	aAttr, ok := top["a"]
	if !ok {
		t.Fatal("group 'a' missing from record attrs")
	}
	aInner := map[string]string{}
	for _, ga := range aAttr.(slog.Attr).Value.Group() {
		aInner[ga.Key] = ga.Value.String()
	}
	if aInner["x"] != "1" {
		t.Errorf("a.x = %q, want \"1\"", aInner["x"])
	}
	if aInner["w"] != "record" {
		t.Errorf("a.w = %q, want \"record\"", aInner["w"])
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

func TestWithGroup_RecordAttrsRouted(t *testing.T) {
	// Record-level attrs must be nested under an open (unconsumed) group,
	// exercising the mergeGroupAttrs path in Handle (lines 128-135) for
	// attrs that arrive via the record itself rather than WithAttrs.
	h := New(10, nil)
	child := h.WithGroup("req") // open group: no WithAttrs called after it
	logger := slog.New(child)
	logger.Info("hit", "method", "GET")

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}

	top := map[string]slog.Attr{}
	recs[0].Attrs(func(a slog.Attr) bool {
		top[a.Key] = a
		return true
	})

	if _, ok := top["method"]; ok {
		t.Error("method must be inside group 'req', not at top level")
	}
	reqAttr, ok := top["req"]
	if !ok {
		t.Fatal("group 'req' missing from record attrs")
	}
	inner := map[string]string{}
	for _, ga := range reqAttr.Value.Group() {
		inner[ga.Key] = ga.Value.String()
	}
	if inner["method"] != "GET" {
		t.Errorf("req.method = %q, want \"GET\"", inner["method"])
	}
}

func TestHandle_EmptyKeyAttrWithNonComparableValue(t *testing.T) {
	// slog.Value.Equal panics on non-comparable types (slices, maps, functions)
	// because it eventually calls v.Any() == w.Any() for KindAny values.
	// The handler must not call a.Equal(slog.Attr{}); instead it checks
	// a.Key == "" and a.Value.Resolve().Kind() to decide whether to skip.
	//
	// Rule: skip empty-keyed attrs whose resolved kind is NOT KindGroup.
	// Inline groups (empty key + KindGroup) must be preserved — they are
	// a slog.Handler contract requirement tested by slogtest.
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(slog.Any("", []int{1, 2, 3}))          // empty key, non-comparable value → dropped
	r.AddAttrs(slog.Group("", slog.String("c", "d"))) // inline group → kept
	r.AddAttrs(slog.String("keep", "yes"))             // normal attr → kept
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("Handle panicked or errored: %v", err)
	}
	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	top := map[string]slog.Attr{}
	recs[0].Attrs(func(a slog.Attr) bool {
		top[a.Key] = a
		return true
	})

	// The empty-key non-group attr must be dropped (would panic via Equal).
	for k, a := range top {
		if k == "" && a.Value.Kind() != slog.KindGroup {
			t.Errorf("empty-key non-group attr not filtered: %v", a)
		}
	}
	// Inline group must be preserved.
	if _, ok := top[""]; !ok {
		t.Error("inline group (empty key + KindGroup) was incorrectly dropped")
	}
	// Normal attr must survive.
	if top["keep"].Value.String() != "yes" {
		t.Errorf("keep = %q, want \"yes\"", top["keep"].Value.String())
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

func TestJSON_InlineGroup(t *testing.T) {
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(
		slog.Group("", slog.String("c", "d")), // inline group: children go to top level
		slog.String("keep", "yes"),
	)
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

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
	if attrs["c"] != "d" {
		t.Errorf("inline group child c = %v, want \"d\"", attrs["c"])
	}
	if attrs["keep"] != "yes" {
		t.Errorf("attrs[keep] = %v, want \"yes\"", attrs["keep"])
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
	mu         sync.Mutex
	recs       []slog.Record
	err        error      // if non-nil, Handle returns this error
	handleFunc func()     // optional hook called inside Handle (outside mu)
}

func (ch *collectingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (ch *collectingHandler) Handle(_ context.Context, r slog.Record) error {
	if ch.handleFunc != nil {
		ch.handleFunc()
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.err != nil {
		return ch.err
	}
	ch.recs = append(ch.recs, r)
	return nil
}

func (ch *collectingHandler) WithAttrs([]slog.Attr) slog.Handler { return ch }
func (ch *collectingHandler) WithGroup(string) slog.Handler      { return ch }

func (ch *collectingHandler) len() int {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return len(ch.recs)
}

func (ch *collectingHandler) records() []slog.Record {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return slices.Clone(ch.recs)
}

func (ch *collectingHandler) reset() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	ch.recs = nil
}

func (ch *collectingHandler) messages() []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	msgs := make([]string, len(ch.recs))
	for i, r := range ch.recs {
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
		wg.Go(func() {
			for range 100 {
				logger.Error("concurrent")
			}
		})
	}
	wg.Wait()

	if n := collector.len(); n == 0 {
		t.Error("expected flush records from concurrent errors")
	}
}

func TestFlush_ClearDuringFlush(t *testing.T) {
	// Verify that Clear() racing with an in-flight flush does not cause stale
	// pre-Clear records to appear in the next flush window.
	//
	// Mechanism: lastFlush is claimed inside the write lock before flushing
	// begins. Clear() resets lastFlush to 0 under the same lock. The next
	// ERROR therefore computes its window from lastFlush=0 and can only see
	// records written after Clear().
	ready := make(chan struct{})
	proceed := make(chan struct{})

	collector := &collectingHandler{
		handleFunc: func() {
			// Signal we're inside Handle, then wait for Clear to complete.
			select {
			case ready <- struct{}{}:
				<-proceed
			default:
			}
		},
	}

	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	// Seed some records then trigger flush in a goroutine.
	for range 5 {
		logger.Info("before")
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		logger.Error("trigger")
	})

	// Wait until the flush goroutine is inside flushTo.Handle, then Clear.
	<-ready
	h.Clear()
	close(proceed)
	wg.Wait()

	// After Clear and a new error, the flush window is computed from
	// lastFlush=0 and should only include records written after Clear.
	logger.Info("after-clear")
	collector.reset()
	logger.Error("new-trigger")

	got := collector.records()
	for _, r := range got {
		if r.Message == "before" {
			t.Errorf("stale pre-Clear record appeared in post-Clear flush: %q", r.Message)
		}
	}
}

func TestFlush_NoDuplicates(t *testing.T) {
	// Two goroutines trigger flush concurrently. Each claims its window inside
	// the write lock, so records must appear at most once across all flushes.
	collector := &collectingHandler{}
	h := New(200, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: collector,
	})
	logger := slog.New(h)

	// Write uniquely-named INFO records so duplicates are detectable.
	const seed = 50
	for i := range seed {
		logger.Info(fmt.Sprintf("seed-%d", i))
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			logger.Error("trigger")
		})
	}
	wg.Wait()

	// Check that no seed record appears more than once across all flush windows.
	// Trigger records are excluded: two goroutines each deliver one per non-overlapping window.
	seenSeed := map[string]int{}
	for _, r := range collector.records() {
		if r.Message != "trigger" {
			seenSeed[r.Message]++
		}
	}
	for msg, n := range seenSeed {
		if n > 1 {
			t.Errorf("seed record %q flushed %d times (want ≤1)", msg, n)
		}
	}
}

func TestFlush_ErrorMidBatch(t *testing.T) {
	// Verify at-most-once semantics when FlushTo errors mid-batch:
	// records before the error are delivered, records after are permanently lost
	// (they were already claimed under the lock).
	errBoom := errors.New("boom")
	inner := &collectingHandler{}
	flushTo := &nthErrorHandler{n: 3, err: errBoom, wrapped: inner}

	h := New(100, &Options{
		Level:   slog.LevelInfo,
		FlushOn: slog.LevelError,
		FlushTo: flushTo,
	})
	logger := slog.New(h)

	logger.Info("one")
	logger.Info("two")
	logger.Error("three") // triggers flush of [one, two, three]; errors on call 3 (three)

	// Handle must return errBoom.
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelError, "four", 0)
	err := h.Handle(ctx, r) // triggers a new single-record flush; call 4 is not the error call
	if err != nil {
		t.Errorf("second Handle() unexpected error: %v", err)
	}

	// The first flush errored on record "three": one and two were delivered.
	msgs := inner.messages()
	if len(msgs) < 2 {
		t.Fatalf("expected at least 2 delivered records before error, got %d: %v", len(msgs), msgs)
	}
	if msgs[0] != "one" || msgs[1] != "two" {
		t.Errorf("delivered messages = %v, want [one two ...]", msgs)
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

// nthErrorHandler is a slog.Handler that returns an error on the Nth Handle call.
// Calls before N are forwarded to wrapped; calls after N are silently dropped.
type nthErrorHandler struct {
	mu      sync.Mutex
	n       int // error on this call (1-indexed)
	calls   int
	err     error
	wrapped *collectingHandler
}

func (h *nthErrorHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *nthErrorHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.calls == h.n {
		return h.err
	}
	return h.wrapped.Handle(context.Background(), r)
}
func (h *nthErrorHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *nthErrorHandler) WithGroup(string) slog.Handler      { return h }

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

// --- Internal unit tests (white-box) ---

func TestSnapshotLast_Zero(t *testing.T) {
	// snapshotLast(0) must return nil without panicking.
	c := &recorder{
		buf:   make([]slog.Record, 10),
		count: 5,
		head:  5,
	}
	got := c.snapshotLast(0)
	if got != nil {
		t.Errorf("snapshotLast(0) = %v, want nil", got)
	}
}

func TestFilterByAge_BoundaryIncluded(t *testing.T) {
	// A record timestamped exactly at now-maxAge sits at the cutoff.
	// BinarySearchFunc finds the first record with time >= cutoff, so the
	// boundary record must be included in the result.
	now := time.Now()
	maxAge := time.Minute
	records := []slog.Record{
		slog.NewRecord(now.Add(-time.Minute-time.Second), slog.LevelInfo, "too-old", 0),
		slog.NewRecord(now.Add(-time.Minute), slog.LevelInfo, "boundary", 0),
		slog.NewRecord(now.Add(-time.Second), slog.LevelInfo, "recent", 0),
	}
	got := filterByAge(records, maxAge, now)
	if len(got) != 2 {
		t.Fatalf("filterByAge returned %d records, want 2", len(got))
	}
	if got[0].Message != "boundary" {
		t.Errorf("got[0].Message = %q, want \"boundary\"", got[0].Message)
	}
	if got[1].Message != "recent" {
		t.Errorf("got[1].Message = %q, want \"recent\"", got[1].Message)
	}
}

func TestCollectAttrs_EmptyInlineGroup(t *testing.T) {
	// An empty inline group (key="", KindGroup, no children) passes Handle's
	// filter but produces no map entries. collectAttrs must return nil so that
	// the JSON output omits the "attrs" field entirely.
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "test", 0)
	r.AddAttrs(slog.Group("")) // empty inline group
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
		t.Error("attrs should be omitted when the only attr is an empty inline group")
	}
}

func TestAddAttrToMap_DeepInlineGroup(t *testing.T) {
	// An inline group (key="", KindGroup) nested inside another inline group
	// exercises the recursive path in addAttrToMap twice. Children at both
	// levels must be merged into the same parent map.
	h := New(10, nil)
	ctx := t.Context()
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	r.AddAttrs(
		slog.Group("",
			slog.Group("", slog.String("deep", "yes")), // nested inline group
			slog.String("shallow", "also"),
		),
		slog.String("top", "level"),
	)
	if err := h.Handle(ctx, r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

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
	if attrs["deep"] != "yes" {
		t.Errorf("deep inline child: got %v, want \"yes\"", attrs["deep"])
	}
	if attrs["shallow"] != "also" {
		t.Errorf("shallow inline child: got %v, want \"also\"", attrs["shallow"])
	}
	if attrs["top"] != "level" {
		t.Errorf("top-level attr: got %v, want \"level\"", attrs["top"])
	}
}

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
