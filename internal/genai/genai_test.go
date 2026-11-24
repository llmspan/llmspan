package genai

import (
	"sort"
	"testing"
)

func TestSpanName(t *testing.T) {
	t.Parallel()
	if got := SpanName(OpChat, "gpt-4o"); got != "chat gpt-4o" {
		t.Fatalf("got %q", got)
	}
	if got := SpanName(OpChat, ""); got != "chat" {
		t.Fatalf("got %q", got)
	}
}

func TestKeysMatchSpec(t *testing.T) {
	t.Parallel()
	for got, want := range map[string]string{
		AttrTokenType:          "gen_ai.token.type",
		AttrSystem:             "gen_ai.provider.name",
		AttrInputMessages:      "gen_ai.input.messages",
		AttrOutputMessages:     "gen_ai.output.messages",
		AttrSystemInstruction:  "gen_ai.system_instructions",
		SystemGemini:           "gcp.gemini",
		SystemVertexAI:         "gcp.vertex_ai",
		MetricTimeToFirstChunk: "gen_ai.client.inference.time_to_first_chunk",
		EventOperationDetails:  "gen_ai.client.inference.operation.details",
	} {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestBuckets(t *testing.T) {
	t.Parallel()
	for name, b := range map[string][]float64{"duration": DurationBuckets, "ttfc": FirstChunkBuckets, "tokens": TokenBuckets} {
		if !sort.Float64sAreSorted(b) {
			t.Errorf("%s buckets unsorted", name)
		}
	}
	if DurationBuckets[0] != 0.01 || DurationBuckets[len(DurationBuckets)-1] != 81.92 {
		t.Error("duration buckets must span 0.01..81.92")
	}
	if TokenBuckets[0] != 1 || TokenBuckets[len(TokenBuckets)-1] != 67108864 {
		t.Error("token buckets must span 1..67108864")
	}
	if FirstChunkBuckets[0] >= DurationBuckets[0] {
		t.Error("first-chunk buckets need sub-10ms resolution")
	}
}
