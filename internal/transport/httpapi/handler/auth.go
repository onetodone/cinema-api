package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/auth"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/dto"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/render"
)

// AuthService is the part of the account use cases that the HTTP layer needs.
type AuthService interface {
	Register(ctx context.Context, email, password string) (domain.User, error)
	Authenticate(ctx context.Context, email, password string) (domain.User, error)
	User(ctx context.Context, id uuid.UUID) (domain.User, error)
}

// TokenIssuer signs access tokens. It is implemented by service/auth.Tokens.
type TokenIssuer interface {
	Issue(p domain.Principal) (auth.AccessToken, error)
}

// Auth serves registration, login, and the caller's own account.
type Auth struct {
	svc    AuthService
	tokens TokenIssuer
	logger *slog.Logger
}

// NewAuth returns the account handlers.
func NewAuth(svc AuthService, tokens TokenIssuer, logger *slog.Logger) *Auth {
	return &Auth{svc: svc, tokens: tokens, logger: logger}
}

// Register handles POST /v1/auth/register.
func (h *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req dto.Credentials
	if !decodeJSON(w, r, &req) {
		return
	}

	u, err := h.svc.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusCreated, dto.NewUser(u))
}

// Login handles POST /v1/auth/login. It answers with a bearer token.
func (h *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req dto.Credentials
	if !decodeJSON(w, r, &req) {
		return
	}

	u, err := h.svc.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	token, err := h.tokens.Issue(domain.Principal{UserID: u.ID, Role: u.Role})
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}

	w.Header().Set("Cache-Control", "no-store") // RFC 6749 §5.1: token responses must not be cached
	render.JSON(w, http.StatusOK, dto.NewAccessToken(token))
}

// Me handles GET /v1/me. It must run behind the Authenticate middleware.
func (h *Auth) Me(w http.ResponseWriter, r *http.Request) {
	p, ok := principal.FromContext(r.Context())
	if !ok {
		writeError(h.logger, w, r, errors.New("GET /v1/me is routed without authentication"))
		return
	}

	u, err := h.svc.User(r.Context(), p.UserID)
	if err != nil {
		writeError(h.logger, w, r, err)
		return
	}
	render.JSON(w, http.StatusOK, dto.NewUser(u))
}
