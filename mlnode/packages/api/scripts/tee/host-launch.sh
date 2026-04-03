#!/bin/bash
# Launch SEV-SNP guest VM.
# Run as root on the bare-metal host after host-setup.sh.
#
# Usage:
#   bash host-launch.sh          # launch VM
#   bash host-launch.sh stop     # stop VM
#   bash host-launch.sh status   # check VM status

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

SNP_VM_DIR="/root/snp-vm"
QEMU="/usr/local/bin/qemu-system-x86_64"

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
        ssh -p 2222 -o ConnectTimeout=3 -o StrictHostKeyChecking=no ubuntu@localhost "echo 'SSH: OK'" 2>/dev/null \
            || warn "VM running but SSH not responding yet (still booting?)"
    else
        warn "VM not running"
    fi
}

launch_vm() {
    require_cmd "$QEMU" "Run host-setup.sh first"
    for f in guest.qcow2 vmlinuz initrd.img cloud-init.iso OVMF_AMDSEV.fd; do
        require_file "$SNP_VM_DIR/$f" "Run host-setup.sh first"
    done

    if vm_running; then
        warn "VM already running. Use 'bash host-launch.sh stop' to stop it first."
        return 0
    fi

    log "Launching SEV-SNP VM..."
    "$QEMU" \
        -enable-kvm \
        -cpu EPYC-v4 \
        -machine q35,confidential-guest-support=sev0,memory-backend=ram1 \
        -object memory-backend-memfd,id=ram1,size=${VM_RAM:-32G},share=true,prealloc=false \
        -object sev-snp-guest,id=sev0,cbitpos=51,reduced-phys-bits=1,kernel-hashes=on \
        -smp 8 \
        -bios "$SNP_VM_DIR/OVMF_AMDSEV.fd" \
        -kernel "$SNP_VM_DIR/vmlinuz" \
        -initrd "$SNP_VM_DIR/initrd.img" \
        -append "root=/dev/sda1 console=ttyS0 earlyprintk=serial" \
        -drive "file=$SNP_VM_DIR/guest.qcow2,format=qcow2,if=none,id=disk0" \
        -device virtio-scsi-pci,id=scsi0,disable-legacy=on,iommu_platform=on \
        -device scsi-hd,drive=disk0 \
        -drive "file=$SNP_VM_DIR/cloud-init.iso,format=raw,if=none,id=cloud" \
        -device scsi-cd,drive=cloud \
        -netdev user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080 \
        -device virtio-net-pci,netdev=net0,iommu_platform=on \
        -display none \
        -serial "file:$SNP_VM_DIR/console.log" \
        -monitor "unix:$SNP_VM_DIR/monitor.sock,server,nowait" \
        -daemonize

    log "VM launched. Waiting for SSH (takes ~60s for cloud-init)..."
    for i in $(seq 1 24); do
        if ssh -p 2222 -o ConnectTimeout=3 -o StrictHostKeyChecking=accept-new ubuntu@localhost "echo ok" > /dev/null 2>&1; then
            log "VM ready — SSH on port 2222"
            return 0
        fi
        sleep 5
    done
    die "VM launched but SSH not available after 2 minutes" \
        "Check console: tail -f $SNP_VM_DIR/console.log"
}

case "${1:-}" in
    stop)   stop_vm ;;
    status) status_vm ;;
    *)      launch_vm ;;
esac
