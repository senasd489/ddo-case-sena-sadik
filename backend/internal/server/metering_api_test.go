package server

import (
	"fmt"
	"testing"
	"time"
)

func readingsFor(meter int64, day time.Time, n int, perSlot int64) []map[string]any {
	var rs []map[string]any
	for i := 0; i <= n; i++ {
		rs = append(rs, map[string]any{"meter_id": meter, "read_at": day.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339),
			"active_index_wh": int64(i) * perSlot, "reactive_index_varh": int64(i) * 5})
	}
	return rs
}

var testDay = time.Date(2026, 4, 1, 0, 0, 0, 0, time.FixedZone("TRT", 3*3600))

func TestIngestAuthAndConsumptionAPI(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"readings": readingsFor(1, testDay, 96, 120)}

	if r := e.do("POST", "/api/v1/readings/batch", "", body); r.Status != 401 {
		t.Fatalf("no auth: %d", r.Status)
	}
	if r := e.do("POST", "/api/v1/readings/batch", "", body, "X-Ingest-Token", "wrong"); r.Status != 401 || r.errCode(t) != "invalid_ingest_token" {
		t.Fatalf("bad token: %d %s", r.Status, r.Body)
	}
	abone := e.login("abone@enerjihat.local")
	if r := e.do("POST", "/api/v1/readings/batch", abone.AccessToken, body); r.Status != 403 {
		t.Fatalf("subscriber ingesting: %d", r.Status)
	}
	r := e.do("POST", "/api/v1/readings/batch", "", body, "X-Ingest-Token", testIngestToken)
	var res struct{ Inserted, Duplicates, Rejected int }
	r.json(t, &res)
	if r.Status != 202 || res.Inserted != 97 {
		t.Fatalf("ingest: %d %s", r.Status, r.Body)
	}
	admin := e.login("admin@enerjihat.local")
	if r := e.do("POST", "/api/v1/admin/consumption/recompute", admin.AccessToken, nil); r.Status != 200 {
		t.Fatalf("recompute: %d %s", r.Status, r.Body)
	}

	// Abone kendi tüketimini görür …
	var daily struct {
		Items []struct {
			Day      string
			ActiveWh int64 `json:"active_wh"`
			PeakWh   int64 `json:"peak_wh"`
		}
	}
	r = e.do("GET", "/api/v1/subscribers/1/consumption?granularity=day&from=2026-04-01&to=2026-04-02", abone.AccessToken, nil)
	r.json(t, &daily)
	if r.Status != 200 || len(daily.Items) != 1 || daily.Items[0].ActiveWh != 96*120 || daily.Items[0].PeakWh != 20*120 {
		t.Fatalf("daily: %d %s", r.Status, r.Body)
	}
	var slots struct {
		TotalWh int64          `json:"total_wh"`
		Q       map[string]int `json:"quality_counts"`
		Items   []any
	}
	e.do("GET", "/api/v1/subscribers/1/consumption?granularity=15m&from=2026-04-01&to=2026-04-02", abone.AccessToken, nil).json(t, &slots)
	if len(slots.Items) != 96 || slots.TotalWh != 96*120 || slots.Q["measured"] != 96 {
		t.Fatalf("15m: total=%d q=%v n=%d", slots.TotalWh, slots.Q, len(slots.Items))
	}
	// … ama başka abonenin verisini göremez (satır düzeyi yetki).
	if r := e.do("GET", "/api/v1/subscribers/2/consumption", abone.AccessToken, nil); r.Status != 403 {
		t.Fatalf("other subscriber's data: %d", r.Status)
	}
	if r := e.do("GET", "/api/v1/subscribers/2", abone.AccessToken, nil); r.Status != 403 {
		t.Fatalf("other subscriber record: %d", r.Status)
	}
	// Ham okumalar abone rolüne kapalı, operatöre açık.
	if r := e.do("GET", "/api/v1/meters/1/readings?from=2026-04-01", abone.AccessToken, nil); r.Status != 403 {
		t.Fatalf("subscriber raw readings: %d", r.Status)
	}
	op := e.login("operator@enerjihat.local")
	var raw struct{ Items []any }
	e.do("GET", "/api/v1/meters/1/readings?from=2026-04-01&to=2026-04-02", op.AccessToken, nil).json(t, &raw)
	if len(raw.Items) != 96 {
		t.Fatalf("raw readings %d", len(raw.Items))
	}
	var stats struct {
		Inserted int64 `json:"inserted_last_hour"`
	}
	e.do("GET", "/api/v1/readings/stats", op.AccessToken, nil).json(t, &stats)
	if stats.Inserted != 97 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestSubscriberRegistration(t *testing.T) {
	e := newEnv(t)
	tk := e.login("fatura@enerjihat.local")
	valid := map[string]any{"full_name": "Yeni Abone", "address": "Liman Cd. 5", "category": "residential",
		"pole_id": 5, "tariff_id": 1, "contract_kw": 7.5, "meter_serial_no": "SM-TEST-0001"}

	bad := map[string]any{"full_name": "", "address": "x", "category": "uzay", "pole_id": 0, "tariff_id": 0, "contract_kw": -1, "meter_serial_no": ""}
	if r := e.do("POST", "/api/v1/subscribers", tk.AccessToken, bad); r.Status != 422 {
		t.Fatalf("validation: %d %s", r.Status, r.Body)
	}
	// Direk yerine besleyiciye (id 2) bağlamak reddedilir.
	feeder := map[string]any{}
	for k, v := range valid {
		feeder[k] = v
	}
	feeder["pole_id"] = 2
	feeder["meter_serial_no"] = "SM-TEST-0002"
	if r := e.do("POST", "/api/v1/subscribers", tk.AccessToken, feeder); r.Status != 422 {
		t.Fatalf("attach to feeder: %d %s", r.Status, r.Body)
	}
	r := e.do("POST", "/api/v1/subscribers", tk.AccessToken, valid, "Idempotency-Key", "sub-create-0001")
	var created struct {
		Subscriber struct {
			ID           int64
			SubscriberNo string `json:"subscriber_no"`
		}
		MeterID int64 `json:"meter_id"`
	}
	r.json(t, &created)
	if r.Status != 201 || created.MeterID == 0 || created.Subscriber.SubscriberNo != fmt.Sprintf("AB%08d", created.Subscriber.ID) {
		t.Fatalf("create: %d %s", r.Status, r.Body)
	}
	// Aynı seri no ile ikinci kayıt → 409
	valid["full_name"] = "Başka"
	if r := e.do("POST", "/api/v1/subscribers", tk.AccessToken, valid); r.Status != 409 {
		t.Fatalf("duplicate serial: %d %s", r.Status, r.Body)
	}
	var detail struct {
		Meters     []any
		SupplyPath []struct{ Kind string } `json:"supply_path"`
	}
	e.do("GET", fmt.Sprintf("/api/v1/subscribers/%d", created.Subscriber.ID), tk.AccessToken, nil).json(t, &detail)
	if len(detail.Meters) != 1 || len(detail.SupplyPath) < 3 || detail.SupplyPath[0].Kind != "transformer" {
		t.Fatalf("detail: %+v", detail)
	}
	// Analist abone kaydedemez.
	an := e.login("analist@enerjihat.local")
	if r := e.do("POST", "/api/v1/subscribers", an.AccessToken, valid); r.Status != 403 {
		t.Fatalf("analyst create: %d", r.Status)
	}
	var list struct {
		Items []any
		Next  *string `json:"next_cursor"`
	}
	e.do("GET", "/api/v1/subscribers?limit=10&category=residential", tk.AccessToken, nil).json(t, &list)
	if len(list.Items) != 10 || list.Next == nil {
		t.Fatalf("list: %d", len(list.Items))
	}
}

func TestMeterChangeAPI(t *testing.T) {
	e := newEnv(t)
	saha := e.login("saha@enerjihat.local")
	e.do("POST", "/api/v1/readings/batch", "", map[string]any{"readings": readingsFor(3, testDay, 40, 100)}, "X-Ingest-Token", testIngestToken)
	change := map[string]any{"at": testDay.Add(40 * 15 * time.Minute).Format(time.RFC3339), "final_index_wh": 3000, "new_serial_no": "SM-CHG-3"}
	if r := e.do("POST", "/api/v1/subscribers/3/meter-change", saha.AccessToken, change); r.Status != 422 {
		t.Fatalf("final index below last reading accepted: %d %s", r.Status, r.Body)
	}
	change["final_index_wh"] = 4000
	r := e.do("POST", "/api/v1/subscribers/3/meter-change", saha.AccessToken, change)
	if r.Status != 201 {
		t.Fatalf("meter change: %d %s", r.Status, r.Body)
	}
	fat := e.login("fatura@enerjihat.local")
	if r := e.do("POST", "/api/v1/subscribers/3/meter-change", fat.AccessToken, change); r.Status != 403 {
		t.Fatalf("billing meter change: %d", r.Status)
	}
}
