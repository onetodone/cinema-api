package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"golang.org/x/crypto/bcrypt"

	"github.com/onetodone/cinema-api/internal/domain"
)

// memUsers is an in-memory UserRepository with the same case-insensitive email rule as the database.
type memUsers struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]domain.User
	lookup int   // GetUserByEmail calls
	err    error // returned by every call when set
}

func newMemUsers() *memUsers {
	return &memUsers{byID: map[uuid.UUID]domain.User{}}
}

func (m *memUsers) find(email string) (domain.User, bool) {
	for _, u := range m.byID {
		if strings.EqualFold(u.Email, email) {
			return u, true
		}
	}
	return domain.User{}, false
}

func (m *memUsers) CreateUser(_ context.Context, u domain.User) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.User{}, m.err
	}
	if _, taken := m.find(u.Email); taken {
		return domain.User{}, domain.Conflict(domain.CodeEmailTaken, "taken")
	}
	u.CreatedAt = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	m.byID[u.ID] = u
	return u, nil
}

func (m *memUsers) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookup++
	if m.err != nil {
		return domain.User{}, m.err
	}
	if u, ok := m.find(email); ok {
		return u, nil
	}
	return domain.User{}, domain.NotFound(domain.CodeUserNotFound, "user not found")
}

func (m *memUsers) GetUserByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.User{}, m.err
	}
	if u, ok := m.byID[id]; ok {
		return u, nil
	}
	return domain.User{}, domain.NotFound(domain.CodeUserNotFound, "user not found")
}

func (m *memUsers) UpsertAdmin(_ context.Context, u domain.User) (domain.User, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.User{}, false, m.err
	}
	if existing, ok := m.find(u.Email); ok {
		existing.PasswordHash, existing.Role = u.PasswordHash, domain.RoleAdmin
		m.byID[existing.ID] = existing
		return existing, false, nil
	}
	m.byID[u.ID] = u
	return u, true, nil
}

func newService(t *testing.T, users UserRepository) *Service {
	t.Helper()
	svc, err := New(users, bcrypt.MinCost)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// invalidFields returns the fields of a validation error, or fails the test.
func invalidFields(t *testing.T, err error) string {
	t.Helper()
	var ve *domain.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a validation error", err)
	}
	fields := make([]string, 0, len(ve.Fields))
	for _, f := range ve.Fields {
		fields = append(fields, f.Field)
	}
	return strings.Join(fields, ",")
}

func code(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestNewRejectsCostOutsideBcryptRange(t *testing.T) {
	t.Parallel()

	for _, cost := range []int{bcrypt.MinCost - 1, bcrypt.MaxCost + 1} {
		if _, err := New(newMemUsers(), cost); err == nil {
			t.Errorf("New with cost %d succeeded", cost)
		}
	}
}

func TestRegisterCreatesCustomerWithHashedPassword(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	svc := newService(t, users)

	u, err := svc.Register(t.Context(), "  Ann@Example.com ", "correct horse")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if u.Email != "Ann@Example.com" {
		t.Errorf("email = %q, want the trimmed address with its case kept", u.Email)
	}
	if u.Role != domain.RoleCustomer {
		t.Errorf("role = %q, want customer", u.Role)
	}
	if u.ID == (uuid.UUID{}) {
		t.Error("id is not set")
	}
	if u.PasswordHash == "correct horse" || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte("correct horse")) != nil {
		t.Errorf("password hash %q does not verify the password", u.PasswordHash)
	}
	if cost, _ := bcrypt.Cost([]byte(u.PasswordHash)); cost != bcrypt.MinCost {
		t.Errorf("hash cost = %d, want %d", cost, bcrypt.MinCost)
	}
}

func TestRegisterReportsEveryInvalidField(t *testing.T) {
	t.Parallel()

	svc := newService(t, newMemUsers())
	tests := []struct {
		email, password, fields string
	}{
		{email: "", password: "", fields: "email,password"},
		{email: "not-an-email", password: "long enough", fields: "email"},
		{email: "ann@example.com", password: "short", fields: "password"},
		{email: "ann@example.com", password: strings.Repeat("x", 73), fields: "password"},
	}
	for _, tt := range tests {
		_, err := svc.Register(t.Context(), tt.email, tt.password)
		if got := invalidFields(t, err); got != tt.fields {
			t.Errorf("Register(%q, %q): invalid fields %q, want %q", tt.email, tt.password, got, tt.fields)
		}
	}
}

