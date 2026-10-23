FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-X zen-gate/internal/gateway.Version=docker" -o /zen-gate ./cmd/zen-gate

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /zen-gate /usr/local/bin/zen-gate
ENV ZEN_GATE_HEADLESS=1
ENV ZEN_GATE_HOME=/data
VOLUME /data
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD wget -qO- http://127.0.0.1:8787/healthz || exit 1
ENTRYPOINT ["zen-gate"]
