"""
TEE Attestation — SNP report generation and AMD KDS verification.

Generates attestation certificate per proposal #951:
  "VM provides an attestation certificate signed by the CPU's hardware module.
   The signed data includes: Hardware information, GPU information,
   Exact VM metadata, Public key from generated key pair,
   Proof that VM launched from correct entrypoint."
"""

import hashlib
import json
import os
import platform
import struct
import subprocess
from pathlib import Path

from common.logger import create_logger

logger = create_logger(__name__)

ATTESTATION_DIR = Path("/run/tee-attestation")
SNPGUEST = os.getenv("SNPGUEST_PATH", "/root/.cargo/bin/snpguest")

# Processor model for AMD KDS cert fetch
PROCESSOR_MODEL = os.getenv("TEE_PROCESSOR_MODEL", "milan")


def _run(cmd: list, **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, **kwargs)


def _parse_snp_report(report_bytes: bytes) -> dict:
    """Parse binary SNP attestation report (v3, 1184 bytes) into structured dict."""
    if len(report_bytes) < 672:
        return {"raw_hex": report_bytes.hex(), "parse_error": "report too short"}

    # SNP report v3 layout (AMD SEV-SNP ABI spec, Table 22)
    version = struct.unpack_from("<I", report_bytes, 0x00)[0]
    guest_svn = struct.unpack_from("<I", report_bytes, 0x04)[0]
    policy = struct.unpack_from("<Q", report_bytes, 0x08)[0]
    family_id = report_bytes[0x10:0x20].hex()
    image_id = report_bytes[0x20:0x30].hex()
    vmpl = struct.unpack_from("<I", report_bytes, 0x30)[0]
    sig_algo = struct.unpack_from("<I", report_bytes, 0x34)[0]
    # Current TCB at offset 0x38 (8 bytes)
    tcb_raw = struct.unpack_from("<Q", report_bytes, 0x38)[0]
    platform_info = struct.unpack_from("<Q", report_bytes, 0x40)[0]
    # Report data at 0x50 (64 bytes)
    report_data = report_bytes[0x50:0x90].hex()
    # Measurement at 0x90 (48 bytes)
    measurement = report_bytes[0x90:0xC0].hex()
    # Host data at 0xC0 (32 bytes)
    host_data = report_bytes[0xC0:0xE0].hex()
    # ID key digest at 0xE0 (48 bytes)
    # Author key digest at 0x110 (48 bytes)
    # Report ID at 0x140 (32 bytes)
    report_id = report_bytes[0x140:0x160].hex()
    # Chip ID at 0x1A0 (64 bytes)
    chip_id = report_bytes[0x1A0:0x1E0].hex()
    # Committed TCB at 0x1E0 (8 bytes)
    committed_tcb = struct.unpack_from("<Q", report_bytes, 0x1E0)[0]
    # Current build/minor/major at 0x1E8
    current_build = report_bytes[0x1E8]
    current_minor = report_bytes[0x1E9]
    current_major = report_bytes[0x1EA]
    # Committed build/minor/major at 0x1EB
    committed_build = report_bytes[0x1EB]
    committed_minor = report_bytes[0x1EC]
    committed_major = report_bytes[0x1ED]
    # Launch TCB at 0x1F0 (8 bytes)
    launch_tcb = struct.unpack_from("<Q", report_bytes, 0x1F0)[0]
    # CPUID info
    # At offset 0x198
    cpuid_fam = report_bytes[0x198] if len(report_bytes) > 0x198 else 0
    cpuid_mod = report_bytes[0x199] if len(report_bytes) > 0x199 else 0
    cpuid_step = report_bytes[0x19A] if len(report_bytes) > 0x19A else 0

    def parse_tcb(raw: int) -> dict:
        return {
            "bootloader": raw & 0xFF,
            "tee": (raw >> 8) & 0xFF,
            "snp": (raw >> 48) & 0xFF,
            "microcode": (raw >> 56) & 0xFF,
        }

    return {
        "version": version,
        "guest_svn": guest_svn,
        "vmpl": vmpl,
        "signature_algo": sig_algo,
        "report_data": report_data,
        "measurement": measurement,
        "host_data": host_data,
        "report_id": report_id,
        "chip_id": chip_id,
        "family_id": family_id,
        "image_id": image_id,
        "policy": {
            "raw": hex(policy),
            "debug_allowed": bool(policy & (1 << 19)),
            "migrate_ma": bool(policy & (1 << 18)),
            "smt_allowed": bool(policy & (1 << 16)),
            "single_socket": bool(policy & (1 << 20)),
        },
        "platform_info": {
            "raw": hex(platform_info),
            "smt_enabled": bool(platform_info & 1),
            "tsme_enabled": bool(platform_info & 2),
            "ecc_enabled": bool(platform_info & 4),
        },
        "tcb": {
            "current": parse_tcb(tcb_raw),
            "committed": parse_tcb(committed_tcb),
            "launch": parse_tcb(launch_tcb),
        },
        "cpu": {
            "family": cpuid_fam,
            "model": cpuid_mod,
            "stepping": cpuid_step,
        },
        "sev_version": {
            "current": f"{current_major}.{current_minor}.{current_build}",
            "committed": f"{committed_major}.{committed_minor}.{committed_build}",
        },
    }


