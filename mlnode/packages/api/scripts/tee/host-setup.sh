#!/bin/bash
# Host setup: packages, QEMU 9.2, OVMF AmdSev, VM image.
# Run as root on the bare-metal host.
# Idempotent — safe to re-run. Skips completed steps.
#
# Usage:
#   bash host-setup.sh              # normal run
#   FORCE=1 bash host-setup.sh      # redo all steps

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/_common.sh"

CHECKPOINT_FILE="/tmp/.tee-host-setup-progress"
SNP_VM_DIR="/root/snp-vm"

[ "$(id -u)" -eq 0 ] || die "Must run as root"

# ── Step 0: Verify SEV-SNP ──────────────────────────────────────────────────

verify_sev_snp() {
    dmesg | grep -q "SEV-SNP" || die \
        "SEV-SNP not found in dmesg" \
        "Check BIOS: SVM, SMEE, SNP Memory Coverage, IOMMU, SEV-SNP must be enabled. See runbook Prerequisites."
    [ -c /dev/sev ] || die "/dev/sev not found" "Kernel may need kvm_amd module with sev_snp=1"
    log "SEV-SNP verified: $(dmesg | grep 'SEV-SNP API' | tail -1)"
}

# ── Step 1: Install packages ────────────────────────────────────────────────

install_packages() {
    apt-get update -qq
    apt-get install -y \
        qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients \
        ovmf cloud-image-utils virtinst cpu-checker \
        git build-essential ninja-build pkg-config \
        libglib2.0-dev libpixman-1-dev libslirp-dev \
        python3-venv python3-pip flex bison iasl nasm \
        mtools grub-efi-amd64-bin sshpass socat
    kvm-ok || die "KVM not available" "Check BIOS virtualization settings"
}

# ── Step 2: Build QEMU 9.2 ──────────────────────────────────────────────────

build_qemu() {
    if /usr/local/bin/qemu-system-x86_64 -object help 2>&1 | grep -q "sev-snp-guest"; then
        log "QEMU with sev-snp-guest already installed"
        return 0
    fi

    cd /root
    [ -d qemu ] || git clone https://gitlab.com/qemu-project/qemu.git --branch v9.2.3 --depth 1
    cd qemu
    mkdir -p build && cd build
    ../configure --target-list=x86_64-softmmu --enable-kvm --enable-slirp --prefix=/usr/local
    make -j"$(nproc)"
    make install

    /usr/local/bin/qemu-system-x86_64 -object help 2>&1 | grep -q "sev-snp-guest" || \
        die "QEMU built but sev-snp-guest object not found"
    log "QEMU $(/usr/local/bin/qemu-system-x86_64 --version | head -1)"
}

# ── Step 3: Build OVMF AmdSev ───────────────────────────────────────────────

build_ovmf() {
    local ovmf_out="/root/edk2/Build/AmdSev/DEBUG_GCC5/FV/OVMF.fd"
    if [ -f "$ovmf_out" ]; then
        log "OVMF already built: $ovmf_out"
        return 0
    fi

    cd /root
    [ -d edk2 ] || git clone https://github.com/tianocore/edk2.git --branch edk2-stable202411 --depth 1
    cd edk2
    git submodule update --init --depth 1

    # Patch GRUB modules (Ubuntu doesn't ship linuxefi/sevsecret)
    cd OvmfPkg/AmdSev/Grub
    sed -i 's/linuxefi//' grub.sh
    sed -i '/sevsecret/d' grub.sh

    # Replace GRUB config for non-encrypted cloud images
    cat > grub.cfg << 'GRUBCFG'
echo "SEV-SNP Guest Booting..."
set timeout=3
insmod part_gpt
insmod ext2
set root=(hd0,gpt1)
if [ -e (hd0,gpt1)/boot/grub/grub.cfg ]; then
    set prefix=(hd0,gpt1)/boot/grub
    source $prefix/grub.cfg
elif [ -e (hd0,gpt1)/boot/vmlinuz ]; then
    linux (hd0,gpt1)/boot/vmlinuz root=/dev/sda1 console=ttyS0
    initrd (hd0,gpt1)/boot/initrd.img
    boot
else
    for d in (hd0,gpt1) (hd0,gpt2) (hd0,gpt3) (hd0,gpt14) (hd0,gpt15) (hd0,msdos1); do
        if [ -e $d/boot/grub/grub.cfg ]; then
            set root=$d
            set prefix=($root)/boot/grub
            source $prefix/grub.cfg
        fi
    done
    echo "No bootable config found"
fi
GRUBCFG

    # Build
    cd /root/edk2
    source edksetup.sh
    make -C BaseTools -j"$(nproc)"
    export WORKSPACE=/root/edk2
    export EDK_TOOLS_PATH=/root/edk2/BaseTools
    export PATH="$PATH:/root/edk2/BaseTools/BinWrappers/PosixLike"
    build -a X64 -b DEBUG -t GCC5 -p OvmfPkg/AmdSev/AmdSevX64.dsc -n "$(nproc)"

    require_file "$ovmf_out" "OVMF build failed"
    log "OVMF built: $(ls -lh "$ovmf_out" | awk '{print $5}')"
}

