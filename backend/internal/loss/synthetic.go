package loss

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Profile is the deterministic consumption profile of a subscriber; the
// seed's history generator and the live simulator share it, so the history
// and the streamed readings describe the same household.
type Profile struct {
	BaseWh     float64 // ortalama günlük tüketim
	NightShare float64 // gece (22-06) payı
	Noise      float64 // günlük göreli dalgalanma
	Noisy      bool
}

func ProfileOf(seed uint64, subscriberID int64) Profile {
	r := rand.New(rand.NewPCG(seed, uint64(subscriberID)*0x9e3779b97f4a7c15+1))
	p := Profile{BaseWh: 6000 + r.Float64()*8000, NightShare: 0.25 + r.Float64()*0.10, Noise: 0.20}
	if r.IntN(10) == 0 {
		p.Noisy, p.Noise = true, 0.6
	}
	return p
}

// Truth is the ground truth of a planted scenario.
type Truth struct {
	Thieves     []int64 `json:"thieves"`      // ani düşüş (sayaç baypası), TR-0001
	NightUsers  []int64 `json:"night_users"`  // olağandışı gece tüketimi, TR-0002
	OutageSubs  []int64 `json:"outage_subs"`  // tuzak: kesinti yüzünden düşük tüketim
	MissingSubs []int64 `json:"missing_subs"` // tuzak: eksik veri yüzünden düşük görünen tüketim
	Noisy       []int64 `json:"noisy"`        // tuzak: yüksek değişkenlikli normal aboneler
}

func (t Truth) Positives() map[int64]string {
	m := map[int64]string{}
	for _, id := range t.Thieves {
		m[id] = "sudden_drop"
	}
	for _, id := range t.NightUsers {
		m[id] = "night_usage"
	}
	return m
}

