package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
)

// memSessions is an in-memory SessionRepository. Its clock stands in for the database clock.
type memSessions struct {
	mu    sync.Mutex
	clock *clock
	users *memUsers
	rows  map[uuid.UUID]*memSession

	err       error // returned by every call when set
	deleteErr error // returned by Delete when set
	// beforeRotate runs at the start of Rotate, as a request that rotates the session concurrently would.
	beforeRotate func()
	creates      []NewSession
}

type memSession struct {
	session                              domain.Session
	tokenHash, prevTokenHash, graceToken []byte
	rotatedAt                            time.Time
}

func newMemSessions(c *clock, users *memUsers) *memSessions {
	return &memSessions{clock: c, users: users, rows: map[uuid.UUID]*memSession{}}
}

// expiresAt mirrors LEAST(now() + idle, created_at + max age).
func expiresAt(now, created time.Time, idle, maxAge time.Duration) time.Time {
	if idleEnd := now.Add(idle); idleEnd.Before(created.Add(maxAge)) {
		return idleEnd
	}
	return created.Add(maxAge)
}

func (m *memSessions) Create(ctx context.Context, s NewSession) (domain.Session, []uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Session{}, nil, m.err
	}
	m.creates = append(m.creates, s)
	if _, err := m.users.GetUserByID(ctx, s.UserID); err != nil {
		return domain.Session{}, nil, err
	}

	now := m.clock.now
	created := domain.Session{
		ID: s.ID, UserID: s.UserID, UserAgent: s.Client.UserAgent, IP: s.Client.IP,
		CreatedAt: now, LastUsedAt: now, ExpiresAt: expiresAt(now, now, s.IdleTTL, s.MaxAge),
	}
	m.rows[s.ID] = &memSession{session: created, tokenHash: s.TokenHash}

	var own []*memSession
	for _, row := range m.rows {
		if row.session.UserID == s.UserID {
			own = append(own, row)
		}
	}
	// ORDER BY last_used_at DESC, id DESC OFFSET max
	slices.SortFunc(own, func(a, b *memSession) int {
		if c := b.session.LastUsedAt.Compare(a.session.LastUsedAt); c != 0 {
			return c
		}
		return bytes.Compare(b.session.ID[:], a.session.ID[:])
	})
	var evicted []uuid.UUID
	for _, row := range own[min(len(own), s.MaxPerUser):] {
		delete(m.rows, row.session.ID)
		evicted = append(evicted, row.session.ID)
	}
	return created, evicted, nil
}

func (m *memSessions) Get(ctx context.Context, id uuid.UUID, grace time.Duration) (StoredSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return StoredSession{}, m.err
	}
	row, ok := m.rows[id]
	if !ok {
		return StoredSession{}, domain.NotFound(domain.CodeSessionNotFound, "session not found")
	}
	u, err := m.users.GetUserByID(ctx, row.session.UserID)
	if err != nil {
		return StoredSession{}, err
	}
	now := m.clock.now
	return StoredSession{
		Session:       row.session,
		User:          u,
		TokenHash:     bytes.Clone(row.tokenHash),
		PrevTokenHash: bytes.Clone(row.prevTokenHash),
		GraceToken:    bytes.Clone(row.graceToken),
		Expired:       !row.session.ExpiresAt.After(now),
		InGrace:       !row.rotatedAt.IsZero() && row.rotatedAt.After(now.Add(-grace)),
	}, nil
}

func (m *memSessions) Rotate(_ context.Context, r Rotation) (domain.Session, bool, error) {
	if hook := m.beforeRotate; hook != nil {
		m.beforeRotate = nil
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return domain.Session{}, false, m.err
	}
	now := m.clock.now
	row, ok := m.rows[r.ID]
	if !ok || !bytes.Equal(row.tokenHash, r.Presented) || !row.session.ExpiresAt.After(now) {
		return domain.Session{}, false, nil
	}
	row.prevTokenHash, row.tokenHash, row.graceToken = row.tokenHash, r.TokenHash, r.GraceToken
	row.rotatedAt = now
	row.session.Generation++
	row.session.LastUsedAt = now
	row.session.UserAgent = r.Client.UserAgent
	if r.Client.IP.IsValid() {
		row.session.IP = r.Client.IP
	}
	row.session.ExpiresAt = expiresAt(now, row.session.CreatedAt, r.IdleTTL, r.MaxAge)
	return row.session, true, nil
}

