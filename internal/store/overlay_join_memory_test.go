package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func memoryJoinFixture(t *testing.T, st Store, secret string, uses int, expires time.Time) *OverlayJoinToken {
	t.Helper()
	digest := sha256.Sum256([]byte(secret))
	token := &OverlayJoinToken{
		ID: "join-test", TokenHash: digest[:], UserID: "user-test", NetworkID: "network-test",
		RemainingUses: uses, ExpiresAt: expires,
	}
	audit := &AuditLog{Action: AuditActionOverlayJoinCreate, ActorUUID: token.UserID, Details: map[string]any{"join_token_id": token.ID}}
	if err := st.CreateOverlayJoinToken(context.Background(), token, audit); err != nil {
		t.Fatal(err)
	}
	return token
}

func memoryJoinExchange(secret, enrollmentSecret, deviceID string, now time.Time) *OverlayJoinExchange {
	joinHash := sha256.Sum256([]byte(secret))
	enrollmentHash := sha256.Sum256([]byte(enrollmentSecret))
	return &OverlayJoinExchange{
		JoinTokenHash: joinHash[:],
		Enrollment:    OverlayEnrollmentSession{ID: "enrollment-" + deviceID, TokenHash: enrollmentHash[:], CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)},
		Device:        OverlayDevice{ID: deviceID, Name: deviceID, Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
		AddressPrefix: "172.29.10", AddressStartHost: 100, AddressEndHost: 254,
	}
}

func TestMemoryJoinStoresOnlyHashesAndEnrollmentSurvivesLookup(t *testing.T) {
	now := time.Now().UTC()
	concrete := newMemoryStore(false).(*memoryStore)
	const joinSecret = "xjt_raw-secret-must-not-be-stored"
	memoryJoinFixture(t, concrete, joinSecret, 1, now.Add(time.Hour))
	for key, token := range concrete.overlayJoinTokens {
		if key == joinSecret || string(token.TokenHash) == joinSecret {
			t.Fatal("raw join secret was stored")
		}
	}
	encodedToken, err := json.Marshal(concrete.overlayJoinTokens["join-test"])
	if err != nil || strings.Contains(string(encodedToken), "TokenHash") || strings.Contains(string(encodedToken), base64OfSHA256(joinSecret)) {
		t.Fatalf("JSON exposure includes token hash: %s err=%v", encodedToken, err)
	}
	exchange := memoryJoinExchange(joinSecret, "xenr_raw-session-secret", "device-one", now)
	audit := &AuditLog{Action: AuditActionOverlayJoinExchange, Details: map[string]any{}}
	if err := concrete.ExchangeOverlayJoinToken(context.Background(), exchange, audit); err != nil {
		t.Fatal(err)
	}
	if _, exists := concrete.overlayEnrollments["xenr_raw-session-secret"]; exists {
		t.Fatal("raw enrollment secret was used as storage key")
	}
	session, err := concrete.GetOverlayEnrollmentSession(context.Background(), exchange.Enrollment.TokenHash, now.Add(time.Minute))
	if err != nil || session.DeviceID != "device-one" {
		t.Fatalf("session=%#v err=%v", session, err)
	}
	for _, entry := range concrete.auditLogs {
		encoded := fmt.Sprint(entry.Details)
		joinDigest := sha256.Sum256([]byte(joinSecret))
		enrollmentDigest := sha256.Sum256([]byte("xenr_raw-session-secret"))
		if strings.Contains(encoded, joinSecret) || strings.Contains(encoded, "xenr_raw-session-secret") ||
			strings.Contains(encoded, hex.EncodeToString(joinDigest[:])) || strings.Contains(encoded, hex.EncodeToString(enrollmentDigest[:])) {
			t.Fatalf("audit leaked secret: %#v", entry)
		}
	}
	if _, err := concrete.GetOverlayEnrollmentSession(context.Background(), exchange.Enrollment.TokenHash, exchange.Enrollment.ExpiresAt); !errors.Is(err, ErrOverlayEnrollmentExpired) {
		t.Fatalf("expired enrollment error=%v", err)
	}
}

