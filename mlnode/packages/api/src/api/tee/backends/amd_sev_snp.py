"""
AMD SEV-SNP Backend — Attestation via snpguest CLI + AMD KDS.

Extracted from tee/attestation.py (Phase 5.2.1).

Certificate chain: AMD Root Key (ARK) → AMD SEV Key (ASK) → VCEK (per-chip).
Report: SNP attestation report v3, 1184 bytes.
CLI tool: snpguest (Rust, installed via cargo).
"""

import os
import struct
import subprocess
from pathlib import Path

from common.logger import create_logger

from .base import TEEBackend

logger = create_logger(__name__)

SNPGUEST = os.getenv("SNPGUEST_PATH", "/root/.cargo/bin/snpguest")
PROCESSOR_MODEL = os.getenv("TEE_PROCESSOR_MODEL", "milan")


def _run(cmd: list, **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, text=True, **kwargs)


class AmdSevSnpBackend(TEEBackend):
    """AMD SEV-SNP attestation backend using snpguest CLI."""

    def __init__(self, attestation_dir: Path | None = None):
        super().__init__(attestation_dir or Path("/run/tee-attestation"))
        self.certs_dir = self.attestation_dir / "certs"
        self.certs_dir.mkdir(mode=0o755, exist_ok=True)
        self.report_path = self.attestation_dir / "report.bin"
        self.report_data_path = self.attestation_dir / "report_data.bin"
        self._report_bytes: bytes | None = None

    def tee_type(self) -> str:
        return "amd-sev-snp"

    def generate_report(self, report_data: bytes) -> bytes:
        self.report_data_path.write_bytes(report_data)
        r = _run([SNPGUEST, "report", str(self.report_path), str(self.report_data_path)])
        if r.returncode != 0:
            raise RuntimeError(f"snpguest report failed: {r.stderr}")
        self._report_bytes = self.report_path.read_bytes()
        logger.info(f"SNP report generated ({len(self._report_bytes)} bytes)")
        return self._report_bytes

    def fetch_certs(self) -> dict:
        # Fetch VCEK from AMD KDS
        _run([SNPGUEST, "fetch", "vcek", "-p", PROCESSOR_MODEL, "pem",
              str(self.certs_dir), str(self.report_path)])
        # Fetch CA chain (ARK + ASK)
        _run([SNPGUEST, "fetch", "ca", "pem", str(self.certs_dir), PROCESSOR_MODEL])

        certs = {}
        for name in ("vcek", "ask", "ark"):
            p = self.certs_dir / f"{name}.pem"
            certs[name] = p.read_text() if p.exists() else None
        return certs

    def verify_certs(self) -> bool:
        r = _run([SNPGUEST, "verify", "certs", str(self.certs_dir)])
        ok = r.returncode == 0
        logger.info(f"AMD cert chain verification: {'ok' if ok else 'FAILED'}")
        return ok

    def verify_report(self) -> bool:
        r = _run([SNPGUEST, "verify", "attestation", "-p", PROCESSOR_MODEL,
                   str(self.certs_dir), str(self.report_path)])
        ok = r.returncode == 0
        logger.info(f"SNP report verification: {'ok' if ok else 'FAILED'}")
        return ok

    def parse_report(self, raw: bytes) -> dict:
        if len(raw) < 672:
            return {"raw_hex": raw.hex(), "parse_error": "report too short"}

        # SNP report v3 layout (AMD SEV-SNP ABI spec, Table 22)
        version = struct.unpack_from("<I", raw, 0x00)[0]
        guest_svn = struct.unpack_from("<I", raw, 0x04)[0]
        policy = struct.unpack_from("<Q", raw, 0x08)[0]
        family_id = raw[0x10:0x20].hex()
        image_id = raw[0x20:0x30].hex()
        vmpl = struct.unpack_from("<I", raw, 0x30)[0]
        sig_algo = struct.unpack_from("<I", raw, 0x34)[0]
        tcb_raw = struct.unpack_from("<Q", raw, 0x38)[0]
        platform_info = struct.unpack_from("<Q", raw, 0x40)[0]
        report_data = raw[0x50:0x90].hex()
        measurement = raw[0x90:0xC0].hex()
        host_data = raw[0xC0:0xE0].hex()
        report_id = raw[0x140:0x160].hex()
        chip_id = raw[0x1A0:0x1E0].hex()
        committed_tcb = struct.unpack_from("<Q", raw, 0x1E0)[0]
        current_build = raw[0x1E8]
        current_minor = raw[0x1E9]
        current_major = raw[0x1EA]
        committed_build = raw[0x1EB]
        committed_minor = raw[0x1EC]
        committed_major = raw[0x1ED]
        launch_tcb = struct.unpack_from("<Q", raw, 0x1F0)[0]
        cpuid_fam = raw[0x198] if len(raw) > 0x198 else 0
        cpuid_mod = raw[0x199] if len(raw) > 0x199 else 0
        cpuid_step = raw[0x19A] if len(raw) > 0x19A else 0

        def parse_tcb(val: int) -> dict:
            return {
                "bootloader": val & 0xFF,
                "tee": (val >> 8) & 0xFF,
                "snp": (val >> 48) & 0xFF,
                "microcode": (val >> 56) & 0xFF,
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
