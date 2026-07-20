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
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

func TestSentinelsUnwrapToSDK(t *testing.T) {
	for _, s := range []struct {
		name     string
		sentinel error
	}{
		{"errVerificationFailed", errVerificationFailed},
		{"errMalformedClaims", errMalformedClaims},
		{"errNoSubject", errNoSubject},
	} {
		t.Run(s.name, func(t *testing.T) {
			if !errors.Is(s.sentinel, sdkauth.ErrInvalidToken) {
				t.Errorf("%s does not unwrap to sdkauth.ErrInvalidToken", s.name)
			}
		})
	}
}

func TestSanitizeTokenError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "errNoSubject passes through",
			err:  errNoSubject,
			want: errNoSubject,
		},
		{
			name: "errVerificationFailed passes through",
			err:  errVerificationFailed,
			want: errVerificationFailed,
		},
		{
			name: "errMalformedClaims passes through",
			err:  errMalformedClaims,
			want: errMalformedClaims,
		},
		{
			name: "wrapped errMalformedClaims maps to sentinel",
			err:  fmt.Errorf("%w: claim %q has unexpected type: some detail", errMalformedClaims, "scope"),
			want: errMalformedClaims,
		},
		{
			name: "wrapped errNoSubject maps to sentinel",
			err:  fmt.Errorf("extra context: %w", errNoSubject),
			want: errNoSubject,
		},
		{
			name: "unknown error falls back to SDK sentinel",
			err:  errors.New("something unexpected"),
			want: sdkauth.ErrInvalidToken,
		},
		{
			name: "bare SDK sentinel falls back to itself",
			err:  sdkauth.ErrInvalidToken,
			want: sdkauth.ErrInvalidToken,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeTokenError(tt.err)
			if got != tt.want {
				t.Errorf("sanitizeTokenError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSentinelTextIsStatic(t *testing.T) {
	tests := []struct {
		sentinel error
		want     string
	}{
		{errVerificationFailed, "invalid token: verification failed"},
		{errMalformedClaims, "invalid token: malformed claims"},
		{errNoSubject, "invalid token: token has no subject"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.sentinel.Error(); got != tt.want {
				t.Errorf("sentinel.Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
