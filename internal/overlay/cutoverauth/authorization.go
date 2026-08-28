// Package cutoverauth implements the Accounts-issued, node-bound authority
// token consumed by the Gateway accounts-only readiness verifier. It is
// intentionally separate from SignedConfig and GatewaySnapshot signing keys.
package cutoverauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"account/internal/overlay/domain"
)

const (
	SchemaVersion = 1
	Kind          = "xconnect.accounts-only-cutover-authorization"
	RequestedMode = "accounts-only"
)

type ReconcileEvidence struct {
	Processed int `json:"processed"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

type Authorization struct {
	SchemaVersion    int               `json:"schema_version"`
	Kind             string            `json:"kind"`
	RequestedMode    string            `json:"requested_mode"`
	NodeID           string            `json:"node_id"`
	NetworkID        string            `json:"network_id"`
	Generation       uint64            `json:"generation"`
	SnapshotID       string            `json:"snapshot_id"`
	BaselineSHA256   string            `json:"baseline_sha256"`
	ProjectionSHA256 string            `json:"projection_sha256"`
	PolicySHA256     string            `json:"policy_sha256"`
	Reconcile        ReconcileEvidence `json:"reconcile"`
	IssuedAt         time.Time         `json:"issued_at"`
	ExpiresAt        time.Time         `json:"expires_at"`
	Signature        domain.Signature  `json:"signature"`
}

// SigningBytes is byte-for-byte compatible with Playbooks Batch06
// CutoverAuthorization.SigningBytes. Do not reorder fields.
func (a Authorization) SigningBytes() ([]byte, error) {
	payload := struct {
		SchemaVersion    int               `json:"schema_version"`
		Kind             string            `json:"kind"`
		RequestedMode    string            `json:"requested_mode"`
		NodeID           string            `json:"node_id"`
		NetworkID        string            `json:"network_id"`
		Generation       uint64            `json:"generation"`
		SnapshotID       string            `json:"snapshot_id"`
		BaselineSHA256   string            `json:"baseline_sha256"`
		ProjectionSHA256 string            `json:"projection_sha256"`
		PolicySHA256     string            `json:"policy_sha256"`
		Reconcile        ReconcileEvidence `json:"reconcile"`
		IssuedAt         time.Time         `json:"issued_at"`
		ExpiresAt        time.Time         `json:"expires_at"`
	}{a.SchemaVersion, a.Kind, a.RequestedMode, a.NodeID, a.NetworkID, a.Generation, a.SnapshotID, a.BaselineSHA256, a.ProjectionSHA256, a.PolicySHA256, a.Reconcile, a.IssuedAt, a.ExpiresAt}
	return json.Marshal(payload)
}

type Signer struct {
	private ed25519.PrivateKey
	keyID   string
}

func NewSigner(private ed25519.PrivateKey, keyID string) (*Signer, error) {
	if len(private) != ed25519.PrivateKeySize || keyID == "" {
		return nil, errors.New("cutover authorization requires an Ed25519 private key and key id")
	}
	if err := (domain.Signature{Algorithm: domain.SignatureEd25519, KeyID: keyID, Value: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))}).Validate(); err != nil {
		return nil, errors.New("cutover authorization key id is invalid")
	}
	return &Signer{private: append(ed25519.PrivateKey(nil), private...), keyID: keyID}, nil
}
func NewSignerFromBase64(encoded, keyID string) (*Signer, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("decode cutover authorization Ed25519 private key")
	}
	if len(raw) == ed25519.SeedSize {
		return NewSigner(ed25519.NewKeyFromSeed(raw), keyID)
	}
	return NewSigner(ed25519.PrivateKey(raw), keyID)
}
func (s *Signer) Sign(a *Authorization) error {
	if s == nil || a == nil {
		return errors.New("cutover authorization signer is not configured")
	}
	payload, err := a.SigningBytes()
	if err != nil {
		return err
	}
	a.Signature = domain.Signature{Algorithm: domain.SignatureEd25519, KeyID: s.keyID, Value: base64.StdEncoding.EncodeToString(ed25519.Sign(s.private, payload))}
	return nil
}
