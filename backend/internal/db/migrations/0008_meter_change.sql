-- 0008: sayaç değişimi için açılış endeksi + okuma alma istatistikleri
ALTER TABLE meters ADD COLUMN initial_index_wh bigint NOT NULL DEFAULT 0 CHECK (initial_index_wh >= 0);
ALTER TABLE meters ADD COLUMN initial_reactive_varh bigint NOT NULL DEFAULT 0 CHECK (initial_reactive_varh >= 0);
ALTER TABLE meters ADD COLUMN final_reactive_varh bigint CHECK (final_reactive_varh >= 0);
ALTER TABLE meters ADD CONSTRAINT meters_final_when_removed CHECK (removed_at IS NOT NULL OR final_index_wh IS NULL);

-- Her okuma partisinin özeti (arayüzde "sistem canlı" göstergesi ve doğrulama için).
CREATE TABLE ingest_batches (
    id          bigserial PRIMARY KEY,
    source      text NOT NULL,
    received    int  NOT NULL,
    inserted    int  NOT NULL,
    duplicates  int  NOT NULL,
    corrections int  NOT NULL,
    rejected    int  NOT NULL,
    min_read_at timestamptz,
    max_read_at timestamptz,
    duration_ms int  NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ingest_batches_created_idx ON ingest_batches (created_at DESC);
