// Package dispatch implements T-5: clustering customer calls and meter
// "last gasp" signals into outage events, and assigning the nearest
// qualified crew without ever double-booking one.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/grid"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

// ClusterWindow: a signal joins an open outage on the same feeder that
// started within this window, or any open outage that already lists the
// signalling subscriber as affected.
const ClusterWindow = 15 * time.Minute

// AutoConfirmSubscribers: distinct reporting subscribers needed to confirm.
const AutoConfirmSubscribers = 3

var (
	ErrUnknownSource   = errors.New("source must be call or last_gasp")
	ErrUnknownCustomer = errors.New("subscriber or meter not found")
)

var signalsTotal = metrics.NewCounter("enerjihat_outage_signals_total", "Kesinti bildirimleri", "source", "outcome")

type Signal struct {
	Source       string    `json:"source"` // call | last_gasp
	ExternalID   string    `json:"external_id"`
	SubscriberID int64     `json:"subscriber_id"`
	MeterID      int64     `json:"meter_id"`
	ReceivedAt   time.Time `json:"received_at"`
	Note         string    `json:"note"`
}

type SignalResult struct {
	SignalID  int64  `json:"signal_id"`
	OutageID  int64  `json:"outage_id"`
	Outcome   string `json:"outcome"` // new_outage | attached | duplicate | replayed
	Duplicate bool   `json:"duplicate"`
	FaultNode int64  `json:"fault_node_id"`
	Status    string `json:"outage_status"`
	Reporters int    `json:"distinct_reporters"`
}

// Ingest processes one signal in its own transaction. Signals of the same
// feeder are serialised with an advisory lock so two simultaneous last-gasp
// messages can never open two separate outages for one fault.
func Ingest(ctx context.Context, pool *pgxpool.Pool, s Signal) (*SignalResult, error) {
	var res *SignalResult
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		res, err = ingestTx(ctx, tx, s)
		return err
	})
	if err == nil {
		signalsTotal.Inc(s.Source, res.Outcome)
	}
	return res, err
}

