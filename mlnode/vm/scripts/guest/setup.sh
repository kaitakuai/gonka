#!/bin/bash
# Guest setup: verify TEE (SEV-SNP or TDX), install tools, clone gonka, build vLLM CPU.
# Run as root inside the TEE VM.
# Idempotent — safe to re-run.
#
# Usage:
#   sudo bash guest/setup.sh              # normal run
#   sudo FORCE=1 bash guest/setup.sh      # redo all steps

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# If _common.sh is not next to us (first run from gonka clone), inline essentials
if [ -f "$SCRIPT_DIR/../_common.sh" ]; then
    source "$SCRIPT_DIR/../_common.sh"
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

# ── Step 1: Detect and verify TEE ────────────────────────────────────────────

TEE_PLATFORM=""

detect_guest_tee() {
    local enc_lines
    enc_lines=$(dmesg 2>/dev/null | grep "Memory Encryption Features active" || true)
    [ -n "$enc_lines" ] || \
        die "Memory encryption not active" "This VM was not launched with SEV-SNP or TDX. Check host QEMU flags."

    if echo "$enc_lines" | grep -q "AMD SEV"; then
        TEE_PLATFORM="amd-sev-snp"
    elif echo "$enc_lines" | grep -q "Intel TDX"; then
        TEE_PLATFORM="intel-tdx"
    else
        die "Unknown TEE type in: $enc_lines"
    fi

    log "TEE platform: $TEE_PLATFORM"
}

verify_sev_snp() {
    # Load sev-guest module
    if [ ! -c /dev/sev-guest ]; then
        apt-get install -y "linux-modules-extra-$(uname -r)" 2>/dev/null || true
        modprobe sev-guest 2>/dev/null || true
    fi
    [ -c /dev/sev-guest ] || die "/dev/sev-guest not found" "modprobe sev-guest failed"
    log "SEV-SNP active, /dev/sev-guest present"
}

verify_tdx() {
    if [ ! -c /dev/tdx_guest ]; then
        modprobe tdx_guest 2>/dev/null || true
    fi
    [ -c /dev/tdx_guest ] || die "/dev/tdx_guest not found" "modprobe tdx_guest failed"

    # Check configfs-tsm (preferred for quote generation)
    if [ -d /sys/kernel/config/tsm/report ]; then
        log "TDX active, /dev/tdx_guest + configfs-tsm present"
    else
        log "TDX active, /dev/tdx_guest present (no configfs-tsm — will use ioctl)"
    fi
}

verify_tee() {
    detect_guest_tee
    case "$TEE_PLATFORM" in
        amd-sev-snp) verify_sev_snp ;;
        intel-tdx)   verify_tdx ;;
    esac
}

# ── Step 2: Install attestation tools (platform-specific) ────────────────────

install_snpguest() {
    if command -v snpguest > /dev/null 2>&1; then
        log "snpguest already installed: $(snpguest --version 2>&1 | head -1)"
        return 0
    fi

    apt-get install -y build-essential pkg-config libssl-dev 2>&1 | tail -2

    # Install Rust if needed
    if ! su - ubuntu -c 'command -v cargo' > /dev/null 2>&1; then
        su - ubuntu -c 'curl --proto "=https" --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y' 2>&1 | tail -2
    fi

    su - ubuntu -c 'source ~/.cargo/env && cargo install snpguest' 2>&1 | tail -3

    ln -sf /home/ubuntu/.cargo/bin/snpguest /usr/local/bin/snpguest
    require_cmd snpguest "snpguest installation failed"
    log "snpguest installed: $(snpguest --version 2>&1 | head -1)"
}

install_attestation_tools() {
    case "$TEE_PLATFORM" in
        amd-sev-snp)
            install_snpguest
            ;;
        intel-tdx)
            # TDX uses configfs-tsm (kernel built-in) + Python ctypes
            # No extra binary tools needed — intel_tdx.py handles everything
            log "TDX attestation: using configfs-tsm (no extra tools needed)"
            ;;
    esac
}

# ── Step 3: Verify attestation ───────────────────────────────────────────────

verify_amd_attestation() {
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
    log "AMD attestation report generated and verified"
}

verify_tdx_attestation() {
    # Quick test: write to configfs-tsm, check outblob
    if [ -d /sys/kernel/config/tsm/report ]; then
        local entry="/sys/kernel/config/tsm/report/setup-test"
        mkdir -p "$entry"
        dd if=/dev/urandom bs=64 count=1 2>/dev/null > "$entry/inblob"
        sleep 3
        local size
        size=$(wc -c < "$entry/outblob")
        rmdir "$entry" 2>/dev/null || true
        if [ "$size" -gt 0 ]; then
            log "TDX Quote generated via configfs-tsm ($size bytes)"
        else
            warn "TDX Quote empty (QGS may not be running on host)"
            warn "Attestation will use structural validation only"
        fi
    else
        warn "configfs-tsm not available — will use /dev/tdx_guest ioctl"
    fi
}

