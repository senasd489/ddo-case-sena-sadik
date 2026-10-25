// Package testutil provides throw-away PostgreSQL databases for integration
// tests. Set TEST_DATABASE_URL to a server where the user may CREATE DATABASE;
// without it, DB tests are skipped (unit tests still run).
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
)

var (
	tmplOnce sync.Once
	tmplName string
	tmplErr  error
)

func Logger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func baseURL(t testing.TB) *url.URL {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL: %v", err)
	}
	return u
}

func withDB(u *url.URL, name string) string {
	c := *u
	c.Path = "/" + name
	return c.String()
}

func randName(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// template builds (once per test binary) a migrated + seeded database that
// every test clones with CREATE DATABASE … TEMPLATE, which takes ~50 ms.
func template(t testing.TB, u *url.URL) string {
	tmplOnce.Do(func() {
		ctx := context.Background()
		name := randName("tmpl_")
		admin, err := pgx.Connect(ctx, u.String())
		if err != nil {
			tmplErr = err
			return
		}
		defer admin.Close(ctx)
		if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			tmplErr = err
			return
		}
		pool, err := pgxpool.New(ctx, withDB(u, name))
		if err != nil {
			tmplErr = err
			return
		}
		if err = db.Migrate(ctx, pool, Logger()); err == nil {
			err = seed.Run(ctx, pool, Logger(), seed.Options{Seed: 42, Subscribers: 600})
		}
		pool.Close()
		tmplErr = err
		tmplName = name
	})
	if tmplErr != nil {
		t.Fatalf("template db: %v", tmplErr)
	}
	return tmplName
}

// NewDB returns a pool on a fresh, migrated and seeded database (600
// subscribers, 3 transformers) that is dropped when the test ends.
func NewDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	u := baseURL(t)
	tmpl := template(t, u)
	ctx := context.Background()
	name := randName("t_")
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tmpl)); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, withDB(u, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), u.String())
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return pool
}

// EmptyDB returns a fresh database with no schema (for migration/seed tests).
func EmptyDB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	u := baseURL(t)
	ctx := context.Background()
	name := randName("e_")
	admin, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, withDB(u, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, err := pgx.Connect(context.Background(), u.String())
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	return pool
}
