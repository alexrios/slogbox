// Package slogbox provides a [slog.Handler] that keeps the last N log records
// in a circular buffer. It is designed for exposing recent logs via health check
// or admin HTTP endpoints.
package slogbox

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"slices"
	"time"
)

// Options configure a [Handler].
type Options struct {
	// Level reports the minimum record level that will be stored.
	// The handler discards records with lower levels.
	// If Level is nil, the handler assumes LevelInfo.
	Level slog.Leveler

	// FlushOn sets the level threshold that triggers a flush of buffered records
	// to FlushTo. Both FlushOn and FlushTo must be set for flush to be active.
	// Flush errors are returned by Handle. Records are claimed before flushing
	// begins: at-most-once delivery — a record is never re-sent even if
	// FlushTo.Handle returns an error.
	FlushOn slog.Leveler

	// FlushTo is the destination handler for flushed records.
	// Both FlushOn and FlushTo must be set for flush to be active.
	//
	// FlushTo must not directly or indirectly log back to the same slogbox Handler;
	// doing so will deadlock.
	FlushTo slog.Handler

	// MaxAge excludes records older than this duration from read operations
	// (Records, All, JSON, WriteTo). Zero means no age filtering.
	// Negative values cause New to panic.
	// Len returns the physical count regardless of MaxAge.
	//
	// MaxAge assumes records are stored in chronological order (non-decreasing
	// timestamps). If Handle is called with out-of-order timestamps, MaxAge
	// filtering may return incorrect results.
	MaxAge time.Duration
}

// Handler is a [slog.Handler] that stores log records in a fixed-size ring buffer.
// Handlers returned by [Handler.WithAttrs] and [Handler.WithGroup] share the same
// underlying buffer.
type Handler struct {
	core       *recorder
	level      slog.Leveler
	attrs      []slog.Attr
	groups     []string // full group path (never consumed)
	groupsUsed int      // number of groups consumed by WithAttrs
}

// New creates a Handler with the given buffer capacity.
// It panics if size < 1.
func New(size int, opts *Options) *Handler {
	if size < 1 {
		panic("slogbox: size must be at least 1")
	}
	var level slog.Leveler = slog.LevelInfo
	if opts != nil && opts.Level != nil {
		level = opts.Level
	}
	c := &recorder{
		buf: make([]slog.Record, size),
	}
	if opts != nil {
		if opts.FlushOn != nil && opts.FlushTo != nil {
			c.flushOn = opts.FlushOn
			c.flushTo = opts.FlushTo
		}
		if opts.MaxAge < 0 {
			panic("slogbox: MaxAge must be non-negative")
		}
		c.maxAge = opts.MaxAge
	}
	return &Handler{
		core:  c,
		level: level,
	}
}

// Enabled reports whether the handler stores records at the given level.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// Handle stores a clone of r in the ring buffer.
// If FlushOn/FlushTo are configured and the record's level reaches the
// FlushOn threshold, all records since the last flush are forwarded to FlushTo.
// If FlushTo.Handle returns an error, Handle returns that error to the caller.
// The flush window is claimed before flushing begins (at-most-once semantics),
// so claimed records are never re-sent even when an error is returned.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	// Build a new record merging handler-level and record-level attrs.
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)

	// Collect record-level attrs per the slog.Handler contract:
	//   - Resolve LogValuers eagerly so the captured value reflects state at
	//     Handle time, not at the later JSON()/WriteTo() serialization time.
	//   - Skip attrs with an empty key unless they are inline groups
	//     (empty key + KindGroup): those must be kept and their children
	//     treated as if they were at the current level.
	//   - Avoid a.Equal(slog.Attr{}) for the check: Value.Equal panics when
	//     both sides hold the same non-comparable type (e.g. two slices stored
	//     via slog.Any). The key == "" guard below is equivalent and safe.
	var recordAttrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.Resolve()
		if a.Key == "" && v.Kind() != slog.KindGroup {
			return true // skip: empty-key non-group attr
		}
		recordAttrs = append(recordAttrs, slog.Attr{Key: a.Key, Value: v})
		return true
	})

	if len(h.groups) > 0 {
		// Merge handler attrs and record attrs into the correct group nesting.
		allAttrs := mergeGroupAttrs(slices.Clone(h.attrs), h.groups, recordAttrs)
		nr.AddAttrs(allAttrs...)
	} else {
		nr.AddAttrs(h.attrs...)
		nr.AddAttrs(recordAttrs...)
	}

	c := h.core
	c.mu.Lock()
	c.buf[c.head] = nr
	c.head = (c.head + 1) % len(c.buf)
	if c.count < len(c.buf) {
		c.count++
	}
	c.total++

	var flushRecords []slog.Record
	if c.flushOn != nil && c.flushTo != nil && nr.Level >= c.flushOn.Level() {
		n := min(c.total-c.lastFlush, uint64(c.count))
		flushRecords = c.snapshotLast(int(n))
		// Claim the window immediately under the lock so concurrent flushes
		// compute non-overlapping ranges. At-most-once semantics: claimed
		// records are not re-sent even if FlushTo returns an error.
		c.lastFlush = c.total
	}
	c.mu.Unlock()

	// Flush outside the lock to avoid blocking writers if FlushTo does I/O.
	//
	// Note: flushed records are replayed with context.Background(). The original
	// request context (deadlines, trace IDs) is not available here because Handle
	// does not store it. This is intentional for the black-box recorder pattern,
	// but callers should be aware that FlushTo will not receive cancellation
	// signals or tracing metadata from the originating request.
	for _, fr := range flushRecords {
		if err := c.flushTo.Handle(context.Background(), fr); err != nil {
			return err
		}
	}
	return nil
}

