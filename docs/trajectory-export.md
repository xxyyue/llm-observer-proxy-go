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
values. Add `strict=true` to reject ambiguous or incomplete reconstruction.

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
A successful best-effort export prints its warning count to stderr.

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

## Reconstruction and compatibility

Supported text surfaces are OpenAI Chat Completions, OpenAI Responses, and
Anthropic Messages, including their ordinary JSON and SSE responses. The exporter
reads the full event history, not the latest conversation snapshot.

- Request context is matched against the previous observed context by position
  and structured content; matching prefixes and retained suffixes are reused.
  It does not globally deduplicate text. Repeated real turns remain separate.
- Initial supplied history is marked `origin: request_context` and
  `observed_response: false`. It has no invented timestamp or per-call usage.
- Tool results seen in later requests attach to the agent step with the matching
  unique call ID. Missing, duplicate or conflicting IDs are diagnosed. A tool call
  without a later visible result has unknown execution outcome.
- Responses `previous_response_id` can link incremental inputs. Unknown references
  and branches from older responses are diagnosed. Changed or unlinked request
  contexts remain under `extra`, while earlier observed steps remain intact.
- If all request start times are present, events are sorted by start time with a
  stable tie-break. Overlaps are reported; this is not a claim of causal ordering
  among concurrent agents. Missing times retain source-log order.
- The final Responses stream object takes precedence over accumulated deltas.
  Tool argument strings must parse to JSON objects. Broken arguments remain in
  diagnostics/unmapped metadata, never silently become an empty object.
- Only readable reasoning returned by the provider becomes `reasoning_content`.
  Signatures and encrypted/redacted reasoning are not invented readable thoughts.
- Known usage is counted once per recorded exchange. Anthropic cache-read and
  cache-creation tokens are included in ATIF's total prompt tokens. Missing usage
  is omitted, with coverage recorded in `final_metrics.extra`.
- Recorded local cost estimates are preserved without repricing, with provenance.
  They are not provider billing records; historical cache-pricing inaccuracies
  are not silently corrected. Partial totals are explicitly labeled by coverage.
- HTTP errors remain error evidence under `extra`, not fabricated assistant text.
  Known usage/cost on failed calls is included in aggregate metrics with an explicit
  note. Bifrost-internal retries are not individually observable, so the exporter
  does not claim an exact count of upstream inferences.

All extensions live under `extra.llm_observer_proxy` (including step/metrics extra).
The root contains diagnostics, unmapped fragments, source/state information and
`reconstruction_complete`. This flag concerns reconstruction of the visible log,
not completion of the user's task or completeness of unobserved agent activity.

Default mode exports representable steps and preserves unsupported fragments in
`extra`. Strict mode rejects semantic warnings, including ambiguous contexts,
missing tool results, failed/incomplete streams and unsupported content. Both
modes use the same ATIF field types. An empty history cannot produce a valid
trajectory and is rejected. Optional metrics and unknown agent identity alone do
not make a trajectory invalid.

The first release does not package image/audio assets or infer multi-agent trees.
Unsupported content is retained with diagnostics and rejected in strict mode.
This is an observed-traffic export, not a guarantee of replayability or a complete
agent runtime trace. Original HTTP headers and wire payloads remain in the source
log; an ATIF export is not a lossless replacement for that log.

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
