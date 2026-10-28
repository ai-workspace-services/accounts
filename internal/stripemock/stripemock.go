// Package stripemock is a minimal, fixture-driven stand-in for the Stripe REST
// API used by accounts tests and egress-denied lanes (STRIPE_API_BASE_URL).
// It never talks to Stripe; it serves canned JSON objects keyed by request path
// and only accepts test-mode bearer keys.
package stripemock

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Server serves fixtures keyed by URL path, for example "/v1/subscriptions/sub_1".
type Server struct {
	mu       sync.RWMutex
	fixtures map[string]json.RawMessage
	requests []string
}

// New returns a Server with the given fixtures (may be nil).
func New(fixtures map[string]json.RawMessage) *Server {
	s := &Server{fixtures: map[string]json.RawMessage{}}
	for k, v := range fixtures {
		s.fixtures[normalize(k)] = v
	}
	return s
}

// LoadFile reads a JSON object mapping paths to Stripe objects.
func LoadFile(path string) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	return out, json.Unmarshal(raw, &out)
}

// Set adds or replaces one fixture.
func (s *Server) Set(path string, object any) error {
	raw, err := json.Marshal(object)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.fixtures[normalize(path)] = raw
	s.mu.Unlock()
	return nil
}

// Requests returns "METHOD /path" for every request served.
func (s *Server) Requests() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.requests...)
}

// normalize canonicalises a fixture key: an optional "METHOD " prefix followed
// by a path that is forced under /v1.
func normalize(key string) string {
	key = strings.TrimSpace(key)
	if method, rest, ok := strings.Cut(key, " "); ok && method == strings.ToUpper(method) && !strings.Contains(method, "/") {
		return method + " " + normalizePath(rest)
	}
	return normalizePath(key)
}

func normalizePath(p string) string {
	p = "/" + strings.Trim(strings.TrimSpace(p), "/")
	if !strings.HasPrefix(p, "/v1/") && p != "/v1" {
		p = "/v1" + p
	}
	return p
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(auth, "sk_test_") && !strings.HasPrefix(auth, "rk_test_") {
		writeError(w, http.StatusUnauthorized, "authentication_error", "stripemock accepts test-mode keys only")
		return
	}
	path := normalizePath(r.URL.Path)
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+path)
	body, ok := s.fixtures[r.Method+" "+path]
	if !ok && r.Method == http.MethodGet {
		body, ok = s.fixtures[path]
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "No such fixture: "+r.Method+" "+path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
