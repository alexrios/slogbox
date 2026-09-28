//go:build !goexperiment.jsonv2

package slogbox

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestJSONV1_CoercesInvalidUTF8(t *testing.T) {
	h := New(1, nil)
	r := slog.NewRecord(time.Now(), slog.LevelInfo, string([]byte{'x', 0xff}), 0)
	if err := h.Handle(t.Context(), r); err != nil {
		t.Fatalf("Handle() error: %v", err)
	}

	data, err := h.JSON()
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := entries[0]["msg"]; got != "x�" {
		t.Errorf("msg = %q, want coerced replacement rune", got)
	}
}

func TestJSONV1_NilCollectionsAndHTMLEscaping(t *testing.T) {
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
			if !bytes.Contains(data, []byte(`\u003cmessage\u003e\u0026`)) {
				t.Errorf("output does not use JSON v1 HTML escaping: %s", data)
			}
			var entries []map[string]any
			if err := json.Unmarshal(data, &entries); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			attrs := entries[0]["attrs"].(map[string]any)
			if attrs["map"] != nil {
				t.Errorf("nil map = %#v, want null", attrs["map"])
			}
			if attrs["slice"] != nil {
				t.Errorf("nil slice = %#v, want null", attrs["slice"])
			}
		})
	}
}
