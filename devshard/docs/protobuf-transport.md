# Devshard Protobuf Transport Plan

This document sketches what needs to change to make devshard proxy-to-host and
host-to-host communication protobuf-based instead of JSON-based.

The public OpenAI-compatible API should remain JSON:

- client -> `devshardctl` `/v1/chat/completions`
- admin/debug endpoints like `/v1/status`, `/v1/admin/settings`, `/metrics`

The protobuf migration is for internal devshard traffic:

- `devshardctl` / user session -> remote host
- host -> host gossip
- verifier/challenge RPCs between hosts
- host protocol metadata returned to the proxy

## Current State

The protocol already uses protobuf for signed and hashed objects:

- `devshard/proto/devshard/v1/tx.proto`
- `devshard/proto/devshard/v1/diff.proto`
- `devshard/proto/devshard/v1/state.proto`

But the transport wrappers are JSON. `transport.HTTPClient` marshals Go structs
to JSON and `transport.Server` unmarshals them back:

- `POST /sessions/{id}/chat/completions`
- `POST /sessions/{id}/verify-timeout`
- `POST /sessions/{id}/challenge-receipt`
- `POST /sessions/{id}/gossip/nonce`
- `POST /sessions/{id}/gossip/txs`
- `GET /sessions/{id}/diffs`
- `GET /sessions/{id}/mempool`
- `GET /sessions/{id}/signatures`

Streaming inference responses are also JSON-framed control events inside SSE:

- `{"devshard_receipt": ...}`
- `{"devshard_meta": ...}`

The actual OpenAI token stream should not be converted to protobuf. It is
client-facing model output and should remain the same SSE payload the client
expects. Only devshard control metadata around that stream should move to
protobuf.

## Why Change

Moving the internal transport wrappers to protobuf should:

- remove base64 overhead for byte slices currently carried through JSON
- make the host/proxy contract explicit and versioned in `.proto` files
- avoid accidental JSON shape drift in protocol-critical paths
- reduce parse/encode work on hot paths and gossip fan-out
- make compatibility checks easier for versioned devshard hosts

## Target Shape

Add a transport proto file, for example:

```text
devshard/proto/devshard/v1/transport.proto
```

It should define protobuf equivalents of the current JSON transport structs:

- `HostInferenceRequest`
- `HostInferenceResponse`
- `VerifyTimeoutRequest`
- `VerifyTimeoutResponse`
- `ChallengeReceiptRequest`
- `ChallengeReceiptResponse`
- `GossipNonceRequest`
- `GossipTxsRequest`
- `DiffRecord`
- `DiffsResponse`
- `MempoolResponse`
- `SignaturesResponse`
- `StreamControlEvent`

The existing protocol messages should be reused directly where possible:

- `DiffContent`
- `DevshardTx`
- `TimeoutReason`

Do not wrap proto bytes in JSON just to preserve old shapes. Prefer real nested
protobuf fields unless there is a signature-stability reason to preserve exact
bytes.

## HTTP Compatibility Contract

Keep HTTP as the transport initially. This is a serialization migration, not a
network-stack rewrite.

New clients should send:

```text
Content-Type: application/x-protobuf
Accept: application/x-protobuf
```

Servers should return:

```text
Content-Type: application/x-protobuf
```

For the streaming inference endpoint, keep `text/event-stream`, but change
devshard control frames to typed SSE events:

```text
event: devshard_receipt_pb
data: <base64 protobuf StreamControlEvent>

event: devshard_meta_pb
data: <base64 protobuf StreamControlEvent>
```

This still uses base64 inside SSE because SSE is text, but it removes JSON
parsing and gives the control payload a versioned schema. Model output data
frames remain unchanged.

## Proposed Proto Model

Draft field layout:

