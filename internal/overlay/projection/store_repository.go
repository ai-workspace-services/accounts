package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"account/internal/overlay/domain"
	"account/internal/store"
)

// StoreRepository adapts the account Store's memory and PostgreSQL
// implementations to the projection service contract.
type StoreRepository struct {
	store store.Store
	clock func() time.Time
}

func NewStoreRepository(st store.Store, clock func() time.Time) (*StoreRepository, error) {
	if st == nil {
		return nil, errors.New("projection store is required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &StoreRepository{store: st, clock: clock}, nil
}

func (r *StoreRepository) Latest(ctx context.Context, userID, deviceID string) (Record, bool, error) {
	stored, err := r.store.GetLatestOverlaySignedConfig(ctx, userID, deviceID)
	if errors.Is(err, store.ErrOverlaySignedConfigNotFound) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	config, err := domain.DecodeSignedConfig(stored.SignedPayload)
	if err != nil {
		return Record{}, false, fmt.Errorf("decode stored signed config: %w", err)
	}
	record := Record{UserID: stored.UserID, SourceRevision: stored.SourceRevision, Config: config}
	if stored.Ack != nil {
		record.Ack = &Ack{
			UserID: stored.Ack.UserID, DeviceID: stored.Ack.DeviceID, ConfigID: stored.Ack.ConfigID,
			Generation: stored.Ack.Generation, AppliedAt: stored.Ack.AppliedAt, ReceivedAt: stored.Ack.ReceivedAt,
		}
	}
	return record, true, nil
}

func (r *StoreRepository) Save(ctx context.Context, record Record) error {
	payload, err := json.Marshal(record.Config)
	if err != nil {
		return err
	}
	err = r.store.SaveOverlaySignedConfig(ctx, &store.OverlaySignedConfigRecord{
		UserID: record.UserID, DeviceID: record.Config.DeviceID, ConfigID: record.Config.ConfigID,
		NetworkID: record.Config.NetworkID, Generation: record.Config.Generation,
		SourceRevision: record.SourceRevision, SigningKeyID: record.Config.Signature.KeyID,
		SignedPayload: payload, IssuedAt: record.Config.IssuedAt, ExpiresAt: record.Config.ExpiresAt,
	})
	switch {
	case errors.Is(err, store.ErrOverlaySignedConfigStale):
		return ErrStaleGeneration
	case errors.Is(err, store.ErrOverlaySignedConfigGap):
		return ErrGenerationGap
	default:
		return err
	}
}

func (r *StoreRepository) Acknowledge(ctx context.Context, ack Ack) (AckResult, error) {
	now := r.clock().UTC()
	if ack.AppliedAt.IsZero() {
		ack.AppliedAt = now
	} else if ack.AppliedAt.After(now.Add(5 * time.Minute)) {
		return AckResult{}, ErrAppliedAtFuture
	}
	stored := store.OverlaySignedConfigAck{
		UserID: ack.UserID, DeviceID: ack.DeviceID, ConfigID: ack.ConfigID,
		Generation: ack.Generation, AppliedAt: ack.AppliedAt, ReceivedAt: now,
	}
	duplicate, err := r.store.AcknowledgeOverlaySignedConfig(ctx, &stored)
	switch {
	case errors.Is(err, store.ErrOverlaySignedConfigNotFound):
		return AckResult{}, ErrConfigNotFound
	case errors.Is(err, store.ErrOverlaySignedConfigMismatch):
		return AckResult{}, ErrConfigMismatch
	case errors.Is(err, store.ErrOverlaySignedConfigStale):
		return AckResult{}, ErrStaleGeneration
	case err != nil:
		return AckResult{}, err
	}
	return AckResult{Ack: Ack{
		UserID: stored.UserID, DeviceID: stored.DeviceID, ConfigID: stored.ConfigID,
		Generation: stored.Generation, AppliedAt: stored.AppliedAt, ReceivedAt: stored.ReceivedAt,
	}, Duplicate: duplicate}, nil
}
