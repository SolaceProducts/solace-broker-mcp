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
	"testing"
)

// TestPrincipalFrom_EmptyContext pins the accessor contract for the skeleton:
// with no principal stashed, PrincipalFrom returns the zero-value Principal.
// This is the property later code depends on (absence reads as the zero value,
// not a panic or sentinel).
func TestPrincipalFrom_EmptyContext(t *testing.T) {
	t.Parallel()

	got := PrincipalFrom(context.Background())
	if got != (Principal{}) {
		t.Errorf("PrincipalFrom(empty) = %+v, want zero-value Principal", got)
	}
}

// TestPrincipalFrom_RoundTrip exercises the value-present branch: only this
// package can construct principalKey{}, so it guards against a future bug here
// that stores a Principal but fails to read it back.
//
// Known limitation (raised in PR review): Principal is currently an empty
// struct, so the round-trip value is indistinguishable from the zero value.
// This assertion cannot yet tell "read the stored value back" from "type
// assertion failed and returned the zero value" — it documents the contract
// and runs the success-branch path, but provides no failure signal until
// Principal has fields.
//
// TODO(Story 20 – principal population): once Principal carries fields
// (e.g. preferred_username, email), set distinct non-zero values here so the
// assertion actually proves the stored value is read back, rather than a zero
// value that coincidentally matches.
func TestPrincipalFrom_RoundTrip(t *testing.T) {
	t.Parallel()

	want := Principal{}
	ctx := context.WithValue(context.Background(), principalKey{}, want)
	if got := PrincipalFrom(ctx); got != want {
		t.Errorf("PrincipalFrom(ctx) = %+v, want %+v", got, want)
	}
}
