//go:build !goexperiment.jsonv2

package slogbox

import (
	"encoding"
	"encoding/json"
	"io"
)

// JSON returns the buffered records as a JSON array suitable for HTTP responses.
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) JSON() ([]byte, error) {
	return json.Marshal(recordsToEntries(h.Records()))
}

// WriteTo writes the buffered records as a JSON array to w.
// It implements [io.WriterTo] so it can be passed directly to helpers that
// accept that interface, and can write directly to an [http.ResponseWriter].
// If MaxAge is set, records older than MaxAge are excluded.
// A writer error may occur after output has been written; the returned count is
// the exact number of bytes accepted by w.
func (h *Handler) WriteTo(w io.Writer) (int64, error) {
	entries := recordsToEntries(h.Records())
	data, err := json.Marshal(entries)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}

func normalizeAny(v any) any {
	err, ok := v.(error)
	if !ok {
		return v
	}
	if isNilPointer(v) {
		return nil
	}
	if supportsJSONV1Marshaling(v) {
		return v
	}
	return err.Error()
}

func supportsJSONV1Marshaling(v any) bool {
	if _, ok := v.(json.Marshaler); ok {
		return true
	}
	_, ok := v.(encoding.TextMarshaler)
	return ok
}
