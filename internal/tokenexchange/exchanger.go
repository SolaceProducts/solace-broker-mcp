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

package tokenexchange

import (
	"errors"
	"net/http"
)

// Exchanger executes RFC 8693 token exchange against a single IdP. One
// instance per process: every field is written once at construction and
// never mutated, so concurrent calls from many goroutines are safe by
// virtue of effectively-immutable state plus the goroutine-safety of
// *http.Client and (when wired) the singleflight group. Per-broker
// values (audience, scopes) and per-user values (subject token) flow
// through Exchange's ExchangeInput, never onto the struct.
type Exchanger struct {
	tokenURL         string
	clientID         string
	clientAuthMethod ClientAuthMethod
	clientSecret     string
	grantType        GrantType
	audienceParam    AudienceFormat
	httpClient       *http.Client
}

// New constructs an Exchanger from Params. The config validator
// (internal/config.validateBrokerOAuthConfig) has already enforced
// every non-runtime field at startup, so this constructor only checks
// runtime-wired dependencies the validator cannot see — specifically
// that HTTPClient is non-nil.
//
// Tests that build Params{...} directly without going through FromConfig
// are responsible for supplying valid enum values; bad values surface at
// request-construction time via the switch default branches in the
// wire-building helpers.
func New(p Params) (*Exchanger, error) {
	if p.HTTPClient == nil {
		return nil, errors.New("tokenexchange: HTTPClient is required")
	}
	return &Exchanger{
		tokenURL:         p.TokenURL,
		clientID:         p.ClientID,
		clientAuthMethod: p.ClientAuthMethod,
		clientSecret:     p.ClientSecret,
		grantType:        p.GrantType,
		audienceParam:    p.AudienceParam,
		httpClient:       p.HTTPClient,
	}, nil
}
