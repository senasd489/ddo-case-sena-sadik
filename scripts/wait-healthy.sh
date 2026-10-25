#!/usr/bin/env bash
# API ve frontend sağlıklı olana kadar bekler (varsayılan en fazla 300 sn).
set -euo pipefail
API=${API_URL:-http://localhost:8080}
WEB=${WEB_URL:-http://localhost:3000}
TIMEOUT=${1:-300}
start=$(date +%s)
until curl -fsS "$API/health" >/dev/null 2>&1 && curl -fsS "$WEB/" >/dev/null 2>&1; do
  if (( $(date +%s) - start > TIMEOUT )); then
    echo "Sistem $TIMEOUT sn içinde hazır olmadı" >&2; exit 1
  fi
  sleep 2
done
echo "Sistem hazır ($(( $(date +%s) - start )) sn)"
