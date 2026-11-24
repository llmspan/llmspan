package provider

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// chunkReader hands out src in fixed-size reads, emulating network
// segmentation (so events straddle Read boundaries).
type chunkReader struct {
	src  []byte
	size int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.src) == 0 {
		return 0, io.EOF
	}
	n := c.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.src) {
		n = len(c.src)
	}
	copy(p, c.src[:n])
	c.src = c.src[n:]
	return n, nil
}
func (c *chunkReader) Close() error { return nil }

func benchStream(events int, sep string) []byte {
	var b strings.Builder
	for i := 0; i < events; i++ {
		b.WriteString(`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hello"}}]}` + sep)
	}
	b.WriteString("data: [DONE]" + sep)
	return []byte(b.String())
}

func benchTee(b *testing.B, stream []byte, readSize int) {
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	buf := make([]byte, 4096)
	for i := 0; i < b.N; i++ {
		n := 0
		tee := newSSETee(&chunkReader{src: stream, size: readSize}, func(string, []byte) { n++ }, nil, func() {})
		for {
			_, err := tee.Read(buf)
			if err != nil {
				break
			}
		}
		_ = n
	}
}

// 1000 small events, ~100B each, delivered in 512B reads.
func BenchmarkSSETeeSmallEvents(b *testing.B) { benchTee(b, benchStream(1000, "\n\n"), 512) }

// Same, CRLF framing.
func BenchmarkSSETeeSmallEventsCRLF(b *testing.B) { benchTee(b, benchStream(1000, "\r\n\r\n"), 512) }

// One 256 KiB event arriving in 1 KiB reads: exposes quadratic rescanning.
func BenchmarkSSETeeLargeEventSmallReads(b *testing.B) {
	big := []byte("data: " + strings.Repeat("a", 256*1024) + "\n\n")
	benchTee(b, big, 1024)
}

func BenchmarkFindFrameBoundary(b *testing.B) {
	raw := bytes.Repeat([]byte("a"), 4096)
	raw = append(raw, "\n\n"...)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		findFrameBoundary(raw)
	}
}
