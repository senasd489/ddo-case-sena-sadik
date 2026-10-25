package server

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestBillingAPIFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for d := 0; d < 30; d++ {
		day := time.Date(2026, 9, 1+d, 0, 0, 0, 0, time.UTC)
		if _, err := e.pool.Exec(ctx, `INSERT INTO consumption_daily (meter_id, subscriber_id, day, active_wh, day_wh, peak_wh, night_wh,
			reactive_varh, max_demand_w, slots, interpolated_slots) VALUES (1,1,$1,9000,4500,2250,2250,0,900,96,2)`, day); err != nil {
			t.Fatal(err)
		}
	}
	fat := e.login("fatura@enerjihat.local")
	an := e.login("analist@enerjihat.local")
	abone := e.login("abone@enerjihat.local")
	abone2 := e.login("abone2@enerjihat.local")

	req := map[string]any{"subscriber_id": 1, "period": "2026-09"}
	if r := e.do("POST", "/api/v1/billing/invoices/generate", an.AccessToken, req); r.Status != 403 {
		t.Fatalf("analyst generating: %d", r.Status)
	}
	if r := e.do("POST", "/api/v1/billing/invoices/generate", fat.AccessToken, map[string]any{"subscriber_id": 1, "period": "eylül"}); r.Status != 422 {
		t.Fatalf("bad period: %d %s", r.Status, r.Body)
	}
	r := e.do("POST", "/api/v1/billing/invoices/generate", fat.AccessToken, req)
	var out struct {
		Created bool
		Invoice struct {
			ID         int64
			TotalKurus int64 `json:"total_kurus"`
			Kind       string
			Breakdown  []struct {
				Interpolated int `json:"interpolated_slots"`
			}
		}
	}
	r.json(t, &out)
	if r.Status != 201 || !out.Created || out.Invoice.Kind != "actual" || len(out.Invoice.Breakdown) != 30 || out.Invoice.Breakdown[0].Interpolated != 2 {
		t.Fatalf("generate: %d %s", r.Status, r.Body)
	}
	r2 := e.do("POST", "/api/v1/billing/invoices/generate", fat.AccessToken, req)
	var out2 struct {
		Created bool
		Invoice struct{ ID int64 }
	}
	r2.json(t, &out2)
	if r2.Status != 200 || out2.Created || out2.Invoice.ID != out.Invoice.ID {
		t.Fatalf("regenerate: %d %s", r2.Status, r2.Body)
	}

	path := fmt.Sprintf("/api/v1/invoices/%d", out.Invoice.ID)
	if r := e.do("GET", path, abone.AccessToken, nil); r.Status != 200 {
		t.Fatalf("own invoice: %d", r.Status)
	}
	if r := e.do("GET", path, abone2.AccessToken, nil); r.Status != 404 {
		t.Fatalf("other's invoice: %d", r.Status)
	}
	// Ödeme: anahtar zorunlu; aynı anahtarla tekrar → aynı cevap, tek ödeme.
	pay := map[string]any{"amount_kurus": 1000}
	if r := e.do("POST", path+"/payments", abone.AccessToken, pay); r.Status != 400 {
		t.Fatalf("payment without key: %d", r.Status)
	}
	p1 := e.do("POST", path+"/payments", abone.AccessToken, pay, "Idempotency-Key", "pay-0000000001")
	p2 := e.do("POST", path+"/payments", abone.AccessToken, pay, "Idempotency-Key", "pay-0000000001")
	if p1.Status != 201 || p2.Status != 201 || string(p1.Body) != string(p2.Body) {
		t.Fatalf("payment replay: %d %s / %d %s", p1.Status, p1.Body, p2.Status, p2.Body)
	}
	var paid int64
	_ = e.pool.QueryRow(ctx, `SELECT paid_kurus FROM invoices WHERE id=$1`, out.Invoice.ID).Scan(&paid)
	if paid != 1000 {
		t.Fatalf("paid %d (double charge?)", paid)
	}
	if r := e.do("POST", path+"/payments", abone2.AccessToken, pay, "Idempotency-Key", "pay-0000000002"); r.Status != 404 {
		t.Fatalf("paying other's invoice: %d", r.Status)
	}
	over := map[string]any{"amount_kurus": out.Invoice.TotalKurus}
	if r := e.do("POST", path+"/payments", abone.AccessToken, over, "Idempotency-Key", "pay-0000000003"); r.Status != 409 || r.errCode(t) != "overpayment" {
		t.Fatalf("overpayment: %d %s", r.Status, r.Body)
	}
	var prev struct {
		Estimated bool
		Result    struct {
			TotalKurus int64 `json:"total_kurus"`
		}
	}
	e.do("GET", "/api/v1/subscribers/1/invoice-preview?period=2026-10", abone.AccessToken, nil).json(t, &prev)
	if !prev.Estimated || prev.Result.TotalKurus <= 0 {
		t.Fatalf("preview %+v", prev)
	}
	var run struct {
		Result map[string]int
	}
	e.do("POST", "/api/v1/billing/runs", fat.AccessToken, map[string]any{"period": "2026-09", "subscriber_ids": []int{1, 2, 3}}).json(t, &run)
	if run.Result["unchanged"] != 1 || run.Result["created"] != 2 {
		t.Fatalf("run %+v", run)
	}
}
