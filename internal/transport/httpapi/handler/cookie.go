package handler

import (
	"net/http"
	"time"
)

// RefreshCookieName is the cookie that carries the refresh token.
const RefreshCookieName = "cinema_refresh"

// RefreshCookie configures the refresh token cookie.
//
// The cookie is HttpOnly, so scripts on the page cannot read it, and SameSite=Strict, so the browser sends it on
// requests from the API's own site only. It has no Domain attribute: it belongs to the host the browser talks to,
// such as a web client that forwards /v1 to this API. Path limits it to the auth routes, so no other request
// carries it.
type RefreshCookie struct {
	Path string
	// Secure lets the browser send the cookie over HTTPS only. It is off only for development over plain HTTP.
	Secure bool
}

// set sends the refresh token, to be kept until the session expires.
func (c RefreshCookie) set(w http.ResponseWriter, token string, expiresAt time.Time) {
	// Max-Age is a hint for the browser; the server decides by the database clock when the session ends.
	maxAge := max(int(time.Until(expiresAt)/time.Second), 1)
	http.SetCookie(w, c.cookie(token, maxAge))
}

// clear tells the browser to delete the cookie.
func (c RefreshCookie) clear(w http.ResponseWriter) {
	http.SetCookie(w, c.cookie("", -1)) // a negative MaxAge is sent as Max-Age=0
}

func (c RefreshCookie) cookie(value string, maxAge int) *http.Cookie {
	// Secure comes from AUTH_COOKIE_SECURE, which is off only for development over plain HTTP.
	return &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite are fixed; Secure is configured
		Name:     RefreshCookieName,
		Value:    value,
		Path:     c.Path,
		MaxAge:   maxAge,
		Secure:   c.Secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// refreshToken returns the refresh token the request carries, or "" if it has none.
func refreshToken(r *http.Request) string {
	c, err := r.Cookie(RefreshCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
