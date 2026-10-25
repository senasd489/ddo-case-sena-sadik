# EnerjiHat — kısayol komutları
COMPOSE ?= docker compose

.PHONY: up down logs ps test cover lint verify load chaos reset-demo fmt bench-t1 bench-t4 explain

up:            ## Tüm sistemi derleyip ayağa kaldırır
	@test -f .env || cp .env.example .env
	$(COMPOSE) up --build -d
	./scripts/wait-healthy.sh

down:          ## Sistemi durdurur (veriler kalır)
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

ps:
	$(COMPOSE) ps

test:          ## Go birim + entegrasyon testleri (TEST_DATABASE_URL varsa DB testleri de çalışır)
	cd backend && go test -mod=vendor -race -count=1 ./...

cover:         ## Kapsama raporu
	cd backend && go test -mod=vendor -count=1 -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

lint:
	cd backend && go vet -mod=vendor ./... && test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	cd frontend && npm run lint

fmt:
	cd backend && gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

verify:        ## Tüm özellikleri ve zorlu görevleri uçtan uca dener, PASS/FAIL tablosu basar
	@test -f .env || cp .env.example .env
	$(COMPOSE) up --build -d
	./scripts/wait-healthy.sh 300
	$(COMPOSE) --profile verify run --rm --build verify

reset-demo:    ## Verileri ilk seed haline döndürür (şema silinip yeniden kurulur)
	$(COMPOSE) run --rm migrate reset
	$(COMPOSE) restart api simulator

bench-t1:      ## T-1 kanıtı: ≈10 milyon okumayı ayrı bir bench veritabanına yazar ve ölçer
	$(COMPOSE) run --rm migrate bench-t1 $(METERS)

bench-t4:      ## T-4 kanıtı: yerleştirilmiş kaçaklarla precision/recall, filtreli vs filtresiz
	$(COMPOSE) run --rm migrate bench-t4 $(SUBS)

load:          ## k6 yük testi (60 sn karışık trafik) — sonuç tablosu README'de
	@set -a; . ./.env; set +a; docker run --rm -i --network enerjihat_default -e API_URL=http://api:8080 -e INGEST_TOKEN=$$INGEST_TOKEN \
		-e DURATION=$${DURATION:-60s} grafana/k6:0.54.0 run --summary-trend-stats "avg,med,p(95),p(99),max" - < scripts/load/k6.js

chaos:         ## Kaos: veritabanını ve API'yi durdurup sistemin kendiliğinden toparlandığını gösterir
	./scripts/chaos.sh

explain:       ## README'deki dizin ölçümleri (dizinli / dizinsiz EXPLAIN ANALYZE)
	$(COMPOSE) exec -T db psql -U $${POSTGRES_USER:-enerjihat} -d $${POSTGRES_DB:-enerjihat} < scripts/explain.sql
