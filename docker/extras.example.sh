#!/bin/sh
# Example extras hook for blockyard. Bind-mount this file at
# /etc/blockyard/extras.sh (read-only) to install additional
# runtime libraries that R packages in your bundles need, or to
# pin a specific R version via rig.
#
#   docker run \
#       -v $(pwd)/extras.sh:/etc/blockyard/extras.sh:ro \
#       ghcr.io/cynkra/blockyard-process:<version>
#
# Compose and Kubernetes equivalents are in the containerized
# process-backend guide.
#
# Runs as root before blockyard starts. Failures fail-fast and
# abort startup — don't try to recover from apt-get failures
# silently.
#
# Package names below target ubuntu:24.04 (noble). When the
# image's base Ubuntu bumps, names with the "t64" suffix will
# change and this script will need updating.

set -e

# ─── Optional: pin a specific R version via rig ─────────────────
#
# The image ships with the R release current at build time. To
# pin a specific version instead, uncomment and edit:
#
#   rig add 4.4.3
#   rig default 4.4.3
#
# To install a second R version alongside the default (e.g. for
# side-by-side testing):
#
#   rig add 4.5.0
#
# R versions persist in /opt/R. Mount a volume there to keep them
# across container rebuilds.

# ─── Additional system libraries ────────────────────────────────

apt-get update

# Spatial (sf, terra, leaflet extensions)
apt-get install -y --no-install-recommends \
    libgdal34t64 \
    libgeos-c1t64 \
    libproj25 \
    libudunits2-0

# Imaging / PDF (magick, pdftools)
apt-get install -y --no-install-recommends \
    libpoppler-cpp0v5

# Optimization (nloptr, Rglpk)
apt-get install -y --no-install-recommends \
    libnlopt0 \
    libglpk40

rm -rf /var/lib/apt/lists/*
