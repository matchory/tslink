# Build stage
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tslink ./cmd/tslink

# Tailscale binaries, pinned here so every node runs the same version
FROM tailscale/tailscale:v1.102.5@sha256:c507f3a2a6ab1cabd8d809b98edeb41edbd5c3fb6ad9632ffd098b4c7d0b4065 AS tailscale

# Final stage
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk add --no-cache \
    iptables \
    ip6tables \
    iptables-legacy \
    iproute2 \
    util-linux \
    ca-certificates

# Use iptables-nft to match modern Docker (which uses iptables-nft on Ubuntu 24.04+).
# This ensures our FORWARD/MASQUERADE rules go into the same tables Docker uses.
# Tailscale bundles its own iptables runner that handles its own rules.
RUN ln -sf /sbin/iptables-nft /sbin/iptables && \
    ln -sf /sbin/ip6tables-nft /sbin/ip6tables

# Create plugin directories
RUN mkdir -p /run/docker/plugins /data /data/tailscale-bin

COPY --from=builder /tslink /tslink
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscaled /usr/local/bin/

ENTRYPOINT ["/tslink"]
