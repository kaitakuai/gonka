# Multimodal Devshard Inference Plan

## Goal

Gonka now supports text inference through the devshard gateway. The next step is
to support the full OpenAI-compatible content model exposed by multimodal models
such as Kimi K2.6, starting with typed chat content parts:

- `text`
- `image_url`
- `video_url`

The target user contract is that a client can send a normal OpenAI-compatible
chat request containing text, images, or video, and the devshard system will
preserve that request through hashing, execution, validation, storage, and
settlement.

Example target request:

```json
{
  "model": "kimi-k2.6",
  "messages": [
    {
      "role": "user",
      "content": [
        { "type": "text", "text": "Describe this scene." },
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/png;base64,..."
          }
        },
        {
          "type": "video_url",
          "video_url": {
            "url": "data:video/mp4;base64,..."
          }
        }
      ]
    }
  ],
  "max_tokens": 512,
  "stream": true
}
```

This document is ordered from the lowest execution layer upward: ML node first,
then host-to-ML-node communication, then broker, devshard host/protocol,
gateway/proxy, docs, and rollout.

## 1. ML Node

The ML node must be able to run multimodal models and accept their
OpenAI-compatible request shapes without rewriting them.

### Model Runtime

Add model-specific runtime configuration for multimodal models:

- Ensure the selected vLLM version supports the model's multimodal content
  schema.
- Add model-specific startup args through governance model args and/or local
  node config.
- Support required args such as `--trust-remote-code` if the model needs it.
- Configure vLLM multimodal limits when supported, for example maximum images
  or videos per prompt.
- Keep model args deterministic for all nodes assigned to the same model, so
  validators re-execute with compatible settings.

For Kimi-style models, the first supported direct media transport should be
base64 `data:` URLs inside `image_url.url` and `video_url.url`. Server-side
fetching arbitrary remote URLs should not be added until there is a strict
SSRF-safe fetcher and policy.

### HTTP Proxying

The ML node's integrated proxy should remain a pass-through proxy for `/v1/*`:

- Forward request bodies exactly as received.
- Preserve `Content-Type: application/json`.
- Preserve streaming behavior and response headers needed for SSE.
- Avoid parsing, normalizing, or decoding media payloads in the ML node proxy.
- Increase body-size and read-timeout limits enough for base64 image/video
  requests.

### Health and Readiness

The existing `/health` check only proves that vLLM is alive. Add a multimodal
readiness check for models that advertise media support:

- Send a tiny text-only request.
- Send a tiny image request for image-capable models.
- Send a tiny video request or a lightweight schema validation request for
  video-capable models.
- Mark the ML node inference state healthy only if the loaded model accepts the
  advertised content types.

### ML Node Status

Expose loaded model capabilities in ML node status or a new endpoint:

```json
{
  "model": "kimi-k2.6",
  "content_types": ["text", "image_url", "video_url"],
  "max_body_bytes": 104857600,
  "max_images": 16,
  "max_videos": 4,
  "media_transport": ["data_url"]
}
```

This should be informational at first. Broker/gateway policy should still come
from governance or operator configuration until node self-reporting is trusted.

## 2. Host to ML Node Communication

The devshard executor host currently receives a prompt payload and calls the
local decentralized API/broker path, which ultimately posts JSON to the ML node.
This path should stay raw JSON based.

### Request Body Preservation

The executor must pass the original multimodal JSON body to the ML node after
only deterministic request modifications that are already part of the protocol,
such as adding seed/logprob validation fields.

Do not flatten typed content arrays. In particular, this shape must remain
intact:

```json
[
  { "type": "text", "text": "What is in this image?" },
  { "type": "image_url", "image_url": { "url": "data:image/png;base64,..." } }
]
```

### Request Modification

`completionapi.ModifyRequestBodyWithLogprobsMode` already accepts typed content
arrays and passes non-text parts through. Keep that behavior, but tighten tests
around it:

- Text-only `content: "..."` still works.
- Typed text-only content remains an array.
- Mixed text/image/video content remains an array.
- Unsupported content part shapes fail before execution.
- Added protocol fields do not mutate or reorder media payloads except through
  canonical JSON behavior where hashing is explicitly expected.

### Response Handling

Response handling can remain OpenAI-compatible:

- Non-streaming responses are processed as JSON.
- Streaming responses are processed as SSE.
- Usage data from the model response remains the source for prompt and
  completion token counts, but it must not be blindly trusted for multimodal
  billing until media accounting is defined.

### Validation Re-execution

