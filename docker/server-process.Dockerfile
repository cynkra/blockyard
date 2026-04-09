# Process-backend variant image. Ships the blockyard binary built
# with `-tags 'minimal,process_backend'` — no Docker SDK in the
# dependency graph, no socket expectation — plus R (via rig),
# bubblewrap, and the compiled BPF seccomp profile at
# /etc/blockyard/seccomp.bpf.
#
# Base: ubuntu:24.04 + rig + r-release (issue #185). Rocker's full
# R toolchain ships binutils, g++, gfortran, and -dev headers that
# bloat the attack surface for a runtime that executes untrusted R
# code. This image ships only runtime shared libraries; operators
# who need source builds or extra packages install them via the
# extras.sh hook (see the bottom of this file). R itself is managed
# by rig (r-lib/rig), so operators can swap versions without
# rebuilding the image — `rig add 4.5` in an extras.sh override.

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
# Process-only variant: minimal + process_backend excludes the
# Docker SDK from the dep graph entirely. Verified by
# internal/build/deps_test.go.
RUN CGO_ENABLED=0 go build ${COVER:+-cover} \
    -tags "minimal,process_backend" \
    -ldflags "-X main.version=${VERSION}" \
    -o /blockyard ./cmd/blockyard
RUN CGO_ENABLED=0 go build ${COVER:+-cover} -o /by-builder ./cmd/by-builder

# Final stage: ubuntu:24.04 + rig + R release. See the header
# comment for the rationale and issue #185 for the full discussion.
FROM ubuntu:24.04

# rig version pin. rig is the R installation manager from r-lib;
# it downloads official R binaries and manages multiple installed
# R versions via shims under /usr/local/bin. Operators can swap R
# versions at runtime via the extras.sh hook without rebuilding
# this image.
ARG RIG_VERSION=0.7.1
# Docker buildx sets TARGETARCH automatically for multi-platform
# builds. Default to amd64 for local single-arch `docker build`
# invocations so rig downloads the correct tarball.
ARG TARGETARCH=amd64

# Runtime libraries only — no -dev headers, no compiler toolchain.
# r-base-core-style runtime deps + common DB connectors + xml + ssl
# + compression. Packages with compiled code requiring libraries
# not listed here are installed via the extras.sh hook.
#
# `apt-get upgrade` picks up Ubuntu security patches landed since
# the base image's last rebuild.
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
        bubblewrap \
        ca-certificates \
        curl \
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
    && rig add release \
    && ln -sf /usr/local/bin/R /usr/bin/R \
    && ln -sf /usr/local/bin/Rscript /usr/bin/Rscript \
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

# Extras hook. The default is a no-op; operators override by
# bind-mounting their own script to /etc/blockyard/extras.sh to
# install additional system libraries, pin a specific R version
# via rig, or drop credential files. Runs as root before the
# blockyard server starts; failures propagate (set -e in the
# entrypoint) and abort startup with a clear error.
#
# See docs/content/docs/guides/process-backend-container.md for
# the full contract and mount patterns.
COPY docker/extras.sh /etc/blockyard/extras.sh
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh

# Default the process backend's bwrap seccomp profile path to the
# shipped blob so operators don't need to set process.seccomp_profile
# in TOML.
ENV BLOCKYARD_PROCESS_SECCOMP_PROFILE=/etc/blockyard/seccomp.bpf

EXPOSE 8080

# ENTRYPOINT carries the full command; no CMD. docker run args are
# appended to the entrypoint, so `docker run image --log-level debug`
# still works. `docker run --entrypoint cat image /path` still
# replaces the entire entrypoint chain for the seccomp extract flow.
ENTRYPOINT ["/usr/local/bin/entrypoint.sh", "blockyard", "--config", "/etc/blockyard/blockyard.toml"]
