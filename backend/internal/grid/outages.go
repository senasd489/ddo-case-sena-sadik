package grid

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

var (
	ErrBadTransition  = errors.New("invalid outage status transition")
	ErrPlannedOverlap = errors.New("planned outage overlaps another planned outage on the same supply path")
	ErrOutageNotFound = errors.New("outage not found")
	ErrBadOutage      = errors.New("invalid outage")
	ErrStaleVersion   = errors.New("outage was modified concurrently")
)

var outageTransitions = metrics.NewCounter("enerjihat_outage_transitions_total", "Kesinti durum geçişleri", "to")

// transitions is the outage state machine. Anything not listed is rejected.
//
//	unplanned: detected → confirmed → assigned → in_progress → resolved
//	           detected → resolved (kendiliğinden düzeldi, ör. tekrar kapama)
//	           assigned → confirmed (ekip geri çekildi)
//	           assigned → resolved (ekip varmadan enerji geri geldi, ör. sayaçlardan restore)
//	           detected|confirmed|assigned → cancelled (yanlış alarm)
//	planned:   scheduled → in_progress → resolved ; scheduled → cancelled
var transitions = map[string]map[string][]string{
	"unplanned": {
		"detected":    {"confirmed", "resolved", "cancelled"},
		"confirmed":   {"assigned", "resolved", "cancelled"},
		"assigned":    {"in_progress", "confirmed", "resolved", "cancelled"},
		"in_progress": {"resolved"},
	},
	"planned": {
		"scheduled":   {"in_progress", "cancelled"},
		"in_progress": {"resolved"},
	},
}

// CanTransition reports whether kind/from → to is allowed.
func CanTransition(kind, from, to string) bool {
	for _, t := range transitions[kind][from] {
		if t == to {
			return true
		}
	}
	return false
}

type Outage struct {
	ID                 int64      `json:"id"`
	Kind               string     `json:"kind"`
	Status             string     `json:"status"`
	FaultNodeID        int64      `json:"fault_node_id"`
	FaultNodeCode      string     `json:"fault_node_code"`
	FeederID           int64      `json:"feeder_id"`
	RequiredSkill      string     `json:"required_skill"`
	Cause              *string    `json:"cause"`
	StartedAt          time.Time  `json:"started_at"`
	EndedAt            *time.Time `json:"ended_at"`
	PlannedStart       *time.Time `json:"planned_start"`
	PlannedEnd         *time.Time `json:"planned_end"`
	EstimatedRestoreAt *time.Time `json:"estimated_restore_at"`
	AffectedCount      int        `json:"affected_count"`
	ReportCount        int        `json:"report_count"`
	Version            int        `json:"version"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

const outageCols = `o.id, o.kind, o.status, o.fault_node_id, n.code, o.feeder_id, o.required_skill, o.cause, o.started_at, o.ended_at,
	o.planned_start, o.planned_end, o.estimated_restore_at, o.affected_count, o.report_count, o.version, o.created_at, o.updated_at`

func ScanOutage(row pgx.Row) (*Outage, error) {
	var o Outage
	err := row.Scan(&o.ID, &o.Kind, &o.Status, &o.FaultNodeID, &o.FaultNodeCode, &o.FeederID, &o.RequiredSkill, &o.Cause, &o.StartedAt,
		&o.EndedAt, &o.PlannedStart, &o.PlannedEnd, &o.EstimatedRestoreAt, &o.AffectedCount, &o.ReportCount, &o.Version,
		&o.CreatedAt, &o.UpdatedAt)
	return &o, err
}

func GetOutage(ctx context.Context, q db.Querier, id int64, lock bool) (*Outage, error) {
	sql := `SELECT ` + outageCols + ` FROM outages o JOIN network_nodes n ON n.id = o.fault_node_id WHERE o.id = $1`
	if lock {
		sql += ` FOR UPDATE OF o`
	}
	o, err := ScanOutage(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOutageNotFound
	}
	return o, err
}

type CreateOutage struct {
	Kind          string     `json:"kind"` // unplanned | planned
	FaultNodeID   int64      `json:"fault_node_id"`
	Cause         string     `json:"cause"`
	RequiredSkill string     `json:"required_skill"`
	StartedAt     *time.Time `json:"started_at"`
	PlannedStart  *time.Time `json:"planned_start"`
	PlannedEnd    *time.Time `json:"planned_end"`
	CreatedBy     int64      `json:"-"`
}

// CreateOutageTx records an outage and snapshots the affected subscribers.
// Planned outages are checked against other planned outages whose fault node
// is an ancestor or a descendant of this one (i.e. that would interrupt an
// overlapping set of customers) in an overlapping time window. A per-
// transformer advisory lock makes the check-then-insert atomic; the EXCLUDE
// constraint on (feeder, time range) is a second line of defence.
func CreateOutageTx(ctx context.Context, tx pgx.Tx, c CreateOutage) (*Outage, error) {
	if c.Kind != "planned" && c.Kind != "unplanned" {
		return nil, fmt.Errorf("%w: kind must be planned or unplanned", ErrBadOutage)
	}
	if c.RequiredSkill == "" {
		c.RequiredSkill = "lv"
	}
	l, err := LineageOf(ctx, tx, c.FaultNodeID)
	if err != nil {
		return nil, err
	}
	if l.Kind == "transformer" && c.RequiredSkill == "lv" {
		c.RequiredSkill = "transformer"
	}
	status := "detected"
	started := time.Now()
	if c.StartedAt != nil {
		started = *c.StartedAt
	}
	if c.Kind == "planned" {
		if c.PlannedStart == nil || c.PlannedEnd == nil || !c.PlannedEnd.After(*c.PlannedStart) {
			return nil, fmt.Errorf("%w: planned_start < planned_end gerekli", ErrBadOutage)
		}
		status = "scheduled"
		started = *c.PlannedStart
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4343, $1::int)`, l.TransformerID); err != nil {
			return nil, err
		}
		var conflict *int64
		err := tx.QueryRow(ctx, `
			WITH RECURSIVE down AS (
				SELECT id, 0 AS depth FROM network_nodes WHERE id = $1
				UNION ALL SELECT n.id, down.depth + 1 FROM network_nodes n JOIN down ON n.parent_id = down.id WHERE down.depth < 64
			)
			SELECT o.id FROM outages o
			WHERE o.kind = 'planned' AND o.status <> 'cancelled'
			  AND tstzrange(o.planned_start, o.planned_end, '[)') && tstzrange($3, $4, '[)')
			  AND (o.fault_node_id = ANY($2) OR o.fault_node_id IN (SELECT id FROM down))
			LIMIT 1`, c.FaultNodeID, l.Path, c.PlannedStart, c.PlannedEnd).Scan(&conflict)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if conflict != nil {
			return nil, fmt.Errorf("%w (kesinti #%d)", ErrPlannedOverlap, *conflict)
		}
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO outages (kind, status, fault_node_id, feeder_id, required_skill, cause, started_at,
			planned_start, planned_end, created_by)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,NULLIF($10,0)) RETURNING id`,
		c.Kind, status, c.FaultNodeID, l.FeederID, c.RequiredSkill, c.Cause, started, c.PlannedStart, c.PlannedEnd, c.CreatedBy).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
			return nil, ErrPlannedOverlap
		}
		return nil, err
	}
	affected, err := AffectedSubscribers(ctx, tx, c.FaultNodeID)
	if err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO outage_affected (outage_id, subscriber_id, interrupted_at)
		SELECT $1, s, $2 FROM unnest($3::bigint[]) s`, id, started, affected)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET affected_count = $2 WHERE id = $1`, id, tag.RowsAffected()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outage_status_history (outage_id, from_status, to_status, changed_by, note)
		VALUES ($1, NULL, $2, NULLIF($3,0), $4)`, id, status, c.CreatedBy, c.Cause); err != nil {
		return nil, err
	}
	o, err := GetOutage(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	return o, audit.Record(ctx, tx, audit.Entry{Action: "outage.create", Entity: "outage", EntityID: strconv.FormatInt(id, 10), New: o})
}

