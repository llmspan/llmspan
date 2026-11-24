package telemetry

import (
	"context"
	"sync/atomic"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Gate is the two-latch readiness signal for /readyz:
//
//  1. exportOK flips true on the first successful OTLP export
//     (trace or metric). It starts true unless telemetry.ready_requires_export
//     is set, so an unreachable collector does not fail readiness.
//  2. draining flips true on SIGTERM / ctx cancel so /readyz goes
//     503 before graceful drain, giving the load balancer time to
//     stop routing (pair with a k8s preStop / terminationGracePeriod).
//
// Ready is true only when exportOK && !draining.
type Gate struct {
	exportOK atomic.Bool
	draining atomic.Bool
	lastOK   atomic.Int64 // unix nanos of the last successful export; 0 = never
}

// NewGate returns a Gate. With requireExport=false (the default) it is ready
// immediately and only draining makes it not-ready, so a collector outage never
// removes the proxy from rotation. With requireExport=true it starts not-ready
// until the first successful export.
func NewGate(requireExport bool) *Gate {
	g := &Gate{}
	if !requireExport {
		g.exportOK.Store(true)
	}
	return g
}

// Ready reports whether /readyz should return 200.
func (g *Gate) Ready() bool {
	return g != nil && g.exportOK.Load() && !g.draining.Load()
}

// MarkExportOK records a successful OTLP export. Idempotent.
func (g *Gate) MarkExportOK() {
	if g != nil {
		g.exportOK.Store(true)
		g.lastOK.Store(time.Now().UnixNano())
	}
}

// LastExportOK returns when an export last succeeded, or the zero time if none
// has yet.
func (g *Gate) LastExportOK() time.Time {
	if g == nil {
		return time.Time{}
	}
	n := g.lastOK.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// BeginDrain marks the process as terminating. Idempotent. Call as
// soon as SIGTERM is received, before waiting for in-flight work.
func (g *Gate) BeginDrain() {
	if g != nil {
		g.draining.Store(true)
	}
}

// exportOKSpanExporter wraps a SpanExporter and flips the gate on
// the first successful ExportSpans.
type exportOKSpanExporter struct {
	sdktrace.SpanExporter
	gate *Gate
}

func (e *exportOKSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := e.SpanExporter.ExportSpans(ctx, spans)
	if err == nil {
		e.gate.MarkExportOK()
	}
	return err
}

// exportOKMetricExporter wraps a metric Exporter and flips the gate
// on the first successful Export.
type exportOKMetricExporter struct {
	sdkmetric.Exporter
	gate *Gate
}

func (e *exportOKMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := e.Exporter.Export(ctx, rm)
	if err == nil {
		e.gate.MarkExportOK()
	}
	return err
}
