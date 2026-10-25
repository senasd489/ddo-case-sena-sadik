package metering

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

var ist = time.FixedZone("Europe/Istanbul", 3*3600)

// day0 is local midnight of the test day; meter 1 belongs to subscriber 1.
var day0 = time.Date(2026, 3, 10, 0, 0, 0, 0, ist)

func at(slot int) time.Time { return day0.Add(time.Duration(slot) * Slot) }

// flat builds readings for points [from, to] with a constant 100 Wh per slot,
// starting from base, reactive 10 VArh per slot.
func flat(meter int64, from, to int, base int64) []Reading {
	var rs []Reading
	for i := from; i <= to; i++ {
		rs = append(rs, Reading{MeterID: meter, ReadAt: at(i), ActiveWh: base + int64(i)*100, ReactiveVarh: int64(i) * 10})
	}
	return rs
}

func ingest(t *testing.T, pool *pgxpool.Pool, rs []Reading) IngestResult {
	t.Helper()
	res, err := Ingest(context.Background(), pool, "test", rs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DrainDirty(context.Background(), pool, 1000); err != nil {
		t.Fatal(err)
	}
	return res
}

type slotRow struct {
	Start   time.Time
	Wh      int64
	Quality string
}

func slotsOf(t *testing.T, pool *pgxpool.Pool, meter int64, day time.Time) []slotRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT slot_start, active_wh, quality FROM consumption_15m
		WHERE meter_id = $1 AND slot_start >= $2 AND slot_start < $3 ORDER BY slot_start`, meter, day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[slotRow])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sumWh(ss []slotRow) (total int64, byQ map[string]int) {
	byQ = map[string]int{}
	for _, s := range ss {
		total += s.Wh
		byQ[s.Quality]++
	}
	return
}

func dailyOf(t *testing.T, pool *pgxpool.Pool, meter int64, day time.Time) (active, dayWh, peak, night, maxDemand int64, interp int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT active_wh, day_wh, peak_wh, night_wh, max_demand_w, interpolated_slots
		FROM consumption_daily WHERE meter_id=$1 AND day=$2`, meter, day.Format("2006-01-02")).
		Scan(&active, &dayWh, &peak, &night, &maxDemand, &interp)
	if err != nil {
		t.Fatalf("daily row: %v", err)
	}
	return
}

func TestFullDayIsMeasured(t *testing.T) {
	pool := testutil.NewDB(t)
	res := ingest(t, pool, flat(1, 0, 96, 50_000))
	if res.Inserted != 97 || res.Duplicates != 0 || res.Rejected != 0 {
		t.Fatalf("ingest result %+v", res)
	}
	ss := slotsOf(t, pool, 1, day0)
	total, q := sumWh(ss)
	if len(ss) != 96 || total != 9600 || q["measured"] != 96 {
		t.Fatalf("slots=%d total=%d q=%v", len(ss), total, q)
	}
	a, d, p, n, md, _ := dailyOf(t, pool, 1, day0)
	// gündüz 06-17 = 44 slot, puant 17-22 = 20, gece = 32
	if a != 9600 || d != 4400 || p != 2000 || n != 3200 || md != 400 {
		t.Fatalf("daily a=%d day=%d peak=%d night=%d maxdemand=%d", a, d, p, n, md)
	}
}

func TestGapIsInterpolatedExactlyAndFlagged(t *testing.T) {
	pool := testutil.NewDB(t)
	rs := flat(1, 0, 96, 0)
	// 10..13 noktaları eksik (4 okuma) → 9→14 arası 5 slot tek segment.
	var withGap []Reading
	for _, r := range rs {
		i := int(r.ReadAt.Sub(day0) / Slot)
		if i >= 10 && i <= 13 {
			continue
		}
		if i >= 14 {
			r.ActiveWh += 1 // gap içinde 501 Wh tüketilmiş olsun (bölünemeyen miktar)
		}
		withGap = append(withGap, r)
	}
	ingest(t, pool, withGap)
	ss := slotsOf(t, pool, 1, day0)
	total, q := sumWh(ss)
	if len(ss) != 96 || total != 9601 || q["interpolated"] != 5 {
		t.Fatalf("slots=%d total=%d q=%v", len(ss), total, q)
	}
	// Tam sayı bölme: 501 = 100+100+100+100+101, hiçbir Wh kaybolmaz.
	var gapSum int64
	for _, s := range ss[9:14] {
		if s.Quality != "interpolated" {
			t.Fatalf("slot %v quality %s", s.Start, s.Quality)
		}
		gapSum += s.Wh
	}
	if gapSum != 501 || ss[13].Wh != 101 {
		t.Fatalf("gap split wrong: %+v", ss[9:14])
	}
	if _, _, _, _, _, interp := dailyOf(t, pool, 1, day0); interp != 5 {
		t.Fatalf("daily interpolated_slots=%d", interp)
	}
}

