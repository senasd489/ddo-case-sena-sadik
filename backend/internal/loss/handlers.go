package loss

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type Handlers struct{ pool *pgxpool.Pool }

func NewHandlers(pool *pgxpool.Pool) *Handlers { return &Handlers{pool: pool} }

func (h *Handlers) Mount(r chi.Router) {
	analysts := auth.Require(auth.RoleAnalyst, auth.RoleAdmin)
	r.With(analysts).Post("/analytics/loss/run", httpx.H(h.run))
	r.With(auth.Require(auth.RoleAnalyst, auth.RoleOperator, auth.RoleAdmin)).Get("/analytics/loss/transformers", httpx.H(h.transformers))
	r.With(analysts).Get("/analytics/suspects", httpx.H(h.suspects))
	r.With(analysts).Get("/analytics/suspects/{id}", httpx.H(h.suspect))
	r.With(analysts).Post("/analytics/suspects/{id}/review", httpx.H(h.review))
}

type runReq struct {
	AsOf    string `json:"as_of"` // YYYY-MM-DD, varsayılan bugün
	Filters *bool  `json:"filters"`
}

func (h *Handlers) run(w http.ResponseWriter, r *http.Request) error {
	var req runReq
	if r.ContentLength > 0 {
		if err := httpx.Decode(r, &req); err != nil {
			return err
		}
	}
	o := Options{AsOf: time.Now().In(httpx.Istanbul), Filters: true}
	if req.AsOf != "" {
		t, err := time.Parse("2006-01-02", req.AsOf)
		if err != nil {
			return httpx.Validation(map[string]any{"as_of": "YYYY-MM-DD"})
		}
		o.AsOf = t
	}
	if req.Filters != nil {
		o.Filters = *req.Filters
	}
	sum, err := Run(r.Context(), h.pool, o)
	if err != nil {
		return err
	}
	if len(sum.Flags) > 100 {
		sum.Flags = sum.Flags[:100]
	}
	httpx.JSON(w, http.StatusOK, sum)
	return nil
}

type trRow struct {
	ID           int64   `json:"id"`
	Code         string  `json:"code"`
	Days         int64   `json:"days"`
	HeadWh       int64   `json:"head_wh"`
	SubsWh       int64   `json:"subscribers_wh"`
	LossWh       int64   `json:"loss_wh"`
	ExcessWh     int64   `json:"excess_wh"`
	LossPct      float64 `json:"loss_pct"`
	TolerancePct float64 `json:"tolerance_pct"`
	ExcessDays   int64   `json:"excess_days"`
	OpenFlags    int64   `json:"open_flags"`
}

// transformers: F-7 kayıp raporu — dönem içindeki trafo dengesi ve açık işaretler.
func (h *Handlers) transformers(w http.ResponseWriter, r *http.Request) error {
	from, to, err := httpx.ParseRange(r, 30*24*time.Hour, 400*24*time.Hour)
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT n.id, n.code, count(lw.*), coalesce(sum(lw.head_wh),0)::bigint, coalesce(sum(lw.subscribers_wh),0)::bigint,
			coalesce(sum(lw.loss_wh),0)::bigint, coalesce(sum(lw.excess_wh),0)::bigint,
			CASE WHEN sum(lw.head_wh) > 0 THEN round(100.0 * sum(lw.loss_wh) / sum(lw.head_wh), 2) ELSE 0 END::float8,
			n.tech_loss_pct::float8, count(*) FILTER (WHERE lw.excess_wh > 0),
			(SELECT count(*) FROM suspicion_flags f WHERE f.node_id = n.id AND f.status = 'open')
		FROM network_nodes n
		LEFT JOIN loss_windows lw ON lw.node_id = n.id AND lw.window_start >= $1 AND lw.window_start < $2
		WHERE n.kind = 'transformer'
		GROUP BY n.id ORDER BY 8 DESC, n.id`, from, to)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[trRow])
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "items": items})
	return nil
}

type suspectRow struct {
	ID           int64          `json:"id"`
	SubscriberID int64          `json:"subscriber_id"`
	SubscriberNo string         `json:"subscriber_no"`
	FullName     string         `json:"full_name"`
	NodeID       *int64         `json:"transformer_id"`
	Rule         string         `json:"rule"`
	Score        float64        `json:"score"`
	WindowStart  time.Time      `json:"window_start"`
	WindowEnd    time.Time      `json:"window_end"`
	Status       string         `json:"status"`
	Explanation  map[string]any `json:"explanation"`
	CreatedAt    time.Time      `json:"created_at"`
}

const suspectCols = `f.id, f.subscriber_id, s.subscriber_no, s.full_name, f.node_id, f.rule, f.score::float8, f.window_start, f.window_end,
	f.status, f.explanation, f.created_at`

// suspects: keyset pagination on (score DESC, id) via suspicion_flags_open_idx.
func (h *Handlers) suspects(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "open"
	}
	score, lastID := 1e9, int64(0)
	if len(pg.Cursor) == 2 {
		if score, err = strconv.ParseFloat(pg.Cursor[0], 64); err != nil {
			return httpx.BadRequest("geçersiz cursor")
		}
		if lastID, err = pg.CursorInt64(1); err != nil {
			return err
		}
	}
	rows, err := h.pool.Query(r.Context(), `SELECT `+suspectCols+` FROM suspicion_flags f JOIN subscribers s ON s.id = f.subscriber_id
		WHERE f.status = $1 AND ($2 = '' OR f.rule = $2) AND (f.score < $3 OR (f.score = $3 AND f.id > $4))
		ORDER BY f.score DESC, f.id LIMIT $5`, status, q.Get("rule"), score, lastID, pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[suspectRow])
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(s suspectRow) string {
		return httpx.EncodeCursor(fmt.Sprint(s.Score), strconv.FormatInt(s.ID, 10))
	}))
	return nil
}

func (h *Handlers) suspect(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT `+suspectCols+` FROM suspicion_flags f JOIN subscribers s ON s.id = f.subscriber_id WHERE f.id = $1`, id)
	if err != nil {
		return err
	}
	item, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[suspectRow])
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.ErrNotFound
	}
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, item)
	return nil
}

type reviewReq struct {
	Status string `json:"status"` // confirmed | dismissed
	Note   string `json:"note"`
}

func (h *Handlers) review(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	var req reviewReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.Status != "confirmed" && req.Status != "dismissed" {
		return httpx.Validation(map[string]any{"status": "confirmed veya dismissed"})
	}
	p := auth.FromContext(r.Context())
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var old string
		if err := tx.QueryRow(r.Context(), `SELECT status FROM suspicion_flags WHERE id = $1 FOR UPDATE`, id).Scan(&old); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.ErrNotFound
			}
			return err
		}
		if old != "open" {
			return httpx.Conflict("already_reviewed", "bu işaret zaten incelendi ("+old+")")
		}
		if _, err := tx.Exec(r.Context(), `UPDATE suspicion_flags SET status = $2, reviewed_by = $3, reviewed_at = now() WHERE id = $1`,
			id, req.Status, p.UserID); err != nil {
			return err
		}
		return audit.Record(r.Context(), tx, audit.Entry{Action: "suspect.review", Entity: "suspicion_flag", EntityID: strconv.FormatInt(id, 10),
			Old: map[string]any{"status": old}, New: map[string]any{"status": req.Status, "note": req.Note}})
	})
	if err != nil {
		return err
	}
	return h.suspect(w, r)
}
