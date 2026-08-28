package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMemoryNodeCredentialIsHashOnlyBoundAndRevocable(t *testing.T) {
	concrete := newMemoryStore(false).(*memoryStore)
	node := &OverlayNode{ID: "gw_test_01", NetworkID: "net", Healthy: true}
	if err := concrete.UpsertOverlayNode(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	secret := "xgn_raw-must-not-persist"
	digest := sha256.Sum256([]byte(secret))
	credential := &OverlayNodeCredential{ID: "cred_test_01", NodeID: node.ID, TokenHash: digest[:], ExpiresAt: time.Now().Add(time.Hour)}
	audit := &AuditLog{Action: AuditActionOverlayNodeCredentialCreate, Details: map[string]any{"credential_id": credential.ID}}
	if err := concrete.CreateOverlayNodeCredential(context.Background(), credential, audit); err != nil {
		t.Fatal(err)
	}
	if concrete.overlayNodeCredentialHashes[secret] != "" {
		t.Fatal("raw credential persisted")
	}
	if _, err := concrete.AuthenticateOverlayNodeCredential(context.Background(), digest[:], time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := concrete.RevokeOverlayNodeCredential(context.Background(), node.ID, credential.ID, time.Now(), &AuditLog{Action: AuditActionOverlayNodeCredentialRevoke}); err != nil {
		t.Fatal(err)
	}
	if _, err := concrete.AuthenticateOverlayNodeCredential(context.Background(), digest[:], time.Now()); !errors.Is(err, ErrOverlayNodeCredentialRevoked) {
		t.Fatalf("revoked credential err=%v", err)
	}
}

func staticImportFixture(owner, key, idempotency, body string) *OverlayStaticImport {
	return &OverlayStaticImport{IdempotencyKey: idempotency, BodySHA256: body, OwnerUserID: owner, NetworkID: "network-test", SourceKind: "ansible-group-vars", SourceVariable: "xworkmate_bridge_distributed_vpn_clients", BaselineSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Devices: []OverlayProjectionDevice{{Device: OverlayDevice{ID: "device-test", WireGuardPublicKey: key, WireGuardAddress: "10.77.0.10/32", Platform: "legacy-import"}, Tags: []string{"migration:static-group-vars"}, Attachments: []string{"gw_test_01"}}}}
}

func TestMemoryStaticImportConcurrentReceiptAndConflict(t *testing.T) {
	st := newMemoryStore(false).(*memoryStore)
	owner := "11111111-1111-4111-8111-111111111111"
	if err := st.CreateUser(context.Background(), &User{ID: owner, Name: "owner", Email: "owner@example.test"}); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	input := staticImportFixture(owner, key, "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	const workers = 12
	var wg sync.WaitGroup
	receipts := make(chan *OverlayStaticImportReceipt, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, _, err := st.ImportOverlayStaticClients(context.Background(), input, &AuditLog{Action: AuditActionOverlayStaticImport})
			if err != nil {
				errs <- err
			} else {
				receipts <- receipt
			}
		}()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var id string
	for receipt := range receipts {
		if id == "" {
			id = receipt.ImportID
		} else if receipt.ImportID != id {
			t.Fatal("idempotent receipt changed")
		}
	}
	changed := staticImportFixture(owner, key, "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, _, err := st.ImportOverlayStaticClients(context.Background(), changed, &AuditLog{Action: AuditActionOverlayStaticImport}); !errors.Is(err, ErrOverlayStaticImportIdempotency) {
		t.Fatalf("idempotency mismatch err=%v", err)
	}
	other := "22222222-2222-4222-8222-222222222222"
	if err := st.CreateUser(context.Background(), &User{ID: other, Name: "other", Email: "other@example.test"}); err != nil {
		t.Fatal(err)
	}
	conflict := staticImportFixture(other, key, "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	if _, _, err := st.ImportOverlayStaticClients(context.Background(), conflict, &AuditLog{Action: AuditActionOverlayStaticImport}); !errors.Is(err, ErrOverlayStaticImportConflict) {
		t.Fatalf("cross-owner conflict err=%v", err)
	}
}

func TestGatewaySnapshotExtendsSigningKeyVerificationWindow(t *testing.T) {
	st := newMemoryStore(false).(*memoryStore)
	want := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	st.overlayGatewaySnapshots["gw_test_01"] = []*OverlayGatewaySnapshotRecord{{SigningKeyID: "key_previous_01", ExpiresAt: want}}
	got, err := st.GetOverlaySigningKeyMaxExpiresAt(context.Background(), "key_previous_01")
	if err != nil || !got.Equal(want) {
		t.Fatalf("gateway signing window=%v err=%v, want %v", got, err, want)
	}
}
