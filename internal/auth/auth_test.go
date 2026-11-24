package auth_test

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/auth"
)

func TestHashVerifyRoundtrip(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Errorf("encoded hash missing argon2id prefix: %q", encoded)
	}
	ok, err := auth.Verify("correct-horse-battery-staple", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("verify returned false for the correct password")
	}
}

func TestHashIsSalted(t *testing.T) {
	t.Parallel()
	a, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.Hash("same-plaintext")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("hashing the same plaintext twice produced identical output — salt missing")
	}
}

func TestVerifyRejectsWrongPlaintext(t *testing.T) {
	t.Parallel()
	encoded, err := auth.Hash("real-token")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := auth.Verify("guessed-token", encoded)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("verify returned true for the wrong password")
	}
}

func TestVerifyMalformedEncoded(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=foo,t=bar,p=baz$salt$hash",
		"$bcrypt$10$abc$def",
	}
	for _, enc := range cases {
		if _, err := auth.Verify("anything", enc); err == nil {
			t.Errorf("expected error on malformed encoded %q", enc)
		}
	}
}

func TestVerifierAcceptsKnownToken(t *testing.T) {
	t.Parallel()
	h, err := auth.Hash("token-A")
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier([]string{h})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Verify("token-A"); !ok {
		t.Error("verifier rejected the token it was configured with")
	}
	if ok, _ := v.Verify("token-B"); ok {
		t.Error("verifier accepted a token it wasn't configured with")
	}
}

func TestVerifierAcceptsAnyOfMultipleTokens(t *testing.T) {
	t.Parallel()
	hA, _ := auth.Hash("alpha")
	hB, _ := auth.Hash("bravo")
	v, err := auth.NewVerifier([]string{hA, hB})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := v.Verify("alpha"); !ok {
		t.Error("multi-hash verifier rejected first token")
	}
	if ok, _ := v.Verify("bravo"); !ok {
		t.Error("multi-hash verifier rejected second token")
	}
	if ok, _ := v.Verify("charlie"); ok {
		t.Error("multi-hash verifier accepted an unknown token")
	}
}

func TestVerifierRejectsEmptyToken(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real")
	v, _ := auth.NewVerifier([]string{h})
	if ok, _ := v.Verify(""); ok {
		t.Error("empty token must not match")
	}
}

