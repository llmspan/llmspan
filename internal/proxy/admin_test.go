package proxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The admin listener serves /metrics on its own socket; the proxy
// listener must NOT serve it (it would be routed to an upstream/404).
func TestAdminListenerServesMetricsSeparately(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Listen = freeAddr(t)
	cfg.Admin.Listen = freeAddr(t)
	prov, _, _ := realMeterProviders(t)
	h, err := proxy.New(cfg, provider.BuiltIn(), prov, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := proxy.NewServer(cfg, h, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, getErr := http.Get("http://" + cfg.Admin.Listen + "/metrics")
		if getErr == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "# TYPE llmspan_") && !strings.Contains(body, "llmspan_") {
		// Fresh process: only the breaker gauge is seeded.
		t.Fatalf("admin /metrics body unexpected: %q", body)
	}
	resp, err := http.Get("http://" + cfg.Listen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("proxy listener must not expose /metrics")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}
