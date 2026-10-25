// Package users exposes user administration for the admin role.
package users

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	FullName     string    `json:"full_name"`
	Role         auth.Role `json:"role"`
	SubscriberID *int64    `json:"subscriber_id,omitempty"`
	CrewID       *int64    `json:"crew_id,omitempty"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
}

type Handlers struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Handlers { return &Handlers{pool: pool} }

func (h *Handlers) Mount(r chi.Router) {
	r.With(auth.Require(auth.RoleAdmin)).Get("/admin/users", httpx.H(h.list))
	r.With(auth.Require(auth.RoleAdmin)).Post("/admin/users", httpx.H(h.create))
	r.With(auth.Require(auth.RoleAdmin)).Get("/admin/audit", httpx.H(h.auditLog))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		return err
	}
	after, err := pg.CursorInt64(0)
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT id, email, full_name, role, subscriber_id, crew_id, disabled, created_at
		FROM users WHERE id > $1 ORDER BY id LIMIT $2`, after, pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (User, error) {
		var u User
		err := row.Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.SubscriberID, &u.CrewID, &u.Disabled, &u.CreatedAt)
		return u, err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(u User) string {
		return httpx.EncodeCursor(strconv.FormatInt(u.ID, 10))
	}))
	return nil
}

type createReq struct {
	Email        string    `json:"email"`
	FullName     string    `json:"full_name"`
	Password     string    `json:"password"`
	Role         auth.Role `json:"role"`
	SubscriberID *int64    `json:"subscriber_id"`
	CrewID       *int64    `json:"crew_id"`
}

func (req *createReq) validate() *httpx.Error {
	d := map[string]any{}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !strings.Contains(req.Email, "@") || len(req.Email) > 254 {
		d["email"] = "geçerli bir e-posta girin"
	}
	if strings.TrimSpace(req.FullName) == "" {
		d["full_name"] = "gerekli"
	}
	if len(req.Password) < 8 {
		d["password"] = "en az 8 karakter"
	}
	if !req.Role.Valid() {
		d["role"] = "geçersiz rol"
	}
	if req.Role == auth.RoleSubscriber && req.SubscriberID == nil {
		d["subscriber_id"] = "abone rolü için gerekli"
	}
	if req.Role != auth.RoleSubscriber && req.SubscriberID != nil {
		d["subscriber_id"] = "yalnızca abone rolü için"
	}
	if len(d) > 0 {
		return httpx.Validation(d)
	}
	return nil
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	var req createReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if e := req.validate(); e != nil {
		return e
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return err
	}
	var u User
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `INSERT INTO users (email, password_hash, full_name, role, subscriber_id, crew_id)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id, email, full_name, role, subscriber_id, crew_id, disabled, created_at`,
			req.Email, hash, req.FullName, string(req.Role), req.SubscriberID, req.CrewID).
			Scan(&u.ID, &u.Email, &u.FullName, &u.Role, &u.SubscriberID, &u.CrewID, &u.Disabled, &u.CreatedAt)
		if err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Entry{Action: "user.create", Entity: "user",
			EntityID: strconv.FormatInt(u.ID, 10), New: u})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return httpx.Conflict("email_taken", "bu e-posta zaten kayıtlı")
		case "23503":
			return httpx.Validation(map[string]any{"subscriber_id": "abone/ekip bulunamadı"})
		}
	}
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, u)
	return nil
}

type auditRow struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	ActorID   *int64    `json:"actor_id"`
	ActorRole *string   `json:"actor_role"`
	Action    string    `json:"action"`
	Entity    string    `json:"entity"`
	EntityID  *string   `json:"entity_id"`
	Old       any       `json:"old_value"`
	New       any       `json:"new_value"`
}

// auditLog lists audit entries newest-first with keyset pagination.
func (h *Handlers) auditLog(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		return err
	}
	before, err := pg.CursorInt64(0)
	if err != nil {
		return err
	}
	if before == 0 {
		before = 1<<63 - 1
	}
	entity := r.URL.Query().Get("entity")
	rows, err := h.pool.Query(r.Context(), `SELECT id, at, actor_id, actor_role, action, entity, entity_id, old_value, new_value
		FROM audit_log WHERE id < $1 AND ($2 = '' OR entity = $2) ORDER BY id DESC LIMIT $3`, before, entity, pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (auditRow, error) {
		var a auditRow
		var oldV, newV map[string]any
		err := row.Scan(&a.ID, &a.At, &a.ActorID, &a.ActorRole, &a.Action, &a.Entity, &a.EntityID, &oldV, &newV)
		a.Old, a.New = oldV, newV
		return a, err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(a auditRow) string {
		return httpx.EncodeCursor(strconv.FormatInt(a.ID, 10))
	}))
	return nil
}
