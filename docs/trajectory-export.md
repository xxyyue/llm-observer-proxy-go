# ATIF trajectory export

The proxy keeps `events.jsonl` as its internal append-only event log. Export is an
explicit read operation; it never rewrites or deletes source events and never
calls an LLM provider. Both HTTP and CLI export use the same reconstruction code.
Existing history and conversation APIs keep their original formats.

## HTTP download

```bash
curl --fail 'http://127.0.0.1:8790/api/runs/run-001/trajectory?format=atif&version=1.7' \
  -o trajectory.atif.json
```

The response is an ATIF document directly, with `Content-Type: application/json`
and `Content-Disposition: attachment; filename="trajectory.atif.json"`.
`format=atif` and `version=1.7` are the defaults and currently the only supported
values. Add `strict=true` to reject unsupported or lossy representation.

Status codes:

- `200`: valid ATIF document; inspect its reconstruction diagnostics.
- `400`: unsupported format/version, malformed strict option, or invalid run ID.
- `404`: run history does not exist.
- `422`: no exportable conversation steps, or strict reconstruction rejected.
- `500`: unreadable/corrupt event log or corrupt stored run metadata.

An existing run's export remains available after stopping the data plane and after
restarting the control plane. Running exports are snapshots of complete records
within the file size at open; an unterminated final line is excluded and reported.
Corrupt complete lines cause an error in either mode. A stopped proxy does not
prove that the agent's task or external tool executions completed.

## Offline CLI

```bash
llm-observer-proxy export \
  --events /path/to/events.jsonl \
  --format atif \
  --format-version 1.7 \
  --agent-name my-agent \
  --agent-version 1.0.0 \
  --output /path/to/trajectory.atif.json
```

`--events` and `--output` are required. `--strict` enables strict reconstruction.
`--run-id` supplies an identity when source events have none; it must agree with
any recorded run IDs. The command does not load `.env`, a model catalog or provider
credentials. Output is written atomically with restrictive file permissions only
after successful reconstruction. Existing output is replaced on success; failed
exports preserve it. Output cannot be the source log or a symlink/hardlink alias.
A successful export prints its diagnostic count to stderr, including informational observations.

CLI metadata is supplied by flags and does not implicitly read neighboring files.
Missing agent name/version are explicitly `unknown`. The proxy itself is not
presented as the agent that generated the trajectory.

## Optional run metadata

To populate HTTP exports, supply metadata when creating a run:

```json
{
  "trajectory_metadata": {
    "agent_name": "my-agent",
    "agent_version": "1.0.0"
  }
}
```

These optional strings are stored in the run's non-secret `config.json` and survive
control-plane restarts. Older run configs without metadata still export, with
unknown identity. No credentials belong in this object.

## Captured inputs, context boundaries and execution history

OpenAI Chat Completions, Responses and Anthropic Messages support ordinary JSON
and text/tool SSE responses. Complete buffered JSON remains valid even when the
request asked for streaming. Export reads the full log, retaining all earlier
observed responses when the agent replaces, prunes or summarizes its context.

Each source record has a standard ATIF semantic context checkpoint immediately before its
response or operational event. `step.extra.context_management` has
`boundary: "replace"`; its observation contains the supported text/image messages and tool context.
The default `type: "snapshot"` is an export checkpoint, not a claim that the runtime
performed compaction. Structural prefix/suffix matches consider all prior
completed contexts, explicit response IDs, producer hints and tool relationships.
Interleaved auxiliary calls can therefore return to a prior context. Unlinked
inputs use `replacement`; retained suffixes use `pruning`. Adopted summaries use
`compaction` only with an exact prior output in a recognized continuation wrapper,
or an explicit producer hint. Mere summary keywords or a clear-thinking
configuration do not prove that compaction occurred. Parent candidates remain
explicit when evidence cannot identify a unique relationship.

The standard replacement rule discards earlier steps from the current input,
without deleting them from the execution history. Every call has its own checkpoint,
including ordinary continuations. This prevents a later discovered tool result,
backfilled onto an earlier tool-call step, from leaking into intervening inputs.
Only new interactions on a known continuation get extra standalone turns; retained
history is not reclassified as new observed agent calls. Initial supplied assistant
history has `observed_response: false` and no invented usage or timestamps.

The export contains a semantic ATIF projection, not a copy of the event log.
Standard viewers can display text/images and apply replacement boundaries. Each
observation result's `extra.context_message` retains only the role, interpreted tool
calls/result ID and readable reasoning needed to understand that context. Consumers
implementing `ordered_context_messages_v1` recover this semantic message projection;
developer roles remain distinguishable. Anthropic tool-result envelopes and adjacent
Responses assistant items can be flattened by the projection.

`extra.llm_observer_proxy` is limited to lightweight identities, provenance,
relationship evidence, numeric accounting and static diagnostics. `input_context_id`
and `configuration_id` are hashes for matching/change detection, not references to
embedded request data. There is no request/configuration pool, raw request body,
raw response/error payload, provider result/usage copy or unmapped fragment dump.
Tools available to the provider, arbitrary generation parameters, cache-control
configuration and provider-specific envelopes remain solely in `events.jsonl`.
Exact provider request replay requires the original log. This intentionally gives
up the earlier exporter's duplicated full-request manifests.

Provider-side context transforms, tokenization and opaque server state are not
observable. Explicit parent references are evidence only. A typed request/body
mismatch produces `captured_body_differs`; the projection follows the captured typed
record and makes no wire-fidelity claim. Unsupported semantic content is diagnosed
by code/source position, remains in the original log, and fails strict mode. No
multi-agent hierarchy or verified provider inference count is fabricated.

