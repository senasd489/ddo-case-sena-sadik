package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/audit"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/db"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/metrics"
)

var (
	ErrInputsChanged  = errors.New("invoice inputs changed since issue")
	ErrSubscriberGone = errors.New("subscriber not found")
	ErrOverpayment    = errors.New("payment exceeds open amount")
	ErrNotPayable     = errors.New("invoice is not payable")
	ErrInvoiceMissing = errors.New("invoice not found")
)

var invoicesIssued = metrics.NewCounter("enerjihat_invoices_issued_total", "Kesilen faturalar", "kind")

// Default daily usage (Wh) per category when a subscriber has no history at all.
var defaultDailyWh = map[string]int64{"residential": 8_000, "commercial": 40_000, "industrial": 400_000, "agricultural": 60_000}

type Invoice struct {
	ID               int64      `json:"id"`
	InvoiceNo        string     `json:"invoice_no"`
	SubscriberID     int64      `json:"subscriber_id"`
	PeriodStart      string     `json:"period_start"`
	PeriodEnd        string     `json:"period_end"`
	Kind             string     `json:"kind"`
	Status           string     `json:"status"`
	Revision         int        `json:"revision"`
	ConsumptionWh    int64      `json:"consumption_wh"`
	EstimatedWh      int64      `json:"estimated_wh"`
	SubtotalKurus    int64      `json:"subtotal_kurus"`
	TaxKurus         int64      `json:"tax_kurus"`
	TotalKurus       int64      `json:"total_kurus"`
	PaidKurus        int64      `json:"paid_kurus"`
	InputHash        string     `json:"input_hash"`
	SettlesInvoiceID *int64     `json:"settles_invoice_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	Lines            []Line     `json:"lines,omitempty"`
	Breakdown        []DayUsage `json:"breakdown,omitempty"`
}

// PeriodBounds parses "YYYY-MM" into [first day, first day of next month).
func PeriodBounds(p string) (time.Time, time.Time, error) {
	t, err := time.Parse("2006-01", p)
	if err != nil {
		return time.Time{}, time.Time{}, ErrBadPeriod
	}
	return t, t.AddDate(0, 1, 0), nil
}

func ratCol(s *string) *big.Rat {
	if s == nil {
		return nil
	}
	r, _ := new(big.Rat).SetString(*s)
	return r
}

// LoadInput gathers everything the engine needs for one subscriber-period
// and fills missing days with an estimate. It reports whether the result
// must be an estimated invoice.
func LoadInput(ctx context.Context, q db.Querier, subID int64, ps, pe time.Time) (Input, bool, error) {
	in := Input{SubscriberID: subID, PeriodStart: ps, PeriodEnd: pe}
	var tariffID int64
	var category string
	err := q.QueryRow(ctx, `SELECT tariff_id, category FROM subscribers WHERE id = $1`, subID).Scan(&tariffID, &category)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, false, ErrSubscriberGone
	}
	if err != nil {
		return in, false, err
	}
	rows, err := q.Query(ctx, `SELECT id, valid_from, valid_to, mode, day_price::text, peak_price::text, night_price::text,
			distribution_price::text, reactive_price::text, reactive_free_ratio::text, energy_tax_rate::text, fund_rate::text, vat_rate::text
		FROM tariff_versions WHERE tariff_id = $1 AND valid_from < $3 AND (valid_to IS NULL OR valid_to > $2)
		ORDER BY valid_from`, tariffID, ps, pe)
	if err != nil {
		return in, false, err
	}
	for rows.Next() {
		var v TariffVersion
		var dayP, peakP, nightP, dist, react, ratio, etv, fund, vat *string
		if err := rows.Scan(&v.ID, &v.ValidFrom, &v.ValidTo, &v.Mode, &dayP, &peakP, &nightP, &dist, &react, &ratio, &etv, &fund, &vat); err != nil {
			rows.Close()
			return in, false, err
		}
		v.DayPrice, v.PeakPrice, v.NightPrice = ratCol(dayP), ratCol(peakP), ratCol(nightP)
		v.DistributionPrice, v.ReactivePrice, v.ReactiveFreeRatio = ratCol(dist), ratCol(react), ratCol(ratio)
		v.EnergyTaxRate, v.FundRate, v.VATRate = ratCol(etv), ratCol(fund), ratCol(vat)
		in.Versions = append(in.Versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, false, err
	}
	for i := range in.Versions {
		trs, err := q.Query(ctx, `SELECT from_kwh::text, to_kwh::text, unit_price::text FROM tariff_tiers WHERE version_id = $1 ORDER BY tier_no`, in.Versions[i].ID)
		if err != nil {
			return in, false, err
		}
		for trs.Next() {
			var f, t, p *string
			if err := trs.Scan(&f, &t, &p); err != nil {
				trs.Close()
				return in, false, err
			}
			in.Versions[i].Tiers = append(in.Versions[i].Tiers, Tier{From: ratCol(f), To: ratCol(t), Price: ratCol(p)})
		}
		trs.Close()
	}

	// Günlük tüketim (abonenin tüm sayaçları toplanır: sayaç değişimi günü iki sayaç olabilir).
	rows, err = q.Query(ctx, `SELECT day, sum(active_wh), sum(day_wh), sum(peak_wh), sum(night_wh), sum(reactive_varh),
			sum(slots)::int, sum(interpolated_slots)::int
		FROM consumption_daily WHERE subscriber_id = $1 AND day >= $2 AND day < $3 GROUP BY day ORDER BY day`, subID, ps, pe)
	if err != nil {
		return in, false, err
	}
	measured := map[time.Time]DayUsage{}
	for rows.Next() {
		var u DayUsage
		if err := rows.Scan(&u.Day, &u.ActiveWh, &u.DayWh, &u.PeakWh, &u.NightWh, &u.ReactiveVarh, &u.Slots, &u.Interpolated); err != nil {
			rows.Close()
			return in, false, err
		}
		u.Day = time.Date(u.Day.Year(), u.Day.Month(), u.Day.Day(), 0, 0, 0, 0, time.UTC)
		measured[u.Day] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return in, false, err
	}

	// Tahmin profili: dönemdeki tam günlerin ortalaması; yoksa son 90 günün
	// ortalaması; o da yoksa kategori varsayılanı.
	var prof DayUsage
	var full int64
	for _, u := range measured {
		if u.Slots >= 96 {
			prof.ActiveWh += u.ActiveWh
			prof.DayWh += u.DayWh
			prof.PeakWh += u.PeakWh
			prof.NightWh += u.NightWh
			prof.ReactiveVarh += u.ReactiveVarh
			full++
		}
	}
	if full == 0 {
		err := q.QueryRow(ctx, `SELECT count(*), coalesce(sum(active_wh),0), coalesce(sum(day_wh),0), coalesce(sum(peak_wh),0),
				coalesce(sum(night_wh),0), coalesce(sum(reactive_varh),0)
			FROM (SELECT day, sum(active_wh) active_wh, sum(day_wh) day_wh, sum(peak_wh) peak_wh, sum(night_wh) night_wh,
			             sum(reactive_varh) reactive_varh, sum(slots) slots
			      FROM consumption_daily WHERE subscriber_id = $1 AND day >= $2::date - 90 AND day < $2 GROUP BY day) x
			WHERE slots >= 96`, subID, ps).Scan(&full, &prof.ActiveWh, &prof.DayWh, &prof.PeakWh, &prof.NightWh, &prof.ReactiveVarh)
		if err != nil {
			return in, false, err
		}
	}
	if full == 0 {
		w := defaultDailyWh[category]
		prof = DayUsage{ActiveWh: w, DayWh: w / 2, PeakWh: w / 4, NightWh: w - w/2 - w/4}
		full = 1
	}
	avg := func(total int64, missingSlots int) int64 { return total * int64(missingSlots) / (full * 96) }

	estimated := false
	for t := ps; t.Before(pe); t = t.AddDate(0, 0, 1) {
		u, ok := measured[t]
		if !ok {
			u = DayUsage{Day: t}
		}
		if u.Slots < 96 {
			miss := 96 - u.Slots
			ad, ap, an := avg(prof.DayWh, miss), avg(prof.PeakWh, miss), avg(prof.NightWh, miss)
			u.DayWh += ad
			u.PeakWh += ap
			u.NightWh += an
			u.ReactiveVarh += avg(prof.ReactiveVarh, miss)
			u.EstimatedWh = ad + ap + an
			u.ActiveWh += u.EstimatedWh
			u.Estimated = true
			estimated = true
		}
		in.Days = append(in.Days, u)
	}
	return in, estimated, nil
}

type GenerateOptions struct {
	Reissue bool // girdiler değiştiyse eski faturayı iptal edip yenisini kes
}

// Generate issues (or returns the already issued) invoice of a period.
//
// Re-running it with unchanged inputs returns the same invoice (created=false):
// the engine is deterministic and the input hash is stored. If the inputs
// changed (e.g. a back-dated reading correction), it returns ErrInputsChanged
// unless opts.Reissue is set, in which case the old invoice is cancelled and
// a new revision issued. An advisory lock per subscriber serialises
// concurrent generation of the same subscriber's invoices.
func Generate(ctx context.Context, pool *pgxpool.Pool, subID int64, period string, opts GenerateOptions) (*Invoice, bool, error) {
	ps, pe, err := PeriodBounds(period)
	if err != nil {
		return nil, false, err
	}
	var inv *Invoice
	created := false
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4242, $1::int)`, subID); err != nil {
			return err
		}
		in, estimated, err := LoadInput(ctx, tx, subID, ps, pe)
		if err != nil {
			return err
		}
		kind := "actual"
		if estimated {
			kind = "estimated"
		}
		existing, err := openInvoices(ctx, tx, subID, ps)
		if err != nil {
			return err
		}
		var openEstimate *Invoice
		if kind == "actual" {
			if a, ok := existing["actual"]; ok && a.SettlesInvoiceID != nil {
				// Daha önce mahsup edilmiş tahmini fatura girdinin parçası olmaya devam eder.
				prev, err := scanInvoice(tx.QueryRow(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, *a.SettlesInvoiceID))
				if err != nil {
					return err
				}
				in.Prior = &PriorInvoice{ID: prev.ID, InvoiceNo: prev.InvoiceNo, TotalKurus: prev.PaidKurus}
			} else if e, ok := existing["estimated"]; ok {
				openEstimate = e
				in.Prior = &PriorInvoice{ID: e.ID, InvoiceNo: e.InvoiceNo, TotalKurus: e.PaidKurus}
			}
		} else if a, ok := existing["actual"]; ok {
			// Gerçek fatura zaten kesilmiş; veri sonradan eksilmiş görünüyor → değiştirme.
			inv = a
			return nil
		}
		res, err := Compute(in)
		if err != nil {
			return err
		}
		if old, ok := existing[kind]; ok {
			if old.InputHash == res.InputHash {
				inv = old
				return nil
			}
			if !opts.Reissue {
				inv = old
				return ErrInputsChanged
			}
			if _, err := tx.Exec(ctx, `UPDATE invoices SET status = 'cancelled', cancelled_at = now() WHERE id = $1 AND status = 'issued'`, old.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{Action: "invoice.cancel", Entity: "invoice", EntityID: strconv.FormatInt(old.ID, 10),
				Old: map[string]any{"status": old.Status, "total_kurus": old.TotalKurus, "input_hash": old.InputHash},
				New: map[string]any{"status": "cancelled", "reason": "reissue"}}); err != nil {
				return err
			}
		}
		inv, err = insertInvoice(ctx, tx, in, res, kind)
		if err != nil {
			return err
		}
		if openEstimate != nil {
			// Tahmini fatura kapanır; ödenmemiş kısmı artık geçersiz, ödenen kısmı mahsup edildi.
			if _, err := tx.Exec(ctx, `UPDATE invoices SET status = 'settled' WHERE id = $1`, openEstimate.ID); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{Action: "invoice.settle", Entity: "invoice", EntityID: strconv.FormatInt(openEstimate.ID, 10),
				Old: map[string]any{"status": openEstimate.Status, "paid_kurus": openEstimate.PaidKurus},
				New: map[string]any{"status": "settled", "settled_by_invoice_id": inv.ID}}); err != nil {
				return err
			}
		}
		created = true
		invoicesIssued.Inc(kind)
		return audit.Record(ctx, tx, audit.Entry{Action: "invoice.issue", Entity: "invoice", EntityID: strconv.FormatInt(inv.ID, 10),
			New: map[string]any{"invoice_no": inv.InvoiceNo, "kind": kind, "total_kurus": inv.TotalKurus, "input_hash": inv.InputHash,
				"settles_invoice_id": inv.SettlesInvoiceID}})
	})
	return inv, created, err
}

