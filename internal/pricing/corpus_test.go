package pricing

import "testing"

// TestCorpusModelsAllPriced is the freshness gate for the embedded
// catalogue: the table shipped as a stale snapshot
// and unknown models missed silently, so the cost dashboard undercounted
// exactly as new models rolled out. Every model ID below is shaped like
// what the OpenAI / Anthropic APIs return in responses today (including
// dated snapshot pins, which resolve via longest-prefix-match). When a
// provider ships a new family, add it here — the test failing is the
// reminder to refresh prices.yaml and bump pricing.Snapshot.
func TestCorpusModelsAllPriced(t *testing.T) {
	t.Parallel()

	corpus := map[string][]string{
		"openai": {
			// current flagship / general
			"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
			"gpt-5.5", "gpt-5.5-pro",
			"gpt-5.4", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.4-pro",
			"gpt-5.3-codex", "gpt-5.2", "gpt-5.1",
			"gpt-5", "gpt-5-mini", "gpt-5-nano",
			"gpt-4.1", "gpt-4.1-mini",
			// snapshot pins inherit the family rate by prefix
			"gpt-5-2025-08-07", "gpt-4.1-2025-04-14",
			// reasoning
			"o3", "o3-pro",
			// legacy chat + embeddings
			"gpt-4o", "gpt-4o-mini", "gpt-4o-mini-2024-07-18",
			"text-embedding-3-small", "text-embedding-3-large",
			"text-embedding-ada-002",
		},
		"anthropic": {
			// current generation
			"claude-fable-5", "claude-mythos-5",
			"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-6",
			"claude-opus-4-5", "claude-opus-4-1",
			"claude-sonnet-5", "claude-sonnet-4-6", "claude-sonnet-4-5",
			"claude-haiku-4-5",
			// snapshot pins
			"claude-opus-4-8-20260528", "claude-sonnet-4-5-20250929",
			"claude-haiku-4-5-20251001",
			"claude-fable-5-2", "claude-opus-5-5", "claude-sonnet-5-5",
			"claude-haiku-5-5", "claude-opus-4-5-20251101",
			"claude-opus-4-20250514", "claude-sonnet-4-20250514",
		},
	}

	table := Default()
	for system, models := range corpus {
		for _, model := range models {
			if _, ok := table.Cost(system, model, 1000, 1000); !ok {
				t.Errorf("no price for %s/%s — refresh prices.yaml and bump pricing.Snapshot", system, model)
			}
		}
	}
}

// TestOpusFourPrefixDoesNotSwallowMinorVersions guards the trickiest
// longest-prefix hazard in the catalogue: opus 4.1 costs 3x opus 4.5+,
// and both share the "claude-opus-4" prefix. The dated pins must land
// on their own minor version's rate.
func TestOpusFourPrefixDoesNotSwallowMinorVersions(t *testing.T) {
	t.Parallel()

	table := Default()
	// 1M input + 1M output at 5/25 = $30; at 15/75 = $90.
	if usd, ok := table.Cost("anthropic", "claude-opus-4-8-20260528", 1_000_000, 1_000_000); !ok || usd != 30 {
		t.Errorf("opus-4-8 pin priced at $%v (ok=%v), want $30", usd, ok)
	}
	if usd, ok := table.Cost("anthropic", "claude-opus-4-1-20250805", 1_000_000, 1_000_000); !ok || usd != 90 {
		t.Errorf("opus-4-1 pin priced at $%v (ok=%v), want $90", usd, ok)
	}
}

// TestDatedIDsResolveToExpectedRate asserts real dated snapshot IDs land on
// the documented per-family rate (1M input + 1M output tokens).
func TestDatedIDsResolveToExpectedRate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		system, model string
		want          float64
	}{
		{"anthropic", "claude-sonnet-4-5-20250929", 18},
		{"anthropic", "claude-haiku-4-5-20251001", 6},
		{"anthropic", "claude-opus-4-1-20250805", 90},
		{"anthropic", "claude-opus-4-20250514", 90},
		{"anthropic", "claude-opus-4-5-20251101", 30},
		{"anthropic", "claude-sonnet-5-5", 12},
		{"anthropic", "claude-opus-5-5", 24},
		{"openai", "gpt-5-2025-08-07", 11.25},
		{"openai", "gpt-4.1-2025-04-14", 10},
		{"openai", "gpt-4o-mini-2024-07-18", 0.75},
		{"openai", "gpt-5.4-mini-2026-03-17", 5.25},
	}
	for _, c := range cases {
		got, ok := Default().Cost(c.system, c.model, 1_000_000, 1_000_000)
		if !ok || !approxEqual(got, c.want) {
			t.Errorf("%s/%s = $%v (ok=%v), want $%v", c.system, c.model, got, ok, c.want)
		}
	}
}

// TestUnverifiedModelsAreNotPriced guards the tombstones: models removed
// for lack of an official price must miss rather than inherit a parent rate.
func TestUnverifiedModelsAreNotPriced(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"gpt-5.5-mini", "gpt-5.5-nano", "gpt-5.5-mini-2026-01-01", "gpt-4.1-nano", "o3-mini", "o4-mini", "o1", "gpt-4", "gpt-4-turbo", "gpt-3.5-turbo"} {
		if _, ok := Default().Cost("openai", m, 1000, 1000); ok {
			t.Errorf("openai/%s unexpectedly priced", m)
		}
	}
	if _, ok := Default().Cost("anthropic", "claude-3-5-sonnet-20241022", 1000, 1000); ok {
		t.Error("claude-3-5-sonnet unexpectedly priced")
	}
}
