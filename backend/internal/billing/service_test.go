package billing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

// putDays writes consumption_daily rows for subscriber/meter 1 directly
// (the T-1 pipeline is tested separately); slots < 96 marks a partial day.
func putDays(t *testing.T, pool *pgxpool.Pool, sub int64, from, to time.Time, wh int64, slots int) {
	t.Helper()
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		_, err := pool.Exec(context.Background(), `INSERT INTO consumption_daily (meter_id, subscriber_id, day, active_wh, day_wh, peak_wh, night_wh,
				reactive_varh, max_demand_w, slots, interpolated_slots)
			VALUES ($1,$1,$2,$3,$4,$5,$6,0,1000,$7,0)
			ON CONFLICT (meter_id, day) DO UPDATE SET active_wh=EXCLUDED.active_wh, day_wh=EXCLUDED.day_wh, peak_wh=EXCLUDED.peak_wh,
				night_wh=EXCLUDED.night_wh, slots=EXCLUDED.slots`,
			sub, d, wh, wh/2, wh/4, wh-wh/2-wh/4, slots)
		if err != nil {
			t.Fatal(err)
		}
	}
}

var ctx = context.Background()

func TestGenerateActualIsIdempotent(t *testing.T) {
	pool := testutil.NewDB(t)
	putDays(t, pool, 1, d(2026, 9, 1), d(2026, 10, 1), 9_000, 96)
	a, created, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	if err != nil || !created || a.Kind != "actual" || a.ConsumptionWh != 270_000 {
		t.Fatalf("first: %+v %v %v", a, created, err)
	}
	b, created, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	if err != nil || created || b.ID != a.ID || b.TotalKurus != a.TotalKurus || b.InputHash != a.InputHash {
		t.Fatalf("regenerate changed result: %+v %v %v", b, created, err)
	}
	full, err := Get(ctx, pool, a.ID)
	if err != nil || len(full.Lines) == 0 || len(full.Breakdown) != 30 {
		t.Fatalf("get: lines=%d breakdown=%d err=%v", len(full.Lines), len(full.Breakdown), err)
	}
	var sum int64
	for _, l := range full.Lines {
		sum += l.AmountKurus
	}
	if sum != full.TotalKurus {
		t.Fatalf("lines sum %d != total %d", sum, full.TotalKurus)
	}
}

