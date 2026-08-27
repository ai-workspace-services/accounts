package projection

import (
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
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
