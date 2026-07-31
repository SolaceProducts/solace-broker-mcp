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
		GrantType:     config.GrantTypeTokenExchange,
		AudienceParam: config.AudienceParamAudience,
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

	cfg := validBrokerOAuthConfig()
	cfg.GrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	_, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err == nil {
		t.Fatal("FromConfig with unknown grant type = nil error, want error")
		return
	}
	if !strings.Contains(err.Error(), "jwt-bearer") {
		t.Errorf("error = %q, want it to mention the unsupported grant type", err.Error())
	}
}

// TestFromConfig_UnknownAudienceParamReturnsError covers every value other
// than "audience", including "scope" and "resource" — internal/config.
// validAudienceParams rejects all three at config load, before FromConfig is
// ever reached in production, so at this layer they are equally unsupported
// regardless of whether the string names a real (but unimplemented) OAuth
// style or a typo.
func TestFromConfig_UnknownAudienceParamReturnsError(t *testing.T) {
	t.Parallel()

	for _, param := range []string{"custom_param", "scope", "resource"} {
		t.Run(param, func(t *testing.T) {
			t.Parallel()

			cfg := validBrokerOAuthConfig()
			cfg.AudienceParam = param

			_, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
			if err == nil {
				t.Fatal("FromConfig with unknown audience param = nil error, want error")
			}
			if !strings.Contains(err.Error(), param) {
				t.Errorf("error = %q, want it to mention the unsupported param name", err.Error())
			}
		})
	}
}

func TestFromConfig_GrantTypeAndAudienceParamMapToCorrectEnums(t *testing.T) {
	t.Parallel()

	cfg := validBrokerOAuthConfig()
	e, err := FromConfig(cfg, &http.Client{}, cachetest.Default(t))
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	if e.grantType != GrantTypeTokenExchange {
		t.Errorf("grantType = %v, want GrantTypeTokenExchange", e.grantType)
	}
	if e.audienceParam != AudienceParamAudience {
		t.Errorf("audienceParam = %v, want AudienceParamAudience", e.audienceParam)
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
