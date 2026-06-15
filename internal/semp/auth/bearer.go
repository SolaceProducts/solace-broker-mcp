package auth

import (
	"context"
	"net/http"
)

// BearerAuthenticator attaches a static Bearer token captured at
// construction. Fields are never written after construction, so AddAuth
// is safe to call concurrently from any number of goroutines.
type BearerAuthenticator struct {
	token string
}

// NewBearerAuthenticator returns a BearerAuthenticator that will attach
// the given token as "Authorization: Bearer <token>".
func NewBearerAuthenticator(token string) *BearerAuthenticator {
	return &BearerAuthenticator{token: token}
}

// AddAuth sets the Authorization: Bearer header on req. The ctx is
// accepted for interface conformance and ignored — static bearer auth
// needs no I/O and no cancellation. Always returns nil.
func (a *BearerAuthenticator) AddAuth(_ context.Context, req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.token)
	return nil
}
