# Chat Completions — supported parameters

Reference for `POST /v1/chat/completions` on devshard. Documents which OpenAI/vLLM request fields the gateway accepts today, which are rejected, how to use the ones that pass, and what we still need to tighten.

## Why a strict whitelist

vLLM crashes on malformed or pathological requests (deep recursive JSON Schema, unsupported routing fields, unbounded objects). To keep the inference node healthy:

1. **Body-level depth scan** (`ensureRequestNestingDepth` in `request_filters_parameters.go`). A byte-level pass bounds *whole-request* JSON nesting at `MaxRequestNestingDepth = 32` before `encoding/json` allocates anything. Without this, a 7 KiB body with 200 levels of nesting expands to ~180 KiB of `map[string]any` wrappers in the decoder — the validator below would still reject it, but the decoder has already paid the cost. The pre-scan defuses that amplification. (This is a different layer from the schema-level `MaxDepth = 5` cap below: the body limit is generous because legitimate requests can be ~10 levels deep through `messages[].content[].text` + `tools[].function.parameters` + `response_format.json_schema.schema`; the schema limit is tight because the grammar compiler in vLLM is the actual attack surface.)
2. Every inbound `/chat/completions` body is then decoded into a generic JSON document.
3. `VLLMParameterCatalog` (`devshard/cmd/devshardctl/request_filters_parameters.go`) is a closed allow-list. The set of allowed keys is precomputed at catalog construction (no per-request map build). Any top-level field that is not in the catalog is rejected with `feature "<name>" is temporarily unavailable` (HTTP 400) before the request reaches the model.
4. Parameter rules run in two stages:
   - `PreValidation` — on the raw document, before we decode/validate it.
   - `PostLimits` — after `max_tokens` defaults/caps are resolved.
5. The message validator (`request_filters_messages.go`) enforces the OpenAI-compatible message contract (roles, tool_call linkage, text-only content parts).
6. The chat-request projection (`chatRequest` — 5 fields: `model`, `stream`, `max_tokens`, `max_completion_tokens`, `n`) is populated by direct map reads from the document, not a `json.Marshal + Unmarshal` round-trip.

Anything not on this whitelist does not reach the model. That is the contract.

## Supported parameters

| Field | Category | Stage / rule | Behavior |
| --- | --- | --- | --- |
| `model` | string | — | Required. Pass-through. Selects the upstream model id. |
| `stream` | bool | — | Pass-through. Streaming is enabled when `true`. |
| `messages` | object array | message validator | Required, non-empty. Strict OpenAI-compatible message contract — see "Messages" section below. |
| `max_tokens` | int range | PostLimits (token-limit pipeline) | If absent → set to `DefaultRequestMaxTokens`. Capped at `RequestMaxTokensCap` unless the request carries admin auth. When both `max_tokens` and `max_completion_tokens` are set, the smaller wins and both are aligned. |
| `max_completion_tokens` | int range | PostLimits (token-limit pipeline) | Same rules as `max_tokens`. |
| `n` | int range | PostLimits sanitize (`CapUintParameterHandler`) | Capped at `MaxChatRequestChoices` (5). |
| `temperature` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. Capped at `MaxTemperature` (2.0). String-encoded numbers accepted. |
| `top_p` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. No upper cap (forwarded raw). |
| `top_k` | int range | PostLimits sanitize | Strips `NaN`/`±Inf`. vLLM-specific. |
| `min_p` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. vLLM-specific. |
| `repetition_penalty` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. Capped at `MaxRepetitionPenalty` (2.0). |
| `logit_bias` | int→float map | PostLimits sanitize (`SanitizeFloatMapParameterHandler`) | Keeps numeric entries in `[-100, 100]`. Drops the field if the map is empty after sanitization. |
| `stop` | string list | — | Pass-through. |
| `stop_token_ids` | int list | — | Pass-through. vLLM-specific. |
| `seed` | int range | — | Pass-through. |
| `skip_special_tokens` | bool | — | Pass-through. vLLM-specific. |
| `detokenize` | bool | — | Pass-through. vLLM-specific. |
| `thinking` | object | — | Pass-through. Kimi-K2 specific (`{"type": "enabled"|"disabled"}`). For `moonshotai/Kimi-K2.6` it is also mirrored into `chat_template_kwargs.thinking` via `applyKimiRequestOverrides`. |
| `chat_template_kwargs` | object | — | Pass-through. Used for provider-specific chat templates (Qwen, Kimi). |
| `tool_choice` | string | PreValidation reject | Pass-through except `"required"` — rejected because the upstream contract does not honor it on the vLLM path. Stripped together with `tools` if `tools` arrives empty. |
| `min_tokens` | int range | PreValidation conditional strip + PostLimits clamp | Dropped if `stop_token_ids` is also set (vLLM rejects the combination). Otherwise clamped to ≤ `max_tokens`. |
| `bad_words` | string list | PreValidation sanitize | Empty/whitespace strings are removed. Field is dropped if the resulting list is empty. |
| `tools` | object array | PreValidation sanitize | If the array is empty, both `tools` and `tool_choice` are removed (vLLM rejects empty `tools`). |
| `logprobs` | bool | PostLimits force | Forced to `true` for the gateway's observability pipeline regardless of what the client sent. |
| `top_logprobs` | int range | PostLimits force | Forced to `5` for the same reason. |
| `response_format` | object | PreValidation validate (`paramvalidators.ResponseFormatValidator`) | Accepted with bounds. `type` must be one of `text` / `json_object` / `json_schema`. For `json_schema`, the schema is walked once and rejected if depth > 5, nodes > 128, branch arms (anyOf/oneOf/allOf) > 16, enum > 256, serialized size > 16 KiB, or the schema contains `$ref` / `$defs` / `definitions`. See "Validation invariants" below for the policy. |

