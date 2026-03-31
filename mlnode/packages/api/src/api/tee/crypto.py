"""
TEE Cryptography — Key generation, encryption, signing.

All keys exist only as Python objects in encrypted RAM.
Per proposal #951: "If the VM is restarted or the image is replaced,
the private key will be lost."
"""

import hashlib
import json
import os
from pathlib import Path

from nacl.public import PrivateKey, PublicKey, Box
from nacl.signing import SigningKey
from nacl.utils import random as nacl_random

from common.logger import create_logger

logger = create_logger(__name__)

KEYS_DIR = Path("/run/tee-keys")


def _verify_tmpfs(path: str):
    """Verify path is on tmpfs (RAM), refuse to run otherwise.

    Finds the longest matching mountpoint for path, then checks its fstype.
    """
    if not os.path.exists("/proc/mounts"):
        return  # Not Linux, skip check
    best_mount = ""
    best_fstype = ""
    with open("/proc/mounts") as f:
        for line in f:
            parts = line.split()
            if len(parts) >= 3:
                mountpoint = parts[1]
                fstype = parts[2]
                if (path == mountpoint or path.startswith(mountpoint + "/")) \
                        and len(mountpoint) > len(best_mount):
                    best_mount = mountpoint
                    best_fstype = fstype
    if best_fstype == "tmpfs":
        return
    raise RuntimeError(
        f"{path} is NOT on tmpfs (mounted as {best_fstype} at {best_mount}) "
        f"— refusing to store keys on disk"
    )


class TEEKeyManager:
    """
    Manages two keypairs in memory only:
      - X25519  → encrypt/decrypt inference requests and responses
      - Ed25519 → sign metadata (token usage) to prove TEE origin

    Both public keys are bound into the SNP attestation report via:
      report_data = SHA-512(enc_pubkey || sign_pubkey)

    Private keys are NEVER written to files. Only public keys are
    written to tmpfs for external tools that may need them.
    """

    def __init__(self):
        _verify_tmpfs("/run")

        KEYS_DIR.mkdir(mode=0o700, exist_ok=True)

        # Generate keypairs — private keys stay in Python objects only
        self.enc_private = PrivateKey.generate()
        self.enc_public = self.enc_private.public_key

        self.sign_private = SigningKey.generate()
        self.sign_public = self.sign_private.verify_key

        # Only write PUBLIC keys to tmpfs (for snpguest or external tools)
        (KEYS_DIR / "enc_public.key").write_bytes(bytes(self.enc_public))
        (KEYS_DIR / "sign_public.key").write_bytes(bytes(self.sign_public))

        logger.info("TEE keypairs generated (memory only)")
        logger.info(f"  enc pubkey:  {self.enc_public_hex}")
        logger.info(f"  sign pubkey: {self.sign_public_hex}")

    @property
    def enc_public_hex(self) -> str:
        return bytes(self.enc_public).hex()

    @property
    def sign_public_hex(self) -> str:
        return bytes(self.sign_public).hex()

    def compute_report_data(self) -> bytes:
        """SHA-512(enc_pubkey || sign_pubkey) → 64 bytes for SNP report_data."""
        combined = bytes(self.enc_public) + bytes(self.sign_public)
        return hashlib.sha512(combined).digest()

    def decrypt_request(self, ciphertext: bytes, nonce: bytes,
                        sender_pubkey_bytes: bytes) -> bytes:
        """Decrypt incoming NaCl box from client."""
        box = Box(self.enc_private, PublicKey(sender_pubkey_bytes))
        return box.decrypt(ciphertext, nonce)

    def encrypt_response(self, plaintext: bytes,
                         recipient_pubkey_bytes: bytes) -> tuple:
        """Encrypt response for client. Returns (ciphertext, nonce)."""
        box = Box(self.enc_private, PublicKey(recipient_pubkey_bytes))
        nonce = nacl_random(24)
        ct = box.encrypt(plaintext, nonce).ciphertext
        return ct, nonce

    def sign_metadata(self, metadata: dict) -> bytes:
        """Ed25519 sign metadata. Proves TEE computed this."""
        canonical = json.dumps(
            metadata, sort_keys=True, ensure_ascii=True, separators=(",", ":")
        ).encode()
        return self.sign_private.sign(canonical).signature
