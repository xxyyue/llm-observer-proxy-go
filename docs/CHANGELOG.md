# Changelog

All notable changes to this project will be documented in this file.

## Unreleased

- Preserve every captured request through standard ATIF context checkpoints and exact typed input manifests, with evidence-based context relationships and operational records.
- Export validated standard image parts with deduplicated resources, portable JSON/ZIP and run-scoped HTTP resource retrieval.
- Capture optional producer context hints and stable response exchange IDs without forwarding reserved headers.

- Add ATIF v1.7 trajectory downloads and offline `export` command while retaining
  `events.jsonl` and the existing history/conversation formats.
- Reconstruct sequential text trajectories, associate tool results, preserve
  recorded estimates, and report incomplete or ambiguous reconstruction.
- Add optional persisted agent metadata and strict export validation.
- Verify synthetic exports with pinned official Harbor models in CI.

## 0.1.0 - 2026-07-11

- Introduce a run-scoped LLM observability control plane implemented in Go.
- Embed Bifrost Core's OpenAI and Anthropic providers without Docker or a separate Bifrost process.
- Establish the compatible control API, normalized event schema, and run artifact layout.
- Preserve complete OpenAI and Anthropic request and response payloads, including Anthropic system content arrays.
- Serve every catalog model through both supported wire protocols on each data-plane port.
- Return a bare `base_url` together with explicit OpenAI and Anthropic client base URLs.
- Keep create requests forward-compatible by accepting legacy fields and ignoring unknown fields.
- Add standalone model catalog, environment, testing, and installation documentation.
