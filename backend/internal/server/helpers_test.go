package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/config"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/seed"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.NewDB(t)
	cfg := config.Config{JWTSecret: "test-secret-test-secret-test-secret!", AccessTTL: time.Minute,
		RefreshTTL: time.Hour, RequestTimeout: 10 * time.Second, CORSOrigin: "*"}
	s := New(cfg, pool, testutil.Logger())
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	return &env{t: t, pool: pool, srv: srv}
}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		t.Fatalf("decode %s: %v", r.Body, err)
	}
}

func (r resp) errCode(t *testing.T) string {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	r.json(t, &e)
	return e.Error.Code
}

func (e *env) do(method, path, token string, body any, hdr ...string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (e *env) login(email string) tokens {
	e.t.Helper()
	r := e.do("POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": seed.DemoPassword})
	if r.Status != 200 {
		e.t.Fatalf("login %s: %d %s", email, r.Status, r.Body)
	}
	var tk tokens
	r.json(e.t, &tk)
	return tk
}