func TestRegisterPassesDuplicateEmailThrough(t *testing.T) {
	t.Parallel()

	svc := newService(t, newMemUsers())
	if _, err := svc.Register(t.Context(), "ann@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Register(t.Context(), "ANN@example.com", "another password")
	if code(err) != domain.CodeEmailTaken {
		t.Errorf("error = %v, want EMAIL_TAKEN", err)
	}
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	svc := newService(t, users)
	registered, err := svc.Register(t.Context(), "ann@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}

	u, err := svc.Authenticate(t.Context(), " ANN@example.com ", "correct horse")
	if err != nil {
		t.Fatalf("Authenticate with the right password: %v", err)
	}
	if u.ID != registered.ID {
		t.Errorf("authenticated user %s, want %s", u.ID, registered.ID)
	}

	failures := map[string]struct{ email, password string }{
		"wrong password":  {email: "ann@example.com", password: "wrong horse"},
		"unknown account": {email: "bob@example.com", password: "correct horse"},
		// bcrypt ignores everything after 72 bytes, so this would match without the length check.
		"password beyond 72 bytes": {email: "ann@example.com", password: "correct horse" + strings.Repeat("!", 60)},
	}
	for name, f := range failures {
		_, err := svc.Authenticate(t.Context(), f.email, f.password)
		if !errors.Is(err, domain.ErrUnauthenticated) || code(err) != domain.CodeInvalidCredentials {
			t.Errorf("%s: error = %v, want INVALID_CREDENTIALS", name, err)
		}
	}
}

func TestAuthenticateRejectsTruncationAttack(t *testing.T) {
	t.Parallel()

	svc := newService(t, newMemUsers())
	pw72 := strings.Repeat("p", domain.MaxPasswordBytes)
	if _, err := svc.Register(t.Context(), "ann@example.com", pw72); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(t.Context(), "ann@example.com", pw72); err != nil {
		t.Fatalf("the exact 72-byte password must work: %v", err)
	}
	if _, err := svc.Authenticate(t.Context(), "ann@example.com", pw72+"anything"); code(err) != domain.CodeInvalidCredentials {
		t.Errorf("a longer password with the right prefix: error = %v, want INVALID_CREDENTIALS", err)
	}
}

func TestAuthenticateRequiresBothFields(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	svc := newService(t, users)
	_, err := svc.Authenticate(t.Context(), "  ", "")
	if got := invalidFields(t, err); got != "email,password" {
		t.Errorf("invalid fields = %q, want email,password", got)
	}
	if users.lookup != 0 {
		t.Error("the repository was queried for an empty address")
	}
}

func TestAuthenticateUnknownAccountStillComparesAHash(t *testing.T) {
	t.Parallel()

	// A login for an unknown account must cost one bcrypt comparison, like a login with a wrong password.
	// Without the dummy comparison it would return in microseconds and reveal which addresses exist.
	const cost = 8 // about 15 ms per comparison: measurable, still fast
	svc, err := New(newMemUsers(), cost)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := bcrypt.Cost(svc.dummyHash); got != cost {
		t.Fatalf("dummy hash cost = %d, want %d: it must cost as much as a real hash", got, cost)
	}

	// The minimum of several runs filters out scheduling noise from parallel tests.
	fastest := func(f func()) time.Duration {
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			f()
			best = min(best, time.Since(start))
		}
		return best
	}
	oneComparison := fastest(func() { _ = bcrypt.CompareHashAndPassword(svc.dummyHash, []byte("wrong horse")) })
	unknown := fastest(func() { _, _ = svc.Authenticate(t.Context(), "nobody@example.com", "wrong horse") })
	if unknown < oneComparison/4 {
		t.Errorf("a login for an unknown account took %s, one bcrypt comparison %s: the timing reveals which "+
			"accounts exist", unknown, oneComparison)
	}
}

func TestAuthenticatePropagatesRepositoryErrors(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	users.err = errors.New("connection refused")
	_, err := newService(t, users).Authenticate(t.Context(), "ann@example.com", "correct horse")
	if err == nil || errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("error = %v, want the repository failure, not a credentials error", err)
	}
}

func TestAuthenticateReportsCorruptHash(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	id := uuid.NewV7()
	users.byID[id] = domain.User{ID: id, Email: "ann@example.com", PasswordHash: "not-a-bcrypt-hash", Role: domain.RoleCustomer}

	_, err := newService(t, users).Authenticate(t.Context(), "ann@example.com", "correct horse")
	if err == nil || errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("error = %v, want an internal error for a corrupt hash", err)
	}
}

func TestUser(t *testing.T) {
	t.Parallel()

	svc := newService(t, newMemUsers())
	registered, err := svc.Register(t.Context(), "ann@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}

	u, err := svc.User(t.Context(), registered.ID)
	if err != nil || u.Email != "ann@example.com" {
		t.Errorf("User = %+v, %v", u, err)
	}

	_, err = svc.User(t.Context(), uuid.NewV7())
	if !errors.Is(err, domain.ErrUnauthenticated) || code(err) != domain.CodeInvalidToken {
		t.Errorf("unknown user: error = %v, want INVALID_TOKEN", err)
	}
}

func TestEnsureAdmin(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	svc := newService(t, users)

	admin, created, err := svc.EnsureAdmin(t.Context(), "admin@cinema.local", "first password")
	if err != nil || !created || admin.Role != domain.RoleAdmin {
		t.Fatalf("first EnsureAdmin = %+v, created %v, %v", admin, created, err)
	}

	again, created, err := svc.EnsureAdmin(t.Context(), "ADMIN@cinema.local", "second password")
	if err != nil || created || again.ID != admin.ID {
		t.Fatalf("second EnsureAdmin = %+v, created %v, %v; want the same account updated", again, created, err)
	}
	if _, err := svc.Authenticate(t.Context(), "admin@cinema.local", "second password"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
	if _, err := svc.Authenticate(t.Context(), "admin@cinema.local", "first password"); err == nil {
		t.Error("the old password still works")
	}

	_, _, err = svc.EnsureAdmin(t.Context(), "admin@cinema.local", "short")
	if got := invalidFields(t, err); got != "password" {
		t.Errorf("weak password: invalid fields %q, want password", got)
	}
}

func TestEnsureAdminPromotesARegisteredCustomerAndResetsThePassword(t *testing.T) {
	t.Parallel()

	users := newMemUsers()
	svc := newService(t, users)
	squatter, err := svc.Register(t.Context(), "admin@cinema.local", "squatter password")
	if err != nil {
		t.Fatal(err)
	}

	admin, created, err := svc.EnsureAdmin(t.Context(), "admin@cinema.local", "operator password")
	if err != nil || created || admin.ID != squatter.ID || admin.Role != domain.RoleAdmin {
		t.Fatalf("EnsureAdmin = %+v, created %v, %v", admin, created, err)
	}
	if _, err := svc.Authenticate(t.Context(), "admin@cinema.local", "squatter password"); err == nil {
		t.Error("whoever registered the admin address first can still log in as admin")
	}
}
