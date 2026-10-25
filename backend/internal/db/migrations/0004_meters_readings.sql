-- 0004: sayaçlar, ham okumalar (günlük partisyon) ve 15 dakikalık tüketim
--
-- Enerji değerleri Wh / VArh cinsinden bigint tutulur: kWh'ı float ile
-- toplamak yuvarlama hatası biriktirir.

CREATE TABLE meters (
    id            bigserial PRIMARY KEY,
    serial_no     text    NOT NULL UNIQUE,
    kind          text    NOT NULL CHECK (kind IN ('subscriber','head')),
    subscriber_id bigint  REFERENCES subscribers(id),
    node_id       bigint  REFERENCES network_nodes(id),   -- trafo ana sayacı için
    multiplier    int     NOT NULL DEFAULT 1 CHECK (multiplier > 0),
    installed_at  timestamptz NOT NULL,
    removed_at    timestamptz,
    -- Sayaç değişiminde eski sayacın son endeksi (kapanış) burada saklanır.
    final_index_wh bigint CHECK (final_index_wh >= 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT meters_owner CHECK (
        (kind = 'subscriber' AND subscriber_id IS NOT NULL AND node_id IS NULL) OR
        (kind = 'head'       AND node_id IS NOT NULL AND subscriber_id IS NULL)),
    CONSTRAINT meters_period CHECK (removed_at IS NULL OR removed_at > installed_at)
);
-- Bir abonenin aynı anda tek aktif sayacı olabilir.
CREATE UNIQUE INDEX meters_one_active_per_sub ON meters (subscriber_id) WHERE removed_at IS NULL AND kind = 'subscriber';
CREATE UNIQUE INDEX meters_one_active_per_node ON meters (node_id) WHERE removed_at IS NULL AND kind = 'head';
CREATE INDEX meters_subscriber_idx ON meters (subscriber_id, installed_at);

-- Ham okumalar: sayaçtan gelen kümülatif endeks. (meter_id, read_at) başına
-- tek satır; tekrar gelen aynı okuma yok sayılır, düzeltme (correction)
-- gelirse satır güncellenir ve revision artar.
CREATE TABLE readings (
    meter_id         bigint      NOT NULL,
    read_at          timestamptz NOT NULL CHECK (extract(epoch FROM read_at)::bigint % 900 = 0),
    active_index_wh  bigint      NOT NULL CHECK (active_index_wh >= 0),
    reactive_index_varh bigint   NOT NULL DEFAULT 0 CHECK (reactive_index_varh >= 0),
    revision         smallint    NOT NULL DEFAULT 0,
    received_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meter_id, read_at)
) PARTITION BY RANGE (read_at);

-- Partisyonları gün gün oluşturan yardımcı (idempotent).
CREATE FUNCTION ensure_reading_partitions(p_from date, p_to date) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    d date := p_from;
    n int := 0;
    tbl text;
BEGIN
    WHILE d <= p_to LOOP
        tbl := format('readings_%s', to_char(d, 'YYYYMMDD'));
        IF to_regclass(tbl) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF readings FOR VALUES FROM (%L) TO (%L)',
                           tbl, d::text || ' 00:00:00+00', (d + 1)::text || ' 00:00:00+00');
            n := n + 1;
        END IF;
        tbl := format('consumption_15m_%s', to_char(d, 'YYYYMMDD'));
        IF to_regclass(tbl) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF consumption_15m FOR VALUES FROM (%L) TO (%L)',
                           tbl, d::text || ' 00:00:00+00', (d + 1)::text || ' 00:00:00+00');
        END IF;
        d := d + 1;
    END LOOP;
    RETURN n;
END $$;

-- Düzeltme geçmişi: geriye dönük düzeltmenin eski değerini kaybetmemek için.
CREATE TABLE reading_corrections (
    id           bigserial PRIMARY KEY,
    meter_id     bigint      NOT NULL REFERENCES meters(id),
    read_at      timestamptz NOT NULL,
    old_index_wh bigint,
    new_index_wh bigint      NOT NULL,
    corrected_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX reading_corrections_meter_idx ON reading_corrections (meter_id, read_at);

-- Hesaplanmış 15 dakikalık tüketim (T-1 birleştirme çıktısı).
-- quality: measured | interpolated | meter_change | reset | corrected
CREATE TABLE consumption_15m (
    meter_id       bigint      NOT NULL,
    subscriber_id  bigint,
    slot_start     timestamptz NOT NULL,
    active_wh      bigint      NOT NULL CHECK (active_wh >= 0),
    reactive_varh  bigint      NOT NULL DEFAULT 0 CHECK (reactive_varh >= 0),
    quality        text        NOT NULL CHECK (quality IN ('measured','interpolated','meter_change','reset','corrected')),
    computed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meter_id, slot_start)
) PARTITION BY RANGE (slot_start);
CREATE INDEX consumption_15m_sub_idx ON consumption_15m (subscriber_id, slot_start);

-- Günlük özet: fatura ve raporlar 15 dk satırlarını tekrar taramasın diye.
-- Zaman dilimleri: gündüz 06-17, puant 17-22, gece 22-06 (Europe/Istanbul).
CREATE TABLE consumption_daily (
    meter_id        bigint  NOT NULL REFERENCES meters(id),
    subscriber_id   bigint  REFERENCES subscribers(id),
    day             date    NOT NULL,
    active_wh       bigint  NOT NULL,
    day_wh          bigint  NOT NULL,
    peak_wh         bigint  NOT NULL,
    night_wh        bigint  NOT NULL,
    reactive_varh   bigint  NOT NULL,
    max_demand_w    bigint  NOT NULL,   -- en yüksek 15 dk ortalama güç
    slots           smallint NOT NULL,
    interpolated_slots smallint NOT NULL,
    computed_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meter_id, day),
    CHECK (day_wh + peak_wh + night_wh = active_wh)
);
CREATE INDEX consumption_daily_sub_idx ON consumption_daily (subscriber_id, day);

-- Okuma alma işinde hangi sayaç-günlerin yeniden hesaplanacağı (kirli işaret).
CREATE TABLE consumption_dirty (
    meter_id bigint NOT NULL,
    day      date   NOT NULL,
    marked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meter_id, day)
);