// WithAttrs returns a new Handler whose records will include the given attrs.
// The new handler shares the same ring buffer.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	attrs = resolveAttrs(attrs)
	h2 := h.clone()
	pending := h2.groups[h2.groupsUsed:]
	if len(pending) > 0 {
		nested := nestAttrs(pending, attrs)
		h2.attrs = mergeGroupAttrs(h2.attrs, h2.groups[:h2.groupsUsed], nested)
	} else if len(h2.groups) > 0 {
		// All pending groups already consumed; attrs still belong inside the group path.
		h2.attrs = mergeGroupAttrs(h2.attrs, h2.groups, attrs)
	} else {
		h2.attrs = append(h2.attrs, attrs...)
	}
	h2.groupsUsed = len(h2.groups)
	return h2
}

// WithGroup returns a new Handler that qualifies later attrs with the given group name.
// The new handler shares the same ring buffer.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := h.clone()
	h2.groups = append(h2.groups, name)
	return h2
}

// All returns an iterator over stored records from oldest to newest.
// The snapshot is taken under a read lock; the iteration itself holds no lock.
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) All() iter.Seq[slog.Record] {
	return slices.Values(h.Records())
}

// Records returns a snapshot of stored records from oldest to newest.
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) Records() []slog.Record {
	c := h.core
	c.mu.RLock()
	out := c.snapshotAll()
	maxAge := c.maxAge
	c.mu.RUnlock()

	if maxAge > 0 {
		out = filterByAge(out, maxAge, time.Now())
	}
	return out
}

// Len returns the number of records physically stored in the buffer.
// It does not apply MaxAge filtering.
func (h *Handler) Len() int {
	c := h.core
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.count
}

// Capacity returns the total buffer capacity (the size passed to [New]).
func (h *Handler) Capacity() int {
	return len(h.core.buf) // buf is allocated once in New and never resized
}

// Clear removes all records from the buffer and resets flush state.
// It does not wait for any in-flight flush: a concurrent Handle that has
// already claimed its flush window will still deliver those records to FlushTo
// after Clear returns. New records written after Clear form a fresh window.
func (h *Handler) Clear() {
	c := h.core
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.buf)
	c.head = 0
	c.count = 0
	c.total = 0
	c.lastFlush = 0
}

// WriteTo writes the buffered records as a JSON array to w.
// It implements [io.WriterTo] so it can be passed directly to helpers that
// accept that interface, and can write directly to an [http.ResponseWriter].
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) WriteTo(w io.Writer) (int64, error) {
	entries := recordsToEntries(h.Records())
	data, err := json.Marshal(entries)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(data)
	return int64(n), err
}

