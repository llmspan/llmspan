package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteProxyJSONErrorBodyAndHeaders(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Header.Set(headerRequestID, "client-req-1")

	writeProxyJSONError(rec, req, http.StatusBadGateway, "upstream request failed", "bad_gateway")

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(headerRequestID); got != "client-req-1" {
		t.Fatalf("X-Request-Id = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var body proxyErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "llmspan_proxy_error" || body.Error.Code != "bad_gateway" {
		t.Fatalf("body = %+v", body.Error)
	}
	if body.Error.Message != "upstream request failed" {
		t.Fatalf("message = %q", body.Error.Message)
	}
}

func TestWriteProxyJSONErrorSetsRequestID(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	writeProxyJSONError(rec, req, http.StatusUnauthorized, "unauthorized", "unauthorized")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get(headerRequestID); got == "" {
		t.Fatal("expected generated X-Request-Id")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("body = %q", rec.Body.String())
	}
}
