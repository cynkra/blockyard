# Process-backend variant image. Ships the blockyard binary built
# with `-tags 'minimal,process_backend'` — no Docker SDK in the
# dependency graph, no socket expectation — plus R, bubblewrap, and
# the compiled BPF seccomp profile at /etc/blockyard/seccomp.bpf.
#
# Based on rocker/r-ver so the R toolchain and library paths match
# what the process backend's preflight and worker spawn expect. See
# docs/design/v3/phase-3-8.md (step 4) for the base-image rationale.

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

# seccomp-compiler: CGO stage that compiles the bwrap-inner seccomp
# JSON profile to BPF via libseccomp-golang. The resulting blob is
# loaded by bwrap at worker spawn time via --seccomp <fd>. Only the
# output is copied forward; the CGO binary itself is discarded.
FROM golang:1.26.0-alpine AS seccomp-compiler
RUN apk add --no-cache build-base libseccomp-dev
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/seccomp-compile/ cmd/seccomp-compile/
COPY internal/seccomp/blockyard-bwrap.json /tmp/bwrap-seccomp.json
RUN CGO_ENABLED=1 go build -o /seccomp-compile ./cmd/seccomp-compile && \
    /seccomp-compile -in /tmp/bwrap-seccomp.json -out /blockyard-bwrap-seccomp.bpf

FROM golang:1.26.0-alpine AS builder

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
# Process-only variant: minimal + process_backend excludes the
# Docker SDK from the dep graph entirely. Verified by
# internal/build/deps_test.go.
RUN CGO_ENABLED=0 go build ${COVER:+-cover} \
    -tags "minimal,process_backend" \
    -ldflags "-X main.version=${VERSION}" \
    -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

# Final stage: rocker/r-ver is the R-on-Debian base used by phase
# 3-7's CI matrix and the default blockyard.toml worker image. The
# GHCR mirror avoids Docker Hub anonymous-pull rate limits.
FROM ghcr.io/rocker-org/r-ver:4.4.3

# bubblewrap for the sandbox, ca-certificates for TLS, curl for
# bootstrap and health probes. No iptables — the process-backend
# variant relies on the operator's host iptables rules (or the
# outer container's) for worker egress isolation; shipping the
# tool would suggest blockyard itself installs the rules.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bubblewrap \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /blockyard /usr/local/bin/blockyard
COPY --from=builder /by-builder /usr/local/lib/blockyard/by-builder
COPY blockyard.toml /etc/blockyard/blockyard.toml
COPY --from=seccomp-compiler /blockyard-bwrap-seccomp.bpf /etc/blockyard/seccomp.bpf
# Operators need the outer-container seccomp profile on disk before
# the container starts (`--security-opt seccomp=...` reads from the
# host). Ship it inside the image too so `docker run --rm
# --entrypoint cat IMAGE /etc/blockyard/seccomp.json` can extract
# a copy.
COPY internal/seccomp/blockyard-outer.json /etc/blockyard/seccomp.json

# Default the process backend's bwrap seccomp profile path to the
# shipped blob so operators don't need to set process.seccomp_profile
# in TOML.
ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf

EXPOSE 8080

ENTRYPOINT ["blockyard", "--config", "/etc/blockyard/blockyard.toml"]
