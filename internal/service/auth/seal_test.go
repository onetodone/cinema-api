package auth

import (
	"bytes"
	"strings"
	"testing"
	"uuid"
)

func TestSealerRoundTrip(t *testing.T) {
	t.Parallel()

	s := sealer{secret: []byte(testSecret)}
	id := uuid.NewV7()
	secret := bytes.Repeat([]byte{7}, 32)

	a, err := s.seal(id, secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.seal(id, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("two seals of the same secret are equal: the nonce is not random")
	}
	if bytes.Contains(a, secret) {
		t.Error("the sealed value contains the secret")
	}
	if want := len(secret) + 28; len(a) != want { // 12-byte nonce + 16-byte tag
		t.Errorf("sealed length = %d, want %d", len(a), want)
	}

	for _, sealed := range [][]byte{a, b} {
		got, err := s.open(id, sealed)
		if err != nil || !bytes.Equal(got, secret) {
			t.Errorf("open = %x, %v; want the secret back", got, err)
		}
	}
}

func TestSealerRefusesWhatItDidNotSeal(t *testing.T) {
	t.Parallel()

	s := sealer{secret: []byte(testSecret)}
	id := uuid.NewV7()
	sealed, err := s.seal(id, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	tests := map[string]struct {
		sealer sealer
		id     uuid.UUID
		sealed []byte
	}{
		"another session":     {sealer: s, id: uuid.NewV7(), sealed: sealed},
		"another signing key": {sealer: sealer{secret: []byte(strings.Repeat("x", 32))}, id: id, sealed: sealed},
		"tampered":            {sealer: s, id: id, sealed: tampered},
		"truncated":           {sealer: s, id: id, sealed: sealed[:20]},
		"empty":               {sealer: s, id: id, sealed: nil},
	}
	for name, tt := range tests {
		if got, err := tt.sealer.open(tt.id, tt.sealed); err == nil {
			t.Errorf("%s: open = %x, want an error", name, got)
		}
	}
}
