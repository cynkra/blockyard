#!/bin/sh
# Entrypoint shim for the blockyard server-process and
# server-everything images. Runs the operator's extras hook
# (default /etc/blockyard/extras.sh is a no-op baked into the
# image; override by bind-mounting) and then execs the command
# passed in by the Dockerfile's ENTRYPOINT array.
#
# `set -e` propagates extras failures as container startup errors
# — a typo in an apt package name or a missing rig version aborts
# the start cleanly instead of silently producing a running server
# that fails at first dyn.load or first bwrap spawn.
#
# `docker run --entrypoint cat IMAGE /path` still replaces the
# whole entrypoint chain (including this script), so the seccomp
# profile extract flow documented in the containerized guide
# continues to work unchanged.
set -eu

/etc/blockyard/extras.sh

exec "$@"