### Messages contract (recap)

Enforced by `request_filters_messages.go`:

- Roles: `developer`, `system`, `user`, `assistant`, `tool`, `function`.
- Assistant `content` may be empty/null only when `tool_calls` or `function_call` is present.
- Tool messages require `tool_call_id` matching a prior assistant `tool_calls[].id`.
- Function messages require `name`.
- Content parts: only `{"type": "text", "text": "..."}` is accepted. Typed arrays of text parts are flattened to a single string before forwarding.
- Empty tool `content` is normalized to a sentinel string; missing tool `content` is also normalized.

## Unsupported parameters (rejected)

Every OpenAI / vLLM / provider-specific field that is **not** in the table above is rejected at `PreValidation`. The error response is HTTP 400 with body `feature "<name>" is temporarily unavailable`.

The groupings below are not exhaustive — they document the gaps we know clients hit, mostly drawn from comparable gateways (Hermes, OpenClaw, Kilo):

| Group | Examples we reject today | Notes |
| --- | --- | --- |
| Structured output (other than `response_format`) | `guided_json`, `guided_regex`, `guided_grammar`, `guided_choice`, `enforced_tokens` | `response_format` is now supported with bounds (see Supported table). The vLLM-specific `guided_*` family stays rejected because it bypasses the same safety bounds. |
| Sampling | `frequency_penalty`, `presence_penalty` | Easy to add (numeric range sanitize, similar to `temperature`). Not yet validated end-to-end against the vLLM build we run. |
| Tool-calling extensions | `parallel_tool_calls`, `tool_choice="required"` | Provider compatibility differs; we keep them off until we can pin behavior. |
| Routing / metadata | `metadata`, `service_tier`, `user`, `seed_override`, `extra_body`, `extra_headers`, `provider`, `plugins`, `tags` | Vendor-specific; no inference-side meaning for vLLM. |
| Caching / observability | `store`, `prompt_cache_key`, `stream_options` | Could be safely stripped before forward, but currently rejected to avoid silent semantic drift. |
| Reasoning (non-vLLM-native) | `reasoning_effort`, `reasoning`, `thinking_config`, `enable_thinking` | Use `thinking` + `chat_template_kwargs` instead (see "Supported"). |

If you see a new field in this list that you need, add it to the catalog with the smallest safe rule — see "Validation improvements".

## How to use the gateway

### Minimal request

```http
POST /v1/chat/completions
Content-Type: application/json

{
  "model": "moonshotai/Kimi-K2.6",
  "messages": [{"role": "user", "content": "Hello"}]
}
```

