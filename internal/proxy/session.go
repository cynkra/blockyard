package proxy

import (
	"net/http"
	"strings"
)

const cookieName = "blockyard_session"

// extractSessionID reads the blockyard_session cookie from the request.
// Returns empty string if the cookie is missing or empty.
func extractSessionID(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	return c.Value
}

// sessionCookie builds a Set-Cookie header value for the given session
// ID and app name. Path is scoped to /app/{name}/ so the cookie is not
// sent to other apps or the API.
// The Secure flag is derived from externalURL (same as auth cookies).
func sessionCookie(sessionID, appName, externalURL string) *http.Cookie {
	secure := strings.HasPrefix(externalURL, "https://")
	return &http.Cookie{
		Name:     cookieName,
		Value:    sessionID,
		Path:     "/app/" + appName + "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
}
