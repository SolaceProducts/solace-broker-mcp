package auth

import (
	"context"
	"log/slog"
	"net/http"
)

// BasicAuthenticator attaches HTTP Basic authentication using credentials
// captured at construction. Fields are never written after construction,
// so AddAuth and HandleAuthFailure are safe to call concurrently from
// any number of goroutines.
type BasicAuthenticator struct {
	username string
	password string
	jar      CookieJarClearer
}

// NewBasicAuthenticator returns a BasicAuthenticator that will attach the
// given credentials to every request via http.Request.SetBasicAuth.
// jar may be nil — HandleAuthFailure will return retry=false when no jar
// is available.
func NewBasicAuthenticator(username, password string, jar CookieJarClearer) *BasicAuthenticator {
	return &BasicAuthenticator{username: username, password: password, jar: jar}
}

// AddAuth attaches an Authorization: Basic header to req. The ctx is
// accepted for interface conformance and ignored — basic auth needs no
// I/O and no cancellation. Always returns nil.
func (a *BasicAuthenticator) AddAuth(_ context.Context, req *http.Request) error {
	req.SetBasicAuth(a.username, a.password)
	return nil
}

// HandleAuthFailure clears stale session cookies so the next request
// re-sends raw Basic credentials. Returns retry=true on success so the
// Sender retries the request. Returns retry=false when no jar is
// available or the clear fails.
func (a *BasicAuthenticator) HandleAuthFailure(_ context.Context, _ *http.Response) bool {
	if a.jar == nil {
		return false
	}
	if err := a.jar.Clear(); err != nil {
		slog.Warn("basic auth: 401 received but failed to clear cookie jar",
			slog.String("error", err.Error()))
		return false
	}
	return true
}
