package billing

import (
	"errors"
	"math/big"
	"testing"
	"time"
)

func r(s string) *big.Rat {
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return v
}

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }
func dp(t time.Time) *time.Time                { return &t }

func mesken(id int64, from time.Time, to *time.Time, prices [3]string, dist string) TariffVersion {
	return TariffVersion{ID: id, ValidFrom: from, ValidTo: to, Mode: "tiered",
		DistributionPrice: r(dist), ReactivePrice: r("0"), ReactiveFreeRatio: r("0.2"),
		EnergyTaxRate: r("0.05"), FundRate: r("0.02"), VATRate: r("0.20"),
		Tiers: []Tier{{r("0"), r("150"), r(prices[0])}, {r("150"), r("300"), r(prices[1])}, {r("300"), nil, r(prices[2])}}}
}

// month returns daily usage for [start, end) with wh per day, split evenly
// across bands as 50% day / 25% peak / 25% night.
func month(start, end time.Time, wh int64) []DayUsage {
	var out []DayUsage
	for t := start; t.Before(end); t = t.AddDate(0, 0, 1) {
		out = append(out, DayUsage{Day: t, ActiveWh: wh, DayWh: wh / 2, PeakWh: wh / 4, NightWh: wh - wh/2 - wh/4, Slots: 96})
	}
	return out
}

func sumKind(res Result, kind string) int64 {
	var s int64
	for _, l := range res.Lines {
		if l.Kind == kind {
			s += l.AmountKurus
		}
	}
	return s
}

func TestRoundKurusHalfAwayFromZero(t *testing.T) {
	cases := map[string]int64{"0.5": 1, "1.4999": 1, "2.5": 3, "-2.5": -3, "-0.4": 0, "1234567.5": 1234568, "7/3": 2}
	for in, want := range cases {
		if got := RoundKurus(r(in)); got != want {
			t.Errorf("RoundKurus(%s) = %d, want %d", in, got, want)
		}
	}
}

// 30 günlük dönem, tek tarife, 400 kWh: 150 × 182.3 + 150 × 245.6 + 100 × 321.9.
func TestTieredSingleVersion(t *testing.T) {
	in := Input{SubscriberID: 1, PeriodStart: d(2026, 9, 1), PeriodEnd: d(2026, 10, 1),
		Days:     month(d(2026, 9, 1), d(2026, 10, 1), 400_000/30),
		Versions: []TariffVersion{mesken(1, d(2026, 1, 1), nil, [3]string{"182.3", "245.6", "321.9"}, "95")}}
	in.Days[0].ActiveWh += 400_000 - 30*(400_000/30) // tam 400 kWh
	res, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	energy := sumKind(res, "energy")
	want := RoundKurus(r("27345")) + RoundKurus(r("36840")) + RoundKurus(r("32190"))
	if energy != want {
		t.Fatalf("energy %d want %d (%+v)", energy, want, res.Lines)
	}
	dist := sumKind(res, "distribution")
	if dist != 400*95 {
		t.Fatalf("distribution %d", dist)
	}
	etv, fund, vat := sumKind(res, "energy_tax"), sumKind(res, "fund"), sumKind(res, "vat")
	if etv != RoundKurus(new(big.Rat).Mul(rat(energy), r("0.05"))) || fund != RoundKurus(new(big.Rat).Mul(rat(energy), r("0.02"))) {
		t.Fatalf("etv %d fund %d", etv, fund)
	}
	if vat != RoundKurus(new(big.Rat).Mul(rat(energy+dist+etv+fund), r("0.2"))) {
		t.Fatalf("vat %d", vat)
	}
	if res.TotalKurus != energy+dist+etv+fund+vat || res.SubtotalKurus+res.TaxKurus != res.TotalKurus {
		t.Fatalf("totals %+v", res)
	}
}

// Ekim 2026: 1–14 eski tarife (14 gün), 15–31 yeni tarife (17 gün).
// Kademe sınırları 14/31 ve 17/31 oranında küçülür.
func TestMidPeriodTariffChangeSplitsByDays(t *testing.T) {
	ps, pe := d(2026, 10, 1), d(2026, 11, 1)
	in := Input{SubscriberID: 1, PeriodStart: ps, PeriodEnd: pe, Days: month(ps, pe, 10_000), // 10 kWh/gün = 310 kWh
		Versions: []TariffVersion{
			mesken(1, d(2026, 1, 1), dp(d(2026, 10, 15)), [3]string{"182.3", "245.6", "321.9"}, "95"),
			mesken(2, d(2026, 10, 15), nil, [3]string{"199.8", "268.4", "352.0"}, "104.5"),
		}}
	res, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	// Parça 1: 140 kWh; 1. kademe sınırı 150×14/31 = 67.742 → 67.742 + 67.742 + 4.516
	// Parça 2: 170 kWh; 1. kademe sınırı 150×17/31 = 82.258 → 82.258 + 82.258 + 5.484
	var tiers []string
	for _, l := range res.Lines {
		if l.Kind == "energy" {
			tiers = append(tiers, l.Quantity)
		}
	}
	want := []string{"67.742", "67.742", "4.516", "82.258", "82.258", "5.484"}
	if len(tiers) != len(want) {
		t.Fatalf("tier lines %v", tiers)
	}
	for i := range want {
		if tiers[i] != want[i] {
			t.Fatalf("tier %d qty %s want %s (all %v)", i, tiers[i], want[i], tiers)
		}
	}
	// Dağıtım: 140 × 95 + 170 × 104.5
	if got := sumKind(res, "distribution"); got != 140*95+RoundKurus(r("17765")) {
		t.Fatalf("distribution %d", got)
	}
	// Her parçanın kendi vergi satırları var.
	n := 0
	for _, l := range res.Lines {
		if l.Kind == "vat" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected 2 VAT lines, got %d", n)
	}
}

