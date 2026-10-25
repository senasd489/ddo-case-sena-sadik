// Package metering implements T-1: ingesting cumulative 15-minute meter
// readings (with duplicates, gaps, meter changes, resets and back-dated
// corrections) and turning them into per-slot consumption.
package metering

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

// Slot is the reading interval.
const Slot = 15 * time.Minute

// MaxBatch bounds a single ingest call.
const MaxBatch = 50_000

// Reading is one cumulative register value sent by a meter.
type Reading struct {
	MeterID      int64     `json:"meter_id"`
	ReadAt       time.Time `json:"read_at"`
	ActiveWh     int64     `json:"active_index_wh"`
	ReactiveVarh int64     `json:"reactive_index_varh"`
}

// IngestResult summarises what happened to a batch.
type IngestResult struct {
	BatchID     int64 `json:"batch_id"`
	Received    int   `json:"received"`
	Inserted    int   `json:"inserted"`
	Duplicates  int   `json:"duplicates"`
	Corrections int   `json:"corrections"`
	Rejected    int   `json:"rejected"`
	// Rejections lists the first few invalid rows with a reason.
	Rejections []Rejection `json:"rejections,omitempty"`
	DurationMS int64       `json:"duration_ms"`
}

type Rejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

var (
	readingsIngested = metrics.NewCounter("enerjihat_readings_total", "Okuma alma sonuçları", "outcome")
	partMu           sync.Mutex
	partDone         = map[string]bool{}
)

// ensurePartitions creates daily readings/consumption partitions covering
// [from, to] (UTC dates). Results are cached per process.
func ensurePartitions(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) error {
	from, to = from.UTC().Truncate(24*time.Hour), to.UTC().Truncate(24*time.Hour)
	key := fmt.Sprintf("%p:%s-%s", pool, from.Format("20060102"), to.Format("20060102"))
	partMu.Lock()
	done := partDone[key]
	partMu.Unlock()
	if done {
		return nil
	}
	if _, err := pool.Exec(ctx, `SELECT ensure_reading_partitions($1::date, $2::date)`, from, to); err != nil {
		return fmt.Errorf("ensure partitions: %w", err)
	}
	partMu.Lock()
	partDone[key] = true
	partMu.Unlock()
	return nil
}

// Validate checks a reading in isolation; meter existence and installation
// window are checked set-based in SQL.
func Validate(r Reading, now time.Time) string {
	switch {
	case r.MeterID <= 0:
		return "meter_id gerekli"
	case r.ReadAt.IsZero():
		return "read_at gerekli"
	case r.ReadAt.Unix()%int64(Slot/time.Second) != 0 || r.ReadAt.Nanosecond() != 0:
		return "read_at 15 dakikalık sınıra hizalı olmalı (:00, :15, :30, :45)"
	case r.ReadAt.After(now.Add(time.Hour)):
		return "read_at gelecekte olamaz"
	case r.ActiveWh < 0 || r.ReactiveVarh < 0:
		return "endeks negatif olamaz"
	}
	return ""
}

