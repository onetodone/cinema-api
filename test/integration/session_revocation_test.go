//go:build integration

package integration

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/handler"
)

// loginAs logs an account in from a client with the given User-Agent and returns its new session.
func (c apiClient) loginAs(email, userAgent string) session {
	c.t.Helper()
	return sessionOf(c.t, c.doWith(http.MethodPost, "/v1/auth/login", "",
		map[string]string{"email": email, "password": "correct horse"}, http.Header{"User-Agent": {userAgent}}))
}

// sessionIDs returns the ids of the listed sessions, in order, and the listed session that is marked current.
func sessionIDs(t *testing.T, r apiResponse) (ids []string, current string) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("session list = %d %v", r.status, r.body)
	}
	items, _ := r.body["items"].([]any)
	for _, it := range items {
		item, _ := it.(map[string]any)
		id, _ := item["id"].(string)
		ids = append(ids, id)
		if item["current"] == true {
			current = id
		}
	}
	return ids, current
}

// wantRevoked checks that r refused an access token because its session has ended.
func wantRevoked(t *testing.T, r apiResponse) {
	t.Helper()
	if r.status != http.StatusUnauthorized || r.code() != domain.CodeInvalidToken ||
		!strings.Contains(r.header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("request with the access token of an ended session = %d %v, want 401 INVALID_TOKEN", r.status, r.body)
	}
}

// revokedKey is the key of a session on the revocation list.
func revokedKey(env *redisEnv, id uuid.UUID) string {
	return env.prefix + ":revoked-sid:" + id.String()
}

func revocationsCounted(env *redisEnv, reason string) float64 {
	return testCount(env.metrics.SessionRevocations.WithLabelValues(reason))
}

