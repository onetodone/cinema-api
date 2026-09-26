package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
	"uuid"
)

// graceKeyInfo is the HKDF context of the keys that seal refresh secrets. A new version makes every sealed secret
// unreadable, which costs nothing but the grace answers of the refreshes in flight at that moment.
const graceKeyInfo = "cinema-api refresh grace v1"

// sealer encrypts the current refresh secret of a session into its row, so that any API replica can hand it to a
// request that presented the previous secret within the grace window. A process-local memory of recent rotations
// works with one replica only, and Redis must not decide who stays logged in.
//
// Each session gets a key of its own, derived with HKDF-SHA256 from the access token signing key and the session
// id. Whoever holds that signing key can issue access tokens anyway, so it protects the sealed secrets as well as
// anything else would, and there is no second secret to configure. Per-session keys keep every key far below the
// 2^32 messages that AES-GCM with random nonces allows.
type sealer struct {
	secret []byte
}

func (s sealer) aead(sessionID uuid.UUID) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, s.secret, sessionID[:], graceKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive the grace key: %w", err)
	}
	block, err := aes.NewCipher(key) // a 32-byte key selects AES-256
	if err != nil {
		return nil, fmt.Errorf("create the grace cipher: %w", err)
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// seal encrypts and authenticates plaintext for the session. The random nonce is part of the result.
func (s sealer) seal(sessionID uuid.UUID, plaintext []byte) ([]byte, error) {
	aead, err := s.aead(sessionID)
	if err != nil {
		return nil, err
	}
	// The nonce must be nil: the AEAD draws a random one and puts it in front of the result.
	return aead.Seal(nil, nil, plaintext, nil), nil //nolint:gosec // G407: no fixed nonce, see NewGCMWithRandomNonce
}

// open decrypts what seal returned for the same session. It fails for anything else, including a sealed value
// of another session or one sealed under another signing key.
func (s sealer) open(sessionID uuid.UUID, sealed []byte) ([]byte, error) {
	aead, err := s.aead(sessionID)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nil, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("open the grace token: %w", err)
	}
	return plaintext, nil
}
