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
	"errors"
	"fmt"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Category sentinels for token rejection. Each wraps sdkauth.ErrInvalidToken
// so the SDK's RequireBearerToken routes them as 401 with WWW-Authenticate.
// Sentinel text is client-visible (the SDK writes err.Error() into the 401
// response body), so every string here is static, pre-audited, category-level
// text (CWE-209). Never embed dynamic values.
var (
	errVerificationFailed = fmt.Errorf("%w: verification failed", sdkauth.ErrInvalidToken)
	errMalformedClaims    = fmt.Errorf("%w: malformed claims", sdkauth.ErrInvalidToken)
	errNoSubject          = fmt.Errorf("%w: token has no subject", sdkauth.ErrInvalidToken)
)

// categorySentinels lists every category in sanitization precedence order.
// More specific categories precede broader ones so sanitizeTokenError
// reports the most useful static text.
var categorySentinels = []error{
	errNoSubject,
	errVerificationFailed,
	errMalformedClaims,
}

// sanitizeTokenError maps any internal token-rejection error to the static
// category sentinel a client is allowed to see. Rich internal errors (with
// claim names, json detail, go-oidc messages) must be logged by the caller
// BEFORE sanitizing; this function discards everything except the category.
// Unrecognized errors fall back to the bare SDK sentinel.
func sanitizeTokenError(err error) error {
	for _, s := range categorySentinels {
		if errors.Is(err, s) {
			return s
		}
	}
	return sdkauth.ErrInvalidToken
}
