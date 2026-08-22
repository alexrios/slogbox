//go:build goexperiment.jsonv2

package slogbox

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestJSONV2_StrictUTF8(t *testing.T) {
	h := New(1, nil)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, string([]byte{'x', 0xff}), 0)
	if err := h.Handle(t.Context(), r); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	if _, err := h.JSON(); err == nil {
		t.Error("JSON() error = nil, want invalid UTF-8 error")
	}
	if _, err := h.WriteTo(io.Discard); err == nil {
		t.Error("WriteTo() error = nil, want invalid UTF-8 error")
	}
}

func TestJSONV2_NilCollectionsAndMinimalEscaping(t *testing.T) {
	h := New(1, nil)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "<message>&", 0)
	r.AddAttrs(
		slog.Any("map", map[string]int(nil)),
		slog.Any("slice", []int(nil)),
	)
	if err := h.Handle(t.Context(), r); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	outputs := map[string]func() ([]byte, error){
		"JSON": h.JSON,
		"WriteTo": func() ([]byte, error) {
			var buf bytes.Buffer
			_, err := h.WriteTo(&buf)
			return buf.Bytes(), err
		},
	}
	for name, output := range outputs {
		t.Run(name, func(t *testing.T) {
			data, err := output()
			if err != nil {
				t.Fatalf("%s() error: %v", name, err)
			}
			if !bytes.Contains(data, []byte("<message>&")) {
				t.Errorf("output uses more than minimal escaping: %s", data)
			}
			var entries []map[string]any
			if err := json.Unmarshal(data, &entries); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			attrs := entries[0]["attrs"].(map[string]any)
			if got, ok := attrs["map"].(map[string]any); !ok || len(got) != 0 {
				t.Errorf("nil map = %#v, want empty JSON object", attrs["map"])
			}
			if got, ok := attrs["slice"].([]any); !ok || len(got) != 0 {
				t.Errorf("nil slice = %#v, want empty JSON array", attrs["slice"])
			}
		})
	}
}

func TestCountWriter_ConsumesTrailingNewlineAndCountsOutput(t *testing.T) {
	var buf bytes.Buffer
	cw := &countWriter{w: &buf}

	n, err := cw.Write([]byte("abc\n"))
	if err != nil {
		t.Fatalf("first Write() error: %v", err)
	}
	if n != 4 {
		t.Errorf("first Write() = %d, want 4 consumed bytes", n)
	}
	if cw.n != 3 || buf.String() != "abc" || !cw.pending {
		t.Errorf("after first Write: count=%d output=%q pending=%v", cw.n, buf.String(), cw.pending)
	}

	n, err = cw.Write([]byte("def\n"))
	if err != nil {
		t.Fatalf("second Write() error: %v", err)
	}
	if n != 4 {
		t.Errorf("second Write() = %d, want 4 consumed bytes", n)
	}
	if cw.n != 7 || buf.String() != "abc\ndef" || !cw.pending {
		t.Errorf("after second Write: count=%d output=%q pending=%v", cw.n, buf.String(), cw.pending)
	}
}

func TestCountWriter_PropagatesWriterFailures(t *testing.T) {
	errBoom := errors.New("write failed")
	t.Run("body", func(t *testing.T) {
		cw := &countWriter{w: errWriter{err: errBoom}}
		n, err := cw.Write([]byte("body"))
		if n != 0 || !errors.Is(err, errBoom) {
			t.Fatalf("Write() = (%d, %v), want (0, %v)", n, err, errBoom)
		}
	})

	t.Run("held newline", func(t *testing.T) {
		cw := &countWriter{w: errWriter{err: errBoom}}
		if n, err := cw.Write([]byte("\n")); n != 1 || err != nil {
			t.Fatalf("newline Write() = (%d, %v), want (1, nil)", n, err)
		}
		n, err := cw.Write([]byte("body"))
		if n != 0 || !errors.Is(err, errBoom) {
			t.Fatalf("flush Write() = (%d, %v), want (0, %v)", n, err, errBoom)
		}
	})

	t.Run("persistent error", func(t *testing.T) {
		cw := &countWriter{w: errWriter{err: errBoom}}
		if _, err := cw.Write([]byte("first")); !errors.Is(err, errBoom) {
			t.Fatalf("first Write() error = %v, want %v", err, errBoom)
		}
		n, err := cw.Write([]byte("second"))
		if n != 0 || !errors.Is(err, errBoom) {
			t.Fatalf("second Write() = (%d, %v), want (0, %v)", n, err, errBoom)
		}
	})

	t.Run("short write", func(t *testing.T) {
		cw := &countWriter{w: shortWriter{}}
		n, err := cw.Write([]byte("body"))
		if n != 3 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("Write() = (%d, %v), want (3, %v)", n, err, io.ErrShortWrite)
		}
	})

	t.Run("short write flushing held newline", func(t *testing.T) {
		cw := &countWriter{w: shortWriter{}}
		if n, err := cw.Write([]byte("\n")); n != 1 || err != nil {
			t.Fatalf("newline Write() = (%d, %v), want (1, nil)", n, err)
		}
		n, err := cw.Write([]byte("body"))
		if n != 0 || !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("flush Write() = (%d, %v), want (0, %v)", n, err, io.ErrShortWrite)
		}
	})
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(0, len(p)-1), nil }

