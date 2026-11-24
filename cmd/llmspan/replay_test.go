package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A recording naming another host must never receive the replay credential.
func TestReplayRejectsForeignHost(t *testing.T) {
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer evil.Close()
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	t.Setenv("LLMSPAN_REPLAY_AUTHORIZATION", "Bearer secret")

	for _, p := range []string{evil.URL + "/x", "//" + strings.TrimPrefix(evil.URL, "http://") + "/x", "v1/chat"} {
		dir := t.TempDir()
		f := filepath.Join(dir, "rec.jsonl")
		line := `{"method":"POST","path":"` + p + `","request":"{}"}` + "\n"
		if err := os.WriteFile(f, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		err := runReplay([]string{"--target", target.URL, "--out", filepath.Join(dir, "o.jsonl"), f}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("path %q accepted", p)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests escaped validation", hits.Load())
	}
}
