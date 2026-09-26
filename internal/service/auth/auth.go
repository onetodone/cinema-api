// Package auth implements accounts and access tokens: registration, password checks, and signed bearer tokens.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/domain"
)

// UserRepository is the account storage. It is implemented by repository/postgres.Users.
type UserRepository interface {
	CreateUser(ctx context.Context, u domain.User) (domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (domain.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	UpsertAdmin(ctx context.Context, u domain.User) (user domain.User, created bool, err error)
}

// errInvalidCredentials is the single answer to every failed login, whether the address is unknown or the
// password is wrong, so the response does not reveal which accounts exist.
var errInvalidCredentials = domain.Unauthenticated(domain.CodeInvalidCredentials, "the email or password is incorrect")

// Service registers accounts and checks passwords.
type Service struct {
	users UserRepository
	cost  int
	// dummyHash is checked when a login names an unknown address. A failed login then costs one bcrypt
	// comparison either way, so response times do not reveal which addresses are registered.
	dummyHash []byte
}

// New returns a Service that hashes passwords with the given bcrypt cost.
func New(users UserRepository, bcryptCost int) (*Service, error) {
	if bcryptCost < bcrypt.MinCost || bcryptCost > bcrypt.MaxCost {
		return nil, fmt.Errorf("bcrypt cost must be between %d and %d, got %d", bcrypt.MinCost, bcrypt.MaxCost, bcryptCost)
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte("timing equalizer for unknown accounts"), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash dummy password: %w", err)
	}
	return &Service{users: users, cost: bcryptCost, dummyHash: dummy}, nil
}

// Register creates a customer account. Surrounding space is trimmed from the address; the password is used as
// given. Invalid input fails with a *domain.ValidationError listing every bad field; an address that is already
// registered, in any letter case, fails with EMAIL_TAKEN.
func (s *Service) Register(ctx context.Context, email, password string) (domain.User, error) {
	u, err := s.newUser(email, password, domain.RoleCustomer)
	if err != nil {
		return domain.User{}, err
	}
	return s.users.CreateUser(ctx, u)
}

// EnsureAdmin creates the admin account, or promotes the account with this address to admin and resets its
// password. created reports whether a new account was made. The seed command uses it.
func (s *Service) EnsureAdmin(ctx context.Context, email, password string) (user domain.User, created bool, err error) {
	u, err := s.newUser(email, password, domain.RoleAdmin)
	if err != nil {
		return domain.User{}, false, err
	}
	return s.users.UpsertAdmin(ctx, u)
}

// newUser validates the input and hashes the password.
func (s *Service) newUser(email, password string, role domain.Role) (domain.User, error) {
	email = strings.TrimSpace(email)
	var v domain.Violations
	v.Check("email", domain.CheckEmail(email))
	v.Check("password", domain.CheckPassword(password))
	if err := v.Err(); err != nil {
		return domain.User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cost)
	if err != nil {
		return domain.User{}, fmt.Errorf("hash password: %w", err)
	}
	return domain.User{ID: uuid.NewV7(), Email: email, PasswordHash: string(hash), Role: role}, nil
}

// Authenticate returns the account with this address and password. Any mismatch fails with
// INVALID_CREDENTIALS, and takes about as long as a successful check.
func (s *Service) Authenticate(ctx context.Context, email, password string) (domain.User, error) {
	email = strings.TrimSpace(email)
	var v domain.Violations
	if email == "" {
		v.Add("email", "is required")
	}
	if password == "" {
		v.Add("password", "is required")
	}
	if err := v.Err(); err != nil {
		return domain.User{}, err
	}
	// bcrypt reads only the first 72 bytes. Registration rejects longer passwords, so a longer one cannot be
	// right; checking it would accept any suffix after a correct 72-byte prefix.
	if len(password) > domain.MaxPasswordBytes {
		return domain.User{}, errInvalidCredentials
	}

	u, err := s.users.GetUserByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		return domain.User{}, errInvalidCredentials
	}
	if err != nil {
		return domain.User{}, err
	}

	err = bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return domain.User{}, errInvalidCredentials
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("check password of user %s: %w", u.ID, err)
	}
	return u, nil
}

// User returns the account an access token was issued to. A token that outlived its account is rejected
// with INVALID_TOKEN.
func (s *Service) User(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := s.users.GetUserByID(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, domain.Unauthenticated(domain.CodeInvalidToken,
			"the account of this access token no longer exists")
	}
	return u, err
}
