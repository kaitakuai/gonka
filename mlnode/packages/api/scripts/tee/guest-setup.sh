#!/bin/bash
# Guest setup: verify SEV-SNP, install snpguest, clone gonka, build vLLM CPU.
# Run as root inside the SEV-SNP VM.
# Idempotent — safe to re-run.
#
# Usage:
#   sudo bash guest-setup.sh              # normal run
#   sudo FORCE=1 bash guest-setup.sh      # redo all steps

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# If _common.sh is not next to us (first run from gonka clone), inline essentials
if [ -f "$SCRIPT_DIR/_common.sh" ]; then
    source "$SCRIPT_DIR/_common.sh"
else
    set -euo pipefail
    CHECKPOINT_FILE="/tmp/.tee-guest-setup-progress"
    FORCE="${FORCE:-0}"
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
    log()  { echo -e "${GREEN}[+]${NC} $*"; }
    warn() { echo -e "${YELLOW}[!]${NC} $*"; }
    err()  { echo -e "${RED}[ERROR]${NC} $*" >&2; }
    die() { err "$1"; [ -n "${2:-}" ] && echo -e "${YELLOW}[FIX]${NC} $2" >&2; exit "${3:-1}"; }
    checkpoint_set() { echo "$1" >> "$CHECKPOINT_FILE"; }
    checkpoint_done() { [ "$FORCE" = "1" ] && return 1; grep -qxF "$1" "$CHECKPOINT_FILE" 2>/dev/null; }
    run_step() { local n="$1"; shift; if checkpoint_done "$n"; then log "SKIP $n"; return 0; fi; log "START $n"; if "$@"; then checkpoint_set "$n"; log "DONE $n"; else die "Step '$n' failed" "Fix and re-run."; fi; }
    require_cmd() { command -v "$1" > /dev/null 2>&1 || die "'$1' not found" "${2:-}"; }
fi

CHECKPOINT_FILE="/tmp/.tee-guest-setup-progress"

[ "$(id -u)" -eq 0 ] || die "Must run as root (sudo)"

# ── Step 1: Verify SEV-SNP ──────────────────────────────────────────────────

verify_sev_snp() {
    local enc_lines
    enc_lines=$(dmesg 2>/dev/null | grep "Memory Encryption Features active" || true)
    [ -n "$enc_lines" ] || \
        die "Memory encryption not active" "This VM may not be launched with SEV-SNP. Check host QEMU flags."

    # Load sev-guest module
    if [ ! -c /dev/sev-guest ]; then
        apt-get install -y "linux-modules-extra-$(uname -r)" 2>/dev/null || true
        modprobe sev-guest
    fi
    [ -c /dev/sev-guest ] || die "/dev/sev-guest not found" "modprobe sev-guest failed"
    log "SEV-SNP active, /dev/sev-guest present"
}

# ── Step 2: Install snpguest ────────────────────────────────────────────────

install_snpguest() {
    if command -v snpguest > /dev/null 2>&1; then
        log "snpguest already installed: $(snpguest --version 2>&1 | head -1)"
        return 0
    fi

    # Build tools needed for cargo compile
    apt-get install -y build-essential pkg-config libssl-dev 2>&1 | tail -2

    # Install Rust if needed
    if ! su - ubuntu -c 'command -v cargo' > /dev/null 2>&1; then
        su - ubuntu -c 'curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y' 2>&1 | tail -2
    fi

    su - ubuntu -c 'source ~/.cargo/env && cargo install snpguest' 2>&1 | tail -3

    # Symlink to system path (MLNode runs as root)
    ln -sf /home/ubuntu/.cargo/bin/snpguest /usr/local/bin/snpguest
    require_cmd snpguest "snpguest installation failed"
    log "snpguest installed: $(snpguest --version 2>&1 | head -1)"
}

# ── Step 3: Verify attestation ───────────────────────────────────────────────

verify_attestation() {
    local dir=$(mktemp -d)
    cd "$dir"
    snpguest report report.bin request.txt --random
    mkdir -p certs
    snpguest fetch vcek -p milan pem ./certs report.bin
    snpguest fetch ca pem ./certs milan
    snpguest verify certs ./certs
    snpguest verify attestation -p milan ./certs report.bin
    cd /tmp
    rm -rf "$dir"
    log "Attestation report generated and verified"
}

# ── Step 4: Clone gonka ─────────────────────────────────────────────────────

clone_gonka() {
    if [ -d /root/gonka ]; then
        log "gonka already cloned, pulling latest..."
        cd /root/gonka && git pull 2>/dev/null || true
        return 0
    fi
    git clone https://github.com/kaitakuai/gonka.git --branch tee --depth 1 /root/gonka
}

