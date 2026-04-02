# Confidential MLNode — Full Deployment Runbook

Complete guide to deploy a Confidential MLNode with AMD SEV-SNP, E2E encrypted inference, and hardware attestation.

Implements [Gonka TEE Proposal #951](https://github.com/gonka-ai/gonka/discussions/951).

**Tested on:** 2026-03-27
**Host:** Ubuntu 24.04 LTS, kernel 6.14.0-37-generic
**CPU:** AMD EPYC 7443P 24-Core (Milan, SP3 socket)
**Guest:** Ubuntu 24.04 cloud image, kernel 6.8.0-106-generic
**Code:** [kaitakuai/gonka tee](https://github.com/kaitakuai/gonka/tree/tee/mlnode)

---

## Prerequisites

### Hardware Requirements

- **CPU:** AMD EPYC 7003 (Milan) or newer on SP3/SP5 socket
  - NOT compatible: Ryzen, Threadripper, EPYC 4004 (AM5 socket)
- **RAM:** 64 GB recommended (16 GB allocated to guest)
- **Storage:** 60+ GB free disk space
- **BIOS/UEFI:** Full BMC/IPMI access required for BIOS changes

### BIOS Settings (via BMC/IPMI)

Navigate to **Advanced > CPU Configuration** and set:
- **SVM Mode** → Enabled
- **SMEE** → Enabled
- **SEV ASID Count** → configured
- **SEV-ES ASID Space Limit Control** → Manual (99)
- **SNP Memory (RMP Table) Coverage** → Enabled

Navigate to **Advanced > North Bridge Configuration** and set:
- **IOMMU** → Enabled
- **SEV-SNP Support** → Enabled

Reboot the server after BIOS changes.

### Verify SEV-SNP on Host

```bash
dmesg | grep -i sev
```

Expected output:
```
SEV-SNP: RMP table physical range [0x0000000097900000 - 0x00000000a7efffff]
ccp 0000:45:00.1: sev enabled
ccp 0000:45:00.1: SEV-SNP API:1.55 build:29
kvm_amd: SEV enabled (ASIDs 100 - 509)
kvm_amd: SEV-ES enabled (ASIDs 1 - 99)
kvm_amd: SEV-SNP enabled (ASIDs 1 - 99)
```

Also verify:
```bash
ls /dev/sev         # Should exist
cat /sys/module/kvm_amd/parameters/sev_snp  # Should be Y
```

---

## Step 1: Install Host Packages

```bash
apt update
apt install -y \
  qemu-system-x86 qemu-utils libvirt-daemon-system libvirt-clients \
  ovmf cloud-image-utils virtinst cpu-checker \
  git build-essential ninja-build pkg-config \
  libglib2.0-dev libpixman-1-dev libslirp-dev \
  python3-venv python3-pip flex bison iasl nasm \
  mtools grub-efi-amd64-bin sshpass socat
```

Verify KVM works:
```bash
kvm-ok
# Expected: KVM acceleration can be used
```

---

## Step 2: Build QEMU 9.2 with SEV-SNP Support

Stock QEMU 8.2 from Ubuntu 24.04 only has `sev-guest`. SEV-SNP requires `sev-snp-guest` object, available in QEMU 9.0+.

```bash
cd /root
git clone https://gitlab.com/qemu-project/qemu.git --branch v9.2.3 --depth 1
cd qemu
mkdir build && cd build
../configure --target-list=x86_64-softmmu --enable-kvm --enable-slirp --prefix=/usr/local
make -j$(nproc)
make install
```

Verify:
```bash
/usr/local/bin/qemu-system-x86_64 --version
# QEMU emulator version 9.2.3

/usr/local/bin/qemu-system-x86_64 -object help 2>&1 | grep sev
# sev-guest
# sev-snp-guest    <-- this is what we need
```

---

## Step 3: Build OVMF (AmdSev Platform)

Stock OVMF lacks SEV GUID tables required by QEMU for SNP initialization. `KVM_CAP_READONLY_MEM` is not supported on KVM AMD, so pflash (split CODE/VARS) doesn't work — we need the single-file AmdSev OVMF that uses `-bios`.

```bash
cd /root
git clone https://github.com/tianocore/edk2.git --branch edk2-stable202411 --depth 1
cd edk2
git submodule update --init --depth 1
```

### Patch GRUB Modules

The AmdSev OVMF embeds a GRUB bootloader. Ubuntu's grub package doesn't include `linuxefi` (merged into `linux`) or `sevsecret` modules. Remove them:

```bash
cd /root/edk2/OvmfPkg/AmdSev/Grub
sed -i 's/linuxefi//' grub.sh
sed -i '/sevsecret/d' grub.sh
```

### Replace GRUB Config for Non-Encrypted Boot

The default `grub.cfg` expects LUKS-encrypted disks. Replace it for regular cloud images:

```bash
cat > /root/edk2/OvmfPkg/AmdSev/Grub/grub.cfg << 'EOF'
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
    echo "Searching for bootable partition..."
    for d in (hd0,gpt1) (hd0,gpt2) (hd0,gpt3) (hd0,gpt14) (hd0,gpt15) (hd0,msdos1); do
        if [ -e $d/boot/grub/grub.cfg ]; then
            set root=$d
            set prefix=($root)/boot/grub
            echo "Found grub config on $d"
            source $prefix/grub.cfg
        fi
    done
    echo "No bootable config found"
fi
EOF
```

### Build

```bash
cd /root/edk2
source edksetup.sh
make -C BaseTools -j$(nproc)

export WORKSPACE=/root/edk2
export EDK_TOOLS_PATH=/root/edk2/BaseTools
export PATH=$PATH:/root/edk2/BaseTools/BinWrappers/PosixLike

build -a X64 -b DEBUG -t GCC5 -p OvmfPkg/AmdSev/AmdSevX64.dsc -n $(nproc)
```

Output: `/root/edk2/Build/AmdSev/DEBUG_GCC5/FV/OVMF.fd` (4 MB)

---

## Step 4: Prepare Guest VM Image

### Download Ubuntu Cloud Image

```bash
mkdir -p /root/snp-vm && cd /root/snp-vm
wget https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img \
  -O ubuntu-24.04-cloud.img
```

### Create Guest Disk (60 GB overlay)

```bash
qemu-img create -f qcow2 -b ubuntu-24.04-cloud.img -F qcow2 guest.qcow2 60G
```

### Extract Kernel and Initrd

Direct kernel boot is used because the embedded GRUB in AmdSev OVMF can't easily find the cloud image's kernel on partition 16.

```bash
modprobe nbd max_part=8
qemu-nbd -c /dev/nbd0 ubuntu-24.04-cloud.img
sleep 2

mkdir -p /mnt/boot
mount /dev/nbd0p16 /mnt/boot
cp /mnt/boot/vmlinuz-* ./vmlinuz
cp /mnt/boot/initrd.img-* ./initrd.img
umount /mnt/boot
qemu-nbd -d /dev/nbd0
```

### Create Cloud-Init Config

```bash
cat > meta-data << 'EOF'
instance-id: snp-guest-01
local-hostname: snp-guest
EOF

cat > user-data << 'EOF'
#cloud-config
password: snpguest
chpasswd: { expire: False }
ssh_pwauth: True
ssh_authorized_keys:
  - <YOUR_SSH_PUBLIC_KEY_HERE>
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
```

### Copy OVMF

```bash
cp /root/edk2/Build/AmdSev/DEBUG_GCC5/FV/OVMF.fd ./OVMF_AMDSEV.fd
```

---

## Step 5: Launch SEV-SNP Guest VM

```bash
cd /root/snp-vm

/usr/local/bin/qemu-system-x86_64 \
  -enable-kvm \
  -cpu EPYC-v4 \
  -machine q35,confidential-guest-support=sev0,memory-backend=ram1 \
  -object memory-backend-memfd,id=ram1,size=16G,share=true,prealloc=false \
  -object sev-snp-guest,id=sev0,cbitpos=51,reduced-phys-bits=1,kernel-hashes=on \
  -smp 8 \
  -bios /root/snp-vm/OVMF_AMDSEV.fd \
  -kernel /root/snp-vm/vmlinuz \
  -initrd /root/snp-vm/initrd.img \
  -append "root=/dev/sda1 console=ttyS0 earlyprintk=serial" \
  -drive file=/root/snp-vm/guest.qcow2,format=qcow2,if=none,id=disk0 \
  -device virtio-scsi-pci,id=scsi0,disable-legacy=on,iommu_platform=on \
  -device scsi-hd,drive=disk0 \
  -drive file=/root/snp-vm/cloud-init.iso,format=raw,if=none,id=cloud \
  -device scsi-cd,drive=cloud \
  -netdev user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080 \
  -device virtio-net-pci,netdev=net0,iommu_platform=on \
  -display none \
  -serial file:/root/snp-vm/console.log \
  -monitor unix:/root/snp-vm/monitor.sock,server,nowait \
  -daemonize
```

### QEMU Flags Explained

| Flag | Purpose |
|------|---------|
| `-cpu EPYC-v4` | Expose EPYC CPU features to guest |
| `-machine q35,confidential-guest-support=sev0,memory-backend=ram1` | Q35 machine with SEV-SNP support |
| `-object memory-backend-memfd,...,share=true` | Shared memory backend required for SEV |
| `-object sev-snp-guest,...,kernel-hashes=on` | Enable SEV-SNP with kernel measurement |
| `cbitpos=51,reduced-phys-bits=1` | AMD encryption bit position |
| `-bios OVMF_AMDSEV.fd` | Single-file AmdSev OVMF (no pflash) |
| `-kernel/-initrd/-append` | Direct kernel boot (bypasses GRUB) |
| `iommu_platform=on` | Required for SEV-SNP DMA protection |
| `disable-legacy=on` | Use modern virtio (required for SEV) |
| `hostfwd=tcp::2222-:22` | Forward host port 2222 to guest SSH |
| `hostfwd=tcp::8080-:8080` | Forward host port 8080 to MLNode API |

### Verify Boot

```bash
# Watch console output
tail -f /root/snp-vm/console.log

# Wait ~60s for cloud-init, then SSH in
ssh -p 2222 ubuntu@localhost
# Password: snpguest (or use your SSH key)
```

---

## Step 6: Verify SEV-SNP Inside Guest

### Check Encryption Active

```bash
sudo dmesg | grep -i "Memory Encryption"
# Memory Encryption Features active: AMD SEV SEV-ES SEV-SNP
```

### Load SEV Guest Module

```bash
sudo apt install -y linux-modules-extra-$(uname -r)
sudo modprobe sev-guest
ls -la /dev/sev-guest
# crw------- 1 root root 10, 261 ... /dev/sev-guest
```

### Install snpguest

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
source ~/.cargo/env
cargo install snpguest
```

### Verify Attestation

```bash
mkdir -p /tmp/attestation && cd /tmp/attestation
snpguest report report.bin request_data.txt --random
snpguest display report report.bin

# Fetch and verify AMD cert chain
mkdir -p certs
snpguest fetch vcek -p milan pem ./certs report.bin
snpguest fetch ca pem ./certs milan
snpguest verify certs ./certs
# The AMD ARK was self-signed!
# The AMD ASK was signed by the AMD ARK!
# The VCEK was signed by the AMD ASK!

snpguest verify attestation -p milan ./certs report.bin
# VEK signed the Attestation Report!
```

---

## Step 7: Install MLNode Stack (Stages 1-3)

### Install Miniconda

```bash
curl -fsSL https://repo.anaconda.com/miniconda/Miniconda3-py312_24.11.1-0-Linux-x86_64.sh -o /tmp/miniconda.sh
bash /tmp/miniconda.sh -b -p /root/miniconda3
rm /tmp/miniconda.sh
export PATH="/root/miniconda3/bin:$PATH"
```

### Install CUDA Toolkit

```bash
curl -fsSL https://developer.download.nvidia.com/compute/cuda/repos/ubuntu2404/x86_64/cuda-keyring_1.1-1_all.deb -o /tmp/cuda-keyring.deb
dpkg -i /tmp/cuda-keyring.deb
apt-get update -qq
apt-get install -y --no-install-recommends cuda-toolkit-12-8
```

### Install vLLM 0.15.1 (CPU Build)

```bash
export PATH="/root/miniconda3/bin:/usr/local/cuda/bin:$PATH"
pip install --no-cache-dir uv
uv pip install --python /root/miniconda3/bin/python3.12 'vllm==0.15.1'

# Uninstall GPU vLLM, install CPU PyTorch
pip uninstall -y vllm
pip install --no-cache-dir torch==2.9.1+cpu torchvision==0.24.1+cpu \
    --index-url https://download.pytorch.org/whl/cpu

# Build vLLM CPU from source
cd /tmp
git clone https://github.com/vllm-project/vllm.git vllm-build --branch v0.15.1 --depth 1
cd vllm-build
sed -i 's/torch==2.10.0/torch>=2.9.0/' pyproject.toml
apt-get install -y libnuma-dev

VLLM_TARGET_DEVICE=cpu python3.12 setup.py build_ext --inplace
VLLM_TARGET_DEVICE=cpu pip wheel --no-deps --no-build-isolation -w /tmp/vllm-wheels .
pip install --no-deps /tmp/vllm-wheels/vllm-*.whl

# Verify
python3.12 -c "from vllm.platforms import current_platform; print(f'Platform: {current_platform.device_type}')"
# Platform: cpu
```

### Clone gonka MLNode and Run Stages 2-3

```bash
cd /root
git clone https://github.com/kaitakuai/gonka.git --branch tee --depth 1

# Stage 2: PoC v2 overlay
cd /root/gonka/mlnode/packages/api
dos2unix stage2_poc_patch.sh stage3_mlnode.sh stage3_tee.sh 2>/dev/null
dos2unix patches/*.sh patches/*.patch 2>/dev/null
bash stage2_poc_patch.sh

# Stage 3: MLNode API
bash stage3_mlnode.sh

# Fix flash-attn (not needed on CPU)
pip uninstall -y flash-attn
```

---

## Step 8: Install TEE Layer (Stage 3.5)

```bash
cd /root/gonka/mlnode/packages/api
bash stage3_tee.sh
```

This installs:
- PyNaCl for encryption
- Verifies snpguest is available
- Loads `sev-guest` kernel module

TEE module (`tee/`) and `app.py` changes are already in the repo.

---

## Step 9: Start Confidential MLNode

```bash
export PATH="/root/miniconda3/bin:/root/.cargo/bin:/usr/sbin:/usr/bin:/sbin:/bin"
export PYTHONPATH="/app:/app/packages/api/src:/app/packages/pow/src:/app/packages/train/src:/app/packages/common/src"
export VLLM_TARGET_DEVICE=cpu
export VLLM_ENABLE_V1_MULTIPROCESSING=0
export VLLM_ATTENTION_BACKEND=TORCH_SDPA
export TEE_ENABLED=1

# Start vLLM backend
nohup python3.12 -m vllm.entrypoints.openai.api_server \
    --model Qwen/Qwen2.5-0.5B-Instruct \
    --dtype float32 \
    --max-model-len 512 \
    --enforce-eager \
    --host 127.0.0.1 \
    --port 5000 \
    > /tmp/vllm.log 2>&1 &

# Wait for vLLM to load model (~30-60s on CPU)
while ! curl -s http://127.0.0.1:5000/health > /dev/null 2>&1; do sleep 5; done
echo "vLLM ready"

# Start MLNode API with TEE
cd /app/packages/api/src
nohup python3.12 -m uvicorn api.app:app \
    --host 0.0.0.0 \
    --port 8080 \
    --log-level warning \
    > /tmp/mlnode.log 2>&1 &

sleep 10
curl -s http://127.0.0.1:8080/health
```

On startup, MLNode will:
1. Generate X25519 (encryption) and Ed25519 (signing) keypairs in memory
2. Request SNP attestation report with SHA-512(pubkeys) as report_data
3. Fetch and verify AMD cert chain (ARK → ASK → VCEK)
4. Serve `GET /attestation` and encrypted `POST /v1/chat/completions`

---

## Step 10: Test E2E Encrypted Inference

From the host (or any machine that can reach port 8080):

```bash
pip3 install pynacl httpx
python3 /root/gonka/mlnode/packages/api/client/tee_client.py --url http://127.0.0.1:8080 --prompt "What is TEE?"
```

Expected output:
```
=== 1. Fetch attestation ===
  enc pubkey:    89c6262240250a26...
  sign pubkey:   b7d4f126b3966180...
  certs valid:   True
  report valid:  True
  keys bound:    True

=== 2. Verify key binding ===
  MATCH: True

=== 3. Encrypted inference ===
  HTTP: 200

=== 4. Decrypt response ===
  response: TEE stands for ...

=== 5. Verify metadata signature ===
  signature: VALID
  Response hash bound: VALID
  Tokens: 27 prompt + 35 completion = 62 total

=== Done ===
```

---

## Verify Host Cannot Read VM Memory

Write a secret inside the guest, then try to find it from the host:

```bash
# Inside guest:
python3 -c "
import ctypes
secret = b'SUPER_SECRET_TEE_KEY_12345678'
buf = ctypes.create_string_buffer(secret, len(secret))
print(f'Secret in memory: {secret}')
import time; time.sleep(30)
"

# From host (in parallel):
QEMU_PID=$(pgrep -f "qemu.*guest.qcow2")
ADDR=$(cat /proc/$QEMU_PID/maps | grep "memfd:memory-backend-memfd" | head -1 | cut -d'-' -f1)

python3 -c "
import os
fd = os.open(f'/proc/$QEMU_PID/mem', os.O_RDONLY)
os.lseek(fd, 0x$ADDR, os.SEEK_SET)
data = os.read(fd, 16*1024*1024)
os.close(fd)
print(f'Found secret: {data.find(b\"SUPER_SECRET\") != -1}')
# Expected: False — memory is encrypted
"
```

---

## Troubleshooting

### "pflash with kvm requires KVM readonly memory support"

`KVM_CAP_READONLY_MEM` is 0 on KVM AMD. Use the AmdSev OVMF with `-bios` flag instead of split pflash CODE/VARS.

### "SEV information block/Firmware GUID Table block not found"

Stock OVMF doesn't have SEV GUID tables. Must use `OvmfPkg/AmdSev/AmdSevX64.dsc` build.

### "linuxefi.mod not found" during OVMF build

Ubuntu's grub merged `linuxefi` into `linux`. Remove `linuxefi` and `sevsecret` from `grub.sh` GRUB_MODULES.

### Guest boots to GRUB prompt, can't find kernel

Cloud images put the kernel on partition 16 (extended boot). Use direct kernel boot (`-kernel`/`-initrd`) instead of relying on embedded GRUB.

### /dev/sev-guest missing in guest

Install `linux-modules-extra-$(uname -r)` and run `modprobe sev-guest`.

### snpguest requires newer Rust

Ubuntu 24.04 ships Rust 1.75. snpguest 0.10 needs 1.86+. Install via `rustup`.

### vLLM V1 engine fails on CPU

Set `VLLM_ENABLE_V1_MULTIPROCESSING=0` — V1 multiprocessing doesn't work without GPU. Also uninstall `flash-attn` (requires CUDA).

### vLLM "torch==2.10.0+cpu not found"

Patch `pyproject.toml` in vLLM source: `sed -i 's/torch==2.10.0/torch>=2.9.0/'`

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│ Host (Ubuntu 24.04, EPYC 7443P)                                 │
│                                                                  │
│  QEMU 9.2.3 + KVM                                               │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ SEV-SNP Encrypted VM (16 GB, 8 vCPU)                      │  │
│  │                                                            │  │
│  │  vLLM 0.15.1+cpu (:5000, localhost only)                   │  │
│  │       ↑                                                    │  │
│  │  MLNode API (:8080, TEE_ENABLED=1)                         │  │
│  │  ├── GET /attestation                                      │  │
│  │  │   → pubkeys + SNP report + AMD certs + VM metadata      │  │
│  │  └── POST /v1/chat/completions (encrypted-only)            │  │
│  │      → decrypt NaCl box → vLLM → encrypt → sign metadata   │  │
│  │                                                            │  │
│  │  TEE Keys (memory only, never on disk):                    │  │
│  │  ├── X25519  → encrypt/decrypt requests and responses      │  │
│  │  └── Ed25519 → sign metadata (token usage)                 │  │
│  │                                                            │  │
│  │  All memory encrypted with per-VM key                      │  │
│  │  Host CANNOT read VM memory                                │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
│  AmdSev OVMF (firmware with SEV GUID tables)                     │
│  /dev/sev (host SEV device)                                      │
│  RMP Table (Reverse Map Table for memory protection)             │
└──────────────────────────────────────────────────────────────────┘
         │
         │ Client verifies attestation, then sends encrypted requests
         ▼
┌─────────────────────────────────┐
│ Client                          │
│  1. GET /attestation            │
│  2. Verify AMD chain + keys     │
│  3. Encrypt request (NaCl box)  │
│  4. POST /v1/chat/completions   │
│  5. Decrypt response            │
│  6. Verify metadata signature   │
└─────────────────────────────────┘
```

---

## Security Properties

| Property | How it's enforced |
|----------|-------------------|
| **Memory encryption** | AMD SEV-SNP hardware — per-VM AES key, host reads zeros |
| **E2E encryption** | NaCl box (X25519 + XSalsa20-Poly1305) |
| **Key binding** | SHA-512(pubkeys) embedded in SNP report_data (signed by VCEK) |
| **No plaintext on network** | ProxyMiddleware disabled, only encrypted endpoint active |
| **No keys on disk** | Private keys in Python objects only, tmpfs verified at startup |
| **No sensitive logs** | Prompts/responses never logged, only model name and token counts |
| **Metadata integrity** | Ed25519 signature over metadata + SHA-256(ciphertext) binding |
| **Attestation** | SNP v3 report signed by AMD VCEK, verified via AMD KDS |
| **Tamper evidence** | Measurement in SNP report changes if VM image modified |
