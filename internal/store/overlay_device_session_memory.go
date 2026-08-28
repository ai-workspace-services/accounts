package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"regexp"
	"strings"
	"time"
)

const maxOverlayDeviceCredentialTTL = 31 * 24 * time.Hour
const maxOverlayDeviceSessionTTL = 15 * time.Minute

var overlayDeviceCredentialScopes = []string{"overlay:session:mint", "overlay:credential:rotate", "overlay:device:revoke"}
var overlayDeviceSessionScopes = []string{"overlay:config:read", "overlay:config:ack"}
var overlayDeviceCredentialIDStorePattern = regexp.MustCompile(`^xdcid_[0-9a-f]{32}$`)
var overlayDeviceRequestSHA256StorePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func cloneOverlayDeviceCredential(src *OverlayDeviceCredential) *OverlayDeviceCredential {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Verifier = append([]byte(nil), src.Verifier...)
	clone.Scopes = append([]string(nil), src.Scopes...)
	if src.RevokedAt != nil {
		value := src.RevokedAt.UTC()
		clone.RevokedAt = &value
	}
	return &clone
}

func cloneOverlayDeviceRevokeReceipt(src *OverlayDeviceRevokeReceipt) *OverlayDeviceRevokeReceipt {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Device = *cloneOverlayDevice(&src.Device)
	return &clone
}

