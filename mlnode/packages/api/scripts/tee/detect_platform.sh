#!/bin/bash
# Detect TEE platform on the host (before launching VM).
# Outputs: "amd-sev-snp" or "intel-tdx"
# Exits with error if no TEE platform found.
#
# Usage:
#   source detect_platform.sh   # sets TEE_PLATFORM variable
#   # or
#   TEE_PLATFORM=$(bash detect_platform.sh)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -f "$SCRIPT_DIR/_common.sh" ] && source "$SCRIPT_DIR/_common.sh" || {
    log()  { echo "[+] $*"; }
    warn() { echo "[!] $*"; }
    die()  { echo "[ERROR] $1" >&2; exit 1; }
}

detect_platform() {
    local platform=""

    # Check AMD SEV-SNP
    if [ -c /dev/sev ]; then
        local sev_lines
        sev_lines=$(dmesg 2>/dev/null | grep "SEV-SNP" || true)
        if [ -n "$sev_lines" ]; then
            platform="amd-sev-snp"
            log "Detected AMD SEV-SNP: /dev/sev present, SEV-SNP in dmesg"
        fi
    fi

    # Check Intel TDX
    if [ -z "$platform" ]; then
        local tdx_lines
        tdx_lines=$(dmesg 2>/dev/null | grep "virt/tdx" || true)
        if [ -n "$tdx_lines" ]; then
            local tdx_param
            tdx_param=$(cat /sys/module/kvm_intel/parameters/tdx 2>/dev/null || echo "N")
            if [ "$tdx_param" = "Y" ]; then
                platform="intel-tdx"
                log "Detected Intel TDX: kvm_intel.tdx=Y, TDX in dmesg"
            else
                warn "TDX visible in dmesg but kvm_intel.tdx=N"
                warn "Enable with: modprobe kvm_intel tdx=1 (or add kvm_intel.tdx=on to GRUB)"
            fi
        fi
    fi

    # No TEE found
    if [ -z "$platform" ]; then
        echo "" >&2
        echo "========================================================" >&2
        echo " No TEE platform detected on this host." >&2
        echo " Confidential MLNode requires AMD SEV-SNP or Intel TDX." >&2
        echo "========================================================" >&2
        echo "" >&2
        echo " Checked:" >&2
        echo "   /dev/sev          — $([ -c /dev/sev ] && echo 'exists' || echo 'NOT FOUND')" >&2
        echo "   dmesg SEV-SNP     — $(dmesg 2>/dev/null | grep -c 'SEV-SNP' || echo '0') matches" >&2
        echo "   dmesg virt/tdx    — $(dmesg 2>/dev/null | grep -c 'virt/tdx' || echo '0') matches" >&2
        echo "   kvm_intel.tdx     — $(cat /sys/module/kvm_intel/parameters/tdx 2>/dev/null || echo 'N/A')" >&2
        echo "" >&2
        die "No TEE hardware found. Cannot launch Confidential MLNode."
    fi

    TEE_PLATFORM="$platform"
    echo "$platform"
}

# If executed (not sourced), run detection
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    detect_platform
fi
