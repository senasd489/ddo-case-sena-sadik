package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/grid"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/testutil"
)

var ctx = context.Background()

// subsOnFeeder returns n subscriber ids fed by the feeder with code, on distinct poles when possible.
func subsOnFeeder(t *testing.T, pool *pgxpool.Pool, feeder string, n int) []int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT DISTINCT ON (s.pole_id) s.id FROM subscribers s JOIN network_nodes p ON p.id = s.pole_id
		WHERE p.code LIKE $1 || '-P%' ORDER BY s.pole_id, s.id LIMIT $2`, feeder, n)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) < n {
		t.Fatalf("need %d subscribers on %s, got %d (%v)", n, feeder, len(ids), err)
	}
	return ids
}

var t0 = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

func sig(src string, sub int64, at time.Time, ext string) Signal {
	return Signal{Source: src, SubscriberID: sub, ReceivedAt: at, ExternalID: ext}
}

func TestHaversineAndEstimate(t *testing.T) {
	// İstanbul–Ankara ≈ 350 km
	if d := Haversine(41.0082, 28.9784, 39.9334, 32.8597); math.Abs(d-350) > 5 {
		t.Fatalf("haversine %v", d)
	}
	travel, repair := EstimateMinutes(10, "feeder", 0.8)
	if travel != 15 || repair != 96 {
		t.Fatalf("estimate %d %d", travel, repair)
	}
}

func TestSignalsClusterIntoOneOutageAndRelocate(t *testing.T) {
	pool := testutil.NewDB(t)
	subs := subsOnFeeder(t, pool, "TR-0001-F1", 3)
	r1, err := Ingest(ctx, pool, sig("last_gasp", subs[0], t0, "lg-1"))
	if err != nil || r1.Outcome != "new_outage" {
		t.Fatalf("first: %+v %v", r1, err)
	}
	r2, _ := Ingest(ctx, pool, sig("call", subs[1], t0.Add(4*time.Minute), "call-1"))
	if r2.OutageID != r1.OutageID || r2.Outcome != "attached" {
		t.Fatalf("second not clustered: %+v", r2)
	}
	// Aynı abone tekrar arar → mükerrer, aynı olaya bağlanır.
	r3, _ := Ingest(ctx, pool, sig("call", subs[1], t0.Add(6*time.Minute), "call-2"))
	if r3.OutageID != r1.OutageID || !r3.Duplicate || r3.Outcome != "duplicate" {
		t.Fatalf("duplicate: %+v", r3)
	}
	// Aynı mesaj kimliği yeniden gelir → replay, yeni satır yok.
	r4, _ := Ingest(ctx, pool, sig("call", subs[1], t0.Add(6*time.Minute), "call-2"))
	if r4.Outcome != "replayed" || r4.OutageID != r1.OutageID {
		t.Fatalf("replay: %+v", r4)
	}
	if r3.Status != "detected" {
		t.Fatalf("confirmed too early: %+v", r3)
	}
	r5, _ := Ingest(ctx, pool, sig("last_gasp", subs[2], t0.Add(7*time.Minute), "lg-2"))
	if r5.OutageID != r1.OutageID || r5.Status != "confirmed" || r5.Reporters != 3 {
		t.Fatalf("auto confirm: %+v", r5)
	}
	// Arıza noktası üç direğin en yakın ortak atasına taşındı.
	var lca int64
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var poles []int64
		_ = tx.QueryRow(ctx, `SELECT array_agg(pole_id) FROM subscribers WHERE id = ANY($1)`, subs).Scan(&poles)
		var err error
		lca, err = LowestCommonAncestor(ctx, tx, poles)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	o, _ := grid.GetOutage(ctx, pool, r1.OutageID, false)
	if o.FaultNodeID != lca || r1.FaultNode == lca && lca != o.FaultNodeID {
		t.Fatalf("fault node %d, lca %d", o.FaultNodeID, lca)
	}
	im, _ := grid.ImpactOf(ctx, pool, lca)
	if o.AffectedCount != im.Subscribers || o.ReportCount != 3 || !o.StartedAt.Equal(t0) {
		t.Fatalf("outage %+v (impact %d)", o, im.Subscribers)
	}
}

func TestSignalsOnOtherFeederOrOutsideWindowStartNewOutage(t *testing.T) {
	pool := testutil.NewDB(t)
	a := subsOnFeeder(t, pool, "TR-0001-F1", 1)[0]
	b := subsOnFeeder(t, pool, "TR-0001-F2", 1)[0]
	r1, _ := Ingest(ctx, pool, sig("call", a, t0, "x1"))
	r2, _ := Ingest(ctx, pool, sig("call", b, t0.Add(time.Minute), "x2"))
	if r1.OutageID == r2.OutageID {
		t.Fatal("different feeders merged")
	}
	// Aynı besleyici ama 2 saat sonra ve abone etkilenen listede değil → yeni olay.
	c := subsOnFeeder(t, pool, "TR-0001-F3", 2)
	r3, _ := Ingest(ctx, pool, sig("call", c[0], t0, "x3"))
	r4, _ := Ingest(ctx, pool, sig("call", c[1], t0.Add(2*time.Hour), "x4"))
	o3, _ := grid.GetOutage(ctx, pool, r3.OutageID, false)
	stillAffected := false
	for _, id := range mustAffected(t, pool, o3.FaultNodeID) {
		if id == c[1] {
			stillAffected = true
		}
	}
	if !stillAffected && r3.OutageID == r4.OutageID {
		t.Fatal("signal outside window merged into unrelated outage")
	}
	if stillAffected && r3.OutageID != r4.OutageID {
		t.Fatal("late call from an affected subscriber not attached")
	}
}

func mustAffected(t *testing.T, pool *pgxpool.Pool, node int64) []int64 {
	ids, err := grid.AffectedSubscribers(ctx, pool, node)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// 20 eşzamanlı son-nefes sinyali (aynı besleyici) → tek kesinti olayı.
func TestConcurrentSignalsCreateExactlyOneOutage(t *testing.T) {
	pool := testutil.NewDB(t)
	subs := subsOnFeeder(t, pool, "TR-0002-F2", 8)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := Ingest(ctx, pool, sig("last_gasp", subs[i%len(subs)], t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("c-%d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	var outages, signals, dups int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM outages`).Scan(&outages)
	_ = pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE duplicate) FROM outage_signals`).Scan(&signals, &dups)
	if outages != 1 || signals != 20 || dups != 12 {
		t.Fatalf("outages=%d signals=%d dups=%d", outages, signals, dups)
	}
}

func confirmedOutage(t *testing.T, pool *pgxpool.Pool, code string) int64 {
	t.Helper()
	var id int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var n int64
		if err := tx.QueryRow(ctx, `SELECT id FROM network_nodes WHERE code=$1`, code).Scan(&n); err != nil {
			return err
		}
		o, err := grid.CreateOutageTx(ctx, tx, grid.CreateOutage{Kind: "unplanned", FaultNodeID: n})
		if err != nil {
			return err
		}
		id = o.ID
		_, err = grid.TransitionTx(ctx, tx, o.ID, "confirmed", 0, "", -1)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDispatchPicksNearestQualifiedCrew(t *testing.T) {
	pool := testutil.NewDB(t)
	id := confirmedOutage(t, pool, "TR-0001-F1-P02")
	var lat, lon float64
	_ = pool.QueryRow(ctx, `SELECT lat, lon FROM network_nodes WHERE code='TR-0001-F1-P02'`).Scan(&lat, &lon)
	rows, _ := pool.Query(ctx, `SELECT id, lat, lon FROM crews WHERE 'lv' = ANY(skills)`)
	best, bestD := int64(0), math.MaxFloat64
	for rows.Next() {
		var cid int64
		var clat, clon float64
		_ = rows.Scan(&cid, &clat, &clon)
		if d := Haversine(lat, lon, clat, clon); d < bestD {
			best, bestD = cid, d
		}
	}
	rows.Close()
	a, err := Dispatch(ctx, pool, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.CrewID != best || math.Abs(a.DistanceKM-bestD) > 0.02 {
		t.Fatalf("assigned crew %d (%.2f km), nearest %d (%.2f km)", a.CrewID, a.DistanceKM, best, bestD)
	}
	o, _ := grid.GetOutage(ctx, pool, id, false)
	if o.Status != "assigned" || o.EstimatedRestoreAt == nil {
		t.Fatalf("outage after dispatch %+v", o)
	}
	if _, err := Dispatch(ctx, pool, id, 0); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("second dispatch: %v", err)
	}
}

// Yarış durumu: 3 boş ekip, 5 eşzamanlı atama → 3 atama, 2 "ekip yok"; hiçbir ekip iki işte değil.
func TestConcurrentDispatchNeverDoubleBooks(t *testing.T) {
	pool := testutil.NewDB(t)
	_, _ = pool.Exec(ctx, `UPDATE crews SET status='off_duty' WHERE id NOT IN (SELECT id FROM crews WHERE 'lv' = ANY(skills) ORDER BY id LIMIT 3)`)
	codes := []string{"TR-0001-F1-P02", "TR-0001-F2-P02", "TR-0001-F3-P02", "TR-0002-F1-P02", "TR-0002-F2-P02"}
	ids := make([]int64, len(codes))
	for i, c := range codes {
		ids[i] = confirmedOutage(t, pool, c)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	start := make(chan struct{})
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			<-start
			_, errs[i] = Dispatch(ctx, pool, id, 0)
		}(i, id)
	}
	close(start)
	wg.Wait()
	ok, none := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrNoCrew):
			none++
		default:
			t.Fatal(e)
		}
	}
	var active, distinctCrews, busy int
	_ = pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT crew_id) FROM crew_assignments WHERE status='active'`).Scan(&active, &distinctCrews)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM crews WHERE status='busy'`).Scan(&busy)
	if ok != 3 || none != 2 || active != 3 || distinctCrews != 3 || busy != 3 {
		t.Fatalf("ok=%d none=%d active=%d distinct=%d busy=%d", ok, none, active, distinctCrews, busy)
	}
}

func TestCrewLifecycleAndRelease(t *testing.T) {
	pool := testutil.NewDB(t)
	id := confirmedOutage(t, pool, "TR-0003-F1-P01")
	a, err := Dispatch(ctx, pool, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	run := func(fn func(pgx.Tx) error) error { return pgx.BeginFunc(ctx, pool, fn) }
	// Başka ekip bu işi yürütemez.
	if err := run(func(tx pgx.Tx) error {
		_, err := CrewTransitionTx(ctx, tx, a.CrewID+100, "in_progress", 0, "")
		return err
	}); !errors.Is(err, ErrNoAssignment) {
		t.Fatalf("other crew: %v", err)
	}
	if err := run(func(tx pgx.Tx) error { _, err := CrewTransitionTx(ctx, tx, a.CrewID, "cancelled", 0, ""); return err }); !errors.Is(err, grid.ErrBadTransition) {
		t.Fatalf("crew cancelling: %v", err)
	}
	// Geri çek → tekrar ata → yürüt → çöz.
	if err := run(func(tx pgx.Tx) error { return ReleaseTx(ctx, tx, id, 0, "test") }); err != nil {
		t.Fatal(err)
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM crews WHERE id=$1`, a.CrewID).Scan(&st)
	if st != "available" {
		t.Fatalf("crew after release %s", st)
	}
	a2, err := Dispatch(ctx, pool, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"in_progress", "resolved"} {
		if err := run(func(tx pgx.Tx) error { _, err := CrewTransitionTx(ctx, tx, a2.CrewID, to, 0, ""); return err }); err != nil {
			t.Fatalf("→%s: %v", to, err)
		}
	}
	var asg string
	_ = pool.QueryRow(ctx, `SELECT c.status, a.status FROM crews c JOIN crew_assignments a ON a.crew_id=c.id WHERE a.id=$1`, a2.ID).Scan(&st, &asg)
	if st != "available" || asg != "completed" {
		t.Fatalf("after resolve crew=%s assignment=%s", st, asg)
	}
}

