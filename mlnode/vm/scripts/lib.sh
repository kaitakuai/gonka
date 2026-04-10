#!/bin/bash
# Library of helpers for TEE deployment scripts.
# Sourced by host/*.sh and guest/*.sh — not executed directly.
#
# Provides:
#   - logging: log, warn, err, die (with fix-hint)
#   - checkpoint state: checkpoint_set, checkpoint_done, run_step
#   - preconditions: require_cmd, require_file

set -euo pipefail

CHECKPOINT_FILE="${CHECKPOINT_FILE:-/tmp/.tee-setup-progress}"
FORCE="${FORCE:-0}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[+]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }

die() {
    err "$1"
    if [ -n "${2:-}" ]; then
        echo -e "${YELLOW}[FIX]${NC} $2" >&2
    fi
    exit "${3:-1}"
}

# Checkpoint: mark step as done
checkpoint_set() { echo "$1" >> "$CHECKPOINT_FILE"; }

# Checkpoint: check if step already done (skip if --force)
checkpoint_done() {
    [ "$FORCE" = "1" ] && return 1
    grep -qxF "$1" "$CHECKPOINT_FILE" 2>/dev/null
}

# Run a step: skip if done, mark done on success
run_step() {
    local name="$1"
    shift
    if checkpoint_done "$name"; then
        log "SKIP $name (already done, use FORCE=1 to redo)"
        return 0
    fi
    log "START $name"
    if "$@"; then
        checkpoint_set "$name"
        log "DONE $name"
    else
        die "Step '$name' failed" "Fix the issue above and re-run the script. It will resume from this step."
    fi
}

# Require a command to exist
require_cmd() {
    command -v "$1" > /dev/null 2>&1 || die "'$1' not found" "${2:-Install $1 and retry}"
}

# Require a file to exist
require_file() {
    [ -f "$1" ] || die "File not found: $1" "${2:-}"
}
