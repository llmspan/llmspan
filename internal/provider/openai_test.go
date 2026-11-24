package provider

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newRecorderTracer() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	return rec, tp
}

func TestOpenAIParseRequest(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model":"gpt-4o-mini",
		"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"temperature":0.2,
		"max_tokens":64,
		"stream":true,
		"stop":["END"]
	}`)
	rec, tp := newRecorderTracer()
	tr := tp.Tracer("t")
	_, span := tr.Start(context.Background(), "init")

	info := OpenAI{}.ParseRequest(span, "/v1/chat/completions", body, ContentOpts{})
	span.End()

	if info.Operation != "chat" {
		t.Errorf("op = %q", info.Operation)
	}
	if info.RequestModel != "gpt-4o-mini" {
		t.Errorf("model = %q", info.RequestModel)
	}
	if !info.Stream {
		t.Errorf("stream = false")
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	got := spans[0]
	if got.Name() != "chat gpt-4o-mini" {
		t.Errorf("span name = %q", got.Name())
	}
	mustHaveAttr(t, got.Attributes(), "gen_ai.provider.name", "openai")
	mustHaveAttr(t, got.Attributes(), "gen_ai.request.model", "gpt-4o-mini")
}

func TestOpenAIParseResponseJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"id":"chatcmpl-1",
		"model":"gpt-4o-mini-2024-07-18",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}
	}`)
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat"}

	OpenAI{}.ParseResponseJSON(span, info, body, ContentOpts{})
	span.End()

	if info.InputTokens != 10 || info.OutputTokens != 3 {
		t.Errorf("tokens = %d/%d", info.InputTokens, info.OutputTokens)
	}
	if info.ResponseModel != "gpt-4o-mini-2024-07-18" {
		t.Errorf("response model = %q", info.ResponseModel)
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", info.FinishReasons)
	}

	spans := rec.Ended()
	mustHaveAttrInt64(t, spans[0].Attributes(), "gen_ai.usage.input_tokens", 10)
	mustHaveAttrInt64(t, spans[0].Attributes(), "gen_ai.usage.output_tokens", 3)
}

func TestOpenAIWrapStreamFinishesOnce(t *testing.T) {
	t.Parallel()

	chunks := []map[string]any{
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}},
		}},
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"content": " world"}, "finish_reason": "stop"},
		}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2}},
	}

	var b strings.Builder
	for _, c := range chunks {
		buf, _ := json.Marshal(c)
		b.WriteString("data: ")
		b.Write(buf)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")

	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat", RequestModel: "gpt-4o-mini"}

	done := 0
	stream := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(b.String())), ContentOpts{},
		func() { done++ },
	)

	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Errorf("forwarded body lost [DONE]")
	}
	_ = stream.Close()

	if info.FirstTokenAt.IsZero() {
		t.Errorf("FirstTokenAt is zero on text stream; TTFT metric is silently broken")
	}
	if done != 1 {
		t.Errorf("done called %d times (want exactly 1)", done)
	}
	if info.OutputTokens != 2 || info.InputTokens != 5 {
		t.Errorf("usage merged from final chunk: in=%d out=%d", info.InputTokens, info.OutputTokens)
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "stop" {
		t.Errorf("finish reasons = %v", info.FinishReasons)
	}
}

// TestOpenAIWrapStreamToolOnlyStreamFiresFirstToken is a regression test:
// regression for OpenAI. A streaming chat where the model decides to
// emit a tool call rather than text never populates `delta.content`,
// so the existing TTFT trigger (`delta.content != ""`) never fires.
// Result: tool-only streams record FirstTokenAt = zero — every
// downstream metric / span that depends on TTFT silently undercounts
// real tool usage.
//
// The fix: fire on EITHER text content OR a non-empty tool_calls slice.
func TestOpenAIWrapStreamToolOnlyStreamFiresFirstToken(t *testing.T) {
	t.Parallel()

	// Mirror real OpenAI streaming: the model emits tool_calls deltas
	// (no `content` at all), then a chunk with finish_reason=tool_calls.
	chunks := []map[string]any{
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": "get_weather", "arguments": ""},
				}},
			}},
		}},
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": `{"city":"`},
				}},
			}},
		}},
		{"id": "c", "model": "gpt-4o-mini", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": `Paris"}`},
				}},
			}, "finish_reason": "tool_calls"},
		}},
	}

	var b strings.Builder
	for _, c := range chunks {
		buf, _ := json.Marshal(c)
		b.WriteString("data: ")
		b.Write(buf)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")

	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat", RequestModel: "gpt-4o-mini"}

	done := 0
	stream := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(b.String())), ContentOpts{},
		func() { done++ },
	)
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()

	if done != 1 {
		t.Errorf("done called %d times, want exactly 1", done)
	}
	if info.FirstTokenAt.IsZero() {
		t.Errorf("FirstTokenAt is zero on tool-only stream; TTFT metric is silently broken")
	}
	if len(info.FinishReasons) != 1 || info.FinishReasons[0] != "tool_calls" {
		t.Errorf("finish reasons = %v, want [tool_calls]", info.FinishReasons)
	}
}

