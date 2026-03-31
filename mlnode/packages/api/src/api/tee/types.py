"""
TEE Types — Shared data structures for TEE detection and attestation.

Used by detect.py, attestation.py, routes.py, app_tee.py.
"""

from dataclasses import dataclass, field
from enum import Enum
from typing import Optional


class TEEType(str, Enum):
    """CPU TEE platform type."""
    AMD_SEV_SNP = "amd-sev-snp"
    INTEL_TDX = "intel-tdx"


class GPUCCMode(str, Enum):
    """NVIDIA GPU Confidential Computing mode."""
    SPT = "spt"  # Single GPU Passthrough (H100, RTX PRO 6000 SE)
    MPT = "mpt"  # Multi-GPU Passthrough (B200)


@dataclass
class GPUCCInfo:
    """GPU Confidential Computing status."""
    mode: GPUCCMode
    gpu_name: str          # e.g. "NVIDIA H100 SXM", "NVIDIA RTX PRO 6000"
    driver_version: str    # e.g. "535.129.03"


@dataclass
class TEEInfo:
    """
    Complete TEE environment description.

    cpu_tee is always set (no TEE = process exits before TEEInfo is created).
    gpu_cc is None when no CC-capable GPU is present or CC mode is not enabled.
    """
    cpu_tee: TEEType
    device_path: str                       # "/dev/sev-guest" or "/dev/tdx_guest"
    gpu_cc: Optional[GPUCCInfo] = None
    warnings: list[str] = field(default_factory=list)
