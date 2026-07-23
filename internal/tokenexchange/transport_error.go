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
	"net"
)

// classifyTransportError maps a Go error from httpClient.Do (no usable HTTP
// response was received) to its FailureClass. The distinction the circuit
// breaker cares about: a permanent, operator-caused endpoint misconfiguration
// (bad TLS trust/hostname, or a DNS name that does not exist) must be EXCLUDED
// from the breaker — it is not an IdP outage and would never heal, so counting
// it would let one operator typo trip the shared breaker for every tenant.
// Everything else at this layer (connection refused/reset, unreachable, TLS or
// DNS *timeout*, header/read timeout) is a transient network fault that counts.
//
// Returns FailureClassConfig for the misconfiguration cases, FailureClassNetwork
// otherwise. Only called on the error path of httpClient.Do.
func classifyTransportError(err error) FailureClass {
	// TLS trust / certificate / hostname problems: the endpoint's certificate
	// cannot be validated or does not match. Operator/PKI misconfiguration, not
	// an outage.
	var unknownAuthority x509.UnknownAuthorityError
	var certInvalid x509.CertificateInvalidError
	var hostnameErr x509.HostnameError
	if errors.As(err, &unknownAuthority) ||
		errors.As(err, &certInvalid) ||
		errors.As(err, &hostnameErr) {
		return FailureClassConfig
	}

	// DNS name-not-found (NXDOMAIN): the configured hostname does not resolve.
	// A DNS *timeout* is deliberately excluded from this branch — it is a
	// transient resolver fault and stays FailureClassNetwork so it counts.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return FailureClassConfig
	}

	return FailureClassNetwork
}
