# devshardctl

## Build

```bash
go build -o devshardctl ./cmd/devshardctl/
```

Local HTTP proxy that exposes an OpenAI-compatible API for devshard inference.
Users point any OpenAI client at `localhost:8080` and make chat completion requests; the proxy handles all devshard protocol complexity internally.

## Configuration

All settings can be passed as flags or environment variables. Flags take precedence over env vars.

| Flag | Env var | Required | Default | Description |
| ------ | ------ | ------ | ------ | ------ |
| `--private-key` | `DEVSHARD_PRIVATE_KEY` | yes | - | Hex-encoded secp256k1 private key |
| `--escrow-id` | `DEVSHARD_ESCROW_ID` | yes | - | On-chain escrow ID |
| `--chain-rest` | `DEVSHARD_CHAIN_REST` | no | `http://localhost:1317` | Chain REST API URL |
| `--model` | `DEVSHARD_MODEL` | no | `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` | Default model (used when request omits `model`) |
| `--port` | `DEVSHARD_PORT` | no | `8080` | Listen port |
| `--storage-path` | `DEVSHARD_STORAGE_PATH` | no | `~/.cache/gonka/devshard-<escrow-id>.db` | SQLite path for crash recovery |
| - | `DEVSHARD_API_KEYS` | no | - | Comma-separated public API bearer keys |
| - | `DEVSHARD_ADMIN_API_KEY` | no | - | Admin bearer key for finalize and `/v1/admin/*` endpoints |
| - | `DEVSHARD_CHAIN_ID` | no | queried from REST | Chain ID used when signing admin-created escrow transactions |
| - | `DEVSHARD_TX_FEE_AMOUNT` | no | `1000000` | Fee amount for admin-created escrow transactions |
| - | `DEVSHARD_TX_FEE_DENOM` | no | `ngonka` | Fee denom for admin-created escrow transactions |
| - | `DEVSHARD_TX_GAS_LIMIT` | no | `500000` | Gas limit for admin-created escrow transactions |
| - | `DEVSHARD_TX_POLL_TIMEOUT_MS` | no | `45000` | How long to wait for the create-escrow transaction result |
| - | `DEVSHARD_GATEWAY_DISABLED` | no | `false` | Return a synthetic chat completion response for all requests |
| - | `DEVSHARD_GATEWAY_DISABLED_MESSAGE` | no | `please use ... base url` | Message shown while the gateway is disabled |
| - | `DEVSHARD_ESCROW_ROTATION_ENABLED` | no | `false` | Enable automatic epoch escrow rotation |
| - | `DEVSHARD_ESCROW_ROTATION_PRIVATE_KEY_ENV` | when rotation enabled | - | Environment variable holding the creator/settler key |
| - | `DEVSHARD_ESCROW_ROTATION_AMOUNT` | no | `5000000000` | Amount locked in each automatically created escrow, in ngonka (5 GNK) |
| - | `DEVSHARD_ESCROW_ROTATION_MODEL_ID` | no | `DEVSHARD_MODEL` | Model used for automatically created escrows; defaults to the gateway Qwen model |
| - | `DEVSHARD_ESCROW_ROTATION_PRE_POC_BLOCKS` | no | `300` | Blocks before PoC to create temp bridge escrows |
| - | `DEVSHARD_ESCROW_ROTATION_TEMP_COUNT` | no | `8` | Number of temp bridge escrows |
| - | `DEVSHARD_ESCROW_ROTATION_TARGET_COUNT` | no | `16` | Number of regular escrows to create after PoC |

## Quick start

```bash
devshardctl \
  --private-key "deadbeef..." \
  --escrow-id 42 \
  --chain-rest "http://localhost:1317"

# In another terminal:
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","messages":[{"role":"user","content":"Hello"}],"max_tokens":100}'
```

Or using environment variables:

```bash
export DEVSHARD_PRIVATE_KEY="deadbeef..."
export DEVSHARD_ESCROW_ID="42"
export DEVSHARD_CHAIN_REST="http://localhost:1317"

devshardctl
```

## Finalize Escrow

```bash
curl -X 'POST' http://localhost:8080/v1/finalize \
  -H "Authorization: Bearer $DEVSHARD_ADMIN_API_KEY" \
  > ./settle.json
```

## Settle Escrow

```bash
./inferenced tx inference \
  settle-devshard-escrow \
  settle.json \
  --from dev1 \
  --keyring-backend file \
  --home ~/testnet-2 \
  --chain-id gonka-testnet-2 \
  --node http://localhost:26657 \
  --gas auto \
  --gas-adjustment 1.5 \
  --fees 500000ngonka -y
```

## Endpoints

### POST /v1/chat/completions

Standard OpenAI chat completion format. The full request body is forwarded as the inference prompt.

Request fields used by the proxy:

- `model` -- passed to InferenceParams (falls back to `DEVSHARD_MODEL`)
- `max_tokens` -- passed to InferenceParams (default 2048)
- `stream` -- if true, response is SSE; if false, response is a single JSON object

Returns 429 if another inference is already in flight.

### POST /v1/finalize

Admin endpoint. Triggers devshard finalization and returns settlement JSON.

No request body needed. Response is the settlement payload ready for `inferenced tx inference settle-devshard-escrow`.

### GET /v1/status

Returns current session state.

```json
{"escrow_id":"42","nonce":15,"phase":"active","balance":5000000000}
```

