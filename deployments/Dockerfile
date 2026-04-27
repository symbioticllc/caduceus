# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /dragonboat-server ./cmd/dragonboat-server

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /dragonboat-server /dragonboat-server

# Raft internal communication
EXPOSE 63001
# Gossip (UDP + TCP)
EXPOSE 63002
# gRPC API
EXPOSE 8080
# REST/JSON API
EXPOSE 8081

VOLUME ["/data"]

ENTRYPOINT ["/dragonboat-server"]
