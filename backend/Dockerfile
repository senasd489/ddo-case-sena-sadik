# syntax=docker/dockerfile:1
# Bağımlılıklar vendor/ altında olduğu için derleme ağ erişimi gerektirmez.
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=vendor
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && go build -trimpath -ldflags="-s -w" -o /out/simulator ./cmd/simulator

# distroless/static: kabuk yok, paket yöneticisi yok, kullanıcı 65532 (nonroot)
FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/api /app/api
USER nonroot:nonroot
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --retries=20 CMD ["/app/api", "healthcheck"]
ENTRYPOINT ["/app/api"]

FROM gcr.io/distroless/static-debian12:nonroot AS simulator
COPY --from=build /out/simulator /app/simulator
USER nonroot:nonroot
ENTRYPOINT ["/app/simulator"]
