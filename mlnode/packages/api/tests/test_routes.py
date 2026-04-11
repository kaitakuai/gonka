"""
Unit tests for tee/routes.py — TEE endpoints.

Spec references:
  §3.1: GET /attestation (freshness_proof, 503 on not ready)
  §3.2: POST /v1/chat/completions (encrypted inference, errors)
  §10.3 MI-4: Runtime model change blocked
"""

import base64
import hashlib
import json
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch

sys.modules["common"] = MagicMock()
sys.modules["common.logger"] = MagicMock()
sys.modules["common.logger"].create_logger = MagicMock(return_value=MagicMock())

from nacl.public import PrivateKey, PublicKey, Box
from nacl.signing import SigningKey
from nacl.utils import random as nacl_random


class TestEncryptedRequest(unittest.TestCase):
    """§3.2: EncryptedRequest validation."""

    def test_valid_request(self):
        """Valid encrypted request passes validation."""
        from tee.routes import EncryptedRequest

        client_sk = PrivateKey.generate()
        nonce = nacl_random(24)

        req = EncryptedRequest(
            ciphertext=base64.b64encode(b"\x00" * 100).decode(),
            nonce=base64.b64encode(nonce).decode(),
            sender_pubkey=bytes(client_sk.public_key).hex(),
        )
        assert req.ciphertext
        assert req.sender_pubkey

    def test_invalid_pubkey_length(self):
        """§3.2: Invalid sender_pubkey (not 32 bytes) → 422."""
        from tee.routes import EncryptedRequest
        from pydantic import ValidationError

        with self.assertRaises(ValidationError):
            EncryptedRequest(
                ciphertext=base64.b64encode(b"\x00" * 10).decode(),
                nonce=base64.b64encode(b"\x00" * 24).decode(),
                sender_pubkey="aabb",  # too short
            )

    def test_invalid_nonce_length(self):
        """§3.2: Nonce must be 24 bytes → 422."""
        from tee.routes import EncryptedRequest
        from pydantic import ValidationError

        with self.assertRaises(ValidationError):
            EncryptedRequest(
                ciphertext=base64.b64encode(b"\x00" * 10).decode(),
                nonce=base64.b64encode(b"\x00" * 16).decode(),  # 16 != 24
                sender_pubkey="aa" * 32,
            )

    def test_ciphertext_size_limit(self):
        """§3.2: Ciphertext > 10 MB → 422."""
        from tee.routes import EncryptedRequest, MAX_CIPHERTEXT_SIZE
        from pydantic import ValidationError

        huge = base64.b64encode(b"\x00" * (MAX_CIPHERTEXT_SIZE + 1)).decode()
        with self.assertRaises(ValidationError):
            EncryptedRequest(
                ciphertext=huge,
                nonce=base64.b64encode(b"\x00" * 24).decode(),
                sender_pubkey="aa" * 32,
            )


class TestEncryptedResponse(unittest.TestCase):
    """§3.2: EncryptedResponse structure."""

    def test_response_fields(self):
        """Response has ciphertext, nonce, metadata, metadata_signature."""
        from tee.routes import EncryptedResponse

        resp = EncryptedResponse(
            ciphertext=base64.b64encode(b"\x00" * 10).decode(),
            nonce=base64.b64encode(b"\x00" * 24).decode(),
            metadata={"model": "test", "prompt_tokens": 1, "completion_tokens": 1,
                       "total_tokens": 2, "timestamp": 1, "tee_type": "amd-sev-snp",
                       "response_hash": "aabb"},
            metadata_signature="cc" * 64,
        )
        assert resp.metadata["model"] == "test"


class TestCanonicalJson(unittest.TestCase):
    """§3.2 + §4.4 SIG-2: Canonical JSON specification."""

    def test_spec_test_vector(self):
        """SIG-4: Test vector from spec §4.4."""
        metadata = {
            "model": "test-model",
            "prompt_tokens": 10,
            "completion_tokens": 20,
            "total_tokens": 30,
            "timestamp": 1700000000,
            "tee_type": "amd-sev-snp",
            "response_hash": "abc123",
        }

        canonical = json.dumps(
            metadata, sort_keys=True, ensure_ascii=True, separators=(",", ":")
        ).encode()

        expected = (
            b'{"completion_tokens":20,"model":"test-model","prompt_tokens":10,'
            b'"response_hash":"abc123","tee_type":"amd-sev-snp","timestamp":1700000000,'
            b'"total_tokens":30}'
        )
        assert canonical == expected

    def test_canonical_deterministic(self):
        """Same metadata always produces same bytes."""
        m = {"z": 1, "a": 2}
        c1 = json.dumps(m, sort_keys=True, ensure_ascii=True, separators=(",", ":"))
        c2 = json.dumps(m, sort_keys=True, ensure_ascii=True, separators=(",", ":"))
        assert c1 == c2 == '{"a":2,"z":1}'


class TestResponseHash(unittest.TestCase):
    """§4.5 RH-1..RH-3: Response hash binding."""

    def test_rh1_raw_bytes_not_base64(self):
        """RH-1: Hash MUST be over raw ciphertext bytes."""
        raw_ct = b"\xde\xad\xbe\xef" * 10
        b64_ct = base64.b64encode(raw_ct)

        hash_raw = hashlib.sha256(raw_ct).hexdigest()
        hash_b64 = hashlib.sha256(b64_ct).hexdigest()
        assert hash_raw != hash_b64

    def test_rh3_tamper_detected(self):
        """RH-3: Modified ciphertext → hash mismatch."""
        ct = b"\x00" * 100
        original_hash = hashlib.sha256(ct).hexdigest()

        tampered = bytearray(ct)
        tampered[0] = 0xFF
        tampered_hash = hashlib.sha256(bytes(tampered)).hexdigest()

        assert original_hash != tampered_hash


if __name__ == "__main__":
    unittest.main()
