// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package auth

import (
	"context"
	"net/http"
)

// BearerAuthenticator attaches a static Bearer token captured at
// construction. Fields are never written after construction, so AddAuth
// and HandleAuthFailure are safe to call concurrently from any number
// of goroutines.
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

// HandleAuthFailure declines to retry: a static bearer token cannot be
// refreshed, so retrying with the same token is pointless.
func (a *BearerAuthenticator) HandleAuthFailure(_ context.Context, _ http.Header) AuthFailureResult {
	return AuthFailureResult{}
}
