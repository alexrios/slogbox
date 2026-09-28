package slogbox_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestHTTPHandler_LegacyErrorHandlerReceivesHTTPError(t *testing.T) {
	h := slogbox.New(10, nil)
	slog.New(h).Info("test")
	errBoom := errors.New("write failed")

	var called bool
	var gotErr error
	onErr := func(_ http.ResponseWriter, _ *http.Request, err error) {
		called = true
		gotErr = err
	}

	w := &commitThenFailResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		err:              errBoom,
	}
	slogbox.HTTPHandler(h, onErr).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if !called {
		t.Fatal("error handler was not called")
	}
	if !errors.Is(gotErr, errBoom) {
		t.Fatalf("error handler error = %v, want %v", gotErr, errBoom)
	}
	var failure *slogbox.HTTPError
	if !errors.As(gotErr, &failure) {
		t.Fatalf("error handler error type = %T, want *slogbox.HTTPError", gotErr)
	}
	if !failure.WriteAttempted {
		t.Error("WriteAttempted = false, want true")
	}
	if failure.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0", failure.BytesWritten)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want already-committed 200", w.Code)
	}
	if w.calls != 1 {
		t.Errorf("Write calls = %d, want 1", w.calls)
	}
}

type commitThenFailResponseWriter struct {
	*httptest.ResponseRecorder
	calls int
	err   error
}

func (w *commitThenFailResponseWriter) Write([]byte) (int, error) {
	w.calls++
	w.ResponseRecorder.WriteHeader(http.StatusOK)
	return 0, w.err
}

func TestHTTPHandler_DoesNotAttempt500AfterWriterCalled(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("record")
	errBoom := errors.New("write failed")
	w := &commitThenFailResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		err:              errBoom,
	}

	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
	}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want already-committed 200", w.Code)
	}
	if w.calls != 1 {
		t.Errorf("Write calls = %d, want 1; handler attempted to repair a committed response", w.calls)
	}
}

func TestHTTPHandlerWithOptions_ReportsWriteAttemptWithZeroBytes(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("record")
	errBoom := errors.New("write failed")
	w := &commitThenFailResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		err:              errBoom,
	}

	var gotFailure *slogbox.HTTPError
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
		OnError: func(_ http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
			gotFailure = failure
		},
	}).ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	if gotFailure == nil {
		t.Fatal("OnError was not called")
	}
	if !errors.Is(gotFailure, errBoom) {
		t.Errorf("OnError error = %v, want %v", gotFailure, errBoom)
	}
	if !gotFailure.WriteAttempted {
		t.Error("WriteAttempted = false, want true after ResponseWriter.Write was called")
	}
	if gotFailure.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0", gotFailure.BytesWritten)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want already-committed 200", w.Code)
	}
	if w.calls != 1 {
		t.Errorf("Write calls = %d, want 1", w.calls)
	}
}

func TestHTTPHandlerWithOptions_BufferedSuccess(t *testing.T) {
	h := slogbox.New(10, nil)
	slog.New(h).Info("buffered", "key", "value")

	rec := httptest.NewRecorder()
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
	}).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var entries []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(entries) != 1 || entries[0]["msg"] != "buffered" {
		t.Errorf("entries = %#v, want one buffered record", entries)
	}
}

func TestHTTPHandlerWithOptions_BufferedSerializationErrorIsAtomic(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("unsupported", "function", func() {})

	rec := httptest.NewRecorder()
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
	}).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("body is empty, want an error response")
	}
	if json.Valid(rec.Body.Bytes()) {
		t.Errorf("body contains JSON after serialization failed: %q", rec.Body.Bytes())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain default error response", ct)
	}
}

func TestHTTPHandlerWithOptions_BufferedCustomErrorHandler(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("unsupported", "function", func() {})

	var gotFailure *slogbox.HTTPError
	rec := httptest.NewRecorder()
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
		OnError: func(w http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
			gotFailure = failure
			if failure.WriteAttempted {
				t.Error("WriteAttempted = true for a buffered serialization failure")
				return
			}
			http.Error(w, "serialization unavailable", http.StatusServiceUnavailable)
		},
	}).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if gotFailure == nil {
		t.Fatal("OnError received nil error")
	}
	if gotFailure.Err == nil {
		t.Fatal("OnError underlying error is nil")
	}
	if gotFailure.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0", gotFailure.BytesWritten)
	}
	if gotFailure.Error() != gotFailure.Err.Error() {
		t.Errorf("HTTPError text = %q, want %q", gotFailure.Error(), gotFailure.Err.Error())
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "serialization unavailable") {
		t.Errorf("body = %q, want custom error message", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain custom error response", ct)
	}
}

func TestHTTPHandlerWithOptions_RejectsInvalidMode(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("HTTPHandlerWithOptions did not panic for an invalid JSON mode")
		}
	}()
	slogbox.HTTPHandlerWithOptions(slogbox.New(1, nil), &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONMode(255),
	})
}

