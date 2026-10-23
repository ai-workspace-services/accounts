package migrate

import (
	"bytes"
	"fmt"
	"github.com/google/uuid"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMigrationAPIRateLimit(t *testing.T) {
	var limiter migrationRateLimiter
	now := time.Now()
	for i := 0; i < 120; i++ {
		if !limiter.allow("job", "subject", now) {
			t.Fatal("burst rejected")
		}
	}
	if limiter.allow("job", "subject", now) {
		t.Fatal("burst exceeded")
	}
	if !limiter.allow("job", "subject", now.Add(time.Second)) {
		t.Fatal("tokens not replenished")
	}
	if !limiter.allow("job", "other", now) {
		t.Fatal("subjects share bucket")
	}
	for i := 0; i < 1100; i++ {
		limiter.allow("job", fmt.Sprint(i), now)
	}
	if len(limiter.buckets) > 1024 {
		t.Fatal("unbounded bucket map")
	}
	if !limiter.allow("job", "fresh", now.Add(2*time.Minute)) {
		t.Fatal("stale buckets not reclaimed")
	}
	key := bytes.Repeat([]byte{9}, 32)
	job := uuid.NewString()
	s := &migrationServer{MigrationServerOptions: MigrationServerOptions{Role: "source", Audience: "source", SourceAudience: "source", TargetAudience: "target", JobID: job, AuthKey: key}}
	for i := 0; i < 120; i++ {
		s.rate.allow(job, "operator", now.Add(time.Minute))
	}
	token, err := MintMigrationToken(key, "source", "operator", []string{"source.read"}, time.Minute, MigrationAuthorization{SourceAudience: "source", TargetAudience: "target", JobID: job})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", MigrationAPIBase+"/snapshots", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.serve(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatal("rate response contract")
	}
}
