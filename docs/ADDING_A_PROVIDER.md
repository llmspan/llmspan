# Adding a provider

A provider is one Go file that teaches llmspan the wire format of one LLM API:
which paths are LLM calls, and how to read model, parameters, tokens, finish
reasons and (optionally) content out of requests, JSON responses and SSE
streams. Everything else (auth, routing, breaker, spans, metrics, cost,
redaction, recording) is shared and needs no changes.

The worked example is `internal/provider/gemini.go` (about 200 lines). Read it
next to this page. `anthropic.go` and `openai.go` are good second references
for SSE-heavy and multi-endpoint APIs.

## What you will touch

| Step | File |
|---|---|
| Parser | `internal/provider/<name>.go` (new) |
| Registration | `internal/provider/provider.go`, `BuiltIn()` |
| Provider name constant | `internal/genai/genai.go` |
| Prices | `internal/pricing/prices.yaml` (and `pricingSystem` in `internal/proxy/observer.go` if the key differs) |
| Tests and fixtures | `internal/provider/<name>_test.go`, `internal/provider/testdata/` |
| Docs | `config.example.yaml` comment, `docs/SEMCONV-COMPLIANCE.md`, `CHANGELOG.md`, README provider list |

## 1. The interface

```go
type Provider interface {
    System() string
    OperationFor(urlPath string) string
    ParseRequest(span trace.Span, urlPath string, body []byte, content ContentOpts) Info
    ParseResponseJSON(span trace.Span, info *Info, body []byte, content ContentOpts)
    WrapStream(span trace.Span, info *Info, body io.ReadCloser, content ContentOpts, onDone func()) io.ReadCloser
}
```

Implementations are stateless value types (`type Gemini struct{}`); all
per-call state lives in `Info`. End the file with a compile-time assertion:
`var _ Provider = (*Gemini)(nil)`.

Hard rules that apply to every method:

- **Never fail the request.** Parsers run on the data path. On malformed
  input, populate what you can and return. Gemini's `ParseRequest` returns
  the path-derived `Info` when JSON decoding fails. Never panic; a panic in a
  parser is a production outage.
- **Do not retain `body`.** It belongs to the proxy.
- **Content is opt-in.** Touch prompt or completion text only inside
  `if content.Capture()`, and only through the `content.emit*` helpers, which
  apply redaction. Never put content in an error string or log line.
- **Bound everything.** Do not accumulate unbounded text. Use the shared
  stream collector, which caps its buffers.

## 2. Path matching: `OperationFor`

`OperationFor(path)` returns a GenAI operation name (`genai.OpChat`,
`genai.OpEmbeddings`) or `""`. `""` means "not an LLM call": the proxy forwards
it untouched with no span. This is the safe default, so be strict.

Gemini matches the last path segment containing `:generateContent` or
`:streamGenerateContent`. Use the helpers in `paths.go`:

- `pathSegments` rejects trailing slashes and cleans the path.
- `isAPIParent` requires a version segment (`v1`) or an Azure
  `deployments/{name}` pair before the operation, so
  `/v1/files/messages` or `/.well-known/messages` do not match.

Add negative cases to your test (sub-resource collisions, trailing slash,
unrelated paths). Operations are matched on the path after the proxy has
canonicalised it and applied `strip_prefix`, so do not hardcode the llmspan
route prefix.

## 3. `Info`: what to fill in

`Info` (in `provider.go`) is the vendor-neutral result. Zero means unknown and
unknown fields are simply not reported.

| Field | Source | Notes |
|---|---|---|
| `System` | `genai` constant | The `gen_ai.provider.name` value. Gemini returns `gcp.vertex_ai` instead of `gcp.gemini` for Vertex-shaped paths (`/projects/.../locations/...`), so one parser can report two providers |
| `Operation` | `OperationFor` | |
| `RequestModel` | body, or path (Gemini) | |
| `ResponseModel`, `ResponseID` | response | |
| `FinishReasons` | response | Keep the provider's own strings |
| `InputTokens`, `OutputTokens` | usage block | **Output must be what the provider bills.** Gemini folds `thoughtsTokenCount` into output because it is billed as output, and reports `ReasoningOutputTokens` as the subset |
| `CacheReadInputTokens`, `CacheCreationInputTokens` | usage block | Check whether the provider counts them inside or outside input tokens, and normalise to match `pricing.CostDetailed` (see `docs/PRICING.md`) |
| `ErrorType` | error body | Short, bounded, never a message |
| `Stream` | request | Gemini derives it from the `:streamGenerateContent` path |
| `Params` | request | Pointer fields so "absent" differs from zero |
| `ServiceTier` | response | Only if the provider has billing tiers |

Request attributes are emitted for you by calling `emitRequestAttrs(span,
&info)` in `ParseRequest`; call `emitResponseAttrs(span, info)` after merging a
response. Do not set `gen_ai.*` attributes by hand.

## 4. Streaming

`WrapStream` returns a reader that passes bytes through unchanged while a
callback sees each SSE event. Use the shared collector:

```go
c := newStreamCollector(span, info, content, onDone)
return c.wrap(body, func(eventName string, data []byte) {
    // decode data, merge usage into info, then:
    c.noteFirstToken()  // first content delta -> time to first chunk
    c.appendText(delta) // only matters when content capture is on
})
```

