package metering

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type Handlers struct {
	pool        *pgxpool.Pool
	signer      *auth.Signer
	ingestToken string
}

func NewHandlers(pool *pgxpool.Pool, signer *auth.Signer, ingestToken string) *Handlers {
	return &Handlers{pool: pool, signer: signer, ingestToken: ingestToken}
}

// MountIngest registers machine-to-machine routes: they accept either the
// X-Ingest-Token shared secret (simulator / head-end system) or a JWT of a
// field crew member or admin.
func (h *Handlers) MountIngest(r chi.Router) {
	r.With(h.ingestAuth).Post("/readings/batch", httpx.H(h.batch))
	r.With(h.ingestAuth).Get("/ingest/meters", httpx.H(h.activeMeters))
}

// Mount registers user-facing routes (behind auth.Authenticate).
func (h *Handlers) Mount(r chi.Router) {
	r.With(auth.Require(auth.RoleFieldCrew, auth.RoleAdmin)).Post("/readings", httpx.H(h.single))
	r.With(auth.Require(auth.RoleFieldCrew, auth.RoleOperator, auth.RoleAnalyst, auth.RoleBilling, auth.RoleAdmin)).
		Get("/meters/{id}/readings", httpx.H(h.meterReadings))
	r.With(auth.Require(auth.RoleSubscriber, auth.RoleFieldCrew, auth.RoleOperator, auth.RoleAnalyst, auth.RoleBilling, auth.RoleAdmin)).
		Get("/subscribers/{id}/consumption", httpx.H(h.consumption))
	r.With(auth.Require(auth.RoleFieldCrew, auth.RoleAdmin)).Post("/subscribers/{id}/meter-change", httpx.H(h.meterChange))
	r.With(auth.Require(auth.RoleOperator, auth.RoleAnalyst, auth.RoleBilling, auth.RoleAdmin)).Get("/readings/stats", httpx.H(h.stats))
	r.With(auth.Require(auth.RoleAdmin)).Post("/admin/consumption/recompute", httpx.H(h.recompute))
}

func (h *Handlers) ingestAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := r.Header.Get("X-Ingest-Token"); tok != "" {
			if h.ingestToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(h.ingestToken)) != 1 {
				httpx.WriteError(w, r, httpx.NewError(http.StatusUnauthorized, "invalid_ingest_token", "geçersiz ingest token"))
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.System)))
			return
		}
		auth.Authenticate(h.signer)(auth.Require(auth.RoleFieldCrew, auth.RoleAdmin)(next)).ServeHTTP(w, r)
	})
}

type batchReq struct {
	Readings []Reading `json:"readings"`
}

func (h *Handlers) batch(w http.ResponseWriter, r *http.Request) error {
	var req batchReq
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := httpxDecodeLarge(r, &req); err != nil {
		return err
	}
	if len(req.Readings) == 0 || len(req.Readings) > MaxBatch {
		return httpx.Validation(map[string]any{"readings": "1-" + strconv.Itoa(MaxBatch) + " okuma gönderilmeli"})
	}
	src := "api"
	if p := auth.FromContext(r.Context()); p == auth.System {
		src = "head-end"
	}
	res, err := Ingest(r.Context(), h.pool, src, req.Readings)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusAccepted, res)
	return nil
}

// single: a field crew member enters one reading manually.
func (h *Handlers) single(w http.ResponseWriter, r *http.Request) error {
	var rd Reading
	if err := httpx.Decode(r, &rd); err != nil {
		return err
	}
	if reason := Validate(rd, time.Now()); reason != "" {
		return httpx.Validation(map[string]any{"reading": reason})
	}
	res, err := Ingest(r.Context(), h.pool, "manual", []Reading{rd})
	if err != nil {
		return err
	}
	if res.Rejected > 0 {
		return httpx.Validation(map[string]any{"meter_id": "sayaç bulunamadı veya bu tarihte kurulu değil"})
	}
	httpx.JSON(w, http.StatusCreated, res)
	return nil
}

type activeMeter struct {
	ID           int64     `json:"id"`
	Kind         string    `json:"kind"`
	SubscriberID *int64    `json:"subscriber_id,omitempty"`
	NodeID       *int64    `json:"node_id,omitempty"`
	InstalledAt  time.Time `json:"installed_at"`
}

// activeMeters lists active meters (keyset) so the simulator knows what to emit.
func (h *Handlers) activeMeters(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 5000, 50000)
	if err != nil {
		return err
	}
	after, err := pg.CursorInt64(0)
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT id, kind, subscriber_id, node_id, installed_at FROM meters
		WHERE removed_at IS NULL AND id > $1 ORDER BY id LIMIT $2`, after, pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (activeMeter, error) {
		var m activeMeter
		err := row.Scan(&m.ID, &m.Kind, &m.SubscriberID, &m.NodeID, &m.InstalledAt)
		return m, err
	})
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(m activeMeter) string {
		return httpx.EncodeCursor(strconv.FormatInt(m.ID, 10))
	}))
	return nil
}

type readingRow struct {
	ReadAt       time.Time `json:"read_at"`
	ActiveWh     int64     `json:"active_index_wh"`
	ReactiveVarh int64     `json:"reactive_index_varh"`
	Revision     int16     `json:"revision"`
	ReceivedAt   time.Time `json:"received_at"`
}

func (h *Handlers) meterReadings(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	from, to, err := httpx.ParseRange(r, 24*time.Hour, 31*24*time.Hour)
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT read_at, active_index_wh, reactive_index_varh, revision, received_at
		FROM readings WHERE meter_id = $1 AND read_at >= $2 AND read_at < $3 ORDER BY read_at`, id, from, to)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[readingRow])
	if err != nil {
		return err
	}
	if items == nil {
		items = []readingRow{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"meter_id": id, "from": from, "to": to, "items": items})
	return nil
}

