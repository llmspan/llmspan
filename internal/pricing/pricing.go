// Package pricing converts (system, model, tokens) into USD cost.
//
// Prices are denominated per million tokens, matching how upstream
// providers publish them. The built-in catalogue is the embedded
// `prices.yaml`; operators override per their negotiated rate by
// pointing `pricing.path` at a YAML file with the same shape — the
// file is merged on top of the built-ins, leaving unspecified model
// families at their default rates. Unknown models return (0, false);
// callers should not record a cost in that case so dashboards stay
// honest.
package pricing

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed prices.yaml
var embeddedPrices []byte

// Snapshot is the date of the embedded price catalogue. Surfaced by
// `llmspan version` so operators can tell at a glance whether the
// built-in rates predate the models they're running — stale pricing
// undercounts spend exactly when it grows.
const Snapshot = "2026-11"

// Rate is a model's input/output price in USD per 1M tokens. YAML
// field names match the documented operator-facing schema.
type Rate struct {
	InputUSDPerMTok         float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok        float64 `yaml:"output_usd_per_mtok"`
	CachedInputUSDPerMTok   float64 `yaml:"cached_input_usd_per_mtok,omitempty"`
	CacheCreationUSDPerMTok float64 `yaml:"cache_creation_input_usd_per_mtok,omitempty"`

	// Optional long-context tier. When LongContextThresholdTokens > 0 and
	// a request's total prompt (inputTokens, including cache reads and
	// writes) exceeds it, the Long* rates replace the standard ones for
	// the whole request. A zero Long* cache rate falls back to the
	// standard cache rate.
	LongContextThresholdTokens  int64   `yaml:"long_context_threshold_tokens,omitempty"`
	LongInputUSDPerMTok         float64 `yaml:"long_input_usd_per_mtok,omitempty"`
	LongOutputUSDPerMTok        float64 `yaml:"long_output_usd_per_mtok,omitempty"`
	LongCachedInputUSDPerMTok   float64 `yaml:"long_cached_input_usd_per_mtok,omitempty"`
	LongCacheCreationUSDPerMTok float64 `yaml:"long_cache_creation_input_usd_per_mtok,omitempty"`

	// Unpriced marks a tombstone: the prefix matches (so it shadows any
	// shorter parent prefix) but reports "no price". Use it for models
	// whose price is unverified and would otherwise inherit a wrong parent.
	Unpriced bool `yaml:"unpriced,omitempty"`

	// ServiceTierMultipliers optionally overrides the per-system default
	// service-tier multipliers (see defaultTierMultipliers) for this model.
	ServiceTierMultipliers map[string]float64 `yaml:"service_tier_multipliers,omitempty"`
}

// defaultTierMultipliers are the whole-request multipliers applied by
// CostTiered for non-standard service tiers, keyed by system then
// lower-cased tier name. Tiers not listed (including "", "auto",
// "default", "standard") bill at 1x. Values mirror the providers'
// published batch/flex (0.5x) and priority/fast (about 2x; Gemini 1.8x)
// pricing; see docs/PRICING.md. Per-model exceptions go in
// Rate.ServiceTierMultipliers.
var defaultTierMultipliers = map[string]map[string]float64{
	"openai":     {"batch": 0.5, "flex": 0.5, "priority": 2, "fast": 2},
	"anthropic":  {"batch": 0.5},
	"gcp.gen_ai": {"batch": 0.5, "flex": 0.5, "priority": 1.8},
}

// Table is the active pricing catalogue. Construct via Default for the
// embedded defaults, or via Load to merge an operator override on top.
type Table struct {
	// rates is keyed by gen_ai.provider.name then by model-prefix. The map
	// preserves O(1) system membership and supports the merge path
	// in Load.
	rates map[string]map[string]Rate
	// trees is the per-system lookup index. Each `*trieNode` is the
	// root of a byte-trie of every prefix configured for that system,
	// terminating in a Rate at the matching node. Lookup walks at most
	// `len(model)` byte transitions and records the deepest
	// rate-bearing node seen — that's the longest matching prefix in
	// O(K) time, with deterministic ordering by construction (the
	// trie has no concept of map-iteration order).
	trees map[string]*trieNode
}

// trieNode is a node in a per-system prefix trie. Children are held in
// a map keyed by byte so the memory footprint stays proportional to
// the actual character set in use (catalogues are ASCII-only today;
// a switch to a fixed [128]*trieNode array would be slightly faster
// but blow up memory for sparse children).
type trieNode struct {
	children map[byte]*trieNode
	rate     *Rate // non-nil if a configured prefix terminates here
}

func (n *trieNode) insert(prefix string, rate Rate) {
	cur := n
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if cur.children == nil {
			cur.children = make(map[byte]*trieNode)
		}
		next, ok := cur.children[c]
		if !ok {
			next = &trieNode{}
			cur.children[c] = next
		}
		cur = next
	}
	r := rate
	cur.rate = &r
}

func (n *trieNode) lookup(model string) (Rate, bool) {
	var best *Rate
	cur := n
	for i := 0; i < len(model); i++ {
		if cur.rate != nil {
			best = cur.rate
		}
		next, ok := cur.children[model[i]]
		if !ok {
			break
		}
		cur = next
	}
	if cur.rate != nil {
		best = cur.rate
	}
	if best == nil {
		return Rate{}, false
	}
	return *best, true
}

// defaultTable is the embedded built-in catalogue. It is constructed
// once at package init and never mutated, so concurrent readers of
// Default() are safe.
var defaultTable *Table

func init() {
	var parsed map[string]map[string]Rate
	if err := yaml.Unmarshal(embeddedPrices, &parsed); err != nil {
		panic(fmt.Errorf("pricing: embedded prices.yaml malformed: %w", err))
	}
	defaultTable = newTable(parsed)
}

