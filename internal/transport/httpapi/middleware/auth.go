package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/platform/logging"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/principal"
	"github.com/onetodone/cinema-api/internal/transport/httpapi/problem"
)

// TokenVerifier checks an access token and returns the caller it identifies. It is implemented by
// service/auth.Tokens.
type TokenVerifier interface {
	Verify(token string) (domain.Principal, error)
}

// RevocationList tells whether a session has ended before it expired. It is implemented by
// repository/redis.Revocations.
type RevocationList interface {
	Revoked(ctx context.Context, sessionID uuid.UUID) (bool, error)
}

var (
	errAuthRequired   = domain.Unauthenticated(domain.CodeUnauthenticated, "this endpoint requires a bearer token")
	errNoToken        = domain.Unauthenticated(domain.CodeInvalidToken, "the bearer token is empty")
	errSessionRevoked = domain.Unauthenticated(domain.CodeInvalidToken, "the session of the access token has ended")
	errForbidden      = domain.Forbidden(domain.CodeForbidden, "your role does not allow this action")
)

// Challenges added to the realm (RFC 6750 §3). A request without credentials gets the bare challenge.
const (
	invalidTokenChallenge      = problem.BearerChallenge + `, error="invalid_token"`
	insufficientScopeChallenge = problem.BearerChallenge + `, error="insufficient_scope"`
)

// Authenticate requires a valid bearer token (RFC 6750):
//   - no Authorization header, or another scheme: 401 UNAUTHENTICATED;
//   - an empty, malformed, forged, or expired token: 401 INVALID_TOKEN or TOKEN_EXPIRED, with
//     error="invalid_token" in the WWW-Authenticate challenge;
//   - a token whose session is on the revocation list: 401 INVALID_TOKEN, the same way. When the list cannot be
//     read, the token passes (fail open): it expires within JWT_TTL, and its session can no longer refresh. A nil
//     list checks nothing.
//
// On success the caller is stored in the request context (read it with principal.FromContext), and every
// log record written with that context carries the caller's user id.
func Authenticate(verifier TokenVerifier, revoked RevocationList, logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok {
				problem.Write(w, r, problem.FromError(errAuthRequired))
				return
			}
			if token == "" {
				w.Header().Set("WWW-Authenticate", invalidTokenChallenge)
				problem.Write(w, r, problem.FromError(errNoToken))
				return
			}

			p, err := verifier.Verify(token)
			if err != nil {
				// The reason (bad signature, wrong issuer, ...) is logged, never sent: it would help forgers.
				logger.DebugContext(r.Context(), "access token rejected", slog.Any("error", err))
				w.Header().Set("WWW-Authenticate", invalidTokenChallenge)
				problem.Write(w, r, problem.FromError(err))
				return
			}
			if revoked != nil {
				// On error the list has recorded the failure, and the token passes.
				if gone, err := revoked.Revoked(r.Context(), p.SessionID); err == nil && gone {
					logger.DebugContext(r.Context(), "access token of an ended session rejected",
						slog.String("session_id", p.SessionID.String()))
					w.Header().Set("WWW-Authenticate", invalidTokenChallenge)
					problem.Write(w, r, problem.FromError(errSessionRevoked))
					return
				}
			}

			ctx := principal.NewContext(r.Context(), p)
			ctx = logging.WithAttrs(ctx, slog.String("user_id", p.UserID.String()))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the token of a "Bearer <token>" header. ok is false when the header is missing or
// uses another scheme; the scheme name is case-insensitive (RFC 9110 §11.1).
func bearerToken(header string) (token string, ok bool) {
	scheme, token, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// RequireRole lets callers with one of roles through and answers 403 FORBIDDEN to everyone else. It must
// run inside Authenticate; without a caller in the context it fails closed with 401.
func RequireRole(roles ...domain.Role) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := principal.FromContext(r.Context())
			if !ok {
				problem.Write(w, r, problem.FromError(errAuthRequired))
				return
			}
			if !slices.Contains(roles, p.Role) {
				w.Header().Set("WWW-Authenticate", insufficientScopeChallenge)
				problem.Write(w, r, problem.FromError(errForbidden))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
