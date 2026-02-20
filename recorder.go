package slogbox

import (
	"log/slog"
	"slices"
	"sync"
	"time"
)

// recorder is the shared ring buffer backing one or more Handlers.
type recorder struct {
	mu    sync.RWMutex
	buf   []slog.Record
	head  int    // next write position
	count int    // records stored (max = len(buf))
	total uint64 // monotonic write counter

	flushOn   slog.Leveler
	flushTo   slog.Handler
	lastFlush uint64 // value of total claimed by the last flush; updated inside mu

	maxAge time.Duration
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
	n = min(n, c.count)
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
// Records must be in chronological order (oldest first) — snapshotAll always
// reconstructs the ring in that order. Binary search finds the cutoff in O(log n)
// with no extra allocation.
//
// Callers must not pass a slice that contains out-of-order timestamps; binary
// search will silently return incorrect results if the invariant is violated.
func filterByAge(records []slog.Record, maxAge time.Duration, now time.Time) []slog.Record {
	cutoff := now.Add(-maxAge)
	i, _ := slices.BinarySearchFunc(records, cutoff, func(r slog.Record, t time.Time) int {
		return r.Time.Compare(t)
	})
	return records[i:]
}