// newTable builds a Table from a parsed (system, prefix, rate) map and
// precomputes a per-system byte-trie. Construction allocates; lookups
// are read-only and lock-free.
func newTable(rates map[string]map[string]Rate) *Table {
	trees := make(map[string]*trieNode, len(rates))
	for sys, models := range rates {
		root := &trieNode{}
		for prefix, rate := range models {
			root.insert(prefix, rate)
		}
		trees[sys] = root
	}
	return &Table{rates: rates, trees: trees}
}

// Default returns the embedded built-in pricing catalogue. The returned
// table is shared — do not mutate it.
func Default() *Table {
	return defaultTable
}

// Load builds a Table that layers an operator-supplied YAML override
// on top of the built-in catalogue. Empty path returns Default().
//
// failOpen controls behaviour when the file is missing or malformed:
//   - false (recommended for production): return an error so the
//     operator notices.
//   - true: silently fall back to Default() so a typo'd path doesn't
//     crash the proxy.
func Load(path string, failOpen bool) (*Table, error) {
	if path == "" {
		return Default(), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if failOpen {
			return Default(), nil
		}
		return nil, fmt.Errorf("read pricing file %q: %w", path, err)
	}
	var override map[string]map[string]Rate
	if err := yaml.Unmarshal(raw, &override); err != nil {
		if failOpen {
			return Default(), nil
		}
		return nil, fmt.Errorf("parse pricing file %q: %w", path, err)
	}

	// Merge: deep-copy the defaults, then layer the override per
	// (system, model-prefix) key. The override wins per matching key
	// without dropping any unspecified default.
	merged := make(map[string]map[string]Rate, len(defaultTable.rates))
	for sys, models := range defaultTable.rates {
		clone := make(map[string]Rate, len(models))
		for k, v := range models {
			clone[k] = v
		}
		merged[sys] = clone
	}
	for sys, models := range override {
		if merged[sys] == nil {
			merged[sys] = make(map[string]Rate, len(models))
		}
		for k, v := range models {
			merged[sys][k] = v
		}
	}
	return newTable(merged), nil
}

// Cost returns the USD cost for the given token usage against this
// table. ok is false if no rate exists for (system, model); callers
// should skip cost emission in that case.
func (t *Table) Cost(system, model string, inputTokens, outputTokens int64) (usd float64, ok bool) {
	return t.CostDetailed(system, model, inputTokens, outputTokens, 0, 0)
}

// CostDetailed prices cached input separately when the catalogue contains
// cached_input_usd_per_mtok. inputTokens is the provider-reported total,
// including cached tokens. Reasoning tokens are normally included in the
// provider's output total and must not be added a second time.
//
// If cacheRead+cacheCreation exceeds inputTokens (a provider reporting
// inconsistent usage), the cache counts are trusted, the uncached portion
// is clamped to zero and the total prompt is taken as their sum, so cost
// is never silently dropped.
func (t *Table) CostDetailed(system, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64) (usd float64, ok bool) {
	return t.CostTiered(system, model, "", inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens)
}

// CostTiered is CostDetailed plus a provider service tier (for example
// the OpenAI response service_tier). The tier multiplier from
// Rate.ServiceTierMultipliers, else defaultTierMultipliers, scales the
// whole request; unknown tiers bill at 1x.
func (t *Table) CostTiered(system, model, serviceTier string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int64) (usd float64, ok bool) {
	r, found := t.lookup(system, model)
	if !found {
		return 0, false
	}
	if inputTokens < 0 {
		inputTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if cacheReadTokens < 0 {
		cacheReadTokens = 0
	}
	if cacheCreationTokens < 0 {
		cacheCreationTokens = 0
	}
	if cached := cacheReadTokens + cacheCreationTokens; cached > inputTokens {
		inputTokens = cached
	}
	uncachedTokens := inputTokens - cacheReadTokens - cacheCreationTokens

	inRate, outRate := r.InputUSDPerMTok, r.OutputUSDPerMTok
	readRate, createRate := r.CachedInputUSDPerMTok, r.CacheCreationUSDPerMTok
	if r.LongContextThresholdTokens > 0 && inputTokens > r.LongContextThresholdTokens && r.LongInputUSDPerMTok > 0 {
		inRate, outRate = r.LongInputUSDPerMTok, r.LongOutputUSDPerMTok
		if outRate == 0 {
			outRate = r.OutputUSDPerMTok
		}
		if r.LongCachedInputUSDPerMTok > 0 {
			readRate = r.LongCachedInputUSDPerMTok
		}
		if r.LongCacheCreationUSDPerMTok > 0 {
			createRate = r.LongCacheCreationUSDPerMTok
		}
	}
	if readRate == 0 {
		readRate = inRate
	}
	if createRate == 0 {
		createRate = inRate
	}
	usd = (float64(uncachedTokens)/1_000_000)*inRate +
		(float64(cacheReadTokens)/1_000_000)*readRate +
		(float64(cacheCreationTokens)/1_000_000)*createRate +
		(float64(outputTokens)/1_000_000)*outRate
	return usd * tierMultiplier(system, serviceTier, r), true
}

func tierMultiplier(system, tier string, r Rate) float64 {
	tier = strings.ToLower(tier)
	if tier == "" {
		return 1
	}
	if m, ok := r.ServiceTierMultipliers[tier]; ok {
		return m
	}
	if m, ok := defaultTierMultipliers[system][tier]; ok {
		return m
	}
	return 1
}

func (t *Table) lookup(system, model string) (Rate, bool) {
	root, ok := t.trees[system]
	if !ok {
		return Rate{}, false
	}
	r, found := root.lookup(strings.ToLower(model))
	if !found || r.Unpriced {
		return Rate{}, false
	}
	return r, true
}
