// Package config loads and validates llmspan's runtime configuration.
//
// Precedence (lowest to highest): built-in defaults -> YAML file -> environment
// variables. Environment variables prefixed with LLMSPAN_ override scalar
// fields (see EnvLookup). The full schema is documented in config.example.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/llmspan/llmspan/internal/auth"
	"github.com/llmspan/llmspan/internal/provider"

	"gopkg.in/yaml.v3"
)

// Config is the validated runtime config for the proxy.
type Config struct {
	Listen        string `yaml:"listen"`
	AllowInsecure bool   `yaml:"allow_insecure"`
	// AllowUnauthenticated acknowledges serving a non-loopback listener
	// with no listener auth (no auth.tokens and no mTLS client CA). TLS
	// alone only protects the wire; it does not stop anyone who can
	// reach the port from spending the operator's upstream API quota
	// through the proxy, so this must be an explicit opt-in.
	AllowUnauthenticated bool           `yaml:"allow_unauthenticated"`
	TLS                  TLS            `yaml:"tls"`
	Upstreams            []Upstream     `yaml:"upstreams"`
	Telemetry            Telemetry      `yaml:"telemetry"`
	Content              ContentCapture `yaml:"content"`
	HTTP                 HTTPTimeouts   `yaml:"http"`
	Service              Service        `yaml:"service"`
	Request              Request        `yaml:"request"`
	Pricing              Pricing        `yaml:"pricing"`
	Auth                 Auth           `yaml:"auth"`
	Attribution          Attribution    `yaml:"attribution"`
	Recording            Recording      `yaml:"recording"`
	Admin                Admin          `yaml:"admin"`
}

