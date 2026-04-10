#!/bin/sh
# Entrypoint shim for the blockyard server-process and
# server-everything images. Runs the R version policy script
# and the operator's extras hook, then execs the server.
#
# Hook order:
#   1. r-versions.sh — R version installation and default
#      (baked-in policy; override by bind-mounting)
#   2. extras.sh     — system libraries, credentials, etc.
#      (no-op by default; override by bind-mounting)
#
# `set -e` propagates hook failures as container startup errors
# — a typo in an apt package name or a missing rig version aborts
# the start cleanly instead of silently producing a running server
# that fails at first dyn.load or first bwrap spawn.
#
# `docker run --entrypoint cat IMAGE /path` still replaces the
# whole entrypoint chain (including this script), so the seccomp
# profile extract flow documented in the containerized guide
# continues to work unchanged.
set -eu

/etc/blockyard/r-versions.sh
/etc/blockyard/extras.sh

exec "$@"
