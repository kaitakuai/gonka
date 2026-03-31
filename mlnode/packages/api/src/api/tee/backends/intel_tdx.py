"""
Intel TDX Backend — Attestation via /dev/tdx_guest ioctl + Intel PCS.

Phase 5.3.1: Code + unit tests. Integration tests blocked until TDX hardware available.

Certificate chain: Intel Root CA → Intermediate → PCK (per-platform).
Report: TDX Quote v4 (ECDSA-256-with-P-256).
Device: /dev/tdx_guest (kernel 6.7+).
"""

import ctypes
import os
import struct
import subprocess
from pathlib import Path

from common.logger import create_logger

from .base import TEEBackend

logger = create_logger(__name__)

# Intel PCS (Provisioning Certification Service) base URL.
# Override with local PCCS if available.
INTEL_PCS_URL = os.getenv(
    "INTEL_PCS_URL",
    "https://api.trustedservices.intel.com/sgx/certification/v4"
)

# TDX ioctl constants (linux/tdx-guest.h)
TDX_CMD_GET_REPORT0 = 0xC0405401   # _IOWR('T', 1, struct tdx_report_req)
TDX_CMD_GET_QUOTE = 0xC0185402     # _IOWR('T', 2, struct tdx_quote_req)

# Sizes
TDX_REPORTDATA_SIZE = 64
TDX_REPORT_SIZE = 1024
TDX_QUOTE_MAX_SIZE = 8192


class _TdxReportReq(ctypes.Structure):
    """struct tdx_report_req from linux/tdx-guest.h"""
    _fields_ = [
        ("report_data", ctypes.c_uint8 * TDX_REPORTDATA_SIZE),
        ("tdx_report", ctypes.c_uint8 * TDX_REPORT_SIZE),
    ]