// jsonEntry is the shape of each record in the JSON output.
type jsonEntry struct {
	Time    time.Time      `json:"time,omitzero"`
	Level   string         `json:"level"`
	Message string         `json:"msg"`
	Attrs   map[string]any `json:"attrs,omitzero"`
}

// JSON returns the buffered records as a JSON array suitable for HTTP responses.
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) JSON() ([]byte, error) {
	return json.Marshal(recordsToEntries(h.Records()))
}

func recordsToEntries(records []slog.Record) []jsonEntry {
	entries := make([]jsonEntry, len(records))
	for i, r := range records {
		entries[i] = jsonEntry{
			Time:    r.Time,
			Level:   r.Level.String(),
			Message: r.Message,
			Attrs:   collectAttrs(r),
		}
	}
	return entries
}

// clone returns a shallow copy of h with independent attr/group slices
// but the same shared recorder.
func (h *Handler) clone() *Handler {
	return &Handler{
		core:       h.core,
		level:      h.level,
		attrs:      slices.Clone(h.attrs),
		groups:     slices.Clone(h.groups),
		groupsUsed: h.groupsUsed,
	}
}

// resolveAttrs eagerly resolves LogValuers in attrs, matching the resolution
// applied to record-level attrs in Handle. Empty-key non-group attrs are
// dropped (same filtering as Handle). Group attrs are resolved recursively.
func resolveAttrs(attrs []slog.Attr) []slog.Attr {
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		v := a.Value.Resolve()
		if a.Key == "" && v.Kind() != slog.KindGroup {
			continue
		}
		if v.Kind() == slog.KindGroup {
			v = slog.GroupValue(resolveAttrs(v.Group())...)
		}
		out = append(out, slog.Attr{Key: a.Key, Value: v})
	}
	return out
}

// nestAttrs wraps attrs under a chain of group names (outermost first).
// For groups ["a", "b"] and attrs [x], it produces [slog.Group("a", slog.Group("b", x))].
func nestAttrs(groups []string, attrs []slog.Attr) []slog.Attr {
	for _, g := range slices.Backward(groups) {
		attrs = []slog.Attr{slog.Group(g, attrsToAny(attrs)...)}
	}
	return attrs
}

// mergeGroupAttrs navigates into an existing attr tree following path,
// then appends newAttrs at that level. If a matching Group attr exists
// at each path segment, it recurses into it; otherwise it creates it.
// existing must be an independent copy, elements may be modified in place.
// If duplicate Group attrs with the same name exist at a level, only the
// first match is merged into; subsequent duplicates are left intact.
func mergeGroupAttrs(existing []slog.Attr, path []string, newAttrs []slog.Attr) []slog.Attr {
	if len(path) == 0 {
		return append(existing, newAttrs...)
	}
	name := path[0]
	rest := path[1:]
	for i, a := range existing {
		if a.Key == name && a.Value.Kind() == slog.KindGroup {
			merged := mergeGroupAttrs(slices.Clone(a.Value.Group()), rest, newAttrs)
			existing[i] = slog.Group(name, attrsToAny(merged)...)
			return existing
		}
	}
	// No existing group at this level, wrap newAttrs under remaining path.
	return append(existing, nestAttrs(path, newAttrs)...)
}

func attrsToAny(attrs []slog.Attr) []any {
	out := make([]any, len(attrs))
	for i, a := range attrs {
		out[i] = a
	}
	return out
}

// collectAttrs extracts all attributes from a record into a map.
func collectAttrs(r slog.Record) map[string]any {
	if r.NumAttrs() == 0 {
		return nil
	}
	m := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		addAttrToMap(m, a)
		return true
	})
	if len(m) == 0 {
		return nil
	}
	return m
}

func addAttrToMap(m map[string]any, a slog.Attr) {
	// Values stored in the ring buffer are already resolved at Handle time
	v := a.Value
	if a.Key == "" {
		// Inline group: merge children directly into m.
		if v.Kind() == slog.KindGroup {
			for _, ga := range v.Group() {
				addAttrToMap(m, ga)
			}
		}
		return
	}
	if v.Kind() == slog.KindGroup {
		gm := make(map[string]any)
		for _, ga := range v.Group() {
			addAttrToMap(gm, ga)
		}
		m[a.Key] = gm
	} else {
		m[a.Key] = v.Any()
	}
}
