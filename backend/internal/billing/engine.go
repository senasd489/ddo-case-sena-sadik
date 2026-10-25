// Package billing implements T-2: the tariff and invoice engine.
//
// engine.go is a pure function from (daily consumption, tariff versions,
// prior estimated invoice) to invoice lines. It has no database access, uses
// exact rational arithmetic (math/big.Rat) and rounds every line exactly once
// to whole kuruş (half away from zero), so the same input always yields the
// same invoice — byte for byte.
package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Tier is one step of a tiered tariff: [From, To) kWh per full period.
type Tier struct {
	From  *big.Rat `json:"from_kwh"`
	To    *big.Rat `json:"to_kwh"` // nil = open ended
	Price *big.Rat `json:"unit_price"`
}

// TariffVersion mirrors tariff_versions (+ tiers). Prices are kuruş/kWh.
type TariffVersion struct {
	ID                int64      `json:"id"`
	ValidFrom         time.Time  `json:"valid_from"`
	ValidTo           *time.Time `json:"valid_to"` // exclusive
	Mode              string     `json:"mode"`     // tiered | tou
	DayPrice          *big.Rat   `json:"day_price,omitempty"`
	PeakPrice         *big.Rat   `json:"peak_price,omitempty"`
	NightPrice        *big.Rat   `json:"night_price,omitempty"`
	DistributionPrice *big.Rat   `json:"distribution_price"`
	ReactivePrice     *big.Rat   `json:"reactive_price"`
	ReactiveFreeRatio *big.Rat   `json:"reactive_free_ratio"`
	EnergyTaxRate     *big.Rat   `json:"energy_tax_rate"`
	FundRate          *big.Rat   `json:"fund_rate"`
	VATRate           *big.Rat   `json:"vat_rate"`
	Tiers             []Tier     `json:"tiers,omitempty"`
}

// DayUsage is one local day of consumption (summed over the subscriber's meters).
type DayUsage struct {
	Day          time.Time `json:"day"`
	ActiveWh     int64     `json:"active_wh"`
	DayWh        int64     `json:"day_wh"`
	PeakWh       int64     `json:"peak_wh"`
	NightWh      int64     `json:"night_wh"`
	ReactiveVarh int64     `json:"reactive_varh"`
	Slots        int       `json:"slots"`              // ölçülen 15 dk slot sayısı (96 = tam gün)
	Interpolated int       `json:"interpolated_slots"` // interpolasyonla doldurulan slotlar
	Estimated    bool      `json:"estimated"`          // eksik gün tahminle tamamlandı
	EstimatedWh  int64     `json:"estimated_wh"`       // tahminle eklenen miktar
}

// PriorInvoice is the estimated invoice of the same period that the actual
// invoice settles; TotalKurus is the amount already paid on it.
type PriorInvoice struct {
	ID         int64  `json:"id"`
	InvoiceNo  string `json:"invoice_no"`
	TotalKurus int64  `json:"paid_kurus"`
}

type Input struct {
	SubscriberID int64           `json:"subscriber_id"`
	PeriodStart  time.Time       `json:"period_start"`
	PeriodEnd    time.Time       `json:"period_end"` // exclusive
	Days         []DayUsage      `json:"days"`
	Versions     []TariffVersion `json:"versions"`
	Prior        *PriorInvoice   `json:"prior,omitempty"`
}

type Line struct {
	Kind        string         `json:"kind"`
	Description string         `json:"description"`
	Quantity    string         `json:"quantity"`   // kWh / kVArh, 3 ondalık
	UnitPrice   string         `json:"unit_price"` // kuruş, 4 ondalık
	AmountKurus int64          `json:"amount_kurus"`
	Meta        map[string]any `json:"meta,omitempty"`
}

type Result struct {
	Lines         []Line `json:"lines"`
	ConsumptionWh int64  `json:"consumption_wh"`
	EstimatedWh   int64  `json:"estimated_wh"`
	SubtotalKurus int64  `json:"subtotal_kurus"`
	TaxKurus      int64  `json:"tax_kurus"`
	TotalKurus    int64  `json:"total_kurus"`
	InputHash     string `json:"input_hash"`
}

var (
	ErrNoTariff   = errors.New("no tariff version covers the period")
	ErrBadPeriod  = errors.New("invalid billing period")
	ErrTariffGaps = errors.New("tariff versions leave a gap inside the period")
)

func rat(n int64) *big.Rat { return new(big.Rat).SetInt64(n) }

