package telemetry

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/llmspan/llmspan/internal/config"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type metricdataTemporality = metricdata.Temporality

const (
	temporalityDelta      = metricdata.DeltaTemporality
	temporalityCumulative = metricdata.CumulativeTemporality
)

// newInstanceID returns a random RFC 4122 v4 UUID for service.instance.id.
func newInstanceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func envSet(keys ...string) bool {
	for _, k := range keys {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// endpointFromEnv reports whether a standard OTEL_EXPORTER_OTLP_*ENDPOINT
// variable applies to signal ("TRACES", "METRICS", "LOGS"). When true the
// SDK exporter reads the endpoint, scheme (insecure) and timeout itself, so
// llmspan must NOT pass explicit options that would override them.
func endpointFromEnv(signal string) bool {
	return envSet("OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT")
}

// baseOpts builds the endpoint+timeout options for an exporter, deferring to
// standard OTEL_EXPORTER_OTLP_* env when set. Protocol (grpc vs http) is
// still chosen by config; OTEL_EXPORTER_OTLP_PROTOCOL is not consulted.
func baseOpts[O any](signal string, t config.Telemetry, endpoint func(string) O, timeout func(time.Duration) O) []O {
	if endpointFromEnv(signal) {
		return nil
	}
	return []O{endpoint(t.Endpoint), timeout(t.Timeout)}
}

// temporalitySelector honours OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE:
// "cumulative" (default), "delta", or "lowmemory".
func temporalitySelector() sdkmetric.TemporalitySelector {
	switch strings.ToLower(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE")) {
	case "delta":
		return deltaSelector
	case "lowmemory":
		return lowMemorySelector
	default:
		return sdkmetric.DefaultTemporalitySelector
	}
}

func deltaSelector(k sdkmetric.InstrumentKind) metricdataTemporality {
	switch k {
	case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram, sdkmetric.InstrumentKindGauge, sdkmetric.InstrumentKindObservableCounter:
		return temporalityDelta
	}
	return temporalityCumulative
}

func lowMemorySelector(k sdkmetric.InstrumentKind) metricdataTemporality {
	switch k {
	case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram:
		return temporalityDelta
	}
	return temporalityCumulative
}

// isURLEndpoint reports whether telemetry.endpoint is a full URL such as
// https://otlp-gateway.example.com/otlp (needed for backends that serve OTLP
// under a path prefix, like Grafana Cloud). The URL scheme then decides TLS and
// telemetry.insecure is ignored.
func isURLEndpoint(e string) bool { return strings.Contains(e, "://") }

// httpEndpoint picks WithEndpointURL for URL-form endpoints and WithEndpoint for
// host:port, for the OTLP/HTTP exporters. A URL endpoint is a base URL, as with
// OTEL_EXPORTER_OTLP_ENDPOINT: the signal path (/v1/traces etc.) is appended,
// because WithEndpointURL itself would use the path verbatim.
func httpEndpoint[O any](plain, urlOpt func(string) O, signalPath string) func(string) O {
	return func(e string) O {
		if isURLEndpoint(e) {
			return urlOpt(strings.TrimRight(e, "/") + signalPath)
		}
		return plain(e)
	}
}
