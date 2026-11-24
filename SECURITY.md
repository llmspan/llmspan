# Security Policy

llmspan sits between an application and its LLM provider's API. Every
request that flows through it carries production credentials and the
prompts (often sensitive) the application sends. We take vulnerability
reports seriously and aim to keep llmspan a credible part of any
production trust boundary.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security findings.

Use GitHub's private vulnerability reporting:
**https://github.com/llmspan/llmspan/security/advisories/new**

We acknowledge every report within **72 hours** and aim for an
initial triage assessment within **7 days**.

## Coordinated disclosure window

Default window: **90 days** from acknowledgement, or sooner if a
fix ships and is broadly available. Reporters may request a shorter
window for active in-the-wild exploitation; we'll always negotiate
in good faith.

If we cannot ship a fix within 90 days we'll communicate the reason
and request an extension explicitly — never silently.

## In scope

- The `llmspan` binary and the container image at
  `ghcr.io/llmspan/llmspan`.
- The release pipeline (`.github/workflows/release.yml`) and the
  CI pipeline (`.github/workflows/ci.yml`) including the signed
  artifacts they produce.
- The Go modules in this repo: `internal/*`, `cmd/llmspan`.

## Out of scope

- Vulnerabilities in third-party dependencies we transitively import
  — please report those to the upstream project first. We'll
  prioritize the dep bump on our side as soon as a fix is available.
- Configuration foot-guns that the proxy explicitly warns about
  (e.g. `LLMSPAN_ALLOW_INSECURE=true` on a non-loopback bind, or
  `telemetry.acknowledge_insecure: true` — both opt-in declarations
  that the operator is accepting the trade-off).
- Denial-of-service from a configured upstream returning malicious
  bytes. The proxy has bounded buffers, body deadlines, concurrency
  caps, and per-upstream circuit breakers, but a determined
  upstream can still consume operator-side resources;
  that's an upstream-trust question, not a vulnerability.

## Threat model and known limits

llmspan is an observe-only forwarding proxy. What it does and does not protect:

- **It sees your provider API keys and prompts in transit.** It forwards the
  client's provider auth headers unchanged and cannot work without them. Run it
  only where you would run an API gateway. It does not store keys; prompt
  content is exported only when `content.mode` is `events` or `logs`, and
  persisted locally only when `recording.path` is set (which records prompts
  and responses even when `content.mode` is `off`).
- **Redaction is best-effort, not DLP.** It masks well-known credential shapes
  and a few PII patterns. It misses bare keys with no prefix or keyword,
  encoded or split secrets, and free-text PII such as names and addresses.
- **Caller attribution is only as strong as its source.** Named auth tokens and
  mTLS identities are authenticated; `attribution.header` is client-supplied
  and spoofable. Label cardinality is first-come: once `max_cardinality` is
  reached, new callers and models are bucketed as `_other`, so a client that can
  reach the proxy can crowd legitimate callers out of the per-caller metrics.
  Use named tokens for any attribution you rely on, and set an explicit
  `max_cardinality`.
- **A non-loopback listener needs transport protection and client identity.**
  The proxy refuses to start without TLS (or `allow_insecure`) and without
  tokens or mTLS (or `allow_unauthenticated`). Those two flags are explicit
  opt-ins that move the risk to the operator.
- **Availability limits.** Failed-token argon2 verifications are bounded by a
  semaphore (excess gets 429), so a flood of guessed tokens can briefly
  delay callers whose tokens are not in the 60 s positive cache; put a rate
  limiter or ingress in front of a listener that untrusted networks can reach.
  `max_body_bytes` applies to recognised LLM endpoints; other paths on an
  upstream (for example `/v1/files`) are forwarded without a size cap.
- **Upstreams are fixed in configuration.** Clients cannot choose the upstream
  host, so the proxy is not an open relay or SSRF primitive by design.
- **Recordings are untrusted input.** `llmspan replay` only sends to the
  `--target` host, rejects absolute or scheme-bearing paths, and allows only
  GET and POST.

## What we'll do

1. Acknowledge the report (≤ 72h).
2. Triage and assign a severity (≤ 7d).
3. Develop a fix on a private branch.
4. Coordinate disclosure timing with the reporter.
5. Release a patched version, sign + attest it per the standard
   release pipeline (SLSA-3 provenance, CycloneDX SBOM, cosign).
6. Publish a security advisory on the GitHub repo crediting the
   reporter (unless they prefer anonymity).
7. Update `CHANGELOG.md` with the CVE reference and the upgrade
   path.

## Verification

Every release artifact is signed with cosign keyless via GitHub
OIDC. Verify a downloaded binary:

```bash
cosign verify-blob \
  --certificate llmspan-linux-amd64.pem \
  --signature   llmspan-linux-amd64.sig \
  --certificate-identity-regexp "^https://github\.com/llmspan/llmspan/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  llmspan-linux-amd64
```

The container image at `ghcr.io/llmspan/llmspan` is signed
the same way:

```bash
cosign verify \
  --certificate-identity-regexp "^https://github\.com/llmspan/llmspan/" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/llmspan/llmspan:latest
```

SLSA-3 provenance and CycloneDX SBOMs are attached to each release.

## Thanks

We appreciate every responsible report and will credit reporters in
the advisory unless they prefer anonymity.
