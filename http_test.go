package slogbox_test

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alexrios/slogbox"
)

func TestHTTPHandler_ServesJSON(t *testing.T) {
	h := slogbox.New(10, nil)
	logger := slog.New(h)
	logger.Info("one")
	logger.Warn("two")

	rec := httptest.NewRecorder()
	slogbox.HTTPHandler(h, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var entries []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0]["msg"] != "one" || entries[1]["msg"] != "two" {
		t.Errorf("messages = [%v, %v], want [one, two]", entries[0]["msg"], entries[1]["msg"])
	}
}

func TestHTTPHandler_EmptyBuffer(t *testing.T) {
	h := slogbox.New(10, nil)

	rec := httptest.NewRecorder()
	slogbox.HTTPHandler(h, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	var entries []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

// failResponseWriter is an http.ResponseWriter whose Write always fails
// before any bytes reach the client (simulates a marshal-time or pre-write error).
type failResponseWriter struct {
	*httptest.ResponseRecorder
}

func (w *failResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestHTTPHandler_Default500OnError(t *testing.T) {
	h := slogbox.New(10, nil)
	slog.New(h).Info("test")

	w := &failResponseWriter{httptest.NewRecorder()}
	slogbox.HTTPHandler(h, nil).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHTTPHandler_CustomErrorHandler(t *testing.T) {
	h := slogbox.New(10, nil)
	slog.New(h).Info("test")

	var called bool
	var gotErr error
	onErr := func(w http.ResponseWriter, _ *http.Request, err error) {
		called = true
		gotErr = err
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	w := &failResponseWriter{httptest.NewRecorder()}
	slogbox.HTTPHandler(h, onErr).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if !called {
		t.Fatal("error handler was not called")
	}
	if gotErr == nil {
		t.Fatal("error handler received nil error")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