// Admin configures the optional self-observability listener. It is a
// SEPARATE socket from the proxy listener so scrape traffic never
// shares auth, TLS, or the upstream routing table with LLM traffic.
type Admin struct {
	// Listen is the address of the admin listener serving /metrics
	// (Prometheus text format) for the llmspan.* self metrics. Empty
	// (default) disables it.
	Listen string `yaml:"listen"`
	// AllowNonLoopback permits Listen to bind a non-loopback address
	// (needed for in-cluster Prometheus scraping). The listener is
	// plaintext and unauthenticated: restrict it with a NetworkPolicy.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

type Recording struct {
	// Path enables append-only JSONL request/response recording. Payloads
	// always pass through Content.Redact before being written.
	Path string `yaml:"path"`
}

// Attribution selects the caller identity attached to spans and metrics.
// Priority is configured header, mTLS certificate CN, then a one-way
// fingerprint of the listener auth token.
type Attribution struct {
	Header         string `yaml:"header"`
	MaxCardinality int    `yaml:"max_cardinality"`
	// ConversationHeader names a request header whose value becomes the
	// gen_ai.conversation.id span attribute (stripped before forwarding).
	// Spans only; never a metric label (unbounded cardinality).
	ConversationHeader string `yaml:"conversation_header"`
	// GrafanaAgentO11y stamps agento11y.record=true on spans so Grafana
	// Cloud Agent Observability ingests them as conversations.
	GrafanaAgentO11y bool `yaml:"grafana_agent_o11y"`
}

// Auth controls listener-side bearer-token authentication. Each token
// entry is either a bare PHC hash or "name:<PHC hash>"; a named entry
// makes the name the caller identity for spans/metrics/logs (taking
// priority over the client-supplied attribution header, which a named
// caller can no longer spoof). When Tokens
// is non-empty (after merging TokensFile), every request must present a
// matching token in the configured Header, or the proxy responds 401
// with no upstream call. Stored tokens are argon2id PHC hashes —
// generate with `llmspan hash-token`.
type Auth struct {
	Tokens []string `yaml:"tokens"`
	// TokensFile is an optional path to a file containing additional
	// PHC hashes, one per line (`#` comments and blank lines ignored).
	// A line is `<hash>`, `name:<hash>`, or `name <hash>`.
	// Intended for Kubernetes Secret mounts so hashes never live in
	// the ConfigMap. Entries are appended to Tokens at Load time.
	TokensFile string `yaml:"tokens_file"`
	// Header is the request header llmspan reads the bearer token from.
	// Default `X-LLMSPAN-Token` keeps the upstream `Authorization`
	// header untouched so the LLM API key still reaches the upstream
	// verbatim. Operators who want to combine boundaries can set this
	// to `Authorization`, in which case the upstream API key must come
	// from a different header (e.g. `X-Upstream-Authorization`).
	Header string `yaml:"header"`
}

// Enabled reports whether the auth gate is active.
func (a Auth) Enabled() bool { return len(a.Tokens) > 0 }

// HeaderName returns the configured header or the documented default.
func (a Auth) HeaderName() string {
	if a.Header == "" {
		return "X-LLMSPAN-Token"
	}
	return a.Header
}

// Pricing overrides the built-in cost catalogue with operator-supplied
// rates. Production deployments should point Path at a YAML file
// matching `internal/pricing/prices.yaml`'s shape; the override is
// merged on top of the built-ins so unspecified models keep their
// default rate.
type Pricing struct {
	// Path is the YAML override file. Empty = use built-ins only.
	Path string `yaml:"path"`
	// FailOpen, when true, swallows file-missing / malformed errors and
	// falls back to the built-in catalogue. When false (the default),
	// such errors fail the process so the operator notices.
	FailOpen bool `yaml:"fail_open"`
}

// Request shapes the proxy's handling of inbound request bodies.
type Request struct {
	// MaxBodyBytes is the hard ceiling above which the proxy refuses an
	// inbound request body with 413. Below this, bodies are forwarded
	// byte-for-byte even if they exceed the enrichment buffer used to
	// parse model / stream / token metadata. A value of 0 disables the
	// limit, which is rarely what the operator wants.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxBufferedBodies is a process-wide cap on how many requests may
	// hold their (up to 1 MiB) enrichment head buffer at once. Above
	// it the proxy answers 429 + Retry-After: 1 without reading the
	// body. Bounds buffer memory to roughly MaxBufferedBodies x 1 MiB
	// independent of the per-upstream max_in_flight. 0 = unlimited.
	MaxBufferedBodies int `yaml:"max_buffered_bodies"`
}

// TLS configures listener-side TLS termination. Setting CertFile and KeyFile
// flips the listener to HTTPS. Setting ClientCAFile additionally requires
// every client to present a certificate signed by that CA — the right knob
// when llmspan is shared across services in a VPC and you want the proxy to
// be the audit boundary.
type TLS struct {
	CertFile     string `yaml:"cert_file"`
	KeyFile      string `yaml:"key_file"`
	ClientCAFile string `yaml:"client_ca_file"`
}

// Enabled reports whether listener-side TLS is configured.
func (t TLS) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

// MTLS reports whether mutual TLS is configured.
func (t TLS) MTLS() bool { return t.ClientCAFile != "" }

// Upstream binds a path prefix to an upstream LLM API and a parser.
type Upstream struct {
	Name     string `yaml:"name"`
	Prefix   string `yaml:"prefix"`
	Target   string `yaml:"target"`
	Provider string `yaml:"provider"`
	// StripPrefix removes Prefix from the path before forwarding, so an
	// inbound /anthropic/v1/messages reaches the upstream as
	// /v1/messages. Leave false when the mount prefix IS the upstream's
	// real base path (the /v1 → api.openai.com case, where the joined
	// path is already correct). httputil's SetURL only ever JOINS
	// paths; without this flag a routing-only prefix like /anthropic
	// is forwarded verbatim and the upstream 404s every request.
	StripPrefix bool `yaml:"strip_prefix"`
	// MaxInFlight caps the concurrent in-flight requests llmspan will
	// forward to this upstream. Above the cap the proxy replies
	// 429 + Retry-After: 1 without contacting upstream. Zero (default)
	// disables the cap. The fuse pairs with the per-upstream circuit
	// breaker — without it, an upstream brownout consumes every
	// goroutine + memory slice the proxy has.
	MaxInFlight int `yaml:"max_in_flight"`
	// PinSHA256 is an optional list of base16-encoded SPKI SHA-256
	// digests that the upstream's leaf certificate must match. When
	// non-empty, the proxy installs a TLS VerifyPeerCertificate
	// callback that rejects any leaf whose SubjectPublicKeyInfo
	// SHA-256 isn't in the set. Empty list (default) leaves stdlib
	// chain verification untouched. Each entry must be exactly 64
	// hex characters (32 bytes).
	PinSHA256 []string `yaml:"pin_sha256"`
	// ResponseHeaderTimeout bounds how long llmspan waits for the
	// upstream's response HEADERS after sending the request. Non-streaming
	// completions (large max_tokens, reasoning models) send no bytes
	// until generation finishes, so this must exceed the slowest
	// legitimate non-streaming call. Zero = default (5m). Streaming
	// responses are unaffected once headers arrive (see
	// http.stream_idle_timeout).
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	// Breaker configures the per-upstream circuit breaker. The
	// breaker trips after Failures consecutive 5xx responses within
	// Window; while open, requests short-circuit to 503 +
	// Retry-After. After RecoveryWindow the breaker enters half-open
	// and admits exactly one probe. Failures=0 disables the breaker.
	Breaker BreakerConfig `yaml:"breaker"`
}

// BreakerConfig is the per-upstream circuit-breaker tuning. Sensible
// production defaults are Failures=5, Window=30s, RecoveryWindow=15s;
// the defaults are not applied automatically because zeroing Failures
// disables the breaker entirely, which is the desired behaviour for
// the unconfigured case.
type BreakerConfig struct {
	// Failures is the count of consecutive 5xx that trips the breaker.
	// Any non-5xx response (including 4xx) resets the counter. Zero
	// disables the breaker.
	Failures int `yaml:"failures"`
	// Window is the time-bound on the consecutive-failure counter. A
	// 5xx older than Window relative to the most recent failure does
	// not contribute. Zero defaults to "no time bound".
	Window time.Duration `yaml:"window"`
	// RecoveryWindow is the time the breaker stays open before
	// allowing one probe request. Zero defaults to Window when set,
	// else to 15 seconds.
	RecoveryWindow time.Duration `yaml:"recovery_window"`
	// TripOnOverloaded makes the non-standard 529 "overloaded" status
	// (Anthropic) count as a failure. Default false: 529 is the
	// provider shedding load, and opening the circuit on it would turn
	// a partial slowdown into a full local outage for every caller.
	TripOnOverloaded bool `yaml:"trip_on_overloaded"`
	// IgnoreStatuses lists additional 5xx codes that never count as
	// failures (e.g. [503] for a provider that uses 503 for capacity
	// shedding). 429, every other 4xx and 499 (client disconnect) are
	// never counted regardless.
	IgnoreStatuses []int `yaml:"ignore_statuses"`
}

// DefaultBreaker is the production-sane breaker applied to the built-in
// upstreams and to YAML upstreams that omit breaker settings.
func DefaultBreaker() BreakerConfig {
	return BreakerConfig{Failures: 5, Window: 30 * time.Second, RecoveryWindow: 15 * time.Second}
}

// DefaultMaxInFlight is the per-upstream concurrency cap applied unless
// the operator sets max_in_flight explicitly (0 = unlimited).
const DefaultMaxInFlight = 256

// DefaultResponseHeaderTimeout is used when an upstream leaves
// response_header_timeout unset.
const DefaultResponseHeaderTimeout = 5 * time.Minute

// Counts reports whether an upstream response with this status code
// should count as a circuit-breaker failure.
func (b BreakerConfig) Counts(status int) bool {
	if status < 500 || status > 599 {
		return false
	}
	if status == 529 && !b.TripOnOverloaded {
		return false
	}
	for _, s := range b.IgnoreStatuses {
		if s == status {
			return false
		}
	}
	return true
}

// Telemetry controls how llmspan exports its OTel signals.
type Telemetry struct {
	Endpoint    string        `yaml:"endpoint"`
	Protocol    string        `yaml:"protocol"`
	Insecure    bool          `yaml:"insecure"`
	Headers     Headers       `yaml:"headers"`
	SampleRatio float64       `yaml:"sample_ratio"`
	Timeout     time.Duration `yaml:"timeout"`
	// Priority sampling records every span in-process, then keeps errors,
	// slow requests, and expensive requests at export time. SampleRatio
	// remains the deterministic baseline for all other spans.
	CostSampleThresholdUSD float64       `yaml:"cost_sample_threshold_usd"`
	SlowSampleThreshold    time.Duration `yaml:"slow_sample_threshold"`
	// AcknowledgeInsecure explicitly opts the operator into plaintext
	// OTLP to a non-loopback / non-RFC1918 endpoint. The default refuses
	// that combination — exporting prompts and trace metadata over the
	// WAN in cleartext is rarely intentional.
	AcknowledgeInsecure bool `yaml:"acknowledge_insecure"`
	// ReadyRequiresExport makes /readyz wait for the first successful OTLP
	// export. Default false: a telemetry outage must never take the LLM
	// path out of rotation, so /readyz only reports draining (SIGTERM).
	ReadyRequiresExport bool `yaml:"ready_requires_export"`
}

// Headers is a typed map so YAML round-trips and we never reach for `any`.
type Headers map[string]string

// ContentCapture controls capture of prompt/completion content. Defaults are
// privacy-preserving: capture metadata only, never content. See OTel GenAI
// semconv: GEN_AI_CAPTURE_MESSAGE_CONTENT.
type ContentCapture struct {
	// Mode is one of "off", "events", "logs". "off" emits only metadata.
	// "events" attaches content as span events. "logs" emits content via
	// the OTel log signal so collectors can scope retention separately.
	Mode string `yaml:"mode"`
	// Redact selects the redaction profile applied to every captured
	// content string before it reaches a span attribute or the
	// otelslog bridge. One of "off", "default", "strict". Default is
	// "default" so an operator who flips Mode to "events" doesn't
	// silently ship raw secrets — the privacy story has to survive a
	// single config edit.
	Redact string `yaml:"redact"`
}

// HTTPTimeouts shapes the listening server's deadlines. The defaults are
// generous because LLM responses can stream for minutes.
type HTTPTimeouts struct {
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
	// ShutdownDrainDelay keeps the listener open for this long after SIGTERM
	// while /readyz already answers 503, so a load balancer or kube-proxy can
	// stop routing before connections are refused. Default 0 (close at once).
	ShutdownDrainDelay time.Duration `yaml:"shutdown_drain_delay"`
	// BodyReadTimeout bounds how long the proxy waits for the inbound
	// request body to arrive. Zero or negative disables the deadline
	// entirely. Default 30 s tolerates legitimately large multimodal
	// uploads while killing slowloris-style holds.
	BodyReadTimeout time.Duration `yaml:"body_read_timeout"`
	// CertReloadInterval is how often the cert manager polls the cert /
	// key files for ModTime changes and reloads them in place. Zero
	// disables hot-reload (one-shot load at boot, restart for rotation).
	// Default 30 s is short enough to catch cert-manager renewals
	// quickly, long enough to be invisible in stat noise.
	CertReloadInterval time.Duration `yaml:"cert_reload_interval"`
	// StreamIdleTimeout is the maximum silence between upstream body
	// bytes after response headers. If a stream stalls longer than
	// this, the proxy closes it so concurrency slots and WaitForStreams
	// cannot wedge forever. Zero disables. Default 5m covers long
	// thinking / tool pauses without letting a hung upstream pin a
	// slot indefinitely.
	StreamIdleTimeout time.Duration `yaml:"stream_idle_timeout"`
}

// Service identifies llmspan to the OTel backend (resource attributes).
type Service struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Env       string `yaml:"environment"`
}

