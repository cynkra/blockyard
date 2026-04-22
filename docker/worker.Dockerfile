# Worker image for the Docker backend (issue #191). Replaces the
# upstream rocker/r-ver:<v> default with a hardened ubuntu + single-
# version R foundation, mirroring server-process.Dockerfile (#185).
#
# Rocker ships a full compiler toolchain (binutils, g++, gfortran)
# and -dev headers at runtime — convenient for interactive research,
# wrong default for a runtime that executes user-supplied R code in
# internet-facing Shiny apps. This image ships only runtime shared
# libraries; operators who need source builds or extra packages
# layer them on via FROM blockyard-worker:<v>.
#
# One image per R version. Built and pushed as
# ghcr.io/cynkra/blockyard-worker:<r-version> by worker-publish.yml
# (current + previous 4 minor releases). Operators on older or niche
# R versions can extend this image or bring their own via [docker]
# image. No version manager in the image — rig is purpose-built for
# switching between R versions at runtime, which this per-tag layout
# obviates.
#
# No blockyard binary, no bwrap/seccomp profile, no entrypoint —
# the Docker backend supplies the full Cmd at spawn time.
FROM ubuntu:24.04

ARG R_VERSION=4.4.3
ARG TARGETARCH

# Runtime libraries only — keep this list aligned with
# server-process.Dockerfile so process-backend and docker-backend
# workers see the same shared libraries. libuv1 is required by the
# `fs` R package (and any package that imports it transitively, i.e.
# every Shiny app via shiny → bslib → sass → fs); see #264.
#
# R itself comes from r-hub/R's glibc .deb, which installs to
# /opt/R/<version>-glibc and declares only libc6 as a hard Depends
# (runtime lib deps are met by the apt list above). `apt-get install`
# on the local .deb resolves libc6 from the cache and refuses to
# proceed if something is missing.
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
        arm64) R_DEB="r-${R_VERSION}-glibc_1_arm64.deb" ;; \
        amd64) R_DEB="r-${R_VERSION}-glibc_1_amd64.deb" ;; \
        *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && curl -fsSL -o /tmp/r.deb \
        "https://github.com/r-hub/R/releases/download/v${R_VERSION}/${R_DEB}" \
    && apt-get install -y --no-install-recommends /tmp/r.deb \
    && rm -f /tmp/r.deb \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf "/opt/R/${R_VERSION}-glibc/bin/R" /usr/local/bin/R \
    && ln -sf "/opt/R/${R_VERSION}-glibc/bin/Rscript" /usr/local/bin/Rscript
