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

func (m *memSessions) ListOfUser(_ context.Context, userID uuid.UUID) ([]domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	var own []domain.Session
	for _, row := range m.rows {
		if row.session.UserID == userID && row.session.ExpiresAt.After(m.clock.now) {
			own = append(own, row.session)
		}
	}
	// ORDER BY last_used_at DESC, id DESC
	slices.SortFunc(own, func(a, b domain.Session) int {
		if c := b.LastUsedAt.Compare(a.LastUsedAt); c != 0 {
			return c
		}
		return bytes.Compare(b.ID[:], a.ID[:])
	})
	return own, nil
}

func (m *memSessions) DeleteOfUser(_ context.Context, userID, id uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	row, ok := m.rows[id]
	if !ok || row.session.UserID != userID {
		return false, nil
	}
	delete(m.rows, id)
	return true, nil
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

// memRevocations records what the session use cases put on the revocation list.
type memRevocations struct {
	mu      sync.Mutex
	revoked map[uuid.UUID]time.Duration // session id → ttl
	err     error                       // returned by Revoke when set, after recording nothing
	// ctxErrs records whether the context of each call was already done.
	ctxErrs []error
}

func (m *memRevocations) Revoke(ctx context.Context, ttl time.Duration, sessionIDs ...uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctxErrs = append(m.ctxErrs, ctx.Err())
	if m.err != nil {
		return m.err
	}
	for _, id := range sessionIDs {
		m.revoked[id] = ttl
	}
	return nil
}

// has reports whether every one of ids is on the list.
func (m *memRevocations) has(ids ...uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if _, ok := m.revoked[id]; !ok {
			return false
		}
	}
	return true
}

func (m *memRevocations) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.revoked)
}

var testSessionConfig = SessionConfig{
	IdleTTL:    24 * time.Hour,
	MaxAge:     72 * time.Hour,
	Grace:      30 * time.Second,
	MaxPerUser: 3,
}

