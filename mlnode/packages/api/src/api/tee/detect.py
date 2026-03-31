"""
TEE Detection — Auto-detect CPU TEE and GPU CC inside guest VM.

Called at MLNode startup before key generation or attestation.
If no CPU TEE is found, the process exits immediately.

Detection order:
  1. CPU TEE (mandatory): /dev/sev-guest (AMD SEV-SNP) → /dev/tdx_guest (Intel TDX)
  2. GPU CC (optional): nvidia-smi conf-compute
  3. Compatibility validation (warnings for known problematic combos)
"""

import shutil
import subprocess
import sys

from common.logger import create_logger

from .types import GPUCCInfo, GPUCCMode, TEEInfo, TEEType

logger = create_logger(__name__)

# CPU TEE devices to probe, in priority order.
# AMD first: proven NVIDIA CC path, our primary hardware.
_CPU_TEE_DEVICES = [
    ("/dev/sev-guest", TEEType.AMD_SEV_SNP),
    ("/dev/tdx_guest", TEEType.INTEL_TDX),
]


def _check_device(path: str) -> bool:
    """Check that a device file exists and can be opened."""
    try:
        fd = open(path, "rb")
        fd.close()
        return True
    except FileNotFoundError:
        return False
    except PermissionError:
        logger.warning(f"{path} exists but permission denied — run as root?")
        return False
    except OSError as e:
        logger.warning(f"{path} exists but open failed: {e}")
        return False


def _check_sysfs(tee_type: TEEType) -> bool:
    """Non-blocking kernel module confirmation via sysfs."""
    sysfs_paths = {
        TEEType.AMD_SEV_SNP: "/sys/module/sev_guest",
        TEEType.INTEL_TDX: "/sys/module/tdx_guest",
    }
    import os
    path = sysfs_paths.get(tee_type)
    if path and os.path.isdir(path):
        return True
    return False


def _detect_cpu_tee() -> tuple[TEEType, str] | None:
    """
    Detect CPU TEE by probing device files.

    Returns (TEEType, device_path) or None if no TEE found.
    """
    for device_path, tee_type in _CPU_TEE_DEVICES:
        if _check_device(device_path):
            sysfs_ok = _check_sysfs(tee_type)
            logger.info(
                f"CPU TEE detected: {tee_type.value} "
                f"(device={device_path}, sysfs={'confirmed' if sysfs_ok else 'not found (non-critical)'})"
            )
            return tee_type, device_path
        else:
            logger.debug(f"Checked {device_path} — not available")

    return None


def _detect_gpu_cc() -> GPUCCInfo | None:
    """
    Detect NVIDIA GPU Confidential Computing mode.

    Uses nvidia-smi conf-compute to check if CC is active.
    Returns GPUCCInfo or None.
    """
    nvidia_smi = shutil.which("nvidia-smi")
    if nvidia_smi is None:
        logger.info("No NVIDIA GPU detected (nvidia-smi not in PATH)")
        return None

    # Check CC mode
    try:
        r = subprocess.run(
            [nvidia_smi, "conf-compute", "-grs"],
            capture_output=True, text=True, timeout=10,
        )
    except subprocess.TimeoutExpired:
        logger.warning("nvidia-smi conf-compute timed out")
        return None
    except OSError as e:
        logger.warning(f"nvidia-smi failed: {e}")
        return None

    if r.returncode != 0:
        # GPU present but conf-compute not supported (older driver or non-CC GPU)
        logger.info("GPU present but CC not supported by driver/hardware")
        return None

    stdout = r.stdout.strip().lower()

    # nvidia-smi conf-compute -grs outputs something like:
    #   "CC status: ON" or "CC status: OFF" or "DevTools mode: ON"
    if "on" not in stdout:
        logger.warning(
            f"GPU present but CC mode not enabled. "
            f"Enable with: nvidia-smi conf-compute --set-cc-mode on"
        )
        return None

    # Determine CC mode (SPT vs MPT)
    mode = GPUCCMode.SPT
    if "mpt" in stdout or "multi" in stdout:
        mode = GPUCCMode.MPT

    # Get GPU name and driver version
    gpu_name = "unknown"
    driver_version = "unknown"
    try:
        q = subprocess.run(
            [nvidia_smi, "--query-gpu=gpu_name,driver_version", "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10,
        )
        if q.returncode == 0 and q.stdout.strip():
            parts = q.stdout.strip().split(",", 1)
            gpu_name = parts[0].strip()
            if len(parts) > 1:
                driver_version = parts[1].strip()
    except (subprocess.TimeoutExpired, OSError):
        pass

    logger.info(f"GPU CC detected: {gpu_name}, mode={mode.value}, driver={driver_version}")
    return GPUCCInfo(mode=mode, gpu_name=gpu_name, driver_version=driver_version)


def _validate_compatibility(cpu_tee: TEEType, gpu_cc: GPUCCInfo | None) -> list[str]:
    """Check for known problematic CPU TEE + GPU CC combinations."""
    warnings = []

    if gpu_cc is not None and cpu_tee == TEEType.INTEL_TDX:
        warnings.append(
            "Intel TDX + GPU CC requires TDX Connect (Granite Rapids+ / Xeon 6). "
            "On older Intel CPUs, GPU DMA bypasses TDX memory encryption. "
            "Verify your CPU supports TDX Connect before trusting GPU CC attestation."
        )

    return warnings


def detect_tee() -> TEEInfo:
    """
    Detect TEE environment. Called once at MLNode startup.

    Returns TEEInfo with CPU TEE type and optional GPU CC info.
    Exits the process if no CPU TEE is detected.
    """
    logger.info("Detecting TEE environment...")

    # Step 1: CPU TEE (mandatory)
    result = _detect_cpu_tee()
    if result is None:
        logger.critical(
            "No TEE hardware detected. "
            "Confidential MLNode requires AMD SEV-SNP or Intel TDX.\n"
            "  Checked /dev/sev-guest — not available\n"
            "  Checked /dev/tdx_guest — not available"
        )
        sys.exit(1)

    cpu_tee, device_path = result

    # Step 2: GPU CC (optional)
    gpu_cc = _detect_gpu_cc()

    # Step 3: Compatibility validation
    warnings = _validate_compatibility(cpu_tee, gpu_cc)
    for w in warnings:
        logger.warning(w)

    info = TEEInfo(
        cpu_tee=cpu_tee,
        device_path=device_path,
        gpu_cc=gpu_cc,
        warnings=warnings,
    )

    logger.info(
        f"TEE environment: cpu={info.cpu_tee.value}, "
        f"gpu_cc={'yes (' + info.gpu_cc.gpu_name + ')' if info.gpu_cc else 'none'}"
    )

    return info