// Plant writes 28 days of daily consumption for every subscriber and head
// meter of a seeded database, with known anomalies and known confounders.
// The same seed always produces the same data.
func Plant(ctx context.Context, pool *pgxpool.Pool, asOf time.Time, seed uint64) (Truth, error) {
	asOf = time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, time.UTC)
	const days = 28
	from := asOf.AddDate(0, 0, -days)
	recentFrom := asOf.AddDate(0, 0, -7)

	type sub struct {
		id, meter int64
		tr        string
	}
	rows, err := pool.Query(ctx, `WITH RECURSIVE t AS (
			SELECT id AS node, code AS tr FROM network_nodes WHERE kind='transformer'
			UNION ALL SELECT n.id, t.tr FROM network_nodes n JOIN t ON n.parent_id = t.node)
		SELECT s.id, m.id, t.tr FROM subscribers s JOIN t ON t.node = s.pole_id
		JOIN meters m ON m.subscriber_id = s.id AND m.removed_at IS NULL
		WHERE s.active ORDER BY s.id`)
	if err != nil {
		return Truth{}, err
	}
	var subs []sub
	byTr := map[string][]int64{}
	for rows.Next() {
		var s sub
		if err := rows.Scan(&s.id, &s.meter, &s.tr); err != nil {
			rows.Close()
			return Truth{}, err
		}
		subs = append(subs, s)
		byTr[s.tr] = append(byTr[s.tr], s.id)
	}
	rows.Close()
	pick := func(tr string, from, n int) []int64 {
		l := byTr[tr]
		if len(l) < from+n {
			return nil
		}
		return append([]int64(nil), l[from:from+n]...)
	}
	truth := Truth{
		Thieves:     pick("TR-0001", 10, 5),
		NightUsers:  pick("TR-0002", 20, 3),
		OutageSubs:  pick("TR-0003", 30, 3),
		MissingSubs: pick("TR-0003", 40, 2),
	}
	role := map[int64]string{}
	for _, id := range truth.Thieves {
		role[id] = "thief"
	}
	for _, id := range truth.NightUsers {
		role[id] = "night"
	}
	for _, id := range truth.OutageSubs {
		role[id] = "outage"
	}
	for _, id := range truth.MissingSubs {
		role[id] = "missing"
	}
	for _, s := range subs {
		if role[s.id] == "" && ProfileOf(seed, s.id).Noisy {
			role[s.id] = "noisy"
			truth.Noisy = append(truth.Noisy, s.id)
		}
	}
	// TR-0003'te bir gün (asOf-3) 10 abonenin verisi hiç gelmez: trafo dengesi o gün bozuk görünür.
	gapDay := asOf.AddDate(0, 0, -3)
	gapSubs := map[int64]bool{}
	for _, id := range pick("TR-0003", 60, 10) {
		gapSubs[id] = true
	}

	headTrue := map[string]map[time.Time]float64{}
	var dailyRows [][]any
	for _, s := range subs {
		pr := ProfileOf(seed, s.id)
		base, nightShare, noise := pr.BaseWh, pr.NightShare, pr.Noise
		if role[s.id] == "thief" {
			base *= 3 // kaçak genelde yüksek tüketimli abonede (klima, atölye) görülür
		}
		rng := rand.New(rand.NewPCG(seed^0xabcdef, uint64(s.id)))
		for d := 0; d < days; d++ {
			day := from.AddDate(0, 0, d)
			wh := base * (1 + (rng.Float64()*2-1)*noise)
			if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
				wh *= 1.1
			}
			if role[s.id] == "noisy" && rng.IntN(14) == 0 {
				wh *= 3 // ara sıra misafir/klima: uç değer
			}
			ns := nightShare
			trueWh := wh
			recorded := wh
			slots, interp := 96, 0
			recent := !day.Before(recentFrom)
			switch role[s.id] {
			case "thief":
				if recent {
					recorded = wh * 0.12 // sayaç baypas: gerçek tüketim aynı, ölçülen %12
				}
			case "night":
				if recent {
					ns = 0.78
					trueWh = wh * 1.3
					recorded = trueWh
				}
			case "outage":
				if recent && d >= days-5 {
					trueWh, recorded = wh*0.05, wh*0.05 // gerçekten elektrik yok
				}
			case "missing":
				if recent && d >= days-5 {
					slots = 40
					recorded = wh * 40 / 96 // sayaç haberleşmesi koptu: veri eksik
				}
			}
			if day.Equal(gapDay) && gapSubs[s.id] {
				if headTrue[s.tr] == nil {
					headTrue[s.tr] = map[time.Time]float64{}
				}
				headTrue[s.tr][day] += trueWh
				continue // satır yok
			}
			if headTrue[s.tr] == nil {
				headTrue[s.tr] = map[time.Time]float64{}
			}
			headTrue[s.tr][day] += trueWh
			a := int64(recorded)
			night := int64(recorded * ns)
			peak := int64(recorded * 0.25)
			dayWh := a - night - peak
			if dayWh < 0 {
				dayWh, peak = 0, a-night
			}
			dailyRows = append(dailyRows, []any{s.meter, s.id, day, a, dayWh, peak, night, int64(0), a / 24, int16(slots), int16(interp)})
		}
	}
	// Trafo ana sayaçları: gerçek tüketim + %2 teknik kayıp (her trafonun toleransının altında).
	hrows, err := pool.Query(ctx, `SELECT m.id, n.code FROM meters m JOIN network_nodes n ON n.id = m.node_id WHERE m.kind='head'`)
	if err != nil {
		return truth, err
	}
	heads := map[string]int64{}
	for hrows.Next() {
		var id int64
		var code string
		_ = hrows.Scan(&id, &code)
		heads[code] = id
	}
	hrows.Close()
	for tr, byDay := range headTrue {
		for day, wh := range byDay {
			a := int64(wh * 1.02)
			dailyRows = append(dailyRows, []any{heads[tr], nil, day, a, a / 2, a / 4, a - a/2 - a/4, int64(0), a / 24, int16(96), int16(0)})
		}
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM consumption_daily WHERE day >= $1 AND day < $2`, from, asOf); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"consumption_daily"},
			[]string{"meter_id", "subscriber_id", "day", "active_wh", "day_wh", "peak_wh", "night_wh", "reactive_varh", "max_demand_w", "slots", "interpolated_slots"},
			pgx.CopyFromRows(dailyRows)); err != nil {
			return err
		}
		// Kesinti tuzağı: son 5 gün süren gerçek bir kesinti kaydı.
		if len(truth.OutageSubs) == 0 {
			return nil
		}
		var pole int64
		if err := tx.QueryRow(ctx, `SELECT pole_id FROM subscribers WHERE id = $1`, truth.OutageSubs[0]).Scan(&pole); err != nil {
			return err
		}
		start := asOf.AddDate(0, 0, -5).Add(-3 * time.Hour) // yerel gece yarısı
		end := asOf.Add(-3 * time.Hour)
		var oid int64
		if err := tx.QueryRow(ctx, `INSERT INTO outages (kind, status, fault_node_id, feeder_id, started_at, ended_at, cause)
			VALUES ('unplanned','resolved',$1,$1,$2,$3,'sentetik: uzun süreli arıza') RETURNING id`, pole, start, end).Scan(&oid); err != nil {
			return err
		}
		for _, id := range truth.OutageSubs {
			if _, err := tx.Exec(ctx, `INSERT INTO outage_affected (outage_id, subscriber_id, interrupted_at, restored_at) VALUES ($1,$2,$3,$4)
				ON CONFLICT DO NOTHING`, oid, id, start, end); err != nil {
				return fmt.Errorf("outage affected: %w", err)
			}
		}
		return nil
	})
	return truth, err
}

// Score compares flags with the truth (subscriber-level, any rule).
type Score struct {
	Flagged        int            `json:"flagged"`
	TruePositives  int            `json:"true_positives"`
	FalsePositives int            `json:"false_positives"`
	FalseNegatives int            `json:"false_negatives"`
	Precision      float64        `json:"precision"`
	Recall         float64        `json:"recall"`
	FalseAlarmRate float64        `json:"false_alarm_rate"` // FP / tüm normal aboneler
	FPByCause      map[string]int `json:"false_positives_by_cause"`
}

func Evaluate(flags []Flag, t Truth, totalSubs int) Score {
	pos := t.Positives()
	cause := map[int64]string{}
	for _, id := range t.OutageSubs {
		cause[id] = "kesinti"
	}
	for _, id := range t.MissingSubs {
		cause[id] = "eksik_veri"
	}
	for _, id := range t.Noisy {
		cause[id] = "degisken_normal"
	}
	seen := map[int64]bool{}
	s := Score{FPByCause: map[string]int{}}
	for _, f := range flags {
		if seen[f.SubscriberID] {
			continue
		}
		seen[f.SubscriberID] = true
		s.Flagged++
		if _, ok := pos[f.SubscriberID]; ok {
			s.TruePositives++
		} else {
			s.FalsePositives++
			c := cause[f.SubscriberID]
			if c == "" {
				c = "diger_normal"
			}
			s.FPByCause[c]++
		}
	}
	s.FalseNegatives = len(pos) - s.TruePositives
	if s.Flagged > 0 {
		s.Precision = round(float64(s.TruePositives)/float64(s.Flagged), 3)
	}
	if len(pos) > 0 {
		s.Recall = round(float64(s.TruePositives)/float64(len(pos)), 3)
	}
	if n := totalSubs - len(pos); n > 0 {
		s.FalseAlarmRate = round(float64(s.FalsePositives)/float64(n), 4)
	}
	return s
}
