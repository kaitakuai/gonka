#!/bin/bash
# Launch TEE guest VM (AMD SEV-SNP or Intel TDX — auto-detected).
# Run as root on the bare-metal host after host/setup.sh.
#
# Usage:
#   bash host/launch.sh          # launch VM (auto-detect platform)
#   bash host/launch.sh stop     # stop VM
#   bash host/launch.sh status   # check VM status
#
# Environment:
#   VM_RAM=32G       # guest RAM (default: 32G)
#   VM_CPUS=8        # guest vCPUs (default: 8)
#   SSH_PORT=2222    # host port forwarded to guest SSH (default: 2222)
#   API_PORT=8080    # host port forwarded to guest API (default: 8080)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/../_common.sh"
source "$SCRIPT_DIR/detect.sh"

VM_DIR="/root/tee-vm"
VM_RAM="${VM_RAM:-32G}"
VM_CPUS="${VM_CPUS:-8}"
SSH_PORT="${SSH_PORT:-2222}"
API_PORT="${API_PORT:-8080}"

[ "$(id -u)" -eq 0 ] || die "Must run as root"

vm_running() { pgrep -f "qemu.*guest.qcow2" > /dev/null 2>&1; }

stop_vm() {
    if vm_running; then
        log "Stopping VM..."
        pkill -f "qemu.*guest.qcow2"
        sleep 3
        vm_running && die "VM did not stop"
        log "VM stopped"
    else
        warn "VM not running"
    fi
}

status_vm() {
    if vm_running; then
        log "VM is running (PID $(pgrep -f 'qemu.*guest.qcow2'))"
        ssh -p "$SSH_PORT" -o ConnectTimeout=3 -o StrictHostKeyChecking=no ubuntu@localhost "echo 'SSH: OK'" 2>/dev/null \
            || warn "VM running but SSH not responding yet (still booting?)"
    else
        warn "VM not running"
    fi
}

# ── QEMU binary ──────────────────────────────────────────────────────────────

find_qemu() {
    # Prefer locally built QEMU (host/setup.sh builds to /usr/local/bin)
    if [ -x /usr/local/bin/qemu-system-x86_64 ]; then
        echo /usr/local/bin/qemu-system-x86_64
    elif [ -x /usr/bin/qemu-system-x86_64 ]; then
        echo /usr/bin/qemu-system-x86_64
    else
        die "qemu-system-x86_64 not found" "Run host/setup.sh first"
    fi
}

# ── AMD SEV-SNP launch ───────────────────────────────────────────────────────

launch_amd() {
    local qemu="$1"

    require_file "$VM_DIR/OVMF_AMDSEV.fd" "Run host/setup.sh first (AMD OVMF not built)"
    require_file "$VM_DIR/vmlinuz" "Run host/setup.sh first (kernel not extracted)"
    require_file "$VM_DIR/initrd.img" "Run host/setup.sh first (initrd not extracted)"

    log "Launching AMD SEV-SNP VM (RAM=$VM_RAM, CPUs=$VM_CPUS)..."
    "$qemu" \
        -enable-kvm \
        -cpu EPYC-v4 \
        -machine q35,confidential-guest-support=sev0,memory-backend=ram1 \
        -object memory-backend-memfd,id=ram1,size="$VM_RAM",share=true,prealloc=false \
        -object sev-snp-guest,id=sev0,cbitpos=51,reduced-phys-bits=1,kernel-hashes=on \
        -smp "$VM_CPUS" \
        -bios "$VM_DIR/OVMF_AMDSEV.fd" \
        -kernel "$VM_DIR/vmlinuz" \
        -initrd "$VM_DIR/initrd.img" \
        -append "root=/dev/sda1 console=ttyS0 earlyprintk=serial" \
        -drive "file=$VM_DIR/guest.qcow2,format=qcow2,if=none,id=disk0" \
        -device virtio-scsi-pci,id=scsi0,disable-legacy=on,iommu_platform=on \
        -device scsi-hd,drive=disk0 \
        -drive "file=$VM_DIR/cloud-init.iso,format=raw,if=none,id=cloud" \
        -device scsi-cd,drive=cloud \
        -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22,hostfwd=tcp::${API_PORT}-:8080" \
        -device virtio-net-pci,netdev=net0,iommu_platform=on \
        -display none \
        -serial "file:$VM_DIR/console.log" \
        -monitor "unix:$VM_DIR/monitor.sock,server,nowait" \
        -daemonize
}