// Ingest stores a batch. Rows are COPY'd into a temp table and merged with a
// single INSERT … ON CONFLICT:
//   - same (meter, read_at) with the same value → duplicate, ignored
//   - same (meter, read_at) with a different value → correction: the old value
//     goes to reading_corrections and revision is incremented
//   - every changed meter-day (plus days bridged by a filled gap) is queued in
//     consumption_dirty for recomputation.
func Ingest(ctx context.Context, pool *pgxpool.Pool, source string, rows []Reading) (IngestResult, error) {
	res, err := ingestOnce(ctx, pool, source, rows)
	var pgErr *pgconn.PgError
	for attempt := 0; attempt < 3 && errors.As(err, &pgErr) && (pgErr.Code == "40P01" || pgErr.Code == "40001"); attempt++ {
		// Deadlock / serileştirme hatası: işlem tamamen geri alındı, güvenle tekrar denenir.
		time.Sleep(time.Duration(10*(attempt+1)) * time.Millisecond)
		res, err = ingestOnce(ctx, pool, source, rows)
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && strings.Contains(pgErr.Message, "no partition") {
		// Önbellekteki partisyon bilgisi eskimiş (ör. reset-demo sonrası): temizle, bir kez tekrar dene.
		partMu.Lock()
		partDone = map[string]bool{}
		partMu.Unlock()
		res, err = ingestOnce(ctx, pool, source, rows)
	}
	return res, err
}

func ingestOnce(ctx context.Context, pool *pgxpool.Pool, source string, rows []Reading) (IngestResult, error) {
	start := time.Now()
	res := IngestResult{Received: len(rows)}
	if len(rows) == 0 {
		return res, nil
	}
	now := time.Now()
	valid := make([][]any, 0, len(rows))
	minT, maxT := rows[0].ReadAt, rows[0].ReadAt
	for i, r := range rows {
		if reason := Validate(r, now); reason != "" {
			res.Rejected++
			if len(res.Rejections) < 20 {
				res.Rejections = append(res.Rejections, Rejection{Index: i, Reason: reason})
			}
			continue
		}
		if r.ReadAt.Before(minT) {
			minT = r.ReadAt
		}
		if r.ReadAt.After(maxT) {
			maxT = r.ReadAt
		}
		valid = append(valid, []any{r.MeterID, r.ReadAt.UTC(), r.ActiveWh, r.ReactiveVarh, i})
	}
	if len(valid) == 0 {
		return res, nil
	}
	// Yerel gün UTC'de iki güne yayıldığı için bir gün öncesi de oluşturulur.
	if err := ensurePartitions(ctx, pool, minT.Add(-24*time.Hour), maxT.Add(24*time.Hour)); err != nil {
		return res, err
	}

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE stage (meter_id bigint, read_at timestamptz, a bigint, q bigint, ord int) ON COMMIT DROP`); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"stage"}, []string{"meter_id", "read_at", "a", "q", "ord"}, pgx.CopyFromRows(valid)); err != nil {
			return err
		}
		// Parti içi tekrarlar: aynı anahtar için son gelen kazanır.
		// Sayaç yoksa veya okuma sayacın kurulu olduğu aralık dışındaysa reddedilir.
		var staged, stagedDistinct, accepted int
		if _, err := tx.Exec(ctx, `
			CREATE TEMP TABLE s ON COMMIT DROP AS
			SELECT DISTINCT ON (st.meter_id, st.read_at) st.meter_id, st.read_at, st.a, st.q
			FROM stage st
			JOIN meters m ON m.id = st.meter_id
			 AND st.read_at >= m.installed_at
			 AND (m.removed_at IS NULL OR st.read_at <= m.removed_at)
			ORDER BY st.meter_id, st.read_at, st.ord DESC`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM stage),
				(SELECT count(*) FROM (SELECT DISTINCT meter_id, read_at FROM stage) x),
				(SELECT count(*) FROM s)`).Scan(&staged, &stagedDistinct, &accepted); err != nil {
			return err
		}
		res.Rejected += stagedDistinct - accepted
		if stagedDistinct-accepted > 0 && len(res.Rejections) < 20 {
			res.Rejections = append(res.Rejections, Rejection{Index: -1,
				Reason: fmt.Sprintf("%d okuma bilinmeyen sayaç veya sayacın kurulu olmadığı zaman için", stagedDistinct-accepted)})
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO reading_corrections (meter_id, read_at, old_index_wh, new_index_wh)
			SELECT s.meter_id, s.read_at, r.active_index_wh, s.a
			FROM s JOIN readings r ON r.meter_id = s.meter_id AND r.read_at = s.read_at
			WHERE (r.active_index_wh, r.reactive_index_varh) IS DISTINCT FROM (s.a, s.q)`)
		if err != nil {
			return err
		}
		res.Corrections = int(tag.RowsAffected())

		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE changed ON COMMIT DROP AS
			WITH up AS (
				INSERT INTO readings (meter_id, read_at, active_index_wh, reactive_index_varh)
				SELECT meter_id, read_at, a, q FROM s
				ON CONFLICT (meter_id, read_at) DO UPDATE
				   SET active_index_wh = EXCLUDED.active_index_wh,
				       reactive_index_varh = EXCLUDED.reactive_index_varh,
				       revision = readings.revision + 1,
				       received_at = now()
				 WHERE (readings.active_index_wh, readings.reactive_index_varh)
				       IS DISTINCT FROM (EXCLUDED.active_index_wh, EXCLUDED.reactive_index_varh)
				RETURNING meter_id, read_at)
			SELECT * FROM up`); err != nil {
			return err
		}
		var changed int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM changed`).Scan(&changed); err != nil {
			return err
		}
		res.Inserted = changed - res.Corrections
		res.Duplicates = (staged - stagedDistinct) + (accepted - changed)

		if _, err := tx.Exec(ctx, markDirtySQL); err != nil {
			return fmt.Errorf("mark dirty: %w", err)
		}
		return tx.QueryRow(ctx, `INSERT INTO ingest_batches (source, received, inserted, duplicates, corrections, rejected, min_read_at, max_read_at, duration_ms)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			source, res.Received, res.Inserted, res.Duplicates, res.Corrections, res.Rejected, minT, maxT,
			time.Since(start).Milliseconds()).Scan(&res.BatchID)
	})
	if err != nil {
		return res, err
	}
	res.DurationMS = time.Since(start).Milliseconds()
	readingsIngested.Add(float64(res.Inserted), "inserted")
	readingsIngested.Add(float64(res.Duplicates), "duplicate")
	readingsIngested.Add(float64(res.Corrections), "correction")
	readingsIngested.Add(float64(res.Rejected), "rejected")
	return res, nil
}

