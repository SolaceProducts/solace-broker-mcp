package auth

import (
	"context"
	"net/http"
)

// BasicAuthenticator attaches HTTP Basic authentication using credentials
// captured at construction. Fields are never written after construction,
// so AddAuth is safe to call concurrently from any number of goroutines.
type BasicAuthenticator struct {
	username string
	password string
}

// NewBasicAuthenticator returns a BasicAuthenticator that will attach the
// given credentials to every request via http.Request.SetBasicAuth.
func NewBasicAuthenticator(username, password string) *BasicAuthenticator {
	return &BasicAuthenticator{username: username, password: password}
}

// AddAuth attaches an Authorization: Basic header to req. The ctx is
// accepted for interface conformance and ignored — basic auth needs no
// I/O and no cancellation. Always returns nil.
func (a *BasicAuthenticator) AddAuth(_ context.Context, req *http.Request) error {
	req.SetBasicAuth(a.username, a.password)
	return nil
}
