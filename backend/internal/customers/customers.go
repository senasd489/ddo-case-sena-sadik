// Package customers implements F-1: subscriber and meter registration.
package customers

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

type Subscriber struct {
	ID           int64     `json:"id"`
	SubscriberNo string    `json:"subscriber_no"`
	FullName     string    `json:"full_name"`
	Address      string    `json:"address"`
	Category     string    `json:"category"`
	PoleID       int64     `json:"pole_id"`
	TariffID     *int64    `json:"tariff_id"`
	TariffCode   *string   `json:"tariff_code,omitempty"`
	ContractKW   float64   `json:"contract_kw"`
	Lat          *float64  `json:"lat"`
	Lon          *float64  `json:"lon"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"created_at"`
}

type MeterInfo struct {
	ID             int64      `json:"id"`
	SerialNo       string     `json:"serial_no"`
	InstalledAt    time.Time  `json:"installed_at"`
	RemovedAt      *time.Time `json:"removed_at,omitempty"`
	InitialIndexWh int64      `json:"initial_index_wh"`
	FinalIndexWh   *int64     `json:"final_index_wh,omitempty"`
	LastReadAt     *time.Time `json:"last_read_at"`
	LastIndexWh    *int64     `json:"last_index_wh"`
}

type PathNode struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Handlers struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Handlers { return &Handlers{pool: pool} }

func (h *Handlers) Mount(r chi.Router) {
	staff := auth.Require(auth.Staff...)
	r.With(staff).Get("/subscribers", httpx.H(h.list))
	r.With(auth.Require(append([]auth.Role{auth.RoleSubscriber}, auth.Staff...)...)).Get("/subscribers/{id}", httpx.H(h.get))
	r.With(auth.Require(auth.RoleBilling, auth.RoleAdmin)).Post("/subscribers", httpx.H(h.create))
	r.With(auth.Require(auth.Staff...)).Get("/tariffs", httpx.H(h.tariffs))
}

const subCols = `s.id, s.subscriber_no, s.full_name, s.address, s.category, s.pole_id, s.tariff_id, t.code,
	s.contract_kw::float8, s.lat, s.lon, s.active, s.created_at`

func scanSub(row pgx.Row) (Subscriber, error) {
	var s Subscriber
	err := row.Scan(&s.ID, &s.SubscriberNo, &s.FullName, &s.Address, &s.Category, &s.PoleID, &s.TariffID, &s.TariffCode,
		&s.ContractKW, &s.Lat, &s.Lon, &s.Active, &s.CreatedAt)
	return s, err
}

