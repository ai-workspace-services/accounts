package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
)

func cloneOverlayJoinToken(src *OverlayJoinToken) *OverlayJoinToken {
	if src == nil {
		return nil
	}
	clone := *src
	clone.TokenHash = append([]byte(nil), src.TokenHash...)
	if src.RevokedAt != nil {
		value := src.RevokedAt.UTC()
		clone.RevokedAt = &value
	}
	if src.LastExchangedAt != nil {
		value := src.LastExchangedAt.UTC()
		clone.LastExchangedAt = &value
	}
	return &clone
}

func cloneOverlayEnrollment(src *OverlayEnrollmentSession) *OverlayEnrollmentSession {
	if src == nil {
		return nil
	}
	clone := *src
	clone.TokenHash = append([]byte(nil), src.TokenHash...)
	clone.Scopes = append([]string(nil), src.Scopes...)
	if src.LastUsedAt != nil {
		value := src.LastUsedAt.UTC()
		clone.LastUsedAt = &value
	}
	return &clone
}

func validateJoinAudit(audit *AuditLog) error {
	if audit == nil || strings.TrimSpace(audit.Action) == "" {
		return errors.New("overlay join audit entry is required")
	}
	if audit.Details == nil {
		audit.Details = map[string]any{}
	}
	if audit.UUID == "" {
		audit.UUID = uuid.NewString()
	}
	return nil
}

