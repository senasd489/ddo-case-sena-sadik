-- make explain — README "Doğrulama Soruları" S3 ölçümleri.
-- Her sorgu önce dizinle, sonra dizin bir işlem içinde düşürülüp (ROLLBACK ile geri gelir) ölçülür.
\timing off
\echo '=== 1) Abone portalı: 1 günlük 15 dk tüketim — consumption_15m_sub_idx (subscriber_id, slot_start) ==='
\echo '--- dizinli'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON)
SELECT slot_start, active_wh, quality FROM consumption_15m
WHERE subscriber_id = 1 AND slot_start >= now() - interval '2 days' AND slot_start < now() - interval '1 day';
BEGIN;
DROP INDEX consumption_15m_sub_idx;
\echo '--- dizinsiz'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON)
SELECT slot_start, active_wh, quality FROM consumption_15m
WHERE subscriber_id = 1 AND slot_start >= now() - interval '2 days' AND slot_start < now() - interval '1 day';
ROLLBACK;

\echo '=== 2) Fatura girdisi: abonenin 1 aylık günlük tüketimi — consumption_daily_sub_idx (subscriber_id, day) ==='
\echo '--- dizinli'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON)
SELECT day, sum(active_wh) FROM consumption_daily WHERE subscriber_id = 777 AND day >= current_date - 30 AND day < current_date GROUP BY day;
BEGIN;
DROP INDEX consumption_daily_sub_idx;
\echo '--- dizinsiz'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON)
SELECT day, sum(active_wh) FROM consumption_daily WHERE subscriber_id = 777 AND day >= current_date - 30 AND day < current_date GROUP BY day;
ROLLBACK;

\echo '=== 3) Açık kesintiler — outages_open_idx (kısmi dizin) ==='
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, SUMMARY ON)
SELECT id FROM outages WHERE feeder_id = 2 AND status NOT IN ('resolved','cancelled') ORDER BY started_at;