func ingestTx(ctx context.Context, tx pgx.Tx, s Signal) (*SignalResult, error) {
	if s.Source != "call" && s.Source != "last_gasp" && s.Source != "restore" {
		return nil, ErrUnknownSource
	}
	if s.ReceivedAt.IsZero() {
		s.ReceivedAt = time.Now()
	}
	// Abone ↔ sayaç çözümü.
	var pole int64
	var meterID *int64
	if s.SubscriberID == 0 && s.MeterID > 0 {
		if err := tx.QueryRow(ctx, `SELECT subscriber_id FROM meters WHERE id = $1 AND kind = 'subscriber'`, s.MeterID).Scan(&s.SubscriberID); err != nil {
			return nil, ErrUnknownCustomer
		}
	}
	if s.MeterID > 0 {
		meterID = &s.MeterID
	}
	if err := tx.QueryRow(ctx, `SELECT pole_id FROM subscribers WHERE id = $1`, s.SubscriberID).Scan(&pole); err != nil {
		return nil, ErrUnknownCustomer
	}
	lin, err := grid.LineageOf(ctx, tx, pole)
	if err != nil {
		return nil, err
	}
	if s.ExternalID == "" {
		s.ExternalID = fmt.Sprintf("%s-%d-%d", s.Source, s.SubscriberID, s.ReceivedAt.UnixNano())
	}
	var sigID int64
	err = tx.QueryRow(ctx, `INSERT INTO outage_signals (source, external_id, subscriber_id, meter_id, feeder_id, pole_id, received_at, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')) ON CONFLICT (source, external_id) DO NOTHING RETURNING id`,
		s.Source, s.ExternalID, s.SubscriberID, meterID, lin.FeederID, pole, s.ReceivedAt, s.Note).Scan(&sigID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Aynı mesaj ikinci kez geldi (ör. yeniden gönderim): ilk sonucu döndür.
		var outage *int64
		_ = tx.QueryRow(ctx, `SELECT id, outage_id FROM outage_signals WHERE source = $1 AND external_id = $2`, s.Source, s.ExternalID).Scan(&sigID, &outage)
		r := &SignalResult{SignalID: sigID, Outcome: "replayed", Duplicate: true}
		if outage != nil {
			r.OutageID = *outage
		}
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4444, $1::int)`, lin.FeederID); err != nil {
		return nil, err
	}
	if s.Source == "restore" {
		return restoreTx(ctx, tx, sigID, s)
	}

	// Aynı besleyicide açık kesinti: ya zaman penceresinde başlamış ya da bu
	// aboneyi zaten etkiliyor.
	var outageID int64
	err = tx.QueryRow(ctx, `SELECT o.id FROM outages o
		WHERE o.feeder_id = $1 AND o.kind = 'unplanned' AND o.status IN ('detected','confirmed','assigned','in_progress')
		  AND (o.started_at BETWEEN $2::timestamptz - $4::interval AND $2::timestamptz + $4::interval
		       OR EXISTS (SELECT 1 FROM outage_affected a WHERE a.outage_id = o.id AND a.subscriber_id = $3 AND a.restored_at IS NULL))
		ORDER BY o.started_at DESC LIMIT 1 FOR UPDATE`,
		lin.FeederID, s.ReceivedAt, s.SubscriberID, fmt.Sprintf("%d seconds", int(ClusterWindow.Seconds()))).Scan(&outageID)
	res := &SignalResult{SignalID: sigID}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		o, err := grid.CreateOutageTx(ctx, tx, grid.CreateOutage{Kind: "unplanned", FaultNodeID: pole, StartedAt: &s.ReceivedAt,
			Cause: "otomatik tespit: " + s.Source})
		if err != nil {
			return nil, err
		}
		outageID = o.ID
		res.Outcome = "new_outage"
	case err != nil:
		return nil, err
	default:
		res.Outcome = "attached"
	}
	// Bu abone bu kesinti için daha önce bildirimde bulunduysa mükerrer.
	var dup bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM outage_signals WHERE outage_id = $1 AND subscriber_id = $2 AND id <> $3)`,
		outageID, s.SubscriberID, sigID).Scan(&dup); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE outage_signals SET outage_id = $2, duplicate = $3 WHERE id = $1`, sigID, outageID, dup); err != nil {
		return nil, err
	}
	res.Duplicate = dup
	if dup {
		res.Outcome = "duplicate"
	}
	if err := refreshOutage(ctx, tx, outageID, res); err != nil {
		return nil, err
	}
	res.OutageID = outageID
	return res, nil
}

// refreshOutage recounts reports, re-localises the fault to the lowest
// common ancestor of all reporting poles (while the outage is still only
// detected), and auto-confirms once enough distinct customers reported.
func refreshOutage(ctx context.Context, tx pgx.Tx, outageID int64, res *SignalResult) error {
	o, err := grid.GetOutage(ctx, tx, outageID, true)
	if err != nil {
		return err
	}
	var reporters int
	var poles []int64
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT subscriber_id), array_agg(DISTINCT pole_id) FROM outage_signals WHERE outage_id = $1`,
		outageID).Scan(&reporters, &poles); err != nil {
		return err
	}
	var started time.Time
	if err := tx.QueryRow(ctx, `SELECT least($2::timestamptz, min(received_at)) FROM outage_signals WHERE outage_id = $1`, outageID, o.StartedAt).Scan(&started); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET report_count = (SELECT count(*) FROM outage_signals WHERE outage_id = $1 AND NOT duplicate),
		started_at = $2, updated_at = now() WHERE id = $1`, outageID, started); err != nil {
		return err
	}
	res.Reporters = reporters
	res.FaultNode = o.FaultNodeID
	res.Status = o.Status
	if o.Status == "detected" && len(poles) > 0 {
		lca, err := LowestCommonAncestor(ctx, tx, poles)
		if err != nil {
			return err
		}
		if lca != o.FaultNodeID {
			if err := relocate(ctx, tx, o, lca, started); err != nil {
				return err
			}
			res.FaultNode = lca
		}
	}
	if o.Status == "detected" && reporters >= AutoConfirmSubscribers {
		if _, err := grid.TransitionTx(ctx, tx, outageID, "confirmed", 0,
			fmt.Sprintf("otomatik onay: %d farklı abone bildirdi", reporters), -1); err != nil {
			return err
		}
		res.Status = "confirmed"
	}
	return nil
}

// relocate moves the estimated fault point and rebuilds the affected set.
func relocate(ctx context.Context, tx pgx.Tx, o *grid.Outage, node int64, started time.Time) error {
	ids, err := grid.AffectedSubscribers(ctx, tx, node)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM outage_affected WHERE outage_id = $1 AND NOT (subscriber_id = ANY($2))`, o.ID, ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outage_affected (outage_id, subscriber_id, interrupted_at)
		SELECT $1, s, $3 FROM unnest($2::bigint[]) s ON CONFLICT DO NOTHING`, o.ID, ids, started); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE outage_affected SET interrupted_at = $2 WHERE outage_id = $1 AND interrupted_at > $2`, o.ID, started); err != nil {
		return err
	}
	l, err := grid.LineageOf(ctx, tx, node)
	if err != nil {
		return err
	}
	skill := o.RequiredSkill
	if l.Kind == "transformer" {
		skill = "transformer"
	} else if l.Kind == "feeder" && skill == "lv" {
		skill = "mv"
	}
	if _, err := tx.Exec(ctx, `UPDATE outages SET fault_node_id = $2, required_skill = $3, affected_count = $4 WHERE id = $1`,
		o.ID, node, skill, len(ids)); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{Action: "outage.relocate", Entity: "outage", EntityID: strconv.FormatInt(o.ID, 10),
		Old: map[string]any{"fault_node_id": o.FaultNodeID}, New: map[string]any{"fault_node_id": node, "affected": len(ids)}})
}