func (s *memoryStore) CreateOverlayJoinToken(_ context.Context, token *OverlayJoinToken, audit *AuditLog) error {
	if token == nil || token.ID == "" || len(token.TokenHash) != sha256.Size || token.UserID == "" || token.NetworkID == "" || token.RemainingUses != 1 || token.ExpiresAt.IsZero() {
		return errors.New("valid overlay join token is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hashKey := string(token.TokenHash)
	if _, exists := s.overlayJoinTokens[token.ID]; exists || s.overlayJoinTokenHashes[hashKey] != "" {
		return errors.New("overlay join token already exists")
	}
	stored := cloneOverlayJoinToken(token)
	stored.CreatedAt = time.Now().UTC()
	s.overlayJoinTokens[stored.ID] = stored
	s.overlayJoinTokenHashes[hashKey] = stored.ID
	audit.CreatedAt = stored.CreatedAt
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	*token = *cloneOverlayJoinToken(stored)
	return nil
}

func (s *memoryStore) RevokeOverlayJoinToken(_ context.Context, userID, tokenID string, revokedAt time.Time, audit *AuditLog) error {
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.overlayJoinTokens[strings.TrimSpace(tokenID)]
	if token == nil || token.UserID != strings.TrimSpace(userID) {
		return ErrOverlayJoinTokenNotFound
	}
	if token.RevokedAt == nil {
		value := revokedAt.UTC()
		token.RevokedAt = &value
	}
	audit.CreatedAt = revokedAt.UTC()
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}

func validateJoinTokenForExchange(token *OverlayJoinToken, exchange *OverlayJoinExchange, now time.Time) error {
	if token == nil {
		return ErrOverlayJoinTokenNotFound
	}
	if token.RevokedAt != nil {
		return ErrOverlayJoinTokenRevoked
	}
	if !token.ExpiresAt.After(now.UTC()) {
		return ErrOverlayJoinTokenExpired
	}
	if token.RemainingUses <= 0 {
		return ErrOverlayJoinTokenExhausted
	}
	if token.DeviceID != "" && token.DeviceID != exchange.Device.ID {
		return ErrOverlayJoinConstraint
	}
	if token.Platform != "" && token.Platform != exchange.Device.Platform {
		return ErrOverlayJoinConstraint
	}
	return nil
}

func (s *memoryStore) ExchangeOverlayJoinToken(_ context.Context, exchange *OverlayJoinExchange, audit *AuditLog) error {
	if exchange == nil || len(exchange.JoinTokenHash) != sha256.Size || len(exchange.Enrollment.TokenHash) != sha256.Size || len(exchange.DeviceCredential.Verifier) != sha256.Size || !overlayDeviceCredentialIDStorePattern.MatchString(exchange.DeviceCredential.ID) || !exchange.DeviceCredential.ExpiresAt.After(exchange.Enrollment.CreatedAt.UTC()) || exchange.DeviceCredential.ExpiresAt.Sub(exchange.Enrollment.CreatedAt.UTC()) > maxOverlayDeviceCredentialTTL || exchange.Device.ID == "" || exchange.Device.Platform == "" || !exactScopes(exchange.DeviceCredential.Scopes, overlayDeviceCredentialScopes) {
		return errors.New("valid overlay join exchange is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tokenID := s.overlayJoinTokenHashes[string(exchange.JoinTokenHash)]
	token := s.overlayJoinTokens[tokenID]
	now := exchange.Enrollment.CreatedAt.UTC()
	if err := validateJoinTokenForExchange(token, exchange, now); err != nil {
		return err
	}
	if _, exists := s.overlayEnrollments[string(exchange.Enrollment.TokenHash)]; exists {
		return errors.New("overlay enrollment session already exists")
	}
	for _, enrollment := range s.overlayEnrollments {
		if enrollment.JoinTokenID == token.ID && enrollment.DeviceID == exchange.Device.ID {
			return ErrOverlayJoinReplay
		}
	}
	for _, credential := range s.overlayDeviceCredentials {
		if len(credential.Verifier) == sha256.Size && subtle.ConstantTimeCompare(credential.Verifier, exchange.DeviceCredential.Verifier) == 1 {
			return ErrOverlayJoinDeviceConflict
		}
		if credential.UserID == token.UserID && credential.DeviceID == exchange.Device.ID && credential.Status == OverlayDeviceCredentialActive {
			return ErrOverlayJoinDeviceConflict
		}
	}
	if s.overlayDeviceCredentials[exchange.DeviceCredential.ID] != nil {
		return ErrOverlayJoinDeviceConflict
	}
	deviceKey := overlayDeviceKey(token.UserID, exchange.Device.ID)
	if existing := s.overlayDevices[deviceKey]; existing != nil {
		if existing.Status == OverlayDeviceRevoked {
			return ErrOverlayDeviceRevoked
		}
		if existing.Status != "" && existing.Status != OverlayDeviceActive {
			return ErrOverlayJoinDeviceConflict
		}
		if existing.NetworkID != token.NetworkID || existing.WireGuardPublicKey != exchange.Device.WireGuardPublicKey || existing.Platform != exchange.Device.Platform {
			return ErrOverlayJoinDeviceConflict
		}
		exchange.Device.WireGuardAddress = existing.WireGuardAddress
	} else {
		used := make(map[string]bool)
		for _, device := range s.overlayDevices {
			if device.NetworkID == token.NetworkID {
				used[device.WireGuardAddress] = true
			}
		}
		address, err := allocateOverlayJoinAddress(token.UserID, exchange.Device.ID, exchange.AddressPrefix, exchange.AddressStartHost, exchange.AddressEndHost, used)
		if err != nil {
			return err
		}
		exchange.Device.WireGuardAddress = address
		if err := s.claimOverlayDeviceKeyLocked(token.NetworkID, exchange.Device.WireGuardPublicKey, token.UserID, exchange.Device.ID, 1, now); err != nil {
			return ErrOverlayJoinDeviceConflict
		}
	}
	exchange.Device.UserID = token.UserID
	exchange.Device.NetworkID = token.NetworkID
	exchange.Device.CreatedAt = now
	exchange.Device.UpdatedAt = now
	lastSeen := now
	exchange.Device.LastSeenAt = &lastSeen
	exchange.Device.Status = OverlayDeviceActive
	exchange.Device.StateVersion = 1
	exchange.Device.KeyVersion = 1
	s.overlayDevices[deviceKey] = cloneOverlayDevice(&exchange.Device)
	s.appendOverlayDeviceEventLocked(&exchange.Device, "registered", now)

	token.RemainingUses--
	token.LastExchangedAt = &now
	exchange.Enrollment.JoinTokenID = token.ID
	exchange.Enrollment.UserID = token.UserID
	exchange.Enrollment.NetworkID = token.NetworkID
	exchange.Enrollment.DeviceID = exchange.Device.ID
	exchange.Enrollment.Platform = exchange.Device.Platform
	exchange.Enrollment.WireGuardPublicKey = exchange.Device.WireGuardPublicKey
	exchange.Enrollment.Scopes = []string{"overlay:config:read", "overlay:config:ack", "overlay:device:revoke"}
	s.overlayEnrollments[string(exchange.Enrollment.TokenHash)] = cloneOverlayEnrollment(&exchange.Enrollment)
	exchange.DeviceCredential.UserID = token.UserID
	exchange.DeviceCredential.NetworkID = token.NetworkID
	exchange.DeviceCredential.DeviceID = exchange.Device.ID
	exchange.DeviceCredential.Status = OverlayDeviceCredentialActive
	exchange.DeviceCredential.IssuedAt = now
	exchange.DeviceCredential.CreatedAt = now
	s.overlayDeviceCredentials[exchange.DeviceCredential.ID] = cloneOverlayDeviceCredential(&exchange.DeviceCredential)
	if audit.Details == nil {
		audit.Details = map[string]any{}
	}
	audit.Details["target_uuid"] = token.UserID
	audit.Details["join_token_id"] = token.ID
	audit.Details["enrollment_id"] = exchange.Enrollment.ID
	audit.Details["credential_id"] = exchange.DeviceCredential.ID
	audit.CreatedAt = now
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	issueAudit := &AuditLog{Action: AuditActionOverlayDeviceCredentialIssue, ActorUUID: token.UserID, CreatedAt: now, Details: map[string]any{"credential_id": exchange.DeviceCredential.ID, "device_id": exchange.Device.ID, "network_id": token.NetworkID}}
	if err := validateJoinAudit(issueAudit); err != nil {
		return err
	}
	s.auditLogs = append(s.auditLogs, cloneAuditLog(issueAudit))
	return nil
}

func (s *memoryStore) GetOverlayEnrollmentSession(_ context.Context, tokenHash []byte, now time.Time) (*OverlayEnrollmentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.overlayEnrollments[string(tokenHash)]
	if session == nil {
		return nil, ErrOverlayEnrollmentNotFound
	}
	if !session.ExpiresAt.After(now.UTC()) {
		return nil, ErrOverlayEnrollmentExpired
	}
	device := s.overlayDevices[overlayDeviceKey(session.UserID, session.DeviceID)]
	if device == nil || device.Status != OverlayDeviceActive || device.WireGuardPublicKey != session.WireGuardPublicKey {
		return nil, ErrOverlayEnrollmentNotFound
	}
	used := now.UTC()
	session.LastUsedAt = &used
	clone := cloneOverlayEnrollment(session)
	if len(clone.Scopes) == 0 {
		clone.Scopes = []string{"overlay:config:read", "overlay:config:ack", "overlay:device:revoke"}
	}
	return clone, nil
}

func allocateOverlayJoinAddress(userID, deviceID, prefix string, startHost, endHost int, used map[string]bool) (string, error) {
	if strings.TrimSpace(prefix) == "" || net.ParseIP(prefix+".1").To4() == nil || startHost <= 0 || endHost < startHost || endHost > 254 {
		return "", errors.New("invalid overlay address pool")
	}
	sum := sha256.Sum256([]byte(userID + "\x00" + deviceID))
	span := endHost - startHost + 1
	preferred := startHost + int(sum[0])%span
	for offset := 0; offset < span; offset++ {
		host := startHost + (preferred-startHost+offset)%span
		candidate := fmt.Sprintf("%s.%d/32", prefix, host)
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", errors.New("overlay address pool exhausted")
}
