package domain

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"unicode/utf8"
	"uuid"
)

func TestRefreshTokenRoundTrip(t *testing.T) {
	t.Parallel()

	id := uuid.NewV7()
	a, b := NewRefreshToken(id), NewRefreshToken(id)
	if a.SessionID != id {
		t.Fatalf("session id = %s, want %s", a.SessionID, id)
	}
	if a.Secret == b.Secret {
		t.Fatal("two tokens got the same secret")
	}

	s := a.String()
	if want := 36 + 1 + 43; len(s) != want {
		t.Errorf("token %q has %d characters, want %d", s, len(s), want)
	}
	if !strings.HasPrefix(s, id.String()+".") {
		t.Errorf("token %q does not start with the session id", s)
	}
	parsed, ok := ParseRefreshToken(s)
	if !ok || parsed != a {
		t.Fatalf("ParseRefreshToken(%q) = %v, %t; want the token back", s, parsed, ok)
	}

	sum := sha256.Sum256(a.Secret[:])
	if !bytes.Equal(a.Hash(), sum[:]) {
		t.Error("Hash is not the SHA-256 of the secret")
	}
	if bytes.Equal(a.Hash(), b.Hash()) {
		t.Error("different secrets hash alike")
	}
}

func TestParseRefreshTokenRejectsOtherSpellings(t *testing.T) {
	t.Parallel()

	good := NewRefreshToken(uuid.MustParse("0199a1f0-7c1e-7d2a-9b3e-5f0c2d1e4a77")).String()
	id, secret, _ := strings.Cut(good, ".")
	// The last of 43 base64url characters carries 2 bits beyond the 32 bytes. A real secret leaves them 0; "B" (1)
	// sets one.
	lastWithSpareBits := secret[:42] + "B"

	for name, token := range map[string]string{
		"empty":                 "",
		"no dot":                id + secret,
		"only the id":           id,
		"empty secret":          id + ".",
		"uppercase id":          strings.ToUpper(id) + "." + secret,
		"id without hyphens":    strings.ReplaceAll(id, "-", "") + "." + secret,
		"id in braces":          "{" + id + "}." + secret,
		"urn id":                "urn:uuid:" + id + "." + secret,
		"not a uuid":            strings.Repeat("z", 36) + "." + secret,
		"short secret":          id + "." + secret[:42],
		"long secret":           id + "." + secret + "A",
		"padded secret":         id + "." + secret[:42] + "=",
		"standard base64":       id + "." + strings.Repeat("+", 43),
		"a second dot":          id + "." + secret[:21] + "." + secret[22:],
		"spare bits set":        id + "." + lastWithSpareBits,
		"surrounding space":     " " + good,
		"secret with a space":   id + "." + secret[:42] + " ",
		"secret with a newline": id + "." + secret[:42] + "\n",
	} {
		if _, ok := ParseRefreshToken(token); ok {
			t.Errorf("%s: ParseRefreshToken(%q) accepted it", name, token)
		}
	}
	if _, ok := ParseRefreshToken(good); !ok {
		t.Errorf("ParseRefreshToken(%q) rejected a token String wrote", good)
	}
}

func TestCleanUserAgent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ in, want string }{
		"plain":              {"Mozilla/5.0 (X11; Linux x86_64)", "Mozilla/5.0 (X11; Linux x86_64)"},
		"control characters": {"curl/8.5\x00\r\n\tx", "curl/8.5x"},
		"invalid utf-8":      {"agent\xff\xfe/1", "agent/1"},
		"surrounding space":  {"  agent  ", "agent"},
		"empty":              {"", ""},
	}
	for name, tt := range tests {
		if got := CleanUserAgent(tt.in); got != tt.want {
			t.Errorf("%s: CleanUserAgent(%q) = %q, want %q", name, tt.in, got, tt.want)
		}
	}

	long := strings.Repeat("a", MaxUserAgentBytes-1) + "é" // "é" is 2 bytes and would straddle the limit
	got := CleanUserAgent(long)
	if len(got) != MaxUserAgentBytes-1 || !utf8.ValidString(got) {
		t.Errorf("a long agent was cut to %d bytes (valid UTF-8: %t), want %d", len(got), utf8.ValidString(got), MaxUserAgentBytes-1)
	}
	if got := CleanUserAgent(strings.Repeat("b", 2*MaxUserAgentBytes)); len(got) != MaxUserAgentBytes {
		t.Errorf("a long ASCII agent was cut to %d bytes, want %d", len(got), MaxUserAgentBytes)
	}
}
