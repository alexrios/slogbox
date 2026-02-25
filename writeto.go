//go:build !goexperiment.jsonv2

package slogbox

import (
	"encoding/json"
	"io"
)

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
