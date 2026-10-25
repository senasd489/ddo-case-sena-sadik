// Package idempotency implements the Idempotency-Key header for state
// changing POST requests: the first response for a (user, key) pair is stored
// and replayed verbatim for every retry with the same key and body.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

const Header = "Idempotency-Key"

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }
func (r *recorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}

// Middleware stores and replays responses. When required is true, a missing
// header is rejected with 400 (used for the most critical endpoints).
func Middleware(pool *pgxpool.Pool, required bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(Header)
			if r.Method != http.MethodPost || key == "" {
				if required && r.Method == http.MethodPost {
					httpx.WriteError(w, r, httpx.BadRequest("bu işlem için Idempotency-Key başlığı zorunlu"))
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if len(key) < 8 || len(key) > 128 {
				httpx.WriteError(w, r, httpx.BadRequest("Idempotency-Key 8-128 karakter olmalı"))
				return
			}
			p := auth.FromContext(r.Context())
			if p == nil {
				httpx.WriteError(w, r, httpx.ErrUnauthorized)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				httpx.WriteError(w, r, httpx.BadRequest("gövde okunamadı"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
			ctx := r.Context()

			tag, err := pool.Exec(ctx, `INSERT INTO idempotency_keys (user_id, key, method, path, request_hash)
				VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, p.UserID, key, r.Method, r.URL.Path, sum[:])
			if err != nil {
				httpx.WriteError(w, r, err)
				return
			}
			if tag.RowsAffected() == 0 {
				replay(ctx, pool, w, r, p.UserID, key, sum[:])
				return
			}

			rec := &recorder{header: http.Header{}}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			// 5xx: kalıcı sonuç değil; anahtarı sil ki istemci tekrar deneyebilsin.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if rec.status >= 500 {
				_, _ = pool.Exec(sctx, `DELETE FROM idempotency_keys WHERE user_id=$1 AND key=$2`, p.UserID, key)
			} else {
				_, _ = pool.Exec(sctx, `UPDATE idempotency_keys SET status_code=$3, response_body=$4, completed_at=now()
					WHERE user_id=$1 AND key=$2`, p.UserID, key, rec.status, rec.body.Bytes())
			}
			for k, v := range rec.header {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.status)
			_, _ = w.Write(rec.body.Bytes())
		})
	}
}

func replay(ctx context.Context, pool *pgxpool.Pool, w http.ResponseWriter, r *http.Request, userID int64, key string, hash []byte) {
	var (
		storedHash []byte
		status     *int
		body       []byte
	)
	err := pool.QueryRow(ctx, `SELECT request_hash, status_code, response_body FROM idempotency_keys
		WHERE user_id=$1 AND key=$2`, userID, key).Scan(&storedHash, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		// İlk istek 5xx ile bitip anahtar silinmiş: istemci yeniden denemeli.
		httpx.WriteError(w, r, httpx.Conflict("idempotency_retry", "önceki istek tamamlanamadı, tekrar deneyin"))
		return
	}
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if !bytes.Equal(storedHash, hash) {
		httpx.WriteError(w, r, httpx.NewError(http.StatusUnprocessableEntity, "idempotency_key_mismatch",
			"bu Idempotency-Key farklı bir istek için kullanılmış"))
		return
	}
	if status == nil {
		httpx.WriteError(w, r, httpx.Conflict("idempotency_in_progress", "aynı anahtarlı istek hâlâ işleniyor"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(*status)
	_, _ = w.Write(body)
}
