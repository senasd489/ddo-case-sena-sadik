package dispatch

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/grid"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

var (
	ErrNoCrew          = errors.New("no available crew with the required skill")
	ErrNotDispatchable = errors.New("outage is not in a dispatchable state")
	ErrNoAssignment    = errors.New("no active assignment for this crew")
)

var assignments = metrics.NewCounter("enerjihat_crew_assignments_total", "Ekip atamaları", "outcome")

// TravelKMH is the assumed average road speed used for the ETA.
const TravelKMH = 40.0

// RepairMinutes is the base repair time by fault-node kind; it is scaled by
// the crew's speed_factor (<1 means a faster crew).
var RepairMinutes = map[string]float64{"pole": 60, "feeder": 120, "transformer": 240}

type Assignment struct {
	ID               int64     `json:"id"`
	OutageID         int64     `json:"outage_id"`
	CrewID           int64     `json:"crew_id"`
	CrewCode         string    `json:"crew_code"`
	DistanceKM       float64   `json:"distance_km"`
	ETAMinutes       int       `json:"eta_minutes"`
	RepairMinutes    int       `json:"repair_minutes"`
	EstimatedRestore time.Time `json:"estimated_restore_at"`
	AssignedAt       time.Time `json:"assigned_at"`
}

// Haversine distance in km.
func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dlat, dlon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

// EstimateMinutes returns (travel, repair) minutes for a crew.
func EstimateMinutes(distanceKM float64, faultKind string, speedFactor float64) (int, int) {
	travel := int(math.Ceil(distanceKM / TravelKMH * 60))
	base, ok := RepairMinutes[faultKind]
	if !ok {
		base = 90
	}
	return travel, int(math.Ceil(base * speedFactor))
}

// nearestCrewSQL picks the closest available crew with the skill and locks
// it. SKIP LOCKED makes concurrent dispatchers take *different* crews
// instead of queueing on (or double-booking) the same one; the partial
// unique index crew_one_active_job is the final guarantee.
const nearestCrewSQL = `
SELECT c.id, c.code, c.speed_factor::float8,
       6371 * 2 * asin(sqrt(power(sin(radians(c.lat - $2) / 2), 2) +
                             cos(radians($2)) * cos(radians(c.lat)) * power(sin(radians(c.lon - $3) / 2), 2))) AS dist
FROM crews c
WHERE c.status = 'available' AND $1 = ANY(c.skills)
ORDER BY dist, c.id
LIMIT 1
FOR UPDATE SKIP LOCKED`

// DispatchTx assigns the nearest qualified free crew to a confirmed outage.
func DispatchTx(ctx context.Context, tx pgx.Tx, outageID, by int64) (*Assignment, error) {
	o, err := grid.GetOutage(ctx, tx, outageID, true)
	if err != nil {
		return nil, err
	}
	if o.Status != "confirmed" {
		return nil, ErrNotDispatchable
	}
	var lat, lon float64
	var kind string
	if err := tx.QueryRow(ctx, `SELECT coalesce(lat, 0), coalesce(lon, 0), kind FROM network_nodes WHERE id = $1`, o.FaultNodeID).Scan(&lat, &lon, &kind); err != nil {
		return nil, err
	}
	a := &Assignment{OutageID: outageID}
	var speed float64
	err = tx.QueryRow(ctx, nearestCrewSQL, o.RequiredSkill, lat, lon).Scan(&a.CrewID, &a.CrewCode, &speed, &a.DistanceKM)
	if errors.Is(err, pgx.ErrNoRows) {
		assignments.Inc("no_crew")
		return nil, ErrNoCrew
	}
	if err != nil {
		return nil, err
	}
	a.DistanceKM = math.Round(a.DistanceKM*100) / 100
	a.ETAMinutes, a.RepairMinutes = EstimateMinutes(a.DistanceKM, kind, speed)
	now := time.Now()
	a.EstimatedRestore = now.Add(time.Duration(a.ETAMinutes+a.RepairMinutes) * time.Minute)
	err = tx.QueryRow(ctx, `INSERT INTO crew_assignments (outage_id, crew_id, distance_km, eta_minutes, repair_minutes, assigned_by)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,0)) RETURNING id, assigned_at`,
		outageID, a.CrewID, a.DistanceKM, a.ETAMinutes, a.RepairMinutes, by).Scan(&a.ID, &a.AssignedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			assignments.Inc("conflict")
			return nil, ErrNoCrew
		}
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE crews SET status = 'busy', updated_at = now() WHERE id = $1`, a.CrewID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET estimated_restore_at = $2 WHERE id = $1`, outageID, a.EstimatedRestore); err != nil {
		return nil, err
	}
	if _, err := grid.TransitionTx(ctx, tx, outageID, "assigned", by, "ekip atandı: "+a.CrewCode, -1); err != nil {
		return nil, err
	}
	assignments.Inc("assigned")
	return a, audit.Record(ctx, tx, audit.Entry{Action: "crew.assign", Entity: "outage", EntityID: strconv.FormatInt(outageID, 10), New: a})
}

// Dispatch runs DispatchTx in its own transaction.
func Dispatch(ctx context.Context, pool *pgxpool.Pool, outageID, by int64) (*Assignment, error) {
	var a *Assignment
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		a, err = DispatchTx(ctx, tx, outageID, by)
		return err
	})
	return a, err
}

// ReleaseTx takes the crew off an assigned outage (back to confirmed).
func ReleaseTx(ctx context.Context, tx pgx.Tx, outageID, by int64, note string) error {
	if _, err := tx.Exec(ctx, `WITH a AS (
			UPDATE crew_assignments SET status = 'released', closed_at = now()
			WHERE outage_id = $1 AND status = 'active' RETURNING crew_id)
		UPDATE crews SET status = 'available', updated_at = now() WHERE id IN (SELECT crew_id FROM a)`, outageID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET estimated_restore_at = NULL WHERE id = $1`, outageID); err != nil {
		return err
	}
	_, err := grid.TransitionTx(ctx, tx, outageID, "confirmed", by, note, -1)
	return err
}

// AutoDispatch assigns crews to confirmed outages that have none, most
// customers affected first. Used by the background worker.
func AutoDispatch(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	rows, err := pool.Query(ctx, `SELECT id FROM outages WHERE status = 'confirmed' AND kind = 'unplanned'
		ORDER BY affected_count DESC, started_at LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		_, err := Dispatch(ctx, pool, id, 0)
		switch {
		case err == nil:
			n++
		case errors.Is(err, ErrNoCrew):
			return n, nil // boş ekip kalmadı
		case errors.Is(err, ErrNotDispatchable):
		default:
			return n, err
		}
	}
	return n, nil
}

// CrewTransitionTx lets a field crew move *its own* assigned outage to
// in_progress or resolved; anything else is forbidden.
func CrewTransitionTx(ctx context.Context, tx pgx.Tx, crewID int64, to string, by int64, note string) (*grid.Outage, error) {
	if to != "in_progress" && to != "resolved" {
		return nil, grid.ErrBadTransition
	}
	var outageID int64
	err := tx.QueryRow(ctx, `SELECT outage_id FROM crew_assignments WHERE crew_id = $1 AND status = 'active'`, crewID).Scan(&outageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoAssignment
	}
	if err != nil {
		return nil, err
	}
	return grid.TransitionTx(ctx, tx, outageID, to, by, note, -1)
}
