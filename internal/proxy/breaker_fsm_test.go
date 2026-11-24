package proxy

import (
	"testing"
	"time"

	"github.com/llmspan/llmspan/internal/config"
)

// breaker FSM tests use injected clocks — no time.Sleep.

func TestBreakerFSMOpensAfterNFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newBreaker(config.BreakerConfig{Failures: 3, Window: time.Minute, RecoveryWindow: 15 * time.Second})
	b.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		ok, _ := b.admit()
		if !ok {
			t.Fatalf("admit %d: breaker closed", i)
		}
		b.report(500)
	}
	ok, retry := b.admit()
	if ok {
		t.Fatal("breaker should be open after 3 failures")
	}
	if retry < 1 {
		t.Fatalf("retry-after = %d", retry)
	}
}

func TestBreakerFSM4xxResetsConsecutive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newBreaker(config.BreakerConfig{Failures: 3, Window: time.Minute, RecoveryWindow: 15 * time.Second})
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		ok, _ := b.admit()
		if !ok {
			t.Fatal("admit failed")
		}
		b.report(500)
	}
	ok, _ := b.admit()
	if !ok {
		t.Fatal("should still admit before threshold")
	}
	b.report(400) // upstream rejected request — not a breaker failure
	ok, _ = b.admit()
	if !ok {
		t.Fatal("should admit after 4xx reset")
	}
	b.report(500)
	b.report(500)
	b.report(500)
	ok, _ = b.admit()
	if ok {
		t.Fatal("should open after fresh run of 3")
	}
}

func TestBreakerFSMHalfOpenProbeSuccessCloses(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := start
	b := newBreaker(config.BreakerConfig{Failures: 1, RecoveryWindow: 10 * time.Second})
	b.now = func() time.Time { return tick }

	if ok, _ := b.admit(); !ok {
		t.Fatal("admit")
	}
	b.report(500)
	if ok, _ := b.admit(); ok {
		t.Fatal("open")
	}

	tick = start.Add(11 * time.Second)
	if ok, _ := b.admit(); !ok {
		t.Fatal("half-open probe should admit")
	}
	b.report(200)
	if ok, _ := b.admit(); !ok {
		t.Fatal("closed after successful probe")
	}
}

func TestBreakerFSMWindowBreaksConsecutiveRun(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tick := start
	b := newBreaker(config.BreakerConfig{Failures: 3, Window: 5 * time.Second, RecoveryWindow: 10 * time.Second})
	b.now = func() time.Time { return tick }

	for i := 0; i < 2; i++ {
		ok, _ := b.admit()
		if !ok {
			t.Fatal("admit")
		}
		b.report(500)
	}
	tick = start.Add(6 * time.Second)
	ok, _ := b.admit()
	if !ok {
		t.Fatal("admit")
	}
	b.report(500)
	ok, _ = b.admit()
	if !ok {
		t.Fatal("only one failure in window — should stay closed")
	}
}

func TestBreakerFSMClientCloseIsNeutral(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newBreaker(config.BreakerConfig{Failures: 3, Window: time.Minute, RecoveryWindow: 10 * time.Second})
	b.now = func() time.Time { return now }

	// 499 neither counts nor resets the run: 500, 499, 500, 500 trips.
	for _, st := range []int{500, 499, 500} {
		b.admit()
		b.report(st)
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("open too early")
	}
	b.report(500)
	if ok, _ := b.admit(); ok {
		t.Fatal("499 must not reset the consecutive count")
	}

	// Half-open probe cancelled by the client: slot freed, still half-open.
	now = now.Add(11 * time.Second)
	if ok, _ := b.admit(); !ok {
		t.Fatal("probe should be admitted")
	}
	b.report(499)
	if b.state != breakerHalfOpen {
		t.Fatalf("state = %v, want half-open after cancelled probe", b.state)
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("a new probe must be admitted after the cancelled one")
	}
	b.report(200)
	if b.state != breakerClosed {
		t.Fatalf("state = %v, want closed", b.state)
	}
}

func TestBreakerFSMOverloadedAndRateLimit(t *testing.T) {
	t.Parallel()
	b := newBreaker(config.BreakerConfig{Failures: 2, RecoveryWindow: time.Second})
	for i := 0; i < 5; i++ {
		b.admit()
		b.report(529)
		b.admit()
		b.report(429)
	}
	if ok, _ := b.admit(); !ok {
		t.Fatal("429/529 must not trip the breaker by default")
	}
}