// list: keyset pagination by id with optional ?q= (abone no / ad) and ?category=.
func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		return err
	}
	after, err := pg.CursorInt64(0)
	if err != nil {
		return err
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cat := r.URL.Query().Get("category")
	rows, err := h.pool.Query(r.Context(), `SELECT `+subCols+`
		FROM subscribers s LEFT JOIN tariffs t ON t.id = s.tariff_id
		WHERE s.id > $1
		  AND ($2 = '' OR s.subscriber_no = upper($2) OR s.full_name ILIKE '%' || $2 || '%')
		  AND ($3 = '' OR s.category = $3)
		ORDER BY s.id LIMIT $4`, after, q, cat, pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Subscriber, error) { return scanSub(row) })
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(s Subscriber) string {
		return httpx.EncodeCursor(strconv.FormatInt(s.ID, 10))
	}))
	return nil
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	if !auth.FromContext(r.Context()).CanAccessSubscriber(id) {
		return httpx.ErrForbidden
	}
	ctx := r.Context()
	s, err := scanSub(h.pool.QueryRow(ctx, `SELECT `+subCols+` FROM subscribers s LEFT JOIN tariffs t ON t.id = s.tariff_id WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.ErrNotFound
	}
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(ctx, `SELECT m.id, m.serial_no, m.installed_at, m.removed_at, m.initial_index_wh, m.final_index_wh,
			lr.read_at, lr.active_index_wh
		FROM meters m
		LEFT JOIN LATERAL (SELECT read_at, active_index_wh FROM readings r WHERE r.meter_id = m.id
		                   ORDER BY read_at DESC LIMIT 1) lr ON true
		WHERE m.subscriber_id = $1 ORDER BY m.installed_at`, id)
	if err != nil {
		return err
	}
	meters, err := pgx.CollectRows(rows, pgx.RowToStructByPos[MeterInfo])
	if err != nil {
		return err
	}
	// Abonenin besleme yolu: direk → … → besleyici → trafo (recursive CTE).
	rows, err = h.pool.Query(ctx, `WITH RECURSIVE up AS (
			SELECT n.id, n.code, n.kind, n.name, n.parent_id, 0 AS depth FROM network_nodes n WHERE n.id = $1
			UNION ALL
			SELECT n.id, n.code, n.kind, n.name, n.parent_id, up.depth + 1 FROM network_nodes n JOIN up ON n.id = up.parent_id
		) SELECT id, code, kind, name FROM up ORDER BY depth DESC`, s.PoleID)
	if err != nil {
		return err
	}
	path, err := pgx.CollectRows(rows, pgx.RowToStructByPos[PathNode])
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"subscriber": s, "meters": meters, "supply_path": path})
	return nil
}

type createReq struct {
	FullName    string     `json:"full_name"`
	Address     string     `json:"address"`
	Category    string     `json:"category"`
	PoleID      int64      `json:"pole_id"`
	TariffID    int64      `json:"tariff_id"`
	ContractKW  float64    `json:"contract_kw"`
	Lat         *float64   `json:"lat"`
	Lon         *float64   `json:"lon"`
	MeterSerial string     `json:"meter_serial_no"`
	InstalledAt *time.Time `json:"installed_at"`
}

func (req *createReq) validate() *httpx.Error {
	d := map[string]any{}
	if strings.TrimSpace(req.FullName) == "" {
		d["full_name"] = "gerekli"
	}
	if strings.TrimSpace(req.Address) == "" {
		d["address"] = "gerekli"
	}
	switch req.Category {
	case "residential", "commercial", "industrial", "agricultural":
	default:
		d["category"] = "residential | commercial | industrial | agricultural"
	}
	if req.PoleID <= 0 {
		d["pole_id"] = "gerekli"
	}
	if req.TariffID <= 0 {
		d["tariff_id"] = "gerekli"
	}
	if req.ContractKW <= 0 || req.ContractKW > 100000 {
		d["contract_kw"] = "0'dan büyük olmalı"
	}
	if strings.TrimSpace(req.MeterSerial) == "" {
		d["meter_serial_no"] = "gerekli"
	}
	if len(d) > 0 {
		return httpx.Validation(d)
	}
	return nil
}

// create registers a subscriber and its first meter atomically.
func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	var req createReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if e := req.validate(); e != nil {
		return e
	}
	installed := time.Now().Truncate(15 * time.Minute)
	if req.InstalledAt != nil {
		installed = req.InstalledAt.Truncate(15 * time.Minute)
	}
	var s Subscriber
	var meterID int64
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		var tariffCat string
		if err := tx.QueryRow(ctx, `SELECT category FROM tariffs WHERE id=$1`, req.TariffID).Scan(&tariffCat); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return httpx.Validation(map[string]any{"tariff_id": "tarife bulunamadı"})
			}
			return err
		}
		if tariffCat != req.Category {
			return httpx.Validation(map[string]any{"tariff_id": "tarife kategorisi abone kategorisiyle uyuşmuyor"})
		}
		var id int64
		if err := tx.QueryRow(ctx, `WITH n AS (SELECT nextval('subscribers_id_seq') AS id)
			INSERT INTO subscribers (id, subscriber_no, full_name, address, category, pole_id, tariff_id, contract_kw, lat, lon)
			SELECT n.id, 'AB' || lpad(n.id::text, 8, '0'), $1,$2,$3,$4,$5,$6,$7,$8 FROM n RETURNING id`,
			req.FullName, req.Address, req.Category, req.PoleID, req.TariffID, req.ContractKW, req.Lat, req.Lon).Scan(&id); err != nil {
			return err
		}
		var err error
		s, err = scanSub(tx.QueryRow(ctx, `SELECT `+subCols+` FROM subscribers s LEFT JOIN tariffs t ON t.id = s.tariff_id WHERE s.id = $1`, id))
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO meters (serial_no, kind, subscriber_id, installed_at) VALUES ($1,'subscriber',$2,$3) RETURNING id`,
			req.MeterSerial, id, installed).Scan(&meterID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Action: "subscriber.create", Entity: "subscriber",
			EntityID: strconv.FormatInt(id, 10), New: map[string]any{"subscriber": s, "meter_id": meterID, "meter_serial_no": req.MeterSerial}})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return httpx.Conflict("duplicate", "sayaç seri numarası zaten kayıtlı")
		case "23503":
			return httpx.Validation(map[string]any{"pole_id": "direk bulunamadı (abone yalnızca bir direğe bağlanabilir)"})
		}
	}
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"subscriber": s, "meter_id": meterID})
	return nil
}

type tariffOut struct {
	ID       int64  `json:"id"`
	Code     string `json:"code"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

func (h *Handlers) tariffs(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.pool.Query(r.Context(), `SELECT id, code, name, category FROM tariffs ORDER BY id`)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[tariffOut])
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}
