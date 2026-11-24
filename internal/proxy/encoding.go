package proxy

import (
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Content-encoding policy
//
// llmspan never changes the bytes it relays. The client's Accept-Encoding
// is forwarded to the upstream (see narrowAcceptEncoding) and whatever
// the upstream returns, compressed or not, reaches the client verbatim
// with its original Content-Encoding and Content-Length. The upstream
// Transport runs with DisableCompression so it neither injects its own
// Accept-Encoding nor transparently decodes.
//
// Telemetry still needs plaintext to read token usage. For gzip and
// deflate responses (the encodings the standard library can decode) the
// raw stream is split: the client reads the raw bytes, and the same
// bytes are fed through a decoder into the observer's normal tee/parse
// pipeline, which therefore sees plaintext exactly as before. Parsing is
// best-effort: a corrupt or unsupported stream stops observation but
// never affects what the client receives. Encodings llmspan cannot decode
// (br, zstd) are relayed untouched and recorded on the span as
// llmspan.response.undecoded_encoding; their token/cost metrics are zero.

// narrowAcceptEncoding rewrites a client Accept-Encoding value so the
// upstream is only offered encodings llmspan can decode for telemetry
// (gzip, deflate), keeping the client's own tokens and q-values. If the
// client offered nothing we can decode, the header is returned unchanged
// (true passthrough; telemetry for such responses is skipped). A bare
// wildcard is narrowed to gzip, which any client that sent `*` accepts.
func narrowAcceptEncoding(v string) string {
	var kept []string
	wildcard := false
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(tok, ";", 2)[0]))
		switch name {
		case "gzip", "x-gzip", "deflate", "identity":
			kept = append(kept, tok)
		case "*":
			wildcard = true
		}
	}
	supported := false
	for _, k := range kept {
		n := strings.ToLower(strings.TrimSpace(strings.SplitN(k, ";", 2)[0]))
		if n != "identity" {
			supported = true
		}
	}
	if !supported && wildcard {
		kept = append(kept, "gzip")
		supported = true
	}
	if !supported {
		return v
	}
	return strings.Join(kept, ", ")
}

// observeDecoded invokes observe (the observer's ModifyResponse) with a
// plaintext view of the body when the response is gzip/deflate encoded,
// while arranging for the client to receive the original bytes.
func observeDecoded(resp *http.Response, observe func(*http.Response) error) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" || resp.Body == nil || resp.Body == http.NoBody {
		return observe(resp)
	}
	switch enc {
	case "gzip", "x-gzip", "deflate":
	default:
		if span := trace.SpanFromContext(resp.Request.Context()); span.IsRecording() {
			span.SetAttributes(attribute.String("llmspan.response.undecoded_encoding", enc))
		}
		return observe(resp)
	}

	raw := resp.Body
	pr, pw := io.Pipe()
	resp.Body = &decodedBody{src: pr, raw: raw, enc: enc}
	if err := observe(resp); err != nil {
		_ = pr.Close()
		return err
	}
	observed := resp.Body
	t := &rawTee{raw: raw, pw: pw, pr: pr, observed: observed, done: make(chan struct{})}
	go t.drain()
	resp.Body = t
	return nil
}

// decodedBody is the plaintext side handed to the observer. The decoder
// is constructed on first Read (gzip reads its header eagerly, which must
// happen on the drain goroutine, not inside ModifyResponse).
type decodedBody struct {
	src  *io.PipeReader
	raw  io.Closer
	enc  string
	once sync.Once
	dec  io.Reader
	err  error
}

func (d *decodedBody) Read(p []byte) (int, error) {
	d.once.Do(func() {
		if d.enc == "deflate" {
			d.dec, d.err = zlib.NewReader(d.src)
		} else {
			d.dec, d.err = gzip.NewReader(d.src)
		}
	})
	if d.err != nil {
		return 0, d.err
	}
	return d.dec.Read(p)
}

// Close unblocks any pending decoder read and the upstream body (used by
// the observer's idle watchdog).
func (d *decodedBody) Close() error {
	_ = d.src.Close()
	return d.raw.Close()
}

// rawTee is the client-facing body: it relays upstream bytes unchanged
// and mirrors them into the decode pipeline.
type rawTee struct {
	raw      io.ReadCloser
	pw       *io.PipeWriter
	pr       *io.PipeReader
	observed io.ReadCloser // observer-wrapped plaintext body
	done     chan struct{}
	dead     bool // pipe closed by the drain side; stop mirroring
	closed   sync.Once
}

// drain pulls the observer's wrapped plaintext body to completion so its
// parse-on-EOF / stream-wrapper logic runs. Closing pr on exit makes
// any later mirror writes fail fast instead of blocking.
func (t *rawTee) drain() {
	defer close(t.done)
	_, _ = io.Copy(io.Discard, t.observed)
	_ = t.pr.Close()
}

func (t *rawTee) Read(p []byte) (int, error) {
	n, err := t.raw.Read(p)
	if n > 0 && !t.dead {
		if _, werr := t.pw.Write(p[:n]); werr != nil {
			t.dead = true
		}
	}
	if err != nil {
		if err == io.EOF {
			_ = t.pw.Close()
		} else {
			_ = t.pw.CloseWithError(err)
		}
		// Let the observer finish (parse + Finalize) before the client
		// sees EOF, preserving "span attributes are set before the last
		// byte hits the wire".
		<-t.done
	}
	return n, err
}

func (t *rawTee) Close() error {
	var err error
	t.closed.Do(func() {
		err = t.raw.Close()
		_ = t.pw.CloseWithError(io.ErrUnexpectedEOF)
		<-t.done
		_ = t.observed.Close()
	})
	return err
}
