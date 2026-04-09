# Everything variant image. Ships the blockyard binary built with
# both backends compiled in (default `go build` — no `minimal` tag)
# plus R, bubblewrap, iptables, and the compiled BPF seccomp
# profile. This is the default `ghcr.io/cynkra/blockyard:<v>` image
# under phase 3-8; operators who want slim Docker-only pull
# `-docker` instead.

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

# Final stage: rocker/r-ver, same rationale as server-process.
# iptables is added here because the everything variant also
# supports the Docker backend (which uses iptables for worker
# egress in native mode).
FROM ghcr.io/rocker-org/r-ver:4.4.3

# `apt-get upgrade` pulls in Ubuntu security patches that the
# rocker/r-ver base may have missed since its last rebuild.
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        bubblewrap \
        ca-certificates \
        curl \
        iptables \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml
COPY --from=seccomp-compiler /blockyard-bwrap-seccomp.bpf /etc/blockyard/seccomp.bpf
COPY internal/seccomp/blockyard-outer.json /etc/blockyard/seccomp.json

ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf

EXPOSE 8080

ENTRYPOINT ["blockyard", "--config", "/etc/blockyard/blockyard.toml"]
