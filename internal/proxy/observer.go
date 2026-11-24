package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/llmspan/llmspan/internal/genai"
	"github.com/llmspan/llmspan/internal/provider"
	"github.com/llmspan/llmspan/internal/replay"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// requestObserver owns per-request enrichment: ModifyResponse wiring,
// metric recording, and span finalization. Extracted from the old
// responseInterceptor closure pyramid so finalize / metrics / status-
// recovery paths are unit-testable without standing up a ReverseProxy.
//
// Concurrency invariant (documented, not atomicized field-by-field):
//
//   - ModifyResponse runs at most once per request, and ReverseProxy
//     calls it before any bytes of the response body are copied to
//     the client. So statusCode / errCapture are exclusively written
//     here (or later by Finalize when status is still 0).
//   - Finalize may be invoked from the body Close/EOF callback and
//     again from ServeHTTP after ReverseProxy returns. Those two
//     calls race; finalized CompareAndSwap makes Finalize idempotent.
//   - info fields are mutated by the provider parsers during body
//     reads, which complete before (or as part of) the Finalize that
//     wins the CAS. The post-ServeHTTP Finalize is always a no-op by
//     then, or is the sole Finalize when ModifyResponse never ran.
//
// Do not start new writer goroutines against these fields without
// revisiting this invariant — that is what made the closure form
// untestable.
type requestObserver struct {
	h              *Handler
	prv            provider.Provider
	info           *provider.Info
	content        provider.ContentOpts
	span           trace.Span
	upstreamName   string
	fallbackStatus func() int
	// serverAddr/serverPort are the upstream's host and port (spec
	// server.address / server.port); empty/0 when unresolvable.
	serverAddr string
	serverPort int

	finalized  atomic.Bool
	statusCode int
	// errCapture, when non-nil, indicates the response was an error
	// (4xx/5xx). It tees a snippet from the body without buffering
	// the whole thing so the original stream forwards intact.
	errCapture *snippetCapture
	record     replay.Entry
}

// newRequestObserver constructs an observer bound to an in-flight
// request. The returned Finalize / ModifyResponse method values are
// what ResponseInterceptor hands to ReverseProxy and ServeHTTP.
func newRequestObserver(
	h *Handler,
	prv provider.Provider,
	info *provider.Info,
	content provider.ContentOpts,
	span trace.Span,
	upstreamName string,
	fallbackStatus func() int,
) *requestObserver {
	o := &requestObserver{
		h:              h,
		prv:            prv,
		info:           info,
		content:        content,
		span:           span,
		upstreamName:   upstreamName,
		fallbackStatus: fallbackStatus,
	}
	for _, u := range h.cfg.Upstreams {
		if u.Name == upstreamName {
			o.serverAddr, o.serverPort = splitTarget(u.Target)
			break
		}
	}
	if o.serverAddr != "" {
		span.SetAttributes(
			attribute.String(genai.AttrServerAddress, o.serverAddr),
			attribute.Int(genai.AttrServerPort, o.serverPort),
		)
	}
	return o
}

// splitTarget extracts host and effective port from an upstream target URL.
func splitTarget(target string) (string, int) {
	u, err := url.Parse(target)
	if err != nil || u.Hostname() == "" {
		return "", 0
	}
	port := 0
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	} else if u.Scheme == "https" {
		port = 443
	} else if u.Scheme == "http" {
		port = 80
	}
	return u.Hostname(), port
}

// pricingSystem maps gen_ai.provider.name values to the keys of the price
// table, which still files all Google models under the legacy "gcp.gen_ai".
func pricingSystem(system string) string {
	switch system {
	case genai.SystemGemini, genai.SystemVertexAI:
		return "gcp.gen_ai"
	}
	return system
}

// errorType returns the spec error.type for a failed operation, or "".
func (o *requestObserver) errorType() string {
	if o.info.ErrorType != "" {
		return o.info.ErrorType
	}
	if o.statusCode >= 400 {
		return strconv.Itoa(o.statusCode)
	}
	return ""
}