func exactScopes(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func (s *memoryStore) activeDeviceCredentialLocked(credentialID string, now time.Time) (*OverlayDeviceCredential, error) {
	credential := s.overlayDeviceCredentials[strings.TrimSpace(credentialID)]
	if credential == nil || credential.Status != OverlayDeviceCredentialActive || !credential.ExpiresAt.After(now.UTC()) || !exactScopes(credential.Scopes, overlayDeviceCredentialScopes) {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	device := s.overlayDevices[overlayDeviceKey(credential.UserID, credential.DeviceID)]
	user := s.byID[credential.UserID]
	if device == nil || device.Status != OverlayDeviceActive || device.NetworkID != credential.NetworkID || user == nil || !user.Active {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	return credential, nil
}

func (s *memoryStore) AuthenticateOverlayDeviceCredential(_ context.Context, credentialID string, verifier []byte, now time.Time) (*OverlayDeviceCredential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	credential := s.overlayDeviceCredentials[strings.TrimSpace(credentialID)]
	want := make([]byte, sha256.Size)
	if credential != nil && len(credential.Verifier) == sha256.Size {
		want = credential.Verifier
	}
	if len(verifier) != sha256.Size || subtle.ConstantTimeCompare(want, verifier) != 1 || credential == nil {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	active, err := s.activeDeviceCredentialLocked(credential.ID, now)
	if err != nil {
		return nil, err
	}
	return cloneOverlayDeviceCredential(active), nil
}

func (s *memoryStore) MintOverlayDeviceSession(_ context.Context, credentialID string, session *OverlayEnrollmentSession, now time.Time, audit *AuditLog) error {
	if session == nil || len(session.TokenHash) != sha256.Size || session.ID == "" || !session.ExpiresAt.After(now.UTC()) || session.ExpiresAt.Sub(now.UTC()) > maxOverlayDeviceSessionTTL || !exactScopes(session.Scopes, overlayDeviceSessionScopes) {
		return errors.New("valid overlay device session is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, err := s.activeDeviceCredentialLocked(credentialID, now)
	if err != nil {
		return err
	}
	hashKey := string(session.TokenHash)
	if s.overlayDeviceSessions[hashKey] != nil || s.overlayEnrollments[hashKey] != nil {
		return ErrOverlayDeviceCredentialConflict
	}
	session.CredentialID = credential.ID
	session.UserID = credential.UserID
	session.NetworkID = credential.NetworkID
	session.DeviceID = credential.DeviceID
	session.CreatedAt = now.UTC()
	s.overlayDeviceSessions[hashKey] = cloneOverlayEnrollment(session)
	audit.CreatedAt = now.UTC()
	audit.Details["credential_id"] = credential.ID
	audit.Details["device_id"] = credential.DeviceID
	audit.Details["network_id"] = credential.NetworkID
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return nil
}

func (s *memoryStore) GetOverlayDeviceSession(_ context.Context, tokenHash []byte, now time.Time) (*OverlayEnrollmentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.overlayDeviceSessions[string(tokenHash)]
	if session == nil || !session.ExpiresAt.After(now.UTC()) {
		return nil, ErrOverlayEnrollmentNotFound
	}
	if _, err := s.activeDeviceCredentialLocked(session.CredentialID, now); err != nil {
		return nil, ErrOverlayEnrollmentNotFound
	}
	used := now.UTC()
	session.LastUsedAt = &used
	return cloneOverlayEnrollment(session), nil
}

func (s *memoryStore) RotateOverlayDeviceCredential(_ context.Context, currentCredentialID string, successor *OverlayDeviceCredential, requestSHA256 string, now time.Time, audit *AuditLog) (*OverlayDeviceCredential, bool, error) {
	if successor == nil || !overlayDeviceCredentialIDStorePattern.MatchString(successor.ID) || len(successor.Verifier) != sha256.Size || !overlayDeviceRequestSHA256StorePattern.MatchString(requestSHA256) || !successor.ExpiresAt.After(now.UTC()) || successor.ExpiresAt.Sub(now.UTC()) > maxOverlayDeviceCredentialTTL || !exactScopes(successor.Scopes, overlayDeviceCredentialScopes) {
		return nil, false, errors.New("valid overlay device credential rotation is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.overlayDeviceCredentials[strings.TrimSpace(currentCredentialID)]
	if current == nil {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if current.Status == OverlayDeviceCredentialReplaced && current.ReplacedByCredentialID == successor.ID {
		stored := s.overlayDeviceCredentials[successor.ID]
		if stored != nil && stored.RotationRequestSHA256 == requestSHA256 && subtle.ConstantTimeCompare(stored.Verifier, successor.Verifier) == 1 {
			return cloneOverlayDeviceCredential(stored), true, nil
		}
		return nil, false, ErrOverlayDeviceCredentialIdempotency
	}
	if _, err := s.activeDeviceCredentialLocked(current.ID, now); err != nil {
		return nil, false, err
	}
	if s.overlayDeviceCredentials[successor.ID] != nil {
		return nil, false, ErrOverlayDeviceCredentialConflict
	}
	for _, candidate := range s.overlayDeviceCredentials {
		if len(candidate.Verifier) == sha256.Size && subtle.ConstantTimeCompare(candidate.Verifier, successor.Verifier) == 1 {
			return nil, false, ErrOverlayDeviceCredentialConflict
		}
		if candidate.UserID == current.UserID && candidate.DeviceID == current.DeviceID && candidate.Status == OverlayDeviceCredentialActive && candidate.ID != current.ID {
			return nil, false, ErrOverlayDeviceCredentialConflict
		}
	}
	stored := cloneOverlayDeviceCredential(successor)
	stored.UserID, stored.NetworkID, stored.DeviceID = current.UserID, current.NetworkID, current.DeviceID
	stored.Status = OverlayDeviceCredentialActive
	stored.ReplacesCredentialID = current.ID
	stored.RotationRequestSHA256 = requestSHA256
	stored.IssuedAt, stored.CreatedAt = now.UTC(), now.UTC()
	s.overlayDeviceCredentials[stored.ID] = stored
	current.Status = OverlayDeviceCredentialReplaced
	current.ReplacedByCredentialID = stored.ID
	audit.CreatedAt = now.UTC()
	audit.Details["credential_id"] = stored.ID
	audit.Details["replaces_credential_id"] = current.ID
	audit.Details["device_id"] = current.DeviceID
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayDeviceCredential(stored), false, nil
}

func (s *memoryStore) RevokeOverlayDeviceWithCredential(_ context.Context, credentialID string, verifier []byte, requestSHA256, clientNonce string, now time.Time, audit *AuditLog) (*OverlayDeviceRevokeReceipt, bool, error) {
	if len(verifier) != sha256.Size || !overlayDeviceRequestSHA256StorePattern.MatchString(requestSHA256) || strings.TrimSpace(clientNonce) == "" {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	credential := s.overlayDeviceCredentials[strings.TrimSpace(credentialID)]
	want := make([]byte, sha256.Size)
	if credential != nil && len(credential.Verifier) == sha256.Size {
		want = credential.Verifier
	}
	if subtle.ConstantTimeCompare(want, verifier) != 1 || credential == nil {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	receiptKey := overlayDeviceKey(credential.UserID, credential.DeviceID)
	if receipt := s.overlayDeviceRevokeReceipts[receiptKey]; receipt != nil {
		if receipt.RequestSHA256 != requestSHA256 || receipt.ClientNonce != clientNonce {
			return nil, false, ErrOverlayDeviceCredentialIdempotency
		}
		return cloneOverlayDeviceRevokeReceipt(receipt), true, nil
	}
	device := s.overlayDevices[receiptKey]
	if device == nil || device.NetworkID != credential.NetworkID {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	alreadyRevoked := device.Status == OverlayDeviceRevoked
	if device.Status == OverlayDeviceInactive || (!alreadyRevoked && (credential.Status != OverlayDeviceCredentialActive || !credential.ExpiresAt.After(now.UTC()))) {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if !alreadyRevoked {
		user := s.byID[credential.UserID]
		if user == nil || !user.Active {
			return nil, false, ErrOverlayDeviceCredentialUnauthorized
		}
		revokedAt := now.UTC()
		device.Status = OverlayDeviceRevoked
		device.StateVersion++
		device.RevokedAt = &revokedAt
		device.RevokedReason = "device_credential_leave"
		device.UpdatedAt = revokedAt
		s.appendOverlayDeviceEventLocked(device, "revoked", revokedAt)
	}
	for _, candidate := range s.overlayDeviceCredentials {
		if candidate.UserID == credential.UserID && candidate.DeviceID == credential.DeviceID && candidate.Status == OverlayDeviceCredentialActive {
			revokedAt := now.UTC()
			candidate.Status = OverlayDeviceCredentialRevoked
			candidate.RevokedAt = &revokedAt
		}
	}
	for hashKey, session := range s.overlayDeviceSessions {
		if session.UserID == credential.UserID && session.DeviceID == credential.DeviceID {
			delete(s.overlayDeviceSessions, hashKey)
		}
	}
	for hashKey, session := range s.overlayEnrollments {
		if session.UserID == credential.UserID && session.DeviceID == credential.DeviceID {
			delete(s.overlayEnrollments, hashKey)
		}
	}
	for _, token := range s.overlayJoinTokens {
		if token.UserID == credential.UserID && token.NetworkID == credential.NetworkID && token.DeviceID == credential.DeviceID && token.RevokedAt == nil {
			revokedAt := now.UTC()
			token.RevokedAt = &revokedAt
		}
	}
	s.overlayPolicyPending[credential.NetworkID] = OverlayPolicyReconcilePending{NetworkID: credential.NetworkID, LastError: "device credential revoke requires policy reconciliation", UpdatedAt: now.UTC()}
	receipt := &OverlayDeviceRevokeReceipt{UserID: credential.UserID, NetworkID: credential.NetworkID, DeviceID: credential.DeviceID, CredentialID: credential.ID, RequestSHA256: requestSHA256, ClientNonce: clientNonce, Device: *cloneOverlayDevice(device), CreatedAt: now.UTC()}
	s.overlayDeviceRevokeReceipts[receiptKey] = receipt
	audit.CreatedAt = now.UTC()
	audit.Details["credential_id"] = credential.ID
	audit.Details["device_id"] = credential.DeviceID
	audit.Details["network_id"] = credential.NetworkID
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	return cloneOverlayDeviceRevokeReceipt(receipt), alreadyRevoked, nil
}
