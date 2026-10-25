-- 0012: tüketim kuyruğu artık "talep et → hesapla → sil" üç kısa işlemle
-- işlenir. Eskiden işçi satırları DELETE … RETURNING ile tek uzun işlemde
-- tutuyordu ve okuma alma aynı (sayaç, gün) anahtarına yazmak için o işlemin
-- bitmesini bekliyordu (yük testinde 20 sn'lik gecikmeler).
ALTER TABLE consumption_dirty ADD COLUMN claimed_at timestamptz;
CREATE INDEX consumption_dirty_pick_idx ON consumption_dirty (day, meter_id) WHERE claimed_at IS NULL;