Validation must re-execute against the same full multimodal prompt payload:

- Fetch the stored original prompt payload.
- Rebuild the validation request from that full prompt.
- Preserve media parts in the validation request.
- Compare logits and token counts as today where available.
- Add a policy for providers/models that cannot produce comparable logprobs for
  multimodal requests.

If multimodal logprob validation is not available for a model, the model should
not be enabled for permissionless settlement until a replacement validation rule
is defined.

## 3. Broker and Decentralized API

The broker is responsible for choosing a capable ML node and making sure the
request is supported before node lock/acquisition.

### Capability Registry

Add a model capability registry. It can initially come from operator config and
later move into governance model metadata.

Example:

```json
{
  "kimi-k2.6": {
    "content_types": ["text", "image_url", "video_url"],
    "max_body_bytes": 104857600,
    "max_images": 16,
    "max_videos": 4,
    "requires_base64_data_urls": true,
    "media_transport": ["data_url"]
  }
}
```

The broker should use this registry to:

- Reject unsupported content types before selecting a node.
- Avoid locking nodes for requests they cannot execute.
- Route only to nodes whose loaded model and runtime config match the requested
  capability set.
- Surface clear errors for unsupported media.

### Request Validation

Extend request validation around OpenAI typed content parts:

- Accept `messages[].content` as either a string or an array.
- For arrays, require each part to be an object with a non-empty `type`.
- Require `text.text` for text parts.
- Require `image_url.url` for image parts.
- Require `video_url.url` for video parts.
- Enforce model-specific counts, byte limits, and transport rules.
- Reject remote `http://` or `https://` media URLs until SSRF-safe fetching is
  intentionally designed.

### Accounting

This is the main safety gate. Text token accounting is not enough for
multimodal traffic.

Add a deterministic "input unit" accounting model before public billing:

- Text units from tokenizer or model-reported prompt tokens.
- Image units from image count, dimensions, and/or model-reported usage.
- Video units from duration, frames, resolution, and/or model-reported usage.
- Raw byte limits for base64 payloads as a separate abuse-control dimension.

Do not rely solely on model-reported `usage.prompt_tokens` unless the model's
multimodal accounting is well understood and validators can reproduce it.

### Payload Storage

Payload storage must handle larger prompts:

- Confirm database/file storage limits for large JSON prompts.
- Store the canonical original prompt including media references or data URLs.
- Consider content-addressed media blobs later if base64 prompts become too
  large, but do not introduce that indirection in the first protocol unless all
  validators can fetch the exact same bytes.

## 4. Devshard Host and Protocol

The core devshard protocol can mostly remain unchanged because prompts are
already opaque bytes plus a canonical JSON hash.

### Prompt Hashing

Keep hashing the full canonical JSON prompt:

- Same text with different image/video payloads must produce different prompt
  hashes.
- Same media with different JSON key order must produce the same prompt hash.
- Validation must fail if a host executes a flattened or media-stripped prompt.

Add tests for:

- Text-only prompt hash compatibility.
- Typed content prompt hash determinism.
- Different image payloads producing different hashes.
- Different video payloads producing different hashes.
- Executor/validator prompt mismatch when content is flattened.

### Protocol Metadata

No protobuf change is required for basic support. `Prompt []byte`,
`PromptHash`, `InputTokens`, and `OutputTokens` are sufficient for raw
multimodal payload transport.

Consider a later protocol extension only if needed:

- `prompt_bytes`
- `input_units`
- `media_units`
- `content_types`
- `validation_mode`

If added, these fields must be included in state hashing and settlement rules.

### Limits

Add protocol-aware limits:

- Maximum prompt body bytes.
- Maximum media count.
- Maximum estimated input units.
- Maximum in-flight input units per gateway/escrow.

These limits should be enforced before sending requests to executor hosts where
possible, and again at host execution boundaries for defense in depth.

## 5. Devshard Gateway / Proxy

The current gateway has the largest text-only assumption. It normalizes typed
content arrays into strings before hashing and forwarding. That must be
removed.

### Preserve Typed Content

Replace content normalization with validation-only parsing:

- Read and limit the raw request body.
- Parse enough JSON to extract `model`, `stream`, `max_tokens`, and content
  metadata.
- Preserve the full original request body as the devshard prompt.
- Do not flatten `messages[].content`.
- Do not drop non-text content parts.

Legacy text requests must keep working:

```json
{ "messages": [{ "role": "user", "content": "Hello" }] }
```

Typed multimodal requests must also work:

