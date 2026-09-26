package dto

import (
	"time"

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

// AccessToken is a successful login, in the shape of an OAuth 2.0 token response (RFC 6749 §5.1).
type AccessToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"` // seconds
}

// NewUser maps a domain user. The password hash is never included.
func NewUser(u domain.User) User {
	return User{ID: u.ID.String(), Email: u.Email, Role: string(u.Role), CreatedAt: u.CreatedAt.UTC()}
}

// NewAccessToken maps an issued token.
func NewAccessToken(t auth.AccessToken) AccessToken {
	return AccessToken{AccessToken: t.Token, TokenType: "Bearer", ExpiresIn: int64(t.ExpiresIn.Seconds())}
}
