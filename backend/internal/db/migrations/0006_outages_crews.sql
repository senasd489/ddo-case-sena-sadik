-- 0006: kesintiler, bildirimler (arama / son nefes), ekipler ve atamalar

CREATE TABLE crews (
    id          bigserial PRIMARY KEY,
    code        text NOT NULL UNIQUE,
    name        text NOT NULL,
    skills      text[] NOT NULL DEFAULT '{}',   -- ör. {'mv','lv','transformer'}
    lat         double precision NOT NULL,
    lon         double precision NOT NULL,
    status      text NOT NULL DEFAULT 'available' CHECK (status IN ('available','busy','off_duty')),
    -- Ortalama onarım hızı katsayısı (tahmini onarım süresi hesabında)
    speed_factor numeric(4,2) NOT NULL DEFAULT 1.0 CHECK (speed_factor > 0),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE users ADD CONSTRAINT users_crew_fk FOREIGN KEY (crew_id) REFERENCES crews(id);

-- Durum makinesi (geçişler uygulama + tetikleyici ile korunur):
--   detected → confirmed → assigned → in_progress → resolved
--   detected/confirmed/assigned → cancelled ; planned: scheduled → in_progress → resolved | cancelled
CREATE TABLE outages (
    id               bigserial PRIMARY KEY,
    kind             text NOT NULL CHECK (kind IN ('unplanned','planned')),
    status           text NOT NULL CHECK (status IN ('scheduled','detected','confirmed','assigned','in_progress','resolved','cancelled')),
    fault_node_id    bigint NOT NULL REFERENCES network_nodes(id),
    feeder_id        bigint NOT NULL REFERENCES network_nodes(id),
    required_skill   text NOT NULL DEFAULT 'lv',
    cause            text,
    started_at       timestamptz NOT NULL,
    ended_at         timestamptz,
    planned_start    timestamptz,
    planned_end      timestamptz,
    estimated_restore_at timestamptz,
    affected_count   int NOT NULL DEFAULT 0 CHECK (affected_count >= 0),
    report_count     int NOT NULL DEFAULT 0 CHECK (report_count >= 0),
    created_by       bigint REFERENCES users(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    version          int NOT NULL DEFAULT 0,
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK (kind <> 'planned' OR (planned_start IS NOT NULL AND planned_end > planned_start)),
    -- Aynı besleyicide iptal edilmemiş planlı kesintiler zamanca çakışamaz.
    CONSTRAINT outages_planned_no_overlap EXCLUDE USING gist (
        feeder_id WITH =, tstzrange(planned_start, planned_end, '[)') WITH &&
    ) WHERE (kind = 'planned' AND status <> 'cancelled')
);
CREATE INDEX outages_open_idx ON outages (feeder_id, started_at) WHERE status NOT IN ('resolved','cancelled');
CREATE INDEX outages_started_idx ON outages (started_at DESC, id DESC);

CREATE TABLE outage_status_history (
    id          bigserial PRIMARY KEY,
    outage_id   bigint NOT NULL REFERENCES outages(id),
    from_status text,
    to_status   text NOT NULL,
    changed_by  bigint REFERENCES users(id),
    changed_at  timestamptz NOT NULL DEFAULT now(),
    note        text
);
CREATE INDEX outage_status_history_idx ON outage_status_history (outage_id, id);

-- Kesintiden etkilenen aboneler: SAIDI/SAIFI bu tablodan hesaplanır.
-- Anahtarlama ile beslemesi geri gelen abonenin restored_at'i erken kapanır.
CREATE TABLE outage_affected (
    outage_id     bigint NOT NULL REFERENCES outages(id) ON DELETE CASCADE,
    subscriber_id bigint NOT NULL REFERENCES subscribers(id),
    interrupted_at timestamptz NOT NULL,
    restored_at    timestamptz,
    PRIMARY KEY (outage_id, subscriber_id),
    CHECK (restored_at IS NULL OR restored_at >= interrupted_at)
);
CREATE INDEX outage_affected_sub_idx ON outage_affected (subscriber_id);

-- Ham bildirimler: abone araması veya sayacın "son nefes" (last gasp) sinyali.
CREATE TABLE outage_signals (
    id            bigserial PRIMARY KEY,
    source        text NOT NULL CHECK (source IN ('call','last_gasp')),
    external_id   text,                         -- çağrı merkezi / sayaç mesaj kimliği
    subscriber_id bigint REFERENCES subscribers(id),
    meter_id      bigint REFERENCES meters(id),
    feeder_id     bigint NOT NULL REFERENCES network_nodes(id),
    pole_id       bigint REFERENCES network_nodes(id),
    received_at   timestamptz NOT NULL,
    outage_id     bigint REFERENCES outages(id),
    duplicate     boolean NOT NULL DEFAULT false,
    note          text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outage_signals_ext_uq UNIQUE (source, external_id)
);
CREATE INDEX outage_signals_feeder_time_idx ON outage_signals (feeder_id, received_at);
CREATE INDEX outage_signals_outage_idx ON outage_signals (outage_id);

CREATE TABLE crew_assignments (
    id           bigserial PRIMARY KEY,
    outage_id    bigint NOT NULL REFERENCES outages(id),
    crew_id      bigint NOT NULL REFERENCES crews(id),
    status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active','completed','released')),
    distance_km  numeric(8,2) NOT NULL,
    eta_minutes  int NOT NULL,
    repair_minutes int NOT NULL,
    assigned_by  bigint REFERENCES users(id),
    assigned_at  timestamptz NOT NULL DEFAULT now(),
    closed_at    timestamptz
);
-- Çift atama engeli: bir ekip aynı anda tek aktif işte, bir kesintide tek aktif ekip.
CREATE UNIQUE INDEX crew_one_active_job ON crew_assignments (crew_id) WHERE status = 'active';
CREATE UNIQUE INDEX outage_one_active_crew ON crew_assignments (outage_id) WHERE status = 'active';
