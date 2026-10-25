package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/loss"
)

func TestLossAPI(t *testing.T) {
	e := newEnv(t)
	asOf := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	truth, err := loss.Plant(context.Background(), e.pool, asOf, 3)
	if err != nil {
		t.Fatal(err)
	}
	an := e.login("analist@enerjihat.local")
	op := e.login("operator@enerjihat.local")
	if r := e.do("POST", "/api/v1/analytics/loss/run", op.AccessToken, map[string]any{"as_of": "2026-10-01"}); r.Status != 403 {
		t.Fatalf("operator running analysis: %d", r.Status)
	}
	r := e.do("POST", "/api/v1/analytics/loss/run", an.AccessToken, map[string]any{"as_of": "2026-10-01"})
	var sum struct {
		Flags []struct {
			SubscriberID int64 `json:"subscriber_id"`
		}
	}
	r.json(t, &sum)
	if r.Status != 200 || len(sum.Flags) != len(truth.Positives()) {
		t.Fatalf("run: %d flags=%d want %d", r.Status, len(sum.Flags), len(truth.Positives()))
	}
	var page struct {
		Items []struct {
			ID          int64
			Score       float64
			Explanation map[string]any
		}
		Next *string `json:"next_cursor"`
	}
	e.do("GET", "/api/v1/analytics/suspects?limit=5", an.AccessToken, nil).json(t, &page)
	if len(page.Items) != 5 || page.Next == nil || page.Items[0].Score < page.Items[4].Score || page.Items[0].Explanation["reason"] == nil {
		t.Fatalf("suspects page %+v", page)
	}
	var page2 struct{ Items []struct{ ID int64 } }
	e.do("GET", "/api/v1/analytics/suspects?limit=5&cursor="+*page.Next, an.AccessToken, nil).json(t, &page2)
	if len(page2.Items) != len(sum.Flags)-5 {
		t.Fatalf("second page %d", len(page2.Items))
	}
	path := fmt.Sprintf("/api/v1/analytics/suspects/%d/review", page.Items[0].ID)
	if r := e.do("POST", path, an.AccessToken, map[string]any{"status": "confirmed", "note": "saha kontrolünde baypas bulundu"}); r.Status != 200 {
		t.Fatalf("review: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", path, an.AccessToken, map[string]any{"status": "dismissed"}); r.Status != 409 {
		t.Fatalf("second review: %d", r.Status)
	}
	var trs struct {
		Items []struct {
			Code       string
			ExcessDays int64 `json:"excess_days"`
		}
	}
	e.do("GET", "/api/v1/analytics/loss/transformers?from=2026-09-01&to=2026-10-01", op.AccessToken, nil).json(t, &trs)
	if len(trs.Items) != 3 || trs.Items[0].Code != "TR-0001" || trs.Items[0].ExcessDays < 5 {
		t.Fatalf("transformer report %+v", trs)
	}
}
