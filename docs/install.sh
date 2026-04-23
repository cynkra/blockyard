#!/bin/sh
#
# Blockyard installer.
#
# Downloads a release binary (the `by` CLI by default, or the `blockyard`
# server with --server) from github.com/cynkra/blockyard and drops it into
# an install directory on your PATH.
#
# Usage:
#   curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh
#   curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- --channel main
#   curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- --version v0.1.0
#   curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- --install-dir "$HOME/.local/bin"
#   curl -fsSL https://cynkra.github.io/blockyard/install.sh | sh -s -- --server
#
# Flags:
#   --channel <name>      Release channel: `stable` (default) or `main`.
#                         `stable` tracks the latest tagged release; `main` is a
#                         rolling pre-release rebuilt on every push to main.
#                         The main channel ships the `by` CLI only — the server
#                         is distributed as `ghcr.io/cynkra/blockyard:main`.
#   --version <tag>       Pin to a specific release tag (e.g. v0.0.3). Mutually
#                         exclusive with --channel.
#   --install-dir <path>  Directory to install into (default: /usr/local/bin)
#   --server              Install the `blockyard` server instead of the `by` CLI
#                         (Linux-only; server is typically run as a container)
#   --help                Show this message
#
# Environment overrides (seed defaults; flags take precedence):
#   BLOCKYARD_CHANNEL       Same as --channel
#   BLOCKYARD_VERSION       Same as --version
#   BLOCKYARD_INSTALL_DIR   Same as --install-dir
#   BLOCKYARD_BINARY        Binary to install: `by` or `blockyard` (default: by)

set -eu

REPO="cynkra/blockyard"
BASE_URL="${BLOCKYARD_BASE_URL:-https://github.com/${REPO}/releases}"
BINARY="${BLOCKYARD_BINARY:-by}"
CHANNEL="${BLOCKYARD_CHANNEL:-}"
VERSION="${BLOCKYARD_VERSION:-}"
INSTALL_DIR="${BLOCKYARD_INSTALL_DIR:-/usr/local/bin}"

info() { printf '%s\n' "==> $*" >&2; }
warn() { printf '%s\n' "warning: $*" >&2; }
die()  { printf '%s\n' "error: $*" >&2; exit 1; }

usage() {
  # Print the hard-coded usage/help text.
  cat <<'EOF'
Blockyard installer.

Usage:
  install.sh [--channel <name>] [--version <tag>] [--install-dir <path>] [--server]

Flags:
  --channel <name>      Release channel: stable (default) or main
  --version <tag>       Pin to a specific tag (mutually exclusive with --channel)
  --install-dir <path>  Directory to install into (default: /usr/local/bin)
  --server              Install the `blockyard` server instead of `by`
  --help                Show this message

Environment overrides:
  BLOCKYARD_CHANNEL, BLOCKYARD_VERSION, BLOCKYARD_INSTALL_DIR, BLOCKYARD_BINARY
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --channel)        [ $# -ge 2 ] || die "--channel requires a value"; CHANNEL="$2"; shift 2 ;;
    --channel=*)      CHANNEL="${1#*=}"; shift ;;
    --version)        [ $# -ge 2 ] || die "--version requires a value"; VERSION="$2"; shift 2 ;;
    --version=*)      VERSION="${1#*=}"; shift ;;
    --install-dir)    [ $# -ge 2 ] || die "--install-dir requires a value"; INSTALL_DIR="$2"; shift 2 ;;
    --install-dir=*)  INSTALL_DIR="${1#*=}"; shift ;;
    --server)         BINARY="blockyard"; shift ;;
    -h|--help)        usage; exit 0 ;;
    *)                die "unknown argument: $1 (try --help)" ;;
  esac
done

case "$BINARY" in
  by|blockyard) ;;
  *) die "unsupported binary: $BINARY (expected: by or blockyard)" ;;
esac

# --channel and --version pick the same release in two different ways.
# Accepting both invites ambiguity, so require exactly one (with stable
# as the default when neither is set).
if [ -n "$CHANNEL" ] && [ -n "$VERSION" ]; then
  die "--channel and --version are mutually exclusive"
fi
if [ -z "$CHANNEL" ] && [ -z "$VERSION" ]; then
  CHANNEL=stable
fi
case "$CHANNEL" in
  "")      ;;  # --version path — VERSION is already set
  stable)  VERSION=latest ;;
  main)    VERSION=main ;;
  *)       die "unknown channel: $CHANNEL (expected: stable or main)" ;;
esac

# The main channel (publish.yml) only uploads `by` binaries; the
# rolling server is shipped as the ghcr.io/cynkra/blockyard:main image.
if [ "$VERSION" = "main" ] && [ "$BINARY" = "blockyard" ]; then
  die "--channel main does not ship a blockyard server binary; pull ghcr.io/cynkra/blockyard:main instead"
fi

detect_os() {
  os=$(uname -s 2>/dev/null || echo unknown)
  case "$os" in
    Linux)   echo linux ;;
    Darwin)  echo darwin ;;
    MINGW*|MSYS*|CYGWIN*)
      die "Windows is not supported by this script. Download by-windows-amd64.exe from ${BASE_URL}/latest/download/by-windows-amd64.exe."
      ;;
    *) die "unsupported operating system: $os" ;;
  esac
}

detect_arch() {
  m=$(uname -m 2>/dev/null || echo unknown)
  case "$m" in
    x86_64|amd64)   echo amd64 ;;
    arm64|aarch64)  echo arm64 ;;
    *) die "unsupported architecture: $m" ;;
  esac
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

require_cmd uname
require_cmd chmod
require_cmd mktemp

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$1" "$2"; }
else
  die "neither curl nor wget is installed"
fi

os=$(detect_os)
arch=$(detect_arch)

# Server is built for Linux only (see .github/workflows/release.yml).
if [ "$BINARY" = "blockyard" ] && [ "$os" != "linux" ]; then
  die "the blockyard server is only published for Linux; use a container image on $os"
fi

asset="${BINARY}-${os}-${arch}"
if [ "$VERSION" = "latest" ]; then
  url="${BASE_URL}/latest/download/${asset}"
else
  url="${BASE_URL}/download/${VERSION}/${asset}"
fi

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t blockyard-install)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

info "Downloading ${asset} (${VERSION})"
fetch "$tmpdir/$BINARY" "$url" || die "download failed: $url"
chmod +x "$tmpdir/$BINARY"

target="$INSTALL_DIR/$BINARY"
if [ ! -d "$INSTALL_DIR" ]; then
  info "Creating ${INSTALL_DIR}"
  mkdir -p "$INSTALL_DIR" 2>/dev/null || {
    command -v sudo >/dev/null 2>&1 || die "${INSTALL_DIR} does not exist and cannot be created"
    sudo mkdir -p "$INSTALL_DIR"
  }
fi

if [ -w "$INSTALL_DIR" ]; then
  mv "$tmpdir/$BINARY" "$target"
elif command -v sudo >/dev/null 2>&1; then
  info "Installing to ${target} (requires sudo)"
  sudo mv "$tmpdir/$BINARY" "$target"
else
  die "${INSTALL_DIR} is not writable and sudo is not available; re-run with --install-dir <path>"
fi

info "Installed ${BINARY} to ${target}"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) warn "${INSTALL_DIR} is not on your PATH — add it to your shell profile to run '${BINARY}' directly" ;;
esac

"$target" --version 2>/dev/null || true
