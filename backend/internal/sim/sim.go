// Package sim generates the head-end data stream: cumulative 15-minute
// register readings for every subscriber and transformer head meter, with
// deliberate data-quality problems (gaps, duplicates, corrections) and
// suspicious consumption patterns. Everything is a pure function of
// (seed, meter, slot), so the same seed always yields the same stream.
package sim

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"time"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/loss"
)

// Meter is one entry of GET /api/v1/ingest/meters.
type Meter struct {
	ID             int64      `json:"id"`
	Kind           string     `json:"kind"`
	SubscriberID   *int64     `json:"subscriber_id"`
	NodeID         *int64     `json:"node_id"`
	Category       *string    `json:"category"`
	Path           []int64    `json:"path"`
	Profile        *string    `json:"sim_profile"`
	InstalledAt    time.Time  `json:"installed_at"`
	InitialIndexWh int64      `json:"initial_index_wh"`
	LastReadAt     *time.Time `json:"last_read_at"`
	LastIndexWh    *int64     `json:"last_index_wh"`
}

func (m Meter) Transformer() int64 {
	if len(m.Path) == 0 {
		return 0
	}
	return m.Path[len(m.Path)-1]
}

func (m Meter) Under(node int64) bool {
	for _, p := range m.Path {
		if p == node {
			return true
		}
	}
	return false
}

// Reading is what the simulator sends.
type Reading struct {
	MeterID      int64     `json:"meter_id"`
	ReadAt       time.Time `json:"read_at"`
	ActiveWh     int64     `json:"active_index_wh"`
	ReactiveVarh int64     `json:"reactive_index_varh"`
}

// Rates of deliberate data-quality problems.
const (
	GapRate        = 0.005  // okuma hiç gelmez
	DuplicateRate  = 0.003  // aynı okuma iki kez gelir
	CorrectionRate = 0.0002 // önce hatalı değer, bir sonraki partide doğrusu
	HeadLossFactor = 1.02   // trafo ana sayacı: gerçek tüketim + %2 teknik kayıp
	TheftRatio     = 0.12   // baypaslı sayacın ölçtüğü oran
)

var categoryFactor = map[string]float64{"residential": 1, "commercial": 4, "industrial": 30, "agricultural": 6}

// Hourly load shapes (relative weights, local time).
var shapes = map[string][24]float64{
	"residential":  {.5, .4, .35, .35, .35, .45, .8, 1.2, 1.1, .8, .7, .7, .8, .8, .75, .8, 1, 1.5, 1.9, 2, 1.8, 1.4, 1, .7},
	"commercial":   {.3, .3, .3, .3, .3, .35, .6, 1.2, 1.6, 1.8, 1.8, 1.8, 1.7, 1.8, 1.8, 1.7, 1.6, 1.4, 1.1, .9, .7, .5, .4, .35},
	"industrial":   {.8, .8, .8, .8, .8, .9, 1.1, 1.3, 1.3, 1.3, 1.3, 1.2, 1.2, 1.3, 1.3, 1.3, 1.2, 1.1, 1, .9, .9, .9, .8, .8},
	"agricultural": {1.4, 1.4, 1.4, 1.4, 1.3, 1, .6, .6, .7, .8, .9, 1, 1, 1, .9, .8, .7, .6, .6, .7, .9, 1.1, 1.3, 1.4},
}

var ist = func() *time.Location {
	if l, err := time.LoadLocation("Europe/Istanbul"); err == nil {
		return l
	}
	return time.FixedZone("TRT", 3*3600)
}()

// Generator is deterministic given its seed.
type Generator struct{ Seed uint64 }

func (g Generator) rng(meter int64, t time.Time, salt uint64) *rand.Rand {
	return rand.New(rand.NewPCG(g.Seed^salt, uint64(meter)<<32^uint64(t.Unix())))
}

// SlotWh returns (true, measured) energy of a subscriber meter in the slot
// starting at t. A thief's meter measures only TheftRatio of the true value;
// a "night" profile shifts most consumption into 22:00-06:00.
func (g Generator) SlotWh(m Meter, t time.Time) (float64, float64) {
	if m.SubscriberID == nil {
		return 0, 0
	}
	cat := "residential"
	if m.Category != nil {
		cat = *m.Category
	}
	pr := loss.ProfileOf(g.Seed, *m.SubscriberID)
	daily := pr.BaseWh * categoryFactor[cat]
	profile := ""
	if m.Profile != nil {
		profile = *m.Profile
	}
	if profile == "thief" {
		daily *= 3
	}
	shape := shapes[cat]
	if profile == "night" {
		daily *= 1.3
		for h := range shape {
			if h >= 22 || h < 6 {
				shape[h] *= 6
			}
		}
	}
	var sum float64
	for _, w := range shape {
		sum += w
	}
	h := t.In(ist).Hour()
	r := g.rng(m.ID, t, 0x51)
	wh := daily * shape[h] / sum / 4 * (1 + (r.Float64()*2-1)*math.Min(pr.Noise, 0.5))
	if wh < 0 {
		wh = 0
	}
	measured := wh
	if profile == "thief" {
		measured = wh * TheftRatio
	}
	return wh, measured
}

