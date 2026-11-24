package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func attrOf(t *testing.T, attrs []attribute.KeyValue, key string) (string, bool) {
	t.Helper()
	for _, a := range attrs {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

func TestContentSpecAttributesRedacted(t *testing.T) {
	t.Setenv("LLMSPAN_LEGACY_GENAI_EVENTS", "")
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	opts := ContentOpts{CaptureEvents: true, Redact: func(s string) string {
		return strings.ReplaceAll(s, "SECRET", "[redacted]")
	}}
	body := []byte(`{"model":"claude-x","system":"be SECRET nice","max_tokens":5,"messages":[{"role":"user","content":"hi SECRET"}]}`)
	Anthropic{}.ParseRequest(span, "/v1/messages", body, opts)
	Anthropic{}.ParseResponseJSON(span, &Info{}, []byte(`{"id":"m","model":"claude-x","stop_reason":"end_turn","content":[{"type":"text","text":"ok SECRET"}]}`), opts)
	span.End()
	s := rec.Ended()[0]
	if len(s.Events()) != 0 {
		t.Fatalf("legacy events emitted without opt-in: %v", s.Events())
	}
	in, ok := attrOf(t, s.Attributes(), "gen_ai.input.messages")
	if !ok {
		t.Fatal("no gen_ai.input.messages")
	}
	var msgs []struct {
		Role  string
		Parts []struct{ Type, Content string }
	}
	if err := json.Unmarshal([]byte(in), &msgs); err != nil || len(msgs) != 1 || msgs[0].Role != "user" ||
		msgs[0].Parts[0].Type != "text" || msgs[0].Parts[0].Content != "hi [redacted]" {
		t.Fatalf("input = %s (%v)", in, err)
	}
	sys, _ := attrOf(t, s.Attributes(), "gen_ai.system_instructions")
	if !strings.Contains(sys, "be [redacted] nice") || strings.Contains(sys, "SECRET") {
		t.Fatalf("system = %s", sys)
	}
	out, _ := attrOf(t, s.Attributes(), "gen_ai.output.messages")
	if !strings.Contains(out, `"role":"assistant"`) || strings.Contains(out, "SECRET") {
		t.Fatalf("output = %s", out)
	}
}

func TestContentOffEmitsNothing(t *testing.T) {
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	OpenAI{}.ParseRequest(span, "/v1/chat/completions",
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), ContentOpts{})
	span.End()
	if _, ok := attrOf(t, rec.Ended()[0].Attributes(), "gen_ai.input.messages"); ok {
		t.Fatal("content captured while off")
	}
}

func TestLegacyEventsOptIn(t *testing.T) {
	t.Setenv("LLMSPAN_LEGACY_GENAI_EVENTS", "1")
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	OpenAI{}.ParseRequest(span, "/v1/chat/completions",
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), ContentOpts{CaptureEvents: true})
	span.End()
	s := rec.Ended()[0]
	if len(s.Events()) != 1 || s.Events()[0].Name != "gen_ai.user.message" {
		t.Fatalf("events = %v", s.Events())
	}
	if _, ok := attrOf(t, s.Attributes(), "gen_ai.input.messages"); !ok {
		t.Fatal("new-form attribute must be emitted alongside legacy events")
	}
}

func TestGeminiProviderNames(t *testing.T) {
	t.Parallel()
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	if got := (Gemini{}).ParseRequest(span, "/v1beta/models/gemini-2.5-pro:generateContent", []byte(`{}`), ContentOpts{}).System; got != "gcp.gemini" {
		t.Fatalf("ai studio = %q", got)
	}
	if got := (Gemini{}).ParseRequest(span, "/v1/projects/p/locations/us/publishers/google/models/gemini-2.5-pro:generateContent", []byte(`{}`), ContentOpts{}).System; got != "gcp.vertex_ai" {
		t.Fatalf("vertex = %q", got)
	}
}

func TestSpanNameIncludesModel(t *testing.T) {
	t.Parallel()
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "chat")
	OpenAI{}.ParseRequest(span, "/v1/chat/completions", []byte(`{"model":"gpt-4o"}`), ContentOpts{})
	span.End()
	if got := rec.Ended()[0].Name(); got != "chat gpt-4o" {
		t.Fatalf("name = %q", got)
	}
}