// Finalize records metrics and ends the span exactly once. Safe to
// call from both the streaming/error body close path and the
// post-ServeHTTP path.
func (o *requestObserver) Finalize(ctx context.Context) {
	if !o.finalized.CompareAndSwap(false, true) {
		return
	}
	if o.info.Finished.IsZero() {
		o.info.Finished = time.Now()
	}
	// ModifyResponse never ran (upstream error path): recover the
	// status the ErrorHandler wrote to the client; if even that is
	// absent, the request admitted but produced no response — an
	// upstream-side fault, same mapping release() uses.
	if o.statusCode == 0 && o.fallbackStatus != nil {
		o.statusCode = o.fallbackStatus()
	}
	if o.statusCode == 0 {
		o.statusCode = http.StatusBadGateway
	}
	if et := o.errorType(); et != "" {
		o.info.ErrorType = et
	}
	// Span name per spec: "{gen_ai.operation.name} {gen_ai.request.model}".
	// proxy.go starts the span with the bare operation; ParseRequest renames
	// it on the happy path, this covers parse-failure paths.
	if o.info.Operation != "" {
		o.span.SetName(genai.SpanName(o.info.Operation, o.info.RequestModel))
	}
	o.recordMetrics(ctx)
	o.logSummary(ctx)
	o.record.Status = o.statusCode
	o.record.Stream = o.info.Stream
	if err := o.h.recorder.Append(o.record); err != nil && o.h.logger != nil {
		o.h.logger.ErrorContext(ctx, "record request", slog.Any("err", err))
	}
	o.finishSpan()
}

func (o *requestObserver) logSummary(ctx context.Context) {
	if o.h.logger == nil {
		return
	}
	model := o.info.ResponseModel
	if model == "" {
		model = o.info.RequestModel
	}
	args := []any{
		slog.String("system", o.info.System),
		slog.String("operation", o.info.Operation),
		slog.String("model", model),
		slog.String("upstream", o.upstreamName),
		slog.Int("status", o.statusCode),
		slog.Int64("input_tokens", o.info.InputTokens),
		slog.Int64("output_tokens", o.info.OutputTokens),
		slog.Duration("duration", o.info.Finished.Sub(o.info.Started)),
	}
	if o.info.Caller != "" {
		args = append(args, slog.String("caller", o.info.Caller))
	}
	if usd, ok := o.h.pricing.CostTiered(
		pricingSystem(o.info.System), model, o.info.ServiceTier, o.info.InputTokens, o.info.OutputTokens,
		o.info.CacheReadInputTokens, o.info.CacheCreationInputTokens,
	); ok {
		args = append(args, slog.Float64("cost_usd", usd))
	}
	o.h.logger.InfoContext(ctx, "llm request completed", args...)
}

func (o *requestObserver) finishSpan() {
	if o.statusCode >= 400 {
		o.span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", o.statusCode))
		if o.info.ErrorType == "" {
			o.info.ErrorType = strconv.Itoa(o.statusCode)
		}
		o.span.SetAttributes(attribute.String(genai.AttrErrorType, o.info.ErrorType))
	} else if o.statusCode > 0 {
		o.span.SetStatus(codes.Ok, "")
	}
	o.span.SetAttributes(attribute.Int("http.response.status_code", o.statusCode))
	if o.errCapture != nil {
		o.span.SetAttributes(attribute.Int("http.response.body_size", o.errCapture.total))
		if o.content.Capture() {
			// Redact BEFORE truncating: truncating first can slice
			// a secret so its tail no longer matches any redaction
			// pattern, shipping the surviving prefix into the
			// span. The capture buffer is 2x the snippet size so a
			// secret straddling the 1 KiB boundary is still seen
			// whole by the redactor.
			o.span.SetAttributes(attribute.String(
				"http.response.body_snippet",
				truncateUTF8(o.content.Clean(string(o.errCapture.head)), 1024),
			))
		}
	}
	// The circuit breaker is fed from ServeHTTP's admission release
	// path — NOT from here. release() reads the final status code
	// that reached the client via the statusCaptureWriter wrapper
	// and folds it into the breaker. That central reporting is the
	// only thing that prevents transparent-passthrough and
	// hard-cap-reject paths from leaking the half-open probe slot.
	o.span.End()
}

