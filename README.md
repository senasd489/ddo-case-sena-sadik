# EnerjiHat — Akıllı Sayaç, Faturalama ve Kesinti Yönetimi

**Geliştiren:** Sena Sadık ([@senasd489](https://github.com/senasd489)) · DDO Yazılım Ekibi alım projesi

> Bu README proje ilerledikçe tamamlanıyor.

## Hızlı başlangıç

```bash
git clone https://github.com/senasd489/ddo-case-sena-sadik.git && cd ddo-case-sena-sadik
cp .env.example .env
docker compose up --build
```

- Frontend: http://localhost:3000
- API: http://localhost:8080 (`GET /health`, `GET /metrics`)

## Yığın

| Katman | Seçim |
|---|---|
| Backend | Go 1.25, `chi` router, `pgx/v5`, düz SQL |
| Veritabanı | PostgreSQL 16 |
| Frontend | Vue 3 + Vite + TypeScript, Pinia, Vue Router |
| Konteyner | Docker Compose; distroless (Go) ve nginx-unprivileged (frontend) imajları |