class IntelTdxBackend(TEEBackend):
    """Intel TDX attestation backend using /dev/tdx_guest."""

    def __init__(self, attestation_dir: Path | None = None):
        super().__init__(attestation_dir or Path("/run/tee-attestation"))
        self.certs_dir = self.attestation_dir / "certs"
        self.certs_dir.mkdir(mode=0o755, exist_ok=True)
        self.report_path = self.attestation_dir / "tdx_report.bin"
        self.quote_path = self.attestation_dir / "tdx_quote.bin"
        self._quote_bytes: bytes | None = None

    def tee_type(self) -> str:
        return "intel-tdx"

    def generate_report(self, report_data: bytes) -> bytes:
        """
        Generate TDX Quote via /dev/tdx_guest.

        Two-step process:
          1. GET_REPORT0: get TD Report (local, fast)
          2. GET_QUOTE: get Quote (involves QE, slower)

        Falls back to configfs-tsm if available (kernel 6.7+).
        """
        if len(report_data) > TDX_REPORTDATA_SIZE:
            report_data = report_data[:TDX_REPORTDATA_SIZE]
        report_data = report_data.ljust(TDX_REPORTDATA_SIZE, b'\x00')

        # Try configfs-tsm first (modern kernels, simpler)
        tsm_path = Path("/sys/kernel/config/tsm/report")
        if tsm_path.exists():
            return self._generate_via_configfs(report_data, tsm_path)

        # Fall back to direct ioctl
        return self._generate_via_ioctl(report_data)

    def _generate_via_configfs(self, report_data: bytes, tsm_path: Path) -> bytes:
        """Generate quote via configfs-tsm (kernel 6.7+)."""
        entry_name = f"tdx-att-{os.getpid()}"
        entry_path = tsm_path / entry_name

        try:
            entry_path.mkdir(exist_ok=True)
            (entry_path / "inblob").write_bytes(report_data)
            (entry_path / "provider").write_text("tdx_guest\n")
            quote_bytes = (entry_path / "outblob").read_bytes()
        finally:
            if entry_path.exists():
                try:
                    entry_path.rmdir()
                except OSError:
                    pass

        self._quote_bytes = quote_bytes
        self.quote_path.write_bytes(quote_bytes)
        logger.info(f"TDX Quote generated via configfs-tsm ({len(quote_bytes)} bytes)")
        return quote_bytes

    def _generate_via_ioctl(self, report_data: bytes) -> bytes:
        """Generate quote via /dev/tdx_guest ioctl."""
        import fcntl

        # Step 1: Get TD Report
        req = _TdxReportReq()
        ctypes.memmove(req.report_data, report_data, TDX_REPORTDATA_SIZE)

        try:
            fd = os.open("/dev/tdx_guest", os.O_RDWR)
            try:
                fcntl.ioctl(fd, TDX_CMD_GET_REPORT0, req)
            finally:
                os.close(fd)
        except OSError as e:
            raise RuntimeError(f"TDX GET_REPORT0 ioctl failed: {e}")

        td_report = bytes(req.tdx_report)

        # Step 2: Get Quote via QE (Quoting Enclave)
        # The quote generation typically goes through a quote generation service.
        # On modern kernels, configfs-tsm handles this. For older kernels,
        # we use the tdx_guest ioctl which requires qgsd (Quote Generation Service Daemon).
        try:
            quote_bytes = self._request_quote_via_qgs(td_report)
        except Exception as e:
            logger.warning(f"QGS quote generation failed: {e}, saving TD Report only")
            quote_bytes = td_report

        self._quote_bytes = quote_bytes
        self.quote_path.write_bytes(quote_bytes)
        logger.info(f"TDX Quote generated via ioctl ({len(quote_bytes)} bytes)")
        return quote_bytes

    def _request_quote_via_qgs(self, td_report: bytes) -> bytes:
        """Request quote from Quote Generation Service daemon."""
        # Try qgs-client CLI if available
        qgs_client = "/usr/bin/qgs-client"
        if os.path.isfile(qgs_client):
            report_file = self.attestation_dir / "td_report.bin"
            report_file.write_bytes(td_report)
            r = subprocess.run(
                [qgs_client, "--report", str(report_file), "--quote", str(self.quote_path)],
                capture_output=True, text=True, timeout=30,
            )
            if r.returncode == 0 and self.quote_path.exists():
                return self.quote_path.read_bytes()
            raise RuntimeError(f"qgs-client failed: {r.stderr}")

        raise RuntimeError("No quote generation method available (need configfs-tsm or qgsd)")

    def fetch_certs(self) -> dict:
        """
        Fetch Intel SGX/TDX certificate chain from PCS.

        The PCK cert is platform-specific and retrieved using the platform's
        QE identity from the quote.
        """
        certs = {"pck": None, "intermediate": None, "root": None}

        # Extract QE certification data from quote if available
        if self._quote_bytes and len(self._quote_bytes) > 48:
            try:
                certs = self._fetch_from_pcs()
            except Exception as e:
                logger.warning(f"PCS cert fetch failed: {e}")

        return certs

    def _fetch_from_pcs(self) -> dict:
        """Fetch certs from Intel Provisioning Certification Service."""
        import urllib.request
        import urllib.error

        certs = {"pck": None, "intermediate": None, "root": None}

        # Fetch PCK CRL (contains intermediate cert in header)
        try:
            req = urllib.request.Request(f"{INTEL_PCS_URL}/pckcrl?ca=platform")
            with urllib.request.urlopen(req, timeout=10) as resp:
                resp.read()  # consume body
                issuer_chain = resp.headers.get("SGX-PCK-CRL-Issuer-Chain", "")
                if issuer_chain:
                    # URL-decode the chain
                    from urllib.parse import unquote
                    chain = unquote(issuer_chain)
                    parts = chain.split("-----END CERTIFICATE-----")
                    if len(parts) >= 2:
                        certs["intermediate"] = parts[0] + "-----END CERTIFICATE-----\n"
                        certs["root"] = parts[1].strip() + "\n" if parts[1].strip() else None
        except (urllib.error.URLError, OSError) as e:
            logger.warning(f"Failed to fetch PCK CRL: {e}")

        # Save certs to disk
        for name, pem in certs.items():
            if pem:
                (self.certs_dir / f"{name}.pem").write_text(pem)

        return certs

    def verify_certs(self) -> bool:
        """Verify Intel certificate chain using openssl."""
        root_path = self.certs_dir / "root.pem"
        intermediate_path = self.certs_dir / "intermediate.pem"

        if not root_path.exists() or not intermediate_path.exists():
            logger.warning("Intel certs not available for verification")
            return False

        r = subprocess.run(
            ["openssl", "verify", "-CAfile", str(root_path), str(intermediate_path)],
            capture_output=True, text=True,
        )
        ok = r.returncode == 0
        logger.info(f"Intel cert chain verification: {'ok' if ok else 'FAILED'}")
        return ok

    def verify_report(self) -> bool:
        """
        Verify TDX Quote signature.

        Full verification requires Intel DCAP library (libsgx-dcap-quote-verify).
        Falls back to basic structural validation.
        """
        if not self._quote_bytes:
            if self.quote_path.exists():
                self._quote_bytes = self.quote_path.read_bytes()
            else:
                logger.warning("No TDX Quote available for verification")
                return False

        # Try DCAP verification via CLI
        dcap_verify = "/usr/bin/dcap_quoteverify"
        if os.path.isfile(dcap_verify):
            r = subprocess.run(
                [dcap_verify, str(self.quote_path)],
                capture_output=True, text=True, timeout=30,
            )
            ok = r.returncode == 0
            logger.info(f"TDX Quote DCAP verification: {'ok' if ok else 'FAILED'}")
            return ok

        # Fallback: structural validation only
        parsed = self.parse_report(self._quote_bytes)
        if "parse_error" in parsed:
            logger.warning(f"TDX Quote parse error: {parsed['parse_error']}")
            return False

        logger.info("TDX Quote structural validation: ok (DCAP not available for full verify)")
        return True

    def parse_report(self, raw: bytes) -> dict:
        """
        Parse TDX Quote v4 structure.

        TDX Quote layout:
          - Header (48 bytes): version, att_key_type, tee_type, qe_svn, pce_svn, qe_vendor_id, user_data
          - TD Report (584 bytes): tee_tcb_svn, mr_seam, mr_signer_seam, td_attributes,
                                    xfam, mr_td, mr_config_id, mr_owner, mr_owner_config,
                                    rt_mr[0-3], report_data
          - Signature (variable): ECDSA-256-with-P-256
        """
        if len(raw) < 632:
            return {"raw_hex": raw[:128].hex(), "parse_error": f"too short ({len(raw)} bytes, need >=632)"}

        # Header (48 bytes)
        version = struct.unpack_from("<H", raw, 0)[0]
        att_key_type = struct.unpack_from("<H", raw, 2)[0]
        tee_type = struct.unpack_from("<I", raw, 4)[0]
        qe_svn = struct.unpack_from("<H", raw, 8)[0]
        pce_svn = struct.unpack_from("<H", raw, 10)[0]
        qe_vendor_id = raw[12:28].hex()
        user_data = raw[28:48].hex()

        # TD Report body starts at offset 48
        td_offset = 48

        # TEE TCB SVN (16 bytes)
        tee_tcb_svn = raw[td_offset:td_offset + 16].hex()
        # MR_SEAM (48 bytes)
        mr_seam = raw[td_offset + 16:td_offset + 64].hex()
        # MR_SIGNER_SEAM (48 bytes)
        mr_signer_seam = raw[td_offset + 64:td_offset + 112].hex()
        # SEAM Attributes (8 bytes)
        seam_attributes = raw[td_offset + 112:td_offset + 120].hex()
        # TD Attributes (8 bytes)
        td_attributes = raw[td_offset + 120:td_offset + 128].hex()
        # XFAM (8 bytes)
        xfam = raw[td_offset + 128:td_offset + 136].hex()
        # MR_TD (48 bytes) — measurement of TD
        mr_td = raw[td_offset + 136:td_offset + 184].hex()
        # MR_CONFIG_ID (48 bytes)
        mr_config_id = raw[td_offset + 184:td_offset + 232].hex()
        # MR_OWNER (48 bytes)
        mr_owner = raw[td_offset + 232:td_offset + 280].hex()
        # MR_OWNER_CONFIG (48 bytes)
        mr_owner_config = raw[td_offset + 280:td_offset + 328].hex()
        # RT_MR[0..3] (4 x 48 bytes)
        rt_mrs = []
        for i in range(4):
            start = td_offset + 328 + (i * 48)
            rt_mrs.append(raw[start:start + 48].hex())
        # Report Data (64 bytes) at td_offset + 520
        report_data = raw[td_offset + 520:td_offset + 584].hex()

        return {
            "version": version,
            "attestation_key_type": att_key_type,
            "tee_type_raw": tee_type,
            "qe_svn": qe_svn,
            "pce_svn": pce_svn,
            "qe_vendor_id": qe_vendor_id,
            "user_data": user_data,
            "tee_tcb_svn": tee_tcb_svn,
            "mr_seam": mr_seam,
            "mr_signer_seam": mr_signer_seam,
            "seam_attributes": seam_attributes,
            "td_attributes": td_attributes,
            "xfam": xfam,
            "mr_td": mr_td,
            "mr_config_id": mr_config_id,
            "mr_owner": mr_owner,
            "mr_owner_config": mr_owner_config,
            "rt_mr0": rt_mrs[0],
            "rt_mr1": rt_mrs[1],
            "rt_mr2": rt_mrs[2],
            "rt_mr3": rt_mrs[3],
            "report_data": report_data,
            "measurement": mr_td,  # Canonical measurement field (consistent with AMD)
        }
