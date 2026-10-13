// Package httpx holds the one response shape every endpoint uses:
//
//	success -> { "success": true,  "data": ..., "error": null }
//	failure -> { "success": false, "data": null, "error": { "code", "message" } }
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Error is a failure we deliberately raise and are happy to show the client.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func E(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func OK(w http.ResponseWriter, status int, data any) {
	write(w, status, map[string]any{"success": true, "data": data, "error": nil})
}

func Fail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, map[string]any{
		"success": false, "data": nil,
		"error": map[string]string{"code": code, "message": message},
	})
}

// Handler is an http.HandlerFunc that may return an error; Wrap turns that error into
// the failure shape, so handlers never write error responses by hand.
type Handler func(w http.ResponseWriter, r *http.Request) error

func Wrap(h Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		var e *Error
		if errors.As(err, &e) {
			Fail(w, e.Status, e.Code, e.Message)
			return
		}
		// Unexpected: log the real error, tell the client nothing useful.
		slog.Error("unhandled", "method", r.Method, "path", r.URL.Path, "err", err)
		Fail(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Something went wrong.")
	}
}

// maxBody caps request bodies; no JSON endpoint here needs more.
const maxBody = 100 << 10

// Decode reads a JSON body into v and rejects unknown fields and oversized bodies.
func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return E(http.StatusBadRequest, "INVALID_INPUT", "Invalid request body: "+err.Error())
	}
	return nil
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