func (m *memSessions) Delete(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.rows, id)
	return nil
}

func (m *memSessions) DeleteByToken(_ context.Context, id uuid.UUID, tokenHash []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	row, ok := m.rows[id]
	if !ok || !bytes.Equal(row.tokenHash, tokenHash) && !bytes.Equal(row.prevTokenHash, tokenHash) {
		return false, nil
	}
	delete(m.rows, id)
	return true, nil
}

func (m *memSessions) DeleteAllOfUser(_ context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var ended []uuid.UUID
	for id, row := range m.rows {
		if row.session.UserID == userID {
			delete(m.rows, id)
			ended = append(ended, id)
		}
	}
	return ended, nil
}

func (m *memSessions) row(id uuid.UUID) (memSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return memSession{}, false
	}
	return *row, true
}

func (m *memSessions) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

var testSessionConfig = SessionConfig{
	IdleTTL:    24 * time.Hour,
	MaxAge:     72 * time.Hour,
	Grace:      30 * time.Second,
	MaxPerUser: 3,
}

// sessionEnv is a Sessions over fakes, with one registered account.
type sessionEnv struct {
	clock  *clock
	users  *memUsers
	repo   *memSessions
	tokens *Tokens
	svc    *Sessions
	ann    domain.User
	logs   *bytes.Buffer
}

