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
	"sort"
	"sync"
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
	FlushOn slog.Leveler

	// FlushTo is the destination handler for flushed records.
	// Both FlushOn and FlushTo must be set for flush to be active.
	FlushTo slog.Handler

	// MaxAge excludes records older than this duration from read operations
	// (Records, All, JSON, WriteTo). Zero means no age filtering.
	// Len returns the physical count regardless of MaxAge.
	MaxAge time.Duration
}

// recorder is the shared ring buffer backing one or more Handlers.
type recorder struct {
	mu    sync.RWMutex
	buf   []slog.Record
	head  int    // next write position
	count int    // records stored (max = len(buf))
	total uint64 // monotonic write counter

	flushOn   slog.Leveler
	flushTo   slog.Handler
	lastFlush uint64 // value of total at the last flush

	maxAge time.Duration
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
	level := slog.Leveler(slog.LevelInfo)
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
		c.maxAge = opts.MaxAge
	}
	return &Handler{
		core:  c,
		level: level,
	}
}

// Enabled reports whether the handler stores records at the given level.
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	if h.level == nil {
		return level >= slog.LevelInfo
	}
	return level >= h.level.Level()
}

// Handle stores a clone of r in the ring buffer.
// If FlushOn/FlushTo are configured and the record's level reaches the
// FlushOn threshold, all records since the last flush are forwarded to FlushTo.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	// Build a new record merging handler-level and record-level attrs.
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)

	// Collect record-level attrs, skipping empty attrs per slog.Handler contract.
	var recordAttrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		if !a.Equal(slog.Attr{}) {
			recordAttrs = append(recordAttrs, a)
		}
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
	if c.flushOn != nil && nr.Level >= c.flushOn.Level() {
		n := int(c.total - c.lastFlush)
		if n > c.count {
			n = c.count
		}
		flushRecords = c.snapshotLast(n)
	}
	c.mu.Unlock()

	// Flush outside the lock to avoid blocking writers if FlushTo do I/O.
	var flushed int
	for _, fr := range flushRecords {
		if err := c.flushTo.Handle(context.Background(), fr); err != nil {
			// Update lastFlush for successfully flushed records before returning an error.
			c.mu.Lock()
			c.lastFlush += uint64(flushed)
			c.mu.Unlock()
			return err
		}
		flushed++
	}
	if flushed > 0 {
		c.mu.Lock()
		c.lastFlush += uint64(flushed)
		c.mu.Unlock()
	}
	return nil
}

// WithAttrs returns a new Handler whose records will include the given attrs.
// The new handler shares the same ring buffer.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	h2 := h.clone()
	pending := h2.groups[h2.groupsUsed:]
	if len(pending) > 0 {
		nested := nestAttrs(pending, attrs)
		h2.attrs = mergeGroupAttrs(h2.attrs, h2.groups[:h2.groupsUsed], nested)
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
func (h *Handler) All() iter.Seq[slog.Record] {
	snapshot := h.Records()
	return func(yield func(slog.Record) bool) {
		for _, r := range snapshot {
			if !yield(r) {
				return
			}
		}
	}
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

// snapshotAll returns all buffered records oldest-to-newest.
// Must be called while holding c.mu.
func (c *recorder) snapshotAll() []slog.Record {
	out := make([]slog.Record, c.count)
	if c.count < len(c.buf) {
		copy(out, c.buf[:c.count])
	} else {
		n := copy(out, c.buf[c.head:])
		copy(out[n:], c.buf[:c.head])
	}
	return out
}

// snapshotLast returns the last n records (the newest n), oldest-to-newest.
// Must be called while holding c.mu.
func (c *recorder) snapshotLast(n int) []slog.Record {
	if n <= 0 {
		return nil
	}
	if n > c.count {
		n = c.count
	}
	out := make([]slog.Record, n)
	// The newest record is at (head-1), the n-th newest at (head-n).
	start := (c.head - n + len(c.buf)) % len(c.buf)
	if start+n <= len(c.buf) {
		copy(out, c.buf[start:start+n])
	} else {
		first := copy(out, c.buf[start:])
		copy(out[first:], c.buf[:n-first])
	}
	return out
}

// filterByAge returns the sub-slice of records whose time is within maxAge of now.
// Records are assumed to be in chronological order (oldest first), so binary search
// finds the cutoff point with no extra allocation.
func filterByAge(records []slog.Record, maxAge time.Duration, now time.Time) []slog.Record {
	cutoff := now.Add(-maxAge)
	i := sort.Search(len(records), func(i int) bool {
		return !records[i].Time.Before(cutoff)
	})
	return records[i:]
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
	c := h.core
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.buf)
}

// Clear removes all records from the buffer.
func (h *Handler) Clear() {
	c := h.core
	c.mu.Lock()
	clear(c.buf)
	c.head = 0
	c.count = 0
	c.total = 0
	c.lastFlush = 0
	c.mu.Unlock()
}

// WriteTo writes the buffered records as a JSON array to w.
// It implements [io.WriterTo], making it composable with [http.ResponseWriter].
func (h *Handler) WriteTo(w io.Writer) (int64, error) {
	entries := recordsToEntries(h.Records())
	data, err := json.Marshal(entries)
	if err != nil {
		return 0, err
	}
	cw := &countingWriter{w: w}
	if _, err := cw.Write(data); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

// countingWriter wraps an io.Writer and counts bytes written.
type countingWriter struct {
	w io.Writer
	n int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

// jsonEntry is the shape of each record in the JSON output.
type jsonEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"msg"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// JSON returns the buffered records as a JSON array suitable for HTTP responses.
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

// nestAttrs wraps attrs under a chain of group names.
// For groups ["a", "b"] and attrs [x], it produces slog.Group("a", slog.Group("b", x)).
func nestAttrs(groups []string, attrs []slog.Attr) []slog.Attr {
	for i := len(groups) - 1; i >= 0; i-- {
		attrs = []slog.Attr{slog.Group(groups[i], attrsToAny(attrs)...)}
	}
	return attrs
}

// mergeGroupAttrs navigates into an existing attr tree following path,
// then appends newAttrs at that level. If a matching Group attr exists
// at each path segment, it recurses into it; otherwise it creates it.
func mergeGroupAttrs(existing []slog.Attr, path []string, newAttrs []slog.Attr) []slog.Attr {
	if len(path) == 0 {
		return append(existing, newAttrs...)
	}
	name := path[0]
	rest := path[1:]
	for i, a := range existing {
		if a.Key == name && a.Value.Kind() == slog.KindGroup {
			merged := mergeGroupAttrs(a.Value.Group(), rest, newAttrs)
			existing[i] = slog.Group(name, attrsToAny(merged)...)
			return existing
		}
	}
	// No existing group at this level — wrap newAttrs under remaining path.
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
	return m
}

func addAttrToMap(m map[string]any, a slog.Attr) {
	if a.Key == "" {
		return
	}
	v := a.Value.Resolve()
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
