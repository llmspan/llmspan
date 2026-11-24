package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestObserverMetricsFollowSemconv drives Finalize with a real SDK meter and
// asserts spec attribute names, absence of non-spec labels on standard
// metrics, and the span name.
func TestObserverMetricsFollowSemconv(t *testing.T) {
	t.Parallel()
	h, rec, tp := observerHarness(t)
	defer func() { _ = tp.Shutdown(context.Background()) }()

	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	meters, err := telemetry.NewGenAIMeters(mp.Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	h.meters = meters
	h.cfg.Upstreams[0].Name = "openai"
	h.cfg.Upstreams[0].Target = "https://api.openai.com"

	ctx, span := h.tracer.Start(context.Background(), "chat")
	info := &provider.Info{
		System: "openai", Operation: "chat", RequestModel: "gpt-4o-mini", ResponseModel: "gpt-4o-mini-2024",
		InputTokens: 10, OutputTokens: 5,
		Started: time.Now().Add(-time.Second),
	}
	o := newRequestObserver(h, h.providers["openai"], info, provider.ContentOpts{}, span, "openai",
		func() int { return 429 })
	o.Finalize(ctx)

	if got := rec.Ended()[0].Name(); got != "chat gpt-4o-mini" {
		t.Errorf("span name = %q", got)
	}
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	sets := map[string]*attribute.Set{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				sets[m.Name] = &d.DataPoints[0].Attributes
			case metricdata.Histogram[int64]:
				sets[m.Name] = &d.DataPoints[0].Attributes
			case metricdata.Sum[int64]:
				sets[m.Name] = &d.DataPoints[0].Attributes
			}
		}
	}
	dur := sets["gen_ai.client.inference.duration"]
	if dur == nil {
		t.Fatal("no inference duration metric")
	}
	for k, want := range map[string]string{
		"gen_ai.operation.name": "chat", "gen_ai.provider.name": "openai",
		"gen_ai.request.model": "gpt-4o-mini", "gen_ai.response.model": "gpt-4o-mini-2024",
		"error.type": "429", "server.address": "api.openai.com",
	} {
		if v, ok := dur.Value(attribute.Key(k)); !ok || v.String() != want {
			t.Errorf("duration %s = %v (%v), want %s", k, v.String(), ok, want)
		}
	}
	if v, _ := dur.Value("server.port"); v.AsInt64() != 443 {
		t.Errorf("server.port = %v", v.AsInt64())
	}
	for _, name := range []string{"gen_ai.client.operation.duration", "gen_ai.client.inference.duration", "gen_ai.client.token.usage"} {
		for _, bad := range []string{"upstream", "http.response.status_code", "token_type"} {
			if sets[name] == nil {
				t.Fatalf("missing %s", name)
			}
			if _, ok := sets[name].Value(attribute.Key(bad)); ok {
				t.Errorf("%s carries non-spec label %q", name, bad)
			}
		}
	}
	if v, ok := sets["gen_ai.client.token.usage"].Value("gen_ai.token.type"); !ok || v.String() == "" {
		t.Error("token.usage missing gen_ai.token.type")
	}
	if _, ok := sets["gen_ai.client.inference.usage.input_tokens"]; !ok {
		t.Error("missing inference usage counter")
	}
	if c := sets["gen_ai.client.cost.usd"]; c != nil {
		if v, ok := c.Value("gen_ai.operation.name"); !ok || v.String() != "chat" {
			t.Errorf("cost operation = %v", v.String())
		}
	}
}
