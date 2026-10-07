# Build stage
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /tslink ./cmd/tslink

SHELL ["/bin/ash", "-o", "pipefail", "-c"]

# License notices of everything linked into the binary: Go itself and every
# module it imports
RUN set -e; \
    mkdir -p /licenses/tslink/go; \
    cp LICENSE /licenses/tslink/; \
    cp "$(go env GOROOT)/LICENSE" /licenses/tslink/go/; \
    self="$(go list -m)"; \
    GOOS=linux go list -deps -f '{{with .Module}}{{.Path}} {{.Dir}}{{end}}' ./cmd/tslink \
      | sort -u | while read -r mod dir; do \
        [ "$mod" = "$self" ] && continue; \
        mkdir -p "/licenses/tslink/$mod"; \
        find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' \) \
          -exec cp {} "/licenses/tslink/$mod/" \; ; \
        [ -n "$(ls "/licenses/tslink/$mod")" ] || { echo "no license file in $mod" >&2; exit 1; }; \
      done

# Tailscale binaries, pinned here so every node runs the same version
FROM tailscale/tailscale:v1.102.5@sha256:c507f3a2a6ab1cabd8d809b98edeb41edbd5c3fb6ad9632ffd098b4c7d0b4065 AS tailscale

# Tailscale's license, taken from the source of the version bundled above
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS tailscale-licenses
SHELL ["/bin/ash", "-o", "pipefail", "-c"]
COPY --from=tailscale /usr/local/bin/tailscale /tmp/tailscale
RUN set -e; \
    version="$(/tmp/tailscale version | head -n 1)"; \
    dir="$(go mod download -json "tailscale.com@v$version" | sed -n 's/^\t"Dir": "\(.*\)",$/\1/p')"; \
    mkdir -p /licenses/tailscale; \
    cp "$dir/LICENSE" /licenses/tailscale/; \
    cp "$dir/licenses/tailscale.md" /licenses/tailscale/THIRD-PARTY.md

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
RUN mkdir -p /run/docker/plugins /data

COPY --from=builder /tslink /tslink
COPY --from=tailscale /usr/local/bin/tailscale /usr/local/bin/tailscaled /usr/local/bin/
COPY --from=builder /licenses/tslink /usr/share/licenses/tslink
COPY --from=tailscale-licenses /licenses/tailscale /usr/share/licenses/tailscale

ENTRYPOINT ["/tslink"]