Phase values: `active`, `finalizing`, `settlement`.

### GET /v1/state

Admin endpoint. Returns the full session state and requires
`Authorization: Bearer $DEVSHARD_ADMIN_API_KEY`.

### POST /v1/admin/escrows

Admin endpoint. Creates a new on-chain devshard escrow by signing
`MsgCreateDevshardEscrow` locally and broadcasting the signed transaction to
`DEVSHARD_CHAIN_REST` via `/cosmos/tx/v1beta1/txs`. By default, the returned
escrow ID is also registered as an active local gateway runtime.

```bash
curl -X POST http://localhost:8080/v1/admin/escrows \
  -H "Authorization: Bearer $DEVSHARD_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"amount":5000000000,"model_id":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","private_key_env":"DEVSHARD_PRIVATE_KEY"}'
```

Set `"register": false` to create the escrow on-chain without adding it to the
local runtime pool.

### POST /v1/admin/devshards/{id}/settle

Admin endpoint. Locally deactivates the devshard, finalizes it if it is not
already in settlement phase, signs `MsgSettleDevshardEscrow`, and broadcasts the
signed transaction to `DEVSHARD_CHAIN_REST`.

```bash
curl -X POST http://localhost:8080/v1/admin/devshards/42/settle \
  -H "Authorization: Bearer $DEVSHARD_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"private_key_env":"DEVSHARD_PRIVATE_KEY"}'
```

If the request body omits `private_key` and `private_key_env`, the gateway uses
the key already persisted for that devshard. The endpoint returns `409` while
the devshard still has active requests.

### Automatic escrow rotation

Automatic rotation uses two roles:

- `regular` escrows carry normal traffic for an epoch.
- `temp` escrows are bridge escrows that keep capacity available through the
  PoC/epoch transition.

When `escrow_rotation.enabled` is true, the gateway watches the chain phase
snapshot from `DEVSHARD_PUBLIC_API`:

1. During inference phase, when the chain is within `pre_poc_blocks` of PoC,
   the gateway ensures `temp_count` temp escrows exist for the current epoch.
2. It then locally deactivates active non-temp escrows, finalizes them, and
   settles them on-chain through `DEVSHARD_CHAIN_REST`.
3. After the next epoch leaves PoC, it ensures `target_count` regular escrows
   exist for the new epoch.
4. It then deactivates, finalizes, and settles the previous epoch's temp
   escrows.

Rotation settings are persisted in `gateway.db`. After first boot, update them
through `POST /v1/admin/settings`:

```bash
curl -X POST http://localhost:8080/v1/admin/settings \
  -H "Authorization: Bearer $DEVSHARD_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "escrow_rotation": {
      "enabled": true,
      "private_key_env": "DEVSHARD_PRIVATE_KEY",
      "amount": 5000000000,
      "model_id": "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
      "pre_poc_blocks": 300,
      "temp_count": 8,
      "target_count": 16
    }
  }'
```

### Gateway disabled state

Set `DEVSHARD_GATEWAY_DISABLED=true` on first boot, or update
`disabled.enabled` through `POST /v1/admin/settings`, to make the gateway return
a normal OpenAI-compatible chat completion response for every request:

```json
{"id":"chatcmpl-gateway-disabled-...","object":"chat.completion","created":...,"model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8","choices":[{"index":0,"message":{"role":"assistant","content":"please use ... base url"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":5}}
```

The disabled settings are persisted in `gateway.db`:

```bash
curl -X POST http://localhost:8080/v1/admin/settings \
  -H "Authorization: Bearer $DEVSHARD_ADMIN_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"disabled":{"enabled":true,"message":"please use ... base url"}}'
```

### GET /metrics

Prometheus scrape endpoint. In the join-stack deployment, `devshardctl` is
published only on the host loopback address, so scrape it directly from the host:

```yaml
scrape_configs:
  - job_name: devshardctl
    static_configs:
      - targets: ["127.0.0.1:18080"]
```

Do not expose `/metrics` through the public nginx gateway. Public devshard
clients should use `/devshard-gateway/v1/...`; Prometheus should use
`http://127.0.0.1:18080/metrics`.

## OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="unused")
response = client.chat.completions.create(
    model="Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
    messages=[{"role": "user", "content": "Hello"}],
    max_tokens=100,
)
print(response.choices[0].message.content)
```

The `api_key` is required by the SDK but ignored by the proxy.

## Finalization and settlement

After all inferences are done:

1. POST to `/v1/finalize` with `Authorization: Bearer $DEVSHARD_ADMIN_API_KEY` -- the proxy runs the multi-phase finalization protocol, collects host signatures, and returns settlement JSON.
2. Submit the settlement on-chain: `inferenced tx inference settle-devshard-escrow settlement.json --from <user>`

The proxy holds the session open until finalization. Once finalized, the session cannot accept new inferences.

## Non-streaming vs streaming

Non-streaming (`"stream": false` or omitted): the proxy buffers all SSE chunks from the ML node and returns the final assembled JSON response.

Streaming (`"stream": true`): the proxy relays SSE `data:` lines in real time. The stream ends with `data: [DONE]`. Devshard protocol events (receipts, metadata) are filtered out -- only inference data reaches the client.

## Speculative execution

The proxy uses speculative execution to reduce tail latency and route around unresponsive hosts.

See `devshard/docs/speculative-proxy.md` for the detailed design and escalation rules.
