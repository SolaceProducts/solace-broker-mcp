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
	"errors"
	"testing"
)

// recordingJar records Clear() invocations and can be configured to return
// an error from Clear() to exercise the failure branch of HandleAuthFailure.
type recordingJar struct {
	clearErr   error
	clearCalls int
}

func (r *recordingJar) Clear() error {
	r.clearCalls++
	return r.clearErr
}

// TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue pins
// the happy-path contract: on a 401, HandleAuthFailure calls jar.Clear()
// exactly once and returns retry=true so the Sender re-sends with fresh
// Basic credentials. Covered transitively by resilience.Sender tests, but
// isolating it here defends the auth-side contract against refactors of
// the retry loop that might silently drop the Clear() call.
func TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue(t *testing.T) {
	jar := &recordingJar{}
	a := NewBasicAuthenticator("admin", "s3cret", jar)

	res := a.HandleAuthFailure(context.Background(), nil)
	if !res.Retry {
		t.Error("HandleAuthFailure returned Retry=false, want true on successful clear")
	}
	if res.ReAuth {
		t.Error("HandleAuthFailure returned ReAuth=true, want false — the Basic header is static")
	}
	if jar.clearCalls != 1 {
		t.Errorf("jar.Clear() called %d times, want 1", jar.clearCalls)
	}
}

// TestBasicAuthenticator_HandleAuthFailure_JarClearError pins the failure
// branch: if jar.Clear() fails, HandleAuthFailure returns retry=false so
// the Sender does not retry with a possibly-inconsistent jar. This branch
// was previously untested at any level.
func TestBasicAuthenticator_HandleAuthFailure_JarClearError(t *testing.T) {
	sentinel := errors.New("clear failed")
	jar := &recordingJar{clearErr: sentinel}
	a := NewBasicAuthenticator("admin", "s3cret", jar)

	if a.HandleAuthFailure(context.Background(), nil).Retry {
		t.Error("HandleAuthFailure returned Retry=true, want false when jar.Clear() fails")
	}
	if jar.clearCalls != 1 {
		t.Errorf("jar.Clear() called %d times, want 1", jar.clearCalls)
	}
}