`max_tokens` is filled in for you (`DefaultRequestMaxTokens`).

### With sampling knobs

```json
{
  "model": "moonshotai/Kimi-K2.6",
  "messages": [{"role": "user", "content": "Hello"}],
  "temperature": 0.7,
  "top_p": 0.95,
  "top_k": 40,
  "repetition_penalty": 1.05,
  "max_tokens": 512
}
```

All five knobs are sanitized at `PostLimits`: non-finite numbers and out-of-range values are dropped or clamped before the request reaches vLLM.

### With tools (function calling)

```json
{
  "model": "moonshotai/Kimi-K2.6",
  "messages": [{"role": "user", "content": "Weather in Berlin?"}],
  "tools": [
    {"type": "function", "function": {"name": "get_weather",
      "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
  ],
  "tool_choice": "auto"
}
```

If `tools` is `[]`, the gateway strips both `tools` and `tool_choice` (the vLLM build we run rejects empty tools).

### With Kimi-K2.6 thinking

```json
{
  "model": "moonshotai/Kimi-K2.6",
  "messages": [{"role": "user", "content": "Plan this refactor"}],
  "thinking": {"type": "enabled"}
}
```

For `moonshotai/Kimi-K2.6` the gateway also writes `chat_template_kwargs.thinking = true` automatically.

### Rejected request

```json
{
  "model": "moonshotai/Kimi-K2.6",
  "messages": [{"role": "user", "content": "Hello"}],
  "response_format": {"type": "json_object"}
}
```

→ `HTTP 400 — feature "response_format" is temporarily unavailable`. Same shape for any other unknown key.

## Validation improvements (roadmap)

### Priority: safe `response_format`

The example below currently crashes vLLM because the JSON Schema contains ~200 nested `properties` levels:

```json
{
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "r",
      "schema": {"properties": {"x": {"properties": {"x": {"properties": {"x": "..."}}}}}, "required": ["x"], "type": "object"}
    }
  }
}
```

A safe `response_format` handler must enforce all of the following at `PreValidation` and reject the request with HTTP 400 if any check fails:

| Invariant | Initial threshold | Rationale |
| --- | --- | --- |
| `type` ∈ {`text`, `json_object`, `json_schema`} | Hard whitelist | Same set OpenClaw/Kilo document. Anything else → reject. |
| For `json_schema`: `json_schema.name` is a non-empty string ≤ 64 chars, regex `^[A-Za-z0-9_.-]+$` | Hard | vLLM uses the name as a grammar identifier. |
| For `json_schema`: `json_schema.schema` is an object | Hard | Reject arrays/strings/null. |
| Max schema depth | **≤ 5** | Counts nesting through `properties`, `items`, `additionalProperties`, `anyOf`/`oneOf`/`allOf` element schemas, `prefixItems`. The 200-level attack above fails at depth 6. |
| Max serialized schema size | **≤ 16 KiB** | Measured on the marshalled `json_schema.schema` bytes. Bounds memory/time on the vLLM grammar compiler. |
| Max schema node count | **≤ 128** | Each visited schema object (root, every nested property/item/branch arm) counts as one node. Bounds breadth attacks like a single `properties` map with hundreds of children. |
| `$ref`, `$defs`, `definitions` | Forbidden | Prevents cycles and indirect blow-ups. |
| `anyOf`/`oneOf`/`allOf` array length | ≤ 16 | Avoid combinatorial explosion in vLLM's grammar compiler. |
| `enum` length | ≤ 256 | Same reason. |

Order of checks: type → name + regex → walk the schema once, counting depth, nodes, branch arms, enums, refusing on the first violation → only then `json.Marshal` the schema to enforce the byte-size cap. Walking comes first because `json.Marshal` is O(input size); doing it before the walk lets an attacker amplify a 200-level recursive payload into hundreds of allocations before the depth check ever fires. Walking first cuts that path from ~87 µs / 1606 allocs to ~560 ns / 2 allocs (Apple M2 Pro). Single bounded pass — no recursion without a depth counter.

