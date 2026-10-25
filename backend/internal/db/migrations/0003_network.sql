-- 0003: şebeke topolojisi (trafo → besleyici → direk → abone) ve aboneler
--
-- Ağaç bir komşuluk listesi (parent_id) olarak tutulur ve recursive CTE ile
-- gezilir. Kural ve döngü kontrolü tetikleyicidedir; böylece API dışından
-- (psql, seed) yapılan yazımlar da korunur.

CREATE TABLE network_nodes (
    id            bigserial PRIMARY KEY,
    code          text    NOT NULL UNIQUE,
    kind          text    NOT NULL CHECK (kind IN ('transformer','feeder','pole')),
    name          text    NOT NULL,
    parent_id     bigint  REFERENCES network_nodes(id),
    -- Yedek besleme: anahtarlama yapıldığında parent_id bu değere çevrilir.
    alt_parent_id bigint  REFERENCES network_nodes(id),
    normal_parent_id bigint REFERENCES network_nodes(id),
    lat           double precision,  -- koordinat; para değil, float uygundur
    lon           double precision,
    capacity_kva  int CHECK (capacity_kva > 0),
    -- Trafo için teknik kayıp toleransı (%)
    tech_loss_pct numeric(5,2) NOT NULL DEFAULT 0 CHECK (tech_loss_pct BETWEEN 0 AND 50),
    in_service    boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT nn_root_is_transformer CHECK ((kind = 'transformer') = (parent_id IS NULL)),
    CONSTRAINT nn_not_self CHECK (parent_id IS DISTINCT FROM id AND alt_parent_id IS DISTINCT FROM id),
    CONSTRAINT nn_id_kind_uq UNIQUE (id, kind)
);
CREATE INDEX network_nodes_parent_idx ON network_nodes (parent_id);

-- Ebeveyn türü kuralı + döngü engeli.
CREATE FUNCTION network_nodes_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    pkind text;
    cyc   boolean;
BEGIN
    IF NEW.parent_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT kind INTO pkind FROM network_nodes WHERE id = NEW.parent_id;
    IF NEW.kind = 'feeder' AND pkind IS DISTINCT FROM 'transformer' THEN
        RAISE EXCEPTION 'feeder parent must be a transformer' USING ERRCODE = 'check_violation';
    ELSIF NEW.kind = 'pole' AND pkind NOT IN ('feeder','pole') THEN
        RAISE EXCEPTION 'pole parent must be a feeder or pole' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.parent_id IS DISTINCT FROM OLD.parent_id THEN
        -- Eşzamanlı iki taşıma (A→B altına, B→A altına) birbirini göremeyip
        -- döngü oluşturmasın diye topoloji değişiklikleri serileştirilir.
        PERFORM pg_advisory_xact_lock(hashtext('network_topology'));
        -- Yeni ebeveyn, bu düğümün alt ağacında mı? Öyleyse döngü oluşur.
        WITH RECURSIVE up(id, parent_id, depth) AS (
            SELECT id, parent_id, 1 FROM network_nodes WHERE id = NEW.parent_id
            UNION ALL
            SELECT n.id, n.parent_id, up.depth + 1
            FROM network_nodes n JOIN up ON n.id = up.parent_id
            WHERE up.depth < 1000
        )
        SELECT EXISTS (SELECT 1 FROM up WHERE id = NEW.id) INTO cyc;
        IF cyc THEN
            RAISE EXCEPTION 'topology cycle: node % cannot be moved under %', NEW.id, NEW.parent_id
                USING ERRCODE = 'check_violation', HINT = 'cycle';
        END IF;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER network_nodes_check_trg BEFORE INSERT OR UPDATE OF parent_id, kind ON network_nodes
    FOR EACH ROW EXECUTE FUNCTION network_nodes_check();

CREATE TABLE subscribers (
    id              bigserial PRIMARY KEY,
    subscriber_no   text    NOT NULL UNIQUE,
    full_name       text    NOT NULL,
    address         text    NOT NULL,
    category        text    NOT NULL CHECK (category IN ('residential','commercial','industrial','agricultural')),
    pole_id         bigint  NOT NULL,
    pole_kind       text    NOT NULL DEFAULT 'pole' CHECK (pole_kind = 'pole'),
    tariff_id       bigint,          -- FK 0005'te
    contract_kw     numeric(8,2) NOT NULL DEFAULT 10 CHECK (contract_kw > 0),
    lat             double precision,
    lon             double precision,
    active          boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Abone yalnızca bir DİREĞE bağlanabilir (bileşik FK ile bildirimsel kural).
    FOREIGN KEY (pole_id, pole_kind) REFERENCES network_nodes (id, kind)
);
CREATE INDEX subscribers_pole_idx ON subscribers (pole_id);

ALTER TABLE users ADD CONSTRAINT users_subscriber_fk FOREIGN KEY (subscriber_id) REFERENCES subscribers(id);
