# Everything variant image. Ships the blockyard binary built with
# both backends compiled in (default `go build` — no `minimal` tag)
# plus R (via rig), bubblewrap, iptables, and the compiled BPF
# seccomp profile. This is the default `ghcr.io/cynkra/blockyard:<v>`
# image under phase 3-8; operators who want slim Docker-only pull
# `-docker` instead.
#
# Base: ubuntu:24.04 + rig (issue #185). Shares the rationale and
# runtime-lib list with server-process.Dockerfile; the only
# additions here are iptables (for the Docker backend's native
# egress firewall path) and the broader set of build tags on the
# Go compile step above. R_VERSION build ARG controls which R
# version is baked in (default: "release").

FROM hugomods/hugo:exts-0.147.4 AS docs
WORKDIR /docs
COPY docs/ .
RUN hugo --minify --baseURL /docs/ --enableGitInfo=false

FROM node:22-alpine AS css-builder
WORKDIR /src/internal/ui
COPY internal/ui/package.json internal/ui/package-lock.json ./
RUN npm ci
COPY internal/ui/input.css ./
COPY internal/ui/templates/ templates/
RUN npm run css:build

FROM golang:1.25.9-alpine AS seccomp-compiler
RUN apk add --no-cache build-base libseccomp-dev
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/seccomp-compile/ cmd/seccomp-compile/
COPY internal/seccomp/blockyard-bwrap.json /tmp/bwrap-seccomp.json
RUN CGO_ENABLED=1 go build -o /seccomp-compile ./cmd/seccomp-compile && \
    /seccomp-compile -in /tmp/bwrap-seccomp.json -out /blockyard-bwrap-seccomp.bpf

FROM golang:1.25.9-alpine AS builder

ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=docs /docs/public internal/docs/dist
COPY --from=css-builder /src/internal/ui/static/style.css internal/ui/static/style.css

ARG COVER=""
ARG VERSION=dev
# Default go build with no tags includes both backends. A runtime
# switch (cfg.Server.Backend) picks which one is instantiated.
RUN CGO_ENABLED=0 go build ${COVER:+-cover} \
    -ldflags "-X main.version=${VERSION}" \
    -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

# Final stage: ubuntu:24.04 + rig + R release. See the header
# comment and issue #185 for the rationale. iptables is the only
# addition over the process-variant image — the Docker backend
# uses it for worker egress in native mode.
FROM ubuntu:24.04

ARG RIG_VERSION=0.7.1
ARG R_VERSION=release
ARG TARGETARCH=amd64

RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        bubblewrap \
        ca-certificates \
        curl \
        iptables \
        libcairo2 \
        libcurl4t64 \
        libicu74 \
        libmariadb3 \
        liblz4-1 \
        libodbc2 \
        libpango-1.0-0 \
        libpangocairo-1.0-0 \
        libpq5 \
        libsqlite3-0 \
        libssl3t64 \
        libxml2 \
        libzstd1 \
    && case "${TARGETARCH}" in \
        arm64) RIG_ASSET="rig-linux-arm64-${RIG_VERSION}.tar.gz" ;; \
        amd64) RIG_ASSET="rig-linux-${RIG_VERSION}.tar.gz" ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/r-lib/rig/releases/download/v${RIG_VERSION}/${RIG_ASSET}" \
        | tar xz -C /usr/local \
    && rig add "${R_VERSION}" \
    && ln -sf /usr/local/bin/R /usr/bin/R \
    && ln -sf /usr/local/bin/Rscript /usr/bin/Rscript \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml
COPY --from=seccomp-compiler /blockyard-bwrap-seccomp.bpf /etc/blockyard/seccomp.bpf
COPY internal/seccomp/blockyard-outer.json /etc/blockyard/seccomp.json

# Extras hook. See server-process.Dockerfile for the full comment
# and docs/content/docs/guides/process-backend-container.md for
# the operator-facing contract.
COPY docker/extras.sh /etc/blockyard/extras.sh
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/entrypoint.sh", "blockyard", "--config", "/etc/blockyard/blockyard.toml"]
