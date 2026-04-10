#!/bin/sh
# Default R version policy for the blockyard process backend.
#
# Installs the latest patch of the current and previous 4 minor
# releases via rig. The default is set to the previous minor (not
# the bleeding-edge release) for stability — most bundles target
# the prior release.
#
# This script runs at both image build time and container start.
# rig skips versions that are already installed, so the startup
# run is effectively a no-op for the default image. Operators
# override the entire policy by bind-mounting their own script to
# /etc/blockyard/r-versions.sh.
#
# Example operator override (pin to a single version):
#
#   #!/bin/sh
#   rig add 4.4.3
#   rig default 4.4.3
#
set -eu

# Install the current release.
rig add release

# Derive the current minor version from what rig installed.
LATEST=$(ls /opt/R | sort -V | tail -1)
MAJOR=${LATEST%%.*}
MINOR=${LATEST#*.}; MINOR=${MINOR%%.*}

# Install previous 4 minor releases.
for i in 1 2 3 4; do
  m=$((MINOR - i))
  [ "$m" -ge 0 ] && rig add "${MAJOR}.${m}" || true
done

# Default to the previous minor for stability.
PREV=$(ls /opt/R | sort -V | tail -2 | head -1)
if [ -n "$PREV" ]; then
  rig default "$PREV"
fi
