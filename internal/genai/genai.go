// Package genai centralises OpenTelemetry GenAI semantic-convention keys and
// values used by llmspan.
//
// We define our own constants rather than importing them from a shipping
// semconv version because (a) the GenAI namespace has been actively
// stabilising through 2024-2025, (b) llmspan should compile against many
// otel-go releases, and (c) the tight surface keeps a clean upgrade story.
//
// Spec: the GenAI conventions now live in
// https://github.com/open-telemetry/semantic-conventions-genai (v0.1.0,
// Development status). See docs/SEMCONV-COMPLIANCE.md for the pinned
// version and the list of llmspan extensions and deviations.
package genai

// Span attribute keys (semconv GenAI).
const (
	AttrSystem                 = "gen_ai.provider.name"
	AttrOperationName          = "gen_ai.operation.name"
	AttrRequestModel           = "gen_ai.request.model"
	AttrRequestTemperature     = "gen_ai.request.temperature"
	AttrRequestTopP            = "gen_ai.request.top_p"
	AttrRequestTopK            = "gen_ai.request.top_k"
	AttrRequestMaxTokens       = "gen_ai.request.max_tokens"
	AttrRequestFrequencyPen    = "gen_ai.request.frequency_penalty"
	AttrRequestPresencePen     = "gen_ai.request.presence_penalty"
	AttrRequestSeed            = "gen_ai.request.seed"
	AttrRequestStopSequences   = "gen_ai.request.stop_sequences"
	AttrRequestEncodingFmts    = "gen_ai.request.encoding_formats"
	AttrResponseID             = "gen_ai.response.id"
	AttrResponseModel          = "gen_ai.response.model"
	AttrResponseFinishReasons  = "gen_ai.response.finish_reasons"
	AttrUsageInputTokens       = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens      = "gen_ai.usage.output_tokens"
	AttrUsageCacheReadTokens   = "gen_ai.usage.cache_read.input_tokens"
	AttrUsageCacheCreateTokens = "gen_ai.usage.cache_creation.input_tokens"
	AttrUsageReasoningTokens   = "gen_ai.usage.reasoning.output_tokens"
	AttrErrorType              = "error.type"
	AttrRequestServiceTier     = "gen_ai.openai.request.service_tier"
	AttrResponseServiceTier    = "gen_ai.openai.response.service_tier"
	AttrCaller                 = "llmspan.caller"
	AttrConversationID         = "gen_ai.conversation.id"
	AttrAgento11yRecord        = "agento11y.record"

	// AttrUsageCacheWriteTokens is the v0.1.0 name; AttrUsageCacheCreateTokens
	// is the pre-v0.1.0 name and is still emitted for one release.
	AttrUsageCacheWriteTokens = "gen_ai.usage.cache_write.input_tokens"
	AttrTokenType             = "gen_ai.token.type"
	AttrServerAddress         = "server.address"
	AttrServerPort            = "server.port"
	AttrRespTimeToFirstChunk  = "gen_ai.response.time_to_first_chunk"

	// Opt-in content attributes (structured JSON; set only when content
	// capture is enabled, after redaction).
	AttrInputMessages     = "gen_ai.input.messages"
	AttrOutputMessages    = "gen_ai.output.messages"
	AttrSystemInstruction = "gen_ai.system_instructions"
)

// EventOperationDetails is the log/event name carrying opt-in content in
// the current spec.
const EventOperationDetails = "gen_ai.client.inference.operation.details"

// llmspan-specific extensions. Names follow the gen_ai.* root so dashboards
// can use a single namespace; the suffix `.cost.usd` is not in the spec yet
// but matches community precedent (Logfire, Phoenix).
const (
	AttrCostUSD          = "gen_ai.cost.usd"
	AttrTimeToFirstToken = "gen_ai.time_to_first_token"
	AttrTimeToFirstByte  = "gen_ai.server.time_to_first_byte"
	AttrStream           = "gen_ai.request.stream"
)

// Provider values (gen_ai.provider.name).
const (
	SystemOpenAI    = "openai"
	SystemAnthropic = "anthropic"
	// SystemGemini is the AI Studio endpoint (generativelanguage.googleapis.com).
	SystemGemini = "gcp.gemini"
	// SystemVertexAI is aiplatform.googleapis.com.
	SystemVertexAI = "gcp.vertex_ai"
)

