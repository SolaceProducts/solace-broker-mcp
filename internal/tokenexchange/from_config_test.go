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
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache/cachetest"
)

func validBrokerOAuthConfig() *config.BrokerOAuthConfig {
	return &config.BrokerOAuthConfig{
		TokenURL: "https://idp.example.com/token",
		ClientID: "mcp-server",
		ClientAuth: config.BrokerClientAuth{
			ClientSecretPost: &config.ClientSecretAuth{Secret: "test-secret"},
		},
		GrantType: config.GrantTypeTokenExchange,
	}
}

func TestFromConfig_ClientSecretPostResolvesCorrectly(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	if e.clientAuthMethod != ClientSecretPost {
		t.Errorf("clientAuthMethod = %v, want ClientSecretPost", e.clientAuthMethod)
	}
	if e.clientSecret != "test-secret" {
		t.Errorf("clientSecret = %q, want %q", e.clientSecret, "test-secret")
	}
	if e.tokenURL != "https://idp.example.com/token" {
		t.Errorf("tokenURL = %q, want %q", e.tokenURL, "https://idp.example.com/token")
	}
	if e.clientID != "mcp-server" {
		t.Errorf("clientID = %q, want %q", e.clientID, "mcp-server")
	}
}

func TestFromConfig_ClientSecretBasicResolvesCorrectly(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	cfg.ClientAuth = config.BrokerClientAuth{
		ClientSecretBasic: &config.ClientSecretAuth{Secret: "basic-secret"},
	}

	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	if e.clientAuthMethod != ClientSecretBasic {
		t.Errorf("clientAuthMethod = %v, want ClientSecretBasic", e.clientAuthMethod)
	}
	if e.clientSecret != "basic-secret" {
		t.Errorf("clientSecret = %q, want %q", e.clientSecret, "basic-secret")
	}
}

func TestFromConfig_NilConfigReturnsError(t *testing.T) {
	t.Parallel()

	_, err := FromConfig(nil, &http.Client{}, cachetest.Default(t))
	if err == nil {
		t.Fatal("FromConfig(nil) = nil error, want error")
		return
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error = %q, want it to mention nil", err.Error())
	}
}

func TestFromConfig_NilHTTPClientReturnsError(t *testing.T) {
	t.Parallel()

	_, err := FromConfig(validBrokerOAuthConfig(), nil, cachetest.Default(t))
	if err == nil {
		t.Fatal("FromConfig with nil HTTPClient = nil error, want error")
		return
	}
	if !strings.Contains(err.Error(), "HTTPClient") {
		t.Errorf("error = %q, want it to mention HTTPClient", err.Error())
	}
}

func TestFromConfig_NoClientAuthMethodReturnsError(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	cfg.ClientAuth = config.BrokerClientAuth{}

	_, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err == nil {
		t.Fatal("FromConfig with no client auth = nil error, want error")
		return
	}
	if !strings.Contains(err.Error(), "no client auth") {
		t.Errorf("error = %q, want it to mention no client auth method", err.Error())
	}
}

func TestFromConfig_BothClientAuthMethodsReturnsError(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	cfg.ClientAuth = config.BrokerClientAuth{
		ClientSecretBasic: &config.ClientSecretAuth{Secret: "a"},
		ClientSecretPost:  &config.ClientSecretAuth{Secret: "b"},
	}

	_, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err == nil {
		t.Fatal("FromConfig with both client auth methods = nil error, want error")
		return
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("error = %q, want it to mention both methods", err.Error())
	}
}

func TestFromConfig_UnknownGrantTypeReturnsError(t *testing.T) {
	t.Parallel()

	// A nickname, and a well-formed grant-type URN this version does not implement.
	for _, gt := range []string{"jwt-bearer", "urn:ietf:params:oauth:grant-type:saml2-bearer"} {
		t.Run(gt, func(t *testing.T) {
			t.Parallel()
			cfg := validBrokerOAuthConfig()
			cfg.GrantType = gt

			_, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
			if err == nil {
				t.Fatal("FromConfig with unknown grant type = nil error, want error")
			}
			if !strings.Contains(err.Error(), strconv.Quote(gt)) {
				t.Errorf("error = %q, want it to mention the unsupported grant type", err.Error())
			}
		})
	}
}

// TestFromConfig_JWTBearerWarnsAtStartup pins the startup signal that a
// jwt-bearer Exchanger cannot work yet, so an operator does not first learn it
// from a failed tool call. Not parallel: captureLogs swaps the process-wide
// default logger.
func TestFromConfig_JWTBearerWarnsAtStartup(t *testing.T) {
	for _, tc := range []struct {
		grantType string
		wantWarn  bool
	}{
		{config.GrantTypeJWTBearer, true},
		{config.GrantTypeTokenExchange, false},
	} {
		t.Run(tc.grantType, func(t *testing.T) {
			records, restore := captureLogs(t)
			defer restore()

			cfg := validBrokerOAuthConfig()
			cfg.GrantType = tc.grantType
			if _, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t)); err != nil {
				t.Fatalf("FromConfig: %v", err)
			}

			warned := false
			for _, r := range records() {
				if r.Level == slog.LevelWarn && strings.Contains(r.Message, "jwt-bearer grant type is not implemented") {
					warned = true
				}
			}
			if warned != tc.wantWarn {
				t.Errorf("jwt-bearer startup WARN logged = %v, want %v; records = %+v", warned, tc.wantWarn, records())
			}
		})
	}
}

func TestFromConfig_GrantTypeMapsToCorrectEnum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		yaml string
		want GrantType
	}{
		{config.GrantTypeTokenExchange, GrantTypeTokenExchange},
		{config.GrantTypeJWTBearer, GrantTypeJWTBearer},
	}
	for _, tc := range tests {
		t.Run(tc.yaml, func(t *testing.T) {
			t.Parallel()
			cfg := validBrokerOAuthConfig()
			cfg.GrantType = tc.yaml
			e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
			if err != nil {
				t.Fatalf("FromConfig: %v", err)
			}
			if e.grantType != tc.want {
				t.Errorf("grantType = %v, want %v", e.grantType, tc.want)
			}
		})
	}
}

func TestFromConfig_NowFuncIsSet(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	if e.nowFunc == nil {
		t.Error("nowFunc = nil, want time.Now (set by New)")
	}
}
