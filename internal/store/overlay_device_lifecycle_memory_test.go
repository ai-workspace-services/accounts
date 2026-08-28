package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestMemoryOverlayDeviceLifecycleIsVersionedIdempotentAndRevokesEnrollment(t *testing.T) {
	st := NewMemoryStore().(*memoryStore)
	user := &User{ID: "11111111-1111-4111-8111-111111111111", Name: "owner", Email: "owner@example.com", Active: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	device := &OverlayDevice{ID: "dev-a", UserID: user.ID, NetworkID: "net-a", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "10.77.0.2/32"}
	if err := st.UpsertOverlayDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	rotated, duplicate, err := st.RotateOverlayDeviceKey(context.Background(), user.ID, "net-a", device.ID, "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=", 1, &AuditLog{Action: AuditActionOverlayDeviceKeyRotate})
	if err != nil || duplicate || rotated.KeyVersion != 2 || rotated.StateVersion != 2 {
		t.Fatalf("rotation=%+v duplicate=%v err=%v", rotated, duplicate, err)
	}
	if _, duplicate, err = st.RotateOverlayDeviceKey(context.Background(), user.ID, "net-a", device.ID, rotated.WireGuardPublicKey, 1, &AuditLog{Action: AuditActionOverlayDeviceKeyRotate}); err != nil || !duplicate {
		t.Fatalf("idempotent rotation duplicate=%v err=%v", duplicate, err)
	}
	if _, _, err = st.RotateOverlayDeviceKey(context.Background(), user.ID, "net-a", device.ID, device.WireGuardPublicKey, 2, &AuditLog{Action: AuditActionOverlayDeviceKeyRotate}); !errors.Is(err, ErrOverlayDeviceKeyConflict) {
		t.Fatalf("rotated-away key was reusable by same device: %v", err)
	}
	joinHash, enrollmentHash := sha256.Sum256([]byte("join")), sha256.Sum256([]byte("enrollment"))
	st.overlayJoinTokens["join-a"] = &OverlayJoinToken{ID: "join-a", UserID: user.ID, NetworkID: "net-a", DeviceID: device.ID, TokenHash: joinHash[:], RemainingUses: 1, ExpiresAt: time.Now().Add(time.Hour)}
	st.overlayEnrollments[string(enrollmentHash[:])] = &OverlayEnrollmentSession{ID: "enr-a", UserID: user.ID, NetworkID: "net-a", DeviceID: device.ID, WireGuardPublicKey: rotated.WireGuardPublicKey, TokenHash: enrollmentHash[:], ExpiresAt: time.Now().Add(time.Hour)}
	revoked, duplicate, err := st.SetOverlayDeviceStatus(context.Background(), user.ID, "net-a", device.ID, OverlayDeviceRevoked, 2, "lost", &AuditLog{Action: AuditActionOverlayDeviceRevoke})
	if err != nil || duplicate || revoked.Status != OverlayDeviceRevoked || revoked.RevokedAt == nil || revoked.StateVersion != 3 {
		t.Fatalf("revoke=%+v duplicate=%v err=%v", revoked, duplicate, err)
	}
	if _, duplicate, err = st.SetOverlayDeviceStatus(context.Background(), user.ID, "net-a", device.ID, OverlayDeviceRevoked, 2, "lost", &AuditLog{Action: AuditActionOverlayDeviceRevoke}); err != nil || !duplicate {
		t.Fatalf("idempotent revoke duplicate=%v err=%v", duplicate, err)
	}
	if _, err = st.GetOverlayEnrollmentSession(context.Background(), enrollmentHash[:], time.Now()); !errors.Is(err, ErrOverlayEnrollmentNotFound) {
		t.Fatalf("revoked enrollment accepted: %v", err)
	}
	if token := st.overlayJoinTokens["join-a"]; token.RevokedAt == nil {
		t.Fatal("device-bound invite was not revoked")
	}
	if err = st.UpsertOverlayDevice(context.Background(), &OverlayDevice{ID: device.ID, UserID: user.ID, NetworkID: "net-a", Platform: "linux", WireGuardPublicKey: rotated.WireGuardPublicKey}); !errors.Is(err, ErrOverlayDeviceRevoked) {
		t.Fatalf("revoked device registered again: %v", err)
	}
	for index, historicalKey := range []string{device.WireGuardPublicKey, rotated.WireGuardPublicKey} {
		err = st.UpsertOverlayDevice(context.Background(), &OverlayDevice{ID: fmt.Sprintf("replacement-%d", index), UserID: user.ID, NetworkID: "net-a", Platform: "linux", WireGuardPublicKey: historicalKey})
		if !errors.Is(err, ErrOverlayDeviceKeyConflict) {
			t.Fatalf("historical key %d was reassigned after revoke: %v", index, err)
		}
	}
	events, err := st.ListOverlayDeviceEvents(context.Background(), user.ID, "net-a", 0, 100)
	if err != nil || len(events) != 3 || events[0].Type != "registered" || events[1].Type != "key_rotated" || events[2].Type != "revoked" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestMemoryOverlayDeviceKeyClaimIsConcurrentAndPermanent(t *testing.T) {
	st := NewMemoryStore().(*memoryStore)
	userID := "11111111-1111-4111-8111-111111111111"
	if err := st.CreateUser(context.Background(), &User{ID: userID, Name: "owner", Email: "owner-concurrent@example.com", Active: true}); err != nil {
		t.Fatal(err)
	}
	const workers = 24
	key := "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM="
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results <- st.UpsertOverlayDevice(context.Background(), &OverlayDevice{ID: fmt.Sprintf("concurrent-%02d", index), UserID: userID, NetworkID: "net-concurrent", Platform: "linux", WireGuardPublicKey: key})
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrOverlayDeviceKeyConflict):
		default:
			t.Fatalf("unexpected claim error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims=%d, want 1", successes)
	}
}
