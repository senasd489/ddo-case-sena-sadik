// Package server wires configuration, the database pool and every domain
// module into a single chi router.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/config"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

type Server struct {
	cfg  config.Config
	pool *pgxpool.Pool
	log  *slog.Logger
}

func New(cfg config.Config, pool *pgxpool.Pool, log *slog.Logger) *Server {
	metrics.NewGaugeFunc("enerjihat_db_pool_acquired_conns", "Kullanımdaki DB bağlantıları", func() float64 {
		return float64(pool.Stat().AcquiredConns())
	})
	return &Server{cfg: cfg, pool: pool, log: log}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(httpx.Recover, httpx.Logging(s.log), httpx.CORS(s.cfg.CORSOrigin))

	r.Get("/health", s.health)
	r.Handle("/metrics", metrics.Handler())

	r.Group(func(r chi.Router) {
		r.Use(httpx.Timeout(s.cfg.RequestTimeout))
		r.Route("/api/v1", func(r chi.Router) {
			// domain routes are mounted here in later steps
		})
	})
	r.NotFound(httpx.H(func(http.ResponseWriter, *http.Request) error { return httpx.ErrNotFound }))
	r.MethodNotAllowed(httpx.H(func(http.ResponseWriter, *http.Request) error {
		return httpx.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "bu yöntem desteklenmiyor")
	}))
	return r
}

// health reports liveness plus database connectivity; it returns 503 when
// the database does not answer within two seconds.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	var one int
	err := s.pool.QueryRow(ctx, "SELECT 1").Scan(&one)
	dbStatus := map[string]any{"status": "up", "latency_ms": time.Since(start).Milliseconds()}
	status, code := "ok", http.StatusOK
	if err != nil {
		dbStatus = map[string]any{"status": "down", "error": "database unreachable"}
		status, code = "degraded", http.StatusServiceUnavailable
	}
	httpx.JSON(w, code, map[string]any{
		"status": status,
		"checks": map[string]any{"database": dbStatus},
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

// RunBackground starts periodic workers; it returns when ctx is cancelled.
func (s *Server) RunBackground(ctx context.Context) { <-ctx.Done() }

// Shutdown releases background resources before the HTTP server stops.
func (s *Server) Shutdown() {}

// Seed loads demo data (filled in step 2).
func Seed(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	log.Info("seed: nothing to do yet")
	return nil
}