// Operation names (gen_ai.operation.name).
const (
	OpChat       = "chat"
	OpEmbeddings = "embeddings"
	// Responses API is a wire API, not a semantic operation. It emits chat.
	OpResponses = OpChat
)

// Span event names: the pre-v0.1.0 per-message event model. DEPRECATED in the
// spec; llmspan emits these only when LLMSPAN_LEGACY_GENAI_EVENTS=1 and will
// remove them in the release after next.
const (
	EventSystemMessage    = "gen_ai.system.message"
	EventUserMessage      = "gen_ai.user.message"
	EventAssistantMessage = "gen_ai.assistant.message"
	EventToolMessage      = "gen_ai.tool.message"
	EventChoice           = "gen_ai.choice"
	EventToolCall         = "gen_ai.tool.call"
)

// Metric instrument names (semconv GenAI metrics).
const (
	MetricTokenUsage        = "gen_ai.client.token.usage"
	MetricOperationDuration = "gen_ai.client.operation.duration"
	// MetricTimeToFirstToken is a DEPRECATED llmspan-invented name, emitted
	// for one release alongside MetricTimeToFirstChunk.
	MetricTimeToFirstToken = "gen_ai.client.time_to_first_token"
	// Current-spec inference metrics (semantic-conventions-genai v0.1.0).
	MetricInferenceDuration     = "gen_ai.client.inference.duration"
	MetricTimeToFirstChunk      = "gen_ai.client.inference.time_to_first_chunk"
	MetricInferenceInputTokens  = "gen_ai.client.inference.operation.input_tokens"
	MetricInferenceOutputTokens = "gen_ai.client.inference.operation.output_tokens"
	MetricUsageInputTokens      = "gen_ai.client.inference.usage.input_tokens"
	MetricUsageOutputTokens     = "gen_ai.client.inference.usage.output_tokens"
	MetricUsageCacheReadTokens  = "gen_ai.client.inference.usage.cache_read.input_tokens"
	MetricUsageCacheWriteTokens = "gen_ai.client.inference.usage.cache_write.input_tokens"
	MetricUsageReasoningTokens  = "gen_ai.client.inference.usage.reasoning.output_tokens"
	MetricEmbeddingsDuration    = "gen_ai.client.embeddings.duration"
	MetricEmbeddingsInputTokens = "gen_ai.client.embeddings.operation.input_tokens"
	// MetricUpstreamTTFB is the llmspan extension replacing the
	// gen_ai-namespaced MetricTimeToFirstByte (kept one release).
	MetricUpstreamTTFB = "llmspan.upstream.time_to_first_byte"
	// MetricTimeToFirstByte is wall-clock from request start to the
	// first upstream response byte (headers/status known). Distinct
	// from TTFT, which waits for the first content/tool delta.
	MetricTimeToFirstByte = "gen_ai.server.time_to_first_byte"
	// MetricCostUSD is a USD-scaled histogram of per-call cost — gives
	// dashboards p50 / p95 cost per operation alongside a running sum.
	// Histogram buckets at 1e-5, 1e-4, 1e-3, 0.01, 0.1, 1, 10, 100 USD
	// span free-tier embeddings ($0.00002) through the most expensive
	// gpt-4-class single calls.
	MetricCostUSD = "gen_ai.client.cost.usd"
	// MetricCostUSDTotal is a monotonic counter of cumulative spend.
	// PromQL `sum(rate(gen_ai_client_cost_usd_total_total[5m])) * 3600`
	// reads as "USD/hour" — what FinOps dashboards actually plot.
	// Kept as a sibling rather than reading from the histogram's
	// _sum series because float-counter drift across millions of small
	// adds is a known concern.
	MetricCostUSDTotal = "gen_ai.client.cost.usd.total"
)

// Advised explicit bucket boundaries (spec).
var (
	// DurationBuckets: 0.01 .. 81.92 s, doubling. TTFC/TTFB prepend
	// sub-10ms buckets because cached/local upstreams answer faster.
	DurationBuckets   = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	FirstChunkBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
	// TokenBuckets: 1 .. 67108864, x4.
	TokenBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864}
)

// Token-type label values for MetricTokenUsage (attribute gen_ai.token.type).
const (
	TokenTypeInput  = "input"
	TokenTypeOutput = "output"
)

// SpanName builds the canonical GenAI span name: "{operation} {model}".
// Falls back gracefully if model is unknown so we still get useful spans.
func SpanName(op, model string) string {
	if model == "" {
		return op
	}
	return op + " " + model
}
