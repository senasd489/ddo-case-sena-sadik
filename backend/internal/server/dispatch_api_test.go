package server

import (
	"fmt"
	"testing"
	"time"
)

func TestSignalDispatchAndCrewAPI(t *testing.T) {
	e := newEnv(t)
	op := e.login("operator@enerjihat.local")
	an := e.login("analist@enerjihat.local")
	saha := e.login("saha@enerjihat.local") // EKIP-01'e bağlı
	// Saha ekibi 1 dışındaki tüm ekipleri izne çıkar → atama mutlaka EKIP-01'e gider.
	_, _ = e.pool.Exec(t.Context(), `UPDATE crews SET status='off_duty' WHERE id <> 1`)

	at := time.Now().Add(-5 * time.Minute)
	var outageID int64
	for i, sub := range []int64{1, 2, 3} {
		body := map[string]any{"source": "call", "subscriber_id": sub, "external_id": fmt.Sprintf("cc-%d", i), "received_at": at.Add(time.Duration(i) * time.Minute)}
		// İlk iki bildirim çağrı merkezi entegrasyonundan (token), üçüncüsü operatörden.
		var r resp
		if i < 2 {
			r = e.do("POST", "/api/v1/outage-signals", "", body, "X-Ingest-Token", testIngestToken)
		} else {
			r = e.do("POST", "/api/v1/outage-signals", op.AccessToken, body)
		}
		var res struct {
			OutageID int64  `json:"outage_id"`
			Status   string `json:"outage_status"`
		}
		r.json(t, &res)
		if r.Status != 201 && r.Status != 202 {
			t.Fatalf("signal %d: %d %s", i, r.Status, r.Body)
		}
		outageID = res.OutageID
	}
	if r := e.do("POST", "/api/v1/outage-signals", an.AccessToken, map[string]any{"source": "call", "subscriber_id": 1}); r.Status != 403 {
		t.Fatalf("analyst signal: %d", r.Status)
	}
	var sigs struct{ Items []any }
	e.do("GET", fmt.Sprintf("/api/v1/outages/%d/signals", outageID), op.AccessToken, nil).json(t, &sigs)
	if len(sigs.Items) == 0 {
		t.Fatal("no signals attached")
	}
	// Abone 1-3 farklı besleyicilerde olabilir; atanacak kesintiyi garanti onayla.
	e.do("POST", fmt.Sprintf("/api/v1/outages/%d/transition", outageID), op.AccessToken, map[string]any{"to": "confirmed"})

	if r := e.do("POST", fmt.Sprintf("/api/v1/outages/%d/dispatch", outageID), an.AccessToken, nil); r.Status != 403 {
		t.Fatalf("analyst dispatch: %d", r.Status)
	}
	r := e.do("POST", fmt.Sprintf("/api/v1/outages/%d/dispatch", outageID), op.AccessToken, nil)
	if r.Status != 201 {
		t.Fatalf("dispatch: %d %s", r.Status, r.Body)
	}
	var me struct {
		Crew   struct{ Status string }
		Outage *struct{ ID int64 }
	}
	e.do("GET", "/api/v1/crew/me", saha.AccessToken, nil).json(t, &me)
	if me.Crew.Status != "busy" || me.Outage == nil || me.Outage.ID != outageID {
		t.Fatalf("crew/me %+v", me)
	}
	if r := e.do("POST", "/api/v1/crew/me/transition", saha.AccessToken, map[string]any{"to": "in_progress"}); r.Status != 200 {
		t.Fatalf("crew in_progress: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/api/v1/crew/me/transition", saha.AccessToken, map[string]any{"to": "resolved", "note": "sigorta değişti"}); r.Status != 200 {
		t.Fatalf("crew resolve: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/api/v1/crew/me/transition", saha.AccessToken, map[string]any{"to": "resolved"}); r.Status != 409 {
		t.Fatalf("no assignment: %d", r.Status)
	}
	var crews struct {
		Items []struct{ Status string }
	}
	e.do("GET", "/api/v1/crews", op.AccessToken, nil).json(t, &crews)
	if crews.Items[0].Status != "available" {
		t.Fatalf("crew 1 after resolve %+v", crews.Items[0])
	}
}