# ── Step 4: Prepare VM image ────────────────────────────────────────────────

prepare_vm_image() {
    mkdir -p "$SNP_VM_DIR" && cd "$SNP_VM_DIR"

    # Download Ubuntu cloud image
    if [ ! -f ubuntu-24.04-cloud.img ]; then
        wget -q --show-progress \
            https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img \
            -O ubuntu-24.04-cloud.img
    fi

    # Create overlay
    [ -f guest.qcow2 ] || qemu-img create -f qcow2 -b ubuntu-24.04-cloud.img -F qcow2 guest.qcow2 60G

    # Extract kernel/initrd
    if [ ! -f vmlinuz ] || [ ! -f initrd.img ]; then
        modprobe nbd max_part=8
        qemu-nbd -c /dev/nbd0 ubuntu-24.04-cloud.img
        sleep 2
        mkdir -p /mnt/boot
        mount /dev/nbd0p16 /mnt/boot
        cp /mnt/boot/vmlinuz-* ./vmlinuz
        cp /mnt/boot/initrd.img-* ./initrd.img
        umount /mnt/boot
        qemu-nbd -d /dev/nbd0
    fi

    # Generate SSH key if missing
    [ -f /root/.ssh/id_ed25519 ] || ssh-keygen -t ed25519 -N "" -f /root/.ssh/id_ed25519 -q

    # Cloud-init
    cat > meta-data << EOF
instance-id: snp-guest-01
local-hostname: snp-guest
EOF

    cat > user-data << EOF
#cloud-config
password: snpguest
chpasswd: { expire: False }
ssh_pwauth: True
ssh_authorized_keys:
  - $(cat /root/.ssh/id_ed25519.pub)
packages:
  - python3-pip
  - python3-venv
  - curl
  - wget
  - dos2unix
  - build-essential
  - pkg-config
  - libssl-dev
  - libnuma-dev
  - cmake
runcmd:
  - echo "SNP guest ready" > /tmp/snp-ready
EOF

    cloud-localds cloud-init.iso user-data meta-data

    # Copy OVMF
    cp /root/edk2/Build/AmdSev/DEBUG_GCC5/FV/OVMF.fd ./OVMF_AMDSEV.fd

    for f in guest.qcow2 vmlinuz initrd.img cloud-init.iso OVMF_AMDSEV.fd; do
        require_file "$SNP_VM_DIR/$f" "$f missing in $SNP_VM_DIR"
    done
    log "VM image ready in $SNP_VM_DIR"
}

# ── Run ──────────────────────────────────────────────────────────────────────

echo "=== TEE Host Setup ==="
run_step "verify-sev-snp"    verify_sev_snp
run_step "install-packages"  install_packages
run_step "build-qemu"        build_qemu
run_step "build-ovmf"        build_ovmf
run_step "prepare-vm-image"  prepare_vm_image
echo "=== Host setup complete. Run host-launch.sh to start the VM ==="
