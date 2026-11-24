package provider

import (
	"bytes"
	"io"
	"sync"
)

// maxEventBytes caps the in-flight SSE-parse buffer per stream. Real
// provider events are kilobytes at most; anything bigger is a sign of
// an upstream stuck in a non-SSE state (or actively trying to OOM the
// proxy). On overflow the buffer is dropped and onOverflow fires; bytes
// keep forwarding to the client unchanged — only the parser gives up.
const maxEventBytes = 1024 * 1024 // 1 MiB

// sseTee wraps an upstream io.ReadCloser, forwarding bytes unchanged to the
// caller while parsing SSE messages out-of-band. It guarantees onClose runs
// exactly once: whichever of EOF, transport error, or Close arrives first.
//
// Why a tee rather than a goroutine + io.Pipe: the proxy already runs in the
// request goroutine; staying single-threaded avoids a goroutine per in-flight
// request and removes a class of leak.
type sseTee struct {
	src     io.ReadCloser
	buf     bytes.Buffer
	onEvent func(event string, data []byte)
	// onOverflow fires every time the parser buffer crosses
	// maxEventBytes. A one-shot signal hides sustained pathological
	// streams (a 100 MiB no-separator payload looked identical to a
	// 2 MiB one); firing each ~1 MiB chunk gives operators a real
	// "this stream is still pathological" signal without flooding —
	// the cap is the overflow rate, not the call count. Callers
	// dedupe at the span/metric layer if they need to.
	onOverflow func()
	onClose    func()
	scanFrom   int // resume offset for the frame-boundary scan
	once       sync.Once
	overflows  int // total overflow events on this stream; exported via Overflows().
}

func newSSETee(src io.ReadCloser, onEvent func(event string, data []byte), onOverflow, onClose func()) *sseTee {
	return &sseTee{src: src, onEvent: onEvent, onOverflow: onOverflow, onClose: onClose}
}

func (t *sseTee) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		_, _ = t.buf.Write(p[:n])
		if t.buf.Len() > maxEventBytes {
			// Pathological / hostile stream — drop the accumulator
			// and announce overflow. Bytes still flow to the client
			// via the n we return; only the SSE parser gives up on
			// this chunk. Firing per-overflow (vs. once-per-stream)
			// surfaces sustained hostile streams to operators.
			t.buf.Reset()
			t.scanFrom = 0
			t.overflows++
			if t.onOverflow != nil {
				t.onOverflow()
			}
		} else {
			t.drain(false)
		}
	}
	if err != nil {
		t.drain(true)
		t.once.Do(t.onClose)
	}
	return n, err
}

func (t *sseTee) Close() error {
	t.once.Do(t.onClose)
	return t.src.Close()
}

// Overflows returns the total number of buffer-overflow events on this
// stream. Useful in onClose to record a single span attribute summarising
// "this stream was pathological N times" rather than emitting N span
// events. Always safe to call after the stream is closed; reads on an
// in-flight stream race with Read and should be avoided.
func (t *sseTee) Overflows() int { return t.overflows }

// drain extracts complete SSE messages (delimited by a blank line) from the
// buffer. When eof is true, any trailing message without a terminating blank
// line is also flushed.
//
// Per the HTML5 EventSource spec (§9.2 "Parsing an event stream") a blank
// line may be `\n\n`, `\r\n\r\n`, or `\r\r` — any of the three sequences is
// a valid frame terminator. OpenAI / Anthropic both ship `\n\n` today, but a
// CRLF-rewriting intermediary (Cloudflare in some modes, Squid, certain
// corporate egress proxies) can rewrite to `\r\n\r\n` mid-flight. Recognize
// all three to avoid a silent "no events ever parsed" degrade.
//
// The scan is a single pass that resumes at t.scanFrom (the tail of the
// previous scan) so a large event arriving in many small reads is scanned
// once overall, not once per read. dispatch consumes the frame in place:
// it never retains the slice and the callbacks cannot touch t.buf.
func (t *sseTee) drain(eof bool) {
	for {
		raw := t.buf.Bytes()
		idx, sep := findFrameBoundaryFrom(raw, t.scanFrom)
		if idx < 0 {
			if !eof || t.buf.Len() == 0 {
				// Terminators are at most 4 bytes, so a partial one
				// can start no earlier than len-3.
				t.scanFrom = len(raw) - 3
				if t.scanFrom < 0 {
					t.scanFrom = 0
				}
				return
			}
			t.dispatch(raw)
			t.buf.Reset()
			t.scanFrom = 0
			return
		}
		t.dispatch(raw[:idx])
		t.buf.Next(idx + sep)
		t.scanFrom = 0
	}
}

