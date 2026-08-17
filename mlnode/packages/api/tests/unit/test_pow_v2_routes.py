"""Schema tests for the pow_v2 request models.

The class of bug these pin down (hardware-confirmed 2026-08-17): a consensus
field unknown to this proxy layer must produce a loud 422, never a silent
drop-and-200 — otherwise a dapi/node version skew makes the node execute the
wrong work while reporting success.
"""
import pytest
from pydantic import ValidationError

from api.inference.pow_v2_routes import (
    ArtifactModel,
    PoCGenerateRequest,
    PoCInitGenerateRequest,
    StatTestModel,
    ValidationModel,
)

PARAMS = {"model": "m", "seq_len": 256, "k_dim": 12,
          "max_tokens": 256, "route_window": 256}
BASE = {"block_hash": "h", "block_height": 1, "public_key": "pk",
        "node_id": 0, "node_count": 1, "params": PARAMS}


def test_unknown_top_level_field_is_rejected():
    with pytest.raises(ValidationError):
        PoCGenerateRequest(**BASE, nonces=[0], no_such_field=1)


def test_unknown_field_rejected_on_every_model():
    with pytest.raises(ValidationError):
        PoCInitGenerateRequest(**BASE, no_such_field=1)
    with pytest.raises(ValidationError):
        ArtifactModel(nonce=0, no_such_field=1)
    with pytest.raises(ValidationError):
        ValidationModel(artifacts=[], no_such_field=1)
    with pytest.raises(ValidationError):
        StatTestModel(no_such_field=1)


def test_enforced_k_steps_survives_the_forward_dump():
    """The exact field the 2026-08-17 bug lost: present in the model AND in
    the payload that goes to the backend."""
    traj = {0: [1, 2, 3], 1: [4, 5, 6]}
    req = PoCGenerateRequest(**BASE, nonces=[0, 1], wait=True,
                             enforced_k_steps=traj)
    payload = req.model_dump(exclude_none=True)
    assert payload["enforced_k_steps"] == traj


def test_decode_artifact_fields_survive_the_forward_dump():
    req = PoCGenerateRequest(
        **BASE, nonces=[0], wait=True,
        validation={"artifacts": [
            {"nonce": 0, "vector_b64": "", "k_points_steps": [1, 2, 3]}]},
        stat_test={"p_mismatch": 0.1198})
    payload = req.model_dump(exclude_none=True)
    assert payload["validation"]["artifacts"][0]["k_points_steps"] == [1, 2, 3]
    assert payload["params"]["max_tokens"] == 256
    assert payload["params"]["route_window"] == 256
    assert payload["stat_test"]["p_mismatch"] == 0.1198


def test_omitted_batch_size_is_excluded_from_the_forward_dump():
    req = PoCGenerateRequest(**BASE, nonces=[0], wait=True)
    assert "batch_size" not in req.model_dump(exclude_none=True)
