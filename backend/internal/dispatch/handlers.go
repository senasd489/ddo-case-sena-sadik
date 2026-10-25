package dispatch

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/grid"
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

// MountIngest: signals come from the call centre / meter head-end (token)
// or are typed in by an operator.
func (h *Handlers) MountIngest(r chi.Router) {
	in := auth.IngestOrRoles(h.signer, h.ingestToken, auth.RoleOperator, auth.RoleAdmin)
	r.With(in).Post("/outage-signals", httpx.H(h.signal))
	r.With(in).Post("/outage-signals/batch", httpx.H(h.signalBatch))
}

func (h *Handlers) Mount(r chi.Router) {
	operators := auth.Require(auth.RoleOperator, auth.RoleAdmin)
	viewers := auth.Require(auth.RoleOperator, auth.RoleAnalyst, auth.RoleFieldCrew, auth.RoleAdmin)
	r.With(viewers).Get("/outages/{id}/signals", httpx.H(h.signals))
	r.With(operators).Post("/outages/{id}/dispatch", httpx.H(h.dispatch))
	r.With(operators).Post("/outages/{id}/release", httpx.H(h.release))
	r.With(viewers).Get("/crews", httpx.H(h.crews))
	r.With(auth.Require(auth.RoleFieldCrew)).Get("/crew/me", httpx.H(h.me))
	r.With(auth.Require(auth.RoleFieldCrew)).Post("/crew/me/transition", httpx.H(h.crewTransition))
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrUnknownSource):
		return httpx.Validation(map[string]any{"source": "call veya last_gasp"})
	case errors.Is(err, ErrUnknownCustomer):
		return httpx.Validation(map[string]any{"subscriber_id": "abone/sayaç bulunamadı"})
	case errors.Is(err, ErrNoCrew):
		return httpx.Conflict("no_crew_available", "gerekli yetkinlikte boşta ekip yok")
	case errors.Is(err, ErrNotDispatchable):
		return httpx.Conflict("not_dispatchable", "yalnızca onaylanmış (confirmed) kesintiye ekip atanabilir")
	case errors.Is(err, ErrNoAssignment):
		return httpx.Conflict("no_assignment", "ekibinizin aktif bir görevi yok")
	case errors.Is(err, grid.ErrOutageNotFound):
		return httpx.ErrNotFound
	case errors.Is(err, grid.ErrBadTransition):
		return httpx.Conflict("invalid_transition", err.Error())
	}
	return err
}

func (h *Handlers) signal(w http.ResponseWriter, r *http.Request) error {
	var s Signal
	if err := httpx.Decode(r, &s); err != nil {
		return err
	}
	res, err := Ingest(r.Context(), h.pool, s)
	if err != nil {
		return mapErr(err)
	}
	status := http.StatusAccepted
	if res.Outcome == "new_outage" {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, res)
	return nil
}

type batchReq struct {
	Signals []Signal `json:"signals"`
}

func (h *Handlers) signalBatch(w http.ResponseWriter, r *http.Request) error {
	var req batchReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if len(req.Signals) == 0 || len(req.Signals) > 2000 {
		return httpx.Validation(map[string]any{"signals": "1-2000 bildirim"})
	}
	out := map[string]int{}
	outages := map[int64]bool{}
	for _, s := range req.Signals {
		res, err := Ingest(r.Context(), h.pool, s)
		if err != nil {
			out["rejected"]++
			continue
		}
		out[res.Outcome]++
		outages[res.OutageID] = true
	}
	httpx.JSON(w, http.StatusAccepted, map[string]any{"result": out, "outages": len(outages)})
	return nil
}

type signalRow struct {
	ID           int64     `json:"id"`
	Source       string    `json:"source"`
	ExternalID   *string   `json:"external_id"`
	SubscriberID *int64    `json:"subscriber_id"`
	PoleID       *int64    `json:"pole_id"`
	ReceivedAt   time.Time `json:"received_at"`
	Duplicate    bool      `json:"duplicate"`
}

func (h *Handlers) signals(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	rows, err := h.pool.Query(r.Context(), `SELECT id, source, external_id, subscriber_id, pole_id, received_at, duplicate
		FROM outage_signals WHERE outage_id = $1 ORDER BY received_at, id LIMIT 500`, id)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[signalRow])
	if err != nil {
		return err
	}
	if items == nil {
		items = []signalRow{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handlers) dispatch(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	a, err := Dispatch(r.Context(), h.pool, id, auth.FromContext(r.Context()).UserID)
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusCreated, a)
	return nil
}

func (h *Handlers) release(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		return ReleaseTx(r.Context(), tx, id, auth.FromContext(r.Context()).UserID, "ekip geri çekildi")
	})
	if err != nil {
		return mapErr(err)
	}
	o, err := grid.GetOutage(r.Context(), h.pool, id, false)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, o)
	return nil
}

type crewRow struct {
	ID          int64     `json:"id"`
	Code        string    `json:"code"`
	Name        string    `json:"name"`
	Skills      []string  `json:"skills"`
	Lat         float64   `json:"lat"`
	Lon         float64   `json:"lon"`
	Status      string    `json:"status"`
	SpeedFactor float64   `json:"speed_factor"`
	UpdatedAt   time.Time `json:"updated_at"`
	OutageID    *int64    `json:"active_outage_id"`
}

const crewCols = `c.id, c.code, c.name, c.skills, c.lat, c.lon, c.status, c.speed_factor::float8, c.updated_at, a.outage_id`

func (h *Handlers) crews(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.pool.Query(r.Context(), `SELECT `+crewCols+` FROM crews c
		LEFT JOIN crew_assignments a ON a.crew_id = c.id AND a.status = 'active' ORDER BY c.id`)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[crewRow])
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) error {
	p := auth.FromContext(r.Context())
	if p.CrewID == nil {
		return httpx.ErrNotFound
	}
	rows, err := h.pool.Query(r.Context(), `SELECT `+crewCols+` FROM crews c
		LEFT JOIN crew_assignments a ON a.crew_id = c.id AND a.status = 'active' WHERE c.id = $1`, *p.CrewID)
	if err != nil {
		return err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[crewRow])
	if err != nil {
		return err
	}
	out := map[string]any{"crew": c, "outage": nil}
	if c.OutageID != nil {
		o, err := grid.GetOutage(r.Context(), h.pool, *c.OutageID, false)
		if err != nil {
			return err
		}
		out["outage"] = o
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

type crewTransitionReq struct {
	To   string `json:"to"`
	Note string `json:"note"`
}

func (h *Handlers) crewTransition(w http.ResponseWriter, r *http.Request) error {
	p := auth.FromContext(r.Context())
	if p.CrewID == nil {
		return httpx.ErrForbidden
	}
	var req crewTransitionReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	var o *grid.Outage
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		o, err = CrewTransitionTx(r.Context(), tx, *p.CrewID, req.To, p.UserID, req.Note)
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusOK, o)
	return nil
}
