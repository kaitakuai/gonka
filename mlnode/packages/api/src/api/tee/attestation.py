"""
TEE Attestation — Unified dispatcher that delegates to the correct backend.

Refactored in Phase 5.6: attestation logic moved to backends/,
this module is now a thin orchestration layer.
"""

import os
import platform

from common.logger import create_logger

from .types import TEEInfo, TEEType
from .backends.base import TEEBackend
from .backends.amd_sev_snp import AmdSevSnpBackend
from .backends.intel_tdx import IntelTdxBackend

logger = create_logger(__name__)


def _get_backend(tee_info: TEEInfo) -> TEEBackend:
    """Select attestation backend based on detected TEE hardware."""
    if tee_info.cpu_tee == TEEType.AMD_SEV_SNP:
        return AmdSevSnpBackend()
    elif tee_info.cpu_tee == TEEType.INTEL_TDX:
        return IntelTdxBackend()
    else:
        raise ValueError(f"Unsupported TEE type: {tee_info.cpu_tee}")


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


def generate_attestation(keys, tee_info: TEEInfo, image_hash: str = None) -> dict:
    """
    Generate full attestation bundle per proposal #951.

    Args:
        keys: TEEKeyManager instance
        tee_info: Detected TEE environment from detect_tee()
        image_hash: SHA-256 of the base VM image

    Returns:
        Attestation bundle dict with all fields needed for remote verification.
    """
    backend = _get_backend(tee_info)
    logger.info(f"Generating attestation using {backend.tee_type()} backend")

    # Step 1: Generate hardware attestation report
    report_data = keys.compute_report_data()
    report_bytes = backend.generate_report(report_data)

    # Step 2: Fetch certificate chain
    certs = backend.fetch_certs()

    # Step 3: Verify
    certs_valid = backend.verify_certs()
    report_valid = backend.verify_report()
    logger.debug(f"Attestation verification: certs={certs_valid}, report={report_valid}")

    # Step 4: Parse report
    parsed_report = backend.parse_report(report_bytes)

    # Step 5: Check keys are bound
    keys_bound = (report_data.hex() == parsed_report.get("report_data", ""))

    # Step 6: Collect VM metadata
    vm_meta = _collect_vm_metadata()
    if image_hash:
        vm_meta["image_hash"] = image_hash

    # Step 7: Build GPU CC info
    gpu_info = None
    if tee_info.gpu_cc:
        gpu_info = {
            "mode": tee_info.gpu_cc.mode.value,
            "gpu_name": tee_info.gpu_cc.gpu_name,
            "driver_version": tee_info.gpu_cc.driver_version,
        }

    # Build attestation bundle — structure is TEE-agnostic
    bundle = {
        # --- TEE type ---
        "tee_type": backend.tee_type(),

        # --- Keys (proposal: "Public key from generated key pair") ---
        "encryption_pubkey": keys.enc_public_hex,
        "signing_pubkey": keys.sign_public_hex,

        # --- Attestation Report ---
        "report": {
            "report_hex": report_bytes.hex(),
            "report_data": parsed_report.get("report_data"),
            "measurement": parsed_report.get("measurement"),
            "parsed": parsed_report,
        },

        # --- Hardware ---
        "hardware": _build_hardware_info(backend, parsed_report, tee_info),

        # --- GPU (proposal: "GPU information, CC enabled") ---
        "gpu": gpu_info,

        # --- VM Image (proposal: "Exact VM metadata") ---
        "vm_image": {
            "measurement": parsed_report.get("measurement"),
            "os": vm_meta["os"],
            "kernel": vm_meta["kernel"],
            "arch": vm_meta["arch"],
            "vllm_version": vm_meta["vllm_version"],
            "image_hash": vm_meta.get("image_hash"),
        },

        # --- Certificate Chain ---
        "certs": certs,

    }

    # Verification object: debug-only per spec §3.1.6.
    # Client MUST NOT use these flags for trust decisions (ADR-0010).
    if os.getenv("TEE_DEBUG_ATTESTATION", "0") == "1":
        bundle["verification"] = {
            "certs_valid": certs_valid,
            "report_valid": report_valid,
            "keys_bound": keys_bound,
        }

    # Warnings from detection
    if tee_info.warnings:
        bundle["warnings"] = tee_info.warnings

    return bundle


def _build_hardware_info(backend: TEEBackend, parsed: dict, tee_info: TEEInfo) -> dict:
    """Build hardware info section, platform-specific."""
    if backend.tee_type() == "amd-sev-snp":
        return {
            "tee_type": "amd-sev-snp",
            "cpu_family": parsed.get("cpu", {}).get("family"),
            "cpu_model": parsed.get("cpu", {}).get("model"),
            "cpu_stepping": parsed.get("cpu", {}).get("stepping"),
            "chip_id": parsed.get("chip_id"),
            "platform_info": parsed.get("platform_info"),
            "tcb": parsed.get("tcb"),
            "sev_version": parsed.get("sev_version"),
        }
    elif backend.tee_type() == "intel-tdx":
        return {
            "tee_type": "intel-tdx",
            "mr_seam": parsed.get("mr_seam"),
            "mr_signer_seam": parsed.get("mr_signer_seam"),
            "td_attributes": parsed.get("td_attributes"),
            "xfam": parsed.get("xfam"),
            "tee_tcb_svn": parsed.get("tee_tcb_svn"),
            "qe_svn": parsed.get("qe_svn"),
            "pce_svn": parsed.get("pce_svn"),
        }
    return {"tee_type": backend.tee_type()}
