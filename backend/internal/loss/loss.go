// Package loss implements T-4: transformer energy balance (head meter vs.
// the sum of downstream subscriber meters, after technical-loss tolerance)
// and suspicious-subscriber detection with robust statistics.
//
// False alarms are reduced by five independent measures, each of which can
// be switched off (Options.Filters=false) to measure its effect:
//  1. median / MAD (robust z) instead of mean / standard deviation,
//  2. data-quality gating: days with missing or heavily interpolated data are
//     ignored, as are transformer days whose meters are < 98 % complete,
//  3. context exclusion: days with an outage on the subscriber or a meter
//     change do not count as "low consumption",
//  4. persistence: a transformer is lossy only if ≥ 3 of the recent days
//     exceed the tolerance, not after one bad day,
//  5. corroboration: a subscriber anomaly under a persistently lossy
//     transformer gets "high" confidence and a higher score.
package loss

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

type Options struct {
	AsOf         time.Time `json:"as_of"`         // pencere sonu (hariç), yerel gün
	RecentDays   int       `json:"recent_days"`   // 7
	BaselineDays int       `json:"baseline_days"` // 21
	Filters      bool      `json:"filters"`       // yanlış alarm filtreleri açık mı
	DropZ        float64   `json:"drop_z"`        // -3.5
	NightZ       float64   `json:"night_z"`       // 3.5
	PersistDays  int       `json:"persist_days"`  // 3
}

func (o *Options) defaults() {
	if o.RecentDays == 0 {
		o.RecentDays = 7
	}
	if o.BaselineDays == 0 {
		o.BaselineDays = 21
	}
	if o.DropZ == 0 {
		o.DropZ = -3.5
	}
	if o.NightZ == 0 {
		o.NightZ = 3.5
	}
	if o.PersistDays == 0 {
		o.PersistDays = 3
	}
	o.AsOf = time.Date(o.AsOf.Year(), o.AsOf.Month(), o.AsOf.Day(), 0, 0, 0, 0, time.UTC)
}

// Flag is one suspicious subscriber finding.
type Flag struct {
	SubscriberID  int64          `json:"subscriber_id"`
	TransformerID int64          `json:"transformer_id"`
	Rule          string         `json:"rule"`
	Score         float64        `json:"score"`
	Confidence    string         `json:"confidence"`
	Explanation   map[string]any `json:"explanation"`
}

type TransformerSummary struct {
	ID           int64   `json:"id"`
	Code         string  `json:"code"`
	Days         int     `json:"days"`
	ExcessDays   int     `json:"excess_days_recent"`
	ValidDays    int     `json:"valid_days_recent"`
	AvgLossPct   float64 `json:"avg_loss_pct_recent"`
	ExcessWh     int64   `json:"excess_wh_recent"`
	TolerancePct float64 `json:"tolerance_pct"`
	Persistent   bool    `json:"persistent_excess"`
}

type Summary struct {
	RunID        int64                `json:"run_id"`
	Options      Options              `json:"options"`
	Transformers []TransformerSummary `json:"transformers"`
	Flags        []Flag               `json:"flags"`
	ByRule       map[string]int       `json:"by_rule"`
	DurationMS   int64                `json:"duration_ms"`
}

var flagsTotal = metrics.NewCounter("enerjihat_loss_flags_total", "Kayıp-kaçak işaretleri", "rule", "confidence")

