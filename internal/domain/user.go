package domain

import (
	"errors"
	"fmt"
	"net/mail"
	"time"
	"unicode/utf8"
	"uuid"
)

// Account input limits.
const (
	// MaxEmailLength is the longest address SMTP can deliver to (RFC 5321 path limit minus the angle brackets).
	MaxEmailLength = 254
	// MinPasswordLength follows NIST SP 800-63B for passwords chosen by users.
	MinPasswordLength = 8
	// MaxPasswordBytes is where bcrypt stops reading its input; longer passwords would be silently truncated.
	MaxPasswordBytes = 72
)

// Role decides what a user may do.
type Role string

// Roles.
const (
	RoleCustomer Role = "customer"
	RoleAdmin    Role = "admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleCustomer, RoleAdmin:
		return true
	}
	return false
}

// User is a registered account.
type User struct {
	ID    uuid.UUID
	Email string
	// PasswordHash is the bcrypt hash of the password. No API response contains it.
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
}

// Principal is the authenticated caller of a request, as proven by an access token.
type Principal struct {
	UserID uuid.UUID
	Role   Role
}

// CheckEmail reports why email is not an acceptable account address, or nil if it is. It accepts a bare
// address such as "ann@example.com" only: no display name, no angle brackets, no surrounding space.
// Addresses are compared case-insensitively, but stored as given.
func CheckEmail(email string) error {
	if email == "" {
		return errors.New("is required")
	}
	if len(email) > MaxEmailLength {
		return fmt.Errorf("must be at most %d characters", MaxEmailLength)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Name != "" || addr.Address != email {
		return errors.New("must be a valid email address")
	}
	return nil
}

// CheckPassword reports why password is not acceptable for a new account, or nil if it is.
func CheckPassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("must be at least %d characters", MinPasswordLength)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("must be at most %d bytes", MaxPasswordBytes)
	}
	return nil
}
