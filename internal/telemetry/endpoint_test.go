package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A URL-form endpoint must keep its path prefix: Grafana Cloud serves OTLP
// under /otlp, so spans must land on /otlp/v1/traces, not /v1/traces.
func TestTraceExporterHonoursURLPathPrefix(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	exp, err := newTraceExporter(context.Background(), config.Telemetry{
		Endpoint: srv.URL + "/otlp",
		Protocol: config.ProtoHTTP,
		Insecure: true, // must be ignored for URL endpoints
		Headers:  config.Headers{"Authorization": "Basic dGVzdA=="},
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	spans := tracetest.SpanStubs{{Name: "x"}}.Snapshots()
	if err := exp.ExportSpans(context.Background(), spans); err != nil {
		t.Fatalf("export: %v", err)
	}
	_ = exp.Shutdown(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/otlp/v1/traces" {
		t.Errorf("path = %q, want /otlp/v1/traces", gotPath)
	}
	if gotAuth != "Basic dGVzdA==" {
		t.Errorf("authorization header not forwarded: %q", gotAuth)
	}
}
