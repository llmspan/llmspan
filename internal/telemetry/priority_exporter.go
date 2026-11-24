package telemetry

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/llmspan/llmspan/internal/genai"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// prioritySpanExporter is a bounded, end-of-span sampling stage. Unlike a
// head sampler it can see outcome, latency, and computed USD cost. This is
// processor-style sampling, not true collector tail sampling: decisions are
// per span and state-free so memory use does not grow with active traces.
type prioritySpanExporter struct {
	trace.SpanExporter
	ratio         float64
	costThreshold float64
	slowThreshold time.Duration

	// promoted remembers trace IDs that had at least one priority span, so
	// the llmspan span and its otelhttp parent (which ends later, possibly
	// in another batch) are kept or dropped together. Bounded; see
	// promotedSet.
	promoted promotedSet
}

const (
	promotedCap = 8192
	promotedTTL = 2 * time.Minute
)

// promotedSet is a small bounded TTL set of trace IDs. Two generations are
// swapped when the current one fills or ages out, so memory is O(cap).
type promotedSet struct {
	mu      sync.Mutex
	cur     map[oteltrace.TraceID]struct{}
	prev    map[oteltrace.TraceID]struct{}
	rotated time.Time
}

func (p *promotedSet) rotateLocked(now time.Time) {
	if p.cur == nil || len(p.cur) >= promotedCap || now.Sub(p.rotated) > promotedTTL {
		p.prev, p.cur, p.rotated = p.cur, make(map[oteltrace.TraceID]struct{}), now
	}
}

func (p *promotedSet) add(id oteltrace.TraceID) {
	p.mu.Lock()
	p.rotateLocked(time.Now())
	p.cur[id] = struct{}{}
	p.mu.Unlock()
}

func (p *promotedSet) has(id oteltrace.TraceID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cur[id]; ok {
		return true
	}
	_, ok := p.prev[id]
	return ok
}

func (e *prioritySpanExporter) ExportSpans(ctx context.Context, spans []trace.ReadOnlySpan) error {
	if e.ratio >= 1 && e.costThreshold <= 0 && e.slowThreshold <= 0 {
		return e.SpanExporter.ExportSpans(ctx, spans)
	}
	// Pass 1: promote every trace containing a priority span, so siblings
	// and ancestors in this batch (and later batches) follow it.
	for _, span := range spans {
		if e.priority(span) {
			e.promoted.add(span.SpanContext().TraceID())
		}
	}
	kept := make([]trace.ReadOnlySpan, 0, len(spans))
	for _, span := range spans {
		if e.keep(span) {
			kept = append(kept, span)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return e.SpanExporter.ExportSpans(ctx, kept)
}

func (e *prioritySpanExporter) keep(span trace.ReadOnlySpan) bool {
	if e.priority(span) || e.promoted.has(span.SpanContext().TraceID()) {
		return true
	}
	if e.ratio <= 0 {
		return false
	}
	id := span.SpanContext().TraceID()
	n := binary.BigEndian.Uint64(id[:8])
	return float64(n)/float64(math.MaxUint64) < e.ratio
}

// priority reports whether the span alone warrants retention.
func (e *prioritySpanExporter) priority(span trace.ReadOnlySpan) bool {
	if span.Status().Code == codes.Error {
		return true
	}
	for _, attr := range span.Attributes() {
		switch string(attr.Key) {
		case genai.AttrErrorType:
			if attr.Value.AsString() != "" {
				return true
			}
		case "http.response.status_code":
			if attr.Value.AsInt64() >= 500 {
				return true
			}
		case genai.AttrCostUSD:
			if e.costThreshold > 0 && attr.Value.AsFloat64() >= e.costThreshold {
				return true
			}
		}
	}
	if e.slowThreshold > 0 && span.EndTime().Sub(span.StartTime()) >= e.slowThreshold {
		return true
	}
	return false
}
