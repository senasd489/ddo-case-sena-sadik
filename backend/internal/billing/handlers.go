package billing

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/auth"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/httpx"
	"github.com/senasd489/ddo-case-sena-sadik/backend/internal/idempotency"
)

type Handlers struct{ pool *pgxpool.Pool }

func NewHandlers(pool *pgxpool.Pool) *Handlers { return &Handlers{pool: pool} }

func (h *Handlers) Mount(r chi.Router) {
	billers := auth.Require(auth.RoleBilling, auth.RoleAdmin)
	readers := auth.Require(auth.RoleBilling, auth.RoleAnalyst, auth.RoleAdmin)
	self := auth.Require(auth.RoleSubscriber, auth.RoleBilling, auth.RoleAnalyst, auth.RoleAdmin)

	r.With(billers).Post("/billing/invoices/generate", httpx.H(h.generate))
	r.With(billers).Post("/billing/runs", httpx.H(h.run))
	r.With(readers).Get("/invoices", httpx.H(h.list))
	r.With(self).Get("/invoices/{id}", httpx.H(h.get))
	r.With(self).Get("/subscribers/{id}/invoices", httpx.H(h.subscriberInvoices))
	r.With(self).Get("/subscribers/{id}/invoice-preview", httpx.H(h.preview))
	// Ödeme: Idempotency-Key zorunlu (ağ hatasında tekrar denenen ödeme iki kez alınmaz).
	r.With(self, requireKey).Post("/invoices/{id}/payments", httpx.H(h.pay))
}

func requireKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(idempotency.Header) == "" {
			httpx.WriteError(w, r, httpx.BadRequest("ödeme için Idempotency-Key başlığı zorunlu"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func mapErr(err error, inv *Invoice) error {
	switch {
	case errors.Is(err, ErrBadPeriod):
		return httpx.Validation(map[string]any{"period": "YYYY-MM biçiminde olmalı"})
	case errors.Is(err, ErrSubscriberGone), errors.Is(err, ErrInvoiceMissing):
		return httpx.ErrNotFound
	case errors.Is(err, ErrNoTariff), errors.Is(err, ErrTariffGaps):
		return httpx.NewError(http.StatusUnprocessableEntity, "tariff_missing", "dönemin tamamını kapsayan tarife yok")
	case errors.Is(err, ErrInputsChanged):
		e := httpx.Conflict("invoice_inputs_changed", "fatura kesildikten sonra girdiler değişti; yeniden kesmek için reissue=true gönderin")
		if inv != nil {
			e.Details = map[string]any{"invoice_id": inv.ID, "invoice_no": inv.InvoiceNo, "input_hash": inv.InputHash}
		}
		return e
	case errors.Is(err, ErrOverpayment):
		return httpx.Conflict("overpayment", "ödeme tutarı açık bakiyeyi aşıyor")
	case errors.Is(err, ErrNotPayable):
		return httpx.Conflict("not_payable", "bu fatura ödenemez (ödenmiş, iptal ya da mahsup edilmiş)")
	}
	return err
}

type generateReq struct {
	SubscriberID int64  `json:"subscriber_id"`
	Period       string `json:"period"`
	Reissue      bool   `json:"reissue"`
}

func (h *Handlers) generate(w http.ResponseWriter, r *http.Request) error {
	var req generateReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.SubscriberID <= 0 {
		return httpx.Validation(map[string]any{"subscriber_id": "gerekli"})
	}
	inv, created, err := Generate(r.Context(), h.pool, req.SubscriberID, req.Period, GenerateOptions{Reissue: req.Reissue})
	if err != nil {
		return mapErr(err, inv)
	}
	full, err := Get(r.Context(), h.pool, inv.ID)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, map[string]any{"created": created, "invoice": full})
	return nil
}

type runReq struct {
	Period        string  `json:"period"`
	SubscriberIDs []int64 `json:"subscriber_ids"`
	Limit         int     `json:"limit"`
}

// run bills many subscribers of a period (each in its own transaction).
func (h *Handlers) run(w http.ResponseWriter, r *http.Request) error {
	var req runReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if _, _, err := PeriodBounds(req.Period); err != nil {
		return mapErr(err, nil)
	}
	ids := req.SubscriberIDs
	if len(ids) == 0 {
		if req.Limit <= 0 || req.Limit > 10000 {
			req.Limit = 1000
		}
		rows, err := h.pool.Query(r.Context(), `SELECT id FROM subscribers WHERE active ORDER BY id LIMIT $1`, req.Limit)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
	}
	out := map[string]int{"created": 0, "unchanged": 0, "changed_inputs": 0, "failed": 0, "estimated": 0, "actual": 0}
	for _, id := range ids {
		inv, created, err := Generate(r.Context(), h.pool, id, req.Period, GenerateOptions{})
		switch {
		case errors.Is(err, ErrInputsChanged):
			out["changed_inputs"]++
			continue
		case err != nil:
			out["failed"]++
			continue
		case created:
			out["created"]++
		default:
			out["unchanged"]++
		}
		out[inv.Kind]++
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"period": req.Period, "subscribers": len(ids), "result": out})
	return nil
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	pg, err := httpx.ParsePage(r, 50, 200)
	if err != nil {
		return err
	}
	before, err := pg.CursorInt64(0)
	if err != nil {
		return err
	}
	if before == 0 {
		before = 1<<63 - 1
	}
	q := r.URL.Query()
	sub, _ := strconv.ParseInt(q.Get("subscriber_id"), 10, 64)
	rows, err := h.pool.Query(r.Context(), `SELECT `+invCols+` FROM invoices
		WHERE id < $1 AND ($2 = 0 OR subscriber_id = $2) AND ($3 = '' OR status = $3) AND ($4 = '' OR to_char(period_start,'YYYY-MM') = $4)
		ORDER BY id DESC LIMIT $5`, before, sub, q.Get("status"), q.Get("period"), pg.Limit+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Invoice, error) { return scanInvoice(row) })
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, httpx.NewList(items, pg.Limit, func(i *Invoice) string {
		return httpx.EncodeCursor(strconv.FormatInt(i.ID, 10))
	}))
	return nil
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	inv, err := Get(r.Context(), h.pool, id)
	if err != nil {
		return mapErr(err, nil)
	}
	if !auth.FromContext(r.Context()).CanAccessSubscriber(inv.SubscriberID) {
		// Başka abonenin faturasının varlığını da sızdırmamak için 404.
		return httpx.ErrNotFound
	}
	httpx.JSON(w, http.StatusOK, inv)
	return nil
}