// RoundKurus rounds a kuruş amount half away from zero.
func RoundKurus(r *big.Rat) int64 {
	num := new(big.Int).Set(r.Num())
	den := r.Denom()
	neg := num.Sign() < 0
	if neg {
		num.Neg(num)
	}
	// floor((2·num + den) / (2·den))
	twice := new(big.Int).Mul(num, big.NewInt(2))
	twice.Add(twice, den)
	q := new(big.Int).Quo(twice, new(big.Int).Mul(den, big.NewInt(2)))
	if neg {
		q.Neg(q)
	}
	return q.Int64()
}

func qty(r *big.Rat) string   { return r.FloatString(3) }
func price(r *big.Rat) string { return r.FloatString(4) }

func days(a, b time.Time) int { return int(b.Sub(a).Hours()/24 + 0.5) }

type segment struct {
	v          TariffVersion
	start, end time.Time
	usage      []DayUsage
}

// Compute builds the invoice. Lines are produced in a fixed order:
// energy (per tier or time band) → distribution → reactive → energy tax →
// fund → VAT, per tariff segment, and finally the settlement of a prior
// estimated invoice.
func Compute(in Input) (Result, error) {
	if !in.PeriodEnd.After(in.PeriodStart) {
		return Result{}, ErrBadPeriod
	}
	periodDays := days(in.PeriodStart, in.PeriodEnd)
	segs, err := split(in)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, d := range in.Days {
		res.ConsumptionWh += d.ActiveWh
		res.EstimatedWh += d.EstimatedWh
	}
	multi := len(segs) > 1
	for _, s := range segs {
		segDays := days(s.start, s.end)
		label := ""
		if multi {
			label = fmt.Sprintf(" [%s–%s, %d/%d gün]", s.start.Format("02.01"), s.end.AddDate(0, 0, -1).Format("02.01"), segDays, periodDays)
		}
		var act, dayWh, peakWh, nightWh, react int64
		for _, d := range s.usage {
			act += d.ActiveWh
			dayWh += d.DayWh
			peakWh += d.PeakWh
			nightWh += d.NightWh
			react += d.ReactiveVarh
		}
		kwh := new(big.Rat).SetFrac64(act, 1000)
		var energy int64
		add := func(l Line, kind string) int64 {
			l.Kind = kind
			res.Lines = append(res.Lines, l)
			return l.AmountKurus
		}
		switch s.v.Mode {
		case "tiered":
			// Kademe sınırları tam dönem içindir; dönem bölündüğünde her parçanın
			// sınırı gün oranında küçültülür (gün esaslı bölme).
			factor := new(big.Rat).SetFrac64(int64(segDays), int64(periodDays))
			remaining := new(big.Rat).Set(kwh)
			for i, t := range s.v.Tiers {
				if remaining.Sign() <= 0 {
					break
				}
				from := new(big.Rat).Mul(t.From, factor)
				portion := new(big.Rat).Set(remaining)
				if t.To != nil {
					width := new(big.Rat).Sub(new(big.Rat).Mul(t.To, factor), from)
					if portion.Cmp(width) > 0 {
						portion = width
					}
				}
				remaining.Sub(remaining, portion)
				amt := RoundKurus(new(big.Rat).Mul(portion, t.Price))
				upper := "∞"
				if t.To != nil {
					upper = new(big.Rat).Mul(t.To, factor).FloatString(1)
				}
				energy += add(Line{
					Description: fmt.Sprintf("Enerji bedeli — %d. kademe (%s–%s kWh)%s", i+1, from.FloatString(1), upper, label),
					Quantity:    qty(portion), UnitPrice: price(t.Price), AmountKurus: amt,
					Meta: map[string]any{"tariff_version_id": s.v.ID, "tier": i + 1},
				}, "energy")
			}
		case "tou":
			for _, b := range []struct {
				name string
				wh   int64
				p    *big.Rat
			}{{"Gündüz (06-17)", dayWh, s.v.DayPrice}, {"Puant (17-22)", peakWh, s.v.PeakPrice}, {"Gece (22-06)", nightWh, s.v.NightPrice}} {
				q := new(big.Rat).SetFrac64(b.wh, 1000)
				energy += add(Line{
					Description: "Enerji bedeli — " + b.name + label,
					Quantity:    qty(q), UnitPrice: price(b.p), AmountKurus: RoundKurus(new(big.Rat).Mul(q, b.p)),
					Meta: map[string]any{"tariff_version_id": s.v.ID, "band": b.name},
				}, "energy")
			}
		default:
			return Result{}, fmt.Errorf("unknown tariff mode %q", s.v.Mode)
		}
		dist := add(Line{
			Description: "Dağıtım bedeli" + label, Quantity: qty(kwh), UnitPrice: price(s.v.DistributionPrice),
			AmountKurus: RoundKurus(new(big.Rat).Mul(kwh, s.v.DistributionPrice)),
			Meta:        map[string]any{"tariff_version_id": s.v.ID},
		}, "distribution")
		reactive := int64(0)
		if s.v.ReactivePrice.Sign() > 0 {
			// Endüktif reaktif, aktif tüketimin serbest oranını (%20) aşarsa aşan kısım ücretlenir.
			free := new(big.Rat).Mul(kwh, s.v.ReactiveFreeRatio)
			kvarh := new(big.Rat).SetFrac64(react, 1000)
			excess := new(big.Rat).Sub(kvarh, free)
			if excess.Sign() > 0 {
				reactive = add(Line{
					Description: fmt.Sprintf("Reaktif enerji bedeli (serbest %%%s aşımı)%s", new(big.Rat).Mul(s.v.ReactiveFreeRatio, rat(100)).FloatString(0), label),
					Quantity:    qty(excess), UnitPrice: price(s.v.ReactivePrice),
					AmountKurus: RoundKurus(new(big.Rat).Mul(excess, s.v.ReactivePrice)),
					Meta:        map[string]any{"tariff_version_id": s.v.ID, "reactive_kvarh": qty(kvarh), "free_kvarh": qty(free)},
				}, "reactive")
			}
		}
		pct := func(r *big.Rat) string { return new(big.Rat).Mul(r, rat(100)).FloatString(0) }
		etv := add(Line{Description: fmt.Sprintf("Elektrik tüketim vergisi (%%%s)%s", pct(s.v.EnergyTaxRate), label),
			UnitPrice: price(rat(0)), Quantity: qty(rat(0)),
			AmountKurus: RoundKurus(new(big.Rat).Mul(rat(energy), s.v.EnergyTaxRate))}, "energy_tax")
		fund := add(Line{Description: fmt.Sprintf("Enerji fonu (%%%s)%s", pct(s.v.FundRate), label),
			UnitPrice: price(rat(0)), Quantity: qty(rat(0)),
			AmountKurus: RoundKurus(new(big.Rat).Mul(rat(energy), s.v.FundRate))}, "fund")
		base := energy + dist + reactive + etv + fund
		vat := add(Line{Description: fmt.Sprintf("KDV (%%%s)%s", pct(s.v.VATRate), label),
			UnitPrice: price(rat(0)), Quantity: qty(rat(0)),
			AmountKurus: RoundKurus(new(big.Rat).Mul(rat(base), s.v.VATRate))}, "vat")
		res.SubtotalKurus += energy + dist + reactive
		res.TaxKurus += etv + fund + vat
	}
	res.TotalKurus = res.SubtotalKurus + res.TaxKurus
	if in.Prior != nil {
		res.Lines = append(res.Lines, Line{Kind: "settlement",
			Description: "Tahmini fatura mahsubu — ödenen tutar (" + in.Prior.InvoiceNo + ")",
			Quantity:    qty(rat(0)), UnitPrice: price(rat(0)), AmountKurus: -in.Prior.TotalKurus,
			Meta: map[string]any{"settles_invoice_id": in.Prior.ID}})
		res.TotalKurus -= in.Prior.TotalKurus
	}
	h, err := Hash(in)
	if err != nil {
		return Result{}, err
	}
	res.InputHash = h
	return res, nil
}

