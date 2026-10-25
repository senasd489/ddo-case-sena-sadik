package sim

import (
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func meters() []Meter {
	var ms []Meter
	for i := int64(1); i <= 50; i++ {
		cat := "residential"
		if i%10 == 0 {
			cat = "commercial"
		}
		var prof *string
		if i == 7 {
			prof = ptr("thief")
		}
		ms = append(ms, Meter{ID: i, Kind: "subscriber", SubscriberID: ptr(i), Category: ptr(cat), Path: []int64{100 + i%5, 99, 1}, Profile: prof})
	}
	ms = append(ms, Meter{ID: 999, Kind: "head", NodeID: ptr(int64(1)), Path: []int64{1}})
	return ms
}

var start = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func run(t *testing.T, seed uint64, slots int) ([]Reading, *State) {
	s := NewState(Generator{Seed: seed}, meters(), start)
	var all []Reading
	for i := 0; i < slots; i++ {
		all = append(all, s.Slot(start.Add(time.Duration(i)*15*time.Minute))...)
	}
	return all, s
}

func TestSameSeedSameStream(t *testing.T) {
	a, sa := run(t, 42, 96)
	b, sb := run(t, 42, 96)
	c, _ := run(t, 43, 96)
	sum := func(rs []Reading) (n int, total int64) {
		for _, r := range rs {
			n++
			total += r.ActiveWh
		}
		return
	}
	na, ta := sum(a)
	nb, tb := sum(b)
	_, tc := sum(c)
	if na != nb || ta != tb || sa.Index[7] != sb.Index[7] {
		t.Fatal("same seed produced a different stream")
	}
	if ta == tc {
		t.Fatal("different seed produced the same stream")
	}
}

func TestHeadMeterSeesTrueConsumptionIncludingTheft(t *testing.T) {
	g := Generator{Seed: 1}
	s := NewState(g, meters(), start)
	h0 := s.Index[999]
	sub0 := map[int64]float64{}
	for k, v := range s.Index {
		sub0[k] = v
	}
	var trueSum, measuredSum float64
	for i := 1; i <= 96; i++ {
		ts := start.Add(time.Duration(i) * 15 * time.Minute)
		for _, m := range s.Meters {
			if m.Kind == "subscriber" {
				tw, mw := g.SlotWh(m, ts.Add(-15*time.Minute))
				trueSum += tw
				measuredSum += mw
			}
		}
		s.Slot(ts)
	}
	headDelta := s.Index[999] - h0
	if d := headDelta - trueSum*HeadLossFactor; d > 1 || d < -1 {
		t.Fatalf("head delta %v vs %v", headDelta, trueSum*HeadLossFactor)
	}
	if measuredSum >= trueSum {
		t.Fatal("theft not reflected in measured consumption")
	}
	thiefMeasured := s.Index[7] - sub0[7]
	tw, _ := g.SlotWh(s.Meters[6], start)
	if thiefMeasured <= 0 || tw <= 0 {
		t.Fatal("thief meter not advancing")
	}
}

func TestDeadMetersStopReporting(t *testing.T) {
	s := NewState(Generator{Seed: 5}, meters(), start)
	s.Dead[3] = true
	before := s.Index[3]
	for i := 0; i < 8; i++ {
		for _, r := range s.Slot(start.Add(time.Duration(i) * 15 * time.Minute)) {
			if r.MeterID == 3 {
				t.Fatal("dead meter reported")
			}
		}
	}
	if s.Index[3] != before {
		t.Fatal("dead meter consumed energy")
	}
	if len(s.Under(101)) != 10 {
		t.Fatalf("under %d", len(s.Under(101)))
	}
}

func TestQualityRatesRoughlyMatch(t *testing.T) {
	g := Generator{Seed: 9}
	counts := map[Quality]int{}
	n := 200000
	for i := 0; i < n; i++ {
		counts[g.QualityOf(int64(i%1000), start.Add(time.Duration(i/1000)*15*time.Minute))]++
	}
	gap := float64(counts[Gap]) / float64(n)
	dup := float64(counts[Duplicate]) / float64(n)
	if gap < 0.003 || gap > 0.007 || dup < 0.0015 || dup > 0.0045 {
		t.Fatalf("gap %.4f dup %.4f", gap, dup)
	}
}
