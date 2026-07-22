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
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
)

// TestClassifyTransportError maps each Go transport error to its FailureClass.
// The split that matters for the breaker: permanent endpoint misconfiguration
// (TLS trust/hostname, DNS name-not-found) → Config (excluded); everything else
// including a DNS *timeout* → Network (counted). Several cases wrap the cause
// (via fmt.Errorf %w, or a net.OpError) so the test exercises errors.As
// reaching through a chain the way a real http.Client error arrives.
func TestClassifyTransportError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{
			name: "unknown certificate authority",
			err:  fmt.Errorf("Get %q: %w", "https://idp", x509.UnknownAuthorityError{}),
			want: FailureClassConfig,
		},
		{
			name: "certificate invalid (expired)",
			err:  fmt.Errorf("tls: %w", x509.CertificateInvalidError{Reason: x509.Expired}),
			want: FailureClassConfig,
		},
		{
			name: "hostname mismatch",
			err:  fmt.Errorf("Get: %w", x509.HostnameError{Host: "wrong.example.com"}),
			want: FailureClassConfig,
		},
		{
			name: "dns name not found (NXDOMAIN)",
			err:  &net.DNSError{Err: "no such host", Name: "typo.example", IsNotFound: true},
			want: FailureClassConfig,
		},
		{
			name: "dns timeout is transient, not config",
			err:  &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true},
			want: FailureClassNetwork,
		},
		{
			name: "connection refused",
			err:  &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			want: FailureClassNetwork,
		},
		{
			name: "connection reset",
			err:  &net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
			want: FailureClassNetwork,
		},
		{
			name: "generic transport error",
			err:  errors.New("some unrecognized transport failure"),
			want: FailureClassNetwork,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyTransportError(tc.err); got != tc.want {
				t.Errorf("classifyTransportError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
