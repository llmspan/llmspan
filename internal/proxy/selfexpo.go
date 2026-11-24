package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Prometheus text exposition of the llmspan.* self metrics.
//
// The OTel Prometheus exporter would pull client_golang and a second
// registry into the binary for ~10 instruments, and the MeterProvider is
// owned by the telemetry package. Instead each self-metric instrument is
// wrapped so every Add/Record is mirrored into this small in-process
// registry, which renders text format 0.0.4 on the admin listener. The
// OTLP path is unchanged: wrappers delegate first.
//
// Only the self metrics (llmspan.*) are exposed here. GenAI metrics keep
// flowing through OTLP; they are histograms with caller/model labels and
// belong in the metrics backend, not in a per-pod scrape.

type selfKind int

const (
	selfCounter selfKind = iota
	selfGauge
)

type selfSample struct {
	labels attribute.Set
	value  float64
}

type selfFamily struct {
	name, help string
	kind       selfKind
	samples    map[string]*selfSample // keyed by label-set encoding
}

type selfRegistry struct {
	mu       sync.Mutex
	families map[string]*selfFamily
}

func newSelfRegistry() *selfRegistry {
	return &selfRegistry{families: map[string]*selfFamily{}}
}

func (r *selfRegistry) family(name, help string, kind selfKind) *selfFamily {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.families[name]
	if !ok {
		f = &selfFamily{name: name, help: help, kind: kind, samples: map[string]*selfSample{}}
		r.families[name] = f
	}
	return f
}

func (r *selfRegistry) add(f *selfFamily, delta float64, set attribute.Set) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := f.sample(set)
	s.value += delta
}

func (r *selfRegistry) set(f *selfFamily, v float64, set attribute.Set) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.sample(set).value = v
}

func (f *selfFamily) sample(set attribute.Set) *selfSample {
	k := set.Encoded(attribute.DefaultEncoder())
	s, ok := f.samples[k]
	if !ok {
		s = &selfSample{labels: set}
		f.samples[k] = s
	}
	return s
}

// counter wraps an Int64Counter (monotonic).
func (r *selfRegistry) counter(c metric.Int64Counter, name, help string) metric.Int64Counter {
	return &mirrorCounter{Int64Counter: c, reg: r, fam: r.family(name, help, selfCounter)}
}

// upDown wraps an Int64UpDownCounter; exposed as a gauge.
func (r *selfRegistry) upDown(c metric.Int64UpDownCounter, name, help string) metric.Int64UpDownCounter {
	return &mirrorUpDown{Int64UpDownCounter: c, reg: r, fam: r.family(name, help, selfGauge)}
}

// gauge wraps an Int64Gauge.
func (r *selfRegistry) gauge(g metric.Int64Gauge, name, help string) metric.Int64Gauge {
	return &mirrorGauge{Int64Gauge: g, reg: r, fam: r.family(name, help, selfGauge)}
}

type mirrorCounter struct {
	metric.Int64Counter
	reg *selfRegistry
	fam *selfFamily
}

func (m *mirrorCounter) Add(ctx context.Context, incr int64, opts ...metric.AddOption) {
	m.Int64Counter.Add(ctx, incr, opts...)
	m.reg.add(m.fam, float64(incr), metric.NewAddConfig(opts).Attributes())
}

type mirrorUpDown struct {
	metric.Int64UpDownCounter
	reg *selfRegistry
	fam *selfFamily
}

func (m *mirrorUpDown) Add(ctx context.Context, incr int64, opts ...metric.AddOption) {
	m.Int64UpDownCounter.Add(ctx, incr, opts...)
	m.reg.add(m.fam, float64(incr), metric.NewAddConfig(opts).Attributes())
}

type mirrorGauge struct {
	metric.Int64Gauge
	reg *selfRegistry
	fam *selfFamily
}

func (m *mirrorGauge) Record(ctx context.Context, v int64, opts ...metric.RecordOption) {
	m.Int64Gauge.Record(ctx, v, opts...)
	m.reg.set(m.fam, float64(v), metric.NewRecordConfig(opts).Attributes())
}

// promName maps an OTel instrument/attribute name to a Prometheus one.
func promName(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			b[i] = '_'
		}
	}
	return string(b)
}

var promEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// writeTo renders the registry in Prometheus text format 0.0.4, sorted
// for stable output.
func (r *selfRegistry) writeTo(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.families[n]
		pn := promName(f.name)
		typ := "gauge"
		if f.kind == selfCounter {
			typ = "counter"
		}
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", pn, promEscaper.Replace(f.help), pn, typ)
		lines := make([]string, 0, len(f.samples))
		for _, s := range f.samples {
			var lb strings.Builder
			iter := s.labels.Iter()
			first := true
			for iter.Next() {
				kv := iter.Attribute()
				if !first {
					lb.WriteByte(',')
				}
				first = false
				lb.WriteString(promName(string(kv.Key)))
				lb.WriteString(`="`)
				lb.WriteString(promEscaper.Replace(kv.Value.String()))
				lb.WriteByte('"')
			}
			if lb.Len() > 0 {
				lines = append(lines, fmt.Sprintf("%s{%s} %v\n", pn, lb.String(), s.value))
			} else {
				lines = append(lines, fmt.Sprintf("%s %v\n", pn, s.value))
			}
		}
		sort.Strings(lines)
		for _, l := range lines {
			_, _ = io.WriteString(w, l)
		}
	}
}

// MetricsHandler serves the llmspan.* self metrics in Prometheus text
// format. Mount it ONLY on the admin listener.
func (h *Handler) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			return
		}
		h.selfReg.writeTo(w)
		h.writeExportHealth(w)
	})
}

// writeExportHealth renders the OTLP export health series. They are kept
// in-process rather than going through OTLP because OTLP is exactly what is
// broken in a collector outage. Both series are always present (0 / never) so
// alerts can distinguish "healthy" from "series missing".
func (h *Handler) writeExportHealth(w io.Writer) {
	var errs int64
	if h.exportErrors != nil {
		errs = h.exportErrors()
	}
	_, _ = fmt.Fprintf(w, "# HELP llmspan_telemetry_export_errors_total OpenTelemetry SDK/exporter errors (dropped batches, unreachable collector).\n"+
		"# TYPE llmspan_telemetry_export_errors_total counter\nllmspan_telemetry_export_errors_total %d\n", errs)
	var ts float64
	if h.lastExportOK != nil {
		if t := h.lastExportOK(); !t.IsZero() {
			ts = float64(t.UnixNano()) / 1e9
		}
	}
	_, _ = fmt.Fprintf(w, "# HELP llmspan_telemetry_last_export_success_timestamp_seconds Unix time of the last successful trace or metric export (0 = none yet).\n"+
		"# TYPE llmspan_telemetry_last_export_success_timestamp_seconds gauge\nllmspan_telemetry_last_export_success_timestamp_seconds %.3f\n", ts)
}