Tool results attach to a unique originating call in the same ATIF step. If no
unique origin exists, the result remains in its semantic input checkpoint with an
informational diagnostic. Missing results stay unknown. Start times order records
when all are available; overlaps remain explicit. Stable `source_record_index`
identifies original log positions even when IDs are reused or rows are duplicated.
HTTP failures, empty responses and reused IDs have explicit operational system
steps. Partial outputs retain their partial state. These are faithful observations,
not automatically representation failures.

Known token usage is counted once per source record. OpenAI prompt totals already
include caches; Anthropic input totals add cache reads/creation. Metrics coverage
is explicit. Stored cost estimates are preserved without repricing and are not
verified bills. Duplicate logging may duplicate aggregate source-record totals.
Bifrost-internal retries are not separately observable.

Diagnostics have `severity: info` or `error`. Strict mode rejects representation
loss: invalid tool arguments, unsupported/malformed media or content, invalid usage,
malformed stream payloads and torn log tails. Ordinary context operations, unknown
causal parents, missing outcomes and captured HTTP failures do not fail strict when
represented faithfully. `semantic_projection_complete`/`reconstruction_complete` refer to
supported ATIF semantic mapping, never exact provider request reconstruction,
completion of the agent task or full hidden model context. Audio and structured output-stream media without an
assembled supported representation are diagnosed and rejected by strict mode.

## Images and portable resources

Standard ATIF `ContentPart` images are supported in mixed messages and tool results:
Anthropic base64/URL blocks, OpenAI image_url/data URLs and Responses input_image.
PNG, JPEG, GIF and WebP are decoded for validation, never re-encoded; original bytes
are deduplicated by SHA256. Limits are 16 MiB per inline image, 256 MiB unique assets
per export, 16 million pixels and 16384 pixels per dimension. Invalid media and
untrusted local path references are rejected rather than read from the filesystem.

Remote HTTP(S) references are never downloaded. Supply explicit `media_type` in the
source or use `--image-media-type image/png` / HTTP `image_media_type=image/png`
when the caller knows an otherwise untyped reference's MIME. Filename inference is
reported as uncertain and rejected by strict mode. An opaque URL is not assigned
an invented JPEG MIME. Original inline data remains in the source log; the ATIF export contains image resource references.

CLI JSON publishes a portable `<output filename>.assets-<unique suffix>/images/...` directory and
then atomically replaces JSON. New resources are rolled back on publication failure;
existing artifacts/source files are protected. Keep the generated directory beside the JSON; subsequent successful exports may leave earlier immutable resource directories for existing readers. `--package zip` publishes one atomic
archive containing `trajectory.json` and `images/...`, with relative content paths.
Files use mode 600 and generated directories 700. Resource keys are generated,
confined and checked against symlink/path traversal attacks.

HTTP JSON references resources at the same serving origin under
`/api/runs/{run_id}/trajectory/assets/images/{digest}.{ext}`. URLs use the request
Host and direct connection's HTTP/TLS scheme; forwarding headers are not trusted.
When a reverse proxy changes that origin, download `?package=zip` for a standalone
portable artifact. JSON URLs require the control server and retained run assets;
ZIP is the recommended format for archival/sharing. Assets are scoped to the run
and only digest-validated image files are served; arbitrary run files are inaccessible.

## Explicit recording hints

Clients may provide these optional request headers on future calls:

| Header | Meaning |
| --- | --- |
| `X-LLM-Observer-Conversation-ID` | Producer's conversation/branch identity |
| `X-LLM-Observer-Agent-ID` | Producer's agent identity within the run |
| `X-LLM-Observer-Parent-Exchange-ID` | Known predecessor returned by this proxy |
| `X-LLM-Observer-Context-Operation` | `snapshot`, `compaction`, `pruning`, `replacement`, `restore`, or `injection` |

Each value is limited to 256 bytes and must occur once. Invalid values/operations
return 400. All reserved `X-LLM-Observer-*` headers are removed before forwarding.
The response's `X-LLM-Observer-Exchange-ID` is generated before forwarding, cannot
be overridden upstream, and equals the ID stored in the event. Optional
`context_metadata` is backward-compatible with old events. These are producer
claims/evidence, not independently verified runtime facts. Do not put secrets in
metadata headers. Stored headers/wire details still belong in `events.jsonl`;
ATIF does not replace that internal log.

## Version and validation

The output profile is fixed to `ATIF-v1.7`. It does not float with upstream Harbor
changes or claim support for v1.8 audio. Official compatibility checks are pinned
to Harbor commit `0533a59c41ce435d9e59ff8c82da67f6e5b6edc7`:

- [ATIF specification](https://github.com/harbor-framework/harbor/blob/0533a59c41ce435d9e59ff8c82da67f6e5b6edc7/rfcs/0001-trajectory-format.md)
- [Official validator](https://github.com/harbor-framework/harbor/blob/0533a59c41ce435d9e59ff8c82da67f6e5b6edc7/src/harbor/utils/trajectory_validator.py)

Generate synthetic fixtures and run the same official check as CI:

```bash
ATIF_FIXTURE_DIR=/tmp/atif-fixtures go test -count=1 ./internal/trajectory
uv run --with pydantic==2.12.3 python scripts/validate_atif.py /tmp/atif-fixtures
```

The validation script downloads only the pinned official model/validator files.
Python, Pydantic and this download are development/CI dependencies, not service or
CLI runtime requirements. Tests cover exact step order, tool-result placement,
metrics, streaming, archived runs, malformed logs and preservation of source files.
