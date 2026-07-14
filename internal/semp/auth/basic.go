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
// given credentials to every request via http.Request.SetBasicAuth. The
// jar is required — HandleAuthFailure clears it to force fresh Basic
// credentials on the retry, so an authenticator without a jar cannot
// recover from a 401. The non-nil jar invariant is owned by the caller
// chain: newCookieJar builds and returns a non-nil jar whenever
// auth.mode is basic, and NewBrokerClient propagates any construction
// error before newAuthenticator dispatches to this constructor.
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
// re-sends raw Basic credentials. On success it returns Retry=true so the
// Sender retries; ReAuth stays false because the Basic Authorization header
// is static — the recovery here is the jar clear, and re-running AddAuth on
// the retry would only re-set the same header.
func (a *BasicAuthenticator) HandleAuthFailure(_ context.Context, _ http.Header) AuthFailureResult {
	if err := a.jar.Clear(); err != nil {
		slog.Warn("basic auth: 401 received but failed to clear cookie jar",
			slog.String("error", err.Error()))
		return AuthFailureResult{}
	}
	return AuthFailureResult{Retry: true}
}
