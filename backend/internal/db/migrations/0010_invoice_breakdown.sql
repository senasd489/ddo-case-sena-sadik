-- 0010: fatura dökümü (günlük tüketim + interpolasyon/tahmin işaretleri) ve revizyon
ALTER TABLE invoices ADD COLUMN breakdown jsonb NOT NULL DEFAULT '[]';
ALTER TABLE invoices ADD COLUMN revision smallint NOT NULL DEFAULT 1 CHECK (revision >= 1);
ALTER TABLE invoices ADD COLUMN cancelled_at timestamptz;
ALTER TABLE invoices ADD CONSTRAINT invoices_cancel_ts CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL));
-- Mahsup edilen tahmini fatura başka bir gerçek faturada ikinci kez mahsup edilemez.
CREATE UNIQUE INDEX invoices_settles_once ON invoices (settles_invoice_id) WHERE settles_invoice_id IS NOT NULL AND status <> 'cancelled';
-- Gerçek fatura kesilince tahmini fatura 'settled' olur; ödenmiş kısmı gerçek faturada mahsup edilir.
ALTER TABLE invoices DROP CONSTRAINT invoices_status_check;
ALTER TABLE invoices ADD CONSTRAINT invoices_status_check CHECK (status IN ('issued','paid','cancelled','settled'));
