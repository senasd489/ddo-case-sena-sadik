package metering

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

// MeterDay identifies one meter on one local (Europe/Istanbul) calendar day.
type MeterDay struct {
	MeterID int64
	Day     time.Time
}

var meterDaysComputed = metrics.NewCounter("enerjihat_consumption_meter_days_total", "Yeniden hesaplanan sayaç-gün sayısı")

// consumptionSQL computes the 96 quarter-hour consumptions of every
// (meter, day) in the job arrays. All the logic is one set-based query:
//
//  1. known  – the real readings inside the day (boundaries inclusive), the
//     nearest reading before and after the day (anchors for gaps that cross
//     midnight), plus synthetic points for a meter's installation (opening
//     index) and removal (closing index) so a meter change never produces a
//     bogus delta;
//  2. pairs  – LAG() over the known points gives each point its predecessor;
//  3. seg    – the delta between consecutive known points; if the register
//     fell below half of the previous value the meter was reset and the new
//     value is the consumption since the reset; a smaller backwards step is a
//     data error and counts as 0 (flagged 'corrected');
//  4. slots  – a segment spanning k quarter-hours is split into k slots with
//     exact integer division (floor(d·i/k) − floor(d·(i−1)/k)), so the slots
//     always sum to the measured delta; k>1 slots are flagged interpolated.
//
// Slots after the last known reading are simply absent (not yet measured).
const consumptionSQL = `
WITH job AS (
    SELECT j.meter_id, j.day, m.subscriber_id, m.installed_at, m.removed_at,
           m.initial_index_wh, m.initial_reactive_varh, m.final_index_wh, m.final_reactive_varh,
           (j.day::timestamp AT TIME ZONE 'Europe/Istanbul')       AS d0,
           ((j.day + 1)::timestamp AT TIME ZONE 'Europe/Istanbul') AS d1
    FROM unnest($1::bigint[], $2::date[]) AS j(meter_id, day)
    JOIN meters m ON m.id = j.meter_id
),
known AS (
    SELECT j.meter_id, j.day, r.read_at AS ts, r.active_index_wh AS a, r.reactive_index_varh AS q,
           r.revision, 'reading'::text AS src
    FROM job j
    JOIN readings r ON r.meter_id = j.meter_id AND r.read_at BETWEEN j.d0 AND j.d1
     AND r.read_at BETWEEN $3 AND $4
    UNION ALL
    SELECT j.meter_id, j.day, p.read_at, p.active_index_wh, p.reactive_index_varh, p.revision, 'reading'
    FROM job j
    CROSS JOIN LATERAL (SELECT r.read_at, r.active_index_wh, r.reactive_index_varh, r.revision
                        FROM readings r
                        WHERE r.meter_id = j.meter_id AND r.read_at < j.d0
                          AND r.read_at >= j.d0 - interval '31 days'
                          AND r.read_at >= $3::timestamptz - interval '31 days'
                        ORDER BY r.read_at DESC LIMIT 1) p
    UNION ALL
    SELECT j.meter_id, j.day, n.read_at, n.active_index_wh, n.reactive_index_varh, n.revision, 'reading'
    FROM job j
    CROSS JOIN LATERAL (SELECT r.read_at, r.active_index_wh, r.reactive_index_varh, r.revision
                        FROM readings r
                        WHERE r.meter_id = j.meter_id AND r.read_at > j.d1
                          AND r.read_at <= j.d1 + interval '31 days'
                          AND r.read_at <= $4::timestamptz + interval '31 days'
                        ORDER BY r.read_at LIMIT 1) n
    UNION ALL
    SELECT j.meter_id, j.day, j.installed_at, j.initial_index_wh, j.initial_reactive_varh, 0, 'install'
    FROM job j
    WHERE j.installed_at > j.d0 - interval '31 days' AND j.installed_at <= j.d1
      AND NOT EXISTS (SELECT 1 FROM readings r WHERE r.meter_id = j.meter_id AND r.read_at = j.installed_at)
    UNION ALL
    SELECT j.meter_id, j.day, j.removed_at, j.final_index_wh, j.final_reactive_varh, 0, 'remove'
    FROM job j
    WHERE j.removed_at IS NOT NULL AND j.final_index_wh IS NOT NULL
      AND j.removed_at >= j.d0 AND j.removed_at < j.d1 + interval '31 days'
      AND NOT EXISTS (SELECT 1 FROM readings r WHERE r.meter_id = j.meter_id AND r.read_at = j.removed_at)
),
pairs AS (
    SELECT meter_id, day, ts AS t1, a AS a1, q AS q1, revision AS rev1, src AS src1,
           lag(ts)       OVER w AS t0,
           lag(a)        OVER w AS a0,
           lag(q)        OVER w AS q0,
           lag(revision) OVER w AS rev0,
           lag(src)      OVER w AS src0
    FROM known
    WINDOW w AS (PARTITION BY meter_id, day ORDER BY ts)
),
seg AS (
    SELECT p.*,
           (extract(epoch FROM p.t1 - p.t0) / 900)::int        AS k,
           -- Endeks yarıdan fazla düştüyse sayaç sıfırlanmıştır; küçük bir geri
           -- düşüş ise veri hatasıdır (tüketim 0 sayılır, slot 'corrected' işaretlenir).
           (p.a1 < p.a0 AND p.a1 < p.a0 / 2)                   AS is_reset,
           (p.a1 < p.a0 AND p.a1 >= p.a0 / 2)                  AS is_glitch,
           CASE WHEN p.a1 >= p.a0 THEN p.a1 - p.a0
                WHEN p.a1 < p.a0 / 2 THEN p.a1
                ELSE 0 END                                     AS da,
           CASE WHEN p.q1 IS NULL OR p.q0 IS NULL THEN 0
                WHEN p.q1 >= p.q0 THEN p.q1 - p.q0 ELSE p.q1 END  AS dq
    FROM pairs p
    WHERE p.t0 IS NOT NULL AND p.t1 > p.t0
),
slots AS (
    SELECT s.meter_id, s.day,
           s.t0 + (i - 1) * interval '15 minutes'            AS slot_start,
           (s.da * i) / s.k - (s.da * (i - 1)) / s.k          AS wh,
           (s.dq * i) / s.k - (s.dq * (i - 1)) / s.k          AS varh,
           CASE WHEN s.is_reset                                        THEN 'reset'
                WHEN s.src0 IN ('install','remove') OR s.src1 IN ('install','remove') THEN 'meter_change'
                WHEN s.is_glitch                                       THEN 'corrected'
                WHEN s.k > 1                                           THEN 'interpolated'
                WHEN s.rev0 > 0 OR s.rev1 > 0                          THEN 'corrected'
                ELSE 'measured' END                          AS quality
    FROM seg s
    CROSS JOIN LATERAL generate_series(1, s.k) AS i
    WHERE s.k > 0
)
SELECT sl.meter_id, j.subscriber_id, sl.slot_start, sl.wh, sl.varh, sl.quality
FROM slots sl
JOIN job j ON j.meter_id = sl.meter_id AND j.day = sl.day
WHERE sl.slot_start >= j.d0 AND sl.slot_start < j.d1
  AND sl.slot_start >= j.installed_at
  AND (j.removed_at IS NULL OR sl.slot_start < j.removed_at)`

