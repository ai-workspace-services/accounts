package auth

import (
	"sync"
	"time"
)

// RateLimiter is a minimal in-process, fixed-window counter.
//
// It exists to blunt account enumeration: register/send, register, and
// login each tell an anonymous caller whether an email is already
// registered (409 email_already_exists / 404 user_not_found), and none of
// them were bounded, so that signal was free to harvest at whatever rate a
// caller chose. This does not hide the signal -- the product still tells a
// genuine user "you already have an account" -- it makes harvesting it
// slow.
//
// State is per-process, matching this package's other ephemeral maps
// (mfaChallenges, verifications, passwordResets in api.handler): fine for a
// single Cloud Run instance, and reset on redeploy or scale-out. That is a
// real gap under horizontal scaling, not a claim of a hard limit -- treat
// this as raising the cost of enumeration, not eliminating it.
type RateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]rateWindow
}

type rateWindow struct {
	count int
	endAt time.Time
}

// NewRateLimiter returns a limiter allowing up to limit calls per key within
// window. A non-positive limit or window disables limiting (Allow always
// returns true) so a misconfigured deployment fails open into today's
// behavior rather than locking everyone out.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:  limit,
		window: window,
		hits:   make(map[string]rateWindow),
	}
}

// Allow reports whether key may proceed, consuming one unit of its budget
// when it does. Callers share one limiter across every key they check
// (e.g. one instance per IP, another per email) so windows are isolated per
// key, not per handler call.
func (r *RateLimiter) Allow(key string) bool {
	if r == nil || r.limit <= 0 || r.window <= 0 || key == "" {
		return true
	}

	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Opportunistically drop a few expired entries so a limiter fed a wide
	// spread of keys (many distinct IPs or emails) doesn't grow forever.
	// This is a cheap, best-effort sweep -- not a guarantee -- consistent
	// with the lazy-expiry style already used for verification codes and
	// password reset tokens elsewhere in this package.
	if len(r.hits) > 4096 {
		swept := 0
		for k, w := range r.hits {
			if now.After(w.endAt) {
				delete(r.hits, k)
				swept++
				if swept >= 512 {
					break
				}
			}
		}
	}

	w, ok := r.hits[key]
	if !ok || now.After(w.endAt) {
		r.hits[key] = rateWindow{count: 1, endAt: now.Add(r.window)}
		return true
	}
	if w.count >= r.limit {
		return false
	}
	w.count++
	r.hits[key] = w
	return true
}