The collector owns the SSE framing (`sse.go`), the event size cap, TTFT
bookkeeping, and calling `onDone` exactly once on EOF, error or close. Your
callback must be cheap, must not block, and must tolerate partial and garbage
events (return silently). Usage in a stream is often cumulative or arrives on
the last chunk only; merge, do not append. Count a tool or function call as a
"first token" (Gemini does this for `functionCall` parts) so tool-only
responses still get a TTFT.

If the provider does not use SSE (NDJSON, for example), you will need a
different framer. Discuss in an issue before building it; the collector is
SSE-specific today.

## 5. Register the provider

1. Add a constant to `internal/genai/genai.go` next to `SystemGemini`. Use the
   OpenTelemetry GenAI well-known value for the provider if the pinned spec
   (see `docs/SEMCONV-COMPLIANCE.md`) defines one; check it rather than
   guessing. Do not invent a name if the spec has one.
2. Add the entry to `BuiltIn()` in `provider.go`:
   `"gemini": &Gemini{}`. The map key is what users write in
   `upstreams[].provider`. Config validation and error messages read from this
   map via `Known` and `KnownNames`, so there is no other registration point.
3. Update the `provider:` list in the `config.example.yaml` upstream comment and
   add a commented example upstream (see the commented Gemini block in `config.example.yaml`, around lines 103-106, for `strip_prefix`).

## 6. Pricing

Add a block to `internal/pricing/prices.yaml` keyed by the system name, with
longest-prefix model keys. If your `gen_ai.provider.name` differs from the
table key (Gemini and Vertex both price under `gcp.gen_ai`), map it in
`pricingSystem` in `internal/proxy/observer.go`.

Put the official pricing URL and fetch date in the YAML header and
`docs/PRICING.md`, and mark anything you could not verify `UNVERIFIED`. A model
that is not in the table is not an error: it increments
`llmspan.pricing.misses.total`, which is visible and honest. Prefer a miss over
a guess. For local or free providers (Ollama), no prices is the correct
answer.

## 7. Tests

At minimum, in `internal/provider/<name>_test.go` (see `gemini_test.go`):

1. `OperationFor`: positive paths, plus trailing slash, sub-resource and
   unrelated paths returning `""`.
2. `ParseRequest` then `ParseResponseJSON` round trip against the recorder
   tracer (`newRecorderTracer`, `mustHaveAttr` in `testutil_test.go`). Assert
   token arithmetic (cache, reasoning), model, finish reasons.
3. Streaming through `WrapStream`: usage on the final chunk, a stream
   truncated mid-event, TTFT set for text and for tool calls, `onDone` called
   once, and the bytes read by the caller identical to the input.
4. Malformed input: invalid JSON, empty body, wrong types. Assert no panic and
   sane `Info`.
5. Content capture: with capture on, content is redacted (`ContentOpts.Redact`)
   before it reaches events; with capture off, no content attributes exist.
   See `content_test.go`.
6. Pricing: one test in `internal/pricing` that a real model id resolves to the
   rate you added.
7. An end-to-end test through `internal/proxy` against an `httptest` upstream
   if the provider has path or header quirks (Vertex-style prefixes, API key
   in a header or query parameter).

### Fixtures

Real captured responses go in `internal/provider/testdata/`, named
`<provider>-<api>-<case>.json` (existing: `gemini-generate-content.json`,
`openai-responses-completed.json`), loaded with `os.ReadFile("testdata/...")`.
Rules: scrub keys, account ids and any user content before committing; keep
them small; for SSE keep the raw `data:` framing in a `.txt` or `.sse` file. Prefer a real capture
over a hand-written one. Providers change shapes quietly, and fixtures are how
we notice.

### Fuzzing

Parsers consume untrusted bytes from the network. Add a `Fuzz...` target for
`ParseRequest` and `ParseResponseJSON` (and your stream callback if it has
non-trivial logic) with the invariant "never panics, never returns negative
token counts". Seed with your fixtures. Run it locally for a few minutes:

```bash
go test ./internal/provider/ -fuzz=FuzzSSETeeScan -fuzztime=2m   # existing target; add a Fuzz<Provider>Parse next to it for your parser
```

Note: CI's short fuzz job lists targets explicitly in `.github/workflows/ci.yml`;
if you add a fuzz target that should run in CI, add its line there.

## 8. Pull request checklist

- [ ] `OperationFor` is strict; negative paths tested; unknown paths pass through
- [ ] Parsers never panic or fail the request on malformed input
- [ ] Token fields match what the provider bills; cache and reasoning semantics documented in a comment
- [ ] Stream parsing handles cumulative usage, truncation and tool-only responses
- [ ] Content only via `content.emit*` and only when capture is on
- [ ] Registered in `BuiltIn()`; `genai` constant uses the spec's well-known value
- [ ] Prices added with source URL and date, or deliberately omitted; unverified rows marked
- [ ] Fixtures scrubbed; fuzz target added
- [ ] `config.example.yaml`, `docs/SEMCONV-COMPLIANCE.md`, `CHANGELOG.md` (Unreleased) updated
- [ ] Local gate passes: `make lint test build` and the coverage commands in
      [CONTRIBUTING.md](../CONTRIBUTING.md)

If the provider is a different shape from "JSON in, JSON or SSE out over
HTTPS" (for example it needs request signing, like AWS SigV4, or a binary event
stream), open an issue first. llmspan forwards the client's own credentials and
does not sign requests, so some providers need design work, not just a parser.
