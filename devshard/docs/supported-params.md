# Chat Completions — supported parameters

Reference for `POST /v1/chat/completions` on devshard. Documents which OpenAI/vLLM request fields the gateway accepts today, which are rejected, how to use the ones that pass, and what we still need to tighten.

## Why a strict whitelist

vLLM crashes on malformed or pathological requests (deep recursive JSON Schema, unsupported routing fields, unbounded objects). To keep the inference node healthy:

1. **Body-level depth scan** (`ensureRequestNestingDepth` in `request_filters_parameters.go`). A byte-level pass bounds whole-request JSON nesting at `MaxRequestNestingDepth = 32` before `encoding/json` allocates anything. Rationale and the legitimate-request depth budget live in the code comment over the constant. This is a separate layer from the schema-level `MaxDepth = 5` cap below: the body limit is generous because legitimate requests stack ~10 levels of wrappers; the schema limit is tight because the grammar compiler is the actual attack surface.
2. Every inbound `/chat/completions` body is then decoded into a generic JSON document.
3. `VLLMParameterCatalog` (`devshard/cmd/devshardctl/request_filters_parameters.go`) is a closed allow-list. The set of allowed keys is precomputed at catalog construction (no per-request map build). Any top-level field that is not in the catalog is rejected with `feature "<name>" is temporarily unavailable` (HTTP 400) before the request reaches the model.
4. Parameter rules run in two stages:
   - `PreValidation` — on the raw document, before we decode/validate it.
   - `PostLimits` — after `max_tokens` defaults/caps are resolved.
5. The message validator (`request_filters_messages.go`) enforces the OpenAI-compatible message contract (roles, tool_call linkage, text-only content parts).
6. The chat-request projection (`chatRequest` — 5 fields: `model`, `stream`, `max_tokens`, `max_completion_tokens`, `n`) is populated by direct map reads from the document, not a `json.Marshal + Unmarshal` round-trip.

Anything not on this whitelist does not reach the model. That is the contract.

## Status at a glance

### ✅ Implemented

**Schema-walking validators** — same vLLM grammar-compiler attack surface. All apply depth/nodes/size/branch/enum bounds, ban `$ref`/`$defs`/`definitions`, validate `type` is a JSON-Schema primitive, and compile-check `pattern` regex with a length cap (defuses CVE-2025-48944):
- `response_format` — `paramvalidators.ResponseFormatValidator`
- `tools[].function.parameters` — `paramvalidators.ToolsValidator`
- `chat_template_kwargs` — `paramvalidators.ChatTemplateKwargsValidator` (plain object, no JSON-Schema semantics; additionally rejects keys that override `apply_hf_chat_template()` positional args: `chat_template`, `tokenize`, `tools`, `documents`, `conversation`, `continue_final_message`, `padding`, `truncation`, `max_length`, `return_tensors`, `return_dict` — defuses CVE-2025-61620 + CVE-2025-62426)