func TestDuplicatesAreIgnored(t *testing.T) {
	pool := testutil.NewDB(t)
	rs := flat(1, 0, 96, 0)
	ingest(t, pool, rs)
	// aynı parti tekrar + parti içi tekrar
	again := append(append([]Reading{}, rs...), rs[5], rs[6])
	res := ingest(t, pool, again)
	if res.Inserted != 0 || res.Corrections != 0 || res.Duplicates != 99 {
		t.Fatalf("duplicate handling: %+v", res)
	}
	total, _ := sumWh(slotsOf(t, pool, 1, day0))
	if total != 9600 {
		t.Fatalf("total %d", total)
	}
}

func TestBackdatedCorrectionRecomputes(t *testing.T) {
	pool := testutil.NewDB(t)
	ingest(t, pool, flat(1, 0, 96, 0))
	// 40. noktanın endeksi yanlış gelmişti; düzeltme +250 Wh.
	res := ingest(t, pool, []Reading{{MeterID: 1, ReadAt: at(40), ActiveWh: 4000 + 250, ReactiveVarh: 400}})
	if res.Corrections != 1 || res.Inserted != 0 {
		t.Fatalf("correction result %+v", res)
	}
	ss := slotsOf(t, pool, 1, day0)
	if ss[39].Wh != 350 || ss[39].Quality != "corrected" {
		t.Fatalf("slot 39 after correction: %+v", ss[39])
	}
	// 40→41 artık 4250→4100: küçük geri düşüş sıfırlama sayılmaz, 0 Wh + 'corrected'.
	if ss[40].Wh != 0 || ss[40].Quality != "corrected" {
		t.Fatalf("slot 40 after correction: %+v", ss[40])
	}
	var old, newV int64
	if err := pool.QueryRow(context.Background(), `SELECT old_index_wh, new_index_wh FROM reading_corrections WHERE meter_id=1`).Scan(&old, &newV); err != nil {
		t.Fatal(err)
	}
	if old != 4000 || newV != 4250 {
		t.Fatalf("correction log old=%d new=%d", old, newV)
	}
}

func TestCounterResetUsesNewValue(t *testing.T) {
	pool := testutil.NewDB(t)
	rs := flat(1, 0, 96, 900_000)
	// 50. noktada sayaç sıfırlanır: 50. endeks 30 Wh, sonrası 30'dan devam.
	for i := range rs {
		if i >= 50 {
			rs[i].ActiveWh = 30 + int64(i-50)*100
		}
	}
	ingest(t, pool, rs)
	ss := slotsOf(t, pool, 1, day0)
	if ss[49].Quality != "reset" || ss[49].Wh != 30 {
		t.Fatalf("reset slot: %+v", ss[49])
	}
	total, q := sumWh(ss)
	if total != 95*100+30 || q["reset"] != 1 {
		t.Fatalf("total=%d q=%v", total, q)
	}
}

func TestMeterChangeMidDay(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	ingest(t, pool, flat(1, 0, 47, 70_000)) // 00:00 → 11:45, son endeks 74 700
	// 12:00'de değişim: eski sayaç kapanış 74 740 (son 15 dk'da 40 Wh), yeni sayaç 0'dan başlar.
	var newID int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		m, err := ReplaceMeter(ctx, tx, MeterChange{SubscriberID: 1, At: at(48), FinalIndexWh: 74_740, NewSerial: "SM-NEW-1"})
		if err != nil {
			return err
		}
		newID = m.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var rs []Reading
	for i := 49; i <= 96; i++ {
		rs = append(rs, Reading{MeterID: newID, ReadAt: at(i), ActiveWh: int64(i-48) * 100})
	}
	ingest(t, pool, rs)

	oldS, newS := slotsOf(t, pool, 1, day0), slotsOf(t, pool, newID, day0)
	if len(oldS) != 48 || oldS[47].Wh != 40 || oldS[47].Quality != "meter_change" {
		t.Fatalf("old meter tail: n=%d last=%+v", len(oldS), oldS[len(oldS)-1])
	}
	if len(newS) != 48 || newS[0].Wh != 100 || newS[0].Quality != "meter_change" {
		t.Fatalf("new meter head: n=%d first=%+v", len(newS), newS[0])
	}
	var subTotal int64
	if err := pool.QueryRow(ctx, `SELECT sum(active_wh) FROM consumption_daily WHERE subscriber_id=1 AND day=$1`, "2026-03-10").Scan(&subTotal); err != nil {
		t.Fatal(err)
	}
	if subTotal != 47*100+40+48*100 {
		t.Fatalf("subscriber day total %d", subTotal)
	}
	// Eski sayaca değişimden sonra okuma yazılamaz.
	res, _ := Ingest(ctx, pool, "test", []Reading{{MeterID: 1, ReadAt: at(60), ActiveWh: 99_999}})
	if res.Rejected != 1 {
		t.Fatalf("reading after removal accepted: %+v", res)
	}
}