func TestTimeOfUseAndReactive(t *testing.T) {
	ps, pe := d(2026, 9, 1), d(2026, 10, 1)
	days := month(ps, pe, 100_000) // 3000 kWh
	for i := range days {
		days[i].ReactiveVarh = 30_000 // 900 kVArh; serbest %20 = 600 → 300 aşım
	}
	v := TariffVersion{ID: 4, ValidFrom: d(2026, 1, 1), Mode: "tou",
		DayPrice: r("230.9"), PeakPrice: r("351.7"), NightPrice: r("142.6"),
		DistributionPrice: r("88"), ReactivePrice: r("52"), ReactiveFreeRatio: r("0.20"),
		EnergyTaxRate: r("0.01"), FundRate: r("0.02"), VATRate: r("0.20")}
	res, err := Compute(Input{SubscriberID: 9, PeriodStart: ps, PeriodEnd: pe, Days: days, Versions: []TariffVersion{v}})
	if err != nil {
		t.Fatal(err)
	}
	wantEnergy := RoundKurus(r("1500").Mul(r("1500"), r("230.9"))) + RoundKurus(new(big.Rat).Mul(r("750"), r("351.7"))) + RoundKurus(new(big.Rat).Mul(r("750"), r("142.6")))
	if got := sumKind(res, "energy"); got != wantEnergy {
		t.Fatalf("tou energy %d want %d", got, wantEnergy)
	}
	if got := sumKind(res, "reactive"); got != 300*52 {
		t.Fatalf("reactive %d", got)
	}
	// Reaktif serbest sınırın altındaysa satır yok.
	for i := range days {
		days[i].ReactiveVarh = 10_000
	}
	res, _ = Compute(Input{SubscriberID: 9, PeriodStart: ps, PeriodEnd: pe, Days: days, Versions: []TariffVersion{v}})
	if sumKind(res, "reactive") != 0 {
		t.Fatal("reactive charged under the free ratio")
	}
}

func TestSettlementOfEstimatedInvoice(t *testing.T) {
	ps, pe := d(2026, 9, 1), d(2026, 10, 1)
	in := Input{SubscriberID: 1, PeriodStart: ps, PeriodEnd: pe, Days: month(ps, pe, 5_000),
		Versions: []TariffVersion{mesken(1, d(2026, 1, 1), nil, [3]string{"182.3", "245.6", "321.9"}, "95")}}
	full, _ := Compute(in)
	in.Prior = &PriorInvoice{ID: 77, InvoiceNo: "EH202609-00000001-T1", TotalKurus: 30_000}
	settled, err := Compute(in)
	if err != nil {
		t.Fatal(err)
	}
	last := settled.Lines[len(settled.Lines)-1]
	if last.Kind != "settlement" || last.AmountKurus != -30_000 || settled.TotalKurus != full.TotalKurus-30_000 {
		t.Fatalf("settlement: %+v total %d vs %d", last, settled.TotalKurus, full.TotalKurus)
	}
	if settled.InputHash == full.InputHash {
		t.Fatal("prior invoice must be part of the input hash")
	}
}

func TestComputeIsDeterministic(t *testing.T) {
	ps, pe := d(2026, 10, 1), d(2026, 11, 1)
	mk := func() Input {
		return Input{SubscriberID: 1, PeriodStart: ps, PeriodEnd: pe, Days: month(ps, pe, 12_345),
			Versions: []TariffVersion{
				mesken(2, d(2026, 10, 15), nil, [3]string{"199.8", "268.4", "352.0"}, "104.5"),
				mesken(1, d(2026, 1, 1), dp(d(2026, 10, 15)), [3]string{"182.3", "245.6", "321.9"}, "95"),
			}}
	}
	a, _ := Compute(mk())
	for i := 0; i < 20; i++ {
		b, _ := Compute(mk())
		if a.InputHash != b.InputHash || a.TotalKurus != b.TotalKurus || len(a.Lines) != len(b.Lines) {
			t.Fatal("non-deterministic result")
		}
	}
	in := mk()
	in.Days[3].ActiveWh++
	c, _ := Compute(in)
	if c.InputHash == a.InputHash {
		t.Fatal("hash ignores consumption change")
	}
}

func TestTariffCoverageErrors(t *testing.T) {
	ps, pe := d(2026, 10, 1), d(2026, 11, 1)
	v := mesken(1, d(2026, 10, 10), nil, [3]string{"1", "2", "3"}, "1")
	if _, err := Compute(Input{PeriodStart: ps, PeriodEnd: pe, Versions: []TariffVersion{v}}); !errors.Is(err, ErrTariffGaps) {
		t.Fatalf("gap at start: %v", err)
	}
	if _, err := Compute(Input{PeriodStart: ps, PeriodEnd: pe}); !errors.Is(err, ErrNoTariff) {
		t.Fatalf("no tariff: %v", err)
	}
	if _, err := Compute(Input{PeriodStart: pe, PeriodEnd: ps}); !errors.Is(err, ErrBadPeriod) {
		t.Fatalf("bad period: %v", err)
	}
}

func TestZeroConsumptionStillBillsNothingNegative(t *testing.T) {
	ps, pe := d(2026, 9, 1), d(2026, 10, 1)
	res, err := Compute(Input{PeriodStart: ps, PeriodEnd: pe, Days: month(ps, pe, 0),
		Versions: []TariffVersion{mesken(1, d(2026, 1, 1), nil, [3]string{"182.3", "245.6", "321.9"}, "95")}})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalKurus != 0 {
		t.Fatalf("total %d", res.TotalKurus)
	}
}
