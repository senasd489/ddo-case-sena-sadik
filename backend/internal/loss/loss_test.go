package loss

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

var ctx = context.Background()
var asOf = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestRobustStatistics(t *testing.T) {
	xs := []float64{10, 11, 9, 10, 12, 10, 300} // tek uç değer
	if m := median(xs); m != 10 {
		t.Fatalf("median %v", m)
	}
	if mad := MAD(xs); math.Abs(mad-1.4826) > 1e-9 {
		t.Fatalf("MAD %v", mad)
	}
	_, sd := meanStd(xs)
	// Uç değer std'yi ~110'a şişirir ama MAD'i etkilemez: 0 tüketim robust z'de çok uç kalır.
	if z := RobustZ(0, xs, 0); z > -6 || sd < 100 {
		t.Fatalf("robust z %v, sd %v", z, sd)
	}
	if z := RobustZ(5, []float64{7, 7, 7, 7}, 0.5); z != -4 {
		t.Fatalf("floored z %v", z)
	}
}

func TestDetectsPlantedAnomaliesAndExplainsThem(t *testing.T) {
	pool := testutil.NewDB(t)
	truth, err := Plant(ctx, pool, asOf, 1)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := Run(ctx, pool, Options{AsOf: asOf, Filters: true})
	if err != nil {
		t.Fatal(err)
	}
	sc := Evaluate(sum.Flags, truth, 600)
	t.Logf("filtreli: %+v", sc)
	if sc.Recall != 1 || sc.Precision < 0.85 || sc.FPByCause["kesinti"] != 0 || sc.FPByCause["eksik_veri"] != 0 {
		t.Fatalf("filtered score %+v", sc)
	}
	// TR-0001 (kaçaklar) kalıcı fazla kayıp; TR-0003'teki tek eksik veri günü alarm üretmez.
	for _, tr := range sum.Transformers {
		switch tr.Code {
		case "TR-0001":
			if !tr.Persistent || tr.ExcessDays < 5 {
				t.Fatalf("TR-0001 %+v", tr)
			}
		case "TR-0003":
			if tr.Persistent {
				t.Fatalf("TR-0003 flagged from one incomplete day: %+v", tr)
			}
		}
	}
	// Kaçakların hepsi "high" güvenle ve gerekçeyle işaretli.
	thief := map[int64]bool{}
	for _, id := range truth.Thieves {
		thief[id] = true
	}
	for _, f := range sum.Flags {
		if thief[f.SubscriberID] {
			if f.Rule != "sudden_drop" || f.Confidence != "high" || f.Explanation["reason"] == "" {
				t.Fatalf("thief flag %+v", f)
			}
		}
	}
	var stored int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM suspicion_flags WHERE status='open'`).Scan(&stored)
	if stored != len(sum.Flags) {
		t.Fatalf("stored %d flags, run produced %d", stored, len(sum.Flags))
	}
}

// Yanlış alarm azaltımının ölçümü: aynı veri, filtreler kapalı vs açık.
func TestFiltersReduceFalseAlarms(t *testing.T) {
	pool := testutil.NewDB(t)
	truth, err := Plant(ctx, pool, asOf, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Run(ctx, pool, Options{AsOf: asOf, Filters: false})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := Run(ctx, pool, Options{AsOf: asOf, Filters: true})
	if err != nil {
		t.Fatal(err)
	}
	a, b := Evaluate(raw.Flags, truth, 600), Evaluate(filtered.Flags, truth, 600)
	t.Logf("filtresiz: %+v", a)
	t.Logf("filtreli:  %+v", b)
	if b.FalsePositives >= a.FalsePositives || b.Precision <= a.Precision || b.Recall < a.Recall {
		t.Fatalf("filters did not help: raw %+v filtered %+v", a, b)
	}
	if a.FPByCause["kesinti"] == 0 && a.FPByCause["eksik_veri"] == 0 {
		t.Fatalf("raw run should have been fooled by confounders: %+v", a)
	}
}

func TestReviewedFlagsKeepStatusOnRerun(t *testing.T) {
	pool := testutil.NewDB(t)
	if _, err := Plant(ctx, pool, asOf, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, pool, Options{AsOf: asOf, Filters: true}); err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = pool.QueryRow(ctx, `UPDATE suspicion_flags SET status='dismissed' WHERE id = (SELECT min(id) FROM suspicion_flags) RETURNING id`).Scan(&id)
	if _, err := Run(ctx, pool, Options{AsOf: asOf, Filters: true}); err != nil {
		t.Fatal(err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM suspicion_flags WHERE id=$1`, id).Scan(&st)
	if st != "dismissed" {
		t.Fatalf("reviewed flag reopened: %s", st)
	}
}

func TestPlantIsDeterministic(t *testing.T) {
	digest := func() string {
		pool := testutil.NewDB(t)
		if _, err := Plant(ctx, pool, asOf, 9); err != nil {
			t.Fatal(err)
		}
		var d string
		_ = pool.QueryRow(ctx, `SELECT md5(string_agg(concat_ws(',', meter_id, day, active_wh, night_wh, slots), ';' ORDER BY meter_id, day)) FROM consumption_daily`).Scan(&d)
		return d
	}
	if digest() != digest() {
		t.Fatal("same seed produced different data")
	}
}
