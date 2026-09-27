package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/metrics"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/middleware"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// AuthService is the part of the account use cases that the HTTP layer needs.
type AuthService interface {
	Register(ctx context.Context, email, password string) (domain.User, error)
	User(ctx context.Context, id uuid.UUID) (domain.User, error)
}

// SessionService runs logins, refreshes, and logouts, and lists and revokes sessions. It is implemented by
// service/auth.Sessions.
type SessionService interface {
	Login(ctx context.Context, email, password string, c auth.Client) (auth.Grant, error)
	Refresh(ctx context.Context, token string, c auth.Client) (auth.Grant, auth.RefreshResult, error)
	Logout(ctx context.Context, token string) (ended bool, err error)
	LogoutAll(ctx context.Context, userID uuid.UUID) (ended int, err error)
	List(ctx context.Context, userID uuid.UUID) ([]domain.Session, error)
	Revoke(ctx context.Context, userID, sessionID uuid.UUID) error
}

// AuthConfig configures the account handlers.
type AuthConfig struct {
	Cookie RefreshCookie
	// TrustedProxies are the networks whose X-Forwarded-For names the client address that a session records.
	TrustedProxies []netip.Prefix
}

// errNoRefreshToken answers a refresh without the cookie. Its detail differs from that of other refresh failures,
// because the client can tell by itself that it sent no cookie, for example to a path outside AUTH_COOKIE_PATH.
var errNoRefreshToken = domain.Unauthenticated(domain.CodeRefreshInvalid,
	"the request carries no refresh token cookie; log in again")

// refreshResults maps refresh results to the result label of cinema_auth_refresh_total.
var refreshResults = map[auth.RefreshResult]string{
	auth.RefreshRotated:       metrics.RefreshRotated,
	auth.RefreshReissued:      metrics.RefreshReissued,
	auth.RefreshGrace:         metrics.RefreshGrace,
	auth.RefreshReuseDetected: metrics.RefreshReuseDetected,
	auth.RefreshExpired:       metrics.RefreshExpired,
	auth.RefreshInvalid:       metrics.RefreshInvalid,
}

// Auth serves registration, sessions, and the caller's own account.
type Auth struct {
	accounts AuthService
	sessions SessionService
	cfg      AuthConfig
	metrics  *metrics.Metrics
	logger   *slog.Logger
}

// NewAuth returns the account handlers. Refreshes and revoked sessions are counted in m.
func NewAuth(accounts AuthService, sessions SessionService, cfg AuthConfig, m *metrics.Metrics, logger *slog.Logger) *Auth {
	return &Auth{accounts: accounts, sessions: sessions, cfg: cfg, metrics: m, logger: logger}
}

// Register handles POST /v1/auth/register. It starts no session: the client logs in next.
func (h *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req dto.Credentials
	if !decodeJSON(w, r, &req) {
		return
	}

	u, err := h.accounts.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusCreated, dto.NewUser(u))
}

// Login handles POST /v1/auth/login. It starts a session and answers with an access token, and with the refresh
// token in a cookie.
func (h *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req dto.Credentials
	if !decodeJSON(w, r, &req) {
		return
	}

	g, err := h.sessions.Login(r.Context(), req.Email, req.Password, h.client(r))
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	h.countRevocations(metrics.RevocationEvicted, g.Evicted)
	h.writeGrant(w, g)
}