// Chat Completions reports cache reads and reasoning under prompt_/completion_
// tokens_details (the Responses API uses input_/output_tokens_details). Both
// spellings must be honoured or cache discounts are silently lost.
func TestOpenAIChatCompletionsCacheAndReasoningTokens(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"id":"chatcmpl-2","model":"gpt-5-mini",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],
		"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,
		  "prompt_tokens_details":{"cached_tokens":64},
		  "completion_tokens_details":{"reasoning_tokens":30}}
	}`)
	_, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "init")
	info := &Info{System: "openai", Operation: "chat"}
	OpenAI{}.ParseResponseJSON(span, info, body, ContentOpts{})
	span.End()

	if info.CacheReadInputTokens != 64 {
		t.Errorf("cache read = %d, want 64", info.CacheReadInputTokens)
	}
	if info.ReasoningOutputTokens != 30 {
		t.Errorf("reasoning = %d, want 30", info.ReasoningOutputTokens)
	}
}

func TestOpenAIStreamingUsageCacheTokens(t *testing.T) {
	t.Parallel()

	var info Info
	mergeOpenAIUsage(&info, openAIUsage{PromptTokens: 27, CompletionTokens: 10})
	if info.CacheReadInputTokens != 0 {
		t.Fatalf("no details => 0, got %d", info.CacheReadInputTokens)
	}
	var u openAIUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":27,"prompt_tokens_details":{"cached_tokens":20},"completion_tokens":10}`), &u); err != nil {
		t.Fatal(err)
	}
	mergeOpenAIUsage(&info, u)
	if info.CacheReadInputTokens != 20 {
		t.Errorf("cache read = %d, want 20", info.CacheReadInputTokens)
	}
}

// Non-streaming Chat Completions must record the assistant message as
// gen_ai.output.messages when content capture is on.
func TestOpenAIChatCompletionsCapturesOutputMessages(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"c","model":"gpt-5-mini","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Hello there"}}],"usage":{"prompt_tokens":8,"completion_tokens":5}}`)
	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	info := &Info{System: "openai", Operation: "chat"}
	OpenAI{}.ParseResponseJSON(span, info, body, ContentOpts{CaptureEvents: true})
	span.End()

	var got string
	for _, a := range rec.Ended()[0].Attributes() {
		if string(a.Key) == "gen_ai.output.messages" {
			got = a.Value.AsString()
		}
	}
	if !strings.Contains(got, "Hello there") || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("gen_ai.output.messages = %q, want assistant text and finish_reason", got)
	}

	// With capture off nothing is recorded.
	rec2, tp2 := newRecorderTracer()
	_, span2 := tp2.Tracer("t").Start(context.Background(), "x")
	OpenAI{}.ParseResponseJSON(span2, &Info{System: "openai", Operation: "chat"}, body, ContentOpts{})
	span2.End()
	for _, a := range rec2.Ended()[0].Attributes() {
		if string(a.Key) == "gen_ai.output.messages" {
			t.Fatal("output messages recorded with capture off")
		}
	}
}

// Streamed assistant text must be recorded decoded, not as raw JSON.
func TestOpenAIStreamingOutputMessageIsDecodedText(t *testing.T) {
	t.Parallel()

	chunks := []map[string]any{
		{"id": "c", "model": "gpt-5-mini", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hello"}}}},
		{"id": "c", "model": "gpt-5-mini", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": nil, "tool_calls": nil}}}},
		{"id": "c", "model": "gpt-5-mini", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": " world"}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2}},
	}
	var sse strings.Builder
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		sse.WriteString("data: " + string(b) + "\n\n")
	}
	sse.WriteString("data: [DONE]\n\n")

	rec, tp := newRecorderTracer()
	_, span := tp.Tracer("t").Start(context.Background(), "x")
	info := &Info{System: "openai", Operation: "chat", Stream: true}
	done := make(chan struct{})
	body := OpenAI{}.WrapStream(span, info, io.NopCloser(strings.NewReader(sse.String())),
		ContentOpts{CaptureEvents: true}, func() { close(done) })
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	<-done
	span.End()

	var got string
	for _, a := range rec.Ended()[0].Attributes() {
		if string(a.Key) == "gen_ai.output.messages" {
			got = a.Value.AsString()
		}
	}
	if !strings.Contains(got, `"content":"Hello world"`) {
		t.Fatalf("gen_ai.output.messages = %s, want decoded text \"Hello world\"", got)
	}
}