Status: **implemented** as a pure validator in subpackage `cmd/devshardctl/paramvalidators/response_format.go` — `paramvalidators.ResponseFormatValidator{MaxDepth: 5, MaxSize: 16384, MaxNodes: 128, MaxBranch: 16, MaxEnum: 256, MaxNameLen: 64}`. The main catalog wires it in via `DocumentValidatorHandler` at `RequestFilterStagePreValidation`. The walker policy is *inverted*: it descends into every object-valued or array-of-object-valued field except an explicit list of data carriers (`enum`/`const`/`default`/`examples`/`required`/`dependentRequired`), so new JSON-Schema keywords (`if`/`then`/`contains`/`unevaluatedProperties`/...) cannot smuggle deep nesting or `$ref` past the validator. Rejection categories are exported as sentinel errors (`ErrResponseFormatDepth`, `ErrResponseFormatRef`, etc.); callers can match them via `errors.Is` even after the gateway wraps with HTTP 400 status. Test coverage: exhaustive unit tests in `paramvalidators/response_format_test.go`; integration wiring in `request_filters_test.go::TestNormalizeChatRequestResponseFormatPipeline`.

Calibrate the thresholds (depth, size, nodes) by running the new handler against the sample structured-output schemas from production clients before raising any limit.

### Other validators worth tightening (later)

- **`logit_bias`**: currently sanitized but does not cap the map size. A `logit_bias` with 100k entries is still forwarded. Suggest `len(map) ≤ 1024`.
- **`stop` / `bad_words`**: no cap on the number of entries or per-entry length. Suggest `len(list) ≤ 16`, per-entry length ≤ 256.
- **`tools`**: no cap on the schema depth/size of each `tools[].function.parameters`. The same depth/byte/key invariants from `response_format` should apply, because vLLM compiles tool argument schemas through the same grammar path.
- **`messages`**: total `messages` byte size has only the 10 MiB body cap. Long single-message content can still pass; the existing token-limit check happens later in the agent stack, not in this layer.
- **Catalog generation**: today the catalog is hand-written. As more fields land, it would be worth generating the doc above from the catalog (`go test` artifact or `go generate`) so this file does not drift.

## Performance characteristics

End-to-end `normalizeChatRequest` (Apple M2 Pro, `-benchtime=2s`):

| Body | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| Minimal (47 B) | 2,742 | 2,450 | 43 |
| Typical (132 B) | 4,311 | 2,947 | 67 |
| Heavy (560 B) | 16,039 | 11,695 | 209 |
| WithResponseFormat (279 B) | 9,437 | 6,776 | 139 |
| RejectedUnknown (72 B) | 1,869 | 2,146 | 36 |
| **RejectedRecursive (7.5 KiB attack)** | **1,058** | **72** | **2** |

`response_format` validator in isolation (`paramvalidators/response_format_test.go`):

| Path | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| Absent | 8.3 | 0 | 0 |
| `type=text` / `json_object` | ~17 | 0 | 0 |
| Simple schema | 1,645 | 640 | 19 |
| At limits (~121 nodes) | 52,825 | 24,701 | 724 |
| Rejects recursion attack | 559 | 96 | 2 |
| Rejects oversized schema | 19,207 | 18,905 | 15 |

Bench files: `request_filters_bench_test.go` (pipeline-level) and `paramvalidators/response_format_bench_test.go` (validator-level).

## Where the code lives

- Pipeline entry: `defaultChatRequestPipeline().Normalize` in `request_filters.go`.
- Catalog, generic handlers (Strip/Reject/Sanitize/Force/Custom), `RequestFilterContext`, `ChatRequestDocument`, and the `DocumentValidatorHandler` adapter: `request_filters_parameters.go`.
- Messages: `defaultChatMessageProcessor` in `request_filters_messages.go`.
- Per-field pure validators (one file per edge case, no coupling to the main pipeline types): `paramvalidators/` subpackage. Today: `response_format.go`. New validators (e.g. `tools_schema.go`, `logit_bias_bounds.go`) should go here and be wired into the catalog via `DocumentValidatorHandler{Validator: paramvalidators.YourValidator{...}}`. Each validator should expose sentinel errors for its rejection categories so callers can match them via `errors.Is`.
- Tests: integration in `request_filters_test.go`; per-validator unit tests in `paramvalidators/<name>_test.go`.
