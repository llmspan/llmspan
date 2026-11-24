package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/llmspan/llmspan/internal/auth"
)

// TestHashTokenCommandRoundtrip pipes a plaintext token into runHashToken
// and round-trips the printed PHC hash through auth.Verify. The CLI is
// the only ergonomic way to produce a hash for the config file, so it
// must actually produce something the runtime accepts.
func TestHashTokenCommandRoundtrip(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runHashToken(nil, strings.NewReader("my-real-token\n"), &stdout, &stderr); err != nil {
		t.Fatalf("runHashToken: %v", err)
	}
	encoded := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("output not a PHC hash: %q", encoded)
	}
	ok, err := auth.Verify("my-real-token", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("freshly emitted hash failed to verify its own plaintext")
	}
}

func TestHashTokenCommandRefusesEmptyStdin(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runHashToken(nil, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Error("expected error on empty stdin")
	}
}

func TestHashTokenNameFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if err := runHashToken([]string{"--name", "billing"}, strings.NewReader("tok\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(stdout.String())
	name, hash, err := auth.SplitEntry(out)
	if err != nil || name != "billing" {
		t.Fatalf("output %q -> name=%q err=%v", out, name, err)
	}
	if ok, err := auth.Verify("tok", hash); err != nil || !ok {
		t.Fatalf("hash does not verify: %v %v", ok, err)
	}
	if err := runHashToken([]string{"--name", "bad name"}, strings.NewReader("tok\n"), &stdout, &stderr); err == nil {
		t.Error("invalid name must be rejected")
	}
}
