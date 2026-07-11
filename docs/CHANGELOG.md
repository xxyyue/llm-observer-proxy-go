# Changelog

All notable changes to this project will be documented in this file.

## 0.1.0 - 2026-07-11

- Introduce a run-scoped LLM observability control plane implemented in Go.
- Embed Bifrost Core's OpenAI and Anthropic providers without Docker or a separate Bifrost process.
- Establish the compatible control API, normalized event schema, and run artifact layout.
- Preserve complete OpenAI and Anthropic request and response payloads, including Anthropic system content arrays.
- Serve every catalog model through both supported wire protocols on each data-plane port.
- Return a bare `base_url` together with explicit OpenAI and Anthropic client base URLs.
- Keep create requests forward-compatible by accepting legacy fields and ignoring unknown fields.
- Add standalone model catalog, environment, testing, and installation documentation.
