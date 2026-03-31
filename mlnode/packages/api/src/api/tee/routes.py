"""
TEE Routes — Attestation endpoint and encrypted /chat/completions.

Per proposal #951:
  - "All requests to the Confidential MLNode are encrypted using
     the public key from the certificate."
  - "Confidential MLNode signs response metadata (used tokens)"
"""

import base64
import hashlib
import json
import time

import asyncio

import httpx
from fastapi import APIRouter, Request, HTTPException
from pydantic import BaseModel, field_validator

from common.logger import create_logger

logger = create_logger(__name__)

router = APIRouter(tags=["TEE"])

# vLLM backend (localhost only, never leaves VM)
VLLM_BACKEND = "http://127.0.0.1:5000"

# Shared httpx client (async, connection pooling)
_vllm_client: httpx.AsyncClient = None
_vllm_client_lock = asyncio.Lock()


async def _get_vllm_client() -> httpx.AsyncClient:
    global _vllm_client
    async with _vllm_client_lock:
        if _vllm_client is None or _vllm_client.is_closed:
            _vllm_client = httpx.AsyncClient(
                base_url=VLLM_BACKEND,
                timeout=300.0,
            )
    return _vllm_client


async def close_vllm_client():
    """Call during shutdown to cleanly close the httpx client."""
    global _vllm_client
    if _vllm_client is not None and not _vllm_client.is_closed:
        await _vllm_client.aclose()
        _vllm_client = None


# ---------------------------------------------------------------------------
# Request / Response models
# ---------------------------------------------------------------------------

MAX_CIPHERTEXT_SIZE = 10 * 1024 * 1024  # 10 MB base64


class EncryptedRequest(BaseModel):
    """Encrypted /chat/completions request from client."""
    ciphertext: str        # base64(NaCl box encrypted OpenAI JSON)
    nonce: str             # base64(24-byte nonce)
    sender_pubkey: str     # hex(client's X25519 public key)

    @field_validator("ciphertext")
    @classmethod
    def limit_ciphertext(cls, v):
        if len(v) > MAX_CIPHERTEXT_SIZE:
            raise ValueError("Request too large")
        return v

    @field_validator("sender_pubkey")
    @classmethod
    def validate_pubkey(cls, v):
        try:
            b = bytes.fromhex(v)
        except ValueError:
            raise ValueError("Invalid hex")
        if len(b) != 32:
            raise ValueError("Public key must be 32 bytes")
        return v

    @field_validator("nonce")
    @classmethod
    def validate_nonce(cls, v):
        b = base64.b64decode(v)
        if len(b) != 24:
            raise ValueError("Nonce must be 24 bytes")
        return v


class EncryptedResponse(BaseModel):
    """Encrypted response + signed metadata."""
    ciphertext: str        # base64(NaCl box encrypted OpenAI response JSON)
    nonce: str             # base64(24-byte nonce)
    metadata: dict         # plaintext: {model, tokens, timestamp, tee_type, response_hash}
    metadata_signature: str  # hex(Ed25519 signature over metadata)


# ---------------------------------------------------------------------------
# GET /attestation
# ---------------------------------------------------------------------------

@router.get("/attestation")
async def get_attestation(request: Request):
    """
    Returns attestation certificate bundle.

    Contains everything a remote verifier needs:
    - Encryption + signing public keys
    - SNP attestation report (signed by AMD VCEK)
    - AMD certificate chain (ARK -> ASK -> VCEK)
    - VM metadata (measurement, OS, vLLM version, image hash)
    """
    att = getattr(request.app.state, "tee_attestation", None)
    if att is None:
        raise HTTPException(status_code=503, detail="Attestation not ready")
    return att


# ---------------------------------------------------------------------------
# POST /v1/chat/completions (encrypted-only in TEE mode)
# ---------------------------------------------------------------------------

@router.post("/v1/chat/completions", response_model=EncryptedResponse)
async def encrypted_chat_completions(req: EncryptedRequest, request: Request):
    """
    Encrypted inference endpoint.

    Proposal flow:
    1. Client encrypts OpenAI-format JSON with TEE's encryption pubkey
    2. This endpoint decrypts in encrypted TEE RAM
    3. Forwards plaintext to vLLM on localhost (never leaves VM)
    4. Encrypts vLLM response with client's pubkey
    5. Signs metadata (token count + response_hash) with TEE signing key
    6. Returns encrypted response + signed metadata

    No sensitive data (prompts, responses) ever appears in logs,
    on disk, or in plaintext on the network.
    """
    keys = getattr(request.app.state, "tee_keys", None)
    if keys is None:
        raise HTTPException(status_code=503, detail="TEE not ready")

    # --- Step 1: Decrypt request ---
    try:
        ct_bytes = base64.b64decode(req.ciphertext)
        nonce_bytes = base64.b64decode(req.nonce)
        sender_pub_bytes = bytes.fromhex(req.sender_pubkey)
        plaintext = keys.decrypt_request(ct_bytes, nonce_bytes, sender_pub_bytes)
        openai_request = json.loads(plaintext)
    except Exception:
        # Never reveal why decryption failed
        raise HTTPException(status_code=400, detail="Decryption failed")

    # --- Step 2: Forward to vLLM (localhost only, never leaves VM) ---
    # Strip streaming — not supported in encrypted mode
    openai_request.pop("stream", None)

    model = openai_request.get("model", "default")
    logger.info(f"TEE inference request (model={model})")  # No prompt logged

    try:
        client = await _get_vllm_client()
        vllm_resp = await client.post("/v1/chat/completions", json=openai_request)
        vllm_resp.raise_for_status()
        vllm_json = vllm_resp.json()
    except httpx.HTTPStatusError as e:
        logger.error(f"vLLM returned {e.response.status_code}")
        raise HTTPException(status_code=502, detail="Inference backend error")
    except Exception as e:
        logger.error(f"vLLM connection error: {type(e).__name__}")
        raise HTTPException(status_code=503, detail="vLLM unavailable")

    # --- Step 3: Encrypt response for client ---
    response_bytes = json.dumps(vllm_json).encode()
    enc_resp, resp_nonce = keys.encrypt_response(response_bytes, sender_pub_bytes)

    # --- Step 4: Build metadata (plaintext, signed) ---
    usage = vllm_json.get("usage", {})
    metadata = {
        "model": vllm_json.get("model", model),
        "prompt_tokens": usage.get("prompt_tokens", 0),
        "completion_tokens": usage.get("completion_tokens", 0),
        "total_tokens": usage.get("total_tokens", 0),
        "timestamp": int(time.time()),
        "tee_type": getattr(request.app.state, "tee_info", None).cpu_tee.value
                    if getattr(request.app.state, "tee_info", None)
                    else "unknown",
        "response_hash": hashlib.sha256(enc_resp).hexdigest(),
    }

    # --- Step 5: Sign metadata (binds to response ciphertext) ---
    signature = keys.sign_metadata(metadata)

    return EncryptedResponse(
        ciphertext=base64.b64encode(enc_resp).decode(),
        nonce=base64.b64encode(resp_nonce).decode(),
        metadata=metadata,
        metadata_signature=signature.hex(),
    )