// findFrameBoundary returns the offset of the first frame terminator and
// the length of the terminator (in bytes). Returns -1 if no terminator is
// present. Recognized terminators:
//
//	\r\n\r\n  (4 bytes)
//	\n\n      (2 bytes)
//	\r\r      (2 bytes)
//
// The earliest match wins; the three cannot start at the same offset, so
// there is no tie to break.
func findFrameBoundary(raw []byte) (idx, sepLen int) {
	return findFrameBoundaryFrom(raw, 0)
}

// findFrameBoundaryFrom is findFrameBoundary starting the scan at offset
// from. Allocation-free.
func findFrameBoundaryFrom(raw []byte, from int) (idx, sepLen int) {
	n := len(raw)
	// nextLF / nextCR cache the position of the next '\n' / '\r' at or
	// after i (n when none), so each byte is examined by the vectorised
	// IndexByte at most once per call even when one of the two bytes is
	// absent from the buffer.
	nextLF, nextCR := -1, -1
	for i := from; i < n; {
		if nextLF < i {
			if j := bytes.IndexByte(raw[i:], '\n'); j >= 0 {
				nextLF = i + j
			} else {
				nextLF = n
			}
		}
		if nextCR < i {
			if j := bytes.IndexByte(raw[i:], '\r'); j >= 0 {
				nextCR = i + j
			} else {
				nextCR = n
			}
		}
		i = nextLF
		if nextCR < i {
			i = nextCR
		}
		if i >= n {
			return -1, 0
		}
		if raw[i] == '\n' {
			if i+1 < n && raw[i+1] == '\n' {
				return i, 2
			}
		} else { // '\r'
			if i+1 >= n {
				return -1, 0
			}
			switch raw[i+1] {
			case '\r':
				return i, 2
			case '\n':
				if i+3 < n && raw[i+2] == '\r' && raw[i+3] == '\n' {
					return i, 4
				}
			}
		}
		i++
	}
	return -1, 0
}

var (
	sseEventPrefix = []byte("event:")
	sseDataPrefix  = []byte("data:")
)

// dispatch parses one frame body and fires onEvent. Lines are split on any
// of `\r\n`, `\n`, or `\r` (HTML5 EventSource line terminators) without
// allocating a line slice. The data slice handed to onEvent is freshly
// allocated, so handlers may retain it.
func (t *sseTee) dispatch(msg []byte) {
	var (
		event string
		data  []byte
	)
	start := 0
	for i := 0; i <= len(msg); i++ {
		if i < len(msg) && msg[i] != '\n' && msg[i] != '\r' {
			continue
		}
		line := msg[start:i]
		// CRLF counts as one terminator.
		if i < len(msg) && msg[i] == '\r' && i+1 < len(msg) && msg[i+1] == '\n' {
			i++
		}
		start = i + 1
		switch {
		case len(line) == 0, line[0] == ':':
			// blank line / comment — ignore.
		case bytes.HasPrefix(line, sseEventPrefix):
			event = string(bytes.TrimSpace(line[len(sseEventPrefix):]))
		case bytes.HasPrefix(line, sseDataPrefix):
			data = append(data, bytes.TrimSpace(line[len(sseDataPrefix):])...)
		}
	}
	if len(data) == 0 && event == "" {
		return
	}
	t.onEvent(event, data)
}