// TransitionTx moves an outage through the state machine. expectedVersion
// (optional, -1 to skip) implements optimistic concurrency for UIs.
func TransitionTx(ctx context.Context, tx pgx.Tx, id int64, to string, by int64, note string, expectedVersion int) (*Outage, error) {
	o, err := GetOutage(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if expectedVersion >= 0 && o.Version != expectedVersion {
		return nil, ErrStaleVersion
	}
	if !CanTransition(o.Kind, o.Status, to) {
		return nil, fmt.Errorf("%w: %s → %s (%s)", ErrBadTransition, o.Status, to, o.Kind)
	}
	now := time.Now()
	switch to {
	case "resolved":
		if _, err := tx.Exec(ctx, `UPDATE outages SET ended_at = $2 WHERE id = $1`, id, now); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE outage_affected SET restored_at = $2 WHERE outage_id = $1 AND restored_at IS NULL`, id, now); err != nil {
			return nil, err
		}
	case "cancelled":
		// Yanlış alarm / iptal edilen planlı kesinti: kesinti yaşanmadı, güvenilirlik hesabına girmez.
		if _, err := tx.Exec(ctx, `DELETE FROM outage_affected WHERE outage_id = $1`, id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE outages SET ended_at = $2, affected_count = 0 WHERE id = $1`, id, now); err != nil {
			return nil, err
		}
	case "in_progress":
		if o.Kind == "planned" {
			if _, err := tx.Exec(ctx, `UPDATE outages SET started_at = $2 WHERE id = $1`, id, now); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE outage_affected SET interrupted_at = $2 WHERE outage_id = $1`, id, now); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET status = $2, version = version + 1, updated_at = now() WHERE id = $1`, id, to); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outage_status_history (outage_id, from_status, to_status, changed_by, note)
		VALUES ($1,$2,$3,NULLIF($4,0),NULLIF($5,''))`, id, o.Status, to, by, note); err != nil {
		return nil, err
	}
	if to == "resolved" || to == "cancelled" {
		// Bu kesintide aktif ekip ataması varsa kapat, ekibi serbest bırak.
		if _, err := tx.Exec(ctx, `WITH a AS (
				UPDATE crew_assignments SET status = CASE WHEN $2 = 'resolved' THEN 'completed' ELSE 'released' END, closed_at = now()
				WHERE outage_id = $1 AND status = 'active' RETURNING crew_id)
			UPDATE crews SET status = 'available', updated_at = now() WHERE id IN (SELECT crew_id FROM a)`, id, to); err != nil {
			return nil, err
		}
	}
	outageTransitions.Inc(to)
	n, err := GetOutage(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	return n, audit.Record(ctx, tx, audit.Entry{Action: "outage.transition", Entity: "outage", EntityID: strconv.FormatInt(id, 10),
		Old: map[string]any{"status": o.Status, "version": o.Version}, New: map[string]any{"status": to, "version": n.Version, "note": note}})
}

// Reliability holds IEEE 1366 indices for a window.
type Reliability struct {
	From                time.Time `json:"from"`
	To                  time.Time `json:"to"`
	FeederID            *int64    `json:"feeder_id,omitempty"`
	CustomersServed     int64     `json:"customers_served"`
	Interruptions       int64     `json:"customer_interruptions"`
	CustomerMinutes     float64   `json:"customer_minutes_interrupted"`
	SAIDI               float64   `json:"saidi_minutes"`
	SAIFI               float64   `json:"saifi"`
	CAIDI               float64   `json:"caidi_minutes"`
	Unplanned           Indices   `json:"unplanned"`
	Planned             Indices   `json:"planned"`
	SustainedMinMinutes int       `json:"sustained_threshold_minutes"`
}

type Indices struct {
	SAIDI float64 `json:"saidi_minutes"`
	SAIFI float64 `json:"saifi"`
}

// ReliabilityIndices computes SAIDI (Σ customer-minutes / customers served)
// and SAIFI (Σ customers interrupted / customers served) for interruptions
// longer than 3 minutes (IEEE 1366 "sustained"), clipped to [from, to).
// Open interruptions count until now. feederID filters to one supply node.
func ReliabilityIndices(ctx context.Context, q db.Querier, from, to time.Time, feederID int64) (*Reliability, error) {
	r := &Reliability{From: from, To: to, SustainedMinMinutes: 3}
	if feederID > 0 {
		r.FeederID = &feederID
	}
	err := q.QueryRow(ctx, `
		WITH RECURSIVE down AS (
			SELECT id, 0 AS depth FROM network_nodes WHERE id = $1
			UNION ALL SELECT n.id, down.depth + 1 FROM network_nodes n JOIN down ON n.parent_id = down.id WHERE down.depth < 64
		)
		SELECT count(*) FROM subscribers s WHERE s.active AND ($1 = 0 OR s.pole_id IN (SELECT id FROM down))`,
		feederID).Scan(&r.CustomersServed)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		WITH RECURSIVE down AS (
			SELECT id, 0 AS depth FROM network_nodes WHERE id = $3
			UNION ALL SELECT n.id, down.depth + 1 FROM network_nodes n JOIN down ON n.parent_id = down.id WHERE down.depth < 64
		),
		iv AS (
			SELECT o.kind,
			       extract(epoch FROM least(coalesce(a.restored_at, now()), $2) - greatest(a.interrupted_at, $1)) / 60.0 AS minutes,
			       extract(epoch FROM coalesce(a.restored_at, now()) - a.interrupted_at) / 60.0 AS full_minutes
			FROM outage_affected a JOIN outages o ON o.id = a.outage_id
			WHERE o.status <> 'cancelled'
			  AND a.interrupted_at < $2 AND coalesce(a.restored_at, now()) > $1
			  AND ($3 = 0 OR a.subscriber_id IN (SELECT s.id FROM subscribers s JOIN down ON s.pole_id = down.id))
		)
		SELECT kind, count(*), coalesce(sum(minutes), 0)::float8 FROM iv WHERE full_minutes > 3 AND minutes > 0 GROUP BY kind`,
		from, to, feederID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int64
		var mins float64
		if err := rows.Scan(&kind, &n, &mins); err != nil {
			return nil, err
		}
		r.Interruptions += n
		r.CustomerMinutes += mins
		if r.CustomersServed > 0 {
			ix := Indices{SAIDI: round3(mins / float64(r.CustomersServed)), SAIFI: round3(float64(n) / float64(r.CustomersServed))}
			if kind == "planned" {
				r.Planned = ix
			} else {
				r.Unplanned = ix
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if r.CustomersServed > 0 {
		r.SAIDI = round3(r.CustomerMinutes / float64(r.CustomersServed))
		r.SAIFI = round3(float64(r.Interruptions) / float64(r.CustomersServed))
	}
	if r.Interruptions > 0 {
		r.CAIDI = round3(r.CustomerMinutes / float64(r.Interruptions))
	}
	r.CustomerMinutes = round3(r.CustomerMinutes)
	return r, nil
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }
