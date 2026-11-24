package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http/httptest"
	"testing"

	"github.com/llmspan/llmspan/internal/config"
)

func TestCallerForRequestPriority(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{Attribution: config.Attribution{Header: "X-Caller"}}}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Caller", "team-search")
	req.TLS = &tls.ConnectionState{}
	req.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "service-a"}}}
	if got := h.callerForRequest(req, authIdentity{}); got != "team-search" {
		t.Fatalf("caller = %q", got)
	}
	if got := req.Header.Get("X-Caller"); got != "" {
		t.Fatalf("attribution header forwarded: %q", got)
	}
}

func TestCallerForRequestMTLS(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{}}
	req := httptest.NewRequest("POST", "/", nil)
	req.TLS = &tls.ConnectionState{}
	req.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "service-a"}}}
	if got := h.callerForRequest(req, authIdentity{}); got != "mtls:service-a" {
		t.Fatalf("caller = %q", got)
	}
}

func TestCallerForRequestAuthFingerprint(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{}}
	req := httptest.NewRequest("POST", "/", nil)
	got := h.callerForRequest(req, authIdentity{fingerprint: "token:0123456789abcdef"})
	if got != "token:0123456789abcdef" {
		t.Fatalf("fingerprint caller = %q", got)
	}
}

// A named token wins over a client-supplied attribution header (which is
// still stripped), so X-Caller cannot spoof an authenticated identity.
func TestCallerForRequestNamedTokenBeatsHeader(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{Attribution: config.Attribution{Header: "X-Caller"}}}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Caller", "someone-else")
	got := h.callerForRequest(req, authIdentity{name: "billing", fingerprint: "token:abcd"})
	if got != "billing" {
		t.Fatalf("caller = %q, want billing", got)
	}
	if req.Header.Get("X-Caller") != "" {
		t.Fatal("attribution header forwarded")
	}
	// Anonymous token: header still wins over fingerprint.
	req.Header.Set("X-Caller", "team-x")
	if got := h.callerForRequest(req, authIdentity{fingerprint: "token:abcd"}); got != "team-x" {
		t.Fatalf("caller = %q", got)
	}
}

func TestConversationForRequest(t *testing.T) {
	t.Parallel()
	h := &Handler{cfg: config.Config{Attribution: config.Attribution{ConversationHeader: "X-Conversation-Id"}}}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Conversation-Id", " conv-123 ")
	if got := h.conversationForRequest(req); got != "conv-123" {
		t.Fatalf("conversation = %q", got)
	}
	if req.Header.Get("X-Conversation-Id") != "" {
		t.Fatal("conversation header forwarded upstream")
	}
	off := &Handler{cfg: config.Config{}}
	req.Header.Set("X-Conversation-Id", "x")
	if got := off.conversationForRequest(req); got != "" {
		t.Fatalf("disabled but got %q", got)
	}
}
