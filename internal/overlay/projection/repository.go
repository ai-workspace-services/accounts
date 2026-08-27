package projection

import (
	"context"
	"errors"
	"sync"
	"time"

	"account/internal/overlay/domain"
)

var (
	ErrConfigNotFound  = errors.New("signed config not found")
	ErrConfigMismatch  = errors.New("signed config id does not match generation")
	ErrStaleGeneration = errors.New("stale signed config generation")
	ErrGenerationGap   = errors.New("signed config generation must advance by one")
	ErrAppliedAtFuture = errors.New("signed config applied_at is too far in the future")
)

type Record struct {
	UserID         string
	SourceRevision string
	Config         domain.SignedConfig
	Ack            *Ack
}

type Ack struct {
	UserID     string    `json:"-"`
	DeviceID   string    `json:"device_id"`
	ConfigID   string    `json:"config_id"`
	Generation uint64    `json:"generation"`
	AppliedAt  time.Time `json:"applied_at"`
	ReceivedAt time.Time `json:"received_at"`
}

type AckResult struct {
	Ack       Ack  `json:"ack"`
	Duplicate bool `json:"duplicate"`
}

type Repository interface {
	Latest(ctx context.Context, userID, deviceID string) (Record, bool, error)
	Save(ctx context.Context, record Record) error
	Acknowledge(ctx context.Context, ack Ack) (AckResult, error)
}

type MemoryRepository struct {
	mu      sync.RWMutex
	records map[string]Record
	clock   func() time.Time
}

func NewMemoryRepository(clock func() time.Time) *MemoryRepository {
	if clock == nil {
		clock = time.Now
	}
	return &MemoryRepository{records: make(map[string]Record), clock: clock}
}

func (r *MemoryRepository) Latest(_ context.Context, userID, deviceID string) (Record, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, ok := r.records[repositoryKey(userID, deviceID)]
	return cloneRecord(record), ok, nil
}

func (r *MemoryRepository) Save(_ context.Context, record Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := repositoryKey(record.UserID, record.Config.DeviceID)
	if current, exists := r.records[key]; exists {
		if record.Config.Generation <= current.Config.Generation {
			return ErrStaleGeneration
		}
		if record.Config.Generation != current.Config.Generation+1 {
			return ErrGenerationGap
		}
	} else if record.Config.Generation != 1 {
		return ErrGenerationGap
	}
	r.records[key] = cloneRecord(record)
	return nil
}

func (r *MemoryRepository) Acknowledge(_ context.Context, ack Ack) (AckResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := repositoryKey(ack.UserID, ack.DeviceID)
	record, exists := r.records[key]
	if !exists {
		return AckResult{}, ErrConfigNotFound
	}
	if ack.Generation < record.Config.Generation {
		return AckResult{}, ErrStaleGeneration
	}
	if ack.Generation > record.Config.Generation {
		return AckResult{}, ErrConfigNotFound
	}
	if ack.ConfigID != record.Config.ConfigID {
		return AckResult{}, ErrConfigMismatch
	}
	if record.Ack != nil {
		return AckResult{Ack: *record.Ack, Duplicate: true}, nil
	}
	now := r.clock().UTC()
	if ack.AppliedAt.IsZero() {
		ack.AppliedAt = now
	} else if ack.AppliedAt.After(now.Add(5 * time.Minute)) {
		return AckResult{}, ErrAppliedAtFuture
	}
	ack.ReceivedAt = now
	record.Ack = &ack
	r.records[key] = record
	return AckResult{Ack: ack}, nil
}

func repositoryKey(userID, deviceID string) string {
	return userID + "\x00" + deviceID
}

func cloneRecord(record Record) Record {
	clone := record
	clone.Config.WireGuard.Addresses = append([]string(nil), record.Config.WireGuard.Addresses...)
	clone.Config.WireGuard.Peers = append([]domain.ClientPeer(nil), record.Config.WireGuard.Peers...)
	for index := range clone.Config.WireGuard.Peers {
		clone.Config.WireGuard.Peers[index].AllowedIPs = append([]string(nil), record.Config.WireGuard.Peers[index].AllowedIPs...)
	}
	if record.Ack != nil {
		ack := *record.Ack
		clone.Ack = &ack
	}
	return clone
}