```json
{
  "messages": [
    {
      "role": "user",
      "content": [
        { "type": "text", "text": "Describe this image" },
        { "type": "image_url", "image_url": { "url": "data:image/png;base64,..." } }
      ]
    }
  ]
}
```

### Gateway Limits

Rename or clarify any limiter that currently treats body bytes as
`input_tokens`. For multimodal, bytes and tokens diverge sharply.

Recommended gateway admission dimensions:

- `max_concurrent_requests`
- `max_request_body_bytes`
- `max_in_flight_body_bytes`
- `max_estimated_input_units`
- `max_in_flight_input_units`

Keep the existing token limiter for compatibility if needed, but expose clearer
metrics so operators do not mistake base64 bytes for model tokens.

### OpenAPI and Docs

Update OpenAPI and public docs:

- `messages[].content` can be a string or typed content part array.
- Document supported part types.
- Document body-size and media-count limits.
- Document that base64 `data:` URLs are the first supported media transport.
- Document unsupported remote media URLs unless/until safe fetching exists.

## 6. Nginx / External Proxy

The nginx proxy mostly forwards requests already. It needs limit and timeout
settings suitable for media payloads:

- Increase `client_max_body_size` for devshard gateway routes.
- Ensure request buffering behavior is intentional for large uploads.
- Keep SSE response buffering disabled.
- Keep long read timeouts for inference and video prompts.
- Preserve `Content-Type` and authorization headers.

Do not expose metrics or admin endpoints while adding new multimodal routes.

## 7. Rollout Plan

### Phase 1: ML Node Readiness

- Run Kimi-style multimodal model locally.
- Confirm vLLM accepts text, image, and video content parts.
- Add ML node startup args and readiness checks.
- Add ML node tests for pass-through request forwarding.

### Phase 2: Host-to-ML-Node Pass-through

- Add tests proving typed content survives decentralized API request
  modification.
- Add validation re-execution tests with media content.
- Confirm payload storage handles representative image/video request sizes.

### Phase 3: Broker Capability Gate

- Add model capability config.
- Reject unsupported content types before node lock.
- Add request size/media count validation.
- Add broker tests for text-only, image, video, and unsupported media.

### Phase 4: Devshard Protocol and Host Tests

- Add prompt hash tests for multimodal payloads.
- Add executor/validator mismatch tests for stripped media.
- Decide whether current `InputTokens` fields are sufficient or whether
  `input_units` must be introduced before enabling settlement.

### Phase 5: Gateway Support

- Remove text-only normalization.
- Preserve typed content in the prompt body.
- Add gateway request validation and body limits.
- Update OpenAPI and docs.
- Add end-to-end gateway tests.

### Phase 6: Accounting and Mainnet Enablement

- Define deterministic multimodal input units.
- Wire units into admission, cost, and settlement.
- Add operator dashboards/metrics for body bytes, media counts, and units.
- Enable multimodal traffic only for models with validated execution and
  accounting.

## 8. Test Matrix

Minimum required tests:

- Text-only legacy request still works.
- Text content array request works.
- Text plus image request works.
- Text plus video request works.
- Unsupported content type returns `400`.
- Remote media URL returns `400` if only data URLs are supported.
- Same text with different image bytes produces different prompt hashes.
- Same media with different JSON key order produces the same prompt hash.
- Host validation fails if executor strips media.
- Streaming multimodal response relays SSE correctly.
- Non-streaming multimodal response stores the canonical response payload.
- Request body limits reject oversized base64 payloads.
- Broker does not lock a node for unsupported media.
- ML node readiness fails if the loaded model cannot accept advertised media.

## 9. Open Questions

- Should media accounting be added as a protocol field now, or can it remain an
  internal cost calculation until a later protocol version?
- Should governance model metadata define multimodal capabilities, or should the
  first version use operator config only?
- Should file upload APIs be supported, or should the first version only accept
  inline base64 `data:` URLs?
- What validation rule should be used for multimodal models that cannot return
  stable logprobs?
- What is the maximum request size the network is willing to replicate and
  store per inference?

## 10. Recommended First Milestone

The first milestone should not touch settlement economics. It should prove
correct payload preservation end to end:

1. ML node accepts a tiny image request for the target model.
2. Host-to-ML-node path preserves the typed content array.
3. Devshard prompt hashing includes the media payload.
4. Gateway no longer strips media.
5. End-to-end devshard inference succeeds on a small base64 image.

After that, implement deterministic multimodal accounting before enabling
larger requests or public mainnet traffic.
