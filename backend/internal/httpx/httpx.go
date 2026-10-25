// Package httpx holds the shared HTTP plumbing: JSON encoding, the uniform
// error envelope and request validation helpers.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Error is the single error type handlers return. It always renders as
// {"error":{"code":"…","message":"…"}}.
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func NewError(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

var (
	ErrUnauthorized = NewError(http.StatusUnauthorized, "unauthorized", "kimlik doğrulama gerekli")
	ErrForbidden    = NewError(http.StatusForbidden, "forbidden", "bu işlem için yetkiniz yok")
	ErrNotFound     = NewError(http.StatusNotFound, "not_found", "kayıt bulunamadı")
)

func BadRequest(msg string) *Error { return NewError(http.StatusBadRequest, "bad_request", msg) }
func Validation(details map[string]any) *Error {
	e := NewError(http.StatusUnprocessableEntity, "validation_failed", "girdi doğrulaması başarısız")
	e.Details = details
	return e
}
func Conflict(code, msg string) *Error { return NewError(http.StatusConflict, code, msg) }

// HandlerFunc is an http handler that may return an error.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// H adapts a HandlerFunc to http.HandlerFunc, rendering any returned error.
func H(fn HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			WriteError(w, r, err)
		}
	}
}

func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		LoggerFrom(r.Context()).Error("internal error", "err", err.Error())
		e = NewError(http.StatusInternalServerError, "internal", "beklenmeyen bir hata oluştu")
	}
	JSON(w, e.Status, map[string]any{"error": e})
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Decode reads a JSON body (max 1 MiB) and rejects unknown fields.
func Decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return BadRequest("geçersiz JSON gövdesi: " + err.Error())
	}
	return nil
}
