-- 0009: eşzamanlı partiler aynı partisyonu aynı anda oluşturmaya çalışınca
-- "already exists" hatası alıyordu (bench-t1 ile bulundu). Fonksiyon artık bir
-- advisory lock ile serileşir ve IF NOT EXISTS kullanır.
CREATE OR REPLACE FUNCTION ensure_reading_partitions(p_from date, p_to date) RETURNS int LANGUAGE plpgsql AS $$
DECLARE
    d date := p_from;
    n int := 0;
    tbl text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtext('ensure_reading_partitions'));
    WHILE d <= p_to LOOP
        tbl := format('readings_%s', to_char(d, 'YYYYMMDD'));
        IF to_regclass(tbl) IS NULL THEN
            EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF readings FOR VALUES FROM (%L) TO (%L)',
                           tbl, d::text || ' 00:00:00+00', (d + 1)::text || ' 00:00:00+00');
            n := n + 1;
        END IF;
        tbl := format('consumption_15m_%s', to_char(d, 'YYYYMMDD'));
        IF to_regclass(tbl) IS NULL THEN
            EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF consumption_15m FOR VALUES FROM (%L) TO (%L)',
                           tbl, d::text || ' 00:00:00+00', (d + 1)::text || ' 00:00:00+00');
        END IF;
        d := d + 1;
    END LOOP;
    RETURN n;
END $$;
