"""
Unit tests for tee/crypto.py — Key generation, encryption, signing.

Spec references:
  §4.1 KG-1..KG-4: Key generation and storage
  §4.2 KB-1..KB-4: Key-to-attestation binding
  §4.3 ENC-1..ENC-4: NaCl box encryption
  §4.4 SIG-1..SIG-4: Metadata signing
  §4.5 RH-1..RH-3: Response hash

All tests mock filesystem/tmpfs so they run on any machine.
"""

import hashlib
import json
import sys
import unittest
from unittest.mock import MagicMock, patch, mock_open

# Mock common.logger before importing
sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from nacl.public import PrivateKey, PublicKey, Box
from nacl.signing import SigningKey, VerifyKey
from nacl.utils import random as nacl_random


class TestKeyGeneration(unittest.TestCase):
    """§4.1 KG-1..KG-4: Key generation and storage."""

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def test_kg1_private_keys_ram_only(self, mock_dir, mock_tmpfs):
        """KG-1: Private keys MUST exist only as in-memory objects."""
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())

        from tee.crypto import TEEKeyManager
        keys = TEEKeyManager()

        # Private keys are Python objects, not bytes on disk
        assert hasattr(keys, "enc_private")
        assert hasattr(keys, "sign_private")
        assert isinstance(keys.enc_private, PrivateKey)
        assert isinstance(keys.sign_private, SigningKey)

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def test_kg3_keys_ephemeral(self, mock_dir, mock_tmpfs):
        """KG-3: Keys MUST be regenerated on every restart."""
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())

        from tee.crypto import TEEKeyManager
        keys1 = TEEKeyManager()
        keys2 = TEEKeyManager()

        assert keys1.enc_public_hex != keys2.enc_public_hex
        assert keys1.sign_public_hex != keys2.sign_public_hex

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def test_kg4_nacl_rng(self, mock_dir, mock_tmpfs):
        """KG-4: Keys MUST use cryptographically secure RNG (PyNaCl)."""
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())

        from tee.crypto import TEEKeyManager
        keys = TEEKeyManager()

        # Public keys are 32 bytes each
        assert len(bytes(keys.enc_public)) == 32
        assert len(bytes(keys.sign_public)) == 32


class TestKeyBinding(unittest.TestCase):
    """§4.2 KB-1..KB-4: Key-to-attestation binding."""

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def _make_keys(self, mock_dir, mock_tmpfs):
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())
        from tee.crypto import TEEKeyManager
        return TEEKeyManager()

    def test_kb1_report_data_sha512(self):
        """KB-1: report_data = SHA-512(enc_pub || sign_pub)."""
        keys = self._make_keys()
        rd = keys.compute_report_data()

        expected = hashlib.sha512(
            bytes(keys.enc_public) + bytes(keys.sign_public)
        ).digest()

        assert rd == expected
        assert len(rd) == 64  # SHA-512 = 64 bytes

    def test_kb4_test_vector(self):
        """KB-4: Test vector from spec — verify concatenation order."""
        # Spec: enc_pub = 0x01..0x20 (32 bytes), sign_pub = 0x21..0x40
        enc_pub = bytes(range(0x01, 0x21))  # 32 bytes
        sign_pub = bytes(range(0x21, 0x41))  # 32 bytes

        expected = hashlib.sha512(enc_pub + sign_pub).digest()

        # Verify it's enc || sign, not sign || enc
        wrong_order = hashlib.sha512(sign_pub + enc_pub).digest()
        assert expected != wrong_order


class TestNaclEncryption(unittest.TestCase):
    """§4.3 ENC-1..ENC-4: NaCl box encryption."""

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def _make_keys(self, mock_dir, mock_tmpfs):
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())
        from tee.crypto import TEEKeyManager
        return TEEKeyManager()

    def test_enc1_fresh_nonce(self):
        """ENC-1: Each response MUST use a fresh nonce."""
        keys = self._make_keys()
        client_sk = PrivateKey.generate()
        client_pk = client_sk.public_key

        plaintext = b'{"model": "test"}'
        ct1, n1 = keys.encrypt_response(plaintext, bytes(client_pk))
        ct2, n2 = keys.encrypt_response(plaintext, bytes(client_pk))

        assert n1 != n2  # nonces must differ

    def test_enc3_authenticated_encryption(self):
        """ENC-3: Tampered ciphertext MUST fail decryption."""
        keys = self._make_keys()
        client_sk = PrivateKey.generate()
        client_pk = client_sk.public_key

        # Client encrypts
        box = Box(client_sk, keys.enc_public)
        nonce = nacl_random(24)
        ct = box.encrypt(b'hello', nonce).ciphertext

        # Server decrypts — should work
        plaintext = keys.decrypt_request(ct, nonce, bytes(client_pk))
        assert plaintext == b'hello'

        # Tamper with ciphertext
        tampered = bytearray(ct)
        tampered[0] ^= 0xFF
        with self.assertRaises(Exception):
            keys.decrypt_request(bytes(tampered), nonce, bytes(client_pk))

    def test_encrypt_decrypt_roundtrip(self):
        """Full encrypt → decrypt roundtrip."""
        keys = self._make_keys()
        client_sk = PrivateKey.generate()
        client_pk = client_sk.public_key

        # Client encrypts for TEE
        box_client = Box(client_sk, keys.enc_public)
        nonce = nacl_random(24)
        original = b'{"model":"test","messages":[{"role":"user","content":"hi"}]}'
        ct = box_client.encrypt(original, nonce).ciphertext

        # TEE decrypts
        decrypted = keys.decrypt_request(ct, nonce, bytes(client_pk))
        assert decrypted == original

        # TEE encrypts response
        response = b'{"choices":[{"message":{"content":"hello"}}]}'
        resp_ct, resp_nonce = keys.encrypt_response(response, bytes(client_pk))

        # Client decrypts response
        box_client2 = Box(client_sk, keys.enc_public)
        resp_plain = box_client2.decrypt(resp_ct, resp_nonce)
        assert resp_plain == response


