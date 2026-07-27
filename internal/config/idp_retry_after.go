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

package config

import (
	"fmt"
	"time"
)

// IdPRetryAfterConfig is the operator-facing schema for the token-exchange
// Retry-After gate (SOL-152285). Nests under broker_oauth for the same
// reason IdPCircuitBreakerConfig does.
//
// MaxHonoredDuration can't be zero — see validateIdPRetryAfter: the runtime
// field it feeds already uses zero to mean "use the shipped default".
type IdPRetryAfterConfig struct {
	MaxHonoredDuration *time.Duration `yaml:"max_honored_duration"`
}

// MaxIdPRetryAfterHonoredDuration is a sanity ceiling only, not a
// recommended setting — same philosophy as idp_circuit_breaker.go's ceilings.
const MaxIdPRetryAfterHonoredDuration = time.Hour

// validateIdPRetryAfter rejects zero (not just negatives): the runtime
// can't tell an explicit zero from an omitted field once translated, so
// zero would silently become "use the default" instead of the operator's
// intent.
func validateIdPRetryAfter(ra *IdPRetryAfterConfig) []error {
	if ra == nil {
		return nil
	}
	var errs []error

	if ra.MaxHonoredDuration != nil && (*ra.MaxHonoredDuration <= 0 || *ra.MaxHonoredDuration > MaxIdPRetryAfterHonoredDuration) {
		errs = append(errs, fmt.Errorf("broker_oauth.retry_after.max_honored_duration must be in (0, %v], got %v", MaxIdPRetryAfterHonoredDuration, *ra.MaxHonoredDuration))
	}

	return errs
}