// Refresh handles POST /v1/auth/refresh. The refresh token cookie authenticates it, and its body must be the JSON
// object {}, so that a cross-site form cannot send it. A failed refresh deletes the cookie.
func (h *Auth) Refresh(w http.ResponseWriter, r *http.Request) {
	var req dto.Empty
	if !decodeJSON(w, r, &req) {
		return
	}

	var (
		g      auth.Grant
		result auth.RefreshResult
		err    = errNoRefreshToken
	)
	if token := refreshToken(r); token != "" {
		g, result, err = h.sessions.Refresh(r.Context(), token, h.client(r))
	} else {
		result = auth.RefreshInvalid
	}
	if label, ok := refreshResults[result]; ok {
		h.metrics.AuthRefreshes.WithLabelValues(label).Inc()
	}
	if result == auth.RefreshReuseDetected {
		h.countRevocations(metrics.RevocationReuse, 1)
	}

	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		if errors.Is(err, domain.ErrUnauthenticated) {
			h.cfg.Cookie.clear(w) // the token is of no use any more; the client must log in again
		}
		writeError(h.logger, w, r, err)
		return
	}
	h.writeGrant(w, g)
}

// Logout handles POST /v1/auth/logout. It ends the session of the refresh token cookie, if there is one, and
// deletes the cookie. It needs no access token, so it works after the access token has expired, and it answers
// 204 whatever the cookie holds, so it can be repeated. Its body must be {}, like that of a refresh.
func (h *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	var req dto.Empty
	if !decodeJSON(w, r, &req) {
		return
	}

	h.cfg.Cookie.clear(w)
	if token := refreshToken(r); token != "" {
		ended, err := h.sessions.Logout(r.Context(), token)
		if err != nil {
			writeError(h.logger, w, r, err)
			return
		}
		if ended {
			h.countRevocations(metrics.RevocationLogout, 1)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// LogoutAll handles POST /v1/auth/logout-all. It ends every session of the caller and deletes the cookie. It
// must run behind the Authenticate middleware.
func (h *Auth) LogoutAll(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}

	ended, err := h.sessions.LogoutAll(r.Context(), p.UserID)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	h.countRevocations(metrics.RevocationLogoutAll, ended)
	h.cfg.Cookie.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// ListSessions handles GET /v1/auth/sessions: the caller's sessions, most recently used first, with the session
// of the caller's access token marked current. It must run behind the Authenticate middleware.
func (h *Auth) ListSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}

	sessions, err := h.sessions.List(r.Context(), p.UserID)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewSessionList(sessions, p.SessionID))
}

// DeleteSession handles DELETE /v1/auth/sessions/{sessionID}. It ends a session of the caller, and stops its access
// tokens at once. When that is the caller's own session, it also deletes the cookie, as a logout would. It must
// run behind the Authenticate middleware.
func (h *Auth) DeleteSession(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}
	var ps params
	id := ps.pathUUID(r, "sessionID")
	if !ps.ok(w, r) {
		return
	}

	if err := h.sessions.Revoke(r.Context(), p.UserID, id); err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	h.countRevocations(metrics.RevocationDeleted, 1)
	if id == p.SessionID {
		h.cfg.Cookie.clear(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /v1/me. It must run behind the Authenticate middleware.
func (h *Auth) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(h.logger, w, r)
	if !ok {
		return
	}

	u, err := h.accounts.User(r.Context(), p.UserID)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewUser(u))
}

// countRevocations counts n sessions that ended before they expired, for reason.
func (h *Auth) countRevocations(reason string, n int) {
	if n > 0 {
		h.metrics.SessionRevocations.WithLabelValues(reason).Add(float64(n))
	}
}

// writeGrant answers a login or refresh: the access token in the body, the refresh token in the cookie.
func (h *Auth) writeGrant(w http.ResponseWriter, g auth.Grant) {
	h.cfg.Cookie.set(w, g.Refresh.String(), g.Session.ExpiresAt)
	w.Header().Set("Cache-Control", "no-store") // RFC 6749 §5.1: token responses must not be cached
	render.JSON(w, http.StatusOK, dto.NewAccessToken(g))
}

// client describes where r comes from, for the session to record.
func (h *Auth) client(r *http.Request) auth.Client {
	ip, _ := middleware.ClientIP(r, h.cfg.TrustedProxies) // the zero Addr, recorded as unknown, if there is none
	return auth.Client{UserAgent: r.UserAgent(), IP: ip}
}