func openInvoices(ctx context.Context, tx pgx.Tx, subID int64, ps time.Time) (map[string]*Invoice, error) {
	rows, err := tx.Query(ctx, `SELECT `+invCols+` FROM invoices WHERE subscriber_id = $1 AND period_start = $2 AND status NOT IN ('cancelled','settled')`, subID, ps)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Invoice{}
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out[inv.Kind] = inv
	}
	return out, rows.Err()
}

const invCols = `id, invoice_no, subscriber_id, to_char(period_start,'YYYY-MM-DD'), to_char(period_end,'YYYY-MM-DD'), kind, status, revision,
	consumption_wh, estimated_wh, subtotal_kurus, tax_kurus, total_kurus, paid_kurus, input_hash, settles_invoice_id, created_at`

func scanInvoice(row pgx.Row) (*Invoice, error) {
	var i Invoice
	err := row.Scan(&i.ID, &i.InvoiceNo, &i.SubscriberID, &i.PeriodStart, &i.PeriodEnd, &i.Kind, &i.Status, &i.Revision,
		&i.ConsumptionWh, &i.EstimatedWh, &i.SubtotalKurus, &i.TaxKurus, &i.TotalKurus, &i.PaidKurus, &i.InputHash,
		&i.SettlesInvoiceID, &i.CreatedAt)
	return &i, err
}