# ── Intel TDX launch ─────────────────────────────────────────────────────────

find_tdx_ovmf() {
    # Try common TDX OVMF locations
    local candidates=(
        "/usr/share/ovmf/OVMF.tdx.fd"
        "/usr/share/ovmf/OVMF.fd"
        "/usr/share/qemu/OVMF.fd"
        "$VM_DIR/OVMF_TDX.fd"
    )
    for f in "${candidates[@]}"; do
        if [ -f "$f" ]; then
            echo "$f"
            return 0
        fi
    done
    die "TDX OVMF firmware not found" \
        "Install: apt install ovmf (Canonical TDX PPA provides TDX-capable OVMF)"
}

launch_tdx() {
    local qemu="$1"

    # TDX OVMF comes from distro packages — no custom build needed
    local ovmf
    ovmf=$(find_tdx_ovmf)
    log "Using TDX OVMF: $ovmf"

    log "Launching Intel TDX VM (RAM=$VM_RAM, CPUs=$VM_CPUS)..."
    "$qemu" \
        -enable-kvm \
        -cpu host \
        -machine q35,kernel_irqchip=split,confidential-guest-support=tdx0,memory-backend=ram1 \
        -object memory-backend-memfd,id=ram1,size="$VM_RAM",share=true,prealloc=false \
        -object tdx-guest,id=tdx0 \
        -smp "$VM_CPUS" \
        -bios "$ovmf" \
        -drive "file=$VM_DIR/guest.qcow2,format=qcow2,if=none,id=disk0" \
        -device virtio-scsi-pci,id=scsi0 \
        -device scsi-hd,drive=disk0 \
        -drive "file=$VM_DIR/cloud-init.iso,format=raw,if=none,id=cloud" \
        -device scsi-cd,drive=cloud \
        -netdev "user,id=net0,hostfwd=tcp::${SSH_PORT}-:22,hostfwd=tcp::${API_PORT}-:8080" \
        -device virtio-net-pci,netdev=net0 \
        -device vhost-vsock-pci,guest-cid=3 \
        -display none \
        -serial "file:$VM_DIR/console.log" \
        -monitor "unix:$VM_DIR/monitor.sock,server,nowait" \
        -daemonize
}

# ── Main launch ──────────────────────────────────────────────────────────────

launch_vm() {
    local qemu
    qemu=$(find_qemu)
    log "QEMU: $qemu ($("$qemu" --version | head -1))"

    # Detect platform
    local platform
    platform=$(detect_platform)
    log "Platform: $platform"

    # Check common files
    require_file "$VM_DIR/guest.qcow2" "Run host/setup.sh first"
    require_file "$VM_DIR/cloud-init.iso" "Run host/setup.sh first"

    if vm_running; then
        warn "VM already running. Use 'bash host/launch.sh stop' to stop it first."
        return 0
    fi

    # Clear stale VM host key
    ssh-keygen -f "$HOME/.ssh/known_hosts" -R "[localhost]:${SSH_PORT}" 2>/dev/null || true

    # Launch platform-specific VM
    case "$platform" in
        amd-sev-snp)  launch_amd "$qemu" ;;
        intel-tdx)    launch_tdx "$qemu" ;;
        *)            die "Unknown platform: $platform" ;;
    esac

    # Wait for SSH
    log "VM launched. Waiting for SSH on port $SSH_PORT..."
    for i in $(seq 1 24); do
        if ssh -p "$SSH_PORT" -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new ubuntu@localhost "echo ok" > /dev/null 2>&1; then
            log "VM ready — SSH on port $SSH_PORT"
            return 0
        fi
        sleep 5
    done
    die "VM launched but SSH not available after 2 minutes" \
        "Check console: tail -f $VM_DIR/console.log"
}

# ── Entry point ──────────────────────────────────────────────────────────────

case "${1:-}" in
    stop)   stop_vm ;;
    status) status_vm ;;
    *)      launch_vm ;;
esac
