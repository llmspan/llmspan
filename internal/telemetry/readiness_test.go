package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestGateStartsNotReadyUntilExport(t *testing.T) {
	t.Parallel()
	g := NewGate(true)
	if g.Ready() {
		t.Fatal("new gate must start not-ready")
	}
	g.MarkExportOK()
	if !g.Ready() {
		t.Fatal("gate must be ready after successful export")
	}
	g.BeginDrain()
	if g.Ready() {
		t.Fatal("gate must go not-ready on drain")
	}
}

func TestExportOKSpanExporter(t *testing.T) {
	t.Parallel()
	g := NewGate(true)
	inner := &stubSpanExporter{}
	exp := &exportOKSpanExporter{SpanExporter: inner, gate: g}

	if err := exp.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !g.Ready() {
		t.Fatal("successful ExportSpans must flip the gate")
	}
}

func TestExportOKSpanExporterKeepsGateOnError(t *testing.T) {
	t.Parallel()
	g := NewGate(true)
	inner := &stubSpanExporter{err: errors.New("collector down")}
	exp := &exportOKSpanExporter{SpanExporter: inner, gate: g}

	if err := exp.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("expected export error")
	}
	if g.Ready() {
		t.Fatal("failed export must not flip the gate")
	}
}

func TestExportOKMetricExporter(t *testing.T) {
	t.Parallel()
	g := NewGate(true)
	inner := &stubMetricExporter{}
	exp := &exportOKMetricExporter{Exporter: inner, gate: g}

	if err := exp.Export(context.Background(), &metricdata.ResourceMetrics{}); err != nil {
		t.Fatal(err)
	}
	if !g.Ready() {
		t.Fatal("successful metric Export must flip the gate")
	}
}

type stubSpanExporter struct{ err error }

func (s *stubSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return s.err
}
func (s *stubSpanExporter) Shutdown(context.Context) error { return nil }

type stubMetricExporter struct{ err error }

func (s *stubMetricExporter) Temporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}
func (s *stubMetricExporter) Aggregation(sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return nil
}
func (s *stubMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return s.err
}
func (s *stubMetricExporter) ForceFlush(context.Context) error { return nil }
func (s *stubMetricExporter) Shutdown(context.Context) error   { return nil }

func TestGateFailOpenByDefault(t *testing.T) {
	g := NewGate(false)
	if !g.Ready() {
		t.Fatal("gate must be ready without any export when ready_requires_export is false")
	}
	g.BeginDrain()
	if g.Ready() {
		t.Fatal("draining must still make the gate not-ready")
	}
}

func TestGateLastExportOK(t *testing.T) {
	g := NewGate(false)
	if !g.LastExportOK().IsZero() {
		t.Fatal("no export yet: want zero time")
	}
	before := time.Now()
	g.MarkExportOK()
	got := g.LastExportOK()
	if got.Before(before) || time.Since(got) > time.Minute {
		t.Fatalf("LastExportOK = %v, want about now", got)
	}
	var nilGate *Gate
	if !nilGate.LastExportOK().IsZero() {
		t.Fatal("nil gate must report zero time")
	}
}
