package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHash = "$argon2id$v=19$m=65536,t=3,p=2$YWFhYWFhYWFhYWFhYWFhYQ$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestDefaultIsValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
}

func TestLoadYAMLOverrides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := []byte(`
listen: "127.0.0.1:9999"
upstreams:
  - name: openai
    prefix: /v1
    target: https://api.openai.com
    provider: openai
telemetry:
  endpoint: tempo:4317
  protocol: grpc
  insecure: true
  acknowledge_insecure: true   # tempo:4317 isn't loopback / RFC1918
  sample_ratio: 0.5
content:
  mode: events
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Telemetry.SampleRatio != 0.5 {
		t.Errorf("sample_ratio = %v", cfg.Telemetry.SampleRatio)
	}
	if cfg.Content.Mode != CaptureEvents {
		t.Errorf("content.mode = %q", cfg.Content.Mode)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("LLMSPAN_LISTEN", "127.0.0.1:12345")
	t.Setenv("LLMSPAN_OTLP_ENDPOINT", "tempo.svc:4317")
	t.Setenv("LLMSPAN_OTLP_INSECURE", "false") // tempo.svc isn't local; keep TLS at collector
	t.Setenv("LLMSPAN_CAPTURE", "events")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:12345" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Telemetry.Endpoint != "tempo.svc:4317" {
		t.Errorf("endpoint = %q", cfg.Telemetry.Endpoint)
	}
	if cfg.Content.Mode != CaptureEvents {
		t.Errorf("capture = %q", cfg.Content.Mode)
	}
}

// TestLoadPrecedenceEnvOverYAML pins defaults → YAML → env.
func TestLoadPrecedenceEnvOverYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := []byte(`
listen: "127.0.0.1:8888"
telemetry:
  endpoint: yaml-host:4317
  protocol: grpc
  insecure: true
  acknowledge_insecure: true
content:
  mode: off
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSPAN_LISTEN", "127.0.0.1:7777")
	t.Setenv("LLMSPAN_CAPTURE", "logs")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:7777" {
		t.Errorf("listen = %q, env should win", cfg.Listen)
	}
	if cfg.Content.Mode != CaptureLogs {
		t.Errorf("content.mode = %q, env should win", cfg.Content.Mode)
	}
	if cfg.Telemetry.Endpoint != "yaml-host:4317" {
		t.Errorf("endpoint = %q, yaml should stick when env unset", cfg.Telemetry.Endpoint)
	}
}

func TestAttributionHeaderCannotConsumeCredentials(t *testing.T) {
	t.Parallel()
	for _, header := range []string{"Authorization", "authorization"} {
		cfg := Default()
		cfg.Attribution.Header = header
		if err := cfg.Validate(); err == nil {
			t.Fatalf("header %q accepted", header)
		}
	}
}

