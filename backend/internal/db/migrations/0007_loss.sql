-- 0007: kayıp-kaçak analizi

-- Trafo bazında zaman penceresi dengesi: ana sayaç − abone toplamı.
CREATE TABLE loss_windows (
    node_id        bigint NOT NULL REFERENCES network_nodes(id),
    window_start   timestamptz NOT NULL,
    window_end     timestamptz NOT NULL,
    head_wh        bigint NOT NULL,
    subscribers_wh bigint NOT NULL,
    loss_wh        bigint NOT NULL,
    tolerance_wh   bigint NOT NULL,
    excess_wh      bigint NOT NULL,          -- tolerans üstü kayıp (≥0)
    loss_pct       numeric(7,3) NOT NULL,
    missing_slots  int NOT NULL DEFAULT 0,   -- veri eksikliği (yanlış alarm filtresi)
    computed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, window_start),
    CHECK (window_end > window_start)
);

-- Şüpheli abone işaretleri; neden işaretlendiği explanation içinde.
CREATE TABLE suspicion_flags (
    id             bigserial PRIMARY KEY,
    subscriber_id  bigint NOT NULL REFERENCES subscribers(id),
    node_id        bigint REFERENCES network_nodes(id),
    rule           text NOT NULL CHECK (rule IN ('sudden_drop','night_usage','loss_share','zero_usage')),
    score          numeric(8,3) NOT NULL,
    window_start   date NOT NULL,
    window_end     date NOT NULL,
    explanation    jsonb NOT NULL,
    status         text NOT NULL DEFAULT 'open' CHECK (status IN ('open','confirmed','dismissed')),
    run_id         bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    reviewed_by    bigint REFERENCES users(id),
    reviewed_at    timestamptz,
    UNIQUE (subscriber_id, rule, window_start)
);
CREATE INDEX suspicion_flags_open_idx ON suspicion_flags (score DESC, id) WHERE status = 'open';

CREATE TABLE analysis_runs (
    id          bigserial PRIMARY KEY,
    kind        text NOT NULL,
    params      jsonb NOT NULL,
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    summary     jsonb
);
