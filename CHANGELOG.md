# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-11-24

Initial public release.

### Features

- Reverse proxy for OpenAI-, Anthropic- and Gemini-compatible APIs (Chat
  Completions, Responses, Messages, `generateContent`, including Vertex-shaped
  paths). Requests and responses are forwarded untouched; usage is parsed from
  a side copy, including SSE streams and compressed responses.
- OTLP export (gRPC or HTTP) of traces, metrics and logs using the
  OpenTelemetry GenAI semantic conventions (`semantic-conventions-genai`
  v0.1.0), with the pre-v0.1.0 metric names still emitted for one release.
  `telemetry.endpoint` accepts a URL (`https://host/otlp`) for backends that
  serve OTLP under a path, such as the Grafana Cloud OTLP gateway.
- Cost estimation from a built-in price table with per-model overrides,
  long-context and service-tier multipliers, and a pricing-miss counter.
- Caller attribution (named auth tokens, mTLS identity, or a configurable
  header), conversation grouping (`gen_ai.conversation.id`) and Grafana Cloud
  Agent Observability support (`agento11y.record`), verified end to end for
  OpenAI-compatible traffic, streaming and non-streaming.
- Optional prompt/completion capture as span attributes or log records, off by
  default, with redaction profiles (best-effort, not DLP).
- Redacted JSONL recording with `replay` and `diff` commands.
- Trace-coherent priority sampling that keeps errors, slow and expensive calls.

### Operations

- Safe defaults: a non-loopback listener requires TLS (or `allow_insecure`) and
  client identity (tokens or mTLS, or `allow_unauthenticated`); per-upstream
  concurrency caps, a global cap on buffered request bodies, and per-upstream
  circuit breakers are on unless disabled.
- Bearer-token auth with argon2id hashes (`llmspan hash-token`), TLS and mTLS
  with hot certificate reload, upstream SPKI pinning.
- `/healthz`, `/readyz` (independent of collector reachability by default),
  graceful drain with `http.shutdown_drain_delay`, optional Prometheus
  `/metrics` on a separate admin listener, self-health metrics for the breaker,
  rejects, streams and pricing misses. OTLP export health (error count and
  last-success time) is also served from process memory on the admin listener,
  so a collector outage stays visible.
- Every error llmspan generates itself is a JSON `llmspan_proxy_error` with a
  stable `code`, distinguishable from the provider's own errors.
- A weekly `pricing-freshness` workflow opens an issue when the price snapshot
  is older than 90 days.
- Helm chart, Kubernetes manifests, Docker image (distroless, non-root), a
  Grafana dashboard and starter alert rules.
- `llmspan demo` (mock upstream), `make demo` (keyless full stack) and
  `make demo-real` (local model through Ollama), `llmspan check` (config
  validation), `llmspan gen-cert`.
- Signed releases with cosign, SLSA provenance and CycloneDX SBOMs.

### Deprecated

- Metrics `gen_ai.client.operation.duration`, `gen_ai.client.token.usage`,
  `gen_ai.client.time_to_first_token`, `gen_ai.server.time_to_first_byte`, the
  `gen_ai.usage.cache_creation.input_tokens` attribute and the per-message
  content events (behind `LLMSPAN_LEGACY_GENAI_EVENTS=1`) are kept for one
  release for compatibility with older dashboards and will then be removed.

[Unreleased]: https://github.com/llmspan/llmspan/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/llmspan/llmspan/releases/tag/v0.2.0
