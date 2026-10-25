package grid

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
)

type Handlers struct{ pool *pgxpool.Pool }

func NewHandlers(pool *pgxpool.Pool) *Handlers { return &Handlers{pool: pool} }

func (h *Handlers) Mount(r chi.Router) {
	viewers := auth.Require(auth.RoleOperator, auth.RoleAnalyst, auth.RoleFieldCrew, auth.RoleAdmin)
	operators := auth.Require(auth.RoleOperator, auth.RoleAdmin)

	r.With(viewers).Get("/network/tree", httpx.H(h.tree))
	r.With(viewers).Get("/network/nodes/{id}/impact", httpx.H(h.impact))
	r.With(operators).Post("/network/switch", httpx.H(h.switchNode))
	r.With(operators).Post("/network/nodes/{id}/restore-normal", httpx.H(h.restoreNormal))

	r.With(operators).Post("/outages", httpx.H(h.create))
	r.With(viewers).Get("/outages", httpx.H(h.list))
	r.With(viewers).Get("/outages/{id}", httpx.H(h.get))
	r.With(operators).Post("/outages/{id}/transition", httpx.H(h.transition))
	r.With(auth.Require(auth.RoleOperator, auth.RoleAnalyst, auth.RoleAdmin)).Get("/reports/reliability", httpx.H(h.reliability))
	// Abone: kendi beslemesindeki açık kesintiler.
	r.With(auth.Require(auth.RoleSubscriber)).Get("/me/outages", httpx.H(h.myOutages))
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, ErrNodeNotFound), errors.Is(err, ErrOutageNotFound):
		return httpx.ErrNotFound
	case errors.Is(err, ErrCycle):
		return httpx.Conflict("topology_cycle", "bu anahtarlama ağaçta döngü oluşturur")
	case errors.Is(err, ErrBadParent):
		return httpx.Validation(map[string]any{"new_parent_id": "besleyici yalnızca trafoya, direk yalnızca besleyici/direğe bağlanabilir; trafo taşınamaz"})
	case errors.Is(err, ErrPlannedOverlap):
		return httpx.Conflict("planned_overlap", err.Error())
	case errors.Is(err, ErrBadTransition):
		return httpx.Conflict("invalid_transition", err.Error())
	case errors.Is(err, ErrStaleVersion):
		return httpx.Conflict("stale_version", "kesinti başka biri tarafından güncellendi; yenileyip tekrar deneyin")
	case errors.Is(err, ErrBadOutage):
		return httpx.Validation(map[string]any{"outage": err.Error()})
	}
	return err
}

func (h *Handlers) tree(w http.ResponseWriter, r *http.Request) error {
	root, _ := strconv.ParseInt(r.URL.Query().Get("root"), 10, 64)
	nodes, err := Tree(r.Context(), h.pool, root)
	if err != nil {
		return err
	}
	if nodes == nil {
		nodes = []Node{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": nodes})
	return nil
}

func (h *Handlers) impact(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	im, err := ImpactOf(r.Context(), h.pool, id)
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusOK, im)
	return nil
}

type switchReq struct {
	NodeID      int64 `json:"node_id"`
	NewParentID int64 `json:"new_parent_id"`
}

func (h *Handlers) switchNode(w http.ResponseWriter, r *http.Request) error {
	var req switchReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.NodeID <= 0 || req.NewParentID <= 0 {
		return httpx.Validation(map[string]any{"node_id": "gerekli", "new_parent_id": "gerekli"})
	}
	var res *SwitchResult
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		res, err = Switch(r.Context(), tx, req.NodeID, req.NewParentID)
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusOK, res)
	return nil
}