verify_attestation() {
    case "$TEE_PLATFORM" in
        amd-sev-snp) verify_amd_attestation ;;
        intel-tdx)   verify_tdx_attestation ;;
    esac
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
    apt-get install -y python3-venv 2>&1 | tail -2

    if [ -f /opt/mlnode/bin/activate ]; then
        log "venv already exists"
    else
        python3 -m venv /opt/mlnode --system-site-packages
    fi
    [ -f /opt/mlnode/bin/pip ] || die "venv creation failed" "apt install python3-venv and retry"
    /opt/mlnode/bin/pip install --upgrade pip 2>&1 | tail -1

    /opt/mlnode/bin/pip install \
        fastapi uvicorn httpx huggingface-hub \
        scipy fire toml tenacity \
        pynacl accelerate h2 nvidia-ml-py 2>&1 | tail -3

    /opt/mlnode/bin/python3 -c "from nacl.public import PrivateKey" || die "PyNaCl not installed"
    log "Python deps installed"
}

# ── Step 7: Build vLLM CPU ───────────────────────────────────────────────────

build_vllm_cpu() {
    source /opt/mlnode/bin/activate

    if python3 -c "import vllm; print(vllm.__version__)" 2>/dev/null | grep -q "0.15.1"; then
        if python3 -c "from vllm.platforms import current_platform; assert current_platform.device_type == 'cpu'" 2>/dev/null; then
            log "vLLM 0.15.1 CPU already installed"
            return 0
        fi
    fi

    pip install torch==2.9.1+cpu torchvision==0.24.1+cpu \
        --index-url https://download.pytorch.org/whl/cpu 2>&1 | tail -2

    pip install "setuptools>=77" "packaging>=24.2" setuptools-scm cmake ninja regex 2>&1 | tail -1
    apt-get install -y libnuma-dev python3-dev 2>&1 | tail -1

    local build_dir="/root/vllm-build"
    rm -rf "$build_dir" /tmp/vllm-wheels
    cd /root
    git clone https://github.com/vllm-project/vllm.git "$build_dir" --branch v0.15.1 --depth 1
    cd "$build_dir"
    sed -i 's/torch==2.10.0/torch>=2.9.0/' pyproject.toml

    pip install vllm==0.15.1 2>&1 | tail -3
    pip uninstall -y vllm 2>&1 | tail -1
    VLLM_TARGET_DEVICE=cpu pip install --no-build-isolation --no-deps -e . 2>&1 | tail -3

    /opt/mlnode/bin/python3 -c "from vllm.platforms import current_platform; assert current_platform.device_type == 'cpu'" \
        || die "vLLM CPU build failed" "Check /tmp/vllm-build for build logs"

    rm -rf "$build_dir" /tmp/vllm-wheels
    log "vLLM 0.15.1 CPU built and installed"
}

# ── Run ──────────────────────────────────────────────────────────────────────

echo "=== TEE Guest Setup ==="
run_step "verify-tee"            verify_tee
run_step "install-attest-tools"  install_attestation_tools
run_step "verify-attestation"    verify_attestation
run_step "clone-gonka"           clone_gonka
run_step "setup-app-dir"         setup_app_dir
run_step "install-python-deps"   install_python_deps
run_step "build-vllm-cpu"        build_vllm_cpu

echo ""
log "TEE platform: $TEE_PLATFORM"
log "All checks:"
if [ "$TEE_PLATFORM" = "amd-sev-snp" ]; then
    log "  /dev/sev-guest:  $([ -c /dev/sev-guest ] && echo OK || echo MISSING)"
    log "  snpguest:        $(snpguest --version 2>&1 | head -1 || echo MISSING)"
elif [ "$TEE_PLATFORM" = "intel-tdx" ]; then
    log "  /dev/tdx_guest:  $([ -c /dev/tdx_guest ] && echo OK || echo MISSING)"
    log "  configfs-tsm:    $([ -d /sys/kernel/config/tsm/report ] && echo OK || echo MISSING)"
fi
log "  vLLM:            $(/opt/mlnode/bin/python3 -c 'import vllm; print(vllm.__version__)' 2>/dev/null || echo MISSING)"
log "  PyNaCl:          $(/opt/mlnode/bin/python3 -c 'from nacl.public import PrivateKey; print("OK")' 2>/dev/null || echo MISSING)"
echo ""
echo "=== Guest setup complete. Run guest/start.sh to launch MLNode ==="