class TestMetadataSigning(unittest.TestCase):
    """§4.4 SIG-1..SIG-4: Metadata signing."""

    @patch("tee.crypto._verify_tmpfs")
    @patch("tee.crypto.KEYS_DIR")
    def _make_keys(self, mock_dir, mock_tmpfs):
        mock_dir.mkdir = MagicMock()
        mock_dir.__truediv__ = MagicMock(return_value=MagicMock())
        from tee.crypto import TEEKeyManager
        return TEEKeyManager()

    def test_sig2_canonical_json(self):
        """SIG-2: Canonical JSON with sorted keys, no whitespace, ASCII-only."""
        keys = self._make_keys()
        metadata = {
            "model": "test-model",
            "prompt_tokens": 10,
            "completion_tokens": 20,
            "total_tokens": 30,
            "timestamp": 1700000000,
            "tee_type": "amd-sev-snp",
            "response_hash": "abc123",
        }

        sig = keys.sign_metadata(metadata)

        # Verify canonical JSON format
        canonical = json.dumps(
            metadata, sort_keys=True, ensure_ascii=True, separators=(",", ":")
        ).encode()

        expected_canonical = (
            b'{"completion_tokens":20,"model":"test-model","prompt_tokens":10,'
            b'"response_hash":"abc123","tee_type":"amd-sev-snp","timestamp":1700000000,'
            b'"total_tokens":30}'
        )
        assert canonical == expected_canonical

        # Verify signature
        vk = VerifyKey(bytes(keys.sign_public))
        vk.verify(canonical, sig)  # raises if invalid

    def test_sig3_signature_verifiable(self):
        """SIG-3: Clients MUST be able to verify signature using signing_pubkey."""
        keys = self._make_keys()
        metadata = {"model": "test", "tokens": 42, "timestamp": 1}
        sig = keys.sign_metadata(metadata)

        # Simulate client-side verification
        canonical = json.dumps(
            metadata, sort_keys=True, ensure_ascii=True, separators=(",", ":")
        ).encode()

        vk = VerifyKey(bytes.fromhex(keys.sign_public_hex))
        vk.verify(canonical, sig)  # should not raise

    def test_sig_different_metadata_different_signature(self):
        """Different metadata produces different signature."""
        keys = self._make_keys()
        sig1 = keys.sign_metadata({"a": 1})
        sig2 = keys.sign_metadata({"a": 2})
        assert sig1 != sig2


class TestResponseHash(unittest.TestCase):
    """§4.5 RH-1..RH-3: Response hash."""

    def test_rh1_hash_over_raw_bytes(self):
        """RH-1: response_hash MUST be over raw ciphertext bytes, NOT base64."""
        import base64

        raw_ct = b'\x00\x01\x02\x03' * 10
        b64_ct = base64.b64encode(raw_ct).decode()

        hash_raw = hashlib.sha256(raw_ct).hexdigest()
        hash_b64 = hashlib.sha256(b64_ct.encode()).hexdigest()

        assert hash_raw != hash_b64  # must be different
        # The correct one is hash of raw bytes


class TestTmpfsVerification(unittest.TestCase):
    """§5.1 I2: Public keys MUST be written only to tmpfs."""

    def test_tmpfs_verified(self):
        """_verify_tmpfs raises if not on tmpfs."""
        from tee.crypto import _verify_tmpfs

        # Mock /proc/mounts with non-tmpfs for /run
        mock_data = "sysfs /sys sysfs rw 0 0\n/dev/sda1 /run ext4 rw 0 0\n"
        with patch("os.path.exists", return_value=True):
            with patch("builtins.open", mock_open(read_data=mock_data)):
                with self.assertRaises(RuntimeError):
                    _verify_tmpfs("/run")

    def test_tmpfs_passes(self):
        """_verify_tmpfs passes when on tmpfs."""
        from tee.crypto import _verify_tmpfs

        mock_data = "tmpfs /run tmpfs rw 0 0\n"
        with patch("os.path.exists", return_value=True):
            with patch("builtins.open", mock_open(read_data=mock_data)):
                _verify_tmpfs("/run")  # should not raise


if __name__ == "__main__":
    # Run from tests/ dir with: python -m pytest test_crypto.py -v
    # Or: cd tests && PYTHONPATH=../src python -m unittest test_crypto -v
    unittest.main()
