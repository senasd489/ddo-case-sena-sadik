// Command api is the EnerjiHat HTTP API.
//
//	api            run the HTTP server (default)
//	api migrate    apply database migrations and exit
//	api seed       apply migrations, load demo seed data and exit
//	api bench-t1 [n]  T-1 ölçümü: n sayaç × 1 gün okuma (varsayılan ≈10 milyon)
//	api bench-t4 [n]  T-4 ölçümü: yerleştirilmiş anomalilerle precision/recall, filtreli vs filtresiz
//	api reset      drop everything, re-migrate and re-seed (make reset-demo)
//	api healthcheck  probe GET /health (used by the Docker HEALTHCHECK)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/bench"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/config"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	log := newLogger(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "migrate":
		return db.Migrate(ctx, pool, log)
	case "seed":
		if err := db.Migrate(ctx, pool, log); err != nil {
			return err
		}
		return server.Seed(ctx, pool, log, seedOptions(), anchor())
	case "reset":
		// Demo verisini ilk haline döndürür: şemayı silip yeniden kurar.
		if err := db.Reset(ctx, pool, log); err != nil {
			return err
		}
		return server.Seed(ctx, pool, log, seedOptions(), anchor())
	case "bench-t1":
		meters := 0
		if len(os.Args) > 2 {
			meters, _ = strconv.Atoi(os.Args[2])
		}
		return bench.RunT1(ctx, cfg.DatabaseURL, bench.T1Options{Meters: meters, Keep: os.Getenv("BENCH_KEEP") == "1"}, os.Stdout)
	case "bench-t4":
		n := 0
		if len(os.Args) > 2 {
			n, _ = strconv.Atoi(os.Args[2])
		}
		return bench.RunT4(ctx, cfg.DatabaseURL, n, os.Stdout)
	case "", "serve":
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}

	srv := server.New(cfg, pool, log)
	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // SSE stream; per-request deadlines come from the Timeout middleware
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	go srv.RunBackground(ctx)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutdown started")
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	srv.Shutdown()
	if err := httpSrv.Shutdown(sctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("shutdown complete")
	return nil
}

func seedOptions() seed.Options {
	n, _ := strconv.Atoi(os.Getenv("SEED_SUBSCRIBERS"))
	s, _ := strconv.ParseUint(os.Getenv("SIM_SEED"), 10, 64)
	if s == 0 {
		s = 42
	}
	return seed.Options{Seed: s, Subscribers: n}
}

// anchor is the simulation's "today": SIM_ANCHOR (YYYY-MM-DD) or the local date.
func anchor() time.Time {
	if v := os.Getenv("SIM_ANCHOR"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			return t
		}
	}
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		loc = time.FixedZone("TRT", 3*3600)
	}
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})).With("service", "api")
}

func healthcheck() int {
	addr := os.Getenv("HEALTHCHECK_URL")
	if addr == "" {
		addr = "http://127.0.0.1:8080/health"
	}
	c := http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, addr, nil)
	if err != nil {
		return 1
	}
	resp, err := c.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