const deleteSlotsSQL = `
DELETE FROM consumption_15m c
USING unnest($1::bigint[], $2::date[]) AS j(meter_id, day)
WHERE c.slot_start >= $3 AND c.slot_start < $4
  AND c.meter_id = j.meter_id
  AND c.slot_start >= (j.day::timestamp AT TIME ZONE 'Europe/Istanbul')
  AND c.slot_start <  ((j.day + 1)::timestamp AT TIME ZONE 'Europe/Istanbul')`

// Zaman dilimleri (yerel saat): gündüz 06–17, puant 17–22, gece 22–06.
const dailySQL = `
INSERT INTO consumption_daily (meter_id, subscriber_id, day, active_wh, day_wh, peak_wh, night_wh,
                               reactive_varh, max_demand_w, slots, interpolated_slots)
SELECT c.meter_id, max(c.subscriber_id), j.day,
       sum(c.active_wh),
       coalesce(sum(c.active_wh) FILTER (WHERE h >= 6  AND h < 17), 0),
       coalesce(sum(c.active_wh) FILTER (WHERE h >= 17 AND h < 22), 0),
       coalesce(sum(c.active_wh) FILTER (WHERE h >= 22 OR  h < 6), 0),
       sum(c.reactive_varh),
       max(c.active_wh) * 4,
       count(*),
       count(*) FILTER (WHERE c.quality = 'interpolated')
FROM unnest($1::bigint[], $2::date[]) AS j(meter_id, day)
JOIN consumption_15m c ON c.meter_id = j.meter_id
 AND c.slot_start >= $3 AND c.slot_start < $4
 AND c.slot_start >= (j.day::timestamp AT TIME ZONE 'Europe/Istanbul')
 AND c.slot_start <  ((j.day + 1)::timestamp AT TIME ZONE 'Europe/Istanbul')
CROSS JOIN LATERAL (SELECT extract(hour FROM c.slot_start AT TIME ZONE 'Europe/Istanbul')::int AS h) hh
GROUP BY c.meter_id, j.day`