# ── Step 5: Setup /app structure ─────────────────────────────────────────────

setup_app_dir() {
    mkdir -p /app/packages
    ln -sf /root/gonka/mlnode/packages/api /app/packages/api
    ln -sf /root/gonka/mlnode/packages/pow /app/packages/pow
    ln -sf /root/gonka/mlnode/packages/train /app/packages/train
    ln -sf /root/gonka/mlnode/packages/common /app/packages/common
    log "/app symlinks created"
}

# ── Step 6: Create venv + install deps ───────────────────────────────────────

install_python_deps() {
    # Ensure python3-venv is available
    apt-get install -y python3-venv 2>&1 | tail -2

    if [ -f /opt/mlnode/bin/activate ]; then
        log "venv already exists"
    else
        python3 -m venv /opt/mlnode --system-site-packages
    fi
    [ -f /opt/mlnode/bin/pip ] || die "venv creation failed" "apt install python3-venv and retry"
    /opt/mlnode/bin/pip install --upgrade pip 2>&1 | tail -1

    # Install MLNode + TEE deps
    /opt/mlnode/bin/pip install \
        fastapi uvicorn httpx huggingface-hub \
        scipy fire toml tenacity \
        pynacl accelerate h2 nvidia-ml-py 2>&1 | tail -3

    /opt/mlnode/bin/python3 -c "from nacl.public import PrivateKey" || die "PyNaCl not installed"
    log "Python deps installed"
}

# ── Step 7: Build vLLM CPU ───────────────────────────────────────────────────

build_vllm_cpu() {
    # Check if already installed
    if /opt/mlnode/bin/python3 -c "import vllm; print(vllm.__version__)" 2>/dev/null | grep -q "0.15.1"; then
        if /opt/mlnode/bin/python3 -c "from vllm.platforms import current_platform; assert current_platform.device_type == 'cpu'" 2>/dev/null; then
            log "vLLM 0.15.1 CPU already installed"
            return 0
        fi
    fi

    # Install CPU PyTorch first
    /opt/mlnode/bin/pip install torch==2.9.1+cpu torchvision==0.24.1+cpu \
        --index-url https://download.pytorch.org/whl/cpu \
        --force-reinstall --no-deps 2>&1 | tail -2

    # Build vLLM from source
    /opt/mlnode/bin/pip install setuptools-scm cmake ninja 2>&1 | tail -1

    cd /tmp
    rm -rf vllm-build vllm-wheels
    git clone https://github.com/vllm-project/vllm.git vllm-build --branch v0.15.1 --depth 1
    cd vllm-build
    sed -i 's/torch==2.10.0/torch>=2.9.0/' pyproject.toml

    export VLLM_TARGET_DEVICE=cpu
    /opt/mlnode/bin/python3 setup.py build_ext --inplace 2>&1 | tail -3
    mkdir -p /tmp/vllm-wheels
    /opt/mlnode/bin/pip wheel --no-deps --no-build-isolation -w /tmp/vllm-wheels . 2>&1 | tail -3
    /opt/mlnode/bin/pip install --no-deps /tmp/vllm-wheels/vllm-*.whl 2>&1 | tail -1

    # Verify
    /opt/mlnode/bin/python3 -c "from vllm.platforms import current_platform; assert current_platform.device_type == 'cpu'" \
        || die "vLLM CPU build failed" "Check /tmp/vllm-build for build logs"

    rm -rf /tmp/vllm-build /tmp/vllm-wheels
    log "vLLM 0.15.1 CPU built and installed"
}

# ── Run ──────────────────────────────────────────────────────────────────────

echo "=== TEE Guest Setup ==="
run_step "verify-sev-snp"       verify_sev_snp
run_step "install-snpguest"     install_snpguest
run_step "verify-attestation"   verify_attestation
run_step "clone-gonka"          clone_gonka
run_step "setup-app-dir"        setup_app_dir
run_step "install-python-deps"  install_python_deps
run_step "build-vllm-cpu"       build_vllm_cpu

echo ""
log "All checks:"
log "  /dev/sev-guest:  $([ -c /dev/sev-guest ] && echo OK || echo MISSING)"
log "  snpguest:        $(snpguest --version 2>&1 | head -1)"
log "  vLLM:            $(/opt/mlnode/bin/python3 -c 'import vllm; print(vllm.__version__)' 2>/dev/null || echo MISSING)"
log "  PyNaCl:          $(/opt/mlnode/bin/python3 -c 'from nacl.public import PrivateKey; print("OK")' 2>/dev/null || echo MISSING)"
echo ""
echo "=== Guest setup complete. Run guest-start.sh to launch MLNode ==="
