package telemetry

import (
	"context"
	"testing"

	"github.com/llmspan/llmspan/internal/config"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestNewGenAIMetersRegistersAllInstruments(t *testing.T) {
	t.Parallel()
	m := noop.NewMeterProvider().Meter("test")
	got, err := NewGenAIMeters(m)
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenUsage == nil || got.OperationDuration == nil || got.TimeToFirstToken == nil ||
		got.TimeToFirstByte == nil || got.CostUSD == nil || got.CostUSDTotal == nil {
		t.Fatalf("incomplete meters: %+v", got)
	}
}

func TestBuildResourceServiceAttrs(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Service.Name = "llmspan-test"
	cfg.Service.Namespace = "ns"
	cfg.Service.Env = "ci"
	res, err := buildResource(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil {
		t.Fatal("nil resource")
	}
}

func TestPropagatorExcludesBaggage(t *testing.T) {
	t.Parallel()
	// Setup registers TraceContext only. W3C baggage from clients
	// must not enter the active context via the global propagator.
	p := propagation.TraceContext{}
	carrier := propagation.MapCarrier{
		"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"baggage":     "userId=alice",
	}
	ctx := p.Extract(context.Background(), carrier)
	bag := baggage.FromContext(ctx)
	if len(bag.Members()) != 0 {
		t.Fatal("TraceContext propagator must not extract baggage members")
	}
}

func TestMeterBucketsAndAttrs(t *testing.T) {
	t.Parallel()
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	g, err := NewGenAIMeters(mp.Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g.OperationDuration.Record(ctx, 0.5)
	g.TokenUsage.Record(ctx, 10)
	g.TimeToFirstChunk.Record(ctx, 0.003)
	g.InferenceInputTokens.Record(ctx, 10)
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	bounds := map[string][]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				bounds[m.Name] = d.DataPoints[0].Bounds
			case metricdata.Histogram[int64]:
				bounds[m.Name] = d.DataPoints[0].Bounds
			}
		}
	}
	if b := bounds["gen_ai.client.operation.duration"]; len(b) != 14 || b[13] != 81.92 {
		t.Errorf("duration bounds = %v", b)
	}
	if b := bounds["gen_ai.client.token.usage"]; len(b) != 14 || b[13] != 67108864 {
		t.Errorf("token bounds = %v", b)
	}
	if b := bounds["gen_ai.client.inference.time_to_first_chunk"]; b[0] >= 0.01 {
		t.Errorf("ttfc bounds = %v", b)
	}
	if b := bounds["gen_ai.client.inference.operation.input_tokens"]; len(b) != 14 {
		t.Errorf("inference token bounds = %v", b)
	}
}

func TestTemporalitySelectorEnv(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
	if temporalitySelector()(sdkmetric.InstrumentKindCounter) != metricdata.DeltaTemporality {
		t.Fatal("delta not honoured")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "")
	if temporalitySelector()(sdkmetric.InstrumentKindCounter) != metricdata.CumulativeTemporality {
		t.Fatal("default must be cumulative")
	}
}

func TestResourceHasInstanceID(t *testing.T) {
	t.Parallel()
	res, err := buildResource(context.Background(), config.Default())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := res.Set().Value("service.instance.id")
	if !ok || len(v.AsString()) != 36 {
		t.Fatalf("service.instance.id = %v %v", v, ok)
	}
}
