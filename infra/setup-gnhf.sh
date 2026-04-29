#!/usr/bin/env bash
# setup-gnhf.sh — Idempotent installer for the gnhf CLI on the claude-vm host.
#
# gnhf (https://www.npmjs.com/package/gnhf) is the autonomous-implementer
# loop spawned by the supervisor for inbox rows with phase=implement.
# This script pins the version, verifies the install, and exits non-zero on
# drift so we get loud failure during deploy rather than a silent mismatch
# at spawn time.
#
# Usage:
#   ./infra/setup-gnhf.sh           # install/upgrade to the pinned version
#   ./infra/setup-gnhf.sh --dry-run # report what would change, no mutation
#
# Re-run is safe: if the pinned version is already installed, the npm step
# is a no-op.

set -euo pipefail

# Pinned gnhf version. Bump deliberately and record the reason in the commit
# that bumps it; gnhf's outcome enum + log format are observed via this exact
# version (see internal/implementer/parse.go and the matching plan revision).
GNHF_VERSION="v0.1.26"
GNHF_PKG="gnhf@${GNHF_VERSION#v}"  # npm wants no leading "v"

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h|--help)
      sed -n '1,16p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "setup-gnhf.sh: unknown flag: $arg" >&2
      exit 2
      ;;
  esac
done

log() { printf '[setup-gnhf] %s\n' "$*"; }

require() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "setup-gnhf.sh: required tool not found: $1" >&2
    exit 1
  fi
}

require node
require npm

NODE_MAJOR="$(node -p 'process.versions.node.split(".")[0]')"
if [ "$NODE_MAJOR" -lt 20 ]; then
  echo "setup-gnhf.sh: Node ${NODE_MAJOR} too old; gnhf requires Node 20+" >&2
  exit 1
fi
log "node v$(node -p 'process.versions.node') ok (>=20 required)"

# Detect installed version (if any). gnhf --version emits "gnhf v0.1.26" on stdout.
INSTALLED_VERSION=""
if command -v gnhf >/dev/null 2>&1; then
  INSTALLED_VERSION="$(gnhf --version 2>/dev/null | awk '{print $NF}' || true)"
fi

if [ "$INSTALLED_VERSION" = "$GNHF_VERSION" ]; then
  log "gnhf $GNHF_VERSION already installed at $(command -v gnhf) — nothing to do"
  exit 0
fi

if [ -n "$INSTALLED_VERSION" ]; then
  log "gnhf $INSTALLED_VERSION detected; will upgrade to $GNHF_VERSION"
else
  log "gnhf not installed; will install $GNHF_VERSION"
fi

if [ "$DRY_RUN" = "1" ]; then
  log "dry-run: would run \`npm install -g $GNHF_PKG\`"
  exit 0
fi

# Use sudo if the npm prefix lib dir is not writable (typical on the VM where
# /usr/local is root-owned). Falls back to a plain install when running as root
# or when the operator has set a user-owned prefix.
NPM_PREFIX="$(npm config get prefix)"
NPM_LIB="$NPM_PREFIX/lib/node_modules"
if [ -w "$NPM_LIB" ] || [ -w "$NPM_PREFIX" ]; then
  npm install -g "$GNHF_PKG"
else
  log "npm prefix $NPM_PREFIX not writable; using sudo"
  sudo npm install -g "$GNHF_PKG"
fi

# Verify post-install.
if ! command -v gnhf >/dev/null 2>&1; then
  echo "setup-gnhf.sh: gnhf still not on PATH after install" >&2
  exit 1
fi
POST_VERSION="$(gnhf --version 2>/dev/null | awk '{print $NF}' || true)"
if [ "$POST_VERSION" != "$GNHF_VERSION" ]; then
  echo "setup-gnhf.sh: post-install version mismatch: got '$POST_VERSION', want '$GNHF_VERSION'" >&2
  exit 1
fi
log "gnhf $POST_VERSION installed at $(command -v gnhf)"
