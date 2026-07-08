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
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache/cachetest"
)

// validParams returns a Params struct with every field set to a value
// that would survive every layer of validation. Tests start from this
// and mutate one field at a time to exercise specific cases.
//
// Takes *testing.T so the cache built into Cache is closed via t.Cleanup
// when the test finishes — leaving that off (as the earlier mustTestCache
// helper did) leaked Otter's sweeper goroutine per test and pinned the
// cache's heap for the whole `go test` run.
func validParams(t *testing.T) Params {
	t.Helper()
	return Params{
		TokenURL:         "https://idp.example.com/token",
		ClientID:         "solace-mcp-server",
		ClientAuthMethod: ClientSecretPost,
		ClientSecret:     "test-secret",
		GrantType:        GrantTypeTokenExchange,
		AudienceParam:    AudienceParamAudience,
		HTTPClient:       &http.Client{},
		Cache:            cachetest.Default(t),
	}
}

// TestNew_HTTPClientNilRejected pins the only runtime check New performs.
// Every other field is trusted (config validator enforced it at startup),
// but HTTPClient is wired at runtime from outside config and must be
// non-nil for the Exchanger to function. A nil here is a programming
// error in main's wiring — fail fast, do not ship a half-built Exchanger.
func TestNew_HTTPClientNilRejected(t *testing.T) {
	p := validParams(t)
	p.HTTPClient = nil

	ex, err := New(p)
	if err == nil {
		t.Fatal("expected error for nil HTTPClient")
	}
	if ex != nil {
		t.Errorf("expected nil Exchanger on error, got %#v", ex)
	}
	if !strings.Contains(err.Error(), "HTTPClient") {
		t.Errorf("error message should mention HTTPClient, got: %v", err)
	}
}

// TestNew_CacheNilRejected pins the runtime check that Cache must be non-nil.
func TestNew_CacheNilRejected(t *testing.T) {
	p := validParams(t)
	p.Cache = nil

	ex, err := New(p)
	if err == nil {
		t.Fatal("expected error for nil Cache")
	}
	if ex != nil {
		t.Errorf("expected nil Exchanger on error, got %#v", ex)
	}
	if !strings.Contains(err.Error(), "Cache") {
		t.Errorf("error message should mention Cache, got: %v", err)
	}
}

// TestNew_HappyPath verifies that a fully-populated Params yields a
// non-nil Exchanger with no error.
func TestNew_HappyPath(t *testing.T) {
	ex, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ex == nil {
		t.Fatal("expected non-nil Exchanger")
	}
}

// TestNew_FieldsAreCopied verifies that New copies Params fields onto
// the Exchanger and that subsequent mutation of the Params struct does
// not affect the constructed Exchanger. Effectively-immutable state is
// the foundation of the concurrency safety story (Decision 8).
func TestNew_FieldsAreCopied(t *testing.T) {
	p := validParams(t)
	ex, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Mutate the original Params struct.
	p.TokenURL = "https://changed.example.com/token"
	p.ClientID = "changed"
	p.ClientSecret = "changed"

	if ex.tokenURL == p.TokenURL {
		t.Error("Exchanger.tokenURL aliased to caller's Params after mutation")
	}
	if ex.clientID == p.ClientID {
		t.Error("Exchanger.clientID aliased to caller's Params after mutation")
	}
	if ex.clientSecret == p.ClientSecret {
		t.Error("Exchanger.clientSecret aliased to caller's Params after mutation")
	}
}
