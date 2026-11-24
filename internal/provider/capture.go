package provider

import (
	"context"
	"encoding/json"
	"os"

	"github.com/llmspan/llmspan/internal/genai"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// ContentOpts shapes how providers handle prompt/completion content
// when populating span events or OTel log records.
type ContentOpts struct {
	// CaptureEvents attaches content as span events (content.mode: events).
	CaptureEvents bool
	// CaptureLogs emits content via the OTel log signal (content.mode:
	// logs) so collectors can route retention separately from traces.
	CaptureLogs bool
	// Logger is required when CaptureLogs is true. Nil is a no-op.
	Logger otellog.Logger
	// Redact, if non-nil, is applied to every captured content string
	// before it reaches a span attribute or log body.
	Redact func(string) string
}

// capturing reports whether any content sink is active.
func (o ContentOpts) capturing() bool {
	return o.CaptureEvents || o.CaptureLogs
}

// Capture reports whether prompt/completion content should be recorded.
func (o ContentOpts) Capture() bool { return o.capturing() }

// Clean returns s with the configured redactor applied, or s unchanged
// when Redact is nil.
func (o ContentOpts) Clean(s string) string {
	if o.Redact == nil {
		return s
	}
	return o.Redact(s)
}

// legacyEventsEnabled reports whether the deprecated per-message events
// (gen_ai.user.message, gen_ai.choice, ...) should ALSO be emitted. Opt-in
// via LLMSPAN_LEGACY_GENAI_EVENTS=1; removal planned the release after next.
func legacyEventsEnabled() bool { return os.Getenv("LLMSPAN_LEGACY_GENAI_EVENTS") == "1" }

// inMsg / outMsg are provider-neutral text messages. Roles are normalised to
// the spec's system|user|assistant|tool.
type inMsg struct {
	Role string
	Text string
}

type outMsg struct {
	Text         string
	FinishReason string
}

type part struct {
	Type     string `json:"type"`
	Content  string `json:"content,omitempty"`
	Response string `json:"response,omitempty"`
}

type specMsg struct {
	Role         string `json:"role"`
	Parts        []part `json:"parts"`
	FinishReason string `json:"finish_reason,omitempty"`
}

func normRole(r string) string {
	switch r {
	case "model":
		return "assistant"
	case "developer":
		return "system"
	case "":
		return "user"
	}
	return r
}

func legacyEventFor(role string) string {
	switch normRole(role) {
	case "system":
		return genai.EventSystemMessage
	case "assistant":
		return genai.EventAssistantMessage
	case "tool":
		return genai.EventToolMessage
	}
	return genai.EventUserMessage
}

// emitInputMessages records the request content in the current-spec form:
// gen_ai.system_instructions and gen_ai.input.messages span attributes
// (JSON, redacted) under content.mode events, or a
// gen_ai.client.inference.operation.details log record under mode logs.
// system may be empty. Nothing is emitted unless capture is on.
func (o ContentOpts) emitInputMessages(ctx context.Context, span trace.Span, system string, msgs []inMsg) {
	if !o.capturing() {
		return
	}
	var sys []part
	if system != "" {
		sys = []part{{Type: "text", Content: o.Clean(system)}}
	}
	in := make([]specMsg, 0, len(msgs))
	for _, m := range msgs {
		if m.Text == "" {
			continue
		}
		role := normRole(m.Role)
		txt := o.Clean(m.Text)
		p := part{Type: "text", Content: txt}
		if role == "tool" {
			p = part{Type: "tool_call_response", Response: txt}
		}
		in = append(in, specMsg{Role: role, Parts: []part{p}})
	}
	if o.CaptureEvents && span.IsRecording() {
		if len(sys) > 0 {
			span.SetAttributes(attribute.String(genai.AttrSystemInstruction, mustJSON(sys)))
		}
		if len(in) > 0 {
			span.SetAttributes(attribute.String(genai.AttrInputMessages, mustJSON(in)))
		}
	}
	if o.CaptureLogs && o.Logger != nil && (len(sys) > 0 || len(in) > 0) {
		var rec otellog.Record
		rec.SetEventName(genai.EventOperationDetails)
		if len(sys) > 0 {
			rec.AddAttributes(attribute.KeyValue{Key: genai.AttrSystemInstruction, Value: logValue(sys)})
		}
		if len(in) > 0 {
			rec.AddAttributes(attribute.KeyValue{Key: genai.AttrInputMessages, Value: logValue(in)})
		}
		o.Logger.Emit(ctx, rec)
	}
	if legacyEventsEnabled() {
		if system != "" {
			o.legacyRoleMessage(ctx, span, genai.EventSystemMessage, "system", system)
		}
		for _, m := range msgs {
			o.legacyRoleMessage(ctx, span, legacyEventFor(m.Role), m.Role, m.Text)
		}
	}
}

// emitOutputMessages records completion content (gen_ai.output.messages).
func (o ContentOpts) emitOutputMessages(ctx context.Context, span trace.Span, outs []outMsg) {
	if !o.capturing() {
		return
	}
	out := make([]specMsg, 0, len(outs))
	for _, m := range outs {
		if m.Text == "" {
			continue
		}
		out = append(out, specMsg{
			Role:         "assistant",
			Parts:        []part{{Type: "text", Content: o.Clean(m.Text)}},
			FinishReason: m.FinishReason,
		})
	}
	if len(out) == 0 {
		return
	}
	if o.CaptureEvents && span.IsRecording() {
		span.SetAttributes(attribute.String(genai.AttrOutputMessages, mustJSON(out)))
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetEventName(genai.EventOperationDetails)
		rec.AddAttributes(attribute.KeyValue{Key: genai.AttrOutputMessages, Value: logValue(out)})
		o.Logger.Emit(ctx, rec)
	}
	if legacyEventsEnabled() {
		for i, m := range outs {
			o.legacyChoice(ctx, span, i, m.FinishReason, m.Text)
		}
	}
}

// emitAssembledChoice records streamed completion text at stream close.
// (Signature kept for stream.go.)
func (o ContentOpts) emitAssembledChoice(ctx context.Context, span trace.Span, message string, truncated bool) {
	if !o.capturing() || message == "" {
		return
	}
	o.emitOutputMessages(ctx, span, []outMsg{{Text: message}})
	if truncated && o.CaptureEvents && span.IsRecording() {
		span.SetAttributes(attribute.Bool("llmspan.content_truncated", true))
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// logValue converts a JSON-marshalable structure into an OTel log value so
// the log record carries structured (not stringified) message parts.
func logValue(v any) attribute.Value {
	b, err := json.Marshal(v)
	if err != nil {
		return attribute.StringValue("")
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		return attribute.StringValue(string(b))
	}
	return toLogValue(x)
}

func toLogValue(x any) attribute.Value {
	switch t := x.(type) {
	case string:
		return attribute.StringValue(t)
	case bool:
		return attribute.BoolValue(t)
	case float64:
		return attribute.Float64Value(t)
	case []any:
		vs := make([]attribute.Value, len(t))
		for i, e := range t {
			vs[i] = toLogValue(e)
		}
		return attribute.SliceValue(vs...)
	case map[string]any:
		kvs := make([]attribute.KeyValue, 0, len(t))
		for k, e := range t {
			kvs = append(kvs, attribute.KeyValue{Key: attribute.Key(k), Value: toLogValue(e)})
		}
		return attribute.MapValue(kvs...)
	}
	return attribute.Value{}
}

// legacyRoleMessage records (pre-v0.1.0 per-message event) a request message (system/user/assistant/tool).
func (o ContentOpts) legacyRoleMessage(ctx context.Context, span trace.Span, eventName, role, content string) {
	if !o.capturing() || content == "" {
		return
	}
	cleaned := o.Clean(content)
	if o.CaptureEvents && span.IsRecording() {
		span.AddEvent(eventName, trace.WithAttributes(
			attribute.String("role", role),
			attribute.String("content", cleaned),
		))
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetBody(attribute.StringValue(cleaned))
		rec.AddAttributes(
			attribute.String("gen_ai.event", eventName),
			attribute.String("role", role),
			attribute.String("message", cleaned),
		)
		o.Logger.Emit(ctx, rec)
	}
}

// legacyChoice records a completion choice.
func (o ContentOpts) legacyChoice(ctx context.Context, span trace.Span, index int, finishReason, message string) {
	if !o.capturing() || message == "" {
		return
	}
	cleaned := o.Clean(message)
	if o.CaptureEvents && span.IsRecording() {
		span.AddEvent(genai.EventChoice, trace.WithAttributes(
			attribute.Int("index", index),
			attribute.String("finish_reason", finishReason),
			attribute.String("message", cleaned),
		))
	}
	if o.CaptureLogs && o.Logger != nil {
		var rec otellog.Record
		rec.SetBody(attribute.StringValue(cleaned))
		rec.AddAttributes(
			attribute.String("gen_ai.event", genai.EventChoice),
			attribute.Int("index", index),
			attribute.String("finish_reason", finishReason),
			attribute.String("message", cleaned),
		)
		o.Logger.Emit(ctx, rec)
	}
}