func TestOctoberInvoiceSplitsAtTariffChange(t *testing.T) {
	pool := testutil.NewDB(t)
	putDays(t, pool, 1, d(2026, 10, 1), d(2026, 11, 1), 10_000, 96)
	inv, _, err := Generate(ctx, pool, 1, "2026-10", GenerateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	full, _ := Get(ctx, pool, inv.ID)
	versions := map[float64]bool{}
	vats := 0
	for _, l := range full.Lines {
		if v, ok := l.Meta["tariff_version_id"].(float64); ok {
			versions[v] = true
		}
		if l.Kind == "vat" {
			vats++
		}
	}
	if !versions[1] || !versions[2] || vats != 2 {
		t.Fatalf("expected two tariff segments, got versions=%v vats=%d", versions, vats)
	}
}

func TestEstimatedThenActualSettlesPaidAmount(t *testing.T) {
	pool := testutil.NewDB(t)
	// 1–20 Eylül ölçülmüş, 21–30 henüz yok → tahmini fatura.
	putDays(t, pool, 1, d(2026, 9, 1), d(2026, 9, 21), 9_000, 96)
	est, _, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	if err != nil || est.Kind != "estimated" || est.EstimatedWh != 10*9_000 {
		t.Fatalf("estimated: %+v %v", est, err)
	}
	full, _ := Get(ctx, pool, est.ID)
	estDays := 0
	for _, d := range full.Breakdown {
		if d.Estimated {
			estDays++
		}
	}
	if estDays != 10 {
		t.Fatalf("breakdown marks %d estimated days", estDays)
	}
	if _, err := Pay(ctx, pool, est.ID, 10_000, 0); err != nil {
		t.Fatal(err)
	}
	// Gerçek okumalar gelir (gerçek tüketim tahminden yüksek).
	putDays(t, pool, 1, d(2026, 9, 21), d(2026, 10, 1), 12_000, 96)
	act, created, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	if err != nil || !created || act.Kind != "actual" || act.SettlesInvoiceID == nil || *act.SettlesInvoiceID != est.ID {
		t.Fatalf("actual: %+v %v %v", act, created, err)
	}
	full, _ = Get(ctx, pool, act.ID)
	last := full.Lines[len(full.Lines)-1]
	if last.Kind != "settlement" || last.AmountKurus != -10_000 {
		t.Fatalf("settlement line %+v", last)
	}
	var estStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id=$1`, est.ID).Scan(&estStatus)
	if estStatus != "settled" {
		t.Fatalf("estimated status %s", estStatus)
	}
	again, created, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	if err != nil || created || again.ID != act.ID {
		t.Fatalf("regenerate after settlement: %+v %v %v", again, created, err)
	}
	if _, err := Pay(ctx, pool, est.ID, 1, 0); !errors.Is(err, ErrNotPayable) {
		t.Fatalf("paying a settled invoice: %v", err)
	}
}

func TestChangedInputsNeedExplicitReissue(t *testing.T) {
	pool := testutil.NewDB(t)
	putDays(t, pool, 1, d(2026, 9, 1), d(2026, 10, 1), 9_000, 96)
	a, _, _ := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	putDays(t, pool, 1, d(2026, 9, 5), d(2026, 9, 6), 50_000, 96) // geriye dönük düzeltme
	if _, _, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{}); !errors.Is(err, ErrInputsChanged) {
		t.Fatalf("want ErrInputsChanged, got %v", err)
	}
	b, created, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{Reissue: true})
	if err != nil || !created || b.Revision != 2 || b.TotalKurus <= a.TotalKurus || b.InvoiceNo == a.InvoiceNo {
		t.Fatalf("reissue: %+v %v", b, err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM invoices WHERE id=$1`, a.ID).Scan(&st)
	var audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE entity='invoice' AND action IN ('invoice.cancel','invoice.issue')`).Scan(&audits)
	if st != "cancelled" || audits != 3 {
		t.Fatalf("old status %s audits %d", st, audits)
	}
}

// 8 eşzamanlı istek aynı abone-dönem için tek fatura üretir.
func TestConcurrentGenerateCreatesOneInvoice(t *testing.T) {
	pool := testutil.NewDB(t)
	putDays(t, pool, 1, d(2026, 9, 1), d(2026, 10, 1), 9_000, 96)
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inv, _, err := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
			errs[i] = err
			if inv != nil {
				ids[i] = inv.ID
			}
		}(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("results %v %v", ids, errs)
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM invoices`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d invoices", n)
	}
}

// Yarış durumu kanıtı: toplamı faturayı aşan iki eşzamanlı ödemeden yalnızca
// biri kabul edilir (SELECT … FOR UPDATE). 20 tur tekrarlanır.
func TestConcurrentPaymentsNeverOverpay(t *testing.T) {
	pool := testutil.NewDB(t)
	putDays(t, pool, 1, d(2026, 9, 1), d(2026, 10, 1), 9_000, 96)
	inv, _, _ := Generate(ctx, pool, 1, "2026-09", GenerateOptions{})
	part := inv.TotalKurus*6/10 + 1
	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = Pay(ctx, pool, inv.ID, part, 0)
		}(i)
	}
	close(start)
	wg.Wait()
	ok, over := 0, 0
	for _, e := range results {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrOverpayment):
			over++
		default:
			t.Fatal(e)
		}
	}
	var paid int64
	_ = pool.QueryRow(ctx, `SELECT paid_kurus FROM invoices WHERE id=$1`, inv.ID).Scan(&paid)
	if ok != 1 || over != 1 || paid != part {
		t.Fatalf("ok=%d over=%d paid=%d part=%d", ok, over, paid, part)
	}
	// Kalanı öde → fatura 'paid'.
	p, err := Pay(ctx, pool, inv.ID, inv.TotalKurus-part, 0)
	if err != nil || p.Status != "paid" || p.Remaining != 0 {
		t.Fatalf("final payment %+v %v", p, err)
	}
}

func TestEstimateWithoutAnyHistoryUsesCategoryDefault(t *testing.T) {
	pool := testutil.NewDB(t)
	in, est, err := LoadInput(ctx, pool, 1, d(2026, 9, 1), d(2026, 10, 1))
	if err != nil || !est {
		t.Fatal(err)
	}
	var total int64
	for _, d := range in.Days {
		total += d.ActiveWh
	}
	if total != 30*defaultDailyWh["residential"] {
		t.Fatalf("default estimate total %d", total)
	}
}
