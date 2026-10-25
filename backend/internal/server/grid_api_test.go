package server

import (
	"context"
	"fmt"
	"testing"
)

func (e *env) nodeID(code string) int64 {
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM network_nodes WHERE code=$1`, code).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestGridAndOutageAPI(t *testing.T) {
	e := newEnv(t)
	op := e.login("operator@enerjihat.local")
	an := e.login("analist@enerjihat.local")
	saha := e.login("saha@enerjihat.local")
	abone := e.login("abone@enerjihat.local")

	var tree struct{ Items []struct{ Kind string } }
	if r := e.do("GET", "/api/v1/network/tree", saha.AccessToken, nil); r.Status != 200 {
		t.Fatalf("tree for field crew: %d", r.Status)
	} else {
		r.json(t, &tree)
	}
	if len(tree.Items) != 84 {
		t.Fatalf("tree nodes %d", len(tree.Items))
	}
	if r := e.do("GET", "/api/v1/network/tree", abone.AccessToken, nil); r.Status != 403 {
		t.Fatalf("tree for subscriber: %d", r.Status)
	}

	// Abone 1'in direğini bul ve o direkte kesinti aç.
	var pole int64
	_ = e.pool.QueryRow(context.Background(), `SELECT pole_id FROM subscribers WHERE id=1`).Scan(&pole)
	var imp struct{ Subscribers int }
	e.do("GET", fmt.Sprintf("/api/v1/network/nodes/%d/impact", pole), op.AccessToken, nil).json(t, &imp)
	if imp.Subscribers < 1 {
		t.Fatalf("impact %+v", imp)
	}
	create := map[string]any{"kind": "unplanned", "fault_node_id": pole, "cause": "direk devrildi"}
	if r := e.do("POST", "/api/v1/outages", an.AccessToken, create); r.Status != 403 {
		t.Fatalf("analyst creating outage: %d", r.Status)
	}
	r := e.do("POST", "/api/v1/outages", op.AccessToken, create)
	var o struct {
		ID            int64
		AffectedCount int `json:"affected_count"`
		Version       int
	}
	r.json(t, &o)
	if r.Status != 201 || o.AffectedCount < imp.Subscribers {
		t.Fatalf("create outage: %d %s", r.Status, r.Body)
	}
	var mine struct{ Items []struct{ ID int64 } }
	e.do("GET", "/api/v1/me/outages", abone.AccessToken, nil).json(t, &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != o.ID {
		t.Fatalf("subscriber outages %+v", mine)
	}
	path := fmt.Sprintf("/api/v1/outages/%d/transition", o.ID)
	if r := e.do("POST", path, op.AccessToken, map[string]any{"to": "in_progress"}); r.Status != 409 || r.errCode(t) != "invalid_transition" {
		t.Fatalf("invalid transition: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", path, op.AccessToken, map[string]any{"to": "confirmed", "version": o.Version + 3}); r.Status != 409 || r.errCode(t) != "stale_version" {
		t.Fatalf("stale version: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", path, op.AccessToken, map[string]any{"to": "confirmed", "version": o.Version}); r.Status != 200 {
		t.Fatalf("confirm: %d %s", r.Status, r.Body)
	}
	// Anahtarlama: döngü → 409, analist → 403, geçerli → 200
	f1, p1 := e.nodeID("TR-0001-F1"), e.nodeID("TR-0001-F1-P01")
	if r := e.do("POST", "/api/v1/network/switch", op.AccessToken, map[string]any{"node_id": f1, "new_parent_id": p1}); r.Status != 422 {
		t.Fatalf("feeder under pole: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/api/v1/network/switch", an.AccessToken, map[string]any{"node_id": f1, "new_parent_id": e.nodeID("TR-0002")}); r.Status != 403 {
		t.Fatalf("analyst switching: %d", r.Status)
	}
	if r := e.do("POST", "/api/v1/network/switch", op.AccessToken, map[string]any{"node_id": f1, "new_parent_id": e.nodeID("TR-0002")}); r.Status != 200 {
		t.Fatalf("switch: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", fmt.Sprintf("/api/v1/network/nodes/%d/restore-normal", f1), op.AccessToken, nil); r.Status != 200 {
		t.Fatalf("restore normal: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", path, op.AccessToken, map[string]any{"to": "resolved"}); r.Status != 200 {
		t.Fatalf("resolve: %d %s", r.Status, r.Body)
	}
	var rel struct {
		CustomersServed int64   `json:"customers_served"`
		SAIDI           float64 `json:"saidi_minutes"`
	}
	r = e.do("GET", "/api/v1/reports/reliability?from=2026-01-01&to=2027-01-01", an.AccessToken, nil)
	r.json(t, &rel)
	if r.Status != 200 || rel.CustomersServed != 600 {
		t.Fatalf("reliability: %d %s", r.Status, r.Body)
	}
	var detail struct {
		History []struct {
			To string `json:"to_status"`
		}
	}
	e.do("GET", fmt.Sprintf("/api/v1/outages/%d", o.ID), op.AccessToken, nil).json(t, &detail)
	if len(detail.History) != 3 {
		t.Fatalf("history %+v", detail)
	}
}

func TestOutageListWithoutFilter(t *testing.T) {
	e := newEnv(t)
	op := e.login("operator@enerjihat.local")
	e.do("POST", "/api/v1/outages", op.AccessToken, map[string]any{"kind": "unplanned", "fault_node_id": e.nodeID("TR-0001-F1")})
	var all struct{ Items []any }
	e.do("GET", "/api/v1/outages?limit=5", op.AccessToken, nil).json(t, &all)
	if len(all.Items) != 1 {
		t.Fatalf("unfiltered outage list returned %d items", len(all.Items))
	}
}
