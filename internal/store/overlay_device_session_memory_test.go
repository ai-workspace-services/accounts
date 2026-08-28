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

func seedMemoryDeviceCredential(t *testing.T, now time.Time) (*memoryStore, *OverlayDeviceCredential, []byte) {
	t.Helper()
	st := newMemoryStore(false).(*memoryStore)
	user := &User{Name: "device owner", Email: "owner@example.test", Active: true, EmailVerified: true}
	if err := st.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	device := &OverlayDevice{ID: "device-1", UserID: user.ID, NetworkID: "network-1", Name: "device-1", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "172.29.10.100/32", Status: OverlayDeviceActive}
	if err := st.UpsertOverlayDevice(context.Background(), device); err != nil {
		t.Fatal(err)
	}
	raw := "xdc_0123456789abcdef0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	verifier := sha256.Sum256([]byte(raw))
	credential := &OverlayDeviceCredential{ID: "xdcid_0123456789abcdef0123456789abcdef", Verifier: verifier[:], UserID: user.ID, NetworkID: device.NetworkID, DeviceID: device.ID, Status: OverlayDeviceCredentialActive, Scopes: append([]string(nil), overlayDeviceCredentialScopes...), IssuedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour), CreatedAt: now}
	st.mu.Lock()
	st.overlayDeviceCredentials[credential.ID] = cloneOverlayDeviceCredential(credential)
	st.mu.Unlock()
	return st, credential, verifier[:]
}

func TestMemoryDeviceCredentialScopesExpiryAndSuspension(t *testing.T) {
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	st, credential, verifier := seedMemoryDeviceCredential(t, now)
	if _, err := st.AuthenticateOverlayDeviceCredential(context.Background(), credential.ID, verifier, now); err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), verifier...)
	bad[0] ^= 0xff
	if _, err := st.AuthenticateOverlayDeviceCredential(context.Background(), credential.ID, bad, now); !errors.Is(err, ErrOverlayDeviceCredentialUnauthorized) {
		t.Fatalf("bad verifier err=%v", err)
	}
	if _, err := st.AuthenticateOverlayDeviceCredential(context.Background(), credential.ID, verifier, credential.ExpiresAt); !errors.Is(err, ErrOverlayDeviceCredentialUnauthorized) {
		t.Fatalf("expired credential err=%v", err)
	}
	sessionHash := sha256.Sum256([]byte("xenr_session"))
	session := &OverlayEnrollmentSession{ID: "session-1", TokenHash: sessionHash[:], Scopes: append([]string(nil), overlayDeviceSessionScopes...), ExpiresAt: now.Add(15 * time.Minute)}
	if err := st.MintOverlayDeviceSession(context.Background(), credential.ID, session, now, &AuditLog{Action: AuditActionOverlayDeviceSessionMint}); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetOverlayDeviceSession(context.Background(), sessionHash[:], now.Add(time.Minute)); err != nil || got.DeviceID != credential.DeviceID || !exactScopes(got.Scopes, overlayDeviceSessionScopes) {
		t.Fatalf("session=%#v err=%v", got, err)
	}
	if _, _, err := st.SetOverlayDeviceStatus(context.Background(), credential.UserID, credential.NetworkID, credential.DeviceID, OverlayDeviceInactive, 1, "suspended", &AuditLog{Action: AuditActionOverlayDeviceStateUpdate}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AuthenticateOverlayDeviceCredential(context.Background(), credential.ID, verifier, now.Add(time.Minute)); !errors.Is(err, ErrOverlayDeviceCredentialUnauthorized) {
		t.Fatalf("inactive device credential err=%v", err)
	}
	if _, err := st.GetOverlayDeviceSession(context.Background(), sessionHash[:], now.Add(time.Minute)); !errors.Is(err, ErrOverlayEnrollmentNotFound) {
		t.Fatalf("inactive device session err=%v", err)
	}
}

func TestMemoryDeviceCredentialConcurrentRotationHasOneSuccessor(t *testing.T) {
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	st, current, _ := seedMemoryDeviceCredential(t, now)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for index := 0; index < 2; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			verifier := sha256.Sum256([]byte(fmt.Sprintf("successor-%d", index)))
			successor := &OverlayDeviceCredential{ID: fmt.Sprintf("xdcid_%032x", index+2), Verifier: verifier[:], Scopes: append([]string(nil), overlayDeviceCredentialScopes...), ExpiresAt: now.Add(30 * 24 * time.Hour)}
			_, _, err := st.RotateOverlayDeviceCredential(context.Background(), current.ID, successor, fmt.Sprintf("%064x", index+1), now, &AuditLog{Action: AuditActionOverlayDeviceCredentialRotate})
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrOverlayDeviceCredentialUnauthorized) && !errors.Is(err, ErrOverlayDeviceCredentialIdempotency) {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful rotations=%d want=1", successes)
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	active := 0
	for _, credential := range st.overlayDeviceCredentials {
		if credential.DeviceID == current.DeviceID && credential.Status == OverlayDeviceCredentialActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active credentials=%d want=1", active)
	}
}

func TestMemoryHistoricalCredentialReturnsOnlyTerminalRevokeReceipt(t *testing.T) {
	now := time.Date(2026, 8, 28, 6, 0, 0, 0, time.UTC)
	st, credential, verifier := seedMemoryDeviceCredential(t, now)
	if _, _, err := st.SetOverlayDeviceStatus(context.Background(), credential.UserID, credential.NetworkID, credential.DeviceID, OverlayDeviceRevoked, 1, "admin revoke", &AuditLog{Action: AuditActionOverlayDeviceRevoke}); err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%064x", 9)
	nonce := "11111111-1111-4111-8111-111111111111"
	receipt, duplicate, err := st.RevokeOverlayDeviceWithCredential(context.Background(), credential.ID, verifier, hash, nonce, now.Add(time.Minute), &AuditLog{Action: AuditActionOverlayDeviceRevoke})
	if err != nil || !duplicate || receipt.Device.Status != OverlayDeviceRevoked {
		t.Fatalf("receipt=%#v duplicate=%v err=%v", receipt, duplicate, err)
	}
	if _, duplicate, err = st.RevokeOverlayDeviceWithCredential(context.Background(), credential.ID, verifier, hash, nonce, now.Add(2*time.Minute), &AuditLog{Action: AuditActionOverlayDeviceRevoke}); err != nil || !duplicate {
		t.Fatalf("terminal replay duplicate=%v err=%v", duplicate, err)
	}
	if _, err = st.AuthenticateOverlayDeviceCredential(context.Background(), credential.ID, verifier, now.Add(time.Minute)); !errors.Is(err, ErrOverlayDeviceCredentialUnauthorized) {
		t.Fatalf("historical credential escaped terminal route: %v", err)
	}
}
