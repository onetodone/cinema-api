package domain

import (
	"strings"
	"testing"
)

func TestRoleValid(t *testing.T) {
	t.Parallel()

	for _, r := range []Role{RoleCustomer, RoleAdmin} {
		if !r.Valid() {
			t.Errorf("%q should be valid", r)
		}
	}
	for _, r := range []Role{"", "root", "Admin"} {
		if r.Valid() {
			t.Errorf("%q should be invalid", r)
		}
	}
}

func TestCheckEmail(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{
		"ann@example.com",
		"Ann.Lee+cinema@Example.COM",
		"admin@cinema.local",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", MaxEmailLength-65),
	} {
		if err := CheckEmail(ok); err != nil {
			t.Errorf("CheckEmail(%q) = %v, want nil", ok, err)
		}
	}

	tests := map[string]string{
		"":                           "is required",
		"ann":                        "must be a valid email address",
		"ann@":                       "must be a valid email address",
		"@example.com":               "must be a valid email address",
		"Ann <ann@example.com>":      "must be a valid email address",
		"<ann@example.com>":          "must be a valid email address",
		" ann@example.com":           "must be a valid email address",
		"ann@example.com, bob@x.org": "must be a valid email address",
		strings.Repeat("a", 64) + "@" + strings.Repeat("b", MaxEmailLength-64): "must be at most 254 characters",
	}
	for email, want := range tests {
		err := CheckEmail(email)
		if err == nil || err.Error() != want {
			t.Errorf("CheckEmail(%q) = %v, want %q", email, err, want)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{
		"12345678",
		"пароль12", // 8 characters, 14 bytes
		strings.Repeat("x", MaxPasswordBytes),
		"  spaces count  ",
	} {
		if err := CheckPassword(ok); err != nil {
			t.Errorf("CheckPassword(%q) = %v, want nil", ok, err)
		}
	}

	tests := map[string]string{
		"":                                      "must be at least 8 characters",
		"1234567":                               "must be at least 8 characters",
		strings.Repeat("x", MaxPasswordBytes+1): "must be at most 72 bytes",
		strings.Repeat("я", MaxPasswordBytes/2+1): "must be at most 72 bytes", // 37 characters, 74 bytes
	}
	for pw, want := range tests {
		err := CheckPassword(pw)
		if err == nil || err.Error() != want {
			t.Errorf("CheckPassword(%q) = %v, want %q", pw, err, want)
		}
	}
}