// markDirtySQL queues every local day whose consumption may have changed:
//   - the day of the slot that starts at read_at and of the slot that ends there
//   - when the new reading fills a gap, all days between it and the nearest
//     existing reading on either side (their interpolation changes too).
const markDirtySQL = `
INSERT INTO consumption_dirty (meter_id, day)
SELECT meter_id, d FROM (
    SELECT meter_id, (read_at AT TIME ZONE 'Europe/Istanbul')::date AS d FROM changed
    UNION ALL
    SELECT meter_id, ((read_at - interval '15 minutes') AT TIME ZONE 'Europe/Istanbul')::date FROM changed
    UNION ALL
    SELECT g.meter_id, gs::date
    FROM (SELECT meter_id, min(read_at) AS first_new FROM changed GROUP BY meter_id) g
    CROSS JOIN LATERAL (SELECT r.read_at FROM readings r
                        WHERE r.meter_id = g.meter_id AND r.read_at < g.first_new
                        ORDER BY r.read_at DESC LIMIT 1) p
    CROSS JOIN LATERAL generate_series((p.read_at AT TIME ZONE 'Europe/Istanbul')::date,
                                       (g.first_new AT TIME ZONE 'Europe/Istanbul')::date, interval '1 day') gs
    WHERE g.first_new - p.read_at > interval '15 minutes' AND g.first_new - p.read_at < interval '31 days'
    UNION ALL
    SELECT g.meter_id, gs::date
    FROM (SELECT meter_id, max(read_at) AS last_new FROM changed GROUP BY meter_id) g
    CROSS JOIN LATERAL (SELECT r.read_at FROM readings r
                        WHERE r.meter_id = g.meter_id AND r.read_at > g.last_new
                        ORDER BY r.read_at LIMIT 1) n
    CROSS JOIN LATERAL generate_series((g.last_new AT TIME ZONE 'Europe/Istanbul')::date,
                                       (n.read_at AT TIME ZONE 'Europe/Istanbul')::date, interval '1 day') gs
    WHERE n.read_at - g.last_new > interval '15 minutes' AND n.read_at - g.last_new < interval '31 days'
) x
GROUP BY meter_id, d
-- Sabit sıra: eşzamanlı partiler aynı satırları aynı sırada kilitler (deadlock olmaz).
ORDER BY meter_id, d
ON CONFLICT (meter_id, day) DO NOTHING`
