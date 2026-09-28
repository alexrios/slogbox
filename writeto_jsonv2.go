//go:build goexperiment.jsonv2

package slogbox

import (
	"encoding"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"io"
	"reflect"
)

// JSON returns the buffered records as a JSON array suitable for HTTP responses.
// If MaxAge is set, records older than MaxAge are excluded.
func (h *Handler) JSON() ([]byte, error) {
	return jsonv2.Marshal(recordsToEntries(h.Records()))
}

// WriteTo writes the buffered records as a JSON array to w.
// It implements [io.WriterTo] so it can be passed directly to helpers that
// accept that interface, and can write directly to an [http.ResponseWriter].
// If MaxAge is set, records older than MaxAge are excluded.
//
// This implementation streams records one at a time through a [jsontext.Encoder],
// avoiding a single large intermediate allocation for the entire JSON output.
// An error may occur after output has been written; the returned count is the
// exact number of bytes accepted by w.
func (h *Handler) WriteTo(w io.Writer) (int64, error) {
	records := h.Records()
	cw := &countWriter{w: w}
	enc := jsontext.NewEncoder(cw)

	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return cw.n, err
	}
	for _, r := range records {
		entry := jsonEntry{
			Time:    r.Time,
			Level:   r.Level.String(),
			Message: r.Message,
			Attrs:   collectAttrs(r),
		}
		if err := jsonv2.MarshalEncode(enc, &entry); err != nil {
			return cw.n, err
		}
	}
	if err := enc.WriteToken(jsontext.EndArray); err != nil {
		return cw.n, err
	}

	// jsontext.Encoder appends a newline after each top-level value.
	// Trim it because WriteTo promises no trailing whitespace.
	if cw.pending {
		cw.pending = false
	}

	return cw.n, nil
}

// countWriter wraps an io.Writer, tracks total bytes written, and holds back
// a single trailing newline. The encoder appends '\n' after each top-level
// value; suppressing it preserves WriteTo's no-trailing-whitespace contract.
type countWriter struct {
	w       io.Writer
	n       int64
	err     error
	pending bool // a '\n' is waiting to be written
}

func (cw *countWriter) Write(p []byte) (int, error) {
	if cw.err != nil {
		return 0, cw.err
	}

	// Flush any held-back newline before writing new data.
	if cw.pending && len(p) > 0 {
		cw.pending = false
		nn, err := cw.w.Write([]byte{'\n'})
		cw.n += int64(nn)
		if err == nil && nn != 1 {
			err = io.ErrShortWrite
		}
		if err != nil {
			cw.err = err
			return 0, err
		}
	}

	originalLen := len(p)
	holdNewline := len(p) > 0 && p[len(p)-1] == '\n'
	// Hold back a trailing newline.
	if holdNewline {
		p = p[:len(p)-1]
	}
	if len(p) > 0 {
		n, err := cw.w.Write(p)
		cw.n += int64(n)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			cw.err = err
			return n, err
		}
	}
	if holdNewline {
		cw.pending = true
	}
	// The trailing newline is intentionally consumed even though it is held
	// back. Reporting originalLen is required by the io.Writer contract.
	return originalLen, nil
}

func normalizeAny(v any) any {
	err, ok := v.(error)
	if !ok {
		return v
	}
	if isNilPointer(v) {
		return nil
	}
	if supportsJSONV2Marshaling(v) {
		return v
	}
	return err.Error()
}

func supportsJSONV2Marshaling(v any) bool {
	t := reflect.TypeOf(v)
	// JSON v2 also calls pointer-receiver methods on non-pointer values.
	for _, t := range [2]reflect.Type{t, reflect.PointerTo(t)} {
		if t.Implements(reflect.TypeFor[jsonv2.MarshalerTo]()) ||
			t.Implements(reflect.TypeFor[jsonv2.Marshaler]()) ||
			t.Implements(reflect.TypeFor[encoding.TextAppender]()) ||
			t.Implements(reflect.TypeFor[encoding.TextMarshaler]()) {
			return true
		}
	}
	return false
}