func TestAutoDispatchAndNotDispatchable(t *testing.T) {
	pool := testutil.NewDB(t)
	confirmedOutage(t, pool, "TR-0001-F1-P03")
	confirmedOutage(t, pool, "TR-0002")
	n, err := AutoDispatch(ctx, pool, 10)
	if err != nil || n != 2 {
		t.Fatalf("auto dispatched %d %v", n, err)
	}
	var skill string
	_ = pool.QueryRow(ctx, `SELECT array_to_string(c.skills, ',') FROM crew_assignments a JOIN crews c ON c.id=a.crew_id
		JOIN outages o ON o.id=a.outage_id WHERE o.required_skill='transformer'`).Scan(&skill)
	if skill == "" || !contains(skill, "transformer") {
		t.Fatalf("transformer fault given to crew with skills %q", skill)
	}
	// detected durumdaki kesinti atanamaz.
	var det int64
	_ = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var n int64
		_ = tx.QueryRow(ctx, `SELECT id FROM network_nodes WHERE code='TR-0003-F2'`).Scan(&n)
		o, err := grid.CreateOutageTx(ctx, tx, grid.CreateOutage{Kind: "unplanned", FaultNodeID: n})
		det = o.ID
		return err
	})
	if _, err := Dispatch(ctx, pool, det, 0); !errors.Is(err, ErrNotDispatchable) {
		t.Fatalf("detected dispatch: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRestoreSignalsAutoResolve(t *testing.T) {
	pool := testutil.NewDB(t)
	subs := subsOnFeeder(t, pool, "TR-0003-F2", 1)
	r1, _ := Ingest(ctx, pool, sig("last_gasp", subs[0], t0, "rg-1"))
	o, _ := grid.GetOutage(ctx, pool, r1.OutageID, false)
	ids, _ := grid.AffectedSubscribers(ctx, pool, o.FaultNodeID)
	for i, id := range ids {
		r, err := Ingest(ctx, pool, sig("restore", id, t0.Add(30*time.Minute), fmt.Sprintf("rs-%d", i)))
		if err != nil || r.Outcome != "restored" {
			t.Fatalf("restore %d: %+v %v", id, r, err)
		}
		if i < len(ids)-1 && r.Status == "resolved" {
			t.Fatal("resolved before every subscriber was restored")
		}
	}
	o, _ = grid.GetOutage(ctx, pool, r1.OutageID, false)
	if o.Status != "resolved" || o.AffectedCount != 0 {
		t.Fatalf("after restores %+v", o)
	}
	var mins float64
	_ = pool.QueryRow(ctx, `SELECT extract(epoch FROM max(restored_at - interrupted_at))/60 FROM outage_affected WHERE outage_id=$1`, o.ID).Scan(&mins)
	if mins != 30 {
		t.Fatalf("interruption minutes %v", mins)
	}
	r, _ := Ingest(ctx, pool, sig("restore", ids[0], t0.Add(40*time.Minute), "rs-late"))
	if r.Outcome != "restore_ignored" {
		t.Fatalf("late restore %+v", r)
	}
}

func TestFreshReadingsCloseOutageWhenRestoreMessageLost(t *testing.T) {
	pool := testutil.NewDB(t)
	start := time.Now().Add(-time.Hour).Truncate(15 * time.Minute)
	subs := subsOnFeeder(t, pool, "TR-0002-F3", 1)
	r, _ := Ingest(ctx, pool, sig("last_gasp", subs[0], start, "lost-1"))
	o, _ := grid.GetOutage(ctx, pool, r.OutageID, false)
	ids, _ := grid.AffectedSubscribers(ctx, pool, o.FaultNodeID)
	_, _ = pool.Exec(ctx, `SELECT ensure_reading_partitions(current_date - 2, current_date + 1)`)
	// Bir abone hariç hepsinden kesinti sonrası okuma gelir.
	for _, id := range ids[1:] {
		if _, err := pool.Exec(ctx, `INSERT INTO readings (meter_id, read_at, active_index_wh) SELECT m.id, $2, 1000 FROM meters m WHERE m.subscriber_id = $1`,
			id, start.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := ReconcileFromReadings(ctx, pool); err != nil || n != 0 {
		t.Fatalf("resolved too early: %d %v", n, err)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO readings (meter_id, read_at, active_index_wh) SELECT m.id, $2, 1000 FROM meters m WHERE m.subscriber_id = $1`, ids[0], start.Add(45*time.Minute))
	if n, err := ReconcileFromReadings(ctx, pool); err != nil || n != 1 {
		t.Fatalf("reconcile: %d %v", n, err)
	}
	o, _ = grid.GetOutage(ctx, pool, r.OutageID, false)
	if o.Status != "resolved" {
		t.Fatalf("status %s", o.Status)
	}
}