func insertInvoice(ctx context.Context, tx pgx.Tx, in Input, res Result, kind string) (*Invoice, error) {
	var rev int
	if err := tx.QueryRow(ctx, `SELECT count(*) + 1 FROM invoices WHERE subscriber_id = $1 AND period_start = $2 AND kind = $3`,
		in.SubscriberID, in.PeriodStart, kind).Scan(&rev); err != nil {
		return nil, err
	}
	letter := "G"
	if kind == "estimated" {
		letter = "T"
	}
	no := fmt.Sprintf("EH%s-%08d-%s%d", in.PeriodStart.Format("200601"), in.SubscriberID, letter, rev)
	breakdown, err := json.Marshal(in.Days)
	if err != nil {
		return nil, err
	}
	var settles *int64
	if in.Prior != nil {
		settles = &in.Prior.ID
	}
	inv, err := scanInvoice(tx.QueryRow(ctx, `INSERT INTO invoices (invoice_no, subscriber_id, period_start, period_end, kind, consumption_wh,
			estimated_wh, subtotal_kurus, tax_kurus, total_kurus, input_hash, settles_invoice_id, breakdown, revision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING `+invCols,
		no, in.SubscriberID, in.PeriodStart, in.PeriodEnd, kind, res.ConsumptionWh, res.EstimatedWh,
		res.SubtotalKurus, res.TaxKurus, res.TotalKurus, res.InputHash, settles, breakdown, rev))
	if err != nil {
		return nil, err
	}
	rows := make([][]any, len(res.Lines))
	for i, l := range res.Lines {
		meta, _ := json.Marshal(l.Meta)
		rows[i] = []any{inv.ID, int16(i + 1), l.Kind, l.Description, l.Quantity, l.UnitPrice, l.AmountKurus, meta}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"invoice_lines"},
		[]string{"invoice_id", "line_no", "kind", "description", "quantity", "unit_price", "amount_kurus", "meta"},
		pgx.CopyFromRows(rows)); err != nil {
		return nil, err
	}
	inv.Lines = res.Lines
	inv.Breakdown = in.Days
	return inv, nil
}