// TestSessionManagementAPI: a user lists their sessions and ends one of them on one replica; the session's access
// token is refused on the other replica at once, and its refresh token too.
func TestSessionManagementAPI(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	env := newRedisEnv(t)
	replicas := []apiClient{newAPIServerWith(t, f, apiOptions{redis: env}), newAPIServerWith(t, f, apiOptions{redis: env})}
	api := replicas[0]
	api.signUp("ann@example.com")
	api.signUp("bob@example.com")
	exec(t, f.pool, `DELETE FROM sessions`) // the sessions of signUp

	laptop := api.loginAs("ann@example.com", "Laptop")
	phone := api.loginAs("ann@example.com", "Phone")
	tablet := api.loginAs("ann@example.com", "Tablet")
	old := api.loginAs("ann@example.com", "Old browser")
	bobs := api.loginAs("bob@example.com", "Bob's laptop")
	exec(t, f.pool, `UPDATE sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, old.id())
	// The laptop refreshes last, so it is the most recently used session.
	laptop = sessionOf(t, api.doWith(http.MethodPost, "/v1/auth/refresh", "", map[string]any{}, http.Header{
		"Cookie": {handler.RefreshCookieName + "=" + laptop.refresh}, "User-Agent": {"Laptop"},
	}))

	// The list: live sessions only, most recently used first, the asking one marked current.
	list := replicas[1].do(http.MethodGet, "/v1/auth/sessions", laptop.access, nil)
	ids, current := sessionIDs(t, list)
	if want := []string{laptop.id().String(), tablet.id().String(), phone.id().String()}; !slices.Equal(ids, want) {
		t.Errorf("sessions = %v, want %v (laptop, tablet, phone; not the expired one, not Bob's)", ids, want)
	}
	if current != laptop.id().String() {
		t.Errorf("current = %s, want the laptop's session %s", current, laptop.id())
	}
	first, _ := list.body["items"].([]any)[0].(map[string]any)
	if first["user_agent"] != "Laptop" || first["ip"] != "127.0.0.1" {
		t.Errorf("first session = %v, want the laptop's agent and address", first)
	}
	for _, field := range []string{"created_at", "last_used_at", "expires_at"} {
		if _, err := time.Parse(time.RFC3339, first[field].(string)); err != nil {
			t.Errorf("%s = %v: %v", field, first[field], err)
		}
	}
	if _, current := sessionIDs(t, api.do(http.MethodGet, "/v1/auth/sessions", phone.access, nil)); current != phone.id().String() {
		t.Errorf("the phone's list marks %s current, want the phone's session", current)
	}

	// Sessions that are not the caller's: all the same answer, and nothing ends.
	for name, path := range map[string]string{"Bob's": bobs.id().String(), "unknown": uuid.NewV7().String()} {
		r := api.do(http.MethodDelete, "/v1/auth/sessions/"+path, laptop.access, nil)
		if r.status != http.StatusNotFound || r.code() != domain.CodeSessionNotFound {
			t.Errorf("deleting an %s session = %d %v, want 404 SESSION_NOT_FOUND", name, r.status, r.body)
		}
	}
	if r := api.do(http.MethodDelete, "/v1/auth/sessions/not-a-uuid", laptop.access, nil); r.status != http.StatusBadRequest {
		t.Errorf("deleting a malformed id = %d %v, want 400", r.status, r.body)
	}
	if r := api.do(http.MethodGet, "/v1/me", bobs.access, nil); r.status != http.StatusOK {
		t.Errorf("Bob's session after Ann's attempts = %d", r.status)
	}

	// The laptop ends the phone's session on one replica; the phone is refused on the other one at once.
	r := replicas[0].do(http.MethodDelete, "/v1/auth/sessions/"+phone.id().String(), laptop.access, nil)
	if r.status != http.StatusNoContent || refreshCookieIn(r) != nil {
		t.Fatalf("delete another session = %d %v, Set-Cookie %v; want 204 without touching the cookie",
			r.status, r.body, r.header.Values("Set-Cookie"))
	}
	wantRevoked(t, replicas[1].do(http.MethodGet, "/v1/me", phone.access, nil))
	wantCleared(t, replicas[1].withCookie("/v1/auth/refresh", phone.refresh), http.StatusUnauthorized)
	if r := replicas[1].do(http.MethodGet, "/v1/me", laptop.access, nil); r.status != http.StatusOK {
		t.Errorf("the laptop after ending the phone = %d, want 200", r.status)
	}
	// The entry lasts as long as the phone's last access token can: 1h, the 30 s leeway, and 30 s more.
	if ttl := pttl(t, env, revokedKey(env, phone.id())); ttl <= time.Hour+50*time.Second || ttl > time.Hour+time.Minute {
		t.Errorf("revocation TTL = %s, want just under 1h1m", ttl)
	}
	if r := api.do(http.MethodDelete, "/v1/auth/sessions/"+phone.id().String(), laptop.access, nil); r.status != http.StatusNotFound {
		t.Errorf("deleting the phone's session again = %d, want 404", r.status)
	}

	// Ending one's own session is a logout: the cookie goes too.
	wantCleared(t, api.do(http.MethodDelete, "/v1/auth/sessions/"+tablet.id().String(), tablet.access, nil), http.StatusNoContent)
	wantRevoked(t, api.do(http.MethodGet, "/v1/me", tablet.access, nil))

	if ids, _ := sessionIDs(t, api.do(http.MethodGet, "/v1/auth/sessions", laptop.access, nil)); !slices.Equal(ids, []string{laptop.id().String()}) {
		t.Errorf("sessions left = %v, want only the laptop's", ids)
	}
	if n := revocationsCounted(env, metrics.RevocationDeleted); n != 2 {
		t.Errorf("deleted sessions counted = %v, want 2", n)
	}
}

// TestEveryRevocationStopsAccessTokens: logout, reuse detection, eviction over the cap, and logout-all put the
// sessions they end on the revocation list, and only those.
func TestEveryRevocationStopsAccessTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	cfg := testSessionConfig
	cfg.MaxPerUser = 2
	api := newAPIServerWith(t, f, apiOptions{sessions: cfg})
	env := api.redis
	api.signUp("ann@example.com")
	api.signUp("bob@example.com")
	exec(t, f.pool, `DELETE FROM sessions`)
	me := func(s session) apiResponse { return api.do(http.MethodGet, "/v1/me", s.access, nil) }
	var ended []uuid.UUID

	// Logout.
	s := api.login("ann@example.com")
	wantCleared(t, api.withCookie("/v1/auth/logout", s.refresh), http.StatusNoContent)
	wantRevoked(t, me(s))
	ended = append(ended, s.id())

	// Reuse detection: whoever holds the session's tokens, owner or thief, is out.
	s = api.login("ann@example.com")
	next := sessionOf(t, api.withCookie("/v1/auth/refresh", s.refresh))
	exec(t, f.pool, `UPDATE sessions SET rotated_at = rotated_at - interval '31 seconds' WHERE id = $1`, s.id())
	wantCleared(t, api.withCookie("/v1/auth/refresh", s.refresh), http.StatusUnauthorized)
	wantRevoked(t, me(next))
	wantRevoked(t, me(s))
	ended = append(ended, s.id())

	// A login over the cap of 2 ends the least recently used session.
	e1, e2, e3 := api.login("ann@example.com"), api.login("ann@example.com"), api.login("ann@example.com")
	wantRevoked(t, me(e1))
	for _, s := range []session{e2, e3} {
		if r := me(s); r.status != http.StatusOK {
			t.Errorf("a session within the cap = %d, want 200", r.status)
		}
	}
	ended = append(ended, e1.id())

	// Logout from every session: Ann's, not Bob's.
	bob := api.login("bob@example.com")
	wantCleared(t, api.do(http.MethodPost, "/v1/auth/logout-all", e2.access, nil), http.StatusNoContent)
	wantRevoked(t, me(e2))
	wantRevoked(t, me(e3))
	ended = append(ended, e2.id(), e3.id())
	if r := me(bob); r.status != http.StatusOK {
		t.Errorf("Bob after Ann's logout-all = %d, want 200", r.status)
	}

	for _, id := range ended {
		if n := env.client.Exists(t.Context(), revokedKey(env, id)).Val(); n != 1 {
			t.Errorf("session %s is not on the revocation list", id)
		}
	}
	if n := env.client.Exists(t.Context(), revokedKey(env, bob.id())).Val(); n != 0 {
		t.Error("Bob's session is on the revocation list")
	}
	for reason, want := range map[string]float64{
		metrics.RevocationLogout: 1, metrics.RevocationReuse: 1, metrics.RevocationEvicted: 1, metrics.RevocationLogoutAll: 2,
		metrics.RevocationDeleted: 0,
	} {
		if n := revocationsCounted(env, reason); n != want {
			t.Errorf("revocations for %s = %v, want %v", reason, n, want)
		}
	}
}

// TestSessionRevocationWithRedisDown: without the revocation list, an ended session still cannot refresh, because
// PostgreSQL decides that, but its access token works until it expires.
func TestSessionRevocationWithRedisDown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	env := newDeadRedisEnv(t)
	api := newAPIServerWith(t, f, apiOptions{redis: env})
	api.signUp("ann@example.com")
	exec(t, f.pool, `DELETE FROM sessions`)
	a, b := api.login("ann@example.com"), api.login("ann@example.com")

	if r := api.do(http.MethodDelete, "/v1/auth/sessions/"+b.id().String(), a.access, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete with Redis down = %d %v, want 204", r.status, r.body)
	}
	wantCleared(t, api.withCookie("/v1/auth/refresh", b.refresh), http.StatusUnauthorized)
	if r := api.do(http.MethodGet, "/v1/me", b.access, nil); r.status != http.StatusOK {
		t.Errorf("the ended session's access token with Redis down = %d, want 200 until it expires", r.status)
	}
	if ids, _ := sessionIDs(t, api.do(http.MethodGet, "/v1/auth/sessions", a.access, nil)); !slices.Equal(ids, []string{a.id().String()}) {
		t.Errorf("sessions = %v, want only a", ids)
	}
	if env.failedOpen(metrics.OpRevocationWrite) != 1 || env.failedOpen(metrics.OpRevocationCheck) < 2 {
		t.Errorf("fail-open writes %v, checks %v; want 1 and at least 2",
			env.failedOpen(metrics.OpRevocationWrite), env.failedOpen(metrics.OpRevocationCheck))
	}
}

func TestRevocationList(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	env := newRedisEnv(t)
	list := env.store.Revocations()
	a, b, c := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()

	if err := list.Revoke(ctx, time.Minute, a, b); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID]bool{a: true, b: true, c: false} {
		if got, err := list.Revoked(ctx, id); err != nil || got != want {
			t.Errorf("Revoked(%s) = %t, %v; want %t", id, got, err, want)
		}
	}
	if ttl := pttl(t, env, revokedKey(env, b)); ttl <= 59*time.Second || ttl > time.Minute {
		t.Errorf("TTL = %s, want about 1m", ttl)
	}
	if err := list.Revoke(ctx, time.Minute); err != nil {
		t.Errorf("revoking no session = %v", err)
	}
	if err := list.Revoke(ctx, 0, c); err == nil {
		t.Error("a revocation without a lifetime was accepted")
	}

	dead := newDeadRedisEnv(t)
	deadList := dead.store.Revocations()
	if err := deadList.Revoke(ctx, time.Minute, a); err == nil {
		t.Error("revoking with Redis down reported success")
	}
	if revoked, err := deadList.Revoked(ctx, a); err == nil || revoked {
		t.Errorf("lookup with Redis down = %t, %v; want an error", revoked, err)
	}
	if dead.failedOpen(metrics.OpRevocationWrite) != 1 || dead.failedOpen(metrics.OpRevocationCheck) != 1 {
		t.Error("the failures were not counted")
	}
}
