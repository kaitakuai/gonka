# Confidential MLNode — Quick Deploy

Deploy a Confidential MLNode with AMD SEV-SNP in 4 commands.

For the full step-by-step guide, see [sev-snp-confidential-vm.md](sev-snp-confidential-vm.md).

**Tested on:** 2026-04-02
**Host:** Ubuntu 24.04 LTS, kernel 6.14.0-37-generic
**CPU:** AMD EPYC 7443P 24-Core (Milan, SP3)
**Code:** [kaitakuai/gonka tee](https://github.com/kaitakuai/gonka/tree/tee/mlnode)

---

## Prerequisites

- AMD EPYC 7003 (Milan) or newer on SP3/SP5 socket
- BIOS: SVM, SMEE, SNP Memory Coverage, IOMMU, SEV-SNP — all enabled
- 64 GB RAM, 60+ GB disk
- Root access

---

## Deploy

### 1. Host setup (~15 min)

Installs packages, builds QEMU 9.2 + OVMF AmdSev, prepares VM image.

```bash
git clone https://github.com/kaitakuai/gonka.git --branch tee --depth 1
cd gonka/mlnode/vm/scripts

bash host/setup.sh
```

### 2. Launch VM (~1 min)

Starts SEV-SNP encrypted VM and waits for SSH.

```bash
bash host/launch.sh
```

### 3. Guest setup (~10 min)

SSH into VM and install everything: verifies SEV-SNP, installs snpguest, clones gonka, builds vLLM CPU.

```bash
ssh -p 2222 ubuntu@localhost
# Inside VM:
sudo bash /root/gonka/mlnode/vm/scripts/guest/setup.sh
```

### 4. Start + test (~2 min)

Starts vLLM + MLNode with TEE attestation, runs E2E encrypted inference test.

```bash
sudo bash /root/gonka/mlnode/vm/scripts/guest/start.sh test
```

Expected output:
```
=== Attestation Verification ===
  TEE type: amd-sev-snp
  Certificate chain:              PASS
  Report signature:               PASS
  Keys bound to TEE:              PASS
  Debug disabled:                 PASS
  OVERALL: TRUSTED

=== Encrypted Inference ===
  Metadata signature: VALID
  Response hash bound: VALID

=== Done ===
```

---

## Operations

```bash
# Check status
sudo bash guest/start.sh status

# Stop
sudo bash guest/start.sh stop

# Start without test
sudo bash guest/start.sh

# Stop VM (from host)
bash host/launch.sh stop

# VM status (from host)
bash host/launch.sh status
```

---

## Re-running after failure

All scripts are **idempotent** — completed steps are skipped on re-run.

```bash
# Just re-run the same command — it picks up where it left off
sudo bash guest/setup.sh

# Force redo all steps
sudo FORCE=1 bash guest/setup.sh
```

Checkpoint files: `/tmp/.tee/host/setup.progress`, `/tmp/.tee/guest/setup.progress`.

---

## Script reference

| Script | Runs on | What it does |
|--------|---------|-------------|
| `host/setup.sh` | Host (root) | Packages, QEMU 9.2, OVMF AmdSev, VM image |
| `host/launch.sh` | Host (root) | Launch / stop / status SEV-SNP VM |
| `guest/setup.sh` | VM (sudo) | SEV verify, snpguest, gonka, venv, vLLM CPU |
| `guest/start.sh` | VM (sudo) | vLLM + MLNode start / stop / status / test |