func TestHTTPHandlerWithOptions_ReportsShortWrites(t *testing.T) {
	for _, mode := range []slogbox.HTTPJSONMode{slogbox.HTTPJSONStream, slogbox.HTTPJSONBuffer} {
		t.Run(modeName(mode), func(t *testing.T) {
			h := slogbox.New(1, nil)
			slog.New(h).Info("record")

			var gotFailure *slogbox.HTTPError
			w := &shortResponseWriter{ResponseRecorder: httptest.NewRecorder()}
			slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
				JSONMode: mode,
				OnError: func(_ http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
					gotFailure = failure
				},
			}).ServeHTTP(w, httptest.NewRequest("GET", "/debug/logs", nil))

			if !errors.Is(gotFailure, io.ErrShortWrite) {
				t.Fatalf("OnError error = %v, want %v", gotFailure, io.ErrShortWrite)
			}
			if !gotFailure.WriteAttempted {
				t.Error("WriteAttempted = false, want true")
			}
			if gotFailure.BytesWritten != int64(w.Body.Len()) {
				t.Errorf("BytesWritten = %d, actual body = %d", gotFailure.BytesWritten, w.Body.Len())
			}
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Errorf("partial response = (status %d, %d bytes), want status 200 and bytes", w.Code, w.Body.Len())
			}
			if json.Valid(w.Body.Bytes()) {
				t.Errorf("short write unexpectedly produced complete JSON: %q", w.Body.Bytes())
			}
		})
	}
}

func TestHTTPHandlerWithOptions_ReportsPartialWriterError(t *testing.T) {
	for _, mode := range []slogbox.HTTPJSONMode{slogbox.HTTPJSONStream, slogbox.HTTPJSONBuffer} {
		t.Run(modeName(mode), func(t *testing.T) {
			h := slogbox.New(1, nil)
			slog.New(h).Info("record")
			errBoom := errors.New("write failed after partial delivery")

			var gotFailure *slogbox.HTTPError
			w := &partialErrorResponseWriter{
				ResponseRecorder: httptest.NewRecorder(),
				accepted:         1,
				err:              errBoom,
			}
			slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
				JSONMode: mode,
				OnError: func(_ http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
					gotFailure = failure
				},
			}).ServeHTTP(w, httptest.NewRequest("GET", "/debug/logs", nil))

			if !errors.Is(gotFailure, errBoom) {
				t.Fatalf("OnError error = %v, want %v", gotFailure, errBoom)
			}
			if !gotFailure.WriteAttempted {
				t.Error("WriteAttempted = false, want true")
			}
			if gotFailure.BytesWritten != int64(w.accepted) {
				t.Errorf("BytesWritten = %d, want %d", gotFailure.BytesWritten, w.accepted)
			}
			if w.Body.Len() != w.accepted {
				t.Errorf("body length = %d, want %d", w.Body.Len(), w.accepted)
			}
		})
	}
}

func TestHTTPHandlerWithOptions_PrecommittedResponseIsNotWriteAttempt(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("unsupported", "function", func() {})

	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusNoContent)
	var gotFailure *slogbox.HTTPError
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONBuffer,
		OnError: func(_ http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
			gotFailure = failure
		},
	}).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if gotFailure == nil {
		t.Fatal("OnError was not called")
	}
	if gotFailure.WriteAttempted {
		t.Error("WriteAttempted = true, want false when only earlier middleware committed the response")
	}
	if gotFailure.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0", gotFailure.BytesWritten)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want previously committed 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty previously committed response", rec.Body.String())
	}
}

func TestHTTPHandlerWithOptions_StreamSerializationErrorIsHandled(t *testing.T) {
	h := slogbox.New(1, nil)
	slog.New(h).Info("unsupported", "function", func() {})

	var gotFailure *slogbox.HTTPError
	rec := httptest.NewRecorder()
	slogbox.HTTPHandlerWithOptions(h, &slogbox.HTTPHandlerOptions{
		JSONMode: slogbox.HTTPJSONStream,
		OnError: func(w http.ResponseWriter, _ *http.Request, failure *slogbox.HTTPError) {
			gotFailure = failure
			if !failure.WriteAttempted {
				http.Error(w, "serialization unavailable", http.StatusServiceUnavailable)
			}
		},
	}).ServeHTTP(rec, httptest.NewRequest("GET", "/debug/logs", nil))

	if gotFailure == nil || gotFailure.Err == nil {
		t.Fatal("OnError did not receive the streaming serialization error")
	}
	if gotFailure.WriteAttempted {
		if gotFailure.BytesWritten != int64(rec.Body.Len()) {
			t.Errorf("BytesWritten = %d, body length = %d", gotFailure.BytesWritten, rec.Body.Len())
		}
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("attempted response = (status %d, %d bytes), want non-empty status 200 response", rec.Code, rec.Body.Len())
		}
		return
	}
	if gotFailure.BytesWritten != 0 {
		t.Errorf("BytesWritten = %d, want 0 before a write attempt", gotFailure.BytesWritten)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want custom 503", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Error("body is empty, want custom error response")
	}
}

type shortResponseWriter struct {
	*httptest.ResponseRecorder
}

type partialErrorResponseWriter struct {
	*httptest.ResponseRecorder
	accepted int
	err      error
}

func (w *partialErrorResponseWriter) Write(p []byte) (int, error) {
	n := min(w.accepted, len(p))
	if n == 0 {
		return 0, w.err
	}
	if _, err := w.ResponseRecorder.Write(p[:n]); err != nil {
		return 0, err
	}
	return n, w.err
}

func (w *shortResponseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return w.ResponseRecorder.Write(p[:len(p)-1])
}

func modeName(mode slogbox.HTTPJSONMode) string {
	if mode == slogbox.HTTPJSONBuffer {
		return "buffer"
	}
	return "stream"
}