// Recompute rebuilds consumption_15m and consumption_daily for the given
// meter-days inside tx. It is idempotent: running it twice yields the same rows.
func Recompute(ctx context.Context, tx pgx.Tx, days []MeterDay) error {
	if len(days) == 0 {
		return nil
	}
	ids := make([]int64, len(days))
	ds := make([]time.Time, len(days))
	minD, maxD := days[0].Day, days[0].Day
	for i, d := range days {
		ids[i], ds[i] = d.MeterID, d.Day
		if d.Day.Before(minD) {
			minD = d.Day
		}
		if d.Day.After(maxD) {
			maxD = d.Day
		}
	}
	if _, err := tx.Exec(ctx, `SELECT ensure_reading_partitions($1::date - 1, $2::date)`, minD, maxD); err != nil {
		return err
	}
	// Sabit sınırlar planlama anında partisyon budamasını sağlar.
	lo, hi := minD.Add(-24*time.Hour), maxD.Add(48*time.Hour)
	if _, err := tx.Exec(ctx, deleteSlotsSQL, ids, ds, lo, hi); err != nil {
		return fmt.Errorf("delete slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO consumption_15m (meter_id, subscriber_id, slot_start, active_wh, reactive_varh, quality) `+consumptionSQL, ids, ds, lo, hi); err != nil {
		return fmt.Errorf("compute slots: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM consumption_daily d USING unnest($1::bigint[], $2::date[]) AS j(meter_id, day)
		WHERE d.meter_id = j.meter_id AND d.day = j.day`, ids, ds); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, dailySQL, ids, ds, lo, hi); err != nil {
		return fmt.Errorf("daily rollup: %w", err)
	}
	meterDaysComputed.Add(float64(len(days)))
	return nil
}

// ProcessDirty claims up to limit queued meter-days and recomputes them.
// It uses three short transactions so ingestion never waits for a long
// recomputation:
//  1. claim: UPDATE … SET claimed_at = now() on rows picked with FOR UPDATE
//     SKIP LOCKED (concurrent workers take disjoint rows);
//  2. compute: Recompute under a per-meter advisory lock;
//  3. finish: delete the rows unless ingestion re-marked them meanwhile
//     (marked_at > claimed_at) — those are picked up again.
//
// Claims older than five minutes (crashed worker) are taken over.
func ProcessDirty(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	return processDirty(ctx, pool, limit, nil)
}

// ProcessDirtyFor is ProcessDirty restricted to the given meters (used by
// the admin "recompute now" endpoint so it never waits for a large backlog).
func ProcessDirtyFor(ctx context.Context, pool *pgxpool.Pool, limit int, meters []int64) (int, error) {
	return processDirty(ctx, pool, limit, meters)
}

func processDirty(ctx context.Context, pool *pgxpool.Pool, limit int, meters []int64) (int, error) {
	rows, err := pool.Query(ctx, `
		UPDATE consumption_dirty d SET claimed_at = now()
		FROM (SELECT meter_id, day FROM consumption_dirty
		      WHERE (claimed_at IS NULL OR marked_at > claimed_at OR claimed_at < now() - interval '5 minutes')
		        AND ($2::bigint[] IS NULL OR meter_id = ANY($2))
		      ORDER BY day, meter_id LIMIT $1 FOR UPDATE SKIP LOCKED) p
		WHERE d.meter_id = p.meter_id AND d.day = p.day
		RETURNING d.meter_id, d.day, d.claimed_at`, limit, meters)
	if err != nil {
		return 0, err
	}
	var days []MeterDay
	var claimed []time.Time
	for rows.Next() {
		var md MeterDay
		var c time.Time
		if err := rows.Scan(&md.MeterID, &md.Day, &c); err != nil {
			rows.Close()
			return 0, err
		}
		days = append(days, md)
		claimed = append(claimed, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(days) == 0 {
		return 0, err
	}
	sort.Slice(days, func(i, j int) bool { return days[i].MeterID < days[j].MeterID })
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Aynı sayacı iki işçi aynı anda hesaplamasın (yeniden işaretlenen satırlar).
		prev := int64(-1)
		for _, d := range days {
			if d.MeterID != prev {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(5151, $1::int)`, d.MeterID); err != nil {
					return err
				}
				prev = d.MeterID
			}
		}
		return Recompute(ctx, tx, days)
	})
	if err != nil {
		// Talep geri bırakılır; bir sonraki turda tekrar denenir.
		_, _ = pool.Exec(context.WithoutCancel(ctx), `UPDATE consumption_dirty SET claimed_at = NULL
			WHERE (meter_id, day) IN (SELECT * FROM unnest($1::bigint[], $2::date[]))`, ids(days), dates(days))
		return 0, err
	}
	_, err = pool.Exec(ctx, `DELETE FROM consumption_dirty d
		USING unnest($1::bigint[], $2::date[]) AS j(meter_id, day)
		WHERE d.meter_id = j.meter_id AND d.day = j.day AND d.marked_at <= d.claimed_at`, ids(days), dates(days))
	_ = claimed
	return len(days), err
}

func ids(ds []MeterDay) []int64 {
	out := make([]int64, len(ds))
	for i, d := range ds {
		out[i] = d.MeterID
	}
	return out
}

func dates(ds []MeterDay) []time.Time {
	out := make([]time.Time, len(ds))
	for i, d := range ds {
		out[i] = d.Day
	}
	return out
}

// DrainDirty processes the queue until it is empty (used by tests, verify and
// the admin "recompute now" endpoint).
func DrainDirty(ctx context.Context, pool *pgxpool.Pool, batch int) (int, error) {
	total := 0
	for {
		n, err := ProcessDirty(ctx, pool, batch)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}
