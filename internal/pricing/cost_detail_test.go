package pricing

import (
	"os"
	"testing"
	"time"
)

func TestCostTieredTable(t *testing.T) {
	t.Parallel()
	d := Default()
	tests := []struct {
		name                  string
		system, model, tier   string
		in, out, read, create int64
		want                  float64
		wantOK                bool
	}{
		// Anthropic: 1M prompt = 800k uncached + 100k read + 100k 5m write; opus 4.8 5/25, read .5, write 6.25.
		{"anthropic cache math", "anthropic", "claude-opus-4-8", "", 1_000_000, 100_000, 100_000, 100_000, 4.0 + 0.05 + 0.625 + 2.5, true},
		{"sonnet 5.5 read 0.05x", "anthropic", "claude-sonnet-5-5-20261001", "", 1_000_000, 0, 1_000_000, 0, 0.10, true},
		{"sonnet 5 standard price", "anthropic", "claude-sonnet-5", "", 1_000_000, 1_000_000, 0, 0, 12, true},
		{"sonnet 5 not shadowed by 5.5", "anthropic", "claude-sonnet-5", "", 1_000_000, 0, 1_000_000, 0, 0.20, true},
		// Clamp fallback: cache > input must not drop cost; total prompt = read+create.
		{"clamp cache over input", "anthropic", "claude-opus-4-8", "", 100, 0, 1_000_000, 0, 0.5, true},
		{"clamp mixed", "anthropic", "claude-opus-4-8", "", 0, 1_000_000, 500_000, 500_000, 0.25 + 3.125 + 25, true},
		{"negative tokens ignored", "openai", "gpt-5", "", -5, -5, -5, -5, 0, true},
		// Gemini lookup (system gcp.gen_ai), cached read, no cache-write charge.
		{"gemini cached", "gcp.gen_ai", "gemini-2.5-flash", "", 1_000_000, 1_000_000, 500_000, 0, 0.15 + 0.015 + 2.5, true},
		{"gemini lite not swallowed", "gcp.gen_ai", "gemini-2.5-flash-lite", "", 1_000_000, 0, 0, 0, 0.10, true},
		{"gemini 3.1 pro short", "gcp.gen_ai", "gemini-3.1-pro-preview", "", 200_000, 1_000_000, 0, 0, 0.4 + 12, true},
		{"gemini 3.1 pro long", "gcp.gen_ai", "gemini-3.1-pro-preview", "", 300_000, 1_000_000, 100_000, 0, 0.8 + 0.04 + 18, true},
		// Long context: OpenAI >272K, Haiku 5.5 >100K counting cache tokens.
		{"openai short", "openai", "gpt-5.5", "", 272_000, 0, 0, 0, 1.36, true},
		{"openai long", "openai", "gpt-5.5", "", 272_001, 1_000_000, 0, 0, 272_001*10/1e6 + 45, true},
		{"haiku 5.5 short", "anthropic", "claude-haiku-5-5", "", 100_000, 0, 0, 0, 0.01, true},
		{"haiku 5.5 long via cache read", "anthropic", "claude-haiku-5-5", "", 100_001, 0, 100_001, 0, 100_001 * 0.05 / 1e6, true},
		// Service tiers.
		{"openai flex", "openai", "gpt-5", "flex", 1_000_000, 0, 0, 0, 0.625, true},
		{"openai priority", "openai", "gpt-5", "Priority", 1_000_000, 0, 0, 0, 2.5, true},
		{"openai default tier", "openai", "gpt-5", "default", 1_000_000, 0, 0, 0, 1.25, true},
		{"anthropic batch", "anthropic", "claude-opus-4-8", "batch", 1_000_000, 0, 0, 0, 2.5, true},
		{"gemini priority", "gcp.gen_ai", "gemini-2.5-flash", "priority", 1_000_000, 0, 0, 0, 0.54, true},
		// Misses stay visible.
		{"unlisted opus minor misses", "anthropic", "claude-opus-4-9", "", 1, 1, 0, 0, 0, false},
		{"unknown gemini misses", "gcp.gen_ai", "gemini-9-nope", "", 1, 1, 0, 0, 0, false},
	}
	for _, tc := range tests {
		got, ok := d.CostTiered(tc.system, tc.model, tc.tier, tc.in, tc.out, tc.read, tc.create)
		if ok != tc.wantOK || !approxEqual(got, tc.want) {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestServiceTierModelOverride(t *testing.T) {
	t.Parallel()
	tbl := newTable(map[string]map[string]Rate{"openai": {"m": {
		InputUSDPerMTok: 10, ServiceTierMultipliers: map[string]float64{"priority": 2.5},
	}}})
	if got, _ := tbl.CostTiered("openai", "m", "priority", 1_000_000, 0, 0, 0); !approxEqual(got, 25) {
		t.Fatalf("got %v want 25", got)
	}
}

func TestAnthropicCacheRatesPopulated(t *testing.T) {
	t.Parallel()
	for prefix, r := range Default().rates["anthropic"] {
		if r.CachedInputUSDPerMTok == 0 || r.CacheCreationUSDPerMTok == 0 {
			t.Errorf("anthropic/%s missing cache rates", prefix)
		}
	}
}

func TestGeminiCorpusPriced(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"gemini-4-carbon", "gemini-3.8-flash", "gemini-3.1-pro-preview", "gemini-2.5-pro", "gemini-2.5-flash-lite"} {
		if _, ok := Default().Cost("gcp.gen_ai", m, 1, 1); !ok {
			t.Errorf("no price for gcp.gen_ai/%s", m)
		}
	}
}

// TestSnapshotFreshness fails when the embedded catalogue is older than
// 90 days. Gated by env var so forks and unrelated CI runs never flake:
//
//	LLMSPAN_CHECK_PRICES=1 go test ./internal/pricing -run TestSnapshotFreshness
func TestSnapshotFreshness(t *testing.T) {
	if os.Getenv("LLMSPAN_CHECK_PRICES") == "" {
		t.Skip("set LLMSPAN_CHECK_PRICES=1 to enforce price-catalogue freshness")
	}
	snap, err := time.Parse("2006-01", Snapshot)
	if err != nil {
		t.Fatalf("Snapshot %q not YYYY-MM: %v", Snapshot, err)
	}
	if age := time.Since(snap); age > 90*24*time.Hour {
		t.Fatalf("price snapshot %s is %d days old (>90); refresh prices.yaml and bump Snapshot", Snapshot, int(age.Hours()/24))
	}
}
