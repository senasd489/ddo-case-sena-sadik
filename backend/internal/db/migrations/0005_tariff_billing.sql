-- 0005: tarifeler, faturalar, ödemeler
--
-- Para: tutarlar bigint KURUŞ. Birim fiyatlar numeric (kuruş/kWh, 4 ondalık);
-- satır tutarı hesaplandıktan sonra tek noktada kuruşa yuvarlanır.

CREATE TABLE tariffs (
    id        bigserial PRIMARY KEY,
    code      text NOT NULL UNIQUE,
    name      text NOT NULL,
    category  text NOT NULL CHECK (category IN ('residential','commercial','industrial','agricultural'))
);

-- Bir tarifenin dönemsel sürümleri; aynı tarifede tarih aralıkları çakışamaz.
CREATE TABLE tariff_versions (
    id                  bigserial PRIMARY KEY,
    tariff_id           bigint  NOT NULL REFERENCES tariffs(id),
    valid_from          date    NOT NULL,
    valid_to            date,   -- dahil değil; NULL = açık uçlu
    mode                text    NOT NULL CHECK (mode IN ('tiered','tou')),
    -- Zaman dilimli (tou) birim fiyatlar, kuruş/kWh
    day_price           numeric(12,4) CHECK (day_price >= 0),
    peak_price          numeric(12,4) CHECK (peak_price >= 0),
    night_price         numeric(12,4) CHECK (night_price >= 0),
    distribution_price  numeric(12,4) NOT NULL CHECK (distribution_price >= 0),
    reactive_price      numeric(12,4) NOT NULL DEFAULT 0 CHECK (reactive_price >= 0), -- kuruş/kVArh
    reactive_free_ratio numeric(5,4)  NOT NULL DEFAULT 0.20 CHECK (reactive_free_ratio BETWEEN 0 AND 1),
    energy_tax_rate     numeric(6,4)  NOT NULL DEFAULT 0.05 CHECK (energy_tax_rate BETWEEN 0 AND 1), -- BTV/ETV
    fund_rate           numeric(6,4)  NOT NULL DEFAULT 0.02 CHECK (fund_rate BETWEEN 0 AND 1),       -- TRT/enerji fonu
    vat_rate            numeric(6,4)  NOT NULL DEFAULT 0.20 CHECK (vat_rate BETWEEN 0 AND 1),
    CONSTRAINT tv_range CHECK (valid_to IS NULL OR valid_to > valid_from),
    CONSTRAINT tv_tou_prices CHECK (mode <> 'tou' OR (day_price IS NOT NULL AND peak_price IS NOT NULL AND night_price IS NOT NULL)),
    CONSTRAINT tv_no_overlap EXCLUDE USING gist (tariff_id WITH =, daterange(valid_from, valid_to, '[)') WITH &&)
);

-- Kademeli tarife basamakları: [from_kwh, to_kwh) aralığı; son basamakta to_kwh NULL.
CREATE TABLE tariff_tiers (
    version_id  bigint NOT NULL REFERENCES tariff_versions(id) ON DELETE CASCADE,
    tier_no     smallint NOT NULL CHECK (tier_no >= 1),
    from_kwh    numeric(12,3) NOT NULL CHECK (from_kwh >= 0),
    to_kwh      numeric(12,3),
    unit_price  numeric(12,4) NOT NULL CHECK (unit_price >= 0),
    PRIMARY KEY (version_id, tier_no),
    CHECK (to_kwh IS NULL OR to_kwh > from_kwh)
);

ALTER TABLE subscribers ADD CONSTRAINT subscribers_tariff_fk FOREIGN KEY (tariff_id) REFERENCES tariffs(id);

CREATE TABLE invoices (
    id              bigserial PRIMARY KEY,
    invoice_no      text NOT NULL UNIQUE,
    subscriber_id   bigint NOT NULL REFERENCES subscribers(id),
    period_start    date   NOT NULL,
    period_end      date   NOT NULL,   -- dahil değil
    kind            text   NOT NULL CHECK (kind IN ('estimated','actual')),
    status          text   NOT NULL DEFAULT 'issued' CHECK (status IN ('issued','paid','cancelled')),
    consumption_wh  bigint NOT NULL CHECK (consumption_wh >= 0),
    estimated_wh    bigint NOT NULL DEFAULT 0 CHECK (estimated_wh >= 0),   -- interpolasyon/tahmin payı
    subtotal_kurus  bigint NOT NULL,
    tax_kurus       bigint NOT NULL,
    total_kurus     bigint NOT NULL,
    paid_kurus      bigint NOT NULL DEFAULT 0 CHECK (paid_kurus >= 0),
    -- Girdilerin özeti: aynı girdiyle yeniden üretim aynı faturayı döndürür.
    input_hash      text   NOT NULL,
    settles_invoice_id bigint REFERENCES invoices(id),   -- gerçek fatura → mahsup ettiği tahmini fatura
    created_by      bigint REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (period_end > period_start),
    CHECK (paid_kurus <= GREATEST(total_kurus, 0))
);
-- Abone + dönem + tür başına tek geçerli fatura.
CREATE UNIQUE INDEX invoices_one_per_period ON invoices (subscriber_id, period_start, kind) WHERE status <> 'cancelled';
CREATE INDEX invoices_sub_idx ON invoices (subscriber_id, period_start DESC, id DESC);

CREATE TABLE invoice_lines (
    invoice_id   bigint   NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    line_no      smallint NOT NULL,
    kind         text     NOT NULL CHECK (kind IN ('energy','distribution','reactive','energy_tax','fund','vat','settlement')),
    description  text     NOT NULL,
    quantity     numeric(14,3) NOT NULL DEFAULT 0,
    unit_price   numeric(12,4) NOT NULL DEFAULT 0,
    amount_kurus bigint   NOT NULL,
    meta         jsonb,
    PRIMARY KEY (invoice_id, line_no)
);

CREATE TABLE payments (
    id            bigserial PRIMARY KEY,
    invoice_id    bigint NOT NULL REFERENCES invoices(id),
    amount_kurus  bigint NOT NULL CHECK (amount_kurus > 0),
    paid_by       bigint REFERENCES users(id),
    paid_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_invoice_idx ON payments (invoice_id);
