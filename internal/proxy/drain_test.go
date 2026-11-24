package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/proxy"
	"github.com/llmspan/llmspan/internal/telemetry"
)

// After SIGTERM the proxy must answer /readyz with 503 while the listener is
// still open (http.shutdown_drain_delay), and only then refuse connections.
func TestShutdownDrainDelayServes503ThenCloses(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfg := config.Default()
	cfg.Listen = addr
	cfg.Upstreams = []config.Upstream{{Name: "openai", Prefix: "/v1", Target: "http://127.0.0.1:1", Provider: "openai"}}
	cfg.HTTP.ShutdownTimeout = 5 * time.Second
	cfg.HTTP.ShutdownDrainDelay = 800 * time.Millisecond

	gate := telemetry.NewGate(false)
	prov := fakeProvidersForShutdown(t)
	prov.Ready = gate.Ready
	prov.BeginDrain = gate.BeginDrain

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := proxy.New(cfg, provider.BuiltIn(), prov, logger)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := proxy.NewServer(cfg, handler, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	get := func() (int, error) {
		c := &http.Client{Timeout: time.Second}
		resp, gerr := c.Get("http://" + addr + "/readyz")
		if gerr != nil {
			return 0, gerr
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if code, gerr := get(); gerr == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel() // SIGTERM equivalent
	time.Sleep(150 * time.Millisecond)
	code, gerr := get()
	if gerr != nil {
		t.Fatalf("listener closed during the drain delay: %v", gerr)
	}
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during drain = %d, want 503", code)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the drain delay")
	}
	if _, gerr = get(); gerr == nil {
		t.Fatal("listener still accepting after shutdown")
	}
}
