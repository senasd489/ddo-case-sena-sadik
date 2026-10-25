package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush lets streaming handlers (SSE) work through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logging attaches a request-scoped logger + request id and writes one
// structured line per request. Headers and bodies are never logged, so
// passwords and tokens cannot leak into logs.
func Logging(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" || len(id) > 64 {
				var b [8]byte
				_, _ = rand.Read(b[:])
				id = hex.EncodeToString(b[:])
			}
			w.Header().Set("X-Request-ID", id)
			l := base.With("request_id", id)
			ctx := context.WithValue(context.WithValue(r.Context(), loggerKey, l), requestIDKey, id)
			rec := &statusRecorder{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(rec, r.WithContext(ctx))
			dur := time.Since(start)

			route := chi.RouteContext(ctx).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			metrics.HTTPRequests.Inc(r.Method, route, strconv.Itoa(rec.status))
			metrics.HTTPDuration.Observe(dur.Seconds(), r.Method, route)
			if r.URL.Path == "/health" || r.URL.Path == "/metrics" {
				return
			}
			l.Info("http_request",
				"method", r.Method, "path", r.URL.Path, "route", route,
				"status", rec.status, "bytes", rec.bytes, "duration_ms", dur.Milliseconds())
		})
	}
}

// Recover converts panics into 500 responses with the standard envelope.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				LoggerFrom(r.Context()).Error("panic", "value", v, "stack", string(debug.Stack()))
				WriteError(w, r, NewError(http.StatusInternalServerError, "internal", "beklenmeyen bir hata oluştu"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds every request with a context deadline; DB calls made with
// r.Context() are cancelled when it fires.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// CORS allows the configured frontend origin (dev server); in compose the
// frontend proxies /api so this mostly matters for local development.
func CORS(origin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" && (o == origin || origin == "*") {
				w.Header().Set("Access-Control-Allow-Origin", o)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