// sessionEnv is a Sessions over fakes, with one registered account.
type sessionEnv struct {
	clock   *clock
	users   *memUsers
	repo    *memSessions
	revoked *memRevocations
	tokens  *Tokens
	svc     *Sessions
	ann     domain.User
	logs    *bytes.Buffer
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
	revoked := &memRevocations{revoked: map[uuid.UUID]time.Duration{}}
	tokens := newTokens(t, c)
	svc, err := NewSessions(accounts, repo, tokens, cfg, slog.New(slog.NewTextHandler(logs, nil)), WithRevocations(revoked))
	if err != nil {
		t.Fatal(err)
	}
	return &sessionEnv{clock: c, users: users, repo: repo, revoked: revoked, tokens: tokens, svc: svc, ann: ann, logs: logs}
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
	if fourth.Evicted != 1 || !e.revoked.has(grants[1].Session.ID) || e.revoked.count() != 1 {
		t.Errorf("evicted %d, revoked %v; want the evicted session revoked, and only it", fourth.Evicted, e.revoked.revoked)
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
	if e.revoked.count() != 0 {
		t.Fatalf("refreshes revoked sessions: %v", e.revoked.revoked)
	}
	e.wantRefused(t, t1Token, RefreshReuseDetected)
	if _, ok := e.repo.row(login.Session.ID); ok {
		t.Fatal("the session survived the reuse of a rotated token")
	}
	if !e.revoked.has(login.Session.ID) {
		t.Error("the access tokens of the session were not revoked")
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
	if e.revoked.count() != 0 {
		t.Error("an expired session was revoked: its access tokens expired long before it")
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
	if e.revoked.count() != 0 {
		t.Error("a session that could not be deleted was revoked: every token it refreshes would be refused")
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
	if ended, err := e.svc.Logout(ctx, g.Refresh.String()); err != nil || !ended {
		t.Fatalf("logout = %t, %v; want the session ended", ended, err)
	}
	if e.repo.count() != 0 {
		t.Fatal("logout with the current token kept the session")
	}
	if !e.revoked.has(g.Session.ID) {
		t.Error("logout left the session's access tokens valid")
	}
	if ended, err := e.svc.Logout(ctx, g.Refresh.String()); err != nil || ended {
		t.Errorf("a second logout = %t, %v; want nothing ended and no error", ended, err)
	}

	// With the previous token, whose owner rotated it a moment ago in another tab.
	g = e.login(t)
	e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	if ended, err := e.svc.Logout(ctx, g.Refresh.String()); err != nil || !ended || e.repo.count() != 0 {
		t.Fatalf("logout with the previous token = %t, %v, %d sessions left", ended, err, e.repo.count())
	}

	// With a token two generations old, or garbage: nothing happens.
	g = e.login(t)
	next := e.mustRefresh(t, g.Refresh.String(), RefreshRotated)
	e.clock.now = e.clock.now.Add(time.Minute)
	e.mustRefresh(t, next.Refresh.String(), RefreshRotated)
	revoked := e.revoked.count()
	for _, token := range []string{g.Refresh.String(), "garbage", ""} {
		if ended, err := e.svc.Logout(ctx, token); err != nil || ended || e.repo.count() != 1 {
			t.Errorf("logout with %q = %t, %v, %d sessions left; want nothing ended", token, ended, err, e.repo.count())
		}
	}
	if e.revoked.count() != revoked {
		t.Error("a logout that ended nothing revoked a session")
	}

	e.repo.err = errors.New("connection refused")
	if _, err := e.svc.Logout(ctx, g.Refresh.String()); err == nil {
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

	if ended, err := e.svc.LogoutAll(t.Context(), e.ann.ID); err != nil || ended != 2 {
		t.Fatalf("logout-all = %d, %v; want Ann's 2 sessions ended", ended, err)
	}
	for _, g := range []Grant{a, b} {
		e.wantRefused(t, g.Refresh.String(), RefreshInvalid)
	}
	if _, ok := e.repo.row(bobs.Session.ID); !ok || bobs.User.ID != bob.ID {
		t.Error("logging Ann out everywhere ended Bob's session")
	}
	if !e.revoked.has(a.Session.ID, b.Session.ID) || e.revoked.has(bobs.Session.ID) {
		t.Errorf("revoked %v, want exactly Ann's sessions", e.revoked.revoked)
	}
	if ended, err := e.svc.LogoutAll(t.Context(), e.ann.ID); err != nil || ended != 0 {
		t.Errorf("a second logout-all = %d, %v; want nothing ended", ended, err)
	}

	e.repo.err = errors.New("connection refused")
	if _, err := e.svc.LogoutAll(t.Context(), e.ann.ID); err == nil {
		t.Error("logout-all with the database down reported success")
	}
}

func TestListSessions(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	var grants []Grant
	for range 3 {
		grants = append(grants, e.login(t))
		e.clock.now = e.clock.now.Add(time.Minute)
	}
	bob, err := e.svc.accounts.Register(t.Context(), "bob@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := e.svc.Login(t.Context(), "bob@example.com", "correct horse", annClient)
	if err != nil {
		t.Fatal(err)
	}
	// The first session refreshes, so it is the most recently used one.
	e.mustRefresh(t, grants[0].Refresh.String(), RefreshRotated)

	sessions, err := e.svc.List(t.Context(), e.ann.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	for _, s := range sessions {
		got = append(got, s.ID)
		if s.UserID != e.ann.ID {
			t.Errorf("the list holds session %s of user %s", s.ID, s.UserID)
		}
	}
	want := []uuid.UUID{grants[0].Session.ID, grants[2].Session.ID, grants[1].Session.ID}
	if !slices.Equal(got, want) {
		t.Errorf("sessions = %v, want %v (most recently used first)", got, want)
	}
	if sessions[0].UserAgent != annClient.UserAgent || sessions[0].IP != annClient.IP || sessions[0].Generation != 1 {
		t.Errorf("first session = %+v, want the client and the rotation", sessions[0])
	}

	if sessions, err := e.svc.List(t.Context(), bob.ID); err != nil || len(sessions) != 1 || sessions[0].ID != bobs.Session.ID {
		t.Errorf("Bob's sessions = %v, %v; want his one", sessions, err)
	}

	// Sessions that have expired are not listed, although the sweeper has not deleted them yet.
	e.clock.now = grants[2].Session.ExpiresAt
	if sessions, err := e.svc.List(t.Context(), e.ann.ID); err != nil || len(sessions) != 1 || sessions[0].ID != grants[0].Session.ID {
		t.Errorf("after two sessions expired: %v, %v; want only the refreshed one", sessions, err)
	}

	e.repo.err = errors.New("connection refused")
	if _, err := e.svc.List(t.Context(), e.ann.ID); err == nil {
		t.Error("listing with the database down reported success")
	}
}

func TestRevokeSession(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	a, b := e.login(t), e.login(t)
	if _, err := e.svc.accounts.Register(t.Context(), "bob@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}
	bobs, err := e.svc.Login(t.Context(), "bob@example.com", "correct horse", annClient)
	if err != nil {
		t.Fatal(err)
	}

	if err := e.svc.Revoke(t.Context(), e.ann.ID, b.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.repo.row(b.Session.ID); ok {
		t.Error("the revoked session is still there")
	}
	e.wantRefused(t, b.Refresh.String(), RefreshInvalid)
	if ttl := e.revoked.revoked[b.Session.ID]; ttl != time.Hour+time.Minute {
		t.Errorf("revoked for %s, want the token lifetime of 1h plus 1m", ttl)
	}
	if _, ok := e.repo.row(a.Session.ID); !ok {
		t.Error("revoking one session ended another")
	}

	// Gone already, never there, or another user's: all the same to the caller.
	for name, id := range map[string]uuid.UUID{
		"revoked twice": b.Session.ID, "unknown": uuid.NewV7(), "Bob's": bobs.Session.ID,
	} {
		err := e.svc.Revoke(t.Context(), e.ann.ID, id)
		if code(err) != domain.CodeSessionNotFound || !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s session: %v, want SESSION_NOT_FOUND", name, err)
		}
	}
	if _, ok := e.repo.row(bobs.Session.ID); !ok || e.revoked.has(bobs.Session.ID) {
		t.Error("Ann ended Bob's session")
	}

	e.repo.err = errors.New("connection refused")
	if err := e.svc.Revoke(t.Context(), e.ann.ID, a.Session.ID); err == nil || code(err) != "" {
		t.Errorf("revoke with the database down = %v, want an internal error", err)
	}
	if e.revoked.has(a.Session.ID) {
		t.Error("a session that was not deleted was revoked")
	}
}

// TestRevocationsOutliveTheRequest: a client that hangs up after its session was deleted must not leave the
// session's access tokens valid, and a revocation list that fails must not fail the request.
func TestRevocationsOutliveTheRequest(t *testing.T) {
	t.Parallel()

	e := newSessionEnv(t, testSessionConfig)
	g := e.login(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the in-memory repository ignores the context; the revocation list must not see it canceled
	if ended, err := e.svc.Logout(ctx, g.Refresh.String()); err != nil || !ended {
		t.Fatalf("logout = %t, %v", ended, err)
	}
	if !e.revoked.has(g.Session.ID) || e.revoked.ctxErrs[0] != nil {
		t.Errorf("revocation context error %v, want a context that the hang-up does not cancel", e.revoked.ctxErrs)
	}

	e.revoked.err = errors.New("redis: connection refused")
	g = e.login(t)
	if ended, err := e.svc.Logout(t.Context(), g.Refresh.String()); err != nil || !ended {
		t.Errorf("logout with a failing revocation list = %t, %v; want the session ended without error", ended, err)
	}
	if err := e.svc.Revoke(t.Context(), e.ann.ID, e.login(t).Session.ID); err != nil {
		t.Errorf("revoke with a failing revocation list = %v, want no error", err)
	}
}

func TestSessionsWithoutRevocationList(t *testing.T) {
	t.Parallel()

	c := &clock{now: t0}
	users := newMemUsers()
	accounts := newService(t, users)
	if _, err := accounts.Register(t.Context(), "ann@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}
	svc, err := NewSessions(accounts, newMemSessions(c, users), newTokens(t, c), testSessionConfig, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.Login(t.Context(), "ann@example.com", "correct horse", annClient)
	if err != nil {
		t.Fatal(err)
	}
	if ended, err := svc.Logout(t.Context(), g.Refresh.String()); err != nil || !ended {
		t.Errorf("logout without a revocation list = %t, %v", ended, err)
	}
}
