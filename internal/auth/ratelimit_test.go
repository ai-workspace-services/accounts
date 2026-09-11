package auth

import (
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToLimitThenDenies(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("call %d: expected allow within budget", i+1)
		}
	}

	if rl.Allow("k") {
		t.Fatal("expected the 4th call in the window to be denied")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute)

	if !rl.Allow("a") {
		t.Fatal("expected first call for key a to be allowed")
	}
	if rl.Allow("a") {
		t.Fatal("expected second call for key a to be denied")
	}
	// A different key must not be affected by key a's exhausted budget --
	// otherwise a single busy account (or IP) would lock out every other
	// caller sharing the limiter instance.
	if !rl.Allow("b") {
		t.Fatal("expected key b to have its own budget")
	}
}

func TestRateLimiterWindowResets(t *testing.T) {
	rl := NewRateLimiter(1, 20*time.Millisecond)

	if !rl.Allow("k") {
		t.Fatal("expected first call to be allowed")
	}
	if rl.Allow("k") {
		t.Fatal("expected second call within the window to be denied")
	}

	time.Sleep(30 * time.Millisecond)

	if !rl.Allow("k") {
		t.Fatal("expected a call after the window elapsed to be allowed again")
	}
}

func TestRateLimiterDisabledFailsOpen(t *testing.T) {
	// A misconfigured (zero/negative) limiter must not lock everyone out --
	// it should behave as if rate limiting were off.
	cases := []*RateLimiter{
		NewRateLimiter(0, time.Minute),
		NewRateLimiter(5, 0),
		nil,
	}
	for i, rl := range cases {
		for call := 0; call < 10; call++ {
			if !rl.Allow("k") {
				t.Fatalf("case %d: expected disabled limiter to always allow (call %d)", i, call)
			}
		}
	}
}

func TestRateLimiterEmptyKeyAlwaysAllowed(t *testing.T) {
	// An empty key (e.g. an unresolvable client IP) must not be silently
	// pooled into one shared bucket for every such caller.
	rl := NewRateLimiter(1, time.Minute)
	for call := 0; call < 5; call++ {
		if !rl.Allow("") {
			t.Fatalf("expected empty key to always be allowed (call %d)", call)
		}
	}
}