func (o *requestObserver) recordMetrics(ctx context.Context) {
	// Bound model-label cardinality before it hits any metric. Pricing
	// uses the raw model so snapshot suffixes still match the table.
	reqModelLabel := o.h.modelLabel.Normalize(o.info.RequestModel)
	respModelLabel := o.h.modelLabel.Normalize(o.info.ResponseModel)
	callerLabel := o.h.callerLabel.Normalize(o.info.Caller)
	m := o.h.meters

	// Spec attribute set shared by duration / token metrics.
	base := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, o.info.Operation),
		attribute.String(genai.AttrSystem, o.info.System),
	}
	if reqModelLabel != "" {
		base = append(base, attribute.String(genai.AttrRequestModel, reqModelLabel))
	}
	if respModelLabel != "" {
		base = append(base, attribute.String(genai.AttrResponseModel, respModelLabel))
	}
	if o.serverAddr != "" {
		base = append(base, attribute.String(genai.AttrServerAddress, o.serverAddr),
			attribute.Int(genai.AttrServerPort, o.serverPort))
	}
	// llmspan extension, not part of the spec attribute set.
	if callerLabel != "" {
		base = append(base, attribute.String(genai.AttrCaller, callerLabel))
	}
	withErr := base
	if et := o.errorType(); et != "" {
		withErr = append(append(make([]attribute.KeyValue, 0, len(base)+1), base...),
			attribute.String(genai.AttrErrorType, et))
	}
	baseOpt := metric.WithAttributes(base...)
	errOpt := metric.WithAttributes(withErr...)
	embeddings := o.info.Operation == genai.OpEmbeddings

	if d := o.info.DurationSeconds(); d > 0 {
		m.OperationDuration.Record(ctx, d, errOpt) // legacy name, kept one release
		if embeddings {
			if m.EmbeddingsDuration != nil {
				m.EmbeddingsDuration.Record(ctx, d, errOpt)
			}
		} else if m.InferenceDuration != nil {
			m.InferenceDuration.Record(ctx, d, errOpt)
		}
	}
	if t := o.info.TimeToFirstByteSeconds(); t > 0 {
		m.TimeToFirstByte.Record(ctx, t, baseOpt) // legacy name
		if m.UpstreamTTFB != nil {
			m.UpstreamTTFB.Record(ctx, t, metric.WithAttributes(
				attribute.String("upstream", o.upstreamName),
				attribute.String(genai.AttrOperationName, o.info.Operation),
				attribute.String(genai.AttrSystem, o.info.System)))
		}
		o.span.SetAttributes(attribute.Float64(genai.AttrTimeToFirstByte, t))
	}
	if t := o.info.TimeToFirstTokenSeconds(); t > 0 {
		m.TimeToFirstToken.Record(ctx, t, baseOpt) // legacy name
		if m.TimeToFirstChunk != nil {
			m.TimeToFirstChunk.Record(ctx, t, baseOpt)
		}
		o.span.SetAttributes(
			attribute.Float64(genai.AttrRespTimeToFirstChunk, t),
			attribute.Float64(genai.AttrTimeToFirstToken, t)) // legacy
	}

	tokenOpt := func(tokenType string) metric.RecordOption {
		kv := append(append(make([]attribute.KeyValue, 0, len(base)+1), base...),
			attribute.String(genai.AttrTokenType, tokenType))
		return metric.WithAttributes(kv...)
	}
	if o.info.InputTokens > 0 {
		m.TokenUsage.Record(ctx, o.info.InputTokens, tokenOpt(genai.TokenTypeInput)) // legacy
		if embeddings {
			if m.EmbeddingsInputTokens != nil {
				m.EmbeddingsInputTokens.Record(ctx, o.info.InputTokens, errOpt)
			}
		} else {
			if m.InferenceInputTokens != nil {
				m.InferenceInputTokens.Record(ctx, o.info.InputTokens, errOpt)
			}
			addTokens(ctx, m.UsageInputTokens, o.info.InputTokens, baseOpt)
		}
	}
	if o.info.OutputTokens > 0 {
		m.TokenUsage.Record(ctx, o.info.OutputTokens, tokenOpt(genai.TokenTypeOutput)) // legacy
		if !embeddings {
			if m.InferenceOutputTokens != nil {
				m.InferenceOutputTokens.Record(ctx, o.info.OutputTokens, errOpt)
			}
			addTokens(ctx, m.UsageOutputTokens, o.info.OutputTokens, baseOpt)
		}
	}
	if !embeddings {
		addTokens(ctx, m.UsageCacheRead, o.info.CacheReadInputTokens, baseOpt)
		addTokens(ctx, m.UsageCacheWrite, o.info.CacheCreationInputTokens, baseOpt)
		addTokens(ctx, m.UsageReasoning, o.info.ReasoningOutputTokens, baseOpt)
	}

	model := o.info.ResponseModel
	if model == "" {
		model = o.info.RequestModel
	}
	// Cost is an llmspan extension: it carries the spec's operation name
	// and provider so it can be grouped like the standard metrics.
	costAttrList := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, o.info.Operation),
		attribute.String(genai.AttrSystem, o.info.System),
		attribute.String(genai.AttrRequestModel, reqModelLabel),
		attribute.String(genai.AttrResponseModel, respModelLabel),
	}
	if callerLabel != "" {
		costAttrList = append(costAttrList, attribute.String(genai.AttrCaller, callerLabel))
	}
	costAttrs := metric.WithAttributes(costAttrList...)
	if usd, ok := o.h.pricing.CostTiered(
		pricingSystem(o.info.System), model, o.info.ServiceTier, o.info.InputTokens, o.info.OutputTokens,
		o.info.CacheReadInputTokens, o.info.CacheCreationInputTokens,
	); ok {
		// Per-call distribution + monotonic cumulative. The
		// histogram serves p50 / p95 cost dashboards; the
		// counter sidesteps float drift across millions of small adds.
		m.CostUSD.Record(ctx, usd, costAttrs)
		m.CostUSDTotal.Add(ctx, usd, costAttrs)
		o.span.SetAttributes(attribute.Float64(genai.AttrCostUSD, usd))
	} else if model != "" && (o.info.InputTokens > 0 || o.info.OutputTokens > 0) {
		// Token usage arrived but the catalogue has no rate for
		// this model — the FinOps dashboard just undercounted.
		o.h.pricingMisses.Add(ctx, 1, costAttrs)
	}
	// llmspan.requests.total is recorded once in ServeHTTP's defer so
	// passthrough and reject paths are counted too; it carries the
	// upstream and HTTP status labels removed from the standard metrics.
}

