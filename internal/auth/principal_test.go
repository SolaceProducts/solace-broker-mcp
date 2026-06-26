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

// TestPrincipalFrom_RoundTrip is defensive: only this package can construct
// principalKey{}, so this guards against a future bug in this package that
// stores a Principal and then fails to read it back. Today Principal is empty,
// so the round-trip value equals the zero value, but the path (value present →
// accessor returns it) is exercised.
func TestPrincipalFrom_RoundTrip(t *testing.T) {
	t.Parallel()

	want := Principal{}
	ctx := context.WithValue(context.Background(), principalKey{}, want)
	if got := PrincipalFrom(ctx); got != want {
		t.Errorf("PrincipalFrom(ctx) = %+v, want %+v", got, want)
	}
}
