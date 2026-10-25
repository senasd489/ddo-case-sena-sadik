package grid

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

var ctx = context.Background()

func nodeID(t *testing.T, pool *pgxpool.Pool, code string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `SELECT id FROM network_nodes WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("node %s: %v", code, err)
	}
	return id
}

func subsUnder(t *testing.T, pool *pgxpool.Pool, prefix string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM subscribers s JOIN network_nodes p ON p.id = s.pole_id WHERE p.code LIKE $1 || '%'`, prefix).Scan(&n)
	return n
}

func tx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	t.Helper()
	return pgx.BeginFunc(ctx, pool, fn)
}

func TestTreeCountsSubscribersPerSubtree(t *testing.T) {
	pool := testutil.NewDB(t)
	nodes, err := Tree(ctx, pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 3*(1+3*(1+8)) {
		t.Fatalf("nodes %d", len(nodes))
	}
	var roots int64
	byCode := map[string]Node{}
	for _, n := range nodes {
		byCode[n.Code] = n
		if n.Kind == "transformer" {
			roots += n.Subscribers
		}
	}
	if roots != 600 {
		t.Fatalf("transformer totals %d", roots)
	}
	if got, want := byCode["TR-0001-F2"].Subscribers, int64(subsUnder(t, pool, "TR-0001-F2-")); got != want {
		t.Fatalf("feeder subtree %d want %d", got, want)
	}
	one, _ := Tree(ctx, pool, byCode["TR-0002"].ID)
	if len(one) != 28 || one[0].Code != "TR-0002" {
		t.Fatalf("single root tree %d %s", len(one), one[0].Code)
	}
}

func TestImpactMatchesSubtree(t *testing.T) {
	pool := testutil.NewDB(t)
	f := nodeID(t, pool, "TR-0001-F1")
	im, err := ImpactOf(ctx, pool, f)
	if err != nil {
		t.Fatal(err)
	}
	if im.Subscribers != subsUnder(t, pool, "TR-0001-F1-") || im.Lineage.FeederID != f || im.Lineage.TransformerID != nodeID(t, pool, "TR-0001") {
		t.Fatalf("impact %+v", im)
	}
	tr, _ := ImpactOf(ctx, pool, nodeID(t, pool, "TR-0001"))
	if tr.Subscribers != 200 {
		t.Fatalf("transformer impact %d", tr.Subscribers)
	}
}

func TestSwitchRejectsCyclesAndWrongKinds(t *testing.T) {
	pool := testutil.NewDB(t)
	p1 := nodeID(t, pool, "TR-0001-F1-P01")
	// P01'in bir alt düğümünü bul.
	var child int64
	if err := pool.QueryRow(ctx, `WITH RECURSIVE d AS (SELECT id FROM network_nodes WHERE parent_id = $1
		UNION ALL SELECT n.id FROM network_nodes n JOIN d ON n.parent_id = d.id) SELECT id FROM d ORDER BY id DESC LIMIT 1`, p1).Scan(&child); err != nil {
		t.Skip("P01 has no descendant in this seed")
	}
	err := tx(t, pool, func(tx pgx.Tx) error { _, err := Switch(ctx, tx, p1, child); return err })
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle: %v", err)
	}
	f1 := nodeID(t, pool, "TR-0001-F1")
	if err := tx(t, pool, func(tx pgx.Tx) error { _, err := Switch(ctx, tx, f1, p1); return err }); !errors.Is(err, ErrBadParent) {
		t.Fatalf("feeder under pole: %v", err)
	}
	if err := tx(t, pool, func(tx pgx.Tx) error { _, err := Switch(ctx, tx, nodeID(t, pool, "TR-0001"), f1); return err }); !errors.Is(err, ErrBadParent) {
		t.Fatalf("moving a transformer: %v", err)
	}
	// Doğrudan SQL ile de döngü kurulamaz (tetikleyici).
	if _, err := pool.Exec(ctx, `UPDATE network_nodes SET parent_id = $2 WHERE id = $1`, p1, child); err == nil {
		t.Fatal("trigger let a cycle through")
	}
}

