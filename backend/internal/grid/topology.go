// Package grid implements T-3: the network topology (transformer → feeder →
// pole → subscriber), outage impact, switching to backup supply, outage
// records with a state machine, planned-outage conflict checks and the
// SAIDI/SAIFI reliability indices.
package grid

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
)

var (
	ErrNodeNotFound = errors.New("network node not found")
	ErrCycle        = errors.New("switching would create a cycle")
	ErrBadParent    = errors.New("invalid parent for this node kind")
)

type Node struct {
	ID             int64    `json:"id"`
	ParentID       *int64   `json:"parent_id"`
	AltParentID    *int64   `json:"alt_parent_id,omitempty"`
	NormalParentID *int64   `json:"normal_parent_id,omitempty"`
	Kind           string   `json:"kind"`
	Code           string   `json:"code"`
	Name           string   `json:"name"`
	Lat            *float64 `json:"lat,omitempty"`
	Lon            *float64 `json:"lon,omitempty"`
	Depth          int      `json:"depth"`
	Subscribers    int64    `json:"subscribers"` // alt ağaçtaki toplam abone
	Switched       bool     `json:"switched"`    // normal beslemesinden farklı bir kaynaktan besleniyor
}

// treeSQL walks the forest top-down once, carrying each node's ancestor path;
// subtree subscriber counts are then a single GROUP BY over unnest(path).
const treeSQL = `
WITH RECURSIVE t AS (
    SELECT id, parent_id, alt_parent_id, normal_parent_id, kind, code, name, lat, lon, 0 AS depth, ARRAY[id] AS path
    FROM network_nodes WHERE parent_id IS NULL AND ($1::bigint = 0 OR id = $1)
    UNION ALL
    SELECT n.id, n.parent_id, n.alt_parent_id, n.normal_parent_id, n.kind, n.code, n.name, n.lat, n.lon, t.depth + 1, t.path || n.id
    FROM network_nodes n JOIN t ON n.parent_id = t.id
    WHERE t.depth < 64
),
direct AS (SELECT pole_id, count(*) AS c FROM subscribers WHERE active GROUP BY pole_id),
agg AS (
    SELECT p.node_id, sum(d.c) AS c
    FROM t JOIN direct d ON d.pole_id = t.id
    CROSS JOIN LATERAL unnest(t.path) AS p(node_id)
    GROUP BY p.node_id
)
SELECT t.id, t.parent_id, t.alt_parent_id, t.normal_parent_id, t.kind, t.code, t.name, t.lat, t.lon, t.depth,
       coalesce(agg.c, 0)::bigint, (t.parent_id IS DISTINCT FROM t.normal_parent_id AND t.normal_parent_id IS NOT NULL)
FROM t LEFT JOIN agg ON agg.node_id = t.id
ORDER BY t.path`

// Tree returns the topology (optionally one transformer) in depth-first order.
func Tree(ctx context.Context, q db.Querier, rootID int64) ([]Node, error) {
	rows, err := q.Query(ctx, treeSQL, rootID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Node])
}

// descendantsSQL lists the subscribers fed through node $1.
const descendantsSQL = `
WITH RECURSIVE down AS (
    SELECT id, 0 AS depth FROM network_nodes WHERE id = $1
    UNION ALL
    SELECT n.id, down.depth + 1 FROM network_nodes n JOIN down ON n.parent_id = down.id WHERE down.depth < 64
)
SELECT s.id FROM subscribers s JOIN down ON s.pole_id = down.id WHERE s.active`