// LowestCommonAncestor returns the deepest node that lies on the supply path
// of every given pole (the most specific place where one fault explains
// every report).
func LowestCommonAncestor(ctx context.Context, tx pgx.Tx, poles []int64) (int64, error) {
	var common []int64 // ilk direğin yolu (direk → trafo)
	counts := map[int64]int{}
	for i, p := range poles {
		l, err := grid.LineageOf(ctx, tx, p)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			common = l.Path
		}
		for _, n := range l.Path {
			counts[n]++
		}
	}
	for _, n := range common {
		if counts[n] == len(poles) {
			return n, nil
		}
	}
	return common[len(common)-1], nil
}

// restoreTx handles a meter's "power restored" message: the subscriber's
// interruption ends at received_at, and when no affected subscriber is left
// without power the outage resolves itself (e.g. a recloser or a field
// switch restored supply before or while the crew worked).
func restoreTx(ctx context.Context, tx pgx.Tx, sigID int64, s Signal) (*SignalResult, error) {
	res := &SignalResult{SignalID: sigID, Outcome: "restore_ignored"}
	rows, err := tx.Query(ctx, `UPDATE outage_affected a SET restored_at = greatest($2, a.interrupted_at)
		FROM outages o
		WHERE o.id = a.outage_id AND a.subscriber_id = $1 AND a.restored_at IS NULL
		  AND o.status IN ('detected','confirmed','assigned','in_progress')
		RETURNING a.outage_id`, s.SubscriberID, s.ReceivedAt)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	for _, oid := range ids {
		res.OutageID, res.Outcome = oid, "restored"
		if _, err := tx.Exec(ctx, `UPDATE outage_signals SET outage_id = $2 WHERE id = $1`, sigID, oid); err != nil {
			return nil, err
		}
		var remaining int
		if err := tx.QueryRow(ctx, `UPDATE outages SET affected_count = (SELECT count(*) FROM outage_affected WHERE outage_id = $1 AND restored_at IS NULL),
			updated_at = now() WHERE id = $1 RETURNING affected_count`, oid).Scan(&remaining); err != nil {
			return nil, err
		}
		if remaining == 0 {
			o, err := grid.TransitionTx(ctx, tx, oid, "resolved", 0, "otomatik: tüm sayaçlardan enerji geldi bildirimi alındı", -1)
			if err != nil {
				return nil, err
			}
			res.Status = o.Status
		}
	}
	return res, nil
}

// ReconcileFromReadings treats a fresh register reading from an affected
// meter as proof that its supply is back (an AMI meter can only report when
// energised). It closes such interruptions and resolves outages that have
// nobody left without power. This keeps outages from staying open forever if
// a "power restored" message was lost (e.g. the head-end restarted).
func ReconcileFromReadings(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `
		UPDATE outage_affected a SET restored_at = r.read_at
		FROM outages o, meters m,
		     LATERAL (SELECT read_at FROM readings rd WHERE rd.meter_id = m.id AND rd.read_at > o.started_at + interval '15 minutes'
		              AND rd.received_at > o.started_at ORDER BY read_at LIMIT 1) r
		WHERE o.id = a.outage_id AND o.kind = 'unplanned' AND o.status IN ('detected','confirmed','assigned','in_progress')
		  AND a.restored_at IS NULL AND m.subscriber_id = a.subscriber_id AND m.removed_at IS NULL
		RETURNING a.outage_id`)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	seen := map[int64]bool{}
	resolved := 0
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var remaining int
			var status string
			if err := tx.QueryRow(ctx, `UPDATE outages SET affected_count = (SELECT count(*) FROM outage_affected WHERE outage_id = $1 AND restored_at IS NULL),
				updated_at = now() WHERE id = $1 RETURNING affected_count, status`, id).Scan(&remaining, &status); err != nil {
				return err
			}
			if remaining > 0 || !grid.CanTransition("unplanned", status, "resolved") {
				return nil
			}
			_, err := grid.TransitionTx(ctx, tx, id, "resolved", 0, "otomatik: etkilenen tüm sayaçlardan yeniden okuma geldi", -1)
			if err == nil {
				resolved++
			}
			return err
		})
		if err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}
