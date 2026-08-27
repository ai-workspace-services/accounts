package projection

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"account/internal/store"
)

func testStoreService(t *testing.T, st store.Store, signer Signer, now time.Time) *Service {
	t.Helper()
	repository, err := NewStoreRepository(st, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository, signer, func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestKeyRingRejectsUnsafeRotationMetadata(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	tests := []struct {
		name    string
		entry   KeyRingEntry
		wantErr string
	}{
		{name: "invalid status", entry: KeyRingEntry{KeyID: "old", PublicKey: publicKey, Status: "retired", NotBefore: now}, wantErr: "invalid status"},
		{name: "previous private key", entry: KeyRingEntry{KeyID: "old", PublicKey: publicKey, PrivateKey: privateKey, Status: "previous", NotBefore: now}, wantErr: "must not contain private"},
		{name: "backwards window", entry: KeyRingEntry{KeyID: "old", PublicKey: publicKey, Status: "previous", NotBefore: now, NotAfter: timePointer(now)}, wantErr: "not_after must be after"},
	}
	current, _ := NewEd25519Signer(privateKey, "current")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewEd25519KeyRingWithCurrentSigner(current, now, nil, []KeyRingEntry{test.entry}, func() time.Time { return now })
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error=%v, want %q", err, test.wantErr)
			}
		})
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestStoreRepositorySurvivesServiceReconstructionAndPreservesAck(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	signer, _ := NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "key-1")
	st := store.NewMemoryStore()
	firstService := testStoreService(t, st, signer, now)
	first, err := firstService.Project(context.Background(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	firstAck, err := firstService.Acknowledge(context.Background(), Ack{UserID: validInput().UserID, DeviceID: first.DeviceID, ConfigID: first.ConfigID, Generation: first.Generation})
	if err != nil {
		t.Fatal(err)
	}

	// Rebuild the repository and service around the same Store, just as API
	// wiring does after a process restart with PostgreSQL.
	secondService := testStoreService(t, st, signer, now)
	duplicate, err := secondService.Acknowledge(context.Background(), Ack{UserID: validInput().UserID, DeviceID: first.DeviceID, ConfigID: first.ConfigID, Generation: first.Generation})
	if err != nil || !duplicate.Duplicate || !duplicate.Ack.ReceivedAt.Equal(firstAck.Ack.ReceivedAt) {
		t.Fatalf("ACK was not preserved: duplicate=%#v err=%v", duplicate, err)
	}
	changed := validInput()
	changed.SourceRevision = "revision-after-restart"
	second, err := secondService.Project(context.Background(), changed)
	if err != nil || second.Generation != 2 {
		t.Fatalf("generation after reconstruction = %d err=%v", second.Generation, err)
	}
}

func TestStoreRepositoryConcurrentDuplicateSourceIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	signer, _ := NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "key-1")
	st := store.NewMemoryStore()
	const workers = 16
	configs := make(chan uint64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			config, err := testStoreService(t, st, signer, now).Project(context.Background(), validInput())
			if err != nil {
				errs <- err
				return
			}
			configs <- config.Generation
		}()
	}
	wg.Wait()
	close(errs)
	close(configs)
	for err := range errs {
		t.Fatalf("concurrent projection: %v", err)
	}
	for generation := range configs {
		if generation != 1 {
			t.Fatalf("duplicate source generation = %d, want 1", generation)
		}
	}
}

func TestStoreRepositoryConcurrentChangedSourcesRemainStrictlyMonotonic(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	seed := make([]byte, ed25519.SeedSize)
	signer, _ := NewEd25519Signer(ed25519.NewKeyFromSeed(seed), "key-1")
	st := store.NewMemoryStore()
	const workers = 8
	services := make([]*Service, workers)
	for index := range services {
		services[index] = testStoreService(t, st, signer, now)
	}
	configs := make(chan uint64, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for index := range services {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			input := validInput()
			input.SourceRevision = fmt.Sprintf("revision-concurrent-%d", index)
			config, err := services[index].Project(context.Background(), input)
			if err != nil {
				errs <- err
				return
			}
			configs <- config.Generation
		}(index)
	}
	wg.Wait()
	close(errs)
	close(configs)
	for err := range errs {
		t.Fatalf("concurrent changed projection: %v", err)
	}
	seen := make(map[uint64]bool, workers)
	for generation := range configs {
		if generation == 0 || generation > workers || seen[generation] {
			t.Fatalf("non-monotonic generation %d; seen=%v", generation, seen)
		}
		seen[generation] = true
	}
	for generation := uint64(1); generation <= workers; generation++ {
		if !seen[generation] {
			t.Fatalf("generation %d missing; seen=%v", generation, seen)
		}
	}
}

func TestSigningKeyRotationVerifiesPreviousAndSignsWithCurrent(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	oldPrivate := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	newSeed := make([]byte, ed25519.SeedSize)
	newSeed[0] = 42
	newPrivate := ed25519.NewKeyFromSeed(newSeed)
	oldSigner, _ := NewEd25519Signer(oldPrivate, "key-old")
	newSigner, _ := NewEd25519Signer(newPrivate, "key-new")
	oldRing, err := NewEd25519KeyRingWithCurrentSigner(oldSigner, now.Add(-time.Hour), nil, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	oldConfig, err := testStoreService(t, st, oldRing, now).Project(context.Background(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	windowEnd := now.Add(24 * time.Hour)
	newRing, err := NewEd25519KeyRingWithCurrentSigner(newSigner, now, nil, []KeyRingEntry{{
		KeyID: "key-old", PublicKey: oldPrivate.Public().(ed25519.PublicKey), Status: "previous", NotBefore: now.Add(-time.Hour), NotAfter: &windowEnd,
	}}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignedConfig(oldConfig, newRing, now); err != nil {
		t.Fatalf("old config did not verify in rotation window: %v", err)
	}
	changed := validInput()
	changed.SourceRevision = "revision-new-key"
	newConfig, err := testStoreService(t, st, newRing, now).Project(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if newConfig.Signature.KeyID != "key-new" || newConfig.Generation != oldConfig.Generation+1 {
		t.Fatalf("new config = %#v", newConfig)
	}
	for _, key := range newRing.PublicSigningKeys() {
		if key.PublicKey == "" || key.KeyID == "" {
			t.Fatalf("invalid public discovery key: %#v", key)
		}
	}
}