const (
	ProtoGRPC = "grpc"
	ProtoHTTP = "http"

	CaptureOff    = "off"
	CaptureEvents = "events"
	CaptureLogs   = "logs"

	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderGemini    = "gemini"
)

// Default returns a config with sensible defaults for local development.
//
// The default listen address is loopback-only on purpose: the only way to
// expose llmspan on a non-loopback interface is to either configure TLS or
// explicitly set AllowInsecure (to acknowledge that you're carrying API
// credentials over plaintext on whatever network the listener faces).
func Default() Config {
	return Config{
		Listen: "127.0.0.1:4000",
		Upstreams: []Upstream{
			// /v1 doubles as OpenAI's real base path, so the joined
			// forward path is already correct — no strip.
			{Name: "openai", Prefix: "/v1", Target: "https://api.openai.com", Provider: ProviderOpenAI,
				MaxInFlight: DefaultMaxInFlight, Breaker: DefaultBreaker(), ResponseHeaderTimeout: DefaultResponseHeaderTimeout},
			// /anthropic is a routing-only mount: the SDK requests
			// /anthropic/v1/messages and the upstream expects
			// /v1/messages, so the prefix must be stripped.
			{Name: "anthropic", Prefix: "/anthropic", Target: "https://api.anthropic.com", Provider: ProviderAnthropic, StripPrefix: true,
				MaxInFlight: DefaultMaxInFlight, Breaker: DefaultBreaker(), ResponseHeaderTimeout: DefaultResponseHeaderTimeout},
		},
		Telemetry: Telemetry{
			Endpoint:    "localhost:4317",
			Protocol:    ProtoGRPC,
			Insecure:    true,
			SampleRatio: 1.0,
			Timeout:     10 * time.Second,
		},
		Content: ContentCapture{Mode: CaptureOff, Redact: "default"},
		HTTP: HTTPTimeouts{
			ReadHeaderTimeout:  30 * time.Second,
			IdleTimeout:        120 * time.Second,
			ShutdownTimeout:    30 * time.Second,
			BodyReadTimeout:    30 * time.Second,
			CertReloadInterval: 30 * time.Second,
			StreamIdleTimeout:  5 * time.Minute,
		},
		Service: Service{
			Name:      "llmspan",
			Namespace: "observability",
			Env:       "dev",
		},
		Request: Request{
			// 32 MiB headroom for multimodal payloads (vision, audio).
			// Above this, llmspan returns 413 without contacting upstream.
			MaxBodyBytes:      32 * 1024 * 1024,
			MaxBufferedBodies: 512,
		},
	}
}