func (h *Handlers) restoreNormal(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	var res *SwitchResult
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var normal *int64
		if err := tx.QueryRow(r.Context(), `SELECT normal_parent_id FROM network_nodes WHERE id = $1`, id).Scan(&normal); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNodeNotFound
			}
			return err
		}
		if normal == nil {
			return ErrBadParent
		}
		res, err = Switch(r.Context(), tx, id, *normal)
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusOK, res)
	return nil
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) error {
	var req CreateOutage
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.FaultNodeID <= 0 {
		return httpx.Validation(map[string]any{"fault_node_id": "gerekli"})
	}
	req.CreatedBy = auth.FromContext(r.Context()).UserID
	var o *Outage
	err := pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		o, err = CreateOutageTx(r.Context(), tx, req)
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusCreated, o)
	return nil
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
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
	q := r.URL.Query()
	var statuses []string
	if s := q.Get("status"); s != "" {
		statuses = strings.Split(s, ",")
	}
	if q.Get("open") == "true" {
		statuses = []string{"scheduled", "detected", "confirmed", "assigned", "in_progress"}
	}
	rows, err := h.pool.Query(r.Context(), `SELECT `+outageCols+` FROM outages o JOIN network_nodes n ON n.id = o.fault_node_id
		WHERE o.id < $1 AND (cardinality($2::text[]) = 0 OR o.status = ANY($2)) AND ($3 = '' OR o.kind = $3)
		ORDER BY o.id DESC LIMIT $4`, before, statuses, q.Get("kind"), pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Outage, error) { return ScanOutage(row) })
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(o *Outage) string {
		return httpx.EncodeCursor(strconv.FormatInt(o.ID, 10))
	}))
	return nil
}

type historyRow struct {
	From      *string   `json:"from_status"`
	To        string    `json:"to_status"`
	ChangedBy *int64    `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
	Note      *string   `json:"note"`
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	o, err := GetOutage(ctx, h.pool, id, false)
	if err != nil {
		return mapErr(err)
	}
	rows, err := h.pool.Query(ctx, `SELECT from_status, to_status, changed_by, changed_at, note FROM outage_status_history
		WHERE outage_id = $1 ORDER BY id`, id)
	if err != nil {
		return err
	}
	hist, err := pgx.CollectRows(rows, pgx.RowToStructByPos[historyRow])
	if err != nil {
		return err
	}
	var interrupted, restored int64
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE restored_at IS NULL), count(*) FILTER (WHERE restored_at IS NOT NULL)
		FROM outage_affected WHERE outage_id = $1`, id).Scan(&interrupted, &restored); err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"outage": o, "history": hist,
		"affected": map[string]int64{"interrupted": interrupted, "restored": restored}})
	return nil
}

type transitionReq struct {
	To      string `json:"to"`
	Note    string `json:"note"`
	Version *int   `json:"version"`
}

func (h *Handlers) transition(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	var req transitionReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	ver := -1
	if req.Version != nil {
		ver = *req.Version
	}
	var o *Outage
	err = pgx.BeginFunc(r.Context(), h.pool, func(tx pgx.Tx) error {
		var err error
		o, err = TransitionTx(r.Context(), tx, id, req.To, auth.FromContext(r.Context()).UserID, req.Note, ver)
		return err
	})
	if err != nil {
		return mapErr(err)
	}
	httpx.JSON(w, http.StatusOK, o)
	return nil
}

func (h *Handlers) reliability(w http.ResponseWriter, r *http.Request) error {
	from, to, err := httpx.ParseRange(r, 30*24*time.Hour, 3*366*24*time.Hour)
	if err != nil {
		return err
	}
	feeder, _ := strconv.ParseInt(r.URL.Query().Get("feeder_id"), 10, 64)
	rel, err := ReliabilityIndices(r.Context(), h.pool, from, to, feeder)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, rel)
	return nil
}

func (h *Handlers) myOutages(w http.ResponseWriter, r *http.Request) error {
	p := auth.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), `SELECT `+outageCols+` FROM outages o JOIN network_nodes n ON n.id = o.fault_node_id
		JOIN outage_affected a ON a.outage_id = o.id
		WHERE a.subscriber_id = $1 AND (a.restored_at IS NULL OR o.status = 'scheduled')
		  AND o.status NOT IN ('resolved','cancelled') ORDER BY o.started_at DESC`, *p.SubscriberID)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Outage, error) { return ScanOutage(row) })
	if err != nil {
		return err
	}
	if items == nil {
		items = []*Outage{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}
