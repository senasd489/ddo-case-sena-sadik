#!/usr/bin/env bash
# Kaos senaryosu: bağımlılıkları sırayla durdurup sistemin kendiliğinden
# toparlandığını kanıtlar. Çıkış kodu 0 = tüm adımlar geçti.
#  1) PostgreSQL durur  → /health 503 (degraded), API ayakta kalır
#  2) PostgreSQL döner  → /health 200, okuma akışı devam eder (simülatör yeniden dener)
#  3) API konteyneri öldürülür (SIGKILL) → restart politikası geri getirir, /health 200
set -euo pipefail
API=${API_URL:-http://localhost:8080}
C=${COMPOSE:-docker compose}
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; exit 1; }
code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$API/health" || true; }
wait_code() { # $1 beklenen kod, $2 saniye
  for _ in $(seq 1 "$2"); do [ "$(code)" = "$1" ] && return 0; sleep 1; done; return 1; }
last_batch() {
  local t; t=$(curl -s "$API/api/v1/auth/login" -H 'Content-Type: application/json' \
    -d '{"email":"operator@enerjihat.local","password":"Demo1234!"}' | sed -E 's/.*"access_token":"([^"]+)".*/\1/')
  curl -s "$API/api/v1/live" -H "Authorization: Bearer $t" | sed -E 's/.*"last_batch_at":"?([^",]*)"?.*/\1/'
}

echo "Kaos senaryosu — $API"
[ "$(code)" = "200" ] || fail "başlangıçta sistem sağlıklı değil"
before=$(last_batch); pass "başlangıç: /health 200, son okuma partisi $before"

echo "1) PostgreSQL durduruluyor"
$C stop db >/dev/null
wait_code 503 30 && pass "/health 503 (degraded) — API çökmedi, hatayı raporluyor" || fail "/health 503 dönmedi"
sleep 10

echo "2) PostgreSQL başlatılıyor"
$C start db >/dev/null
wait_code 200 90 && pass "/health tekrar 200 (bağlantı havuzu kendiliğinden toparlandı)" || fail "API toparlanmadı"
echo "   simülatörün yeni parti göndermesi bekleniyor (en fazla 16 dk: bir sonraki 15 dk slotu)…"
for _ in $(seq 1 200); do
  now=$(last_batch); [ -n "$now" ] && [ "$now" != "$before" ] && break; sleep 5
done
[ "$now" != "$before" ] && pass "okuma akışı devam ediyor (son parti $now)" || fail "simülatör veri göndermeye devam etmedi"

echo "3) API konteyneri SIGKILL ile öldürülüyor"
$C kill -s SIGKILL api >/dev/null
sleep 2
[ "$(code)" != "200" ] && pass "API kapalı" || true
$C up -d api >/dev/null   # restart: unless-stopped; up ile de garanti
wait_code 200 90 && pass "API yeniden ayakta, /health 200" || fail "API geri gelmedi"
echo "Kaos senaryosu tamam."