func TestBackupFeedRestoresCustomersDuringTransformerFault(t *testing.T) {
	pool := testutil.NewDB(t)
	tr1, tr2 := nodeID(t, pool, "TR-0001"), nodeID(t, pool, "TR-0002")
	f2 := nodeID(t, pool, "TR-0001-F2")
	var o *Outage
	if err := tx(t, pool, func(tx pgx.Tx) error {
		var err error
		o, err = CreateOutageTx(ctx, tx, CreateOutage{Kind: "unplanned", FaultNodeID: tr1, Cause: "trafo arızası"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if o.AffectedCount != 200 || o.FeederID != tr1 || o.RequiredSkill != "transformer" {
		t.Fatalf("outage %+v", o)
	}
	moved := subsUnder(t, pool, "TR-0001-F2-")
	var res *SwitchResult
	if err := tx(t, pool, func(tx pgx.Tx) error {
		var err error
		res, err = Switch(ctx, tx, f2, tr2) // F2'yi yedek trafoya aktar
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.RestoredSubs != int64(moved) || res.MovedSubs != moved {
		t.Fatalf("switch result %+v (moved %d)", res, moved)
	}
	o, _ = GetOutage(ctx, pool, o.ID, false)
	if o.AffectedCount != 200-moved {
		t.Fatalf("affected after switch %d", o.AffectedCount)
	}
	// TR2 artık F2'yi de besliyor.
	im, _ := ImpactOf(ctx, pool, tr2)
	if im.Subscribers != 200+moved {
		t.Fatalf("tr2 impact %d", im.Subscribers)
	}
	if err := tx(t, pool, func(tx pgx.Tx) error {
		_, err := TransitionTx(ctx, tx, o.ID, "resolved", 0, "onarıldı", -1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var open int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outage_affected WHERE outage_id=$1 AND restored_at IS NULL`, o.ID).Scan(&open)
	if open != 0 {
		t.Fatalf("%d still interrupted after resolve", open)
	}
}

func TestStateMachine(t *testing.T) {
	pool := testutil.NewDB(t)
	var o *Outage
	_ = tx(t, pool, func(tx pgx.Tx) error {
		var err error
		o, err = CreateOutageTx(ctx, tx, CreateOutage{Kind: "unplanned", FaultNodeID: nodeID(t, pool, "TR-0001-F1-P03")})
		return err
	})
	step := func(to string, ver int) error {
		return tx(t, pool, func(tx pgx.Tx) error { _, err := TransitionTx(ctx, tx, o.ID, to, 0, "", ver); return err })
	}
	if err := step("in_progress", -1); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("detected→in_progress: %v", err)
	}
	if err := step("confirmed", 5); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("stale version: %v", err)
	}
	for _, s := range []string{"confirmed", "assigned", "in_progress", "resolved"} {
		if err := step(s, -1); err != nil {
			t.Fatalf("→%s: %v", s, err)
		}
	}
	if err := step("assigned", -1); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("resolved→assigned: %v", err)
	}
	var hist int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outage_status_history WHERE outage_id=$1`, o.ID).Scan(&hist)
	if hist != 5 {
		t.Fatalf("history rows %d", hist)
	}
	if CanTransition("planned", "detected", "confirmed") || !CanTransition("planned", "scheduled", "in_progress") {
		t.Fatal("planned machine wrong")
	}
}

func TestPlannedOutageConflicts(t *testing.T) {
	pool := testutil.NewDB(t)
	at := func(h int) *time.Time { v := time.Date(2026, 11, 2, h, 0, 0, 0, time.UTC); return &v }
	plan := func(code string, from, to int) error {
		return tx(t, pool, func(tx pgx.Tx) error {
			_, err := CreateOutageTx(ctx, tx, CreateOutage{Kind: "planned", FaultNodeID: nodeID(t, pool, code), PlannedStart: at(from), PlannedEnd: at(to), Cause: "bakım"})
			return err
		})
	}
	if err := plan("TR-0001-F1", 10, 12); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		code     string
		from, to int
		conflict bool
	}{
		{"TR-0001-F1-P02", 11, 13, true}, // alt düğüm, çakışan saat
		{"TR-0001", 11, 12, true},        // üst düğüm (trafo)
		{"TR-0001-F1", 9, 11, true},      // aynı besleyici
		{"TR-0001-F2", 11, 12, false},    // kardeş besleyici: farklı abone kümesi
		{"TR-0001-F1", 12, 14, false},    // bitişik ama çakışmayan ([) aralık)
	}
	for _, c := range cases {
		err := plan(c.code, c.from, c.to)
		if c.conflict != errors.Is(err, ErrPlannedOverlap) || (!c.conflict && err != nil) {
			t.Errorf("%s %d-%d: err=%v want conflict=%v", c.code, c.from, c.to, err, c.conflict)
		}
	}
	if err := plan("TR-0001-F1", 13, 12); !errors.Is(err, ErrBadOutage) {
		t.Fatalf("reversed range: %v", err)
	}
}

// Eşzamanlı iki çakışan planlı kesinti: advisory lock sayesinde yalnızca biri kaydolur.
func TestConcurrentPlannedOutagesOnlyOneWins(t *testing.T) {
	pool := testutil.NewDB(t)
	start := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	codes := []string{"TR-0002-F1", "TR-0002-F1-P04", "TR-0002", "TR-0002-F1-P01"}
	var wg sync.WaitGroup
	errs := make([]error, len(codes))
	for i, c := range codes {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			errs[i] = tx(t, pool, func(tx pgx.Tx) error {
				_, err := CreateOutageTx(ctx, tx, CreateOutage{Kind: "planned", FaultNodeID: id, PlannedStart: &start, PlannedEnd: &end})
				return err
			})
		}(i, nodeID(t, pool, c))
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else if !errors.Is(e, ErrPlannedOverlap) {
			t.Fatal(e)
		}
	}
	// TR-0002 hepsinin atası; F1-P01 ve F1-P04 aynı besleyicide. En az biri kazanmalı,
	// birbiriyle çakışan iki kesinti asla birlikte kaydolmamalı.
	var pairs int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outages a JOIN outages b ON a.id < b.id
		WHERE a.kind='planned' AND b.kind='planned' AND a.feeder_id = b.feeder_id`).Scan(&pairs)
	if ok < 1 || pairs != 0 {
		t.Fatalf("ok=%d overlapping pairs=%d errs=%v", ok, pairs, errs)
	}
}

func TestSAIDIAndSAIFI(t *testing.T) {
	pool := testutil.NewDB(t)
	t0 := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	mk := func(code string, start time.Time, minutes int) {
		var o *Outage
		if err := tx(t, pool, func(tx pgx.Tx) error {
			var err error
			o, err = CreateOutageTx(ctx, tx, CreateOutage{Kind: "unplanned", FaultNodeID: nodeID(t, pool, code), StartedAt: &start})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		end := start.Add(time.Duration(minutes) * time.Minute)
		_, _ = pool.Exec(ctx, `UPDATE outage_affected SET restored_at=$2 WHERE outage_id=$1`, o.ID, end)
		_, _ = pool.Exec(ctx, `UPDATE outages SET status='resolved', ended_at=$2 WHERE id=$1`, o.ID, end)
	}
	mk("TR-0001", t0, 60)                      // 200 abone × 60 dk
	mk("TR-0002-F1", t0.Add(24*time.Hour), 30) // F1 aboneleri × 30 dk
	mk("TR-0003", t0.Add(48*time.Hour), 2)     // 3 dk altı: anlık kesinti, sayılmaz
	f1 := subsUnder(t, pool, "TR-0002-F1-")
	rel, err := ReliabilityIndices(ctx, pool, t0.Add(-time.Hour), t0.Add(72*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	wantMin := float64(200*60 + f1*30)
	if rel.CustomersServed != 600 || rel.Interruptions != int64(200+f1) || math.Abs(rel.CustomerMinutes-wantMin) > 0.01 {
		t.Fatalf("rel %+v", rel)
	}
	if math.Abs(rel.SAIDI-wantMin/600) > 0.001 || math.Abs(rel.SAIFI-float64(200+f1)/600) > 0.001 {
		t.Fatalf("SAIDI %v SAIFI %v", rel.SAIDI, rel.SAIFI)
	}
	// Pencere kırpma: yalnızca ilk kesintinin son 15 dakikası pencerede.
	clip, _ := ReliabilityIndices(ctx, pool, t0.Add(45*time.Minute), t0.Add(2*time.Hour), 0)
	if math.Abs(clip.CustomerMinutes-200*15) > 0.01 {
		t.Fatalf("clipped minutes %v", clip.CustomerMinutes)
	}
	// Besleyici filtresi.
	fr, _ := ReliabilityIndices(ctx, pool, t0.Add(-time.Hour), t0.Add(72*time.Hour), nodeID(t, pool, "TR-0002-F1"))
	if fr.CustomersServed != int64(f1) || math.Abs(fr.SAIDI-30) > 0.001 || math.Abs(fr.SAIFI-1) > 0.001 {
		t.Fatalf("feeder rel %+v", fr)
	}
}
