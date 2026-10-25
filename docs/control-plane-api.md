# Control Plane API

The control plane uses a caller-provided `run_id` as the stable identity of a temporary data plane. Ports are runtime details and are not used to identify history.

## Start the Server

```bash
llm-observer-proxy --host 127.0.0.1 --port 8790
```

The equivalent source invocation is:

```bash
go run ./cmd/llm-observer-proxy --host 127.0.0.1 --port 8790
```

The server reads `.env` from its working directory unless `--env-file` points somewhere else.

## Create a Proxy

```http
POST /api/runs/{run_id}/proxy
Content-Type: application/json
```

Example payload:

```json
{}
```

Fields:

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `bind_host` | string | `127.0.0.1` | Data-plane bind address |
| `port` | integer | automatic | Data-plane port from 1 through 65535 |
| `trajectory_metadata` | object | omitted | Optional string fields `agent_name` and `agent_version` for ATIF export |

Unknown fields are ignored. Compatibility fields including `model`, `mode`, `backend`, `litellm_config_path`, `litellm_env`, and `strip_callbacks` are accepted but do not alter behavior. The service always uses its embedded Bifrost data plane. A data plane serves every configured model, and each LLM request selects a model through the request's `model` field. OpenAI-compatible and Anthropic-compatible endpoints are available simultaneously.

A successful response uses status `201` and contains:

```json
{
  "backend": "bifrost",
  "run_id": "run-001",
  "port": 45678,
  "bind_host": "127.0.0.1",
  "base_url": "http://127.0.0.1:45678",
  "openai_base_url": "http://127.0.0.1:45678/v1",
  "anthropic_base_url": "http://127.0.0.1:45678",
  "config_path": "/path/to/artifacts/runs/run-001/platform/llm/config.json",
  "log_path": "/path/to/artifacts/runs/run-001/platform/llm/bifrost.log",
  "history_path": "/path/to/artifacts/runs/run-001/platform/llm/events.jsonl",
  "conversation_snapshot_path": "/path/to/artifacts/runs/run-001/platform/llm/conversation_snapshot.json",
  "history_count": 0,
  "state": "running"
}
```

`base_url` is always the protocol-neutral bare data-plane URL. Configure OpenAI-compatible clients with `openai_base_url`; configure Anthropic-compatible clients with `anthropic_base_url`. The corresponding HTTP endpoints include `/v1/chat/completions` and `/v1/messages`. `GET /` and `HEAD /` on the data plane return a local health response and are not recorded as LLM events.

## Get Proxy State

```http
GET /api/runs/{run_id}/proxy
```

Returns `200` for a running or process-local stopped proxy and `404` when the run has no known proxy record.

## Stop a Proxy

```http
DELETE /api/runs/{run_id}/proxy
```

Returns:

```json
{ "ok": true, "run_id": "run-001" }
```

Stopping a proxy does not delete its artifacts.

## Read History

```http
GET /api/runs/{run_id}/history
GET /api/runs/{run_id}/history?limit=20
```

`limit` is constrained to 1 through 200. The response contains `run_id`, `port`, `state`, `count`, `items`, `totals`, and an optional `latest_error`. After a control-plane restart, history remains discoverable from the artifact directory even though the former data-plane port is not restored.

## Read the Latest Conversation

```http
GET /api/runs/{run_id}/conversation
```

The response reconstructs the most recent observable exchange into messages, tool calls, usage totals, and an optional `latestError`.

## Export an ATIF Trajectory

```http
GET /api/runs/{run_id}/trajectory?format=atif&version=1.7
GET /api/runs/{run_id}/trajectory?strict=true
```

Returns an ATIF v1.7 JSON document as an attachment, reconstructed from the full
internal event log. Running runs export a snapshot; stopped and archived runs are
also supported. `format` and `version` default to the only supported profile,
`atif` / `1.7`. Unknown values return `400`. `strict` accepts `true` or `false`.
Empty histories and strict reconstruction failures return `422`; strict failures
include `diagnostics`. Read failures or corrupt complete JSONL records return
`500`. Missing history returns `404`.

Default mode preserves unsupported or ambiguous content under namespaced `extra`
with diagnostics. It never silently presents a partial reconstruction as complete.
See [trajectory export](trajectory-export.md) for the full contract and CLI.

## Debug List

```http
GET /api/proxies
```

This endpoint lists running and process-local stopped instances. Business logic should use the run-scoped endpoints instead.

## Health Check

```http
GET /healthz
```

Response:

```json
{ "ok": true }
```

The health check verifies the control-plane process. It does not contact the upstream provider or create a data plane.

## Error Statuses

- `400`: malformed request, invalid run ID, port, bind address, option type, or bind failure
- `404`: run-scoped proxy or history not found
- `409`: a data plane already exists for the run ID
- `500`: artifact history could not be read

Upstream data-plane errors retain the status and body returned through Bifrost whenever possible.

## Security Boundary

The API has no built-in authentication in version 0.1.x. A caller can start data planes and select their listening address. Keep it on a trusted network and read `.github/SECURITY.md` before deployment.