// balanceSQL maps every subscriber to its transformer on the current
// topology, then compares head-meter energy with the subscriber sum per day.
const balanceSQL = `
WITH RECURSIVE t AS (
    SELECT id AS node, id AS tr FROM network_nodes WHERE kind = 'transformer'
    UNION ALL
    SELECT n.id, t.tr FROM network_nodes n JOIN t ON n.parent_id = t.node
),
sub_tr AS (SELECT s.id AS sub, t.tr FROM subscribers s JOIN t ON t.node = s.pole_id WHERE s.active),
n_subs AS (SELECT tr, count(*) AS n FROM sub_tr GROUP BY tr),
head AS (
    SELECT m.node_id AS tr, cd.day, sum(cd.active_wh) AS wh, sum(cd.slots) AS slots
    FROM consumption_daily cd JOIN meters m ON m.id = cd.meter_id AND m.kind = 'head'
    WHERE cd.day >= $1 AND cd.day < $2 GROUP BY 1, 2
),
subs AS (
    SELECT st.tr, cd.day, sum(cd.active_wh) AS wh, sum(cd.slots) AS slots
    FROM consumption_daily cd JOIN sub_tr st ON st.sub = cd.subscriber_id
    WHERE cd.day >= $1 AND cd.day < $2 GROUP BY 1, 2
)
INSERT INTO loss_windows (node_id, window_start, window_end, head_wh, subscribers_wh, loss_wh, tolerance_wh, excess_wh, loss_pct, missing_slots, computed_at)
SELECT h.tr,
       h.day::timestamp AT TIME ZONE 'Europe/Istanbul',
       (h.day + 1)::timestamp AT TIME ZONE 'Europe/Istanbul',
       h.wh, coalesce(s.wh, 0),
       h.wh - coalesce(s.wh, 0),
       round(h.wh * nn.tech_loss_pct / 100)::bigint,
       greatest(0, h.wh - coalesce(s.wh, 0) - round(h.wh * nn.tech_loss_pct / 100)::bigint),
       CASE WHEN h.wh > 0 THEN round(100.0 * (h.wh - coalesce(s.wh, 0)) / h.wh, 3) ELSE 0 END,
       greatest(0, 96 * (1 + ns.n) - h.slots - coalesce(s.slots, 0)),
       now()
FROM head h
JOIN network_nodes nn ON nn.id = h.tr
JOIN n_subs ns ON ns.tr = h.tr
LEFT JOIN subs s ON s.tr = h.tr AND s.day = h.day
ON CONFLICT (node_id, window_start) DO UPDATE SET
    head_wh = EXCLUDED.head_wh, subscribers_wh = EXCLUDED.subscribers_wh, loss_wh = EXCLUDED.loss_wh,
    tolerance_wh = EXCLUDED.tolerance_wh, excess_wh = EXCLUDED.excess_wh, loss_pct = EXCLUDED.loss_pct,
    missing_slots = EXCLUDED.missing_slots, computed_at = now()`

// subscriberDaysSQL returns each subscriber-day with exclusion reasons.
const subscriberDaysSQL = `
WITH RECURSIVE t AS (
    SELECT id AS node, id AS tr FROM network_nodes WHERE kind = 'transformer'
    UNION ALL SELECT n.id, t.tr FROM network_nodes n JOIN t ON n.parent_id = t.node
)
SELECT s.id, s.category, t.tr, d.day::date,
       coalesce(cd.active_wh, 0), coalesce(cd.night_wh, 0), coalesce(cd.slots, 0)::int, coalesce(cd.interpolated, 0)::int,
       EXISTS (SELECT 1 FROM outage_affected a JOIN outages o ON o.id = a.outage_id
               WHERE a.subscriber_id = s.id AND o.status <> 'cancelled'
                 AND a.interrupted_at < (d.day + 1)::timestamp AT TIME ZONE 'Europe/Istanbul'
                 AND coalesce(a.restored_at, now()) > d.day::timestamp AT TIME ZONE 'Europe/Istanbul') AS had_outage,
       EXISTS (SELECT 1 FROM meters m WHERE m.subscriber_id = s.id AND m.kind = 'subscriber'
                 AND ((m.installed_at AT TIME ZONE 'Europe/Istanbul')::date = d.day
                      OR (m.removed_at AT TIME ZONE 'Europe/Istanbul')::date = d.day)) AS meter_change
FROM subscribers s
JOIN t ON t.node = s.pole_id
CROSS JOIN (SELECT gs::date AS day FROM generate_series($1::date, $2::date - 1, interval '1 day') gs) AS d
LEFT JOIN LATERAL (
    SELECT sum(active_wh) AS active_wh, sum(night_wh) AS night_wh, sum(slots) AS slots, sum(interpolated_slots) AS interpolated
    FROM consumption_daily c WHERE c.subscriber_id = s.id AND c.day = d.day::date
) cd ON true
WHERE s.active
ORDER BY s.id, d.day`

type subDay struct {
	day                 time.Time
	active, night       int64
	slots, interp       int
	outage, meterChange bool
}

type subSeries struct {
	id, tr   int64
	category string
	days     []subDay
}

