package projection

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"account/internal/overlay/domain"
)

const defaultConfigLifetime = 24 * time.Hour

type Input struct {
	UserID                     string
	SourceRevision             string
	NetworkID                  string
	DeviceID                   string
	DeviceAddresses            []string
	GatewayID                  string
	GatewayPublicKey           string
	AllowedIPs                 []string
	InterfaceName              string
	MTU                        int
	LoopbackPort               int
	PersistentKeepaliveSeconds int
	RemoteHost                 string
	RemotePort                 int
	ServerName                 string
	AuthID                     string
}

type Service struct {
	repository Repository
	signer     Signer
	clock      func() time.Time
	lifetime   time.Duration
}

func NewService(repository Repository, signer Signer, clock func() time.Time, lifetime time.Duration) (*Service, error) {
	if repository == nil || signer == nil {
		return nil, errors.New("projection repository and signer are required")
	}
	if clock == nil {
		clock = time.Now
	}
	if lifetime <= 0 {
		lifetime = defaultConfigLifetime
	}
	return &Service{repository: repository, signer: signer, clock: clock, lifetime: lifetime}, nil
}

func (s *Service) Project(ctx context.Context, input Input) (domain.SignedConfig, error) {
	if strings.TrimSpace(input.UserID) == "" || strings.TrimSpace(input.SourceRevision) == "" {
		return domain.SignedConfig{}, errors.New("projection user and source revision are required")
	}
	for attempt := 0; attempt < 3; attempt++ {
		now := s.clock().UTC().Truncate(time.Second)
		latest, exists, err := s.repository.Latest(ctx, input.UserID, input.DeviceID)
		if err != nil {
			return domain.SignedConfig{}, err
		}
		if exists && latest.SourceRevision == input.SourceRevision && latest.Config.ExpiresAt.After(now) {
			if err := VerifySignedConfig(latest.Config, s.signer, now); err != nil {
				return domain.SignedConfig{}, fmt.Errorf("verify stored signed config: %w", err)
			}
			return latest.Config, nil
		}
		generation := uint64(1)
		if exists {
			generation = latest.Config.Generation + 1
		}
		config := domain.SignedConfig{
			SchemaVersion: domain.SchemaVersionV1,
			ConfigID:      deriveConfigID(input.UserID, input.DeviceID, input.NetworkID, generation),
			NetworkID:     input.NetworkID,
			DeviceID:      input.DeviceID,
			Generation:    generation,
			IssuedAt:      now,
			ExpiresAt:     now.Add(s.lifetime).UTC().Truncate(time.Second),
			ProxyCore:     domain.ProxyCoreXray,
			Transport: domain.ClientTransport{
				Kind:     domain.TransportVLESS,
				Loopback: domain.Endpoint{Host: domain.LoopbackHost, Port: input.LoopbackPort},
				Remote:   domain.RemoteEndpoint{Host: input.RemoteHost, Port: input.RemotePort, ServerName: firstNonEmpty(input.ServerName, input.RemoteHost)},
				AuthID:   input.AuthID,
			},
			WireGuard: domain.ClientWireGuard{
				InterfaceName: input.InterfaceName,
				Addresses:     append([]string(nil), input.DeviceAddresses...),
				MTU:           input.MTU,
				Peers: []domain.ClientPeer{{
					GatewayID: input.GatewayID, PublicKey: input.GatewayPublicKey,
					AllowedIPs:                 append([]string(nil), input.AllowedIPs...),
					Endpoint:                   domain.Endpoint{Host: domain.LoopbackHost, Port: input.LoopbackPort},
					PersistentKeepaliveSeconds: input.PersistentKeepaliveSeconds,
				}},
			},
		}
		payload, err := config.SigningBytes()
		if err != nil {
			return domain.SignedConfig{}, err
		}
		config.Signature, err = s.signer.Sign(payload)
		if err != nil {
			return domain.SignedConfig{}, err
		}
		if err := VerifySignedConfig(config, s.signer, now); err != nil {
			return domain.SignedConfig{}, fmt.Errorf("self-verify projected config: %w", err)
		}
		raw, err := json.Marshal(config)
		if err != nil {
			return domain.SignedConfig{}, err
		}
		if _, err := domain.DecodeSignedConfig(raw); err != nil {
			return domain.SignedConfig{}, fmt.Errorf("strict projection validation: %w", err)
		}
		err = s.repository.Save(ctx, Record{UserID: input.UserID, SourceRevision: input.SourceRevision, Config: config})
		if errors.Is(err, ErrStaleGeneration) {
			continue
		}
		if err != nil {
			return domain.SignedConfig{}, err
		}
		return config, nil
	}
	return domain.SignedConfig{}, errors.New("projection generation contention")
}

func (s *Service) Acknowledge(ctx context.Context, ack Ack) (AckResult, error) {
	return s.repository.Acknowledge(ctx, ack)
}

func deriveConfigID(userID, deviceID, networkID string, generation uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", userID, deviceID, networkID, generation)))
	return "cfg_" + hex.EncodeToString(sum[:12])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
