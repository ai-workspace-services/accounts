package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSigningKeyPersistenceDualKeyRingAndFailFast(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:signing-key-ring-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Fail-fast when RequireSigningKey is enabled without private key.
	if _, err := NewService(db, Config{RequireSigningKey: true}); err == nil {
		t.Fatal("expected NewService to fail fast when RequireSigningKey=true and SigningPrivateKey is empty")
	}

	t.Setenv("XCONNECT_OVERLAY_REQUIRE_SIGNING_KEY", "true")
	t.Setenv("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY", "")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("expected ConfigFromEnv to fail fast when XCONNECT_OVERLAY_REQUIRE_SIGNING_KEY=true and key missing")
	}

	pubCurr, privCurr, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubPrev, privPrev, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	notAfter := now.Add(12 * time.Hour)

	// 32-byte seed + previous public key + explicit notAfter via env.
	t.Setenv("XCONNECT_OVERLAY_SIGNING_KEY_ID", "zero-signing-202611")
	t.Setenv("XCONNECT_OVERLAY_SIGNING_PRIVATE_KEY", base64.StdEncoding.EncodeToString(privCurr.Seed()))
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_KEY_ID", "zero-signing-202610")
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pubPrev))
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_NOT_AFTER", notAfter.Format(time.RFC3339))

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv with 32-byte seed + previous public key failed: %v", err)
	}
	cfg.Clock = func() time.Time { return now }
	service, err := NewService(db, cfg)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}

	keys := service.SigningKeys(now)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys in dual key ring, got %#v", keys)
	}
	if keys[0].KeyID != "zero-signing-202611" || keys[0].Status != "current" || keys[0].PublicKey != base64.StdEncoding.EncodeToString(pubCurr) {
		t.Fatalf("unexpected current key: %#v", keys[0])
	}
	if keys[1].KeyID != "zero-signing-202610" || keys[1].Status != "previous" || keys[1].PublicKey != base64.StdEncoding.EncodeToString(pubPrev) || keys[1].NotAfter == nil || !keys[1].NotAfter.Equal(notAfter) {
		t.Fatalf("unexpected previous key: %#v", keys[1])
	}

	// After NotAfter passes, previous key automatically drops from the key ring.
	expiredKeys := service.SigningKeys(notAfter.Add(time.Minute))
	if len(expiredKeys) != 1 || expiredKeys[0].Status != "current" {
		t.Fatalf("expected expired previous key to be omitted, got %#v", expiredKeys)
	}

	// Also support deriving previous public key from XCONNECT_OVERLAY_PREVIOUS_SIGNING_PRIVATE_KEY.
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PUBLIC_KEY", "")
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_PRIVATE_KEY", base64.RawStdEncoding.EncodeToString(privPrev))
	t.Setenv("XCONNECT_OVERLAY_PREVIOUS_SIGNING_NOT_AFTER", "")
	cfg2, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv with previous private key failed: %v", err)
	}
	if !bytes.Equal(cfg2.PreviousSigningPublicKey, pubPrev) {
		t.Fatalf("derived previous public key mismatch")
	}
	svc2, err := NewService(db, cfg2)
	if err != nil {
		t.Fatalf("NewService cfg2 failed: %v", err)
	}
	keys2 := svc2.SigningKeys(now)
	if len(keys2) != 2 || keys2[1].NotAfter == nil || !keys2[1].NotAfter.After(now) {
		t.Fatalf("expected default 24h NotAfter for previous key, got %#v", keys2)
	}
}