func (h *Handlers) subscriberInvoices(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	if !auth.FromContext(r.Context()).CanAccessSubscriber(id) {
		return httpx.ErrForbidden
	}
	rows, err := h.pool.Query(r.Context(), `SELECT `+invCols+` FROM invoices WHERE subscriber_id = $1 ORDER BY period_start DESC, id DESC LIMIT 36`, id)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Invoice, error) { return scanInvoice(row) })
	if err != nil {
		return err
	}
	if items == nil {
		items = []*Invoice{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

// preview computes the invoice of a period without storing it (abone portalı: tahmini fatura).
func (h *Handlers) preview(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	if !auth.FromContext(r.Context()).CanAccessSubscriber(id) {
		return httpx.ErrForbidden
	}
	period := r.URL.Query().Get("period")
	ps, pe, err := PeriodBounds(period)
	if err != nil {
		return mapErr(err, nil)
	}
	in, estimated, err := LoadInput(r.Context(), h.pool, id, ps, pe)
	if err != nil {
		return mapErr(err, nil)
	}
	res, err := Compute(in)
	if err != nil {
		return mapErr(err, nil)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"subscriber_id": id, "period": period, "estimated": estimated, "result": res, "breakdown": in.Days})
	return nil
}

type payReq struct {
	AmountKurus int64 `json:"amount_kurus"`
}

func (h *Handlers) pay(w http.ResponseWriter, r *http.Request) error {
	id, err := httpx.PathInt64(r, "id")
	if err != nil {
		return err
	}
	var req payReq
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	if req.AmountKurus <= 0 {
		return httpx.Validation(map[string]any{"amount_kurus": "pozitif olmalı"})
	}
	p := auth.FromContext(r.Context())
	if p.Role == auth.RoleSubscriber {
		var owner int64
		if err := h.pool.QueryRow(r.Context(), `SELECT subscriber_id FROM invoices WHERE id=$1`, id).Scan(&owner); err != nil || !p.CanAccessSubscriber(owner) {
			return httpx.ErrNotFound
		}
	}
	pay, err := Pay(r.Context(), h.pool, id, req.AmountKurus, p.UserID)
	if err != nil {
		return mapErr(err, nil)
	}
	httpx.JSON(w, http.StatusCreated, pay)
	return nil
}
