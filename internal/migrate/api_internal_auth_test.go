package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestIndependentInternalMigrationAuthorization(t *testing.T) {
	if _, err := NewInternalServiceMigrationAuthorizer(""); err == nil {
		t.Fatal("empty credential accepted")
	}
	auth, err := NewInternalServiceMigrationAuthorizer("fixture-internal-token")
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte("accounts-migration-internal-service"))
	for _, scope := range []string{"source.read", "target.plan", "target.apply"} {
		id, status := auth(context.Background(), "fixture-internal-token", scope)
		if status != 0 || id != hex.EncodeToString(expected[:]) {
			t.Fatal("existing identity binding differs")
		}
	}
	for _, token := range []string{"", "wrong"} {
		id, status := auth(context.Background(), token, "target.apply")
		if status != http.StatusUnauthorized || id != "" {
			t.Fatal("unauthorized credential accepted")
		}
	}
	if id, status := auth(context.Background(), "fixture-internal-token", "admin"); id != "" || status != http.StatusForbidden {
		t.Fatal("unknown scope accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, status := auth(ctx, "fixture-internal-token", "target.plan"); status != http.StatusServiceUnavailable {
		t.Fatal("cancelled authorization accepted")
	}
}