func base64OfSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return base64.StdEncoding.EncodeToString(digest[:])
}

func TestMemoryJoinRejectsExpiredRevokedConstraintsAndExhaustion(t *testing.T) {
	now := time.Now().UTC()
	t.Run("expired", func(t *testing.T) {
		st := newMemoryStore(false)
		memoryJoinFixture(t, st, "xjt_expired", 1, now.Add(-time.Second))
		err := st.ExchangeOverlayJoinToken(context.Background(), memoryJoinExchange("xjt_expired", "xenr_expired", "dev", now), &AuditLog{Action: AuditActionOverlayJoinExchange})
		if !errors.Is(err, ErrOverlayJoinTokenExpired) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		st := newMemoryStore(false)
		token := memoryJoinFixture(t, st, "xjt_revoked", 1, now.Add(time.Hour))
		if err := st.RevokeOverlayJoinToken(context.Background(), token.UserID, token.ID, now, &AuditLog{Action: AuditActionOverlayJoinRevoke}); err != nil {
			t.Fatal(err)
		}
		err := st.ExchangeOverlayJoinToken(context.Background(), memoryJoinExchange("xjt_revoked", "xenr_revoked", "dev", now), &AuditLog{Action: AuditActionOverlayJoinExchange})
		if !errors.Is(err, ErrOverlayJoinTokenRevoked) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("constraint", func(t *testing.T) {
		st := newMemoryStore(false)
		digest := sha256.Sum256([]byte("xjt_constraint"))
		token := &OverlayJoinToken{ID: "join-c", TokenHash: digest[:], UserID: "user", NetworkID: "net", DeviceID: "allowed", Platform: "linux", RemainingUses: 1, ExpiresAt: now.Add(time.Hour)}
		if err := st.CreateOverlayJoinToken(context.Background(), token, &AuditLog{Action: AuditActionOverlayJoinCreate}); err != nil {
			t.Fatal(err)
		}
		err := st.ExchangeOverlayJoinToken(context.Background(), memoryJoinExchange("xjt_constraint", "xenr_constraint", "other", now), &AuditLog{Action: AuditActionOverlayJoinExchange})
		if !errors.Is(err, ErrOverlayJoinConstraint) {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestMemoryJoinConcurrentLastUseSucceedsExactlyOnce(t *testing.T) {
	now := time.Now().UTC()
	st := newMemoryStore(false)
	const secret = "xjt_concurrent-one-use"
	memoryJoinFixture(t, st, secret, 1, now.Add(time.Hour))
	const workers = 24
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			exchange := memoryJoinExchange(secret, fmt.Sprintf("xenr_%d", index), fmt.Sprintf("device-%d", index), now)
			results <- st.ExchangeOverlayJoinToken(context.Background(), exchange, &AuditLog{Action: AuditActionOverlayJoinExchange})
		}(index)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrOverlayJoinTokenExhausted) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes=%d, want 1", successes)
	}
}

func TestMemoryJoinOneTimeTokenCannotExchangeAnotherDevice(t *testing.T) {
	now := time.Now().UTC()
	st := newMemoryStore(false)
	const secret = "xjt_one-use-no-device-replay"
	memoryJoinFixture(t, st, secret, 1, now.Add(time.Hour))
	first := memoryJoinExchange(secret, "xenr_first", "first-device", now)
	if err := st.ExchangeOverlayJoinToken(context.Background(), first, &AuditLog{Action: AuditActionOverlayJoinExchange}); err != nil {
		t.Fatal(err)
	}
	second := memoryJoinExchange(secret, "xenr_second", "different-device", now.Add(time.Second))
	if err := st.ExchangeOverlayJoinToken(context.Background(), second, &AuditLog{Action: AuditActionOverlayJoinExchange}); !errors.Is(err, ErrOverlayJoinTokenExhausted) {
		t.Fatalf("different-device replay error=%v", err)
	}
}
