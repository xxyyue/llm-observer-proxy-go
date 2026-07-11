# Contributing

Use Go 1.26.4 or newer. Older Go installations with `GOTOOLCHAIN=auto` can download the toolchain declared in `go.mod` automatically.

Before submitting a change, run:

```bash
test -z "$(gofmt -l cmd internal)"
go mod tidy
git diff --exit-code -- go.mod go.sum
go vet ./...
go test -race ./...
go build -trimpath ./cmd/llm-observer-proxy
```

Changes to HTTP paths, status codes, JSON field names, artifact layout, or the installed `llm-observer-proxy` command are compatibility changes. Add or update contract tests and document their compatibility impact.

Never commit provider keys, raw production histories, or generated run artifacts.

## Dependency Changes

The data plane embeds Bifrost Core and its official OpenAI and Anthropic provider packages. When changing Bifrost or another runtime dependency, run the real control/data-plane integration test in `internal/control` in addition to the unit suite.

Bifrost's management server, UI, database, plugins, governance features, and MCP runtime are outside this project's compatibility contract. Do not add their dependencies unless the project starts using and testing those features directly.
