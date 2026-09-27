package dto

import (
	"time"
	"uuid"

	"github.com/onetodone/cinema-api/internal/domain"
	"github.com/onetodone/cinema-api/internal/service/auth"
)

// Credentials is the body of the register and login requests.
type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// User is an account as its owner sees it.
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// AccessToken is a successful login or refresh, in the shape of an OAuth 2.0 token response (RFC 6749 §5.1), with
// the account it belongs to. The refresh token travels in a cookie, never in the body.
type AccessToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"` // seconds
	User        User   `json:"user"`
}

// NewUser maps a domain user. The password hash is never included.
func NewUser(u domain.User) User {
	return User{ID: u.ID.String(), Email: u.Email, Role: string(u.Role), CreatedAt: u.CreatedAt.UTC()}
}

// NewAccessToken maps the access token and the account of a grant.
func NewAccessToken(g auth.Grant) AccessToken {
	return AccessToken{
		AccessToken: g.Access.Token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(g.Access.ExpiresIn.Seconds()),
		User:        NewUser(g.User),
	}
}

// Empty is the body of requests that carry nothing but must still be JSON, such as a refresh: {}. A cross-site
// HTML form cannot send JSON, so requiring it keeps such forms from triggering requests that the cookie
// authenticates.
type Empty struct{}

// Session is one of the caller's sessions: a browser or device that is logged in.
type Session struct {
	ID string `json:"id"`
	// Current marks the session of the access token that asked.
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"created_at"`
	// LastUsedAt is the last login or refresh; requests made with access tokens do not count.
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	UserAgent  string    `json:"user_agent"`
	IP         string    `json:"ip,omitempty"` // absent when unknown
}

// SessionList is the caller's sessions, most recently used first.
type SessionList struct {
	Items []Session `json:"items"`
}

// NewSessionList maps sessions, marking the one whose id is current.
func NewSessionList(sessions []domain.Session, current uuid.UUID) SessionList {
	items := make([]Session, 0, len(sessions))
	for _, s := range sessions {
		item := Session{
			ID:         s.ID.String(),
			Current:    s.ID == current,
			CreatedAt:  s.CreatedAt.UTC(),
			LastUsedAt: s.LastUsedAt.UTC(),
			ExpiresAt:  s.ExpiresAt.UTC(),
			UserAgent:  s.UserAgent,
		}
		if s.IP.IsValid() {
			item.IP = s.IP.String()
		}
		items = append(items, item)
	}
	return SessionList{Items: items}
}