func TestOneIdempotentRejoinPreservesIPAndPolicyAndRejectsKeyCollision(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:one-idempotent-rejoin-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningPrivateKey: signingKey, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	netCfg := BootstrapNetwork{
		ID: "net-rejoin", DisplayName: "Rejoin Test", CIDR: "10.67.0.0/24",
		GatewayID: "gw-rejoin", GatewayWireGuardKey: gwKey, GatewayWireGuardAddress: "10.67.0.1/32",
		GatewayEndpointHost: "gw.example.test", GatewayEndpointPort: 51820,
		TransportServerName: "gw.example.test", TransportPort: 443,
		TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "owner-1",
	}

	tok1 := newOpaqueToken("xjt_")
	if _, err := service.Seed(t.Context(), BootstrapConfig{Network: netCfg, Invite: BootstrapInvite{DeviceID: "node-mac", Platform: "darwin", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, tok1); err != nil {
		t.Fatal(err)
	}
	keyMac1 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	join1, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: tok1, DeviceID: "node-mac", Name: "MacBook", Hostname: "mac.local", Platform: "darwin", Role: RoleOne, WireGuardPublicKey: keyMac1})
	if err != nil {
		t.Fatalf("first join failed: %v", err)
	}
	origIP := join1.Device.WireGuardAddress
	origGen := join1.Network.ConfigGeneration

	// Enroll a second node so we can verify cross-device key collision is still rejected.
	tokOther := newOpaqueToken("xjt_")
	if _, err := service.Seed(t.Context(), BootstrapConfig{Network: netCfg, Invite: BootstrapInvite{DeviceID: "node-fedora", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, tokOther); err != nil {
		t.Fatal(err)
	}
	keyFedora := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: tokOther, DeviceID: "node-fedora", Name: "Fedora", Hostname: "fedora.local", Platform: "linux", Role: RoleOne, WireGuardPublicKey: keyFedora}); err != nil {
		t.Fatalf("second node join failed: %v", err)
	}

	// Re-join node-mac with a rotated WireGuard key: must preserve IP and advance generation.
	tok2 := newOpaqueToken("xjt_")
	if _, err := service.Seed(t.Context(), BootstrapConfig{Network: netCfg, Invite: BootstrapInvite{DeviceID: "node-mac", Platform: "darwin", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, tok2); err != nil {
		t.Fatal(err)
	}
	keyMac2 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	join2, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: tok2, DeviceID: "node-mac", Name: "MacBook Pro", Hostname: "mac.local", Platform: "darwin", Role: RoleOne, WireGuardPublicKey: keyMac2})
	if err != nil {
		t.Fatalf("re-join with rotated key failed: %v", err)
	}
	if join2.Device.WireGuardAddress != origIP {
		t.Fatalf("expected IP preserved (%s), got %s", origIP, join2.Device.WireGuardAddress)
	}
	if join2.Device.WireGuardPublicKey != keyMac2 {
		t.Fatalf("expected WireGuard public key updated to %s, got %s", keyMac2, join2.Device.WireGuardPublicKey)
	}
	if join2.Network.ConfigGeneration <= origGen {
		t.Fatalf("expected generation bump after key rotation, orig=%d got=%d", origGen, join2.Network.ConfigGeneration)
	}

	// Attempting to re-join node-mac using node-fedora's WireGuard key must fail with ErrDeviceConflict.
	tokCollide := newOpaqueToken("xjt_")
	if _, err := service.Seed(t.Context(), BootstrapConfig{Network: netCfg, Invite: BootstrapInvite{DeviceID: "node-mac", Platform: "darwin", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, tokCollide); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: tokCollide, DeviceID: "node-mac", Name: "MacBook Pro", Hostname: "mac.local", Platform: "darwin", Role: RoleOne, WireGuardPublicKey: keyFedora}); !errors.Is(err, ErrDeviceConflict) {
		t.Fatalf("expected ErrDeviceConflict on cross-device WG key collision, got %v", err)
	}
}

func TestPolicyDefaultICMPAllowAndExplicitDenyPrecedence(t *testing.T) {
	service, _, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"

	updated, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "deny",
		Rules: []PolicyRule{
			{
				ID:                 "deny-icmp-db",
				Action:             "deny",
				SourceDevices:      []string{"macbook"},
				DestinationDevices: []string{"postgres-db"},
				Protocols:          []string{"icmp"},
			},
			{
				ID:                 "allow-ssh",
				Action:             "allow",
				SourceDevices:      []string{"macbook"},
				DestinationDevices: []string{"grok-bot"},
				Protocols:          []string{"tcp"},
				Ports:              []int{22},
			},
		},
	})
	if err != nil {
		t.Fatalf("AdminUpdatePolicy failed: %v", err)
	}
	if updated.Rules[1].Action != "allow" {
		t.Fatalf("expected canonical action 'allow', got %q", updated.Rules[1].Action)
	}

	// Explicit ICMP deny rule takes precedence over default ICMP allow.
	deniedICMP, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "postgres-db",
		Protocol:            "icmp",
	})
	if err != nil || deniedICMP.Decision != "deny" || deniedICMP.MatchedRuleID != "deny-icmp-db" {
		t.Fatalf("expected explicit ICMP deny to win, got %#v err=%v", deniedICMP, err)
	}

	// Unmatched ICMP between other pairs defaults to allow.
	defaultICMP, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "icmp",
	})
	if err != nil || defaultICMP.Decision != "allow" || defaultICMP.Reason != "default_icmp_allow" {
		t.Fatalf("expected unmatched ICMP to default-allow, got %#v err=%v", defaultICMP, err)
	}
}

func TestPruneExpiredEnrollmentSessions(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:prune-sessions-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	service, err := NewService(db, Config{SigningPrivateKey: signingKey, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	records := []EnrollmentRecord{
		{ID: "enr_old_1", DeviceID: "dev-1", TokenHash: HashSecret("old1"), IssuedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-47 * time.Hour)},
		{ID: "enr_old_2", DeviceID: "dev-1", TokenHash: HashSecret("old2"), IssuedAt: now.Add(-26 * time.Hour), ExpiresAt: now.Add(-25 * time.Hour)},
		{ID: "enr_recent_expired", DeviceID: "dev-1", TokenHash: HashSecret("recent"), IssuedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour)},
		{ID: "enr_active", DeviceID: "dev-1", TokenHash: HashSecret("active"), IssuedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(10 * time.Minute)},
	}
	for _, rec := range records {
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := service.PruneExpiredEnrollmentSessions(t.Context(), DefaultEnrollmentRetention)
	if err != nil {
		t.Fatalf("PruneExpiredEnrollmentSessions failed: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 expired sessions deleted, got %d", deleted)
	}

	var remaining []EnrollmentRecord
	if err := db.Order("id ASC").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 || remaining[0].ID != "enr_active" || remaining[1].ID != "enr_recent_expired" {
		t.Fatalf("unexpected remaining sessions: %#v", remaining)
	}
}