// Load merges defaults, an optional YAML file, and environment overrides.
// A zero-length path skips the YAML step (useful for tests and demos).
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %q: %w", path, err)
		}
		// KnownFields: a typo'd key (max_inflight, stripprefix) must
		// fail loudly, not silently leave the default in place. YAML
		// config that "loads fine" while ignoring the operator's
		// intent is worse than a crash.
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
		if err := applyUpstreamDefaults(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := resolveAuthTokens(&cfg); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyUpstreamDefaults gives YAML-declared upstreams the same safe
// defaults as the built-in ones (max_in_flight, breaker, response header
// timeout). A YAML `upstreams:` list replaces the default slice wholesale,
// so zero values are indistinguishable from "unset" after decoding; a
// second, non-strict pass over the raw YAML recovers which keys the
// operator actually wrote. An explicit `max_in_flight: 0` or
// `breaker.failures: 0` still disables the feature.
func applyUpstreamDefaults(raw []byte, cfg *Config) error {
	var probe struct {
		Upstreams []struct {
			MaxInFlight *int `yaml:"max_in_flight"`
			Breaker     *struct {
				Failures       *int `yaml:"failures"`
				Window         *any `yaml:"window"`
				RecoveryWindow *any `yaml:"recovery_window"`
			} `yaml:"breaker"`
		} `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return err
	}
	if len(probe.Upstreams) != len(cfg.Upstreams) {
		return nil // upstreams not from YAML (or shape mismatch); leave as decoded
	}
	def := DefaultBreaker()
	for i := range cfg.Upstreams {
		u := &cfg.Upstreams[i]
		p := probe.Upstreams[i]
		if p.MaxInFlight == nil {
			u.MaxInFlight = DefaultMaxInFlight
		}
		if u.ResponseHeaderTimeout == 0 {
			u.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
		}
		switch {
		case p.Breaker == nil || p.Breaker.Failures == nil:
			// Breaker block absent or failures omitted: enabled with
			// defaults, keeping any other knobs the operator set.
			u.Breaker.Failures = def.Failures
			if p.Breaker == nil || p.Breaker.Window == nil {
				u.Breaker.Window = def.Window
			}
			if p.Breaker == nil || p.Breaker.RecoveryWindow == nil {
				u.Breaker.RecoveryWindow = def.RecoveryWindow
			}
		case u.Breaker.Failures > 0:
			if p.Breaker.Window == nil {
				u.Breaker.Window = def.Window
			}
			if p.Breaker.RecoveryWindow == nil {
				u.Breaker.RecoveryWindow = def.RecoveryWindow
			}
		}
	}
	return nil
}

// resolveAuthTokens appends PHC hashes from Auth.TokensFile (and
// LLMSPAN_AUTH_TOKENS_FILE when set via applyEnv) into Auth.Tokens so
// the rest of the binary only ever sees a flat allow-list.
func resolveAuthTokens(cfg *Config) error {
	path := cfg.Auth.TokensFile
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("auth.tokens_file %q: %w", path, err)
	}
	extra, err := parseTokensFile(raw)
	if err != nil {
		return fmt.Errorf("auth.tokens_file %q: %w", path, err)
	}
	cfg.Auth.Tokens = append(cfg.Auth.Tokens, extra...)
	return nil
}

func parseTokensFile(raw []byte) ([]string, error) {
	var out []string
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, hash, err := auth.SplitEntry(line)
		if err != nil || !strings.HasPrefix(hash, "$argon2") {
			return nil, fmt.Errorf("line %d: expected `<argon2 PHC hash>` or `name <hash>`, got %q", i+1, truncateForErr(line, 32))
		}
		if name != "" {
			out = append(out, name+":"+hash)
		} else {
			out = append(out, hash)
		}
	}
	return out, nil
}

func truncateForErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// applyEnv mutates cfg in place from LLMSPAN_* environment variables. Only
// scalar fields are overridable from env; structural changes (upstreams)
// belong in the YAML file or a separate config-management layer.
func applyEnv(cfg *Config) {
	if v, ok := os.LookupEnv("LLMSPAN_LISTEN"); ok {
		cfg.Listen = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_ALLOW_INSECURE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AllowInsecure = b
		} else {
			warnEnv("LLMSPAN_ALLOW_INSECURE", v, "not a boolean; keeping previous value")
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_ALLOW_UNAUTHENTICATED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AllowUnauthenticated = b
		} else {
			warnEnv("LLMSPAN_ALLOW_UNAUTHENTICATED", v, "not a boolean; keeping previous value")
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_ADMIN_LISTEN"); ok {
		cfg.Admin.Listen = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_TLS_CERT_FILE"); ok {
		cfg.TLS.CertFile = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_TLS_KEY_FILE"); ok {
		cfg.TLS.KeyFile = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_TLS_CLIENT_CA_FILE"); ok {
		cfg.TLS.ClientCAFile = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_OTLP_ENDPOINT"); ok {
		cfg.Telemetry.Endpoint = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_OTLP_PROTOCOL"); ok {
		cfg.Telemetry.Protocol = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_OTLP_INSECURE"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Telemetry.Insecure = b
		} else {
			warnEnv("LLMSPAN_OTLP_INSECURE", v, "not a boolean; keeping previous value")
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_OTLP_ACKNOWLEDGE_INSECURE"); ok {
		// Needed for env-only deployments (containers) whose collector
		// endpoint is a non-"localhost" hostname: validation can't
		// know a docker-network alias is private, so the operator
		// acknowledges plaintext explicitly.
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Telemetry.AcknowledgeInsecure = b
		} else {
			warnEnv("LLMSPAN_OTLP_ACKNOWLEDGE_INSECURE", v, "not a boolean; keeping previous value")
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_OTLP_HEADERS"); ok {
		// Format: "k1=v1,k2=v2". Env overrides YAML wholesale —
		// callers who want to merge should compose at the YAML
		// layer. Trims surrounding whitespace per-pair so the env
		// can be hand-pasted from a shell heredoc.
		headers := Headers{}
		for _, pair := range strings.Split(v, ",") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			k, val, ok := strings.Cut(pair, "=")
			if !ok {
				// Env never crashes the process, but a dropped auth
				// header (typo'd X-Scope-OrgID) silently sends
				// telemetry to the wrong tenant — say something.
				warnEnv("LLMSPAN_OTLP_HEADERS", pair, "malformed pair (want k=v); skipping")
				continue
			}
			headers[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
		if len(headers) > 0 {
			cfg.Telemetry.Headers = headers
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_SAMPLE_RATIO"); ok {
		if f, err := strconv.ParseFloat(v, 64); err != nil {
			warnEnv("LLMSPAN_SAMPLE_RATIO", v, "not a number; keeping previous value")
		} else {
			// Sampling is not a correctness knob; out-of-range
			// values clamp rather than refuse startup. Validate's
			// stricter check is intentionally bypassed here because
			// env vars often come from string-typed orchestrators
			// (helm values, terraform vars) where a typo shouldn't
			// page someone at 3am.
			if f < 0 {
				fmt.Fprintf(os.Stderr, "config: LLMSPAN_SAMPLE_RATIO %v < 0, clamping to 0\n", f)
				f = 0
			} else if f > 1 {
				fmt.Fprintf(os.Stderr, "config: LLMSPAN_SAMPLE_RATIO %v > 1, clamping to 1\n", f)
				f = 1
			}
			cfg.Telemetry.SampleRatio = f
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_CAPTURE"); ok {
		cfg.Content.Mode = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_SERVICE_NAME"); ok {
		cfg.Service.Name = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_SERVICE_NAMESPACE"); ok {
		cfg.Service.Namespace = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_ENV"); ok {
		cfg.Service.Env = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_AUTH_TOKENS_FILE"); ok {
		cfg.Auth.TokensFile = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_ATTRIBUTION_HEADER"); ok {
		cfg.Attribution.Header = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_CONVERSATION_HEADER"); ok {
		cfg.Attribution.ConversationHeader = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_GRAFANA_AGENT_O11Y"); ok {
		cfg.Attribution.GrafanaAgentO11y = v == "true" || v == "1"
	}
	if v, ok := os.LookupEnv("LLMSPAN_RECORDING_PATH"); ok {
		cfg.Recording.Path = v
	}
	if v, ok := os.LookupEnv("LLMSPAN_COST_SAMPLE_THRESHOLD_USD"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Telemetry.CostSampleThresholdUSD = f
		} else {
			warnEnv("LLMSPAN_COST_SAMPLE_THRESHOLD_USD", v, "not a number; keeping previous value")
		}
	}
	if v, ok := os.LookupEnv("LLMSPAN_SLOW_SAMPLE_THRESHOLD"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Telemetry.SlowSampleThreshold = d
		} else {
			warnEnv("LLMSPAN_SLOW_SAMPLE_THRESHOLD", v, "not a duration; keeping previous value")
		}
	}
}

// warnEnv reports an environment value the loader could not honour.
// Env overrides never crash the process (they often come from
// string-typed orchestrators), but silently keeping the previous value
// while the operator believes the env "took" is a debugging trap —
// matching the LLMSPAN_SAMPLE_RATIO clamping precedent, we say so on
// stderr.
func warnEnv(name, value, reason string) {
	fmt.Fprintf(os.Stderr, "config: %s=%q %s\n", name, value, reason)
}

// Validate reports the first violation it finds. The caller surfaces it to
// the user; we never start serving with an invalid config.
func (c *Config) Validate() error {
	var errs []error

	if strings.TrimSpace(c.Listen) == "" {
		errs = append(errs, errors.New("listen: must not be empty"))
	}

	// Listener-side TLS sanity. Two rules:
	//   1. If either cert_file or key_file is set, both must be.
	//   2. Refuse to listen on a non-loopback address without TLS unless the
	//      operator has explicitly opted in via allow_insecure.
	switch {
	case c.TLS.CertFile != "" && c.TLS.KeyFile == "":
		errs = append(errs, errors.New("tls.key_file: required when cert_file is set"))
	case c.TLS.KeyFile != "" && c.TLS.CertFile == "":
		errs = append(errs, errors.New("tls.cert_file: required when key_file is set"))
	}
	if c.TLS.MTLS() && !c.TLS.Enabled() {
		errs = append(errs, errors.New("tls.client_ca_file: requires tls.cert_file/key_file (mTLS implies TLS)"))
	}
	if !c.TLS.Enabled() && !c.AllowInsecure && !isLoopbackAddr(c.Listen) {
		errs = append(errs, fmt.Errorf(
			"listen %q is non-loopback but TLS is not configured: set tls.cert_file/key_file, or bind to 127.0.0.1, or set allow_insecure=true to acknowledge plaintext on this interface",
			c.Listen,
		))
	}

	// Listener auth. TLS protects the wire, not access: a reachable
	// non-loopback port with no bearer tokens and no mTLS lets any peer
	// spend the operator's upstream API quota. Require an explicit opt-in.
	if !isLoopbackAddr(c.Listen) && !c.Auth.Enabled() && !c.TLS.MTLS() && !c.AllowUnauthenticated {
		errs = append(errs, fmt.Errorf(
			"listen %q is non-loopback but no listener authentication is configured: set auth.tokens / auth.tokens_file (generate with `llmspan hash-token`), or tls.client_ca_file for mTLS, or bind to 127.0.0.1, or set allow_unauthenticated=true (LLMSPAN_ALLOW_UNAUTHENTICATED=true) to acknowledge that anyone who can reach this port can use your upstream API keys' quota",
			c.Listen,
		))
	}
	for i, t := range c.Auth.Tokens {
		if _, hash, err := auth.SplitEntry(t); err != nil {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: %w", i, err))
		} else if !strings.HasPrefix(hash, "$argon2id$") {
			errs = append(errs, fmt.Errorf("auth.tokens[%d]: expected an argon2id PHC hash (use `llmspan hash-token`)", i))
		}
	}
	if c.Admin.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Admin.Listen); err != nil {
			errs = append(errs, fmt.Errorf("admin.listen %q: %w", c.Admin.Listen, err))
		} else if !c.Admin.AllowNonLoopback && !isLoopbackAddr(c.Admin.Listen) {
			errs = append(errs, fmt.Errorf("admin.listen %q is non-loopback: bind 127.0.0.1 or set admin.allow_non_loopback=true (the admin listener is plaintext and unauthenticated; restrict it with a NetworkPolicy)", c.Admin.Listen))
		}
		if c.Admin.Listen == c.Listen {
			errs = append(errs, errors.New("admin.listen must differ from listen"))
		}
	}
	if c.Request.MaxBufferedBodies < 0 {
		errs = append(errs, fmt.Errorf("request.max_buffered_bodies %d: must be >= 0", c.Request.MaxBufferedBodies))
	}

	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("upstreams: at least one upstream required"))
	}
	seenPrefix := map[string]string{}
	for i, u := range c.Upstreams {
		if u.Name == "" {
			errs = append(errs, fmt.Errorf("upstreams[%d].name: required", i))
		}
		if !strings.HasPrefix(u.Prefix, "/") {
			errs = append(errs, fmt.Errorf("upstreams[%d].prefix: must start with '/'", i))
		}
		// /healthz and /readyz are reserved by the proxy for
		// unauthenticated probe endpoints. An upstream prefix that
		// collides (exact or as a sub-path) would mask the probes
		// or — worse — let an attacker prepend a probe URL to
		// re-route an authenticated request. Reject at config-load.
		if isReservedPrefix(u.Prefix) {
			errs = append(errs, fmt.Errorf("upstreams[%d].prefix %q: collides with reserved health endpoint", i, u.Prefix))
		}
		if other, dup := seenPrefix[u.Prefix]; dup {
			errs = append(errs, fmt.Errorf("upstreams[%d].prefix %q: duplicate of upstream %q", i, u.Prefix, other))
		} else {
			seenPrefix[u.Prefix] = u.Name
		}
		// url.Parse accepts nearly anything ("api.openai.com" parses
		// as a relative path with no host, and the proxy would then
		// dial ""), so require an absolute http(s) URL with a host.
		switch tu, err := url.Parse(u.Target); {
		case err != nil || u.Target == "":
			errs = append(errs, fmt.Errorf("upstreams[%d].target %q: invalid URL", i, u.Target))
		case tu.Scheme != "http" && tu.Scheme != "https":
			errs = append(errs, fmt.Errorf("upstreams[%d].target %q: scheme must be http or https (did you forget \"https://\"?)", i, u.Target))
		case tu.Host == "":
			errs = append(errs, fmt.Errorf("upstreams[%d].target %q: missing host", i, u.Target))
		}
		if !provider.Known(u.Provider) {
			errs = append(errs, fmt.Errorf("upstreams[%d].provider %q: unsupported (want %s)",
				i, u.Provider, strings.Join(provider.KnownNames(), "|")))
		}
		for j, p := range u.PinSHA256 {
			if !isHex64(p) {
				errs = append(errs, fmt.Errorf("upstreams[%d].pin_sha256[%d]: must be 64 hex chars (32-byte SHA-256), got %q", i, j, p))
			}
		}
		if u.ResponseHeaderTimeout < 0 {
			errs = append(errs, fmt.Errorf("upstreams[%d].response_header_timeout %v: must be >= 0 (0 = default %v)", i, u.ResponseHeaderTimeout, DefaultResponseHeaderTimeout))
		}
		if u.MaxInFlight < 0 {
			errs = append(errs, fmt.Errorf("upstreams[%d].max_in_flight %d: must be >= 0", i, u.MaxInFlight))
		}
		for _, st := range u.Breaker.IgnoreStatuses {
			if st < 500 || st > 599 {
				errs = append(errs, fmt.Errorf("upstreams[%d].breaker.ignore_statuses: %d is not a 5xx status (4xx never counts anyway)", i, st))
			}
		}
		if u.Breaker.Failures < 0 {
			errs = append(errs, fmt.Errorf("upstreams[%d].breaker.failures %d: must be >= 0", i, u.Breaker.Failures))
		}
		if u.Breaker.Window < 0 {
			errs = append(errs, fmt.Errorf("upstreams[%d].breaker.window %v: must be >= 0", i, u.Breaker.Window))
		}
		if u.Breaker.RecoveryWindow < 0 {
			errs = append(errs, fmt.Errorf("upstreams[%d].breaker.recovery_window %v: must be >= 0", i, u.Breaker.RecoveryWindow))
		}
	}

	switch c.Telemetry.Protocol {
	case ProtoGRPC, ProtoHTTP:
	default:
		errs = append(errs, fmt.Errorf("telemetry.protocol %q: must be grpc|http", c.Telemetry.Protocol))
	}
	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		errs = append(errs, fmt.Errorf("telemetry.sample_ratio %v: must be in [0,1]", c.Telemetry.SampleRatio))
	}
	if c.Telemetry.CostSampleThresholdUSD < 0 {
		errs = append(errs, fmt.Errorf("telemetry.cost_sample_threshold_usd %v: must be >= 0", c.Telemetry.CostSampleThresholdUSD))
	}
	if c.Telemetry.SlowSampleThreshold < 0 {
		errs = append(errs, fmt.Errorf("telemetry.slow_sample_threshold %v: must be >= 0", c.Telemetry.SlowSampleThreshold))
	}
	if c.Attribution.MaxCardinality < 0 {
		errs = append(errs, fmt.Errorf("attribution.max_cardinality %d: must be >= 0", c.Attribution.MaxCardinality))
	}
	if h := c.Attribution.ConversationHeader; h != "" {
		if strings.EqualFold(h, "Authorization") || strings.EqualFold(h, c.Attribution.Header) ||
			(c.Auth.Enabled() && strings.EqualFold(h, c.Auth.HeaderName())) {
			errs = append(errs, fmt.Errorf("attribution.conversation_header %q: must not collide with the authorization, auth, or caller headers", h))
		}
	}
	if c.Attribution.Header != "" {
		if strings.EqualFold(c.Attribution.Header, "Authorization") {
			errs = append(errs, fmt.Errorf("attribution.header must not be Authorization (it is consumed before forwarding)"))
		}
		if c.Auth.Enabled() && strings.EqualFold(c.Attribution.Header, c.Auth.HeaderName()) {
			errs = append(errs, fmt.Errorf("attribution.header must differ from auth.header"))
		}
	}
	if strings.Contains(c.Telemetry.Endpoint, "://") && c.Telemetry.Protocol == ProtoGRPC {
		errs = append(errs, fmt.Errorf("telemetry.endpoint %q: URL form (scheme/path) is only supported with protocol http; use host:port for grpc", c.Telemetry.Endpoint))
	}
	plaintextOTLP := c.Telemetry.Insecure
	if strings.Contains(c.Telemetry.Endpoint, "://") { // the URL scheme decides, insecure is ignored
		plaintextOTLP = strings.HasPrefix(strings.ToLower(c.Telemetry.Endpoint), "http://")
	}
	if plaintextOTLP && !c.Telemetry.AcknowledgeInsecure && !isLocalEndpoint(c.Telemetry.Endpoint) {
		errs = append(errs, fmt.Errorf(
			"telemetry.endpoint %q is non-local but telemetry.insecure=true: configure TLS at the collector and set telemetry.insecure=false, or set telemetry.acknowledge_insecure=true to acknowledge plaintext OTLP off-host",
			c.Telemetry.Endpoint,
		))
	}

	switch c.Content.Mode {
	case CaptureOff, CaptureEvents, CaptureLogs, "":
		if c.Content.Mode == "" {
			c.Content.Mode = CaptureOff
		}
	default:
		errs = append(errs, fmt.Errorf("content.mode %q: must be off|events|logs", c.Content.Mode))
	}
	switch c.Content.Redact {
	case "off", "default", "strict", "":
		if c.Content.Redact == "" {
			c.Content.Redact = "default"
		}
	default:
		errs = append(errs, fmt.Errorf("content.redact %q: must be off|default|strict", c.Content.Redact))
	}

	return errors.Join(errs...)
}

// isLocalEndpoint reports whether the OTLP endpoint resolves to a
// host that is process-local, loopback, or in RFC1918 / RFC4193
// private space. Plaintext OTLP to such hosts is operationally
// reasonable; plaintext to anywhere else is a foot-gun.
//
// Hostnames other than "localhost" are conservatively treated as
// non-local — we don't want to resolve DNS at config-validate time.
func isLocalEndpoint(endpoint string) bool {
	s := endpoint
	if idx := strings.Index(s, "://"); idx >= 0 {
		s = s[idx+3:]
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		s = s[:idx]
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// isLoopbackAddr reports whether the configured listen address binds to a
// loopback interface only. The bare ":port" form binds to all interfaces and
// is treated as non-loopback. Hostnames other than "localhost" are also
// treated as non-loopback because we can't resolve them safely at config
// time without surprising DNS dependencies.
func isLoopbackAddr(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// isReservedPrefix reports whether u.Prefix collides with one of the
// proxy-owned probe endpoints. The collision rule is "exact match or
// reserved is a prefix of u" — operators can still mount, e.g.,
// /healthz-internal, but /healthz alone or /healthz/sub is rejected.
func isReservedPrefix(prefix string) bool {
	reserved := []string{"/healthz", "/readyz"}
	for _, r := range reserved {
		if prefix == r {
			return true
		}
		if strings.HasPrefix(prefix, r+"/") {
			return true
		}
	}
	return false
}

// isHex64 reports whether s is exactly 64 characters and every
// character is a valid hex digit. Used to validate operator-supplied
// SPKI sha256 pin hashes.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Match returns the upstream whose Prefix is the longest match of urlPath, or
// (Upstream{}, false) if none. Longest-prefix-match keeps routing predictable
// when prefixes nest (e.g. "/v1" and "/v1/embeddings").
//
// Matching is segment-aware: prefix "/v1" matches "/v1" and "/v1/chat"
// but NOT "/v1foo". Raw byte-prefix matching routed sibling paths into
// the wrong upstream — a parser differential between routing (which
// saw "/v1foo" as openai) and enrichment (which is segment-strict).
func (c *Config) Match(urlPath string) (Upstream, bool) {
	var best Upstream
	bestLen := -1
	for _, u := range c.Upstreams {
		if !prefixMatchesSegment(urlPath, u.Prefix) {
			continue
		}
		if len(u.Prefix) > bestLen {
			best = u
			bestLen = len(u.Prefix)
		}
	}
	return best, bestLen >= 0
}

// prefixMatchesSegment reports whether p falls under prefix at a path
// segment boundary: exact match, or prefix followed by "/".
func prefixMatchesSegment(p, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(p, "/")
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}
