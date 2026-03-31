#!/bin/bash
# Stage 3.5: TEE layer for Confidential MLNode
#
# Adds TEE support (Gonka proposal #951) on top of stage3_mlnode.sh:
#   1. Installs PyNaCl
#   2. Ensures snpguest is available
#   3. Loads sev-guest kernel module
#
# TEE module (tee/) and app.py changes are already in the repo.
#
# Prerequisites:
#   - stage3_mlnode.sh completed
#   - snpguest installed (/root/.cargo/bin/snpguest)
#   - /dev/sev-guest available (SEV-SNP VM)
#
# Usage:
#   bash stage3_tee.sh
#
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH="/root/miniconda3/bin:/usr/local/cuda/bin:/usr/sbin:/usr/bin:/sbin:/bin"

echo "=== Stage 3.5: TEE layer (Confidential MLNode) ==="
echo "Date: $(date)"

# --- Step 1: Install dependencies ---
echo "[1/3] Installing TEE dependencies..."
pip install --quiet PyNaCl 2>&1 | tail -3
echo "  PyNaCl: $(python3.12 -c 'import nacl; print(nacl.__version__)' 2>/dev/null || echo 'installed')"

# --- Step 2: Ensure snpguest is available ---
echo "[2/3] Checking snpguest..."
if [ -f /root/.cargo/bin/snpguest ]; then
    echo "  snpguest: $(/root/.cargo/bin/snpguest --version 2>/dev/null || echo 'found')"
else
    echo "  WARNING: snpguest not found — attestation will fail"
    echo "  Install: cargo install snpguest"
fi

# --- Step 3: Load sev-guest module ---
echo "[3/3] Loading sev-guest kernel module..."
modprobe sev-guest 2>/dev/null && echo "  sev-guest: loaded" || echo "  sev-guest: not available (not in SEV-SNP VM?)"

# --- Verify ---
echo ""
echo "=== Stage 3.5 complete ==="
echo "  PyNaCl:         $(python3.12 -c 'import nacl' 2>/dev/null && echo ok || echo MISSING)"
echo "  snpguest:       $(test -f /root/.cargo/bin/snpguest && echo ok || echo MISSING)"
echo "  /dev/sev-guest: $(test -c /dev/sev-guest && echo ok || echo MISSING)"
echo ""
echo "To enable TEE mode, set environment variable before starting MLNode:"
echo "  export TEE_ENABLED=1"
