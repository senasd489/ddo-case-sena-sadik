# EnerjiHat — kısayol komutları
COMPOSE ?= docker compose

.PHONY: up down logs ps test cover lint verify load chaos reset-demo fmt

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
