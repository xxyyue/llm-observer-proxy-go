# LLM Observer Proxy

[![CI](https://github.com/xxyyue/llm-observer-proxy-go/actions/workflows/ci.yml/badge.svg)](https://github.com/xxyyue/llm-observer-proxy-go/actions/workflows/ci.yml)

LLM Observer Proxy runs an embedded Bifrost data plane for each evaluation or agent run and stores its observable LLM traffic under a stable `run_id`. The observer records requests, responses, streaming output, tool calls, token usage, cost estimates, errors, and provider-visible reasoning content. It does not require Python, LiteLLM, Docker, or a separate Bifrost process.

It is independently installable and can be embedded into any evaluation platform, agent runner, or orchestration service through its HTTP API.

## How It Works

There are two HTTP layers:

1. The **control plane** listens on port `8790` by default. An orchestrator uses it to create, inspect, and stop run-scoped proxies.
2. Each **data plane** listens on a separate, automatically allocated port. An agent uses the returned URL as an OpenAI-compatible or Anthropic-compatible LLM endpoint.

One control-plane process can manage multiple concurrent data planes using goroutines. Stopping a data plane preserves its history on disk, and history remains readable after the control plane restarts.

The observer can only record information exposed by the upstream API. It cannot obtain hidden chain-of-thought that a provider does not return.

## Requirements

- Go 1.26.4 or newer, as required by Bifrost Core
- An upstream API key and model catalog
- Local permission to bind control-plane and data-plane ports

Older Go installations with `GOTOOLCHAIN=auto` can download the toolchain declared in `go.mod` automatically.

## Install

Install the command from GitHub:

```bash
go install github.com/xxyyue/llm-observer-proxy-go/cmd/llm-observer-proxy@latest
```

The repository and module path are named `llm-observer-proxy-go`; the installed command and service are named `llm-observer-proxy`. Ensure `$(go env GOPATH)/bin` is on `PATH`, then verify the installation:

```bash
llm-observer-proxy --version
```

For a source checkout, run or build the same command:

```bash
go run ./cmd/llm-observer-proxy
go build -o llm-observer-proxy ./cmd/llm-observer-proxy
```

## Quick Start

The embedded fallback catalog includes `gpt-5.5` and reads the OpenAI credential from the environment:

```bash
export OPENAI_API_KEY=your-openai-api-key
llm-observer-proxy
```

In another terminal, check the control plane:

```bash
curl http://127.0.0.1:8790/healthz
```

Create a data plane for a run:

```bash
curl -X POST http://127.0.0.1:8790/api/runs/run-001/proxy \
  -H 'Content-Type: application/json' \
  -d '{}'
```

The data plane serves every model in the selected catalog. Each LLM request selects a model through its own `model` field. The create response includes a dynamically allocated bare `base_url`, for example `http://127.0.0.1:45678`, plus `openai_base_url` (`http://127.0.0.1:45678/v1`) and `anthropic_base_url` (`http://127.0.0.1:45678`). Use the explicit URL for the client SDK, or call an endpoint directly:

```bash
curl http://127.0.0.1:45678/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.5",
    "messages": [{"role": "user", "content": "Reply with one short sentence."}]
  }'
```

Read the normalized history:

```bash
curl http://127.0.0.1:8790/api/runs/run-001/history
```

Stop the data plane without deleting its history:

```bash
curl -X DELETE http://127.0.0.1:8790/api/runs/run-001/proxy
```

## Configuration

The command accepts these options:

```text
--host HOST                 Control-plane bind host (default: 127.0.0.1)
--port PORT                 Control-plane port (default: 8790)
--env-file PATH             KEY=VALUE file loaded before startup (default: .env)
--artifact-root PATH        Artifact storage root
--model-catalog PATH        Model catalog JSON
--log-level LEVEL           Bifrost log level
--version                   Print the build version
```

Paths are resolved in this order:

1. `--artifact-root` and `--model-catalog`
2. `LLM_OBSERVER_PROXY_ARTIFACT_ROOT` and `LLM_OBSERVER_PROXY_MODEL_CATALOG_PATH`
3. `./artifacts` and `./config/models.json` in the working directory
4. The binary's secret-free embedded model catalog

Relative paths are resolved against the process working directory. The optional `.env` file is loaded before environment-based path and credential resolution, and existing process environment values take precedence.

### Bifrost Data Plane

The service embeds Bifrost Core `v1.6.3` and the official OpenAI and Anthropic provider packages. It does not start the full Bifrost server, a Docker container, or another process. The data plane keeps the public OpenAI-compatible `/v1/*` routes and Anthropic-compatible `/v1/messages` route, and records exact request and response payloads in `events.jsonl`, including Anthropic system content blocks and cache-control markers.

The HTTP path selects the request protocol: `/v1/messages` uses the Anthropic adapter, while other `/v1/*` paths use the OpenAI adapter. Every catalog model is accepted through either format so an upstream that supports both formats can translate as needed.

### Model Catalog

A model catalog has this shape:

```json
{
  "llm_observer_proxy": {
    "base_url": "https://api.openai.com",
    "api_key": "os.environ/OPENAI_API_KEY",
    "network_config": {
      "max_retries": 6
    }
  },
  "models": [
    {
      "id": "gpt-5.5",
      "upstream_api": "openai"
    }
  ]
}
```

`id` is the public model name accepted by the data plane. `upstream_api` must be `openai` or `anthropic` for catalog compatibility, but the request path selects the adapter at runtime. Optional per-million-token prices under `pricing` are used for local cost estimates. `api_key` accepts a literal value, `os.environ/NAME`, `env.NAME`, or `${NAME}`. All catalog models currently share one upstream `base_url` and credential. `llm_observer_proxy.network_config` is passed to Bifrost; `max_retries` defaults to `6` when omitted and may be explicitly set to `0` to disable gateway retries. See `config/models.example.json` for a complete example.

The legacy top-level catalog key `llm_proxy` remains accepted when `llm_observer_proxy` is absent.

## Control API

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Check control-plane health |
| `GET` | `/api/proxies` | List process-local running and stopped proxies |
| `POST` | `/api/runs/{run_id}/proxy` | Create a run-scoped data plane |
| `GET` | `/api/runs/{run_id}/proxy` | Read proxy state |
| `DELETE` | `/api/runs/{run_id}/proxy` | Stop a data plane |
| `GET` | `/api/runs/{run_id}/history` | Read normalized history |
| `GET` | `/api/runs/{run_id}/conversation` | Read the latest conversation |
| `GET` | `/api/runs/{run_id}/trajectory` | Export an ATIF v1.7 trajectory |

The create payload may be empty. Optional active fields are `port`, `bind_host`, and `trajectory_metadata`. Compatibility fields including `model`, `mode`, `backend`, `litellm_config_path`, `litellm_env`, and `strip_callbacks` are accepted and ignored, as are unknown fields. The complete request and response contract is documented in `docs/control-plane-api.md`.

## ATIF Export

Keep `events.jsonl` as the internal log and export a standard ATIF v1.7 trajectory
through HTTP or offline from an existing run:

```bash
curl --fail 'http://127.0.0.1:8790/api/runs/run-001/trajectory?format=atif&version=1.7' -o trajectory.atif.json
llm-observer-proxy export --events /path/to/events.jsonl --output trajectory.atif.json
```

Export reconstructs observed history across requests and links visible tool results.
Add `strict=true` to HTTP or `--strict` to CLI to reject ambiguous/incomplete
reconstruction. Default exports include explicit diagnostics under
`extra.llm_observer_proxy`. Original event logs remain unchanged.
See [trajectory export](docs/trajectory-export.md) for metadata, accounting,
compatibility limits, and validation.

## Artifacts

Each run writes:

```text
artifacts/runs/<run_id>/platform/llm/
  config.json
  conversation_snapshot.json
  events.jsonl
  bifrost.log
```

`events.jsonl` retains raw requests and responses, streaming bodies, usage, tool calls, visible reasoning, errors, and normalized summaries. `conversation_snapshot.json` contains the latest reconstructed conversation. `config.json` contains non-secret run metadata, and `bifrost.log` is retained as a compatibility path.

Incoming `Authorization` and `x-api-key` headers are redacted in artifacts. Request and response bodies are intentionally retained and may contain sensitive data, so apply appropriate filesystem permissions, access control, and retention policies to the artifact root.

## Supported Scope

The supported data-plane surface is OpenAI-compatible `/v1/*` routes and Anthropic `/v1/messages`, plus the control and history APIs documented above. Bifrost's management server, administration UI, database persistence, plugins, governance features, other providers, and MCP runtime are not part of this project's compatibility contract. Bifrost's shared schemas package imports MCP types, so `mcp-go` remains a transitive compile dependency even though no MCP code is configured or executed.

## Troubleshooting

- **Proxy creation returns `400`:** check the run ID, requested port, bind address, catalog, and filesystem permissions.
- **Proxy creation returns `409`:** the `run_id` already has a running data plane.
- **An LLM request fails:** confirm the upstream `base_url`, credential environment variable, request path, and model name. Provider errors are recorded in `events.jsonl`.
- **History is empty:** confirm the agent is using the returned data-plane URL, not the provider URL directly.
- **`llm-observer-proxy` is not found after installation:** add `$(go env GOPATH)/bin` to `PATH`.

## Security

The control API is an administrative interface: it can start listeners, select listening ports, and store LLM traffic. Version 0.1.x has no built-in authentication. Bind it to loopback or a trusted private network and read `.github/SECURITY.md` before remote deployment.

History can contain prompts, model output, tool output, and credentials. Apply appropriate filesystem permissions, access control, and retention policies to the artifact root.

## Development

```bash
test -z "$(gofmt -l cmd internal)"
go mod tidy
git diff --exit-code -- go.mod go.sum
go vet ./...
go test -race ./...
go build -trimpath ./cmd/llm-observer-proxy
```

## License

Apache-2.0. See `LICENSE` and `NOTICE`.

## Authors

- Shuqiao Zhang <stevenjoezhang@gmail.com>
- Yue Xiong <xxyyyue@gmail.com>