// Get loads an invoice with its lines and daily breakdown.
func Get(ctx context.Context, q db.Querier, id int64) (*Invoice, error) {
	inv, err := scanInvoice(q.QueryRow(ctx, `SELECT `+invCols+` FROM invoices WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvoiceMissing
	}
	if err != nil {
		return nil, err
	}
	var bd []byte
	if err := q.QueryRow(ctx, `SELECT breakdown FROM invoices WHERE id = $1`, id).Scan(&bd); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(bd, &inv.Breakdown); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT kind, description, quantity::text, unit_price::text, amount_kurus, meta FROM invoice_lines
		WHERE invoice_id = $1 ORDER BY line_no`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.Kind, &l.Description, &l.Quantity, &l.UnitPrice, &l.AmountKurus, &l.Meta); err != nil {
			return nil, err
		}
		inv.Lines = append(inv.Lines, l)
	}
	return inv, rows.Err()
}

type Payment struct {
	ID          int64     `json:"id"`
	InvoiceID   int64     `json:"invoice_id"`
	AmountKurus int64     `json:"amount_kurus"`
	PaidAt      time.Time `json:"paid_at"`
	Remaining   int64     `json:"remaining_kurus"`
	Status      string    `json:"invoice_status"`
}

// Pay records a payment. The invoice row is locked with SELECT … FOR UPDATE
// so two concurrent payments can never together exceed the open amount.
func Pay(ctx context.Context, pool *pgxpool.Pool, invoiceID, amount, userID int64) (*Payment, error) {
	if amount <= 0 {
		return nil, ErrOverpayment
	}
	var p Payment
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var total, paid int64
		var status string
		err := tx.QueryRow(ctx, `SELECT total_kurus, paid_kurus, status FROM invoices WHERE id = $1 FOR UPDATE`, invoiceID).Scan(&total, &paid, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvoiceMissing
		}
		if err != nil {
			return err
		}
		if status != "issued" {
			return ErrNotPayable
		}
		if paid+amount > total {
			return ErrOverpayment
		}
		var uid *int64
		if userID > 0 {
			uid = &userID
		}
		if err := tx.QueryRow(ctx, `INSERT INTO payments (invoice_id, amount_kurus, paid_by) VALUES ($1,$2,$3) RETURNING id, paid_at`,
			invoiceID, amount, uid).Scan(&p.ID, &p.PaidAt); err != nil {
			return err
		}
		newStatus := status
		if paid+amount == total {
			newStatus = "paid"
		}
		if _, err := tx.Exec(ctx, `UPDATE invoices SET paid_kurus = paid_kurus + $2, status = $3 WHERE id = $1`, invoiceID, amount, newStatus); err != nil {
			return err
		}
		p.InvoiceID, p.AmountKurus, p.Remaining, p.Status = invoiceID, amount, total-paid-amount, newStatus
		return audit.Record(ctx, tx, audit.Entry{Action: "payment.create", Entity: "invoice", EntityID: strconv.FormatInt(invoiceID, 10),
			Old: map[string]any{"paid_kurus": paid, "status": status},
			New: map[string]any{"paid_kurus": paid + amount, "status": newStatus, "payment_id": p.ID}})
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}
