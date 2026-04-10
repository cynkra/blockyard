#!/bin/sh
# Default extras hook for blockyard — intentionally a no-op.
#
# Override this file by bind-mounting your own script to
# /etc/blockyard/extras.sh (read-only is fine). The script runs
# as root before the blockyard server starts and can:
#
#   - install additional system libraries via apt-get
#   - add custom apt sources and GPG keys
#   - drop .netrc / credentials files into /root
#
# To change the set of installed R versions, override
# /etc/blockyard/r-versions.sh instead of this file.
#
# Failures propagate via `set -e` in the entrypoint shim, so a
# non-zero exit here aborts container startup with a clear error
# — typos and missing packages surface immediately rather than
# failing at first dyn.load inside a worker session.
#
# See docs/content/docs/guides/process-backend-container.md for
# the full contract, mount patterns (docker/compose/kubernetes),
# and an example script with commented blocks for common R
# ecosystem extras (spatial, imaging, optimization).
exit 0