```proto
syntax = "proto3";
package devshard.v1;
option go_package = "devshard/types";

import "devshard/v1/diff.proto";
import "devshard/v1/tx.proto";

message Diff {
  uint64 nonce = 1;
  repeated DevshardTx txs = 2;
  bytes user_sig = 3;
  bytes post_state_root = 4;
}

message InferencePayload {
  bytes prompt = 1;
  string model = 2;
  uint64 input_length = 3;
  uint64 max_tokens = 4;
  int64 started_at = 5;
}

message HostInferenceRequest {
  repeated Diff diffs = 1;
  uint64 nonce = 2;
  InferencePayload payload = 3;
  bool stream = 4;
}

message HostInferenceResponse {
  bytes state_sig = 1;
  bytes state_hash = 2;
  uint64 nonce = 3;
  bytes receipt = 4;
  int64 confirmed_at = 5;
  repeated DevshardTx mempool = 6;
}

message VerifyTimeoutRequest {
  uint64 inference_id = 1;
  TimeoutReason reason = 2;
  InferencePayload payload = 3;
  repeated Diff diffs = 4;
}

message VerifyTimeoutResponse {
  bool accept = 1;
  bytes signature = 2;
  uint32 voter_slot = 3;
}

message ChallengeReceiptRequest {
  uint64 inference_id = 1;
  InferencePayload payload = 2;
  repeated Diff diffs = 3;
}

message ChallengeReceiptResponse {
  bytes receipt = 1;
}

message GossipNonceRequest {
  uint64 nonce = 1;
  bytes state_hash = 2;
  bytes state_sig = 3;
  uint32 slot_id = 4;
}

message GossipTxsRequest {
  repeated DevshardTx txs = 1;
}

message DiffRecord {
  Diff diff = 1;
  bytes state_hash = 2;
}

message DiffsResponse {
  repeated DiffRecord records = 1;
}

message MempoolResponse {
  repeated DevshardTx txs = 1;
}

message SignaturesResponse {
  map<uint32, bytes> signatures = 1;
}

message StreamControlEvent {
  oneof event {
    HostInferenceResponse receipt = 1;
    MempoolResponse meta = 2;
  }
}
```

The exact names can change, but the first implementation should avoid changing
meaning at the same time as changing encoding.

## Migration Plan

1. Add `transport.proto` and generated Go types.
2. Add conversion helpers beside the existing JSON helpers in
   `devshard/transport/types.go`.
3. Update `transport.HTTPClient` with protobuf send/get helpers:
   `postProto`, `getProto`, and protobuf-aware streaming control parsing.
4. Update `transport.Server` handlers to decode protobuf when
   `Content-Type: application/x-protobuf` is present.
5. Keep JSON support temporarily for mixed-version nodes.
6. Add tests that run every internal endpoint in both JSON and protobuf modes.
7. Flip the default client mode to protobuf for the new protocol route/version.
8. Remove JSON fallback only after the deployed compatibility window is over.

## Compatibility Strategy

The safest rollout is content negotiation, not a flag day.

During the mixed period:

- protobuf clients use `Content-Type: application/x-protobuf`
- old JSON clients continue using `Content-Type: application/json`
- servers support both decoders on the same routes
- responses follow the request `Accept` header where practical
- streaming endpoints support both JSON control events and protobuf control
  events until all active hosts are upgraded

For versioned hosts, protobuf can become the default for a new devshard route
version. Legacy `/v1/devshard/*` can stay JSON-only if needed.

## Implementation Notes

Request signing currently signs the raw HTTP body. That is good: it means the
signature layer can remain unchanged as long as both sides sign and verify the
exact protobuf bytes on the wire.

Be careful with deterministic serialization:

- hashes and signatures over existing `DiffContent`, `StateSignatureContent`,
  `ExecutorReceiptContent`, and `TimeoutVoteContent` must not change
- transport wrapper protobuf does not need deterministic marshal unless it is
  itself signed or hashed
- do not change field numbers in existing frozen proto files

GET endpoints can either remain query-param based with protobuf responses, or
gain POST protobuf query equivalents. Keeping the current GET shape is lower
risk:

- `/diffs?from=&to=` returns `DiffsResponse`
- `/mempool` returns `MempoolResponse`
- `/signatures?nonce=` returns `SignaturesResponse`

## Test Checklist

Add coverage for:

- protobuf round-trip conversion for every transport request/response
- request signature verification over protobuf request bodies
- `HandleInference` protobuf request with streamed model output unchanged
- protobuf `devshard_receipt_pb` and `devshard_meta_pb` SSE events
- JSON/protobuf mixed compatibility during rollout
- old JSON clients still passing against the legacy route
- malformed protobuf returning `400`
- wrong `Content-Type`/`Accept` behavior
- golden tests for existing signed proto payloads to prove hashes did not move

## Open Questions

- Should protobuf be enabled only on a new route version, or negotiated on all
  current routes?
- Should the streaming control events use base64 protobuf in SSE, or should the
  host expose a separate non-SSE protobuf metadata channel?
- How long should JSON fallback remain after protobuf becomes the default?
- Should admin/debug endpoints ever get protobuf responses, or intentionally
  remain JSON for operator ergonomics?

## Recommended First Cut

Start with a narrow, low-risk implementation:

- add `transport.proto`
- implement protobuf encode/decode for non-streaming internal endpoints
- keep JSON as the default
- add tests for parity with the existing JSON behavior
- then move the streaming control events once the simple request/response RPCs
  are stable

After that, flip the new versioned devshard route to protobuf by default while
keeping JSON compatibility for old nodes.