func newSessionEnv(t *testing.T, cfg SessionConfig) *sessionEnv {
	t.Helper()
	c := &clock{now: t0}
	users := newMemUsers()
	accounts := newService(t, users)
	ann, err := accounts.Register(t.Context(), "ann@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	repo := newMemSessions(c, users)
	tokens := newTokens(t, c)
	svc, err := NewSessions(accounts, repo, tokens, cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &sessionEnv{clock: c, users: users, repo: repo, tokens: tokens, svc: svc, ann: ann, logs: logs}
}

var annClient = Client{UserAgent: "Firefox/140", IP: netip.MustParseAddr("192.0.2.10")}

func (e *sessionEnv) login(t *testing.T) Grant {
	t.Helper()
	g, err := e.svc.Login(t.Context(), "ann@example.com", "correct horse", annClient)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return g
}

func (e *sessionEnv) refresh(t *testing.T, token string) (Grant, RefreshResult, error) {
	t.Helper()
	return e.svc.Refresh(t.Context(), token, annClient)
}

// mustRefresh refreshes and wants the given result.
func (e *sessionEnv) mustRefresh(t *testing.T, token string, want RefreshResult) Grant {
	t.Helper()
	g, result, err := e.refresh(t, token)
	if err != nil || result != want {
		t.Fatalf("refresh = %s, %v; want %s", result, err, want)
	}
	e.checkGrant(t, g)
	return g
}

// checkGrant checks that the access token of g verifies and names g's session and user.
func (e *sessionEnv) checkGrant(t *testing.T, g Grant) {
	t.Helper()
	p, err := e.tokens.Verify(g.Access.Token)
	if err != nil {
		t.Fatalf("the access token does not verify: %v", err)
	}
	if p.SessionID != g.Session.ID || p.SessionID != g.Refresh.SessionID || p.UserID != g.User.ID || p.Role != g.User.Role {
		t.Errorf("access token principal %+v, want session %s of user %s", p, g.Session.ID, g.User.ID)
	}
}

// wantRefused refreshes and wants REFRESH_INVALID with the given result.
func (e *sessionEnv) wantRefused(t *testing.T, token string, want RefreshResult) {
	t.Helper()
	g, result, err := e.refresh(t, token)
	if result != want || code(err) != domain.CodeRefreshInvalid || !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("refresh = %s, %v; want %s with REFRESH_INVALID", result, err, want)
	}
	if g.Access.Token != "" {
		t.Error("a refused refresh handed out an access token")
	}
}

func TestNewSessionsRejectsBadSettings(t *testing.T) {
	t.Parallel()

	accounts := newService(t, newMemUsers())
	tokens := newTokens(t, &clock{now: t0})
	for name, edit := range map[string]func(*SessionConfig){
		"zero idle ttl":        func(c *SessionConfig) { c.IdleTTL = 0 },
		"zero grace":           func(c *SessionConfig) { c.Grace = 0 },
		"max age below idle":   func(c *SessionConfig) { c.MaxAge = c.IdleTTL - time.Second },
		"no sessions per user": func(c *SessionConfig) { c.MaxPerUser = 0 },
	} {
		cfg := testSessionConfig
		edit(&cfg)
		if _, err := NewSessions(accounts, newMemSessions(&clock{}, newMemUsers()), tokens, cfg, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("%s: NewSessions succeeded", name)
		}
	}
}

func TestLoginStartsASession(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	g, err := e.svc.Login(t.Context(), " Ann@Example.com ", "correct horse",
		Client{UserAgent: "Firefox\x00/140\n", IP: netip.MustParseAddr("::ffff:192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	e.checkGrant(t, g)

	if g.User.ID != e.ann.ID || g.User.Email != "ann@example.com" {
		t.Errorf("grant user = %+v, want Ann", g.User)
	}
	if g.Access.ExpiresIn != time.Hour {
		t.Errorf("access token lifetime = %s, want the Tokens' 1h", g.Access.ExpiresIn)
	}
	if want := t0.Add(testSessionConfig.IdleTTL); !g.Session.ExpiresAt.Equal(want) {
		t.Errorf("session expires at %s, want %s", g.Session.ExpiresAt, want)
	}

	row, ok := e.repo.row(g.Session.ID)
	if !ok {
		t.Fatal("no session was stored")
	}
	if !bytes.Equal(row.tokenHash, g.Refresh.Hash()) {
		t.Error("the stored hash is not the hash of the refresh token's secret")
	}
	if bytes.Contains(row.tokenHash, g.Refresh.Secret[:]) {
		t.Error("the secret itself was stored")
	}
	if row.session.UserAgent != "Firefox/140" || row.session.IP != netip.MustParseAddr("192.0.2.10") {
		t.Errorf("stored client = %q %s, want the cleaned agent and the unmapped address", row.session.UserAgent, row.session.IP)
	}
	if c := e.repo.creates[0]; c.MaxPerUser != 3 || c.IdleTTL != testSessionConfig.IdleTTL || c.MaxAge != testSessionConfig.MaxAge {
		t.Errorf("Create got %+v, want the configured limits", c)
	}
}

func TestLoginFailures(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	for _, password := range []string{"wrong horse", strings.Repeat("x", 73)} {
		if _, err := e.svc.Login(t.Context(), "ann@example.com", password, annClient); code(err) != domain.CodeInvalidCredentials {
			t.Errorf("login with %q = %v, want INVALID_CREDENTIALS", password, err)
		}
	}
	if e.repo.count() != 0 {
		t.Error("a failed login started a session")
	}

	// The account is deleted between the password check and the session insert.
	e.repo.err = domain.NotFound(domain.CodeUserNotFound, "gone")
	if _, err := e.svc.Login(t.Context(), "ann@example.com", "correct horse", annClient); code(err) != domain.CodeInvalidCredentials {
		t.Errorf("login of a deleted account = %v, want INVALID_CREDENTIALS", err)
	}
	e.repo.err = errors.New("connection refused")
	if _, err := e.svc.Login(t.Context(), "ann@example.com", "correct horse", annClient); err == nil || code(err) != "" {
		t.Errorf("login with the database down = %v, want an internal error", err)
	}
}

func TestLoginEvictsTheLeastRecentlyUsedSessions(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig) // at most 3 sessions
	var grants []Grant
	for range 3 {
		grants = append(grants, e.login(t))
		e.clock.now = e.clock.now.Add(time.Minute)
	}
	// The oldest login refreshes, so the second one is now the least recently used.
	e.mustRefresh(t, grants[0].Refresh.String(), RefreshRotated)
	e.clock.now = e.clock.now.Add(time.Minute)

	fourth := e.login(t)
	if n := e.repo.count(); n != 3 {
		t.Fatalf("%d sessions after the fourth login, want 3", n)
	}
	for i, id := range []uuid.UUID{grants[0].Session.ID, grants[2].Session.ID, fourth.Session.ID} {
		if _, ok := e.repo.row(id); !ok {
			t.Errorf("session %d was evicted", i)
		}
	}
	if _, ok := e.repo.row(grants[1].Session.ID); ok {
		t.Error("the least recently used session was kept")
	}
	e.wantRefused(t, grants[1].Refresh.String(), RefreshInvalid)
	if !strings.Contains(e.logs.String(), "per-user limit") {
		t.Errorf("the eviction was not logged: %s", e.logs.String())
	}
}

// TestRefreshRotationTable walks one session through every row of the rotation table.
func TestRefreshRotationTable(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	login := e.login(t)
	t0Token := login.Refresh.String()

	// Current token, never rotated: rotate.
	e.clock.now = t0.Add(time.Hour)
	rotated := e.mustRefresh(t, t0Token, RefreshRotated)
	t1Token := rotated.Refresh.String()
	if t1Token == t0Token || rotated.Refresh.SessionID != login.Session.ID {
		t.Fatalf("rotation gave %q, want a new token of the same session", t1Token)
	}
	row, _ := e.repo.row(login.Session.ID)
	if row.session.Generation != 1 || !bytes.Equal(row.tokenHash, rotated.Refresh.Hash()) ||
		!bytes.Equal(row.prevTokenHash, login.Refresh.Hash()) {
		t.Fatalf("after the rotation the row is %+v", row)
	}
	if want := e.clock.now.Add(testSessionConfig.IdleTTL); !rotated.Session.ExpiresAt.Equal(want) {
		t.Errorf("the rotation moved the expiry to %s, want %s", rotated.Session.ExpiresAt, want)
	}

	// Current token within the grace window: the same token again, no second rotation.
	e.clock.now = e.clock.now.Add(10 * time.Second)
	if g := e.mustRefresh(t, t1Token, RefreshReissued); g.Refresh.String() != t1Token {
		t.Errorf("reissue gave %q, want the current token %q", g.Refresh.String(), t1Token)
	}

	// Previous token within the grace window: the current token.
	e.clock.now = e.clock.now.Add(19 * time.Second)
	if g := e.mustRefresh(t, t0Token, RefreshGrace); g.Refresh.String() != t1Token {
		t.Errorf("grace answer gave %q, want the current token %q", g.Refresh.String(), t1Token)
	}
	if row, _ := e.repo.row(login.Session.ID); row.session.Generation != 1 {
		t.Errorf("generation = %d after answers within the grace window, want still 1", row.session.Generation)
	}

	// Current token after the grace window: rotate again.
	e.clock.now = e.clock.now.Add(time.Minute)
	t2Token := e.mustRefresh(t, t1Token, RefreshRotated).Refresh.String()

	// A token two generations old: refused, but it revokes nothing.
	e.clock.now = e.clock.now.Add(time.Minute)
	e.wantRefused(t, t0Token, RefreshInvalid)
	if _, ok := e.repo.row(login.Session.ID); !ok {
		t.Fatal("an old token revoked the session")
	}

	// The previous token after the grace window: reuse detected, the session is revoked.
	e.wantRefused(t, t1Token, RefreshReuseDetected)
	if _, ok := e.repo.row(login.Session.ID); ok {
		t.Fatal("the session survived the reuse of a rotated token")
	}
	if !strings.Contains(e.logs.String(), "reuse detected") || !strings.Contains(e.logs.String(), login.Session.ID.String()) {
		t.Errorf("the reuse was not logged with the session: %s", e.logs.String())
	}
	// The current token is useless now too: whoever presented the reused one may hold it.
	e.wantRefused(t, t2Token, RefreshInvalid)
}

func TestRefreshOfAnExpiredSession(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	g := e.login(t)

	e.clock.now = g.Session.ExpiresAt.Add(-time.Second)
	g = e.mustRefresh(t, g.Refresh.String(), RefreshRotated)

	e.clock.now = g.Session.ExpiresAt // expires_at <= now()
	e.wantRefused(t, g.Refresh.String(), RefreshExpired)
	if e.repo.count() != 0 {
		t.Error("the expired session was not deleted")
	}

	// A failed delete does not change the answer.
	g = e.login(t)
	e.clock.now = g.Session.ExpiresAt.Add(time.Hour)
	e.repo.deleteErr = errors.New("connection reset")
	e.wantRefused(t, g.Refresh.String(), RefreshExpired)
}

func TestRefreshNeverOutlivesTheMaximumAge(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig) // idle 24h, at most 72h
	g := e.login(t)
	for day := 1; day <= 2; day++ {
		e.clock.now = t0.Add(time.Duration(day) * 23 * time.Hour)
		g = e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	}
	// Refreshed at 46h: the idle lifetime would end at 70h, inside the maximum age.
	if want := t0.Add(70 * time.Hour); !g.Session.ExpiresAt.Equal(want) {
		t.Errorf("expires at %s, want %s", g.Session.ExpiresAt, want)
	}

	e.clock.now = t0.Add(69 * time.Hour)
	g = e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	if want := t0.Add(72 * time.Hour); !g.Session.ExpiresAt.Equal(want) {
		t.Errorf("expires at %s, want the maximum age %s", g.Session.ExpiresAt, want)
	}
	e.clock.now = t0.Add(72 * time.Hour)
	e.wantRefused(t, g.Refresh.String(), RefreshExpired)
}

func TestRefreshThatLosesTheRaceGetsTheWinnersToken(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	login := e.login(t)
	e.clock.now = t0.Add(time.Hour)

	// Another tab rotates the session between this request's read and its compare-and-set.
	var winner Grant
	e.repo.beforeRotate = func() {
		winner = e.mustRefresh(t, login.Refresh.String(), RefreshRotated)
	}
	loser := e.mustRefresh(t, login.Refresh.String(), RefreshGrace)
	if loser.Refresh.String() != winner.Refresh.String() {
		t.Errorf("the losing refresh got %q, want the winner's token %q", loser.Refresh.String(), winner.Refresh.String())
	}
	if row, _ := e.repo.row(login.Session.ID); row.session.Generation != 1 {
		t.Errorf("generation = %d, want 1: the loser must not rotate again", row.session.Generation)
	}
}

func TestRefreshRefusesTokensItCannotMatch(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	g := e.login(t)

	e.wantRefused(t, "", RefreshInvalid)
	e.wantRefused(t, "not-a-token", RefreshInvalid)
	e.wantRefused(t, domain.NewRefreshToken(uuid.NewV7()).String(), RefreshInvalid) // unknown session
	e.wantRefused(t, domain.NewRefreshToken(g.Session.ID).String(), RefreshInvalid) // right session, wrong secret
	if e.repo.count() != 1 {
		t.Error("a token that matches nothing ended the session")
	}
}

func TestRefreshWithAGraceTokenThatCannotBeOpened(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	login := e.login(t)
	e.mustRefresh(t, login.Refresh.String(), RefreshRotated)

	// As if JWT_SECRET had changed since the rotation.
	e.repo.mu.Lock()
	e.repo.rows[login.Session.ID].graceToken[0] ^= 1
	e.repo.mu.Unlock()

	e.wantRefused(t, login.Refresh.String(), RefreshInvalid)
	if _, ok := e.repo.row(login.Session.ID); !ok {
		t.Error("the session was revoked, although its current token is fine")
	}
	if !strings.Contains(e.logs.String(), "level=ERROR") {
		t.Errorf("the broken grace token was not logged as an error: %s", e.logs.String())
	}
}

func TestRefreshDatabaseFailures(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	g := e.login(t)

	e.repo.err = errors.New("connection refused")
	if _, result, err := e.refresh(t, g.Refresh.String()); err == nil || code(err) != "" || result != "" {
		t.Errorf("refresh with the database down = %q, %v; want no result and an internal error", result, err)
	}
	e.repo.err = nil

	// Reuse is detected, but the session cannot be deleted: the request fails, so that a retry deletes it.
	next := e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	e.clock.now = e.clock.now.Add(time.Minute)
	e.repo.deleteErr = errors.New("connection reset")
	if _, result, err := e.refresh(t, g.Refresh.String()); err == nil || code(err) != "" || result != "" {
		t.Errorf("reuse with a failing delete = %q, %v; want an internal error", result, err)
	}
	e.repo.deleteErr = nil
	e.wantRefused(t, g.Refresh.String(), RefreshReuseDetected)
	e.wantRefused(t, next.Refresh.String(), RefreshInvalid)
}

func TestLogout(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	ctx := t.Context()

	// With the current token.
	g := e.login(t)
	if err := e.svc.Logout(ctx, g.Refresh.String()); err != nil {
		t.Fatal(err)
	}
	if e.repo.count() != 0 {
		t.Fatal("logout with the current token kept the session")
	}
	if err := e.svc.Logout(ctx, g.Refresh.String()); err != nil {
		t.Errorf("a second logout = %v, want no error", err)
	}

	// With the previous token, whose owner rotated it a moment ago in another tab.
	g = e.login(t)
	e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	if err := e.svc.Logout(ctx, g.Refresh.String()); err != nil || e.repo.count() != 0 {
		t.Fatalf("logout with the previous token = %v, %d sessions left", err, e.repo.count())
	}

	// With a token two generations old, or garbage: nothing happens.
	g = e.login(t)
	next := e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	e.clock.now = e.clock.now.Add(time.Minute)
	e.mustRefresh(t, next.Refresh.String(), RefreshRotated)
	for _, token := range []string{g.Refresh.String(), "garbage", ""} {
		if err := e.svc.Logout(ctx, token); err != nil || e.repo.count() != 1 {
			t.Errorf("logout with %q = %v, %d sessions left; want nothing ended", token, err, e.repo.count())
		}
	}

	e.repo.err = errors.New("connection refused")
	if err := e.svc.Logout(ctx, g.Refresh.String()); err == nil {
		t.Error("logout with the database down reported success")
	}
}

func TestLogoutAll(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	a, b := e.login(t), e.login(t)
	bob, err := e.svc.accounts.Register(t.Context(), "bob@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := e.svc.Login(t.Context(), "bob@example.com", "correct horse", annClient)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.svc.LogoutAll(t.Context(), e.ann.ID); err != nil {
		t.Fatal(err)
	}
	for _, g := range []Grant{a, b} {
		e.wantRefused(t, g.Refresh.String(), RefreshInvalid)
	}
	if _, ok := e.repo.row(bobs.Session.ID); !ok || bobs.User.ID != bob.ID {
		t.Error("logging Ann out everywhere ended Bob's session")
	}

	e.repo.err = errors.New("connection refused")
	if err := e.svc.LogoutAll(t.Context(), e.ann.ID); err == nil {
		t.Error("logout-all with the database down reported success")
	}
}