func addTokens(ctx context.Context, c metric.Int64Counter, n int64, opt metric.AddOption) {
	if c != nil && n > 0 {
		c.Add(ctx, n, opt)
	}
}

// ModifyResponse is the ReverseProxy hook. Typed as a method so the
// context-value type assertion against modifyFn in Rewrite succeeds —
// named function types and their unnamed underlying type are distinct
// under runtime type assertions, and this method value has type
// modifyFn when assigned.
func (o *requestObserver) ModifyResponse(resp *http.Response) error {
	o.statusCode = resp.StatusCode
	o.info.FirstByteAt = time.Now()
	o.span.SetAttributes(attribute.String(genai.AttrSystem, o.info.System))

	if o.statusCode >= 400 {
		// Error bodies must reach the client byte-for-byte — they
		// frequently carry the structured detail the caller needs
		// to debug. We peel off a bounded snippet for the span via
		// an io.TeeReader; the rest of the bytes flow through
		// untouched. The snippet itself is gated on captureContent
		// since OpenAI/Anthropic error bodies routinely echo a
		// prefix of the offending API key, which would otherwise
		// leak into traces under the privacy-off default.
		//
		// Wrap the body in parseOnClose so Finalize fires on EOF —
		// before httputil's copy loop reports the body as drained.
		streamCtx := context.WithoutCancel(resp.Request.Context())
		// 2 KiB capture for a 1 KiB snippet: redaction runs on the
		// larger buffer so boundary-straddling secrets are masked
		// before the final truncation (see finishSpan).
		o.errCapture = &snippetCapture{headCap: 2048}
		origCloser := o.wrapIdleBody(resp.Body)
		teed := io.TeeReader(origCloser, o.errCapture)
		resp.Body = &parseOnClose{
			Reader: teed,
			closer: origCloser,
			onClose: func() {
				// Provider error envelopes carry stable error.type values.
				// Decode the bounded capture before finalization; content
				// remains gated/redacted by ContentOpts.
				o.prv.ParseResponseJSON(o.span, o.info, o.errCapture.head, o.content)
				o.record.Response = string(o.errCapture.head)
				o.record.ResponseTruncated = o.errCapture.total > len(o.errCapture.head)
				o.Finalize(streamCtx)
			},
		}
		return nil
	}

	if isEventStream(resp.Header) {
		o.h.incActiveStreams(1)
		o.info.Stream = true
		// Keep the request's trace context alive for the metric
		// emission that Finalize will do when the stream closes.
		// context.WithoutCancel preserves baggage/span context
		// while detaching from the request's cancellation chain —
		// metrics for a completed stream should fire even if the
		// client has already disconnected.
		streamCtx := context.WithoutCancel(resp.Request.Context())
		// Idle watchdog wraps the upstream body (inner) so silence
		// is measured on provider bytes, not on client drain pace.
		body := o.wrapIdleBody(resp.Body)
		var recordCallback *callbackReadCloser
		if o.h.recorder != nil {
			capture := &snippetCapture{headCap: maxEnrichmentBodyBytes}
			body = &teeReadCloser{Reader: io.TeeReader(body, capture), Closer: body}
			recordCallback = &callbackReadCloser{ReadCloser: body, beforeClose: func() {
				o.record.Response = string(capture.head)
				o.record.ResponseTruncated = capture.total > len(capture.head)
			}}
			body = recordCallback
		}
		resp.Body = o.prv.WrapStream(o.span, o.info, body, o.content,
			func() {
				if recordCallback != nil {
					recordCallback.run()
				}
				o.h.incActiveStreams(-1)
				o.Finalize(streamCtx)
			},
		)
		return nil
	}

	// Non-streaming JSON: stream-through-with-tee. The old
	// implementation drained the entire body into memory before
	// returning, which delayed the client's first byte by the
	// full upstream duration. Now the body forwards through a
	// TeeReader while a bounded snippetCapture accumulates up
	// to maxEnrichmentBodyBytes for the parser. The parser and
	// Finalize both fire as soon as the upstream reader hits EOF.
	streamCtx := context.WithoutCancel(resp.Request.Context())
	capture := &snippetCapture{headCap: maxEnrichmentBodyBytes}
	origCloser := o.wrapIdleBody(resp.Body)
	teed := io.TeeReader(origCloser, capture)
	resp.Body = &parseOnClose{
		Reader: teed,
		closer: origCloser,
		onClose: func() {
			o.prv.ParseResponseJSON(o.span, o.info, capture.head, o.content)
			o.record.Response = string(capture.head)
			o.record.ResponseTruncated = capture.total > len(capture.head)
			o.Finalize(streamCtx)
		},
	}
	return nil
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

type callbackReadCloser struct {
	io.ReadCloser
	beforeClose func()
	once        atomic.Bool
}

func (c *callbackReadCloser) run() {
	if c.once.CompareAndSwap(false, true) && c.beforeClose != nil {
		c.beforeClose()
	}
}

func (c *callbackReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err != nil {
		c.run()
	}
	return n, err
}

func (c *callbackReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.run()
	return err
}

// wrapIdleBody installs the post-header stream idle watchdog when
// http.stream_idle_timeout > 0. onIdle emits a span event + metric
// then Close()s the upstream body so in-flight Reads unblock.
func (o *requestObserver) wrapIdleBody(body io.ReadCloser) io.ReadCloser {
	idle := o.h.cfg.HTTP.StreamIdleTimeout
	if idle <= 0 {
		return body
	}
	return wrapIdleTimeout(body, idle, func() {
		o.span.AddEvent("llmspan.stream.idle_timeout",
			trace.WithAttributes(
				attribute.Float64("llmspan.stream.idle_timeout_seconds", idle.Seconds()),
				attribute.String("upstream", o.upstreamName),
			),
		)
		o.h.recordStreamIdleTimeout(o.upstreamName)
	})
}