type slotOut struct {
	SlotStart time.Time `json:"slot_start"`
	MeterID   int64     `json:"meter_id"`
	ActiveWh  int64     `json:"active_wh"`
	Reactive  int64     `json:"reactive_varh"`
	DemandW   int64     `json:"demand_w"`
	Quality   string    `json:"quality"`
}

type dayOut struct {
	Day          string `json:"day"`
	ActiveWh     int64  `json:"active_wh"`
	DayWh        int64  `json:"day_wh"`
	PeakWh       int64  `json:"peak_wh"`
	NightWh      int64  `json:"night_wh"`
	ReactiveVarh int64  `json:"reactive_varh"`
	MaxDemandW   int64  `json:"max_demand_w"`
	Slots        int64  `json:"slots"`
	Interpolated int64  `json:"interpolated_slots"`
	MissingSlots int64  `json:"missing_slots"`
}

// consumption returns ?granularity=15m (slots with quality flags) or day.
func (h *Handlers) consumption(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	if !auth.FromContext(r.Context()).CanAccessSubscriber(id) {
		return httpx.ErrForbidden
	}
	gran := r.URL.Query().Get("granularity")
	if gran == "" {
		gran = "day"
	}
	switch gran {
	case "15m":
		from, to, err := httpx.ParseRange(r, 24*time.Hour, 8*24*time.Hour)
		if err != nil {
			return err
		}
		rows, err := h.pool.Query(r.Context(), `SELECT slot_start, meter_id, active_wh, reactive_varh, active_wh * 4, quality
			FROM consumption_15m WHERE subscriber_id = $1 AND slot_start >= $2 AND slot_start < $3
			ORDER BY slot_start, meter_id`, id, from, to)
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[slotOut])
		if err != nil {
			return err
		}
		if items == nil {
			items = []slotOut{}
		}
		q := map[string]int{}
		var total int64
		for _, s := range items {
			q[s.Quality]++
			total += s.ActiveWh
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "granularity": gran, "from": from, "to": to,
			"total_wh": total, "quality_counts": q, "items": items})
	case "day":
		from, to, err := httpx.ParseRange(r, 30*24*time.Hour, 400*24*time.Hour)
		if err != nil {
			return err
		}
		rows, err := h.pool.Query(r.Context(), `SELECT to_char(day,'YYYY-MM-DD'), sum(active_wh), sum(day_wh), sum(peak_wh), sum(night_wh),
				sum(reactive_varh), max(max_demand_w), sum(slots), sum(interpolated_slots), greatest(96 - sum(slots), 0)
			FROM consumption_daily WHERE subscriber_id = $1 AND day >= $2::date AND day < $3::date
			GROUP BY day ORDER BY day`, id, from.In(httpx.Istanbul).Format("2006-01-02"), to.In(httpx.Istanbul).Format("2006-01-02"))
		if err != nil {
			return err
		}
		items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dayOut])
		if err != nil {
			return err
		}
		if items == nil {
			items = []dayOut{}
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "granularity": gran, "items": items})
	default:
		return httpx.BadRequest("granularity: 15m veya day")
	}
	return nil
}

func (h *Handlers) meterChange(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	var c MeterChange
	if err := httpx.Decode(r, &c); err != nil {
		return err
	}
	c.SubscriberID = id
	var m *Meter
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		m, err = ReplaceMeter(r.Context(), tx, c)
		return err
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, ErrNoActiveMeter):
		return httpx.ErrNotFound
	case errors.Is(err, ErrBadChange):
		return httpx.Validation(map[string]any{"meter_change": "at 15 dk sınırında ve kurulumdan sonra olmalı; kapanış endeksi son okumadan küçük olamaz; new_serial_no gerekli"})
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return httpx.Conflict("serial_taken", "bu seri numaralı sayaç zaten kayıtlı")
	case err != nil:
		return err
	}
	httpx.JSON(w, http.StatusCreated, m)
	return nil
}

func (h *Handlers) stats(w http.ResponseWriter, r *http.Request) error {
	var out struct {
		LastBatchAt   *time.Time `json:"last_batch_at"`
		LastReadAt    *time.Time `json:"last_read_at"`
		Batches1h     int64      `json:"batches_last_hour"`
		Received1h    int64      `json:"received_last_hour"`
		Inserted1h    int64      `json:"inserted_last_hour"`
		Duplicates1h  int64      `json:"duplicates_last_hour"`
		Corrections1h int64      `json:"corrections_last_hour"`
		Rejected1h    int64      `json:"rejected_last_hour"`
		DirtyQueue    int64      `json:"dirty_queue"`
	}
	err := h.pool.QueryRow(r.Context(), `SELECT
		(SELECT max(created_at) FROM ingest_batches),
		(SELECT max(max_read_at) FROM ingest_batches),
		count(*), coalesce(sum(received),0), coalesce(sum(inserted),0), coalesce(sum(duplicates),0),
		coalesce(sum(corrections),0), coalesce(sum(rejected),0),
		(SELECT count(*) FROM consumption_dirty)
		FROM ingest_batches WHERE created_at > now() - interval '1 hour'`).
		Scan(&out.LastBatchAt, &out.LastReadAt, &out.Batches1h, &out.Received1h, &out.Inserted1h, &out.Duplicates1h,
			&out.Corrections1h, &out.Rejected1h, &out.DirtyQueue)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

func (h *Handlers) recompute(w http.ResponseWriter, r *http.Request) error {
	n, err := DrainDirty(r.Context(), h.pool, 5000)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"processed_meter_days": n})
	return nil
}
