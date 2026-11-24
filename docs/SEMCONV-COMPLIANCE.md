# OpenTelemetry GenAI semantic-convention compliance

**Pinned spec:** `open-telemetry/semantic-conventions-genai` **v0.1.0**
(Development status), verified 2026-11-24. The GenAI conventions moved out of
`open-telemetry/semantic-conventions`; `opentelemetry.io/docs/specs/semconv/gen-ai/`
now only redirects. The repository does not yet publish a schema URL, so the
instrumentation scope still carries the OTel core `semconv/v1.40.0` schema URL.
The spec is Development: names may change again. The
`OTEL_SEMCONV_STABILITY_OPT_IN` mechanism is **not** defined for the standalone
GenAI repo, and llmspan does not read it; llmspan instead overlaps old and new
names for one release (below).

## Spans (`chat {model}`, kind CLIENT)

Span name is `{gen_ai.operation.name} {gen_ai.request.model}`. The proxy starts
the span with the bare operation; the provider parser renames it on request
parse and the observer renames it again at finalize, so the exported name is
always spec-shaped (bare operation only if the model is unknown).

Emitted standard attributes: `gen_ai.provider.name`, `gen_ai.operation.name`,
`gen_ai.request.{model,temperature,top_p,top_k,max_tokens,frequency_penalty,presence_penalty,seed,stop_sequences,encoding_formats,stream}`,
`gen_ai.response.{id,model,finish_reasons,time_to_first_chunk}`,
`gen_ai.usage.{input_tokens,output_tokens,cache_read.input_tokens,cache_write.input_tokens,reasoning.output_tokens}`,
`gen_ai.conversation.id`, `error.type`, `server.address`, `server.port`.

Provider values: `openai`, `anthropic`, `gcp.gemini` (AI Studio),
`gcp.vertex_ai` (paths containing `/projects/.../locations/`). Operation
values: `chat`, `embeddings` (the OpenAI Responses API maps to `chat`).

## Metrics

| Metric | Status |
|---|---|
| `gen_ai.client.inference.duration`, `.time_to_first_chunk` | standard (v0.1.0) |
| `gen_ai.client.inference.operation.{input,output}_tokens` | standard, spec buckets |
| `gen_ai.client.inference.usage.{input,output,cache_read.input,cache_write.input,reasoning.output}_tokens` | standard counters |
| `gen_ai.client.embeddings.duration`, `.operation.input_tokens` | standard (embeddings only) |
| `gen_ai.client.operation.duration`, `gen_ai.client.token.usage` (`gen_ai.token.type`) | **deprecated**: pre-v0.1.0 names, still emitted for dashboard compatibility; the spec no longer allows them for inference |
| `gen_ai.client.time_to_first_token`, `gen_ai.server.time_to_first_byte` | **deprecated llmspan names**, superseded by `time_to_first_chunk` and `llmspan.upstream.time_to_first_byte` |
| `llmspan.upstream.time_to_first_byte` | llmspan extension (labels: upstream, operation, provider) |
| `gen_ai.client.cost.usd`, `gen_ai.client.cost.usd.total` | llmspan extension living in the reserved `gen_ai.` namespace (known deviation; rename to `llmspan.*` planned) |
| `llmspan.*` (requests, breaker, streams, pricing misses, TLS, export errors) | llmspan extension |

Standard metrics carry only spec attributes (`gen_ai.operation.name`,
`gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`,
`error.type` on duration/token histograms, `server.address`, `server.port`)
plus the extension `llmspan.caller` (cardinality-capped). The former `upstream`
and `http.response.status_code` labels were removed from them; per-upstream and
per-status breakdowns live on `llmspan.requests.total`. Explicit histogram
buckets follow the spec (duration 0.01..81.92 s x2; tokens 1..67108864 x4);
first-chunk and TTFB histograms add sub-10 ms buckets.

## Content capture (off by default, redacted)

`content.mode: events` sets `gen_ai.system_instructions`, `gen_ai.input.messages`
and `gen_ai.output.messages` span attributes (JSON; parts of type `text` /
`tool_call_response`). `content.mode: logs` emits `gen_ai.client.inference.operation.details`
log records with the same keys as structured values. Input and output are
emitted as two records (request time, response time) rather than one merged
record. Only text parts are captured: tool calls, images and reasoning parts
are not. Streamed outputs have no `finish_reason` in the message.

The deprecated per-message events (`gen_ai.user.message`, `gen_ai.choice`,
`gen_ai.tool.call`, ...) are emitted **only** with `LLMSPAN_LEGACY_GENAI_EVENTS=1`
and will be removed the release after next. OpenAI Responses tool-activity
markers are now `llmspan.responses.tool_event` unless that flag is set.

## Other known deviations

- `gen_ai.usage.cache_creation.input_tokens` (old name) is emitted alongside
  `cache_write.input_tokens` for one release.
- `gen_ai.cost.usd`, `gen_ai.time_to_first_token`, `gen_ai.server.time_to_first_byte`
  span attributes are llmspan extensions in the `gen_ai.` namespace.
- Time-to-first-chunk is measured from proxy request start, not including
  client-side retries.
- `service.instance.id` is a random UUID per process unless overridden via
  `OTEL_RESOURCE_ATTRIBUTES`.
- `OTEL_EXPORTER_OTLP_{,TRACES_,METRICS_,LOGS_}ENDPOINT` and `..._HEADERS`
  take precedence over config when set; `OTEL_EXPORTER_OTLP_PROTOCOL` is not
  read (config `telemetry.protocol` decides).
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=cumulative|delta|lowmemory`
  is honoured.
- The W3C `baggage` propagator is deliberately not enabled; inbound `Baggage`
  is stripped before forwarding.

## Sampling

See `docs/adr/0001-priority-sampling.md`. Decisions are made at export time per
span, but a trace containing any priority span (error, 5xx, costly, slow) is
promoted: sibling and ancestor spans in the same or later batches (bounded set,
8192 traces / 2 min) are kept too, so the llmspan span and its `otelhttp` parent
are not split. Spans from earlier batches that already left cannot be recalled;
use a collector tail-sampling processor for exact whole-trace semantics.