def _collect_vm_metadata() -> dict:
    """Collect non-sensitive VM metadata for attestation bundle."""
    uname = platform.uname()

    vllm_version = "unknown"
    try:
        import vllm
        vllm_version = vllm.__version__
    except Exception:
        pass

    return {
        "os": f"{platform.freedesktop_os_release().get('PRETTY_NAME', 'unknown')}",
        "kernel": uname.release,
        "arch": uname.machine,
        "vllm_version": vllm_version,
    }


def generate_attestation(keys, image_hash: str = None) -> dict:
    """
    Generate full attestation bundle per proposal #951.

    Args:
        keys: TEEKeyManager instance
        image_hash: SHA-256 of the base VM image (set by host at launch,
                    or computed inside VM). Published on-chain.

    Returns:
        Attestation bundle dict with all fields needed for remote verification.
    """
    ATTESTATION_DIR.mkdir(mode=0o755, exist_ok=True)
    certs_dir = ATTESTATION_DIR / "certs"
    certs_dir.mkdir(mode=0o755, exist_ok=True)

    report_data = keys.compute_report_data()
    report_data_path = ATTESTATION_DIR / "report_data.bin"
    report_path = ATTESTATION_DIR / "report.bin"

    # Write report_data (SHA-512 of both pubkeys)
    report_data_path.write_bytes(report_data)

    # Request SNP attestation report from hardware
    r = _run([SNPGUEST, "report", str(report_path), str(report_data_path)])
    if r.returncode != 0:
        logger.error(f"SNP report failed: {r.stderr}")
        raise RuntimeError(f"Failed to generate attestation report: {r.stderr}")

    report_bytes = report_path.read_bytes()
    logger.info(f"SNP report generated ({len(report_bytes)} bytes)")

    # Fetch VCEK from AMD KDS
    _run([SNPGUEST, "fetch", "vcek", "-p", PROCESSOR_MODEL, "pem",
          str(certs_dir), str(report_path)])

    # Fetch CA chain (ARK + ASK)
    _run([SNPGUEST, "fetch", "ca", "pem", str(certs_dir), PROCESSOR_MODEL])

    # Verify cert chain
    vc = _run([SNPGUEST, "verify", "certs", str(certs_dir)])
    vr = _run([SNPGUEST, "verify", "attestation", "-p", PROCESSOR_MODEL,
               str(certs_dir), str(report_path)])

    certs_valid = vc.returncode == 0
    report_valid = vr.returncode == 0

    logger.info(f"Attestation verification: certs={certs_valid}, report={report_valid}")

    # Parse report
    parsed_report = _parse_snp_report(report_bytes)

    # Verify keys are bound
    keys_bound = (report_data.hex() == parsed_report.get("report_data", ""))

    # Read certs
    def _read_pem(name):
        p = certs_dir / name
        return p.read_text() if p.exists() else None

    # Collect VM metadata
    vm_meta = _collect_vm_metadata()
    if image_hash:
        vm_meta["image_hash"] = image_hash

    return {
        # --- Keys (proposal: "Public key from generated key pair") ---
        "encryption_pubkey": keys.enc_public_hex,
        "signing_pubkey": keys.sign_public_hex,

        # --- SNP Report (proposal: "attestation certificate signed by CPU") ---
        "snp_report": {
            "version": parsed_report["version"],
            "report_hex": report_bytes.hex(),
            "report_data": parsed_report["report_data"],
            "measurement": parsed_report["measurement"],
            "host_data": parsed_report["host_data"],
            "report_id": parsed_report["report_id"],
            "vmpl": parsed_report["vmpl"],
            "guest_svn": parsed_report["guest_svn"],
            "policy": parsed_report["policy"],
        },

        # --- Hardware (proposal: "Hardware information") ---
        "hardware": {
            "cpu_family": parsed_report["cpu"]["family"],
            "cpu_model": parsed_report["cpu"]["model"],
            "cpu_stepping": parsed_report["cpu"]["stepping"],
            "chip_id": parsed_report["chip_id"],
            "platform_info": parsed_report["platform_info"],
            "tcb": parsed_report["tcb"],
            "sev_version": parsed_report["sev_version"],
        },

        # --- GPU (proposal: "GPU information, CC enabled") ---
        # null until GPU CC hardware is available (H100/RTX PRO 6000 SE)
        "gpu": None,

        # --- VM Image (proposal: "Exact VM metadata") ---
        "vm_image": {
            "measurement": parsed_report["measurement"],
            "os": vm_meta["os"],
            "kernel": vm_meta["kernel"],
            "arch": vm_meta["arch"],
            "vllm_version": vm_meta["vllm_version"],
            "image_hash": vm_meta.get("image_hash"),
        },

        # --- AMD Certificate Chain ---
        "certs": {
            "vcek_pem": _read_pem("vcek.pem"),
            "ask_pem": _read_pem("ask.pem"),
            "ark_pem": _read_pem("ark.pem"),
        },

        # --- Verification status ---
        "verification": {
            "certs_valid": certs_valid,
            "report_valid": report_valid,
            "keys_bound": keys_bound,
        },
    }
