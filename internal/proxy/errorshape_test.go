package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/auth"
	"github.com/llmspan/llmspan/internal/config"
)

type shapedError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeShaped(t *testing.T, resp *http.Response) shapedError {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if resp.Header.Get(headerRequestID) == "" {
		t.Error("proxy-generated errors must carry a request id")
	}
	var e shapedError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if e.Error.Type != "llmspan_proxy_error" {
		t.Errorf("type = %q, want llmspan_proxy_error (lets clients tell llmspan from the provider)", e.Error.Type)
	}
	return e
}

// An open breaker answers 503 with a JSON llmspan_proxy_error, not plain text,
// so it can be told apart from an upstream 503.
func TestBreakerOpenIsJSONProxyError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{
		Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai",
		Breaker: config.BreakerConfig{Failures: 2, Window: time.Minute, RecoveryWindow: time.Minute},
	}}
	_, _, ts := selfMetricHarness(t, cfg)
	body := []byte(`{"model":"gpt-5-mini","messages":[{"role":"user","content":"x"}]}`)
	post := func() *http.Response {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	var last *http.Response
	for i := 0; i < 20; i++ { // trips after 2 upstream failures; poll for the open state
		last = post()
		if last.StatusCode == http.StatusServiceUnavailable {
			break
		}
		_, _ = io.Copy(io.Discard, last.Body)
		_ = last.Body.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if last.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("breaker never opened, last status %d", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("missing Retry-After on breaker 503")
	}
	if e := decodeShaped(t, last); e.Error.Code != "circuit_open" {
		t.Errorf("code = %q, want circuit_open", e.Error.Code)
	}
}

func TestUnauthorizedIsJSONProxyError(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(upstream.Close)
	hashed, err := auth.Hash("right-token-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: upstream.URL, Provider: "openai"}}
	cfg.Auth.Tokens = []string{hashed}
	_, _, ts := selfMetricHarness(t, cfg)

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if e := decodeShaped(t, resp); e.Error.Code != "unauthorized" {
		t.Errorf("code = %q, want unauthorized", e.Error.Code)
	}
}

// The admin /metrics output must always carry the OTLP export-health series
// (even with nothing wrong), because OTLP itself is what fails in a collector
// outage.
func TestAdminMetricsExposeExportHealth(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: "http://127.0.0.1:1", Provider: "openai"}}
	h, _, _ := selfMetricHarness(t, cfg)

	render := func() string {
		rec := httptest.NewRecorder()
		h.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	out := render()
	for _, want := range []string{
		"llmspan_telemetry_export_errors_total 0\n",
		"llmspan_telemetry_last_export_success_timestamp_seconds 0.000\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}

	h.exportErrors = func() int64 { return 7 }
	h.lastExportOK = func() time.Time { return time.Unix(1700000000, 0) }
	out = render()
	for _, want := range []string{
		"llmspan_telemetry_export_errors_total 7\n",
		"llmspan_telemetry_last_export_success_timestamp_seconds 1700000000.000\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
}
