# Worker image for the Docker backend (issue #191). Replaces the
# upstream rocker/r-ver:<v> default with a hardened ubuntu + rig
# foundation, mirroring server-process.Dockerfile (#185).
#
# Rocker ships a full compiler toolchain (binutils, g++, gfortran)
# and -dev headers at runtime — convenient for interactive research,
# wrong default for a runtime that executes user-supplied R code in
# internet-facing Shiny apps. This image ships only runtime shared
# libraries; operators who need source builds or extra packages
# layer them on via FROM blockyard-worker:<v>.
#
# One image per R version. Built and pushed as
# ghcr.io/cynkra/blockyard-worker:<r-version> by the Publish
# workflow (current + previous 4 minor releases). Operators on
# older or niche R versions can FROM this image and `rig add`,
# or bring their own image via [docker] image.
#
# No blockyard binary, no bwrap/seccomp profile, no entrypoint —
# the Docker backend supplies the full Cmd at spawn time.
FROM ubuntu:24.04

ARG RIG_VERSION=0.7.1
ARG R_VERSION=4.4.3
ARG TARGETARCH

# Runtime libraries only — keep this list aligned with
# server-process.Dockerfile so process-backend and docker-backend
# workers see the same shared libraries. libuv1 is required by the
# `fs` R package (and any package that imports it transitively, i.e.
# every Shiny app via shiny → bslib → sass → fs); see #264.
RUN apt-get update \
    && apt-get upgrade -y \
    && apt-get install -y --no-install-recommends \
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
        libuv1 \
        libxml2 \
        libzstd1 \
    && case "${TARGETARCH}" in \
        arm64) RIG_ASSET="rig-linux-arm64-${RIG_VERSION}.tar.gz" ;; \
        amd64) RIG_ASSET="rig-linux-${RIG_VERSION}.tar.gz" ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL "https://github.com/r-lib/rig/releases/download/v${RIG_VERSION}/${RIG_ASSET}" \
        | tar xz -C /usr/local \
    && rm -rf /var/lib/apt/lists/*

# Single R version baked in. The Docker backend selects the worker
# image per blockyard.toml (or per-app override) — image tag is the
# version pin, not r-versions.sh. Symlink R/Rscript so spawn specs
# can use bare "Rscript" without depending on rig's PATH shims.
RUN rig add "${R_VERSION}" \
    && rig default "${R_VERSION}" \
    && ln -sf /usr/local/bin/R /usr/bin/R \
    && ln -sf /usr/local/bin/Rscript /usr/bin/Rscript
