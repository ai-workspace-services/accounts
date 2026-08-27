package projection

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"account/internal/overlay/domain"
)

var (
	ErrConfigExpired    = errors.New("signed config is expired")
	ErrInvalidSignature = errors.New("signed config signature is invalid")
)

type Signer interface {
	Sign(payload []byte) (domain.Signature, error)
	Verify(payload []byte, signature domain.Signature) error
}

type PublicSigningKey struct {
	KeyID     string     `json:"key_id"`
	Algorithm string     `json:"algorithm"`
	PublicKey string     `json:"public_key"`
	Status    string     `json:"status"`
	NotBefore time.Time  `json:"not_before"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

type SigningKeyProvider interface {
	PublicSigningKeys() []PublicSigningKey
}

type KeyRingEntry struct {
	KeyID      string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	Status     string
	NotBefore  time.Time
	NotAfter   *time.Time
}

// Ed25519KeyRing signs only with the current private key and verifies current
// and previous public keys during their declared windows.
type Ed25519KeyRing struct {
	currentID string
	entries   map[string]KeyRingEntry
	clock     func() time.Time
}

func NewEd25519KeyRingWithCurrentSigner(current *Ed25519Signer, notBefore time.Time, notAfter *time.Time, previous []KeyRingEntry, clock func() time.Time) (*Ed25519KeyRing, error) {
	if current == nil {
		return nil, errors.New("current Ed25519 signer is required")
	}
	entries := []KeyRingEntry{{
		KeyID: current.keyID, PublicKey: current.publicKey, PrivateKey: current.privateKey,
		Status: "current", NotBefore: notBefore, NotAfter: notAfter,
	}}
	entries = append(entries, previous...)
	return NewEd25519KeyRing(entries, current.keyID, clock)
}

func NewEd25519KeyRing(entries []KeyRingEntry, currentID string, clock func() time.Time) (*Ed25519KeyRing, error) {
	if clock == nil {
		clock = time.Now
	}
	ring := &Ed25519KeyRing{currentID: strings.TrimSpace(currentID), entries: make(map[string]KeyRingEntry), clock: clock}
	for _, entry := range entries {
		entry.KeyID = strings.TrimSpace(entry.KeyID)
		if entry.KeyID == "" || len(entry.PublicKey) != ed25519.PublicKeySize {
			return nil, errors.New("key ring entries require a key id and Ed25519 public key")
		}
		if _, exists := ring.entries[entry.KeyID]; exists {
			return nil, errors.New("key ring key ids must be unique")
		}
		if entry.Status != "current" && entry.Status != "previous" {
			return nil, fmt.Errorf("key ring key %q has invalid status", entry.KeyID)
		}
		if entry.KeyID == ring.currentID && entry.Status != "current" {
			return nil, errors.New("key ring current key must have current status")
		}
		if entry.KeyID != ring.currentID && entry.Status != "previous" {
			return nil, errors.New("non-current key ring entries must have previous status")
		}
		if entry.KeyID != ring.currentID && len(entry.PrivateKey) != 0 {
			return nil, errors.New("previous key ring entries must not contain private keys")
		}
		if entry.NotAfter != nil && !entry.NotAfter.After(entry.NotBefore) {
			return nil, fmt.Errorf("key ring key %q not_after must be after not_before", entry.KeyID)
		}
		if entry.KeyID == ring.currentID && len(entry.PrivateKey) == ed25519.PrivateKeySize && !entry.PublicKey.Equal(entry.PrivateKey.Public()) {
			return nil, errors.New("current Ed25519 public key does not match private key")
		}
		entry.PublicKey = append(ed25519.PublicKey(nil), entry.PublicKey...)
		entry.PrivateKey = append(ed25519.PrivateKey(nil), entry.PrivateKey...)
		entry.NotBefore = entry.NotBefore.UTC()
		if entry.NotAfter != nil {
			value := entry.NotAfter.UTC()
			entry.NotAfter = &value
		}
		ring.entries[entry.KeyID] = entry
	}
	current, ok := ring.entries[ring.currentID]
	if !ok || len(current.PrivateKey) != ed25519.PrivateKeySize || current.Status != "current" {
		return nil, errors.New("key ring current key requires an injected Ed25519 private key and current status")
	}
	return ring, nil
}

func (r *Ed25519KeyRing) Sign(payload []byte) (domain.Signature, error) {
	entry, ok := r.entries[r.currentID]
	now := r.clock().UTC()
	if !ok || now.Before(entry.NotBefore) || (entry.NotAfter != nil && !now.Before(*entry.NotAfter)) {
		return domain.Signature{}, errors.New("current Ed25519 signing key is outside its validity window")
	}
	return domain.Signature{Algorithm: domain.SignatureEd25519, KeyID: entry.KeyID, Value: base64.StdEncoding.EncodeToString(ed25519.Sign(entry.PrivateKey, payload))}, nil
}

func (r *Ed25519KeyRing) Verify(payload []byte, signature domain.Signature) error {
	if err := signature.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	entry, ok := r.entries[signature.KeyID]
	now := r.clock().UTC()
	if !ok || now.Before(entry.NotBefore) || (entry.NotAfter != nil && !now.Before(*entry.NotAfter)) {
		return ErrInvalidSignature
	}
	value, err := base64.StdEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(entry.PublicKey, payload, value) {
		return ErrInvalidSignature
	}
	return nil
}

func (r *Ed25519KeyRing) PublicSigningKeys() []PublicSigningKey {
	keys := make([]PublicSigningKey, 0, len(r.entries))
	for _, entry := range r.entries {
		keys = append(keys, PublicSigningKey{
			KeyID: entry.KeyID, Algorithm: domain.SignatureEd25519, PublicKey: base64.StdEncoding.EncodeToString(entry.PublicKey),
			Status: entry.Status, NotBefore: entry.NotBefore, NotAfter: entry.NotAfter,
		})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Status != keys[j].Status {
			return keys[i].Status == "current"
		}
		return keys[i].KeyID < keys[j].KeyID
	})
	return keys
}

type Ed25519Signer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	keyID      string
}

func NewEd25519Signer(privateKey ed25519.PrivateKey, keyID string) (*Ed25519Signer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("Ed25519 private key must be 64 bytes")
	}
	testSignature := domain.Signature{
		Algorithm: domain.SignatureEd25519,
		KeyID:     keyID,
		Value:     base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	if err := testSignature.Validate(); err != nil {
		return nil, fmt.Errorf("invalid signing key metadata: %w", err)
	}
	privateCopy := append(ed25519.PrivateKey(nil), privateKey...)
	publicCopy := append(ed25519.PublicKey(nil), privateKey.Public().(ed25519.PublicKey)...)
	return &Ed25519Signer{privateKey: privateCopy, publicKey: publicCopy, keyID: keyID}, nil
}

func NewEd25519SignerFromBase64(encodedPrivateKey, keyID string) (*Ed25519Signer, error) {
	raw, err := base64.StdEncoding.DecodeString(encodedPrivateKey)
	if err != nil {
		return nil, errors.New("decode Ed25519 private key")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return NewEd25519Signer(ed25519.NewKeyFromSeed(raw), keyID)
	case ed25519.PrivateKeySize:
		return NewEd25519Signer(ed25519.PrivateKey(raw), keyID)
	default:
		return nil, errors.New("Ed25519 private key must encode a 32-byte seed or 64-byte private key")
	}
}

func (s *Ed25519Signer) Sign(payload []byte) (domain.Signature, error) {
	if s == nil || len(s.privateKey) != ed25519.PrivateKeySize {
		return domain.Signature{}, errors.New("Ed25519 signer is not configured")
	}
	value := ed25519.Sign(s.privateKey, payload)
	return domain.Signature{
		Algorithm: domain.SignatureEd25519,
		KeyID:     s.keyID,
		Value:     base64.StdEncoding.EncodeToString(value),
	}, nil
}

func (s *Ed25519Signer) Verify(payload []byte, signature domain.Signature) error {
	if s == nil || len(s.publicKey) != ed25519.PublicKeySize {
		return ErrInvalidSignature
	}
	if err := signature.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	if subtle.ConstantTimeCompare([]byte(signature.KeyID), []byte(s.keyID)) != 1 {
		return ErrInvalidSignature
	}
	value, err := base64.StdEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(s.publicKey, payload, value) {
		return ErrInvalidSignature
	}
	return nil
}

func VerifySignedConfig(config domain.SignedConfig, signer Signer, now time.Time) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if !config.ExpiresAt.After(now.UTC()) {
		return ErrConfigExpired
	}
	payload, err := config.SigningBytes()
	if err != nil {
		return fmt.Errorf("encode signed config payload: %w", err)
	}
	if err := signer.Verify(payload, config.Signature); err != nil {
		return err
	}
	return nil
}