// TestEnvOverridesOTLPHeaders covers the L1 low-tier env var. Headers
// come from container orchestrators where YAML is awkward and the
// classic `valueFrom: secretKeyRef` path is via env.
func TestEnvOverridesOTLPHeaders(t *testing.T) {
	t.Setenv("LLMSPAN_OTLP_HEADERS", "Authorization=Bearer abc, X-Scope-OrgID=42 , X-Tenant=acme")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := map[string]string{
		"Authorization": "Bearer abc",
		"X-Scope-OrgID": "42",
		"X-Tenant":      "acme",
	}
	for k, want := range cases {
		if got := cfg.Telemetry.Headers[k]; got != want {
			t.Errorf("Headers[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestEnvOverridesSampleRatio covers the L2 low-tier env var. Includes
// the out-of-range clamp paths the PLAN explicitly called out: don't
// refuse startup — sampling is not a correctness knob.
func TestEnvOverridesSampleRatio(t *testing.T) {
	cases := []struct {
		env  string
		want float64
	}{
		{"0", 0},
		{"0.25", 0.25},
		{"1", 1},
		{"1.5", 1},  // clamped down
		{"-0.1", 0}, // clamped up
		{"3", 1},    // clamped down
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LLMSPAN_SAMPLE_RATIO", tc.env)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Telemetry.SampleRatio != tc.want {
				t.Errorf("SampleRatio = %v, want %v", cfg.Telemetry.SampleRatio, tc.want)
			}
		})
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"empty listen", func(c *Config) { c.Listen = "" }},
		{"no upstreams", func(c *Config) { c.Upstreams = nil }},
		{"bad prefix", func(c *Config) { c.Upstreams[0].Prefix = "v1" }},
		{"bad provider", func(c *Config) { c.Upstreams[0].Provider = "cohere" }},
		{"bad protocol", func(c *Config) { c.Telemetry.Protocol = "tcp" }},
		{"bad ratio", func(c *Config) { c.Telemetry.SampleRatio = 2 }},
		{"bad capture", func(c *Config) { c.Content.Mode = "verbose" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNonLoopbackRequiresTLSOrOptIn(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Listen = "0.0.0.0:4000"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-loopback without TLS or allow_insecure must be rejected")
	}

	cfg.AllowInsecure = true
	cfg.AllowUnauthenticated = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("allow_insecure should permit non-loopback plaintext: %v", err)
	}

	cfg.AllowInsecure = false
	cfg.TLS = TLS{CertFile: "/tmp/c.pem", KeyFile: "/tmp/k.pem"}
	if err := cfg.Validate(); err != nil {
		t.Errorf("TLS should permit non-loopback: %v", err)
	}
}

func TestNonLoopbackRequiresAuthOrOptIn(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Listen = "0.0.0.0:4443"
	cfg.TLS = TLS{CertFile: "/tmp/c.pem", KeyFile: "/tmp/k.pem"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "allow_unauthenticated") {
		t.Fatalf("TLS-only non-loopback must demand auth or opt-in, got %v", err)
	}
	cfg.AllowUnauthenticated = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("allow_unauthenticated should permit: %v", err)
	}
	cfg.AllowUnauthenticated = false
	cfg.TLS.ClientCAFile = "/tmp/ca.pem"
	if err := cfg.Validate(); err != nil {
		t.Errorf("mTLS counts as authentication: %v", err)
	}
	cfg.TLS.ClientCAFile = ""
	cfg.Auth.Tokens = []string{"svc:" + testHash}
	if err := cfg.Validate(); err != nil {
		t.Errorf("tokens count as authentication: %v", err)
	}
}

func TestAllowUnauthenticatedEnv(t *testing.T) {
	t.Setenv("LLMSPAN_LISTEN", "0.0.0.0:4000")
	t.Setenv("LLMSPAN_ALLOW_INSECURE", "true")
	if _, err := Load(""); err == nil {
		t.Fatal("expected refusal without LLMSPAN_ALLOW_UNAUTHENTICATED")
	}
	t.Setenv("LLMSPAN_ALLOW_UNAUTHENTICATED", "true")
	if _, err := Load(""); err != nil {
		t.Fatalf("env opt-in should load: %v", err)
	}
}

func TestYAMLUpstreamsGetSafeDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := []byte(`
upstreams:
  - name: a
    prefix: /a
    target: https://a.example
    provider: openai
  - name: b
    prefix: /b
    target: https://b.example
    provider: openai
    max_in_flight: 0
    breaker:
      failures: 0
  - name: c
    prefix: /c
    target: https://c.example
    provider: openai
    max_in_flight: 7
    response_header_timeout: 20m
    breaker:
      failures: 2
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := cfg.Upstreams[0], cfg.Upstreams[1], cfg.Upstreams[2]
	if a.MaxInFlight != 256 || a.Breaker.Failures != 5 || a.Breaker.Window == 0 || a.ResponseHeaderTimeout != 5*time.Minute {
		t.Errorf("omitted settings must default: %+v", a)
	}
	if b.MaxInFlight != 0 || b.Breaker.Failures != 0 {
		t.Errorf("explicit zero must disable: %+v", b)
	}
	if c.MaxInFlight != 7 || c.Breaker.Failures != 2 || c.Breaker.RecoveryWindow != 15*time.Second || c.ResponseHeaderTimeout != 20*time.Minute {
		t.Errorf("explicit values kept, gaps defaulted: %+v", c)
	}
}

func TestResponseHeaderTimeoutValidation(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Upstreams[0].ResponseHeaderTimeout = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative response_header_timeout must be rejected")
	}
}

func TestBreakerCounts(t *testing.T) {
	t.Parallel()
	b := BreakerConfig{Failures: 5}
	for st, want := range map[int]bool{200: false, 400: false, 429: false, 499: false, 500: true, 502: true, 503: true, 529: false, 599: true} {
		if got := b.Counts(st); got != want {
			t.Errorf("Counts(%d) = %v, want %v", st, got, want)
		}
	}
	b.TripOnOverloaded = true
	b.IgnoreStatuses = []int{503}
	if !b.Counts(529) || b.Counts(503) || !b.Counts(500) {
		t.Error("TripOnOverloaded / IgnoreStatuses not honoured")
	}
	cfg := Default()
	cfg.Upstreams[0].Breaker.IgnoreStatuses = []int{429}
	if err := cfg.Validate(); err == nil {
		t.Error("non-5xx ignore status must be rejected")
	}
}

func TestAdminListenValidation(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Admin.Listen = "0.0.0.0:9464"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-loopback admin must need allow_non_loopback")
	}
	cfg.Admin.AllowNonLoopback = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Admin = Admin{Listen: "127.0.0.1:9464"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTLSPartialIsRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tls  TLS
	}{
		{"only cert", TLS{CertFile: "/tmp/c.pem"}},
		{"only key", TLS{KeyFile: "/tmp/k.pem"}},
		{"client CA without TLS", TLS{ClientCAFile: "/tmp/ca.pem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.TLS = tc.tls
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateRejectsInsecureWAN(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Telemetry.Endpoint = "otel.example.com:4317"
	cfg.Telemetry.Insecure = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("insecure OTLP to a non-local endpoint must be rejected")
	}
}

func TestValidateAcknowledgeInsecureBypassesGuard(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Telemetry.Endpoint = "otel.example.com:4317"
	cfg.Telemetry.Insecure = true
	cfg.Telemetry.AcknowledgeInsecure = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("acknowledge_insecure=true must allow plaintext OTLP off-host: %v", err)
	}
}

func TestValidateInsecureLocalEndpoints(t *testing.T) {
	t.Parallel()

	cases := []string{
		"localhost:4317",
		"127.0.0.1:4317",
		"[::1]:4317",
		"10.0.0.5:4317",
		"172.16.5.5:4317",
		"192.168.1.5:4317",
		"https://localhost:4318",
		"http://127.0.0.1:4318",
	}
	for _, ep := range cases {
		cfg := Default()
		cfg.Telemetry.Endpoint = ep
		cfg.Telemetry.Insecure = true
		if strings.Contains(ep, "://") {
			cfg.Telemetry.Protocol = ProtoHTTP // URL form is http-only
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("endpoint %q + insecure should be allowed: %v", ep, err)
		}
	}
}

func TestValidateSecureWANOK(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Telemetry.Endpoint = "otel.example.com:4317"
	cfg.Telemetry.Insecure = false
	if err := cfg.Validate(); err != nil {
		t.Errorf("secure OTLP to a WAN endpoint must be allowed: %v", err)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		listen string
		want   bool
	}{
		{"127.0.0.1:4000", true},
		{"localhost:4000", true},
		{"LOCALHOST:4000", true},
		{"[::1]:4000", true},
		{":4000", false},
		{"0.0.0.0:4000", false},
		{"10.0.0.5:4000", false},
		{"example.com:4000", false},
		{"not-a-host", false},
	}
	for _, tc := range cases {
		if got := isLoopbackAddr(tc.listen); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v want %v", tc.listen, got, tc.want)
		}
	}
}

func TestMatchLongestPrefix(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Upstreams = append(cfg.Upstreams, Upstream{
		Name: "openai-emb", Prefix: "/v1/embeddings",
		Target: "https://api.openai.com", Provider: ProviderOpenAI,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	got, ok := cfg.Match("/v1/embeddings")
	if !ok || got.Name != "openai-emb" {
		t.Fatalf("longest-prefix match failed: %+v ok=%v", got, ok)
	}
	got, ok = cfg.Match("/v1/chat/completions")
	if !ok || got.Name != "openai" {
		t.Fatalf("default openai prefix failed: %+v ok=%v", got, ok)
	}
	if _, ok := cfg.Match("/random"); ok {
		t.Fatal("expected no match for /random")
	}
}

func TestValidateTelemetryURLEndpoint(t *testing.T) {
	t.Parallel()

	// https URL with a path (Grafana Cloud style): TLS comes from the scheme, so
	// the default insecure=true must not demand an acknowledgement.
	cfg := Default()
	cfg.Telemetry.Protocol = ProtoHTTP
	cfg.Telemetry.Endpoint = "https://otlp-gateway-prod-us-central-0.grafana.net/otlp"
	if err := cfg.Validate(); err != nil {
		t.Errorf("https URL endpoint should validate: %v", err)
	}

	// Plaintext http to a non-local host still needs the acknowledgement.
	cfg = Default()
	cfg.Telemetry.Protocol = ProtoHTTP
	cfg.Telemetry.Endpoint = "http://otel.example.com/otlp"
	cfg.Telemetry.Insecure = false // ignored for URL endpoints
	if err := cfg.Validate(); err == nil {
		t.Error("plaintext http URL to a non-local host must be rejected")
	}
	cfg.Telemetry.AcknowledgeInsecure = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("acknowledged plaintext URL should validate: %v", err)
	}

	// URL form is meaningless for grpc.
	cfg = Default()
	cfg.Telemetry.Protocol = ProtoGRPC
	cfg.Telemetry.Endpoint = "https://otel.example.com/otlp"
	if err := cfg.Validate(); err == nil {
		t.Error("URL endpoint with grpc must be rejected")
	}
}
