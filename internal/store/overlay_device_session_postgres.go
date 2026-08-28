package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const overlayDeviceCredentialColumns = `c.credential_id,c.verifier_sha256,c.user_uuid::text,c.network_id,c.device_id,c.status,c.scope,c.replaces_credential_id,c.replaced_by_credential_id,c.rotation_request_sha256,c.issued_at,c.expires_at,c.revoked_at,c.created_at`

func scanOverlayDeviceCredential(row rowScanner) (*OverlayDeviceCredential, error) {
	var credential OverlayDeviceCredential
	var scopeRaw []byte
	var replaces, replacedBy, rotationHash sql.NullString
	var revokedAt sql.NullTime
	err := row.Scan(&credential.ID, &credential.Verifier, &credential.UserID, &credential.NetworkID, &credential.DeviceID, &credential.Status, &scopeRaw, &replaces, &replacedBy, &rotationHash, &credential.IssuedAt, &credential.ExpiresAt, &revokedAt, &credential.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(scopeRaw, &credential.Scopes); err != nil {
		return nil, err
	}
	credential.ReplacesCredentialID = replaces.String
	credential.ReplacedByCredentialID = replacedBy.String
	credential.RotationRequestSHA256 = rotationHash.String
	credential.IssuedAt = credential.IssuedAt.UTC()
	credential.ExpiresAt = credential.ExpiresAt.UTC()
	credential.CreatedAt = credential.CreatedAt.UTC()
	if revokedAt.Valid {
		value := revokedAt.Time.UTC()
		credential.RevokedAt = &value
	}
	return &credential, nil
}

func (s *postgresStore) AuthenticateOverlayDeviceCredential(ctx context.Context, credentialID string, verifier []byte, now time.Time) (*OverlayDeviceCredential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+overlayDeviceCredentialColumns+`,d.status,d.network_id,u.active FROM public.overlay_device_credentials c JOIN public.overlay_devices d ON d.user_uuid=c.user_uuid AND d.network_id=c.network_id AND d.id=c.device_id JOIN public.users u ON u.uuid=c.user_uuid WHERE c.credential_id=$1`, strings.TrimSpace(credentialID))
	var credential OverlayDeviceCredential
	var scopeRaw []byte
	var replaces, replacedBy, rotationHash sql.NullString
	var revokedAt sql.NullTime
	var deviceStatus, deviceNetwork string
	var userActive bool
	err := row.Scan(&credential.ID, &credential.Verifier, &credential.UserID, &credential.NetworkID, &credential.DeviceID, &credential.Status, &scopeRaw, &replaces, &replacedBy, &rotationHash, &credential.IssuedAt, &credential.ExpiresAt, &revokedAt, &credential.CreatedAt, &deviceStatus, &deviceNetwork, &userActive)
	want := make([]byte, sha256.Size)
	if err == nil && len(credential.Verifier) == sha256.Size {
		want = credential.Verifier
	}
	if len(verifier) != sha256.Size || subtle.ConstantTimeCompare(want, verifier) != 1 || err != nil {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	if json.Unmarshal(scopeRaw, &credential.Scopes) != nil || credential.Status != OverlayDeviceCredentialActive || !credential.ExpiresAt.After(now.UTC()) || !exactScopes(credential.Scopes, overlayDeviceCredentialScopes) || deviceStatus != OverlayDeviceActive || deviceNetwork != credential.NetworkID || !userActive {
		return nil, ErrOverlayDeviceCredentialUnauthorized
	}
	credential.ReplacesCredentialID, credential.ReplacedByCredentialID, credential.RotationRequestSHA256 = replaces.String, replacedBy.String, rotationHash.String
	credential.IssuedAt, credential.ExpiresAt, credential.CreatedAt = credential.IssuedAt.UTC(), credential.ExpiresAt.UTC(), credential.CreatedAt.UTC()
	return &credential, nil
}

func (s *postgresStore) MintOverlayDeviceSession(ctx context.Context, credentialID string, session *OverlayEnrollmentSession, now time.Time, audit *AuditLog) error {
	if session == nil || len(session.TokenHash) != sha256.Size || session.ID == "" || !session.ExpiresAt.After(now.UTC()) || session.ExpiresAt.Sub(now.UTC()) > maxOverlayDeviceSessionTTL || !exactScopes(session.Scopes, overlayDeviceSessionScopes) {
		return errors.New("valid overlay device session is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var userID, networkID, deviceID, status, deviceStatus string
	var expiresAt time.Time
	var userActive bool
	err = tx.QueryRowContext(ctx, `SELECT c.user_uuid::text,c.network_id,c.device_id,c.status,c.expires_at,d.status,u.active FROM public.overlay_device_credentials c JOIN public.overlay_devices d ON d.user_uuid=c.user_uuid AND d.network_id=c.network_id AND d.id=c.device_id JOIN public.users u ON u.uuid=c.user_uuid WHERE c.credential_id=$1 FOR UPDATE OF c,d`, credentialID).Scan(&userID, &networkID, &deviceID, &status, &expiresAt, &deviceStatus, &userActive)
	if err != nil || status != OverlayDeviceCredentialActive || !expiresAt.After(now.UTC()) || deviceStatus != OverlayDeviceActive || !userActive {
		return ErrOverlayDeviceCredentialUnauthorized
	}
	scopeRaw, _ := json.Marshal(session.Scopes)
	_, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_device_sessions(session_id,token_hash,credential_id,user_uuid,network_id,device_id,scope,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`, session.ID, session.TokenHash, credentialID, userID, networkID, deviceID, string(scopeRaw), session.ExpiresAt.UTC(), now.UTC())
	if err != nil {
		return err
	}
	audit.Details["credential_id"] = credentialID
	audit.Details["device_id"] = deviceID
	audit.Details["network_id"] = networkID
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	session.CredentialID, session.UserID, session.NetworkID, session.DeviceID, session.CreatedAt = credentialID, userID, networkID, deviceID, now.UTC()
	return nil
}

func (s *postgresStore) GetOverlayDeviceSession(ctx context.Context, tokenHash []byte, now time.Time) (*OverlayEnrollmentSession, error) {
	var session OverlayEnrollmentSession
	var scopeRaw []byte
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `UPDATE public.overlay_device_sessions s SET last_used_at=$2 FROM public.overlay_device_credentials c,public.overlay_devices d,public.users u WHERE s.token_hash=$1 AND s.expires_at>$2 AND c.credential_id=s.credential_id AND c.status='active' AND c.expires_at>$2 AND d.user_uuid=s.user_uuid AND d.id=s.device_id AND d.network_id=s.network_id AND d.status='active' AND u.uuid=s.user_uuid AND u.active RETURNING s.session_id,s.credential_id,s.user_uuid::text,s.network_id,s.device_id,s.scope,s.expires_at,s.created_at,s.last_used_at`, tokenHash, now.UTC()).Scan(&session.ID, &session.CredentialID, &session.UserID, &session.NetworkID, &session.DeviceID, &scopeRaw, &session.ExpiresAt, &session.CreatedAt, &lastUsed)
	if err != nil {
		return nil, ErrOverlayEnrollmentNotFound
	}
	if err = json.Unmarshal(scopeRaw, &session.Scopes); err != nil || !exactScopes(session.Scopes, overlayDeviceSessionScopes) {
		return nil, ErrOverlayEnrollmentNotFound
	}
	if lastUsed.Valid {
		value := lastUsed.Time.UTC()
		session.LastUsedAt = &value
	}
	return &session, nil
}

func (s *postgresStore) RotateOverlayDeviceCredential(ctx context.Context, currentCredentialID string, successor *OverlayDeviceCredential, requestSHA256 string, now time.Time, audit *AuditLog) (*OverlayDeviceCredential, bool, error) {
	if successor == nil || !overlayDeviceCredentialIDStorePattern.MatchString(successor.ID) || len(successor.Verifier) != sha256.Size || !overlayDeviceRequestSHA256StorePattern.MatchString(requestSHA256) || !successor.ExpiresAt.After(now.UTC()) || successor.ExpiresAt.Sub(now.UTC()) > maxOverlayDeviceCredentialTTL || !exactScopes(successor.Scopes, overlayDeviceCredentialScopes) {
		return nil, false, errors.New("valid overlay device credential rotation is required")
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	current, err := scanOverlayDeviceCredential(tx.QueryRowContext(ctx, `SELECT `+overlayDeviceCredentialColumns+` FROM public.overlay_device_credentials c WHERE c.credential_id=$1 FOR UPDATE`, currentCredentialID))
	if err != nil {
		return nil, false, err
	}
	if current.Status == OverlayDeviceCredentialReplaced && current.ReplacedByCredentialID == successor.ID {
		stored, getErr := scanOverlayDeviceCredential(tx.QueryRowContext(ctx, `SELECT `+overlayDeviceCredentialColumns+` FROM public.overlay_device_credentials c WHERE c.credential_id=$1`, successor.ID))
		if getErr == nil && stored.RotationRequestSHA256 == requestSHA256 && subtle.ConstantTimeCompare(stored.Verifier, successor.Verifier) == 1 {
			return stored, true, tx.Commit()
		}
		return nil, false, ErrOverlayDeviceCredentialIdempotency
	}
	var deviceStatus string
	var userActive bool
	if err = tx.QueryRowContext(ctx, `SELECT d.status,u.active FROM public.overlay_devices d JOIN public.users u ON u.uuid=d.user_uuid WHERE d.user_uuid=$1 AND d.network_id=$2 AND d.id=$3 FOR UPDATE OF d`, current.UserID, current.NetworkID, current.DeviceID).Scan(&deviceStatus, &userActive); err != nil || current.Status != OverlayDeviceCredentialActive || !current.ExpiresAt.After(now.UTC()) || deviceStatus != OverlayDeviceActive || !userActive {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_device_credentials SET status='replaced' WHERE credential_id=$1 AND status='active'`, current.ID); err != nil {
		return nil, false, err
	}
	scopeRaw, _ := json.Marshal(successor.Scopes)
	err = tx.QueryRowContext(ctx, `INSERT INTO public.overlay_device_credentials(credential_id,verifier_sha256,user_uuid,network_id,device_id,status,scope,replaces_credential_id,rotation_request_sha256,issued_at,expires_at,created_at) VALUES($1,$2,$3,$4,$5,'active',$6::jsonb,$7,$8,$9,$10,$9) RETURNING created_at`, successor.ID, successor.Verifier, current.UserID, current.NetworkID, current.DeviceID, string(scopeRaw), current.ID, requestSHA256, now.UTC(), successor.ExpiresAt.UTC()).Scan(&successor.CreatedAt)
	if err != nil {
		return nil, false, ErrOverlayDeviceCredentialConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_device_credentials SET replaced_by_credential_id=$2 WHERE credential_id=$1`, current.ID, successor.ID); err != nil {
		return nil, false, err
	}
	successor.UserID, successor.NetworkID, successor.DeviceID, successor.Status = current.UserID, current.NetworkID, current.DeviceID, OverlayDeviceCredentialActive
	successor.ReplacesCredentialID, successor.RotationRequestSHA256, successor.IssuedAt = current.ID, requestSHA256, now.UTC()
	audit.Details["credential_id"], audit.Details["replaces_credential_id"], audit.Details["device_id"] = successor.ID, current.ID, current.DeviceID
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return cloneOverlayDeviceCredential(successor), false, nil
}

func (s *postgresStore) RevokeOverlayDeviceWithCredential(ctx context.Context, credentialID string, verifier []byte, requestSHA256, clientNonce string, now time.Time, audit *AuditLog) (*OverlayDeviceRevokeReceipt, bool, error) {
	if len(verifier) != sha256.Size || !overlayDeviceRequestSHA256StorePattern.MatchString(requestSHA256) || clientNonce == "" {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if err := validateJoinAudit(audit); err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	credential, err := scanOverlayDeviceCredential(tx.QueryRowContext(ctx, `SELECT `+overlayDeviceCredentialColumns+` FROM public.overlay_device_credentials c WHERE c.credential_id=$1 FOR UPDATE`, credentialID))
	want := make([]byte, sha256.Size)
	if err == nil && len(credential.Verifier) == sha256.Size {
		want = credential.Verifier
	}
	if len(verifier) != sha256.Size || subtle.ConstantTimeCompare(want, verifier) != 1 || err != nil {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	var storedHash, storedNonce, storedCredential string
	var payload []byte
	var receiptCreated time.Time
	err = tx.QueryRowContext(ctx, `SELECT credential_id,request_sha256,client_nonce::text,device_payload,created_at FROM public.overlay_device_revoke_receipts WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3 FOR UPDATE`, credential.UserID, credential.NetworkID, credential.DeviceID).Scan(&storedCredential, &storedHash, &storedNonce, &payload, &receiptCreated)
	if err == nil {
		if storedHash != requestSHA256 || storedNonce != clientNonce {
			return nil, false, ErrOverlayDeviceCredentialIdempotency
		}
		var device OverlayDevice
		if json.Unmarshal(payload, &device) != nil {
			return nil, false, ErrOverlayDeviceCredentialConflict
		}
		return &OverlayDeviceRevokeReceipt{UserID: credential.UserID, NetworkID: credential.NetworkID, DeviceID: credential.DeviceID, CredentialID: storedCredential, RequestSHA256: storedHash, ClientNonce: storedNonce, Device: device, CreatedAt: receiptCreated.UTC()}, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	device, err := s.lifecycleDeviceForUpdate(ctx, tx, credential.UserID, credential.NetworkID, credential.DeviceID)
	if err != nil {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	alreadyRevoked := device.Status == OverlayDeviceRevoked
	if device.Status == OverlayDeviceInactive || (!alreadyRevoked && (credential.Status != OverlayDeviceCredentialActive || !credential.ExpiresAt.After(now.UTC()))) {
		return nil, false, ErrOverlayDeviceCredentialUnauthorized
	}
	if !alreadyRevoked {
		var userActive bool
		if err = tx.QueryRowContext(ctx, `SELECT active FROM public.users WHERE uuid=$1`, credential.UserID).Scan(&userActive); err != nil || !userActive {
			return nil, false, ErrOverlayDeviceCredentialUnauthorized
		}
		device, err = scanOverlayDevice(tx.QueryRowContext(ctx, `UPDATE public.overlay_devices SET status='revoked',state_version=state_version+1,revoked_at=$4,revoked_reason='device_credential_leave',updated_at=$4 WHERE user_uuid=$1 AND network_id=$2 AND id=$3 RETURNING id,user_uuid,network_id,name,platform,hostname,wireguard_public_key,wireguard_address,created_at,updated_at,last_seen_at,status,state_version,key_version,revoked_at,revoked_reason`, credential.UserID, credential.NetworkID, credential.DeviceID, now.UTC()))
		if err != nil {
			return nil, false, err
		}
		if err = insertOverlayDeviceEventTx(ctx, tx, device, "revoked"); err != nil {
			return nil, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_device_credentials SET status='revoked',revoked_at=COALESCE(revoked_at,$4) WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3 AND status='active'`, credential.UserID, credential.NetworkID, credential.DeviceID, now.UTC()); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM public.overlay_device_sessions WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3`, credential.UserID, credential.NetworkID, credential.DeviceID); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM public.overlay_enrollment_sessions WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3`, credential.UserID, credential.NetworkID, credential.DeviceID); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_join_tokens SET revoked_at=COALESCE(revoked_at,$4) WHERE user_uuid=$1 AND network_id=$2 AND device_id=$3`, credential.UserID, credential.NetworkID, credential.DeviceID, now.UTC()); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_policy_reconcile_pending(network_id,last_error) VALUES($1,$2) ON CONFLICT(network_id) DO UPDATE SET last_error=EXCLUDED.last_error,updated_at=now()`, credential.NetworkID, "device credential revoke requires policy reconciliation"); err != nil {
		return nil, false, err
	}
	payload, _ = json.Marshal(device)
	receipt := &OverlayDeviceRevokeReceipt{UserID: credential.UserID, NetworkID: credential.NetworkID, DeviceID: credential.DeviceID, CredentialID: credential.ID, RequestSHA256: requestSHA256, ClientNonce: clientNonce, Device: *device}
	err = tx.QueryRowContext(ctx, `INSERT INTO public.overlay_device_revoke_receipts(user_uuid,network_id,device_id,credential_id,request_sha256,client_nonce,device_payload) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb) RETURNING created_at`, credential.UserID, credential.NetworkID, credential.DeviceID, credential.ID, requestSHA256, clientNonce, string(payload)).Scan(&receipt.CreatedAt)
	if err != nil {
		return nil, false, err
	}
	audit.Details["credential_id"], audit.Details["device_id"], audit.Details["network_id"] = credential.ID, credential.DeviceID, credential.NetworkID
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return receipt, alreadyRevoked, nil
}