// Run computes transformer balances for the window, evaluates every
// subscriber and stores the flags (open flags are refreshed; flags an
// analyst already reviewed keep their status).
func Run(ctx context.Context, pool *pgxpool.Pool, o Options) (*Summary, error) {
	start := time.Now()
	o.defaults()
	from := o.AsOf.AddDate(0, 0, -(o.RecentDays + o.BaselineDays))
	recentFrom := o.AsOf.AddDate(0, 0, -o.RecentDays)
	sum := &Summary{Options: o, ByRule: map[string]int{}}

	params, _ := json.Marshal(o)
	if err := pool.QueryRow(ctx, `INSERT INTO analysis_runs (kind, params) VALUES ('loss', $1) RETURNING id`, params).Scan(&sum.RunID); err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, balanceSQL, from, o.AsOf); err != nil {
		return nil, fmt.Errorf("balance: %w", err)
	}

	nSubs := map[int64]int{}
	nrows, err := pool.Query(ctx, `WITH RECURSIVE t AS (
			SELECT id AS node, id AS tr FROM network_nodes WHERE kind = 'transformer'
			UNION ALL SELECT n.id, t.tr FROM network_nodes n JOIN t ON n.parent_id = t.node)
		SELECT t.tr, count(s.id) FROM t LEFT JOIN subscribers s ON s.pole_id = t.node AND s.active GROUP BY t.tr`)
	if err != nil {
		return nil, err
	}
	for nrows.Next() {
		var tr int64
		var n int
		if err := nrows.Scan(&tr, &n); err != nil {
			nrows.Close()
			return nil, err
		}
		nSubs[tr] = n
	}
	nrows.Close()

	// Trafo özetleri (son pencere).
	rows, err := pool.Query(ctx, `SELECT lw.node_id, n.code, n.tech_loss_pct::float8, lw.window_start, lw.excess_wh, lw.loss_pct::float8, lw.missing_slots
		FROM loss_windows lw JOIN network_nodes n ON n.id = lw.node_id
		WHERE lw.window_start >= $1::timestamp AT TIME ZONE 'Europe/Istanbul' AND lw.window_start < $2::timestamp AT TIME ZONE 'Europe/Istanbul'
		ORDER BY lw.node_id, lw.window_start`, recentFrom, o.AsOf)
	if err != nil {
		return nil, err
	}
	trs := map[int64]*TransformerSummary{}
	var order []int64
	for rows.Next() {
		var id int64
		var code string
		var tol, lossPct float64
		var ws time.Time
		var excess int64
		var missing int
		if err := rows.Scan(&id, &code, &tol, &ws, &excess, &lossPct, &missing); err != nil {
			rows.Close()
			return nil, err
		}
		ts := trs[id]
		if ts == nil {
			ts = &TransformerSummary{ID: id, Code: code, TolerancePct: tol}
			trs[id] = ts
			order = append(order, id)
		}
		ts.Days++
		// Veri tamlığı ≥ %98 değilse o gün trafo dengesi güvenilmez (eksik abone verisi "kayıp" gibi görünür).
		complete := float64(missing) <= 0.02*96*float64(1+nSubs[id])
		if !o.Filters || complete {
			ts.ValidDays++
			ts.AvgLossPct += lossPct
			if excess > 0 {
				ts.ExcessDays++
				ts.ExcessWh += excess
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range order {
		ts := trs[id]
		if ts.ValidDays > 0 {
			ts.AvgLossPct = math.Round(ts.AvgLossPct/float64(ts.ValidDays)*100) / 100
		}
		if o.Filters {
			ts.Persistent = ts.ExcessDays >= o.PersistDays
		} else {
			ts.Persistent = ts.ExcessDays >= 1
		}
		sum.Transformers = append(sum.Transformers, *ts)
	}

	series, err := loadSeries(ctx, pool, from, o.AsOf)
	if err != nil {
		return nil, err
	}
	sum.Flags = evaluate(series, recentFrom, o, trs)
	if err := store(ctx, pool, sum, from, o.AsOf); err != nil {
		return nil, err
	}
	sum.DurationMS = time.Since(start).Milliseconds()
	res, _ := json.Marshal(map[string]any{"flags": len(sum.Flags), "by_rule": sum.ByRule, "ms": sum.DurationMS})
	_, _ = pool.Exec(ctx, `UPDATE analysis_runs SET finished_at = now(), summary = $2 WHERE id = $1`, sum.RunID, res)
	return sum, nil
}

func loadSeries(ctx context.Context, pool *pgxpool.Pool, from, to time.Time) ([]*subSeries, error) {
	rows, err := pool.Query(ctx, subscriberDaysSQL, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*subSeries
	var cur *subSeries
	for rows.Next() {
		var id, tr int64
		var cat string
		var d subDay
		if err := rows.Scan(&id, &cat, &tr, &d.day, &d.active, &d.night, &d.slots, &d.interp, &d.outage, &d.meterChange); err != nil {
			return nil, err
		}
		if cur == nil || cur.id != id {
			cur = &subSeries{id: id, tr: tr, category: cat}
			out = append(out, cur)
		}
		cur.days = append(cur.days, d)
	}
	return out, rows.Err()
}

// ---- statistics ----

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// MAD returns the scaled median absolute deviation (≈ σ for normal data).
func MAD(xs []float64) float64 {
	m := median(xs)
	dev := make([]float64, len(xs))
	for i, x := range xs {
		dev[i] = math.Abs(x - m)
	}
	return 1.4826 * median(dev)
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return math.NaN(), math.NaN()
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	m := s / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	if len(xs) > 1 {
		v /= float64(len(xs) - 1)
	}
	return m, math.Sqrt(v)
}

// RobustZ is (x − median) / MAD with a floor on the spread so a perfectly
// flat history does not turn tiny changes into huge scores.
func RobustZ(x float64, ref []float64, floor float64) float64 {
	m := median(ref)
	s := math.Max(MAD(ref), floor)
	if s == 0 {
		return 0
	}
	return (x - m) / s
}

func round(f float64, p int) float64 {
	k := math.Pow(10, float64(p))
	return math.Round(f*k) / k
}

func (d subDay) valid() bool { return d.slots >= 94 && d.interp <= 8 && !d.outage && !d.meterChange }

func evaluate(series []*subSeries, recentFrom time.Time, o Options, trs map[int64]*TransformerSummary) []Flag {
	var flags []Flag
	type nightObs struct {
		s     *subSeries
		share float64
		avgWh float64
		used  int
	}
	nightByCat := map[string][]nightObs{}

	for _, s := range series {
		var base, recent []float64
		excluded := map[string]int{"outage": 0, "low_quality": 0, "meter_change": 0}
		var nightSum, activeSum float64
		nightDays := 0
		for _, d := range s.days {
			ok := d.valid()
			if o.Filters && !ok {
				switch {
				case d.outage:
					excluded["outage"]++
				case d.meterChange:
					excluded["meter_change"]++
				default:
					excluded["low_quality"]++
				}
				continue
			}
			v := float64(d.active)
			if d.day.Before(recentFrom) {
				base = append(base, v)
			} else {
				recent = append(recent, v)
				nightSum += float64(d.night)
				activeSum += v
				nightDays++
			}
		}
		tr := trs[s.tr]

		// Kural 1: ani tüketim düşüşü
		minBase, minRecent := 10, 4
		if !o.Filters {
			minBase, minRecent = 2, 1
		}
		if len(base) >= minBase && len(recent) >= minRecent {
			var z, center, recentC, thr float64
			var method string
			if o.Filters {
				center, recentC = median(base), median(recent)
				z = RobustZ(recentC, base, math.Max(0.05*center, 200))
				thr = o.DropZ
				method = "robust_z (medyan/MAD)"
			} else {
				m, sd := meanStd(base)
				center, recentC = m, 0
				for _, x := range recent {
					recentC += x
				}
				recentC /= float64(len(recent))
				if sd > 0 {
					z = (recentC - m) / sd
				}
				thr = -3
				method = "z (ortalama/std)"
			}
			drop := 0.0
			if center > 0 {
				drop = 1 - recentC/center
			}
			hit := z <= thr
			if o.Filters {
				hit = hit && drop >= 0.5 && center >= 2000
			}
			if hit {
				flags = append(flags, makeFlag(s, "sudden_drop", z, tr, o, map[string]any{
					"method": method, "baseline_wh": round(center, 0), "recent_wh": round(recentC, 0), "z": round(z, 2), "threshold": thr,
					"drop_pct": round(drop*100, 1), "baseline_days": len(base), "recent_days": len(recent), "excluded_days": excluded,
				}, fmt.Sprintf("Son %d günün tipik tüketimi %.1f kWh/gün; önceki dönemin %.1f kWh/gün (−%%%.0f, z = %.1f ≤ %.1f).",
					len(recent), recentC/1000, center/1000, drop*100, z, thr)))
			}
		}
		if nightDays > 0 && activeSum > 0 {
			nightByCat[s.category] = append(nightByCat[s.category], nightObs{s: s, share: nightSum / activeSum, avgWh: activeSum / float64(nightDays), used: nightDays})
		}
	}

	// Kural 2: olağandışı gece tüketimi — aynı kategorideki akranlara göre.
	for _, obs := range nightByCat {
		shares := make([]float64, len(obs))
		for i, x := range obs {
			shares[i] = x.share
		}
		med := median(shares)
		mean, sd := meanStd(shares)
		for _, x := range obs {
			var z, thr float64
			var hit bool
			if o.Filters {
				z = RobustZ(x.share, shares, 0.02)
				thr = o.NightZ
				hit = z >= thr && x.share >= 0.5 && x.avgWh >= 1000 && x.used >= 4
			} else {
				if sd > 0 {
					z = (x.share - mean) / sd
				}
				thr = 3
				hit = z >= thr
			}
			if hit {
				tr := trs[x.s.tr]
				flags = append(flags, makeFlag(x.s, "night_usage", z, tr, o, map[string]any{
					"night_share_pct": round(x.share*100, 1), "peer_median_pct": round(med*100, 1), "z": round(z, 2), "threshold": thr,
					"peer_group": x.s.category, "peer_count": len(obs), "days": x.used, "avg_daily_wh": round(x.avgWh, 0),
				}, fmt.Sprintf("Tüketimin %%%.0f'i gece (22-06); aynı kategorideki %d abonenin medyanı %%%.0f (z = %.1f ≥ %.1f).",
					x.share*100, len(obs), med*100, z, thr)))
			}
		}
	}
	sort.Slice(flags, func(i, j int) bool {
		if flags[i].Score != flags[j].Score {
			return flags[i].Score > flags[j].Score
		}
		return flags[i].SubscriberID < flags[j].SubscriberID
	})
	return flags
}

func makeFlag(s *subSeries, rule string, z float64, tr *TransformerSummary, o Options, ex map[string]any, reason string) Flag {
	score := math.Min(math.Abs(z), 50)
	conf := "medium"
	if tr != nil {
		ex["transformer"] = map[string]any{"id": tr.ID, "code": tr.Code, "excess_days": tr.ExcessDays, "valid_days": tr.ValidDays,
			"avg_loss_pct": tr.AvgLossPct, "tolerance_pct": tr.TolerancePct, "persistent_excess": tr.Persistent}
		if tr.Persistent {
			conf = "high"
			score *= 1.5
			reason += fmt.Sprintf(" Bağlı olduğu %s trafosunda son %d günün %d'inde tolerans (%%%.1f) üstü kayıp var (ortalama kayıp %%%.1f).",
				tr.Code, tr.ValidDays, tr.ExcessDays, tr.TolerancePct, tr.AvgLossPct)
		} else {
			reason += fmt.Sprintf(" %s trafosunda kalıcı fazla kayıp yok; tek başına zayıf kanıt.", tr.Code)
		}
	}
	if !o.Filters {
		conf = "unfiltered"
	}
	ex["confidence"] = conf
	ex["reason"] = reason
	ex["rule"] = rule
	var trID int64
	if tr != nil {
		trID = tr.ID
	}
	return Flag{SubscriberID: s.id, TransformerID: trID, Rule: rule, Score: round(score, 3), Confidence: conf, Explanation: ex}
}

func store(ctx context.Context, pool *pgxpool.Pool, sum *Summary, from, to time.Time) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Bu pencere için artık geçerli olmayan açık işaretler kapanır.
		if _, err := tx.Exec(ctx, `DELETE FROM suspicion_flags WHERE window_start = $1 AND status = 'open'`, from); err != nil {
			return err
		}
		for _, f := range sum.Flags {
			ex, _ := json.Marshal(f.Explanation)
			var tr *int64
			if f.TransformerID > 0 {
				tr = &f.TransformerID
			}
			if _, err := tx.Exec(ctx, `INSERT INTO suspicion_flags (subscriber_id, node_id, rule, score, window_start, window_end, explanation, run_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (subscriber_id, rule, window_start) DO UPDATE
				  SET score = EXCLUDED.score, explanation = EXCLUDED.explanation, run_id = EXCLUDED.run_id
				  WHERE suspicion_flags.status = 'open'`,
				f.SubscriberID, tr, f.Rule, f.Score, from, to, ex, sum.RunID); err != nil {
				return err
			}
			sum.ByRule[f.Rule]++
			flagsTotal.Inc(f.Rule, f.Confidence)
		}
		return nil
	})
}