func TestWriteToV2_EndArrayWriterFailure(t *testing.T) {
	errBoom := errors.New("end array write failed")
	h := New(1, nil)

	n, err := h.WriteTo(errWriter{err: errBoom})
	if n != 0 || !errors.Is(err, errBoom) {
		t.Fatalf("WriteTo() = (%d, %v), want (0, %v)", n, err, errBoom)
	}
}

func TestWriteToV2_ReturnsExactPartialByteCountOnWriterError(t *testing.T) {
	errBoom := errors.New("write failed after prefix")
	w := &prefixErrorWriter{limit: 8, err: errBoom}
	h := New(1, nil)
	slog.New(h).Info("valid record")

	n, err := h.WriteTo(w)
	if !errors.Is(err, errBoom) {
		t.Fatalf("WriteTo() error = %v, want %v", err, errBoom)
	}
	if n != int64(w.buf.Len()) {
		t.Errorf("WriteTo() count = %d, actual bytes = %d", n, w.buf.Len())
	}
	if n == 0 {
		t.Fatal("WriteTo() wrote no bytes, want a partial representation")
	}
	if json.Valid(w.buf.Bytes()) {
		t.Errorf("partial output unexpectedly contains valid JSON: %q", w.buf.Bytes())
	}
}

type prefixErrorWriter struct {
	buf   bytes.Buffer
	limit int
	err   error
}

func (w *prefixErrorWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit {
		p = p[:w.limit]
	}
	n, _ := w.buf.Write(p)
	return n, w.err
}

type marshalerToError struct{}

func (marshalerToError) Error() string { return "plain marshaler-to error" }

func (marshalerToError) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("marshaler-to"))
}

type textAppenderError struct{}

func (textAppenderError) Error() string { return "plain text-appender error" }

func (textAppenderError) AppendText(dst []byte) ([]byte, error) {
	return append(dst, "text-appender"...), nil
}

func TestJSONV2_PreservesV2MarshalersOnErrors(t *testing.T) {
	var _ jsonv2.MarshalerTo = marshalerToError{}

	h := New(1, nil)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, "custom errors", 0)
	r.AddAttrs(
		slog.Any("marshaler_to", marshalerToError{}),
		slog.Any("text_appender", textAppenderError{}),
	)
	if err := h.Handle(t.Context(), r); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	outputs := map[string]func() ([]byte, error){
		"JSON": h.JSON,
		"WriteTo": func() ([]byte, error) {
			var buf bytes.Buffer
			_, err := h.WriteTo(&buf)
			return buf.Bytes(), err
		},
	}
	for name, output := range outputs {
		t.Run(name, func(t *testing.T) {
			data, err := output()
			if err != nil {
				t.Fatalf("%s() error: %v", name, err)
			}
			var entries []map[string]any
			if err := json.Unmarshal(data, &entries); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			attrs := entries[0]["attrs"].(map[string]any)
			if got := attrs["marshaler_to"]; got != "marshaler-to" {
				t.Errorf("marshaler_to = %#v, want marshaler-to", got)
			}
			if got := attrs["text_appender"]; got != "text-appender" {
				t.Errorf("text_appender = %#v, want text-appender", got)
			}
		})
	}
}
