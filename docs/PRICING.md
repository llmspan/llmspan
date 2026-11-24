# Pricing catalogue

llmspan prices each request from `internal/pricing/prices.yaml` (embedded).
Snapshot: **2026-11**, fetched **2026-11-24**.

## Sources

| Provider | URL | Notes |
|---|---|---|
| Anthropic | https://platform.claude.com/docs/en/about-claude/pricing, https://platform.claude.com/docs/en/models/overview | Input, 5m/1h cache write, cache read, output, batch, fast mode, Haiku 5.5 long-context tier |
| OpenAI | https://developers.openai.com/api/docs/pricing | `openai.com/api/pricing` shows ChatGPT plans only and links here. Standard/batch/flex/fast, >272K long-context columns |
| Google | https://aistudio.google.com/docs/pricing, https://aistudio.google.com/docs/models | Redirect target of `ai.google.dev/gemini-api/docs/pricing`. Standard paid tier, text rates, >200K tiers |

Pages were read through a summarising fetcher, not by hand; spot-check
before relying on them for billing reconciliation.

## Resolved findings

- Sonnet 5: the Anthropic page states the $2/$10 introductory price **is now
  the standard price**; the planned 2026-09-01 increase to $3/$15 was
  cancelled. The table stays at $2/$10.
- Anthropic cache: read 0.1x input (0.05x Opus 5.5 / Sonnet 5.5, 0.025x
  Fable/Mythos 5.1+), 5m write 1.25x. Usage totals do not separate 5m from 1h
  writes, so 1h writes (2x) are under-billed at the 5m rate.
- OpenAI cached rates and >272K long-context rates added for every verified
  model. Gemini added under system `gcp.gen_ai` with context-cache read rates
  and >200K tiers for the Pro models.
- `CostDetailed` no longer returns `(0,false)` when cache tokens exceed input
  tokens: the cache counts are trusted, uncached is clamped to zero.
- Service tiers: `Table.CostTiered(system, model, tier, ...)` applies
  multipliers (OpenAI/Gemini batch and flex 0.5x, OpenAI priority/fast 2x,
  Gemini priority 1.8x, Anthropic batch 0.5x; override per model with
  `service_tier_multipliers`). **Limitation:** the proxy still calls
  `CostDetailed`, so tier pricing is inert until `internal/proxy/observer.go`
  passes `info.ServiceTier` (only OpenAI populates it today). Per-model
  multipliers vary slightly (GPT-5.5 fast is 2.5x, GPT-4o about 1.7x).
- Long context: `long_context_threshold_tokens` plus `long_*` rates; the whole
  request is repriced when the prompt (including cache tokens) exceeds it.

## Verification status (2026-11-24)

- Anthropic: all shipped IDs checked against
  https://platform.claude.com/docs/en/models/overview (current IDs
  `claude-fable-5-2`, `claude-opus-5-5`, `claude-sonnet-5-5`, `claude-haiku-5-5`)
  and https://platform.claude.com/docs/en/about-claude/models/model-ids-and-versions
  (4.6+ IDs are dateless pinned snapshots; earlier IDs carry a `-YYYYMMDD`
  suffix, e.g. `claude-sonnet-4-5-20250929`, `claude-haiku-4-5-20251001`, and
  match by prefix). Prices and cache multipliers match the pricing page.
  Older IDs (Mythos, Fable 5/5.1, Opus 5, 4.x) are listed by display name only
  on those pages; their IDs follow the documented scheme but are not quoted
  verbatim. The `claude-3-*` models were removed (not documented).
- OpenAI: every shipped price is on the pricing page (including gpt-5.3-codex
  and the three embeddings). Removed as undocumented: o4-mini, o1, o1-pro,
  o1-mini, o1-preview, gpt-4, gpt-4-turbo, gpt-3.5-turbo. Tombstoned with
  `unpriced: true` (so they miss instead of inheriting a parent's rate):
  gpt-5.5-mini, gpt-5.5-nano, gpt-4.1-nano, o3-mini.
- Google: model IDs checked against https://aistudio.google.com/docs/models;
  prices against the pricing page. gemini-2.5-flash and 2.5-pro are deprecated
  and reroute on 2026-12-01 (see below); kept while still served.
- The pricing page text for Google was read only up to its first 100k
  characters (audio/image/TTS models are not shipped).

## Remaining caveats

- Upcoming Google changes: gemini-4-carbon/argon go to $4/$20 (cache $0.20) on
  2026-12-01; gemini-3.8-flash/3.6-flash to $1.50/$7.50 (cache $0.15) on
  2027-01-01. Refresh the snapshot on those dates.
- Gemini: the provider records `candidatesTokenCount` as output, which excludes
  thinking tokens, yet Google bills thinking as output. Gemini thinking cost is
  therefore under-counted until `internal/provider/gemini.go` adds
  `thoughtsTokenCount` to `OutputTokens` (outside the pricing package).
- Audio-input rates and Gemini cache storage fees are not modelled.

## Unlisted models and overrides

Lookup is longest-prefix on the lowercased model. A model with no matching
prefix (for example `claude-opus-4-9`) is not priced and increments the
`llmspan.pricing.misses.total` counter; alert on it. Caveat: a bare id such as
`claude-opus-5` is a prefix of later minors, which would silently inherit its
rate, so list each new minor explicitly.

Override or extend prices by setting `pricing.path` in the llmspan config to a
YAML file with the same shape as `prices.yaml`. It is merged over the built-ins
per (system, prefix); `pricing.fail_open` controls missing/malformed files.

## Refreshing

1. Re-read the three source pages, update `prices.yaml` and `Snapshot`.
2. Add new model ids to `corpus_test.go`.
3. `LLMSPAN_CHECK_PRICES=1 go test ./internal/pricing -run TestSnapshotFreshness`
   (or `make check-prices`) fails when the snapshot is older than 90 days. It is
   skipped by default so forks do not flake. The `pricing-freshness` workflow
   runs it weekly and opens an issue when it fails.

## Tombstones

A rate with `unpriced: true` shadows shorter prefixes but reports no price, so
an unverified child model (e.g. `gpt-5.5-mini`) cannot inherit its parent's
(`gpt-5.5`) rate by longest-prefix match.
