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
	"net/url"
	"sync"
	"testing"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

func TestSafeCookieJar_SetAndGet(t *testing.T) {
	jar, err := NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	u := mustURL(t, "https://broker.example.com/SEMP")

	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "abc"}})

	got := jar.Cookies(u)
	if len(got) != 1 || got[0].Name != "session" || got[0].Value != "abc" {
		t.Errorf("Cookies = %+v, want one cookie session=abc", got)
	}
}

func TestSafeCookieJar_Clear_EmptiesTheJar(t *testing.T) {
	jar, err := NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	u := mustURL(t, "https://broker.example.com/SEMP")

	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "abc"}})
	if len(jar.Cookies(u)) != 1 {
		t.Fatal("precondition: expected 1 cookie before Clear")
	}

	if err := jar.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := jar.Cookies(u); len(got) != 0 {
		t.Errorf("after Clear: Cookies = %+v, want empty", got)
	}
}

// TestSafeCookieJar_ConcurrentSetClear hammers SetCookies, Cookies, and Clear
// from many goroutines. Run under `go test -race`: a torn read or unguarded
// write on the inner jar pointer would surface here.
func TestSafeCookieJar_ConcurrentSetClear(t *testing.T) {
	jar, err := NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	u := mustURL(t, "https://broker.example.com/SEMP")

	const (
		readers = 8
		writers = 8
		clears  = 4
		iters   = 200
	)

	var wg sync.WaitGroup
	wg.Add(readers + writers + clears)

	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "abc"}})
			}
		}()
	}
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_ = jar.Cookies(u)
			}
		}()
	}
	for i := 0; i < clears; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if err := jar.Clear(); err != nil {
					t.Errorf("Clear: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
}