// split assigns each day of the period to the tariff version valid on it.
func split(in Input) ([]segment, error) {
	vs := append([]TariffVersion(nil), in.Versions...)
	sort.Slice(vs, func(i, j int) bool { return vs[i].ValidFrom.Before(vs[j].ValidFrom) })
	var segs []segment
	cursor := in.PeriodStart
	for _, v := range vs {
		s := maxT(cursor, v.ValidFrom)
		e := in.PeriodEnd
		if v.ValidTo != nil && v.ValidTo.Before(e) {
			e = *v.ValidTo
		}
		if !e.After(s) {
			continue
		}
		if s.After(cursor) {
			return nil, ErrTariffGaps
		}
		seg := segment{v: v, start: s, end: e}
		for _, d := range in.Days {
			if !d.Day.Before(s) && d.Day.Before(e) {
				seg.usage = append(seg.usage, d)
			}
		}
		segs = append(segs, seg)
		cursor = e
	}
	if len(segs) == 0 {
		return nil, ErrNoTariff
	}
	if cursor.Before(in.PeriodEnd) {
		return nil, ErrTariffGaps
	}
	return segs, nil
}

func maxT(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Hash is the canonical digest of every input that influences the invoice.
// big.Rat marshals as an exact fraction string, so the digest is stable.
func Hash(in Input) (string, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