// StartIndex is the register value of a meter that has never reported.
func (g Generator) StartIndex(m Meter) int64 {
	if m.Kind == "head" {
		return 50_000_000 + int64(hash(g.Seed, m.ID)%10_000_000)
	}
	return m.InitialIndexWh + 300_000 + int64(hash(g.Seed, m.ID)%3_000_000)
}

// Quality decides deterministically whether a reading is dropped,
// duplicated or first sent with a wrong value.
type Quality int

const (
	Normal Quality = iota
	Gap
	Duplicate
	Correction
)

func (g Generator) QualityOf(meterID int64, t time.Time) Quality {
	x := g.rng(meterID, t, 0x99).Float64()
	switch {
	case x < GapRate:
		return Gap
	case x < GapRate+DuplicateRate:
		return Duplicate
	case x < GapRate+DuplicateRate+CorrectionRate:
		return Correction
	}
	return Normal
}

func hash(seed uint64, id int64) uint64 {
	h := fnv.New64a()
	var b [16]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(seed >> (8 * i))
		b[8+i] = byte(uint64(id) >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

// State tracks register values while streaming.
type State struct {
	Gen        Generator
	Meters     []Meter
	Index      map[int64]float64 // active register (Wh) per meter
	Reactive   map[int64]float64
	Next       map[int64]time.Time // next slot to emit per meter
	Dead       map[int64]bool      // meters without power (outage)
	pendingFix []Reading
	byTr       map[int64][]int
	heads      map[int64]int // transformer → head meter index
}

func NewState(g Generator, meters []Meter, defaultStart time.Time) *State {
	s := &State{Gen: g, Meters: meters, Index: map[int64]float64{}, Reactive: map[int64]float64{}, Next: map[int64]time.Time{},
		Dead: map[int64]bool{}, byTr: map[int64][]int{}, heads: map[int64]int{}}
	for i, m := range meters {
		if m.LastReadAt != nil && m.LastIndexWh != nil {
			s.Index[m.ID] = float64(*m.LastIndexWh)
			s.Next[m.ID] = m.LastReadAt.Add(15 * time.Minute)
		} else {
			s.Index[m.ID] = float64(g.StartIndex(m))
			st := defaultStart
			if m.InstalledAt.After(st) {
				st = m.InstalledAt.Truncate(15 * time.Minute).Add(15 * time.Minute)
			}
			s.Next[m.ID] = st
		}
		s.Reactive[m.ID] = s.Index[m.ID] / 10
		if m.Kind == "head" && m.NodeID != nil {
			s.heads[*m.NodeID] = i
		} else {
			s.byTr[m.Transformer()] = append(s.byTr[m.Transformer()], i)
		}
	}
	return s
}

// Slot advances every meter whose next slot is t (the reading at t closes
// the slot [t-15m, t)) and returns the readings to send.
func (s *State) Slot(t time.Time) []Reading {
	out := append([]Reading(nil), s.pendingFix...)
	s.pendingFix = nil
	slotStart := t.Add(-15 * time.Minute)
	for tr, idxs := range s.byTr {
		var trueSum float64
		for _, i := range idxs {
			m := s.Meters[i]
			if s.Next[m.ID].After(t) {
				continue
			}
			if s.Dead[m.ID] {
				s.Next[m.ID] = t.Add(15 * time.Minute) // enerji yok: tüketim yok, okuma da gelmez
				continue
			}
			trueWh, measured := s.Gen.SlotWh(m, slotStart)
			trueSum += trueWh
			s.Index[m.ID] += measured
			ratio := 0.1
			if m.Category != nil && (*m.Category == "industrial" || *m.Category == "commercial") {
				ratio = 0.3
			}
			s.Reactive[m.ID] += measured * ratio
			s.Next[m.ID] = t.Add(15 * time.Minute)
			r := Reading{MeterID: m.ID, ReadAt: t, ActiveWh: int64(s.Index[m.ID]), ReactiveVarh: int64(s.Reactive[m.ID])}
			switch s.Gen.QualityOf(m.ID, t) {
			case Gap:
				continue
			case Duplicate:
				out = append(out, r, r)
				continue
			case Correction:
				wrong := r
				wrong.ActiveWh += 777
				out = append(out, wrong)
				s.pendingFix = append(s.pendingFix, r) // doğrusu bir sonraki partide
				continue
			}
			out = append(out, r)
		}
		if hi, ok := s.heads[tr]; ok {
			h := s.Meters[hi]
			if !s.Next[h.ID].After(t) {
				s.Index[h.ID] += trueSum * HeadLossFactor
				s.Reactive[h.ID] += trueSum * 0.15
				s.Next[h.ID] = t.Add(15 * time.Minute)
				out = append(out, Reading{MeterID: h.ID, ReadAt: t, ActiveWh: int64(s.Index[h.ID]), ReactiveVarh: int64(s.Reactive[h.ID])})
			}
		}
	}
	return out
}

// Under returns the subscriber meters downstream of node.
func (s *State) Under(node int64) []Meter {
	var out []Meter
	for _, m := range s.Meters {
		if m.Kind == "subscriber" && m.Under(node) {
			out = append(out, m)
		}
	}
	return out
}