func TestGapAcrossMidnightAndLateArrival(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	// Gün 1: 00:00-22:00 var; 22:15 sonrası yok. Gün 2: henüz veri yok.
	ingest(t, pool, flat(1, 0, 88, 0))
	if n := len(slotsOf(t, pool, 1, day0)); n != 88 {
		t.Fatalf("before late data: %d slots, want 88 (unmeasured tail absent)", n)
	}
	// Ertesi gün 02:00'den itibaren veri gelir (gece yarısını aşan 16 slotluk boşluk).
	var rs []Reading
	for i := 104; i <= 192; i++ {
		rs = append(rs, Reading{MeterID: 1, ReadAt: at(i), ActiveWh: int64(i) * 100})
	}
	ingest(t, pool, rs)
	d1, d2 := slotsOf(t, pool, 1, day0), slotsOf(t, pool, 1, day0.Add(24*time.Hour))
	t1, q1 := sumWh(d1)
	t2, q2 := sumWh(d2)
	if len(d1) != 96 || len(d2) != 96 || t1 != 9600 || t2 != 9600 {
		t.Fatalf("day1 %d/%d day2 %d/%d", len(d1), t1, len(d2), t2)
	}
	if q1["interpolated"] != 8 || q2["interpolated"] != 8 {
		t.Fatalf("interpolated split across midnight: %v %v", q1, q2)
	}
	_ = ctx
}

func TestRecomputeIsIdempotent(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	ingest(t, pool, flat(1, 0, 96, 0))
	snapshot := func() string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT md5(string_agg(concat_ws(',', slot_start, active_wh, quality), ';' ORDER BY slot_start)) FROM consumption_15m WHERE meter_id=1`).Scan(&s)
		return s
	}
	before := snapshot()
	for i := 0; i < 2; i++ {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			return Recompute(ctx, tx, []MeterDay{{MeterID: 1, Day: day0}})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if snapshot() != before {
		t.Fatal("recompute changed results")
	}
}

func TestRejectsInvalidReadings(t *testing.T) {
	pool := testutil.NewDB(t)
	res, err := Ingest(context.Background(), pool, "test", []Reading{
		{MeterID: 1, ReadAt: at(1).Add(7 * time.Minute), ActiveWh: 1},                  // hizasız
		{MeterID: 1, ReadAt: at(1), ActiveWh: -5},                                      // negatif
		{MeterID: 99_999_999, ReadAt: at(1), ActiveWh: 1},                              // bilinmeyen sayaç
		{MeterID: 1, ReadAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), ActiveWh: 1}, // kurulumdan önce
		{MeterID: 1, ReadAt: time.Now().Add(48 * time.Hour).Truncate(Slot), ActiveWh: 1},
		{MeterID: 1, ReadAt: at(2), ActiveWh: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 5 || res.Inserted != 1 {
		t.Fatalf("result %+v", res)
	}
}

// Birden çok işçi aynı kuyruğu işlerken SKIP LOCKED sayesinde hiçbir
// sayaç-gün iki kez işlenmez ve kuyruk tamamen boşalır.
func TestConcurrentWorkersDrainQueueOnce(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	var rs []Reading
	for m := int64(1); m <= 40; m++ {
		rs = append(rs, flat(m, 0, 96, 0)...)
	}
	if _, err := Ingest(ctx, pool, "test", rs); err != nil {
		t.Fatal(err)
	}
	var queued int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM consumption_dirty`).Scan(&queued)
	done := make(chan int, 4)
	for w := 0; w < 4; w++ {
		go func() {
			n, err := DrainDirty(ctx, pool, 3)
			if err != nil {
				t.Error(err)
			}
			done <- n
		}()
	}
	total := 0
	for w := 0; w < 4; w++ {
		total += <-done
	}
	if total != queued {
		t.Fatalf("processed %d meter-days, queued %d", total, queued)
	}
	var slots int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM consumption_15m WHERE slot_start >= $1 AND slot_start < $2`, day0, day0.Add(24*time.Hour)).Scan(&slots)
	if slots != 40*96 {
		t.Fatalf("slots %d", slots)
	}
}
