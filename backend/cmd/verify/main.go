// Command verify exercises every feature (F-1…F-7), every hard task
// (T-1…T-5), health, authentication and authorisation end-to-end against a
// running, seeded system — over the public HTTP API only, exactly as a
// client would. It prints a PASS/FAIL table and exits non-zero on failure.
//
//	API_URL=http://localhost:8080 WEB_URL=http://localhost:3000 INGEST_TOKEN=… verify
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const pw = "Demo1234!"

var (
	api    = env("API_URL", "http://localhost:8080")
	web    = env("WEB_URL", "")
	ingest = os.Getenv("INGEST_TOKEN")
	client = &http.Client{Timeout: 120 * time.Second}
	runID  = time.Now().UnixNano() % 1_000_000_000
	rng    = rand.New(rand.NewPCG(uint64(runID), 7))
	ist    = func() *time.Location {
		if l, err := time.LoadLocation("Europe/Istanbul"); err == nil {
			return l
		}
		return time.FixedZone("TRT", 3*3600)
	}()
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

type resp struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r resp) J() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}
func (r resp) Into(v any) error { return json.Unmarshal(r.Body, v) }
func (r resp) Code() string {
	if e, ok := r.J()["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func call(method, path, token string, body any, hdr ...string) resp {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = api + path
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := client.Do(req)
	if err != nil {
		return resp{Status: 0, Body: []byte(err.Error())}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, b, res.Header}
}

// ---- result table ----

type result struct {
	ID, Name string
	OK       bool
	Detail   string
	Dur      time.Duration
}

var results []result

type checkErr struct{ msg string }

func fail(format string, a ...any) { panic(checkErr{fmt.Sprintf(format, a...)}) }
func expect(cond bool, format string, a ...any) {
	if !cond {
		fail(format, a...)
	}
}
func expectStatus(r resp, want int, what string) {
	if r.Status != want {
		fail("%s: HTTP %d (beklenen %d) %s", what, r.Status, want, trunc(string(r.Body), 200))
	}
}
func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func check(id, name string, fn func() string) {
	start := time.Now()
	res := result{ID: id, Name: name}
	func() {
		defer func() {
			if v := recover(); v != nil {
				if ce, ok := v.(checkErr); ok {
					res.Detail = ce.msg
				} else {
					res.Detail = fmt.Sprint(v)
				}
			}
		}()
		res.Detail = fn()
		res.OK = true
	}()
	res.Dur = time.Since(start)
	mark := "PASS"
	if !res.OK {
		mark = "FAIL"
	}
	fmt.Printf("  %-4s %-8s %s — %s\n", mark, id, name, trunc(res.Detail, 160))
	results = append(results, res)
}

// ---- helpers ----

var tokens = map[string]string{}

func login(who string) string {
	if t, ok := tokens[who]; ok {
		return t
	}
	r := call("POST", "/api/v1/auth/login", "", map[string]string{"email": who + "@enerjihat.local", "password": pw})
	expectStatus(r, 200, "login "+who)
	t, _ := r.J()["access_token"].(string)
	tokens[who] = t
	return t
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

type readingT struct {
	MeterID  int64  `json:"meter_id"`
	ReadAt   string `json:"read_at"`
	ActiveWh int64  `json:"active_index_wh"`
	Reactive int64  `json:"reactive_index_varh"`
}

func ingestReadings(rs []readingT) map[string]any {
	r := call("POST", "/api/v1/readings/batch", "", map[string]any{"readings": rs}, "X-Ingest-Token", ingest)
	expectStatus(r, 202, "readings/batch")
	return r.J()
}

func recompute() {
	expectStatus(call("POST", "/api/v1/admin/consumption/recompute", login("admin"), nil), 200, "recompute")
}

func localMidnight(daysAgo int) time.Time {
	n := time.Now().In(ist)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, ist).AddDate(0, 0, -daysAgo)
}

// newSubscriber registers a fresh subscriber + meter on a pole, installed in the past.
func newSubscriber(pole int64, installed time.Time) (sub, meter int64) {
	body := map[string]any{"full_name": fmt.Sprintf("Doğrulama Abonesi %d", runID), "address": "Test Sk. 1, Bandırma", "category": "residential",
		"pole_id": pole, "tariff_id": 1, "contract_kw": 7.5, "meter_serial_no": fmt.Sprintf("VRF-%d-%d", runID, rng.IntN(1e6)),
		"installed_at": installed.Format(time.RFC3339)}
	r := call("POST", "/api/v1/subscribers", login("fatura"), body, "Idempotency-Key", fmt.Sprintf("verify-sub-%d-%d", runID, rng.IntN(1e9)))
	expectStatus(r, 201, "abone kaydı")
	var out struct {
		Subscriber struct{ ID int64 }
		MeterID    int64 `json:"meter_id"`
	}
	_ = r.Into(&out)
	return out.Subscriber.ID, out.MeterID
}

type node struct {
	ID          int64  `json:"id"`
	ParentID    *int64 `json:"parent_id"`
	AltParentID *int64 `json:"alt_parent_id"`
	Kind        string `json:"kind"`
	Code        string `json:"code"`
	Subscribers int    `json:"subscribers"`
	Switched    bool   `json:"switched"`
}

func tree() []node {
	r := call("GET", "/api/v1/network/tree", login("operator"), nil)
	expectStatus(r, 200, "tree")
	var t struct{ Items []node }
	_ = r.Into(&t)
	return t.Items
}

func main() {
	start := time.Now()
	fmt.Printf("EnerjiHat doğrulama — API %s, çalıştırma #%d\n\n", api, runID)
	if ingest == "" {
		fmt.Println("INGEST_TOKEN gerekli")
		os.Exit(2)
	}
	// Sistem hazır olana kadar bekle (en fazla 3 dk).
	for i := 0; i < 90; i++ {
		if call("GET", "/health", "", nil).Status == 200 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	runChecks()

	sort.SliceStable(results, func(i, j int) bool { return order(results[i].ID) < order(results[j].ID) })
	failed := 0
	fmt.Printf("\n| Kimlik | Özellik | Sonuç | Süre | Ayrıntı |\n|---|---|---|---|---|\n")
	for _, r := range results {
		mark := "PASS"
		if !r.OK {
			mark = "FAIL"
			failed++
		}
		fmt.Printf("| %s | %s | %s | %.1fs | %s |\n", r.ID, r.Name, mark, r.Dur.Seconds(), strings.ReplaceAll(trunc(r.Detail, 140), "|", "/"))
	}
	fmt.Printf("\nToplam %d kontrol, %d PASS, %d FAIL — %.1f sn\n", len(results), len(results)-failed, failed, time.Since(start).Seconds())
	if failed > 0 {
		os.Exit(1)
	}
}

var ids = []string{"HEALTH", "AUTH-1", "AUTH-2", "AUTH-3", "RBAC-1", "RBAC-2", "RBAC-3", "F-1", "F-2", "F-3", "F-4", "F-5", "F-6", "F-7",
	"T-1", "T-2", "T-3", "T-4", "T-5", "IDEMP", "RACE", "AUDIT", "OBS", "LIVE", "WEB"}

func order(id string) int {
	for i, x := range ids {
		if x == id {
			return i
		}
	}
	return 999
}

func runChecks() {
	check("HEALTH", "GET /health (veritabanı dahil)", func() string {
		r := call("GET", "/health", "", nil)
		expectStatus(r, 200, "health")
		db, _ := r.J()["checks"].(map[string]any)["database"].(map[string]any)
		expect(db["status"] == "up", "db status %v", db["status"])
		return "status=ok, database=up"
	})

	check("AUTH-1", "Yanlış parola / bilinmeyen kullanıcı → 401", func() string {
		r := call("POST", "/api/v1/auth/login", "", map[string]string{"email": "admin@enerjihat.local", "password": "yanlis"})
		expect(r.Status == 401 && r.Code() == "invalid_credentials", "yanlış parola: %d %s", r.Status, r.Code())
		r = call("POST", "/api/v1/auth/login", "", map[string]string{"email": "yok@enerjihat.local", "password": pw})
		expect(r.Status == 401, "bilinmeyen kullanıcı: %d", r.Status)
		return "invalid_credentials (her iki durumda aynı cevap)"
	})
	check("AUTH-2", "Token yok / geçersiz token → 401", func() string {
		expect(call("GET", "/api/v1/auth/me", "", nil).Status == 401, "tokensız")
		r := call("GET", "/api/v1/auth/me", "abc.def.ghi", nil)
		expect(r.Status == 401 && r.Code() == "invalid_token", "geçersiz token %d %s", r.Status, r.Code())
		return "unauthorized / invalid_token"
	})
	check("AUTH-3", "Refresh token rotasyonu ve yeniden kullanım tespiti", func() string {
		r := call("POST", "/api/v1/auth/login", "", map[string]string{"email": "analist@enerjihat.local", "password": pw})
		rt, _ := r.J()["refresh_token"].(string)
		r2 := call("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": rt})
		expectStatus(r2, 200, "refresh")
		r3 := call("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": rt})
		expect(r3.Status == 401 && r3.Code() == "refresh_reused", "eski token tekrar: %d %s", r3.Status, r3.Code())
		rt2, _ := r2.J()["refresh_token"].(string)
		expect(call("POST", "/api/v1/auth/refresh", "", map[string]string{"refresh_token": rt2}).Status == 401, "aile iptal edilmedi")
		return "yenileme dönüyor; eski token tekrar kullanılınca tüm aile iptal"
	})
	check("RBAC-1", "Yetkisiz rol → 403 (sunucu tarafı)", func() string {
		cases := []struct{ who, method, path string }{
			{"abone", "GET", "/api/v1/admin/users"}, {"abone", "GET", "/api/v1/network/tree"}, {"analist", "POST", "/api/v1/outages"},
			{"operator", "POST", "/api/v1/analytics/loss/run"}, {"saha", "POST", "/api/v1/billing/invoices/generate"},
			{"fatura", "POST", "/api/v1/network/switch"}, {"abone", "POST", "/api/v1/readings/batch"},
		}
		for _, c := range cases {
			r := call(c.method, c.path, login(c.who), map[string]any{})
			expect(r.Status == 403, "%s %s %s → %d", c.who, c.method, c.path, r.Status)
		}
		return fmt.Sprintf("%d rol/uç kombinasyonu 403", len(cases))
	})
	check("RBAC-2", "Abone başka abonenin verisine erişemez", func() string {
		t := login("abone")
		expect(call("GET", "/api/v1/subscribers/2", t, nil).Status == 403, "başka abone kaydı")
		expect(call("GET", "/api/v1/subscribers/2/consumption", t, nil).Status == 403, "başka abone tüketimi")
		expect(call("GET", "/api/v1/subscribers/2/invoices", t, nil).Status == 403, "başka abone faturaları")
		expect(call("GET", "/api/v1/subscribers/1/consumption", t, nil).Status == 200, "kendi tüketimi")
		return "kendi verisi 200, başkasınınki 403"
	})
	check("RBAC-3", "Geçersiz ingest token → 401", func() string {
		r := call("POST", "/api/v1/readings/batch", "", map[string]any{"readings": []any{}}, "X-Ingest-Token", "yanlis")
		expect(r.Status == 401 && r.Code() == "invalid_ingest_token", "%d %s", r.Status, r.Code())
		return "invalid_ingest_token"
	})

	// Ortak: doğrulama için yeni abone (geçmişte kurulmuş) — F-1 … T-2.
	var subID, meterID int64
	var pole int64
	check("F-1", "Abone ve sayaç kaydı", func() string {
		for _, n := range tree() {
			if n.Kind == "pole" {
				pole = n.ID
				break
			}
		}
		subID, meterID = newSubscriber(pole, time.Date(2026, 8, 1, 0, 0, 0, 0, ist))
		r := call("GET", fmt.Sprintf("/api/v1/subscribers/%d", subID), login("fatura"), nil)
		expectStatus(r, 200, "abone detay")
		var d struct {
			Meters     []struct{ ID int64 }
			SupplyPath []struct {
				ID   int64
				Kind string
			} `json:"supply_path"`
		}
		_ = r.Into(&d)
		expect(len(d.Meters) == 1 && d.Meters[0].ID == meterID, "sayaç bağlı değil")
		expect(len(d.SupplyPath) >= 3 && d.SupplyPath[0].Kind == "transformer", "besleme yolu %v", d.SupplyPath)
		bad := call("POST", "/api/v1/subscribers", login("fatura"), map[string]any{"full_name": "x", "address": "y", "category": "residential",
			"pole_id": d.SupplyPath[0].ID, "tariff_id": 1, "contract_kw": 5, "meter_serial_no": "dup"})
		expect(bad.Status == 422 || bad.Status == 400, "direk olmayan düğüme bağlama reddedilmedi: %d", bad.Status)
		return fmt.Sprintf("abone #%d, sayaç #%d, besleme %d düğüm", subID, meterID, len(d.SupplyPath))
	})

	day := localMidnight(3)
	mk := func(meter int64, from, to int, base int64, per int64) []readingT {
		var rs []readingT
		for i := from; i <= to; i++ {
			rs = append(rs, readingT{meter, day.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339), base + int64(i)*per, int64(i) * 10})
		}
		return rs
	}
	check("F-2", "15 dakikalık okuma alma (tekrar ve hizasız okuma)", func() string {
		expect(meterID > 0, "F-1 başarısız")
		res := ingestReadings(mk(meterID, 0, 96, 1_000_000, 100))
		expect(num(res["inserted"]) == 97, "yazılan %v", res["inserted"])
		res = ingestReadings(mk(meterID, 0, 10, 1_000_000, 100))
		expect(num(res["duplicates"]) == 11 && num(res["inserted"]) == 0, "tekrarlar %v", res)
		bad := ingestReadings([]readingT{{meterID, day.Add(7 * time.Minute).Format(time.RFC3339), 1, 0}})
		expect(num(bad["rejected"]) == 1, "hizasız okuma kabul edildi")
		return "97 yazıldı; tekrar parti 11 tekrar/0 yeni; :07 okuması reddedildi"
	})
	check("F-3", "Tüketim ve talep hesaplama", func() string {
		recompute()
		d := day.Format("2006-01-02")
		r := call("GET", fmt.Sprintf("/api/v1/subscribers/%d/consumption?granularity=day&from=%s&to=%s", subID, d, day.AddDate(0, 0, 1).Format("2006-01-02")), login("fatura"), nil)
		expectStatus(r, 200, "günlük tüketim")
		var out struct {
			Items []struct {
				ActiveWh   int64 `json:"active_wh"`
				PeakWh     int64 `json:"peak_wh"`
				MaxDemandW int64 `json:"max_demand_w"`
			}
		}
		_ = r.Into(&out)
		expect(len(out.Items) == 1 && out.Items[0].ActiveWh == 9600, "günlük %+v", out.Items)
		expect(out.Items[0].PeakWh == 2000 && out.Items[0].MaxDemandW == 400, "puant/talep %+v", out.Items[0])
		return "9600 Wh/gün, puant 2000 Wh (17-22), en yüksek 15 dk talep 400 W"
	})

	check("T-1", "Okuma birleştirme: boşluk, tekrar, düzeltme, sıfırlama, sayaç değişimi", func() string {
		expect(meterID > 0, "F-1 başarısız")
		d2 := day.AddDate(0, 0, 1)
		at := func(i int) string { return d2.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339) }
		base := int64(1_000_000 + 96*100) // önceki günün son endeksi
		var rs []readingT
		for i := 1; i <= 60; i++ {
			if i >= 20 && i <= 23 { // 4 okuma eksik → 5 slot interpolasyon
				continue
			}
			v := base + int64(i)*100
			if i >= 24 {
				v++ // boşlukta 501 Wh: 100+100+100+100+101
			}
			if i == 10 {
				v += 250 // sayaç önce hatalı değer gönderir; aşağıda geriye dönük düzeltilir
			}
			rs = append(rs, readingT{meterID, at(i), v, 0})
		}
		rs = append(rs, rs[5]) // parti içi tekrar
		res := ingestReadings(rs)
		expect(num(res["duplicates"]) == 1, "tekrar %v", res)
		// Geriye dönük düzeltme: hatalı 10. okumanın doğrusu gelir.
		res = ingestReadings([]readingT{{meterID, at(10), base + 1000, 0}})
		expect(num(res["corrections"]) == 1, "düzeltme %v", res)
		// Sayaç değişimi (saha ekibi): 15:00'te kapanış endeksi.
		closeIdx := base + 60*100 + 1 + 40
		r := call("POST", fmt.Sprintf("/api/v1/subscribers/%d/meter-change", subID), login("saha"),
			map[string]any{"at": at(61), "final_index_wh": closeIdx, "new_serial_no": fmt.Sprintf("VRF-NEW-%d", runID), "initial_index_wh": 0})
		expectStatus(r, 201, "sayaç değişimi")
		newMeter := int64(num(r.J()["id"]))
		var nrs []readingT
		for i := 62; i <= 96; i++ {
			nrs = append(nrs, readingT{newMeter, at(i), int64(i-61) * 100, 0})
		}
		ingestReadings(nrs)
		recompute()
		r = call("GET", fmt.Sprintf("/api/v1/subscribers/%d/consumption?granularity=15m&from=%s&to=%s", subID, d2.Format("2006-01-02"), d2.AddDate(0, 0, 1).Format("2006-01-02")), login("fatura"), nil)
		expectStatus(r, 200, "15 dk tüketim")
		var out struct {
			TotalWh int64          `json:"total_wh"`
			Q       map[string]int `json:"quality_counts"`
			Items   []struct {
				ActiveWh int64 `json:"active_wh"`
				Quality  string
			}
		}
		_ = r.Into(&out)
		// slot 0..95: 60 slot eski sayaç (501'lik boşluk dahil; düzeltme sonrası tutarlı) + 1 slot kapanış (40) + 1 slot yeni sayaç açılış (100) + 34 slot × 100
		want := int64(60*100+1) + 40 + 100 + 34*100
		expect(out.TotalWh == want, "toplam %d, beklenen %d (q=%v)", out.TotalWh, want, out.Q)
		expect(out.Q["interpolated"] == 5, "interpolasyon %d", out.Q["interpolated"])
		expect(out.Q["meter_change"] == 2, "sayaç değişimi slotu %d", out.Q["meter_change"])
		expect(out.Q["corrected"] >= 1, "düzeltilmiş slot yok")
		return fmt.Sprintf("toplam %d Wh birebir; %d interpolasyon, %d sayaç değişimi, %d düzeltilmiş slot", out.TotalWh, out.Q["interpolated"], out.Q["meter_change"], out.Q["corrected"])
	})

	check("F-4", "Tarife ve fatura (tahmini → gerçek + mahsup, döküm)", func() string {
		// Eylül için ikinci bir abone: 1-20 Eylül ölçülü → tahmini, sonra tamamı → gerçek.
		s2, m2 := newSubscriber(pole, time.Date(2026, 8, 1, 0, 0, 0, 0, ist))
		sep := time.Date(2026, 9, 1, 0, 0, 0, 0, ist)
		var rs []readingT
		idx := int64(500_000)
		for i := 0; i <= 20*96; i++ {
			rs = append(rs, readingT{m2, sep.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339), idx, idx / 10})
			idx += 120
		}
		ingestReadings(rs)
		recompute()
		gen := func() resp {
			return call("POST", "/api/v1/billing/invoices/generate", login("fatura"), map[string]any{"subscriber_id": s2, "period": "2026-09"})
		}
		r := gen()
		expectStatus(r, 201, "tahmini fatura")
		var est struct {
			Invoice struct {
				ID          int64
				Kind        string
				TotalKurus  int64 `json:"total_kurus"`
				EstimatedWh int64 `json:"estimated_wh"`
				Breakdown   []struct{ Estimated bool }
			}
		}
		_ = r.Into(&est)
		expect(est.Invoice.Kind == "estimated" && est.Invoice.EstimatedWh > 0, "tür %s", est.Invoice.Kind)
		pay := call("POST", fmt.Sprintf("/api/v1/invoices/%d/payments", est.Invoice.ID), login("fatura"), map[string]any{"amount_kurus": 5000},
			"Idempotency-Key", fmt.Sprintf("verify-pay-%d", runID))
		expectStatus(pay, 201, "tahmini fatura ödemesi")
		rs = rs[:0]
		for i := 20*96 + 1; i <= 30*96; i++ {
			rs = append(rs, readingT{m2, sep.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339), idx, idx / 10})
			idx += 120
		}
		ingestReadings(rs)
		recompute()
		r = gen()
		expectStatus(r, 201, "gerçek fatura")
		var act struct {
			Invoice struct {
				Kind             string
				SettlesInvoiceID *int64 `json:"settles_invoice_id"`
				ConsumptionWh    int64  `json:"consumption_wh"`
				Lines            []struct {
					Kind        string
					AmountKurus int64 `json:"amount_kurus"`
				}
			}
		}
		_ = r.Into(&act)
		last := act.Invoice.Lines[len(act.Invoice.Lines)-1]
		expect(act.Invoice.Kind == "actual" && act.Invoice.SettlesInvoiceID != nil && *act.Invoice.SettlesInvoiceID == est.Invoice.ID, "mahsup bağlantısı yok")
		expect(last.Kind == "settlement" && last.AmountKurus == -5000, "mahsup satırı %+v", last)
		expect(act.Invoice.ConsumptionWh == 30*96*120, "tüketim %d", act.Invoice.ConsumptionWh)
		return fmt.Sprintf("tahmini %d kr → gerçek fatura, ödenen 50,00 TL mahsup edildi; 345,6 kWh", est.Invoice.TotalKurus)
	})
	check("T-2", "Tarife motoru: dönem ortası değişiklik, deterministik yeniden üretim, reissue", func() string {
		t := login("fatura")
		r := call("GET", "/api/v1/subscribers/1/invoice-preview?period=2026-10", t, nil)
		expectStatus(r, 200, "önizleme")
		var p struct {
			Result struct {
				Lines []struct {
					Kind        string
					Description string
				}
			}
		}
		_ = r.Into(&p)
		vats, tiers := 0, 0
		for _, l := range p.Result.Lines {
			if l.Kind == "vat" {
				vats++
			}
			if l.Kind == "energy" {
				tiers++
			}
		}
		expect(vats == 2, "Ekim faturasında 15 Ekim tarife değişikliği iki parçaya bölünmeli, VAT satırı %d", vats)
		// Aynı dönem iki kez: aynı fatura
		gen := func(sub int64, reissue bool) resp {
			return call("POST", "/api/v1/billing/invoices/generate", t, map[string]any{"subscriber_id": sub, "period": "2026-09", "reissue": reissue})
		}
		a, b := gen(subID, false), gen(subID, false)
		expect(a.Status == 201 || a.Status == 200, "ilk üretim %d %s", a.Status, trunc(string(a.Body), 200))
		expectStatus(b, 200, "yeniden üretim")
		ia, ib := a.J()["invoice"].(map[string]any), b.J()["invoice"].(map[string]any)
		expect(ia["id"] == ib["id"] && ia["total_kurus"] == ib["total_kurus"] && ia["input_hash"] == ib["input_hash"], "yeniden üretim farklı")
		expect(b.J()["created"] == false, "ikinci üretim yeni fatura kesti")
		return fmt.Sprintf("Ekim: %d enerji satırı, 2 KDV (14+17 gün); yeniden üretim aynı id/tutar/özet", tiers)
	})

	check("RACE", "Eşzamanlı ödemeler bakiyeyi aşamaz (FOR UPDATE)", func() string {
		t := login("fatura")
		r := call("GET", fmt.Sprintf("/api/v1/subscribers/%d/invoices", subID), t, nil)
		var l struct {
			Items []struct {
				ID         int64
				Status     string
				TotalKurus int64 `json:"total_kurus"`
				PaidKurus  int64 `json:"paid_kurus"`
			}
		}
		_ = r.Into(&l)
		var id, open int64
		for _, i := range l.Items {
			if i.Status == "issued" && i.TotalKurus-i.PaidKurus > 100 {
				id, open = i.ID, i.TotalKurus-i.PaidKurus
			}
		}
		expect(id > 0, "ödenebilir fatura yok")
		part := open*6/10 + 1
		var wg sync.WaitGroup
		st := make([]int, 2)
		for i := range st {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				st[i] = call("POST", fmt.Sprintf("/api/v1/invoices/%d/payments", id), t, map[string]any{"amount_kurus": part},
					"Idempotency-Key", fmt.Sprintf("verify-race-%d-%d", runID, i)).Status
			}(i)
		}
		wg.Wait()
		expect((st[0] == 201 && st[1] == 409) || (st[0] == 409 && st[1] == 201), "durumlar %v", st)
		return fmt.Sprintf("%%60+%%60 iki eşzamanlı ödeme: %v (biri kabul, biri 409)", st)
	})
	check("IDEMP", "Idempotency-Key: aynı anahtar → aynı cevap, farklı gövde → 422", func() string {
		t := login("admin")
		key := fmt.Sprintf("verify-idem-%d", runID)
		body := map[string]any{"email": fmt.Sprintf("v%d@enerjihat.local", runID), "full_name": "V", "password": "Parola123!", "role": "analyst"}
		a := call("POST", "/api/v1/admin/users", t, body, "Idempotency-Key", key)
		b := call("POST", "/api/v1/admin/users", t, body, "Idempotency-Key", key)
		expect(a.Status == 201 && b.Status == 201 && string(a.Body) == string(b.Body) && b.Header.Get("Idempotent-Replayed") == "true", "tekrar oynatma")
		body["full_name"] = "Başka"
		c := call("POST", "/api/v1/admin/users", t, body, "Idempotency-Key", key)
		expect(c.Status == 422 && c.Code() == "idempotency_key_mismatch", "farklı gövde %d", c.Status)
		p := call("POST", "/api/v1/invoices/1/payments", t, map[string]any{"amount_kurus": 1})
		expect(p.Status == 400, "ödeme anahtarsız kabul edildi: %d", p.Status)
		return "tekrar → aynı gövde + Idempotent-Replayed; farklı gövde 422; ödemede anahtar zorunlu"
	})

	nodes := tree()
	check("F-5", "Şebeke topolojisi: trafo → besleyici → direk → abone", func() string {
		kinds := map[string]int{}
		subs := 0
		for _, n := range nodes {
			kinds[n.Kind]++
			if n.Kind == "transformer" {
				subs += n.Subscribers
			}
		}
		expect(kinds["transformer"] > 0 && kinds["feeder"] == 3*kinds["transformer"] && kinds["pole"] > 0, "düğümler %v", kinds)
		var f node
		for _, n := range nodes {
			if n.Kind == "feeder" {
				f = n
				break
			}
		}
		r := call("GET", fmt.Sprintf("/api/v1/network/nodes/%d/impact", f.ID), login("operator"), nil)
		expectStatus(r, 200, "etki")
		expect(int(num(r.J()["subscribers"])) == f.Subscribers, "etki %v ≠ ağaç %d", r.J()["subscribers"], f.Subscribers)
		return fmt.Sprintf("%d trafo, %d besleyici, %d direk, %d abone; %s etkisi %d abone", kinds["transformer"], kinds["feeder"], kinds["pole"], subs, f.Code, f.Subscribers)
	})
	check("T-3", "Kesinti etkisi, anahtarlama, döngü engeli, planlı çakışma, SAIDI/SAIFI", func() string {
		op := login("operator")
		byID := map[int64]node{}
		for _, n := range nodes {
			byID[n.ID] = n
		}
		var feeder, pole1, child node
		for _, n := range nodes {
			if n.Kind == "pole" && n.ParentID != nil && byID[*n.ParentID].Kind == "pole" {
				child, pole1 = n, byID[*n.ParentID]
				feeder = byID[*pole1.ParentID]
				for feeder.Kind != "feeder" && feeder.ParentID != nil {
					feeder = byID[*feeder.ParentID]
				}
				if !feeder.Switched {
					break
				}
			}
		}
		r := call("POST", "/api/v1/network/switch", op, map[string]any{"node_id": pole1.ID, "new_parent_id": child.ID})
		expect(r.Status == 409 && r.Code() == "topology_cycle", "döngü: %d %s", r.Status, r.Code())
		r = call("POST", "/api/v1/network/switch", op, map[string]any{"node_id": feeder.ID, "new_parent_id": *feeder.AltParentID})
		expectStatus(r, 200, "yedek beslemeye anahtarlama")
		moved := num(r.J()["moved_subscribers"])
		expectStatus(call("POST", fmt.Sprintf("/api/v1/network/nodes/%d/restore-normal", feeder.ID), op, nil), 200, "normale dönüş")
		// Planlı kesinti çakışması (gelecekte, bu çalıştırmaya özgü tarih).
		st := time.Now().Add(time.Duration(400+rng.IntN(20000)) * time.Hour).Truncate(time.Hour)
		plan := func(id int64, from, to time.Time) resp {
			return call("POST", "/api/v1/outages", op, map[string]any{"kind": "planned", "fault_node_id": id, "cause": "doğrulama bakımı",
				"planned_start": from.Format(time.RFC3339), "planned_end": to.Format(time.RFC3339)})
		}
		p1 := plan(feeder.ID, st, st.Add(2*time.Hour))
		expectStatus(p1, 201, "planlı kesinti")
		p2 := plan(pole1.ID, st.Add(time.Hour), st.Add(3*time.Hour))
		expect(p2.Status == 409 && p2.Code() == "planned_overlap", "alt düğüm çakışması: %d", p2.Status)
		// iptal edip temizle
		call("POST", fmt.Sprintf("/api/v1/outages/%d/transition", int64(num(p1.J()["id"]))), op, map[string]any{"to": "cancelled"})
		rel := call("GET", "/api/v1/reports/reliability?from=2026-01-01&to=2027-01-01", login("analist"), nil)
		expectStatus(rel, 200, "SAIDI/SAIFI")
		j := rel.J()
		expect(num(j["customers_served"]) > 0, "hizmet verilen abone 0")
		return fmt.Sprintf("döngü 409; %s yedeğe aktarıldı (%.0f abone) ve geri alındı; alt düğüm planlı çakışma 409; SAIDI %.2f dk, SAIFI %.3f",
			feeder.Code, moved, num(j["saidi_minutes"]), num(j["saifi"]))
	})

	check("F-6", "Kesinti kayıtları ve ekip yönlendirme", func() string {
		op := login("operator")
		r := call("GET", "/api/v1/crews", op, nil)
		expectStatus(r, 200, "ekipler")
		r = call("GET", "/api/v1/outages?limit=5", op, nil)
		expectStatus(r, 200, "kesinti listesi")
		var l struct {
			Items []struct{ ID int64 }
		}
		_ = r.Into(&l)
		expect(len(l.Items) > 0, "hiç kesinti yok")
		d := call("GET", fmt.Sprintf("/api/v1/outages/%d", l.Items[0].ID), op, nil)
		expectStatus(d, 200, "kesinti detayı")
		bad := call("POST", fmt.Sprintf("/api/v1/outages/%d/transition", l.Items[0].ID), op, map[string]any{"to": "bogus"})
		expect(bad.Status == 409, "geçersiz geçiş %d", bad.Status)
		return fmt.Sprintf("%d kesinti listelendi; detay + geçmiş; geçersiz durum geçişi 409", len(l.Items))
	})
	check("T-5", "Son nefes + arama kümeleme, mükerrer, ekip atama, restore ile kapanış", func() string {
		op := login("operator")
		// Açık kesintisi olmayan, simülatörün dokunmadığı bir besleyici bul.
		r := call("GET", "/api/v1/outages?open=true&limit=200", op, nil)
		var open struct {
			Items []struct {
				FeederID int64 `json:"feeder_id"`
			}
		}
		_ = r.Into(&open)
		busy := map[int64]bool{}
		for _, o := range open.Items {
			busy[o.FeederID] = true
		}
		var subs []int64
		var feeder node
		byID := map[int64]node{}
		for _, n := range nodes {
			byID[n.ID] = n
		}
		cursor := ""
		for len(subs) < 3 {
			path := "/api/v1/subscribers?limit=200"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			rr := call("GET", path, op, nil)
			var page struct {
				Items []struct {
					ID     int64
					PoleID int64 `json:"pole_id"`
				}
				Next *string `json:"next_cursor"`
			}
			_ = rr.Into(&page)
			byFeeder := map[int64][]int64{}
			for _, s := range page.Items {
				n := byID[s.PoleID]
				for n.Kind != "feeder" && n.ParentID != nil {
					n = byID[*n.ParentID]
				}
				if !busy[n.ID] && n.Kind == "feeder" {
					byFeeder[n.ID] = append(byFeeder[n.ID], s.ID)
				}
			}
			for f, l := range byFeeder {
				if len(l) >= 3 {
					subs, feeder = l[:3], byID[f]
					break
				}
			}
			expect(page.Next != nil || len(subs) >= 3, "uygun besleyici bulunamadı")
			if page.Next != nil {
				cursor = *page.Next
			}
		}
		now := time.Now()
		sig := func(src string, sub int64, k int, at time.Time) resp {
			return call("POST", "/api/v1/outage-signals", "", map[string]any{"source": src, "subscriber_id": sub,
				"external_id": fmt.Sprintf("verify-%d-%s-%d", runID, src, k), "received_at": at.Format(time.RFC3339)}, "X-Ingest-Token", ingest)
		}
		var outage int64
		for i, s := range subs {
			rr := sig("last_gasp", s, i, now.Add(time.Duration(i)*time.Second))
			expect(rr.Status == 201 || rr.Status == 202, "sinyal %d", rr.Status)
			id := int64(num(rr.J()["outage_id"]))
			if outage == 0 {
				outage = id
			}
			expect(id == outage, "aynı besleyici/pencere iki ayrı olay: %d vs %d", id, outage)
		}
		dup := sig("call", subs[0], 99, now.Add(time.Minute))
		expect(dup.J()["duplicate"] == true && int64(num(dup.J()["outage_id"])) == outage, "mükerrer arama bağlanmadı %v", dup.J())
		d := call("GET", fmt.Sprintf("/api/v1/outages/%d", outage), op, nil).J()["outage"].(map[string]any)
		status := d["status"].(string)
		expect(status == "confirmed" || status == "assigned", "3 abone sonrası durum %s", status)
		assigned := status == "assigned"
		if !assigned {
			dr := call("POST", fmt.Sprintf("/api/v1/outages/%d/dispatch", outage), op, nil)
			expect(dr.Status == 201 || (dr.Status == 409 && dr.Code() == "no_crew_available"), "atama %d %s", dr.Status, dr.Code())
			assigned = dr.Status == 201
		}
		// Etkilenen tüm abonelerden "enerji geldi" → kesinti kendiliğinden çözülür.
		affected := int(num(call("GET", fmt.Sprintf("/api/v1/network/nodes/%d/impact", int64(num(d["fault_node_id"]))), op, nil).J()["subscribers"]))
		// Etkilenen aboneleri kesinti etki listesinden değil, tüm aboneler içinden fault düğümünün altındakiler olarak bul.
		fault := int64(num(d["fault_node_id"]))
		under := func(pole int64) bool {
			for n := byID[pole]; ; n = byID[*n.ParentID] {
				if n.ID == fault {
					return true
				}
				if n.ParentID == nil {
					return false
				}
			}
		}
		var restoreSubs []int64
		cursor = ""
		for {
			path := "/api/v1/subscribers?limit=200"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			var page struct {
				Items []struct {
					ID     int64
					PoleID int64 `json:"pole_id"`
				}
				Next *string `json:"next_cursor"`
			}
			_ = call("GET", path, op, nil).Into(&page)
			for _, s := range page.Items {
				if under(s.PoleID) {
					restoreSubs = append(restoreSubs, s.ID)
				}
			}
			if page.Next == nil || len(restoreSubs) >= affected {
				break
			}
			cursor = *page.Next
		}
		var batch []map[string]any
		for i, s := range restoreSubs {
			batch = append(batch, map[string]any{"source": "restore", "subscriber_id": s, "external_id": fmt.Sprintf("verify-%d-rs-%d", runID, i),
				"received_at": time.Now().Format(time.RFC3339)})
		}
		expectStatus(call("POST", "/api/v1/outage-signals/batch", "", map[string]any{"signals": batch}, "X-Ingest-Token", ingest), 202, "restore")
		final := call("GET", fmt.Sprintf("/api/v1/outages/%d", outage), op, nil).J()["outage"].(map[string]any)
		expect(final["status"] == "resolved", "restore sonrası durum %v", final["status"])
		return fmt.Sprintf("%s: 3 sinyal + 1 mükerrer → kesinti #%d (%d abone), ekip atandı=%v, %d restore ile çözüldü", feeder.Code, outage, affected, assigned, len(restoreSubs))
	})

	check("F-7", "Kayıp-kaçak raporları", func() string {
		an := login("analist")
		r := call("POST", "/api/v1/analytics/loss/run", an, map[string]any{})
		expectStatus(r, 200, "analiz")
		tr := call("GET", fmt.Sprintf("/api/v1/analytics/loss/transformers?from=%s&to=%s", localMidnight(7).Format("2006-01-02"), localMidnight(0).Format("2006-01-02")), an, nil)
		expectStatus(tr, 200, "trafo raporu")
		var t struct {
			Items []struct {
				Code       string
				ExcessDays int `json:"excess_days"`
				Days       int
			}
		}
		_ = tr.Into(&t)
		expect(len(t.Items) > 0 && t.Items[0].Days > 0, "trafo dengesi boş")
		return fmt.Sprintf("%d trafo; en kayıplı %s (%d gün tolerans üstü)", len(t.Items), t.Items[0].Code, t.Items[0].ExcessDays)
	})
	check("T-4", "Kayıp-kaçak tespiti: kural + robust z, gerekçe, kalite", func() string {
		an := login("analist")
		r := call("GET", "/api/v1/analytics/suspects?limit=50", an, nil)
		expectStatus(r, 200, "şüpheliler")
		var s struct {
			Items []struct {
				Rule        string
				Explanation map[string]any
			}
		}
		_ = r.Into(&s)
		expect(len(s.Items) > 0, "şüpheli yok")
		rules := map[string]int{}
		for _, it := range s.Items {
			rules[it.Rule]++
			reason, _ := it.Explanation["reason"].(string)
			expect(reason != "", "gerekçesiz işaret")
		}
		expect(rules["sudden_drop"] > 0 && rules["night_usage"] > 0, "kurallar %v", rules)
		ev := call("GET", "/api/v1/analytics/loss/evaluation", an, nil)
		expectStatus(ev, 200, "değerlendirme")
		j := ev.J()
		expect(num(j["recall"]) >= 0.8 && num(j["precision"]) >= 0.8, "precision %.2f recall %.2f", num(j["precision"]), num(j["recall"]))
		return fmt.Sprintf("ani düşüş %d, gece %d; simülatör yer gerçeğine göre precision %.2f, recall %.2f", rules["sudden_drop"], rules["night_usage"], num(j["precision"]), num(j["recall"]))
	})

	check("AUDIT", "Denetim kaydı (kim/ne/eski/yeni)", func() string {
		r := call("GET", "/api/v1/admin/audit?limit=200", login("admin"), nil)
		expectStatus(r, 200, "denetim")
		var a struct {
			Items []struct{ Action string }
		}
		_ = r.Into(&a)
		seen := map[string]bool{}
		for _, i := range a.Items {
			seen[i.Action] = true
		}
		for _, want := range []string{"invoice.issue", "payment.create", "topology.switch", "meter.replace", "subscriber.create"} {
			expect(seen[want], "%s kaydı yok", want)
		}
		return "invoice.issue, payment.create, topology.switch, meter.replace, subscriber.create kayıtlı"
	})
	check("OBS", "/metrics (Prometheus) ve OpenAPI", func() string {
		m := call("GET", "/metrics", "", nil)
		expect(m.Status == 200 && strings.Contains(string(m.Body), "enerjihat_http_requests_total") &&
			strings.Contains(string(m.Body), "enerjihat_readings_total"), "metrikler eksik")
		o := call("GET", "/openapi.yaml", "", nil)
		expect(o.Status == 200 && strings.Contains(string(o.Body), "openapi: 3"), "openapi %d", o.Status)
		return "metrikler + /openapi.yaml"
	})
	check("LIVE", "Veri üretici canlı (son okuma partisi)", func() string {
		r := call("GET", "/api/v1/live", login("operator"), nil)
		expectStatus(r, 200, "live")
		last, _ := r.J()["last_batch_at"].(string)
		t, err := time.Parse(time.RFC3339Nano, last)
		expect(err == nil && time.Since(t) < 20*time.Minute, "son parti %q", last)
		return fmt.Sprintf("son parti %s önce, son 1 sa %0.f okuma", time.Since(t).Round(time.Second), num(r.J()["readings_last_hour"]))
	})
	if web != "" {
		check("WEB", "Arayüz ve API vekili (frontend)", func() string {
			r := call("GET", web+"/", "", nil)
			expect(r.Status == 200 && strings.Contains(string(r.Body), "EnerjiHat"), "arayüz %d", r.Status)
			h := call("GET", web+"/health", "", nil)
			expect(h.Status == 200, "frontend → api vekili %d", h.Status)
			l := call("GET", web+"/subscribers/1", "", nil)
			expect(l.Status == 200, "SPA yönlendirme %d", l.Status)
			return "index.html, /health vekili, SPA derin bağlantı"
		})
	}
}
