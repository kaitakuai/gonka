# Milestone 1: TEE Infrastructure & Prototype — Summary

> Project: [Gonka TEE Proposal #951](https://github.com/gonka-ai/gonka/discussions/951)
> Team: [@baychak](https://github.com/baychak), [@clanster](https://github.com/clanster), [@gmorgachev](https://github.com/gmorgachev), [@tamazgadaev](https://github.com/tamazgadaev)

**Status:** Complete (all acceptance criteria met)

## What was delivered

A working end-to-end encrypted inference prototype running inside a hardware-isolated VM (AMD SEV-SNP). Prompts and responses are cryptographically protected from the host operator — proving that Confidential MLNode is technically feasible.

## Results

### 1. CC-compatible bare-metal host provisioned

- Tested 5 configurations across 3 providers (Hyperbolic, Cherry Servers, Hetzner) — 4 failed (locked BIOS, desktop CPUs, incompatible AM5 socket)
- Working setup: Cherry Servers, AMD EPYC 7443P (Milan), SEV-SNP enabled via BMC/IPMI
- Documented hardware compatibility matrix: CPU (AMD EPYC Milan+ / Intel Xeon 4th+), GPU (H100, B200, RTX PRO 6000 SE)

### 2. SEV-SNP guest VM with attestation

- Built QEMU 9.2.3 and OVMF AmdSev firmware from source (stock packages lack `sev-snp-guest` support)
- Launched VM with full memory encryption (AES-128-XTS, hardware-enforced)
- Generated and verified AMD attestation report with full certificate chain (ARK → ASK → VCEK)
- Wrote CC host setup and SEV-SNP VM launch runbook

### 3. TEE module + encrypted inference

- Ephemeral keys (X25519 + Ed25519) generated in RAM only, bound to attestation via SHA-512 in SNP report_data
- `GET /attestation` — attestation certificate with public keys, SNP report, and AMD cert chain
- `POST /v1/chat/completions` — E2E encrypted inference (NaCl box: X25519 + XSalsa20-Poly1305)
- Signed metadata (token usage) for escrow claims (Ed25519)
- vLLM running inside SEV-SNP VM (CPU mode; GPU CC deferred to Milestone 4)
- Basic test client for full-cycle E2E verification

### 4. Universal TEE backend (ahead of schedule — originally Milestone 2 scope, partially implemented)

- Abstract `TEEBackend` interface for AMD SEV-SNP and Intel TDX
- Auto-detection at startup (`/dev/sev-guest` or `/dev/tdx_guest`); no TEE = hard fail
- Intel TDX backend implemented, awaiting hardware testing
- 24 unit tests + 3 integration tests on real hardware

## Verified security guarantees

| Guarantee | How verified |
|-----------|-------------|
| Host cannot read VM memory | `/proc/PID/mem` test — returns ciphertext only |
| Prompts and responses do not leak | Do not appear in logs, on disk, or in network traffic |
| No unencrypted channels | No open TCP ports for plaintext data (ProxyMiddleware disabled) |
| Keys are ephemeral | Exist only in RAM, lost on VM restart |

## Architecture

```
Client → encrypt (NaCl box) → Network Node (passthrough) → TEE VM → decrypt → vLLM → encrypt → Client
                                                             ↑
                                               AMD SEV-SNP (memory encryption)
                                               Attestation (ARK → ASK → VCEK → Report)
                                               Ephemeral keys (X25519 + Ed25519)
```

## Next

**Milestone 2: Security & Cross-platform** — client-side attestation verification (zero trust: client validates the TEE is real without trusting the server) and Intel TDX backend testing on real hardware.
