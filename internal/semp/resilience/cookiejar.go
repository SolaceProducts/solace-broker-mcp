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

package resilience

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync/atomic"
)

// SafeCookieJar is a thread-safe wrapper around *cookiejar.Jar that supports
// concurrent SetCookies / Cookies calls plus an atomic Clear() that swaps the
// inner jar.
//
// The standard library jar is itself thread-safe for read/write operations
// but exposes no way to clear stored cookies. The 401 re-auth path in the
// Sender wants to drop session state and force fresh credentials on the
// retry. Replacing the entire jar via direct assignment to
// http.Client.Jar — the previous implementation — races with concurrent
// http.Client.Do reads of the interface value (two-word: type pointer + data
// pointer). The data race detector flags it, and even on architectures where
// torn interface reads do not panic, callers may observe inconsistent jar
// state across a swap.
//
// This type pins the http.Client.Jar slot to a stable *SafeCookieJar
// instance — never re-assigned — and routes the actual jar replacement
// through an atomic.Pointer swap. http.Client.Do always sees the same
// SafeCookieJar value; concurrent SetCookies/Cookies calls always read a
// consistent *cookiejar.Jar via atomic.Load.
type SafeCookieJar struct {
	inner atomic.Pointer[cookiejar.Jar]
}

// Compile-time assertion that SafeCookieJar satisfies the http.CookieJar
// interface required by http.Client.Jar.
var _ http.CookieJar = (*SafeCookieJar)(nil)

// NewSafeCookieJar constructs an empty jar. The underlying cookiejar.New
// only returns an error for a non-nil *cookiejar.Options with bad fields;
// passing nil here cannot fail in practice, but the error is propagated for
// surface parity with cookiejar.New.
func NewSafeCookieJar() (*SafeCookieJar, error) {
	inner, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	j := &SafeCookieJar{}
	j.inner.Store(inner)
	return j, nil
}

// SetCookies implements http.CookieJar.
func (s *SafeCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	s.inner.Load().SetCookies(u, cookies)
}

// Cookies implements http.CookieJar.
func (s *SafeCookieJar) Cookies(u *url.URL) []*http.Cookie {
	return s.inner.Load().Cookies(u)
}

// Clear replaces the inner jar with a fresh, empty one. Concurrent
// SetCookies/Cookies calls in flight observe either the old jar (if they
// loaded the pointer before this Store) or the new one — never a torn read.
// In-flight cookie operations against the old jar complete normally; their
// side effects are then discarded with that jar instance.
//
// cookiejar.New(nil) never returns a non-nil error in practice; the error
// path here is unreachable defensively rather than because it can fire.
func (s *SafeCookieJar) Clear() error {
	fresh, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	s.inner.Store(fresh)
	return nil
}