// AffectedSubscribers returns every active subscriber downstream of nodeID.
func AffectedSubscribers(ctx context.Context, q db.Querier, nodeID int64) ([]int64, error) {
	rows, err := q.Query(ctx, descendantsSQL, nodeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Lineage is the supply path of a node from itself up to its transformer.
type Lineage struct {
	NodeID        int64   `json:"node_id"`
	Kind          string  `json:"kind"`
	FeederID      int64   `json:"feeder_id"` // trafo arızasında trafonun kendisi
	TransformerID int64   `json:"transformer_id"`
	Path          []int64 `json:"path"` // düğüm → … → trafo
}

func LineageOf(ctx context.Context, q db.Querier, nodeID int64) (*Lineage, error) {
	rows, err := q.Query(ctx, `WITH RECURSIVE up AS (
			SELECT id, parent_id, kind, 0 AS depth FROM network_nodes WHERE id = $1
			UNION ALL
			SELECT n.id, n.parent_id, n.kind, up.depth + 1 FROM network_nodes n JOIN up ON n.id = up.parent_id WHERE up.depth < 64
		) SELECT id, kind FROM up ORDER BY depth`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	l := &Lineage{NodeID: nodeID}
	for rows.Next() {
		var id int64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		if len(l.Path) == 0 {
			l.Kind = kind
		}
		l.Path = append(l.Path, id)
		switch kind {
		case "feeder":
			l.FeederID = id
		case "transformer":
			l.TransformerID = id
			if l.FeederID == 0 {
				l.FeederID = id
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(l.Path) == 0 {
		return nil, ErrNodeNotFound
	}
	return l, nil
}

// Impact summarises what a fault at nodeID would interrupt.
type Impact struct {
	Lineage     *Lineage         `json:"lineage"`
	Subscribers int              `json:"subscribers"`
	ByCategory  map[string]int64 `json:"by_category"`
	Sample      []int64          `json:"sample_subscriber_ids"`
}

func ImpactOf(ctx context.Context, q db.Querier, nodeID int64) (*Impact, error) {
	l, err := LineageOf(ctx, q, nodeID)
	if err != nil {
		return nil, err
	}
	ids, err := AffectedSubscribers(ctx, q, nodeID)
	if err != nil {
		return nil, err
	}
	im := &Impact{Lineage: l, Subscribers: len(ids), ByCategory: map[string]int64{}}
	if len(ids) > 20 {
		im.Sample = ids[:20]
	} else {
		im.Sample = ids
	}
	rows, err := q.Query(ctx, `SELECT category, count(*) FROM subscribers WHERE id = ANY($1) GROUP BY category`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		im.ByCategory[c] = n
	}
	return im, rows.Err()
}

// SwitchResult reports what a switching operation changed.
type SwitchResult struct {
	NodeID         int64 `json:"node_id"`
	OldParentID    int64 `json:"old_parent_id"`
	NewParentID    int64 `json:"new_parent_id"`
	MovedSubs      int   `json:"moved_subscribers"`
	RestoredSubs   int64 `json:"restored_subscribers"`
	InterruptedSub int64 `json:"newly_interrupted_subscribers"`
}

// Switch re-parents a node (e.g. a feeder onto its backup transformer, or a
// pole subtree onto another feeder) and recomputes every open outage on the
// new topology. The database trigger rejects cycles and wrong parent kinds.
func Switch(ctx context.Context, tx pgx.Tx, nodeID, newParentID int64) (*SwitchResult, error) {
	var old *int64
	var kind string
	err := tx.QueryRow(ctx, `SELECT parent_id, kind FROM network_nodes WHERE id = $1 FOR UPDATE`, nodeID).Scan(&old, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNodeNotFound
	}
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, ErrBadParent // trafo taşınamaz
	}
	moved, err := AffectedSubscribers(ctx, tx, nodeID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE network_nodes SET parent_id = $2 WHERE id = $1`, nodeID, newParentID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch {
			case pgErr.Code == "23514" && pgErr.Hint == "cycle":
				return nil, ErrCycle
			case pgErr.Code == "23514", pgErr.Code == "23503":
				return nil, ErrBadParent
			}
		}
		return nil, err
	}
	res := &SwitchResult{NodeID: nodeID, OldParentID: *old, NewParentID: newParentID, MovedSubs: len(moved)}
	res.RestoredSubs, res.InterruptedSub, err = RecalcOpenOutages(ctx, tx)
	if err != nil {
		return nil, err
	}
	err = audit.Record(ctx, tx, audit.Entry{Action: "topology.switch", Entity: "network_node", EntityID: strconv.FormatInt(nodeID, 10),
		Old: map[string]any{"parent_id": *old}, New: map[string]any{"parent_id": newParentID, "restored": res.RestoredSubs}})
	return res, err
}

// RecalcOpenOutages re-evaluates the affected set of every open unplanned
// or in-progress outage on the current topology: subscribers no longer
// downstream of the fault are restored now; subscribers that moved under a
// faulted node are interrupted now.
func RecalcOpenOutages(ctx context.Context, tx pgx.Tx) (restored, interrupted int64, err error) {
	rows, err := tx.Query(ctx, `SELECT id, fault_node_id FROM outages
		WHERE status IN ('detected','confirmed','assigned','in_progress') FOR UPDATE`)
	if err != nil {
		return 0, 0, err
	}
	type o struct{ id, node int64 }
	var open []o
	for rows.Next() {
		var x o
		if err := rows.Scan(&x.id, &x.node); err != nil {
			rows.Close()
			return 0, 0, err
		}
		open = append(open, x)
	}
	rows.Close()
	for _, x := range open {
		ids, err := AffectedSubscribers(ctx, tx, x.node)
		if err != nil {
			return 0, 0, err
		}
		tag, err := tx.Exec(ctx, `UPDATE outage_affected SET restored_at = now()
			WHERE outage_id = $1 AND restored_at IS NULL AND NOT (subscriber_id = ANY($2))`, x.id, ids)
		if err != nil {
			return 0, 0, err
		}
		restored += tag.RowsAffected()
		tag, err = tx.Exec(ctx, `INSERT INTO outage_affected (outage_id, subscriber_id, interrupted_at)
			SELECT $1, s, now() FROM unnest($2::bigint[]) s ON CONFLICT DO NOTHING`, x.id, ids)
		if err != nil {
			return 0, 0, err
		}
		interrupted += tag.RowsAffected()
		if _, err := tx.Exec(ctx, `UPDATE outages SET affected_count = (SELECT count(*) FROM outage_affected WHERE outage_id = $1 AND restored_at IS NULL),
			updated_at = now() WHERE id = $1`, x.id); err != nil {
			return 0, 0, err
		}
	}
	return restored, interrupted, nil
}
