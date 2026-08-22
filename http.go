package slogbox

import (
	"io"
	"net/http"
)

// HTTPJSONMode controls whether [HTTPHandlerWithOptions] writes through
// [Handler.WriteTo] or buffers a complete representation before writing it.
type HTTPJSONMode uint8

const (
	// HTTPJSONStream writes through [Handler.WriteTo]. It is the default and
	// in default JSON v2 builds avoids materializing the complete JSON response.
	// An encoder or writer error may leave a successful HTTP response containing
	// partial JSON. The JSON v1 fallback buffers the representation in WriteTo.
	HTTPJSONStream HTTPJSONMode = iota

	// HTTPJSONBuffer calls [Handler.JSON] before writing the response. A
	// serialization error therefore occurs before any response bytes are sent.
	// A failure in the http.ResponseWriter itself may still produce a partial
	// response because bytes already accepted by a writer cannot be recalled.
	HTTPJSONBuffer
)

// HTTPError describes a failure while serving a JSON response.
//
// WriteAttempted reports whether slogbox called the destination
// [http.ResponseWriter].Write method. Such a call may commit an implicit 200
// status even when it accepts zero bytes and returns an error. It does not
// report whether middleware or another handler called ResponseWriter.WriteHeader
// before slogbox ran. BytesWritten is the exact number of response bytes
// accepted by the writer from slogbox Write calls.
type HTTPError struct {
	// Err is the underlying serialization or writer error. It is always non-nil
	// when HTTPError is provided to [HTTPHandlerOptions.OnError].
	Err error

	// WriteAttempted reports whether slogbox called ResponseWriter.Write.
	WriteAttempted bool

	// BytesWritten is the exact number of response bytes accepted by the writer.
	BytesWritten int64
}

// Error returns the underlying serialization or writer error text.
func (e *HTTPError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the underlying serialization or writer error.
func (e *HTTPError) Unwrap() error {
	return e.Err
}

// HTTPHandlerOptions configure [HTTPHandlerWithOptions]. The zero value uses
// [HTTPJSONStream] and the default error handling described by [HTTPHandler].
type HTTPHandlerOptions struct {
	// JSONMode selects streaming or serialization-buffered delivery.
	// Its zero value is HTTPJSONStream.
	JSONMode HTTPJSONMode

	// OnError handles serialization and writer errors instead of the default
	// error handling. If slogbox has not attempted a write and the callback knows
	// the ResponseWriter was not committed earlier, it may write a custom error
	// response. After a write attempt, it should only observe the failure. If nil,
	// the handler writes a default 500 response only before slogbox attempts a
	// destination write.
	OnError func(http.ResponseWriter, *http.Request, *HTTPError)
}

// HTTPHandler returns an [http.Handler] that serves the buffered records as a
// JSON array. It sets Content-Type to application/json and calls
// [Handler.WriteTo], which streams in default JSON v2 builds. An empty buffer
// produces a 200 response with "[]".
//
// onErr is called with an [HTTPError] when WriteTo returns an error. Its error
// signature is retained for compatibility; errors.Is and errors.As can inspect
// both the structured error and its underlying cause. If onErr is nil and
// serialization fails before the first call to the response writer, the handler
// replies with 500 Internal Server Error. Once Write has been called, the 200
// status may already be committed even if the writer accepted zero bytes.
func HTTPHandler(h *Handler, onErr func(http.ResponseWriter, *http.Request, error)) http.Handler {
	var callback func(http.ResponseWriter, *http.Request, *HTTPError)
	if onErr != nil {
		callback = func(w http.ResponseWriter, r *http.Request, failure *HTTPError) {
			onErr(w, r, failure)
		}
	}
	return HTTPHandlerWithOptions(h, &HTTPHandlerOptions{OnError: callback})
}

// HTTPHandlerWithOptions returns an [http.Handler] that serves the buffered
// records according to opts. Nil opts use the zero-value defaults.
//
// [HTTPJSONStream] is the default for compatibility with [HTTPHandler]. It may
// expose partial JSON when serialization or writing fails after bytes have been
// sent. [HTTPJSONBuffer] prevents serialization errors from committing a
// response, at the cost of allocating the complete JSON representation first.
// Neither mode can recover bytes already accepted by a failing
// [http.ResponseWriter].
//
// OnError is called for serialization and writer errors and replaces the
// default error handling completely. When it is nil and serialization fails
// before slogbox attempts a writer call, the handler replies with 500 Internal
// Server Error. Once slogbox has called Write, the status and response body may
// already be committed and the handler does not attempt to repair them.
// WriteAttempted cannot reveal whether middleware called WriteHeader before
// this handler ran.
func HTTPHandlerWithOptions(h *Handler, opts *HTTPHandlerOptions) http.Handler {
	var cfg HTTPHandlerOptions
	if opts != nil {
		cfg = *opts
	}
	if cfg.JSONMode != HTTPJSONStream && cfg.JSONMode != HTTPJSONBuffer {
		panic("slogbox: invalid HTTPJSONMode")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tw := &trackingWriter{w: w}

		var err error
		switch cfg.JSONMode {
		case HTTPJSONStream:
			_, err = h.WriteTo(tw)
		case HTTPJSONBuffer:
			var data []byte
			data, err = h.JSON()
			if err == nil {
				var n int
				n, err = tw.Write(data)
				if err == nil && n != len(data) {
					err = io.ErrShortWrite
				}
			}
		}
		if err == nil {
			return
		}
		if !tw.called {
			w.Header().Del("Content-Type")
		}
		if cfg.OnError != nil {
			cfg.OnError(w, r, &HTTPError{
				Err:            err,
				WriteAttempted: tw.called,
				BytesWritten:   tw.n,
			})
			return
		}
		if !tw.called {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	})
}

type trackingWriter struct {
	w      io.Writer
	called bool
	n      int64
}

func (w *trackingWriter) Write(p []byte) (int, error) {
	w.called = true
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}
