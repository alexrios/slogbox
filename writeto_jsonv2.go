//go:build goexperiment.jsonv2

package slogbox

import (
	jsonv2 "encoding/json/v2"
	"encoding/json/jsontext"
	"io"
)

// WriteTo writes the buffered records as a JSON array to w.
// It implements [io.WriterTo] so it can be passed directly to helpers that
// accept that interface, and can write directly to an [http.ResponseWriter].
// If MaxAge is set, records older than MaxAge are excluded.
//
// This implementation streams records one at a time through a [jsontext.Encoder],
// avoiding a single large intermediate allocation for the entire JSON output.
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
	// Trim it so WriteTo output is byte-compatible with the non-streaming path.
	if cw.pending {
		cw.pending = false
	}

	return cw.n, nil
}

// countWriter wraps an io.Writer, tracks total bytes written, and holds back
// a single trailing newline. The encoder appends '\n' after each top-level
// value; suppressing it keeps output identical to json.Marshal.
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
		if err != nil {
			cw.err = err
			return 0, err
		}
	}
	// Hold back a trailing newline.
	if len(p) > 0 && p[len(p)-1] == '\n' {
		p = p[:len(p)-1]
		cw.pending = true
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	if err != nil {
		cw.err = err
	}
	return n, err
}
