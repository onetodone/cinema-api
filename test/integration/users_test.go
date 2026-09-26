//go:build integration

package integration

import (
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/repository/postgres"
)

func newUser(email string, role domain.Role) domain.User {
	return domain.User{ID: uuid.NewV7(), Email: email, PasswordHash: "hash-of-" + email, Role: role}
}

func TestUsersCreateAndGet(t *testing.T) {
	t.Parallel()
	users := postgres.NewUsers(newDB(t))
	ctx := t.Context()

	in := newUser("Ann@Example.com", domain.RoleCustomer)
	created := must(users.CreateUser(ctx, in))(t)
	if created.ID != in.ID || created.Email != "Ann@Example.com" || created.Role != domain.RoleCustomer ||
		created.PasswordHash != in.PasswordHash || created.CreatedAt.IsZero() {
		t.Errorf("created = %+v", created)
	}

	byEmail := must(users.GetUserByEmail(ctx, "ann@EXAMPLE.com"))(t)
	if byEmail.ID != in.ID {
		t.Errorf("lookup by email in another case found %s, want %s", byEmail.ID, in.ID)
	}
	byID := must(users.GetUserByID(ctx, in.ID))(t)
	if byID.Email != in.Email || !byID.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("lookup by id = %+v", byID)
	}

	_, err := users.GetUserByEmail(ctx, "nobody@example.com")
	if !errors.Is(err, domain.ErrNotFound) || domainCode(err) != domain.CodeUserNotFound {
		t.Errorf("unknown email: %v", err)
	}
	_, err = users.GetUserByID(ctx, uuid.NewV7())
	if !errors.Is(err, domain.ErrNotFound) || domainCode(err) != domain.CodeUserNotFound {
		t.Errorf("unknown id: %v", err)
	}
}

func TestUsersEmailIsUniqueIgnoringCase(t *testing.T) {
	t.Parallel()
	users := postgres.NewUsers(newDB(t))
	ctx := t.Context()

	must(users.CreateUser(ctx, newUser("ann@example.com", domain.RoleCustomer)))(t)
	_, err := users.CreateUser(ctx, newUser("ANN@example.COM", domain.RoleCustomer))
	if !errors.Is(err, domain.ErrConflict) || domainCode(err) != domain.CodeEmailTaken {
		t.Errorf("duplicate in another case: %v, want EMAIL_TAKEN", err)
	}
}

func TestUsersConcurrentRegistrationOfOneEmail(t *testing.T) {
	t.Parallel()
	users := postgres.NewUsers(newDB(t))

	// The unique index, not a check-then-insert in Go, decides the race: exactly one insert wins.
	const n = 20
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		created, taken  int
		unexpectedError error
	)
	for i := range n {
		wg.Go(func() {
			email := "race@example.com"
			if i%2 == 1 {
				email = "RACE@example.com"
			}
			_, err := users.CreateUser(t.Context(), newUser(email, domain.RoleCustomer))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case domainCode(err) == domain.CodeEmailTaken:
				taken++
			default:
				unexpectedError = err
			}
		})
	}
	wg.Wait()

	if unexpectedError != nil {
		t.Fatalf("unexpected error: %v", unexpectedError)
	}
	if created != 1 || taken != n-1 {
		t.Errorf("created %d, taken %d; want 1 and %d", created, taken, n-1)
	}
}

func TestUsersUpsertAdmin(t *testing.T) {
	t.Parallel()
	users := postgres.NewUsers(newDB(t))
	ctx := t.Context()

	first := newUser("admin@cinema.local", domain.RoleAdmin)
	admin, created, err := users.UpsertAdmin(ctx, first)
	if err != nil || !created || admin.ID != first.ID || admin.Role != domain.RoleAdmin {
		t.Fatalf("insert: %+v created=%v err=%v", admin, created, err)
	}

	second := newUser("ADMIN@cinema.local", domain.RoleAdmin)
	second.PasswordHash = "new-hash"
	admin, created, err = users.UpsertAdmin(ctx, second)
	if err != nil || created {
		t.Fatalf("update: created=%v err=%v", created, err)
	}
	if admin.ID != first.ID || admin.Email != "admin@cinema.local" || admin.PasswordHash != "new-hash" {
		t.Errorf("update = %+v; want the first row with the new hash and its original address", admin)
	}
}

func TestUsersUpsertAdminPromotesACustomer(t *testing.T) {
	t.Parallel()
	users := postgres.NewUsers(newDB(t))
	ctx := t.Context()

	customer := must(users.CreateUser(ctx, newUser("boss@example.com", domain.RoleCustomer)))(t)
	operator := newUser("boss@example.com", domain.RoleAdmin)
	operator.PasswordHash = "operator-hash"
	admin, created, err := users.UpsertAdmin(ctx, operator)
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if admin.ID != customer.ID || admin.Role != domain.RoleAdmin || admin.PasswordHash != "operator-hash" {
		t.Errorf("promoted = %+v; want the same id, role admin, and a replaced hash", admin)
	}
}
