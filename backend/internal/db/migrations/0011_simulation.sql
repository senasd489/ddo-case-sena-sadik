-- 0011: simülatör desteği
-- Sayaçtan "enerji geri geldi" (power restore) bildirimi.
ALTER TABLE outage_signals DROP CONSTRAINT outage_signals_source_check;
ALTER TABLE outage_signals ADD CONSTRAINT outage_signals_source_check CHECK (source IN ('call','last_gasp','restore'));

-- Simülatörün bilerek ürettiği tüketim profilleri (yer gerçeği). Uygulama
-- mantığı bu tabloyu OKUMAZ; yalnızca simülatör ve `make verify` tespit
-- kalitesini ölçmek için kullanır.
CREATE TABLE sim_profiles (
    subscriber_id bigint PRIMARY KEY REFERENCES subscribers(id),
    profile       text NOT NULL CHECK (profile IN ('thief','night','outage_bait','missing_bait','noisy')),
    since         date NOT NULL
);
