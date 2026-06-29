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

// Package auth — principal context-key scaffold.
//
// This file is a skeleton (SOL-151278). It establishes the context-key and
// accessor shape that later observability stories build on: correlation,
// audit, and metrics all need to read the authenticated principal off the
// request context. The Principal struct is deliberately empty for now — the
// story that populates it (subject, scopes, etc.) and the middleware that
// stashes it onto the context land separately.
package auth

import "context"

// principalKey is the unexported context key under which the authenticated
// Principal is stored. Mirrors the rawSubjectTokenKey{} pattern in
// raw_subject_token.go: an empty-struct, package-private type makes the key
// impossible to construct from outside this package, so no other package can
// collide with it or read the value except through PrincipalFrom.
type principalKey struct{}

// Principal identifies the authenticated caller behind a request. It is the
// value observability code reads off the context to attribute correlation,
// audit, and metrics records to a caller.
//
// Skeleton (SOL-151278): intentionally empty. Fields (e.g. subject, scopes)
// are added by the story that wires principal extraction into the request
// path. Keeping it empty now lets dependent packages compile against the
// type and accessor without committing to a field set prematurely.
type Principal struct{}

// PrincipalFrom returns the Principal stored on ctx, or the zero-value
// Principal when none is set. The skeleton has no writer yet, so today this
// always returns the zero value; the contract (zero value on absence) is what
// later code depends on.
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{}
}