**Shape validators**:
- `thinking` — `paramvalidators.ThinkingValidator`
- `stream_options` — `paramvalidators.StreamOptionsValidator` (whitelist `include_usage`; strip `continuous_usage_stats` for vLLM-project/vllm#9028 + any other sub-field; drop the field if it empties out)
- `metadata` — `paramvalidators.MetadataValidator` (OpenAI bounds: ≤16 keys × 64-char keys × 512-char string values)
- `user` — `paramvalidators.UserValidator` (must be a string, byte-length ≤ 512)

**Length / size caps**:
- `messages` (≤2048) · `stop` (≤16/256B) · `stop_token_ids` (≤64) · `bad_words` (≤64/128B) · `logit_bias` map (≤1024 entries)

**Numeric range sanitizers**:
- `temperature` (≤2.0) · `top_p` · `top_k` · `min_p` · `repetition_penalty` (≤2.0) · `n` (≤5) · `logit_bias` values (`[-100, 100]`)

**Type validators**:
- `seed` — non-negative uint64
- `tool_choice` — rejects `"required"`

**Pipeline / forced**:
- `max_tokens` / `max_completion_tokens` — defaults + caps via `applyOutputTokenLimits`
- `min_tokens` — conditional strip + clamp
- `logprobs` / `top_logprobs` — forced
- `messages` contract — `defaultChatMessageProcessor.ValidateDocument`
- Body-level: `MaxRequestNestingDepth=32`, `MaxChatRequestBodySize=10 MiB`

**Pass-through (safe by type contract)**:
- `model` · `stream` · `skip_special_tokens` · `detokenize` · `parallel_tool_calls`

### ❌ Open / not yet implemented

Field-level additions that Kimi K2/K2.6 actually accepts on the wire (per Moonshot's [chat reference](https://platform.kimi.ai/docs/api/chat) and [K2.6 quickstart](https://platform.kimi.ai/docs/guide/kimi-k2-6-quickstart)):

- **`frequency_penalty`, `presence_penalty`** — Kimi accepts these but **for K2.6 only `0.0`** is valid (any other value is rejected upstream; that's a model-side constraint, not a security one). Easy add via `SanitizeFloatParameterHandler` with range `[-2, 2]`. Smallest concrete next step.
- **`prompt_cache_key`** — string. Kimi's first-class context-cache tag (cheaper re-prompts). Add with a string-length cap (e.g. ≤256 B).
- **`safety_identifier`** — Kimi's analogue to OpenAI's `user`. String pass-through; add over `user` (which Kimi does not document).
- **`messages[].reasoning_content`** — *required* on assistant turns during multi-step tool-calling when thinking is enabled. The message validator already does NOT reject unknown assistant-message fields (only `tool_call_id` is disallowed on assistant), so this already passes through. Just document the contract; no code change needed.

Structural additions:

- **Per-message content byte size cap** — currently bounded only by the 10 MiB body cap; no per-message structural limit. Token-limit logic downstream catches the most pathological cases.
- **Catalog → spec generator** — this doc is hand-written. `go generate` could derive the Supported table from `defaultVLLMParameterCatalog()` so it cannot drift.

Verification / operational:

These constraints are enforced by the vLLM engine configuration, not the gateway. The gateway cannot filter them — confirm on the serving side.

**Engine version**
- **Pin vLLM ≥ 0.20.0** — CVE-2026-44223 lets a single `repetition_penalty ≠ 1.0` (or any `frequency_penalty` / `presence_penalty`) crash EngineCore on vLLM 0.18.0–0.19.1 when `extract_hidden_states` speculative decoding is on. Confirm the deployed image's vLLM version before raising the cap on these fields. If the engine is older, the gateway must also reject `repetition_penalty ≠ 1.0` as a fallback. ([advisory](https://github.com/vllm-project/vllm/security/advisories/GHSA-83vm-p52w-f9pw))

**Tool-call parser selection (all served models)**
- **Disable `pythonic` tool-call parser** — CVE-2025-48887 catastrophic-backtracking ReDoS on the output path. The gateway can't block this; the engine must not run with `--tool-call-parser pythonic`. ([advisory](https://github.com/vllm-project/vllm/security/advisories/GHSA-w6q7-j642-7c25))

**Per-model operational requirements**

- **`Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`**
  - Tool-call parser must be `hermes` (the model's default). Do NOT run with `--tool-call-parser qwen3_coder` — CVE-2025-9141 yields RCE via `eval()` on attacker-controlled tool arguments. The `qwen3_coder` parser is intended for `Qwen3-Coder` only. ([advisory](https://github.com/vllm-project/vllm/security/advisories/GHSA-79j6-g2m3-jgfw))
  - Speculative decoding with `extract_hidden_states` must be off (see vLLM version note above) — otherwise `repetition_penalty` (currently allowed by the gateway) becomes a one-shot kill (CVE-2026-44223).
  - Reasoning parser `qwen3` and hermes tool parser are known to surface 500s on certain Qwen3 output patterns (multiple JSON blobs, `<tool_call>` inside `<think>`). These are server-side parsing bugs the gateway can't prevent — they degrade availability, not security. Track upstream issues (see References).

- **`moonshotai/Kimi-K2.6`** (text-only)
  - No model-specific operational constraints beyond the engine-version and `pythonic` parser notes above. The model is text-only, so CVE-2026-44222 (VL special-token IndexError) does not apply.

### 🚫 Won't add (out of scope or by design)

- **`model` length cap** — redundant with 10 MiB body cap; minimal benefit on a routing-lookup field.
- **`extra_body` / `extra_headers`** — open-ended escape hatches; breaks the strict-whitelist contract.
- **Provider-specific fields without vLLM semantics**: `service_tier`, `store`, `reasoning_effort`, `reasoning`, `thinking_config`, `enable_thinking`, etc. See "Unsupported parameters" below for the full categorized list. (Note: `user`, `metadata`, `parallel_tool_calls`, and `stream_options` are now in the Supported table above — they are OpenAI Chat Completions standard observability fields with no inference-side semantics and no DoS vector; rejecting them broke every OpenAI-built client without security gain. `prompt_cache_key`, `safety_identifier`, `messages[].reasoning_content` remain in "❌ Open" — Kimi K2.6 actually supports them, pending validator work.)
- **vLLM-native structured-output bypass** (`guided_json`, `guided_regex`, `guided_grammar`, `guided_choice`, `enforced_tokens`) — would need their own validators; `response_format` covers the same intent with bounds.
- **Special-token literal stripping in `messages[].content`** (CVE-2026-44222) — text like `<|vision_start|>` crashes vision-language models with an `IndexError`. Kimi-K2.6 is text-only so we are not exposed today. If we route to a VL model in the future, add a content sanitizer at this layer. ([advisory](https://github.com/vllm-project/vllm/security/advisories/GHSA-hpv8-x276-m59f))

## Supported parameters

| Field | Category | Stage / rule | Behavior |
| --- | --- | --- | --- |
| `model` | string | — | Required. Pass-through. Selects the upstream model id. |
| `stream` | bool | — | Pass-through. Streaming is enabled when `true`. |
| `messages` | object array | message validator + PreValidation length cap (`LengthCapListParameterHandler`) | Required, non-empty. `len(messages) ≤ 2048` (way above any realistic conversation; defense against JSON-parse memory amplification, independent of token count). Strict OpenAI-compatible message contract — see "Messages" section below. |
| `max_tokens` | int range | PostLimits (token-limit pipeline) | If absent → set to `DefaultRequestMaxTokens`. Capped at `RequestMaxTokensCap` unless the request carries admin auth. When both `max_tokens` and `max_completion_tokens` are set, the smaller wins and both are aligned. |
| `max_completion_tokens` | int range | PostLimits (token-limit pipeline) | Same rules as `max_tokens`. |
| `n` | int range | PostLimits sanitize (`CapUintParameterHandler`) | Capped at `MaxChatRequestChoices` (5). |
| `temperature` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. Capped at `MaxTemperature` (2.0). String-encoded numbers accepted. |
| `top_p` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. No upper cap (forwarded raw). |
| `top_k` | int range | PostLimits sanitize | Strips `NaN`/`±Inf`. vLLM-specific. |
| `min_p` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. vLLM-specific. |
| `repetition_penalty` | float range | PostLimits sanitize | Strips `NaN`/`±Inf`. Capped at `MaxRepetitionPenalty` (2.0). |
| `logit_bias` | int→float map | PostLimits sanitize (`SanitizeFloatMapParameterHandler`) | Keeps numeric entries in `[-100, 100]`. Drops the field if the map is empty after sanitization. Map size capped at 1024 entries (rejected with HTTP 400 over the cap). |
| `stop` | string list | PreValidation length cap (`LengthCapListParameterHandler`) | Pass-through within bounds. Rejected if `len(stop) > 16` or any entry's string length > 256 bytes. |
| `stop_token_ids` | int list | PreValidation length cap (`LengthCapListParameterHandler`) | Pass-through within bounds. Rejected if `len(stop_token_ids) > 64`. |
| `seed` | int range | PreValidation type-check (`ValidateUintParameterHandler`) | Must parse as a non-negative integer that fits in uint64. Otherwise rejected with HTTP 400 at the gateway boundary instead of relying on the upstream's error path. |
| `skip_special_tokens` | bool | — | Pass-through. vLLM-specific. |
| `detokenize` | bool | — | Pass-through. vLLM-specific. |
| `thinking` | object | PreValidation validate (`paramvalidators.ThinkingValidator`) | Must be `{"type": "enabled" \| "disabled"}`. Any other shape rejected. For `moonshotai/Kimi-K2.6` it is mirrored into `chat_template_kwargs.thinking` via `applyKimiRequestOverrides`. |
| `chat_template_kwargs` | object | PreValidation validate (`paramvalidators.ChatTemplateKwargsValidator`) | Pass-through within plain-object bounds: depth ≤ 5, nodes ≤ 128, serialized size ≤ 16 KiB. Top-level keys that override `apply_hf_chat_template()` positional arguments are rejected (`chat_template`, `tokenize`, `tools`, `documents`, `conversation`, `continue_final_message`, `padding`, `truncation`, `max_length`, `return_tensors`, `return_dict` — defuses CVE-2025-61620 + CVE-2025-62426). `add_generation_prompt` is explicitly *allowed* as a legitimate template knob. |
| `tool_choice` | string | PreValidation reject | Pass-through except `"required"` — rejected because the upstream contract does not honor it on the vLLM path. Stripped together with `tools` if `tools` arrives empty. |
| `min_tokens` | int range | PreValidation conditional strip + PostLimits clamp | Dropped if `stop_token_ids` is also set (vLLM rejects the combination). Otherwise clamped to ≤ `max_tokens`. |
| `bad_words` | string list | PreValidation sanitize + length cap | Empty/whitespace strings are removed. Field is dropped if the resulting list is empty. Then `len(bad_words) ≤ 64`, per-entry length ≤ 128 bytes. |
| `tools` | object array | PreValidation sanitize + shape + schema validate (`paramvalidators.ToolsValidator`) | Empty arrays drop both `tools` and `tool_choice`. Each tool must declare `type: "function"` and a `function` object with a non-empty string `name` — the OpenAI tool contract — otherwise rejected before vLLM ever sees the request. If `function.parameters` is present, it is walked as a JSON Schema with bounds depth ≤ 16, nodes ≤ 128, branch arms ≤ 16, enum ≤ 256, size ≤ 16 KiB; `$ref`/`$defs`/`definitions` forbidden; `type` must be a JSON-Schema primitive; `pattern` must compile as a regex within 512 B — CVE-2025-48944. Parameter-less tools are valid. Depth limit is wider than `response_format` (16 vs 5) because production agent tool schemas (Cursor, Claude Code, Kilo, MCP servers, OpenClaw) routinely sit at 5–12 levels (OpenClaw `message` tool reaches 12 with `presentation.blocks[].buttons[].label`); 16 gives headroom over observed max, while MaxNodes=128, MaxSize=16 KiB, MaxBranch=16, and the `$ref`/`$defs` ban provide the actual schema-bomb defense. |
| `logprobs` | bool | PostLimits force | Forced to `true` for the gateway's observability pipeline regardless of what the client sent. |
| `top_logprobs` | int range | PostLimits force | Forced to `5` for the same reason. |
| `response_format` | object | PreValidation validate (`paramvalidators.ResponseFormatValidator`) | Accepted with bounds. `type` must be one of `text` / `json_object` / `json_schema`. For `json_schema`, the schema is walked once and rejected if depth > 5, nodes > 128, branch arms (anyOf/oneOf/allOf) > 16, enum > 256, serialized size > 16 KiB, or contains `$ref` / `$defs` / `definitions`. Every schema node's `type` must be a JSON-Schema primitive (`string`/`number`/`integer`/`object`/`boolean`/`array`/`null` or an array of those); `pattern` is byte-capped at 512 B and must compile as a regex (defuses CVE-2025-48944). |
| `user` | string | PreValidation validate (`paramvalidators.UserValidator`) | OpenAI-standard end-user identifier for abuse tracking; no inference-side semantics on the vLLM upstream, ignored on the wire. Must be a string; byte-length ≤ 512 (covers production identifiers — `user_<random>`, UUIDs, hashed ids, email-shaped, framework session ids — while preventing the field from being used as a 10 MiB body-size carrier). |
| `metadata` | object | PreValidation validate (`paramvalidators.MetadataValidator`) | OpenAI-standard tracking object (LangSmith, distributed tracing, A/B tagging). Bounded to ≤16 keys × 64-char keys × 512-char string values — the same limits the OpenAI API itself enforces. Values must be strings; any other type rejected. |
| `parallel_tool_calls` | bool | — | Pass-through. OpenAI-standard tool-calling control. Some vLLM versions honor it, some ignore it — the client opted in to that behavioral divergence; no DoS surface. |
| `stream_options` | object | PreValidation validate (`paramvalidators.StreamOptionsValidator`) | Sub-field whitelist. Only `include_usage` survives — the OpenAI-documented opt-in for a final-chunk `usage` object, required by any streaming client that needs token accounting. `continuous_usage_stats` is stripped (triggers vLLM-project/vllm#9028: per-chunk usage counter is wrong under chunked prefill). Any other / future sub-field is also stripped. If the object empties out, the field is dropped entirely so it never reaches the upstream as `{}`. |

### Messages contract (recap)

Enforced by `request_filters_messages.go`:

- Roles: `developer`, `system`, `user`, `assistant`, `tool`, `function`.
- Assistant `content` may be empty/null only when `tool_calls` or `function_call` is present.
- Tool messages require `tool_call_id` matching a prior assistant `tool_calls[].id`.
- Function messages require `name`.
- Content parts: only `{"type": "text", "text": "..."}` is accepted. Typed arrays of text parts are flattened to a single string before forwarding.
- Empty tool `content` is normalized to a sentinel string; missing tool `content` is also normalized.

## Unsupported parameters (rejected)

Every OpenAI / vLLM / provider-specific field that is **not** in the Supported table is rejected at `PreValidation`. The error response is HTTP 400 with body `feature "<name>" is temporarily unavailable`.

Grouped by why the field is off:

| Group | Examples we reject today | Origin | Why off |
| --- | --- | --- | --- |
| Sampling penalties | `frequency_penalty`, `presence_penalty` | Kilo, OpenClaw, OpenAI canonical | vLLM supports them. Trivially safe to add via `SanitizeFloatParameterHandler` with range `[-2, 2]`. Pending only because we have not validated end-to-end on the vLLM build we run. |
| Structured-output (non-`response_format`) | `guided_json`, `guided_regex`, `guided_grammar`, `guided_choice`, `enforced_tokens`, `structured_outputs` | vLLM-native | Bypasses the response_format safety bounds. Same grammar-compiler attack surface; would need its own validator. |
| Tool-calling extensions | `tool_choice="required"` | OpenClaw, OpenAI | `required` is rejected by upstream contract on the vLLM path (cost-amplifier + engine-wedge on Kimi-K2). `parallel_tool_calls` is now passthrough — see Supported table. |
| Routing / metadata | `service_tier`, `extra_body`, `extra_headers`, `provider`, `plugins`, `tags`, `seed_override` | Hermes, OpenClaw, Kilo | Vendor-specific. `extra_body` / `extra_headers` are open-ended escape hatches and break the whitelist contract. (`user` and `metadata` were moved to Supported — they are OpenAI Chat Completions standard observability fields, not vendor-specific.) |
| Caching | `store`, `prompt_cache_key` | OpenAI / OpenClaw | OpenAI-specific server-side state hints. No vLLM semantics. (`stream_options` was moved to Supported with a sub-field whitelist validator — it is required for OpenAI-compatible streaming-with-usage and the only known-bad sub-field, `continuous_usage_stats`, is stripped at the gateway.) |
| Reasoning (non-vLLM-native) | `reasoning_effort`, `reasoning` (object), `thinking_config`, `enable_thinking` | Hermes (multi-provider), OpenClaw (Qwen/Kimi), Gemini | Use `thinking` + `chat_template_kwargs` instead. Provider-specific reasoning fields are wrapper concerns, not what vLLM speaks. |
| Provider-specific extras | `extra_body.vl_high_resolution_images` (Qwen), `extra_body.provider` (OpenRouter), `extra_body.tags` (Nous), `extra_body.thinking_config` (Gemini), `extra_body.plugins` (OpenRouter), `messages[].reasoning_content` (Kimi multi-turn replay), `tools[].function.strict` (OpenAI strict schema), `chat_template_kwargs.preserve_thinking` (Qwen wrapper) | Hermes, OpenClaw | Single-vendor attribution / wrapping fields. No vLLM equivalent. |

If you see a field here that you need, add it to the catalog with the smallest safe rule — see "Validation improvements".

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

## Design notes

### `response_format` invariants (reference)

The motivating attack that drove the validator design — a JSON Schema with ~200 nested `properties` levels would crash vLLM:

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

Implementation: `paramvalidators.ResponseFormatValidator` in `cmd/devshardctl/paramvalidators/response_format.go`, wired into the main catalog via `DocumentValidatorHandler` at `RequestFilterStagePreValidation`. The walker policy is *inverted*: it descends into every object-valued or array-of-object-valued field except an explicit list of data carriers (`enum`/`const`/`default`/`examples`/`required`/`dependentRequired`), so new JSON-Schema keywords (`if`/`then`/`contains`/`unevaluatedProperties`/...) cannot smuggle deep nesting or `$ref` past the validator.

### Shared schema-walking infrastructure

Schema-walk rejection categories are exposed as sentinel errors: `ErrSchemaDepth`, `ErrSchemaNodes`, `ErrSchemaSize`, `ErrSchemaRef`, `ErrSchemaEnum`, `ErrSchemaBranch`, `ErrSchemaType`, `ErrSchemaPattern`. They are shared by `SchemaBounds` (JSON-Schema-aware, used for `response_format` and `tools[].function.parameters`) and `ObjectBounds` (plain-object, used for `chat_template_kwargs`).

Calibrate the thresholds (depth, size, nodes) by running the validators against the sample structured-output schemas from production clients before raising any limit.

## Performance characteristics

End-to-end `normalizeChatRequest` (Apple M2 Pro, `-benchtime=2s`, `-count=3`):

| Body | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| Minimal (47 B) | 3,140 | 2,428 | 41 |
| Typical (132 B) | 4,963 | 2,926 | 65 |
| Heavy (~620 B) | 21,054 | 12,951 | 240 |
| WithResponseFormat (279 B) | 10,372 | 6,663 | 137 |
| RejectedUnknown (72 B) | 1,948 | 2,148 | 34 |
| **RejectedDeepBody (7.5 KiB body-depth attack)** | **1,093** | **96** | **2** |
| RejectedRecursiveSchema (walker reject) | 8,147 | 7,928 | 100 |

`Heavy` carries `chat_template_kwargs` so `ChatTemplateKwargsValidator` runs. `RejectedDeepBody` trips the body-level pre-scan (depth > 32) before any validator. `RejectedRecursiveSchema` reaches the walker (body depth under cap, schema depth over `MaxDepth=5`) and pays for `json.Unmarshal` — ~8× more expensive than pre-scan reject.

`response_format` validator in isolation (`paramvalidators/response_format_bench_test.go`):

| Path | ns/op | B/op | allocs/op |
| --- | --- | --- | --- |
| Absent | 8.7 | 0 | 0 |
| `type=text` / `json_object` | ~18 | 0 | 0 |
| Simple schema | 1,763 | 536 | 19 |
| At limits (~121 nodes) | 56,734 | 21,252 | 724 |
| Rejects recursion attack | 1,215 | 176 | 4 |
| Rejects oversized schema | 17,402 | 514 | 17 |

`CheckSize` switched from `json.Marshal(schema)` to `json.NewEncoder` over a counting `io.Writer`: same byte total, no marshaled buffer allocation. Saves -14% B/op on `At limits` and -97% on `Rejects oversized` (the 17 KiB schema no longer materializes).

Bench files: `request_filters_bench_test.go` (pipeline-level) and `paramvalidators/response_format_bench_test.go` (validator-level).

## References

Re-check these periodically for status changes (fixed-in versions, new variants, advisory updates). Sorted by relevance to this gateway.

### Security advisories covered by current filters

| ID | Title | Coverage in this gateway |
| --- | --- | --- |
| [CVE-2025-48944](https://nvd.nist.gov/vuln/detail/CVE-2025-48944) ([vLLM advisories index](https://github.com/vllm-project/vllm/security/advisories)) | xgrammar crash on invalid `type` or pattern | `SchemaBounds.validateSchemaTypeField` + `validateSchemaPatternField`. Walked on every `response_format` and `tools[].function.parameters` node. |
| [CVE-2025-61620](https://nvd.nist.gov/vuln/detail/CVE-2025-61620) | `chat_template_kwargs` Jinja injection | `ChatTemplateKwargsValidator.forbiddenChatTemplateKwargsKeys` denylist (`chat_template`, …). |
| [CVE-2025-62426](https://nvd.nist.gov/vuln/detail/CVE-2025-62426) | `tokenize=True` stalls the request handler | Same key denylist (`tokenize` rejected). |
| [CVE-2026-34756 / GHSA-3mwp-wvh9-7528](https://github.com/vllm-project/vllm/security/advisories/GHSA-3mwp-wvh9-7528) | Unbounded `n` causes OOM | `CapUintParameterHandler` clamps `n ≤ 5`. |

### Security advisories handled outside the gateway (ops-side)

| ID | Title | Handling |
| --- | --- | --- |
| [CVE-2025-9141 / GHSA-79j6-g2m3-jgfw](https://github.com/vllm-project/vllm/security/advisories/GHSA-79j6-g2m3-jgfw) | RCE via `eval()` in `qwen3_coder` tool-call parser | Per-model ops constraint: keep `hermes` parser on Qwen3-235B-Instruct. |
| [CVE-2025-48887 / GHSA-w6q7-j642-7c25](https://github.com/vllm-project/vllm/security/advisories/GHSA-w6q7-j642-7c25) | ReDoS in `pythonic` tool-call parser | Engine must not run with `--tool-call-parser pythonic`. |
| [CVE-2026-44223 / GHSA-83vm-p52w-f9pw](https://github.com/vllm-project/vllm/security/advisories/GHSA-83vm-p52w-f9pw) | Penalty fields crash EngineCore with `extract_hidden_states` spec decode | Pin vLLM ≥ 0.20.0 or keep that spec-decode method disabled. |
| [CVE-2026-44222 / GHSA-hpv8-x276-m59f](https://github.com/vllm-project/vllm/security/advisories/GHSA-hpv8-x276-m59f) | Special-token literals (`<\|vision_start\|>`, etc.) crash VL models | N/A for Kimi-K2.6 (text-only). For Qwen3-VL or future multimodal routing, add a content sanitizer. |

### Qwen3 upstream issues to monitor

Output-side parser bugs the gateway cannot prevent — track upstream fix status to know when 500-rate from these is expected to drop:

- [vllm-project/vllm#17790](https://github.com/vllm-project/vllm/issues/17790) — hermes parser `JSONDecodeError` on multiple tool-call blobs
- [vllm-project/vllm#27447](https://github.com/vllm-project/vllm/issues/27447) — `enable_thinking=false` breaks guided decoding
- [vllm-project/vllm#29814](https://github.com/vllm-project/vllm/issues/29814) — Qwen3 reasoning parser edge cases
- [vllm-project/vllm#39677](https://github.com/vllm-project/vllm/issues/39677) — structured output + thinking interaction
- [vllm-project/vllm#40875](https://github.com/vllm-project/vllm/issues/40875) — related guided-decoding/thinking bug
- [vllm-project/vllm#42021](https://github.com/vllm-project/vllm/issues/42021) — `<tool_call>` inside `<think>` breaks hermes parsing

### Model documentation

- [Moonshot AI — Kimi Chat API](https://platform.kimi.ai/docs/api/chat)
- [Moonshot AI — Kimi K2.6 quickstart](https://platform.kimi.ai/docs/guide/kimi-k2-6-quickstart)
- [Qwen3-235B-A22B-Instruct-2507 model card](https://huggingface.co/Qwen/Qwen3-235B-A22B-Instruct-2507-FP8)

### Upstream projects

- [vLLM security advisories](https://github.com/vllm-project/vllm/security/advisories) — review on engine upgrades.
- [vLLM releases](https://github.com/vllm-project/vllm/releases) — verify the running image's `__version__` here against the advisory "fixed in" notes above.
