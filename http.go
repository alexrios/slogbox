package slogbox

import "net/http"

// HTTPHandler returns an [http.Handler] that serves the buffered records as a
// JSON array. It sets Content-Type to application/json and calls [Handler.WriteTo]
// to stream the response. An empty buffer produces a 200 response with "[]".
//
// onErr is called when WriteTo returns an error. If onErr is nil and no response
// bytes have been written yet, the handler replies with 500 Internal Server Error.
// When bytes have already been sent (headers committed), the status code cannot
// be changed.
func HTTPHandler(h *Handler, onErr func(http.ResponseWriter, *http.Request, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n, err := h.WriteTo(w)
		if err == nil {
			return
		}
		if onErr != nil {
			onErr(w, r, err)
			return
		}
		if n == 0 {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	})
}
