"""
TEE Backend — Abstract interface for CPU TEE attestation.

Each CPU TEE platform (AMD SEV-SNP, Intel TDX) implements this interface.
The attestation dispatcher selects the correct backend based on detect_tee() result.
"""

from abc import ABC, abstractmethod
from pathlib import Path


class TEEBackend(ABC):
    """Abstract base for TEE attestation backends."""

    def __init__(self, attestation_dir: Path):
        self.attestation_dir = attestation_dir
        self.attestation_dir.mkdir(mode=0o755, exist_ok=True)

    @abstractmethod
    def tee_type(self) -> str:
        """Return TEE type identifier (e.g. 'amd-sev-snp', 'intel-tdx')."""
        ...

    @abstractmethod
    def generate_report(self, report_data: bytes) -> bytes:
        """
        Request attestation report from hardware.

        Args:
            report_data: 64 bytes of custom data to bind into the report
                         (typically SHA-512 of public keys).

        Returns:
            Raw binary report bytes.
        """
        ...

    @abstractmethod
    def fetch_certs(self) -> dict:
        """
        Fetch certificate chain from vendor service.

        Returns:
            Dict of cert name -> PEM string (e.g. {"vcek": "...", "ask": "...", "ark": "..."}).
        """
        ...

    @abstractmethod
    def verify_certs(self) -> bool:
        """Verify the certificate chain integrity."""
        ...

    @abstractmethod
    def verify_report(self) -> bool:
        """Verify the attestation report against the certificate chain."""
        ...

    @abstractmethod
    def parse_report(self, raw: bytes) -> dict:
        """
        Parse raw binary report into structured dict.

        Returns:
            Dict with platform-specific fields (report_data, measurement, etc.).
        """
        ...
