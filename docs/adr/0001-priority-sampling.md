# ADR 0001: Outcome-aware priority sampling

Status: accepted (2026-07-16)

## Decision

llmspan records spans in-process and applies a stateless export-time filter.
The filter always retains:

- spans with an OpenTelemetry error status or `error.type`;
- HTTP 5xx spans;
- spans at or above `telemetry.cost_sample_threshold_usd`;
- spans at or above `telemetry.slow_sample_threshold`.

All other spans use the deterministic `telemetry.sample_ratio` decision based
on trace ID. This preserves stable sampling without retaining active traces in
memory.

## Why not head sampling

Cost, response status, provider error type, and total latency do not exist when
the span starts. Hoisting request parsing above span creation can estimate
maximum output cost, but it cannot know cached-token discounts, actual output
tokens, errors, or latency. Using that estimate as the only sampling decision
would preferentially keep large requested limits rather than expensive calls.

## Trace coherence (amendment, 2026-11)

A trace containing a priority span is promoted in a bounded in-memory set
(8192 trace IDs, two generations, ~2 min). Spans of that trace seen later, or
in the same batch, are kept even if they individually fail the checks, so the
llmspan span and its `otelhttp` parent stay together. A span exported before its
trace is promoted is not recalled; with `sample_ratio < 1` the ratio decision is
trace-ID based and consistent across spans of one trace.

## Why this is not full tail sampling

The in-process filter makes a per-span decision and does not buffer a complete
trace. Cross-service "keep the entire trace if any span failed" semantics
require a collector tail-sampling processor. Operators needing that behavior
should export llmspan spans to an OpenTelemetry Collector and configure its
tail-sampling processor. The local filter remains useful as a bounded fallback.

## Consequences

`sample_ratio: 0` can still export priority spans. The SDK records all spans,
so CPU cost is higher than head sampling, but memory remains bounded by the
existing batch processor and span limits. Metrics and logs are unaffected.
