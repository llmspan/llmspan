package provider

import (
	"bytes"
	"io"
	"math/rand"
	"testing"
)

// refFindFrameBoundary is the original (pre-optimisation) implementation,
// kept as a differential-test oracle.
func refFindFrameBoundary(raw []byte) (int, int) {
	bestIdx, bestLen := -1, 0
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n"), []byte("\r\r")} {
		i := bytes.Index(raw, sep)
		if i < 0 {
			continue
		}
		if bestIdx < 0 || i < bestIdx {
			bestIdx, bestLen = i, len(sep)
		}
	}
	return bestIdx, bestLen
}

func TestFindFrameBoundaryMatchesReference(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	alphabet := []byte("\r\nab:")
	for iter := 0; iter < 200000; iter++ {
		raw := make([]byte, rng.Intn(14))
		for i := range raw {
			raw[i] = alphabet[rng.Intn(len(alphabet))]
		}
		wi, ws := refFindFrameBoundary(raw)
		gi, gs := findFrameBoundary(raw)
		if wi != gi || (wi >= 0 && ws != gs) {
			t.Fatalf("raw=%q ref=(%d,%d) got=(%d,%d)", raw, wi, ws, gi, gs)
		}
	}
}

// Resumed scanning across arbitrary read splits must yield the same events
// as feeding the whole stream at once.
func TestSSETeeSplitInvariance(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2))
	alphabet := []byte("\r\n\r\nda:x ")
	collect := func(stream []byte, size int) []string {
		var out []string
		tee := newSSETee(&chunkReader{src: stream, size: size}, func(e string, d []byte) {
			out = append(out, e+"|"+string(d))
		}, nil, func() {})
		_, _ = io.Copy(io.Discard, tee)
		return out
	}
	for iter := 0; iter < 5000; iter++ {
		stream := make([]byte, rng.Intn(60))
		for i := range stream {
			stream[i] = alphabet[rng.Intn(len(alphabet))]
		}
		want := collect(stream, len(stream)+1)
		for _, size := range []int{1, 2, 3, 5} {
			got := collect(stream, size)
			if len(got) != len(want) {
				t.Fatalf("stream=%q size=%d got=%q want=%q", stream, size, got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("stream=%q size=%d got=%q want=%q", stream, size, got, want)
				}
			}
		}
	}
}