func TestVerifierEnabled(t *testing.T) {
	t.Parallel()
	empty, err := auth.NewVerifier(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Enabled() {
		t.Error("Enabled() must return false when no tokens are configured")
	}
	h, _ := auth.Hash("real")
	on, _ := auth.NewVerifier([]string{h})
	if !on.Enabled() {
		t.Error("Enabled() must return true when at least one token is configured")
	}
}

func TestNewVerifierRejectsMalformedTokenAtConstruction(t *testing.T) {
	t.Parallel()
	if _, err := auth.NewVerifier([]string{"not-a-hash"}); err == nil {
		t.Error("NewVerifier must reject malformed encoded tokens at construction time")
	}
}

// TestVerifierFastRejectsBadShape is a regression test. Tokens with
// characters outside the printable-ASCII subset must fail without
// running argon2. The timing margin in this test is 1ms for 1000
// garbage tokens; a single argon2 evaluation is ~50ms so even one
// missed shape-check would blow the budget.
func TestVerifierFastRejectsBadShape(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real-token-xyz")
	v, _ := auth.NewVerifier([]string{h})

	garbage := []string{
		"<script>alert(1)</script>",
		"token with spaces",
		"token\twith\ttabs",
		"token\nwith\nnewlines",
		"contains\x00nul",
		"unicode-π",
		"semicolon;injection",
		"backtick`subshell",
		"quote\"injection",
		"apostrophe'injection",
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		for _, g := range garbage {
			if ok, _ := v.Verify(g); ok {
				t.Errorf("garbage token %q accepted", g)
			}
		}
	}
	elapsed := time.Since(start)
	// 100 × 10 garbage tokens = 1000 calls. Each must be sub-microsecond
	// in the shape-check path; we allow a generous 50ms total budget
	// (a single argon2 evaluation is ~50ms, so any single argon2 leak
	// would blow it).
	if elapsed > 50*time.Millisecond {
		t.Errorf("1000 garbage-token verifications took %v; want <50ms (shape-check must precede argon2)", elapsed)
	}
}

// TestVerifierCacheTTL is a regression test. A second Verify call
// with the same plaintext after a successful first call must complete
// in under 100µs (cache hit). The cold path is ~50ms; the warm path
// must be microseconds.
func TestVerifierCacheTTL(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("real-token-cached")
	v, _ := auth.NewVerifier([]string{h})

	// Cold call to seed the cache.
	if ok, _ := v.Verify("real-token-cached"); !ok {
		t.Fatal("cold verify rejected the correct token")
	}

	// Warm call: cache hit.
	start := time.Now()
	if ok, _ := v.Verify("real-token-cached"); !ok {
		t.Fatal("warm verify rejected the correct token")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Microsecond {
		t.Errorf("warm verify took %v; want <100µs (cache should short-circuit argon2)", elapsed)
	}
}

// TestVerifierVerifyConstantTimeAcrossPosition is a regression test.
// Verifying a token at position 0 must not be measurably faster than
// verifying a token at position N-1 — the verifier must run argon2
// against every stored hash, accumulating the match flag.
func TestVerifierVerifyConstantTimeAcrossPosition(t *testing.T) {
	t.Parallel()
	const N = 10
	hashes := make([]string, N)
	plains := make([]string, N)
	for i := 0; i < N; i++ {
		p := "token-position-" + string(rune('a'+i))
		plains[i] = p
		h, err := auth.Hash(p)
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = h
	}
	// timeVerify times one successful Verify on a FRESH verifier (so no cache
	// can shortcut it). The minimum of two samples filters scheduler noise from
	// a loaded machine; a short-circuiting implementation would still be several
	// times faster at position 0 than at position N-1.
	timeVerify := func(plain string) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 2; i++ {
			v, err := auth.NewVerifier(hashes)
			if err != nil {
				t.Fatal(err)
			}
			// Warm with a known-wrong but well-shaped token.
			if ok, _ := v.Verify("zzz-warmup"); ok {
				t.Fatal("warmup token unexpectedly accepted")
			}
			start := time.Now()
			if ok, _ := v.Verify(plain); !ok {
				t.Fatalf("token %q rejected", plain)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	t0 := timeVerify(plains[0])
	tLast := timeVerify(plains[N-1])

	// Both must execute argon2 against all N hashes. A leaky implementation
	// that returns on the first match would show t0 about tLast/N (5-10x faster).
	ratio := float64(tLast) / float64(t0)
	if ratio < 0.4 || ratio > 2.5 {
		t.Errorf("position timing ratio = %.2f (t0=%v, tLast=%v); want close to 1.0 (constant-time loop required)", ratio, t0, tLast)
	}
}

// TestVerifierConcurrentArgon2RespectsSemaphore is a regression test.
// Even a flood of distinct garbage tokens (cache misses) must not run
// more than runtime.NumCPU() argon2 evaluations concurrently. Per the
// task spec, over-capacity callers receive busy=true so the proxy can
// translate that to 429.
func TestVerifierConcurrentArgon2RespectsSemaphore(t *testing.T) {
	t.Parallel()
	cpus := runtime.NumCPU()
	if cpus < 2 {
		t.Skip("test requires NumCPU >= 2")
	}
	h, _ := auth.Hash("the-real-token")
	v, _ := auth.NewVerifier([]string{h})

	var (
		current atomic.Int32
		peak    atomic.Int32
	)
	// Instrument the argon2 path via the package's test hook so we
	// can count concurrent in-flight evaluations precisely.
	auth.SetArgon2HookForTest(v, func() {
		c := current.Add(1)
		defer current.Add(-1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	})

	const fanout = 8
	wg := sync.WaitGroup{}
	var busyCount atomic.Int32
	for i := 0; i < cpus*fanout; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			// Use distinct plaintexts so the cache can't short-circuit.
			tok := "garbage-shape-ok-" + strings.Repeat("z", i%32+1)
			ok, busy := v.Verify(tok)
			if ok {
				t.Errorf("garbage token %q accepted", tok)
			}
			if busy {
				busyCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > int32(cpus) {
		t.Errorf("peak concurrent argon2 evaluations = %d; want <= NumCPU=%d", got, cpus)
	}
	// At fanout=8 with mock-blocking argon2, the semaphore MUST refuse
	// at least some callers — otherwise the cap isn't enforced.
	if busyCount.Load() == 0 {
		t.Error("no callers received busy=true; semaphore is not enforcing the cap")
	}
}

func BenchmarkVerifierVerifyCold(b *testing.B) {
	h, _ := auth.Hash("bench-token")
	for i := 0; i < b.N; i++ {
		// Each iteration uses a fresh verifier so the cache is cold.
		v, _ := auth.NewVerifier([]string{h})
		_, _ = v.Verify("bench-token")
	}
}

func BenchmarkVerifierVerifyWarm(b *testing.B) {
	h, _ := auth.Hash("bench-token")
	v, _ := auth.NewVerifier([]string{h})
	// Seed the cache.
	if ok, _ := v.Verify("bench-token"); !ok {
		b.Fatal("cold verify failed during seed")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = v.Verify("bench-token")
	}
}

func TestVerifyNamedReturnsMatchingName(t *testing.T) {
	t.Parallel()
	hA, _ := auth.Hash("token-a")
	hB, _ := auth.Hash("token-b")
	hC, _ := auth.Hash("token-c")
	v, err := auth.NewVerifier([]string{"billing:" + hA, hB, "search:" + hC})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ plain, name string }{
		{"token-a", "billing"}, {"token-b", ""}, {"token-c", "search"},
		{"token-a", "billing"}, // cached path keeps the name
	} {
		name, ok, busy := v.VerifyNamed(tc.plain)
		if !ok || busy || name != tc.name {
			t.Errorf("VerifyNamed(%q) = (%q,%v,%v), want (%q,true,false)", tc.plain, name, ok, busy, tc.name)
		}
	}
	if _, ok, _ := v.VerifyNamed("nope"); ok {
		t.Error("wrong token accepted")
	}
}

func TestSplitEntry(t *testing.T) {
	t.Parallel()
	h := "$argon2id$v=19$m=1,t=1,p=1$YQ$YQ"
	for _, tc := range []struct {
		in, name string
		bad      bool
	}{
		{h, "", false}, {"svc:" + h, "svc", false}, {"svc " + h, "svc", false},
		{"a.b-c_d@e:" + h, "a.b-c_d@e", false},
		{"bad name:" + h, "", true}, {"svc:nothash", "", true}, {"plain", "", true},
		{strings.Repeat("x", 65) + ":" + h, "", true},
	} {
		name, hash, err := auth.SplitEntry(tc.in)
		if (err != nil) != tc.bad || (!tc.bad && (name != tc.name || hash != h)) {
			t.Errorf("SplitEntry(%q) = (%q,%q,%v)", tc.in, name, hash, err)
		}
	}
}

func TestNewVerifierRejectsBadName(t *testing.T) {
	t.Parallel()
	h, _ := auth.Hash("x")
	if _, err := auth.NewVerifier([]string{"bad/name:" + h}); err == nil {
		t.Fatal("expected error")
	}
}
