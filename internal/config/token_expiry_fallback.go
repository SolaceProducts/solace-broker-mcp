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

// validateTokenExpiryFallback rejects an explicitly configured non-positive
// duration. Nil is valid and preserves fail-closed handling when an IdP omits
// expires_in.
func validateTokenExpiryFallback(fallback *time.Duration) []error {
	if fallback == nil {
		return nil
	}
	if *fallback <= 0 {
		return []error{fmt.Errorf("broker_oauth.token_expiry_fallback must be positive, got %v", *fallback)}
	}
	return nil
}
