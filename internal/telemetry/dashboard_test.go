package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/llmspan/llmspan/internal/genai"
)

// promName mirrors the translation Prometheus's OTLP receiver applies
// to an emitted OTel metric name: dots become underscores and a
// non-annotation unit is appended as a suffix ("s" → "seconds"). UCUM
// annotation units like "{token}" and "{USD}" are dropped entirely —
// which is exactly why the meters use them (see newGenAIMeters).
func promName(otelName, unit string) string {
	name := strings.ReplaceAll(otelName, ".", "_")
	switch unit {
	case "s":
		name += "_seconds"
	case "", "{token}", "{USD}":
		// annotations and unitless: no suffix
	default:
		name += "_" + unit
	}
	return name
}

// TestDashboardQueriesReferenceEmittedMetrics is the regression test
// for a past defect: the flagship dashboard shipped panels querying
// Prometheus series that llmspan never produced (unit-suffix mangling,
// double _total), so the demo's cost and token panels were empty out of
// the box. Every metric referenced in the dashboard JSON must resolve
// to a series derivable from an instrument this codebase registers.
func TestDashboardQueriesReferenceEmittedMetrics(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{}
	histogram := func(base string) {
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			allowed[base+suf] = true
		}
	}
	// The GenAI meters, exactly as newGenAIMeters registers them.
	histogram(promName(genai.MetricTokenUsage, "{token}"))
	histogram(promName(genai.MetricOperationDuration, "s"))
	histogram(promName(genai.MetricTimeToFirstToken, "s"))
	histogram(promName(genai.MetricTimeToFirstByte, "s"))
	histogram(promName(genai.MetricCostUSD, "{USD}"))
	histogram(promName(genai.MetricTimeToFirstChunk, "s"))
	histogram(promName(genai.MetricInferenceDuration, "s"))
	allowed[promName(genai.MetricCostUSDTotal, "{USD}")] = true // counter, name already ends _total
	// Cache token counters (unitless Int64 counters gain _total).
	allowed[promName(genai.MetricUsageCacheReadTokens, "")+"_total"] = true
	allowed[promName(genai.MetricUsageCacheWriteTokens, "")+"_total"] = true
	allowed[promName(genai.MetricUsageInputTokens, "")+"_total"] = true
	allowed[promName(genai.MetricUsageOutputTokens, "")+"_total"] = true
	// Proxy self-metrics (internal/proxy.New).
	allowed["llmspan_requests_total"] = true
	allowed["llmspan_requests_in_flight"] = true
	allowed["llmspan_requests_rejected_total"] = true
	allowed["llmspan_auth_rejected_total"] = true
	allowed["llmspan_breaker_state"] = true
	allowed["llmspan_streams_active"] = true
	allowed["llmspan_streams_idle_timeouts_total"] = true
	allowed["llmspan_pricing_misses_total"] = true
	allowed["llmspan_telemetry_export_errors_total"] = true
	allowed[promName("llmspan.tls.cert_not_after", "s")] = true
	allowed["llmspan_tls_cert_reload_failures_total"] = true

	_, thisFile, _, _ := runtime.Caller(0)
	dashPath := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"deploy", "grafana", "dashboards", "llmspan-overview.json")
	raw, err := os.ReadFile(dashPath)
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}

	var dash struct {
		Templating struct {
			List []struct {
				Query struct {
					Query string `json:"query"`
				} `json:"query"`
			} `json:"list"`
		} `json:"templating"`
		Panels []dashPanel `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("parse dashboard: %v", err)
	}

	// Candidate metric names inside PromQL: identifiers in our two
	// emitted namespaces. Label names (gen_ai_request_model etc.)
	// only appear inside selectors/by-clauses, so require the token
	// to be followed by something other than a comma/paren context —
	// simpler and robust enough: check known label names explicitly.
	metricRe := regexp.MustCompile(`\b(?:gen_ai|llmspan)_[a-zA-Z0-9_]+`)
	labels := map[string]bool{
		"gen_ai_request_model":  true,
		"gen_ai_response_model": true,
		"gen_ai_provider_name":  true,
		"gen_ai_operation_name": true,
		"gen_ai_token_type":     true,
		"llmspan_caller":        true, // attribution label, not a metric
	}

	check := func(where, q string) {
		for _, m := range metricRe.FindAllString(q, -1) {
			if labels[m] {
				continue
			}
			if !allowed[m] {
				t.Errorf("%s references %q, which llmspan does not emit (allowed: %v)", where, m, keys(allowed))
			}
		}
	}
	for _, v := range dash.Templating.List {
		check("templating variable", v.Query.Query)
	}
	// Collapsed rows nest their panels; walk them too.
	var walk func(ps []dashPanel)
	walk = func(ps []dashPanel) {
		for _, p := range ps {
			for _, tgt := range p.Targets {
				check("panel "+p.Title, tgt.Expr)
			}
			walk(p.Panels)
		}
	}
	walk(dash.Panels)
}

type dashPanel struct {
	Title      string `json:"title"`
	Type       string `json:"type"`
	Datasource struct {
		UID string `json:"uid"`
	} `json:"datasource"`
	Targets []struct {
		Expr  string `json:"expr"`
		Query string `json:"query"`
	} `json:"targets"`
	Panels []dashPanel `json:"panels"`
}

// TestDashboardHeroPanels pins the launch-critical panels: the Tempo
// "Recent LLM calls" / "Conversations" tables must use TraceQL against the
// provisioned tempo datasource, and the per-caller cost panel must exist.
func TestDashboardHeroPanels(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..",
		"deploy", "grafana", "dashboards", "llmspan-overview.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dash struct {
		Panels []dashPanel `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]dashPanel{}
	for _, p := range dash.Panels {
		byTitle[p.Title] = p
	}
	for _, title := range []string{"Recent LLM calls", "Conversations"} {
		p, ok := byTitle[title]
		if !ok {
			t.Errorf("missing panel %q", title)
			continue
		}
		if p.Datasource.UID != "tempo" || len(p.Targets) == 0 {
			t.Errorf("%q must be a Tempo-backed panel", title)
			continue
		}
		q := p.Targets[0].Query
		for _, want := range []string{"span.gen_ai.operation.name", "span.gen_ai.cost.usd", "span.gen_ai.conversation.id"} {
			if title == "Recent LLM calls" && !strings.Contains(q, want) {
				t.Errorf("%q TraceQL %q lacks %s", title, q, want)
			}
		}
	}
	for _, title := range []string{"Est. cost by caller ($/hour)", "Cache read / write tokens / second", "Time to first chunk (p95)"} {
		if _, ok := byTitle[title]; !ok {
			t.Errorf("missing panel %q", title)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
