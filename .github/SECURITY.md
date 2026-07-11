# Security Policy

## Supported Versions

Security fixes are applied to the latest release on the main branch.

## Deployment Boundary

LLM Observer Proxy 0.1.x is designed as a trusted administrative control plane. It does not authenticate control API requests. Do not expose the control port or dynamically allocated data-plane ports to an untrusted network.

Control API callers can:

- start and stop run-scoped data planes;
- select a bind address and port; and
- cause raw LLM traffic to be stored under the configured artifact root.

Run the service as an unprivileged user, bind to loopback by default, restrict filesystem permissions, and place network authentication in front of it if remote access is required.

## Sensitive Data

The artifact root stores raw LLM requests, responses, visible reasoning, tool calls, and logs. Treat it as sensitive data. Prefer environment references over literal credentials in catalogs. Incoming `Authorization` and `x-api-key` headers are redacted, but request and response bodies are retained intentionally.

## Reporting a Vulnerability

Use the repository host's private security-advisory mechanism. Do not open a public issue containing credentials, prompts, responses, or exploit details.
