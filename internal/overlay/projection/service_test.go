package projection

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func newTestService(t *testing.T) (*Service, *Ed25519Signer, *time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	signer, err := NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "signing_key_01")
	if err != nil {
		t.Fatalf("create signer: %v", err)
	}
	repository := NewMemoryRepository(func() time.Time { return now })
	service, err := NewService(repository, signer, func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	return service, signer, &now
}

func validInput() Input {
	return Input{
		UserID: "usr_01xconnect", SourceRevision: "revision-1",
		NetworkID: "net_private", DeviceID: "dev_laptop",
		DeviceAddresses: []string{"10.77.0.10/32"},
		GatewayID:       "gw_tokyo_01", GatewayPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		AllowedIPs: []string{"10.77.0.0/16"}, InterfaceName: "wg-xco", MTU: 1280,
		LoopbackPort: 51830, PersistentKeepaliveSeconds: 25,
		RemoteHost: "gateway.example.net", RemotePort: 443,
		ServerName: "gateway.example.net", AuthID: "auth_device_01",
	}
}

func TestProjectSignsAndReusesUnchangedGeneration(t *testing.T) {
	service, signer, now := newTestService(t)
	first, err := service.Project(context.Background(), validInput())
	if err != nil {
		t.Fatalf("project first config: %v", err)
	}
	if err := VerifySignedConfig(first, signer, *now); err != nil {
		t.Fatalf("verify first config: %v", err)
	}
	second, err := service.Project(context.Background(), validInput())
	if err != nil {
		t.Fatalf("project unchanged config: %v", err)
	}
	if second.ConfigID != first.ConfigID || second.Generation != first.Generation {
		t.Fatalf("unchanged source was not idempotent: first=%#v second=%#v", first, second)
	}
}

func TestProjectAdvancesGenerationAndRejectsStaleAck(t *testing.T) {
	service, _, now := newTestService(t)
	first, err := service.Project(context.Background(), validInput())
	if err != nil {
		t.Fatalf("project first config: %v", err)
	}
	result, err := service.Acknowledge(context.Background(), Ack{
		UserID: validInput().UserID, DeviceID: first.DeviceID, ConfigID: first.ConfigID,
		Generation: first.Generation,
	})
	if err != nil || result.Duplicate || !result.Ack.AppliedAt.Equal(*now) {
		t.Fatalf("ack first config: result=%#v err=%v", result, err)
	}
	duplicate, err := service.Acknowledge(context.Background(), Ack{
		UserID: validInput().UserID, DeviceID: first.DeviceID, ConfigID: first.ConfigID,
		Generation: first.Generation, AppliedAt: now.Add(time.Minute),
	})
	if err != nil || !duplicate.Duplicate || !duplicate.Ack.AppliedAt.Equal(*now) {
		t.Fatalf("duplicate ACK was not idempotent: result=%#v err=%v", duplicate, err)
	}

	changed := validInput()
	changed.SourceRevision = "revision-2"
	second, err := service.Project(context.Background(), changed)
	if err != nil {
		t.Fatalf("project changed config: %v", err)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
	}
	_, err = service.Acknowledge(context.Background(), Ack{
		UserID: changed.UserID, DeviceID: first.DeviceID, ConfigID: first.ConfigID, Generation: first.Generation,
	})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale ACK error = %v, want %v", err, ErrStaleGeneration)
	}
}

func TestVerifySignedConfigRejectsExpiredAndTamperedPayload(t *testing.T) {
	service, signer, now := newTestService(t)
	config, err := service.Project(context.Background(), validInput())
	if err != nil {
		t.Fatalf("project config: %v", err)
	}
	if err := VerifySignedConfig(config, signer, config.ExpiresAt); !errors.Is(err, ErrConfigExpired) {
		t.Fatalf("expired verify error = %v, want %v", err, ErrConfigExpired)
	}
	config.WireGuard.MTU++
	if err := VerifySignedConfig(config, signer, *now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered verify error = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestAcknowledgeRejectsFutureAppliedAt(t *testing.T) {
	service, _, now := newTestService(t)
	config, err := service.Project(context.Background(), validInput())
	if err != nil {
		t.Fatalf("project config: %v", err)
	}
	_, err = service.Acknowledge(context.Background(), Ack{
		UserID: validInput().UserID, DeviceID: config.DeviceID, ConfigID: config.ConfigID,
		Generation: config.Generation, AppliedAt: now.Add(6 * time.Minute),
	})
	if !errors.Is(err, ErrAppliedAtFuture) {
		t.Fatalf("future ACK error = %v, want %v", err, ErrAppliedAtFuture)
	}
}
