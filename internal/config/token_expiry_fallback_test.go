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
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_TokenExpiryFallback_ParsesFromYAML(t *testing.T) {
	silenceLogs(t)
	yaml := breakerYAMLPrefix + "  token_expiry_fallback: 1h\n" + breakerYAMLBrokerSuffix

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BrokerOAuth.TokenExpiryFallback == nil {
		t.Fatal("TokenExpiryFallback is nil, want parsed duration")
	}
	if got := *cfg.BrokerOAuth.TokenExpiryFallback; got != time.Hour {
		t.Errorf("TokenExpiryFallback = %v, want 1h", got)
	}
}

func TestLoadConfig_TokenExpiryFallback_OmittedIsNil(t *testing.T) {
	silenceLogs(t)

	cfg, err := LoadConfig(writeTemp(t, breakerYAMLPrefix+breakerYAMLBrokerSuffix))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BrokerOAuth.TokenExpiryFallback != nil {
		t.Errorf("TokenExpiryFallback = %v, want nil when omitted", cfg.BrokerOAuth.TokenExpiryFallback)
	}
}

func TestLoadConfig_TokenExpiryFallback_NonPositiveRejected(t *testing.T) {
	silenceLogs(t)
	for _, value := range []string{"0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			yaml := breakerYAMLPrefix + "  token_expiry_fallback: " + value + "\n" + breakerYAMLBrokerSuffix

			_, err := LoadConfig(writeTemp(t, yaml))
			if err == nil {
				t.Fatalf("expected error for token_expiry_fallback %s, got nil", value)
			}
			if !strings.Contains(err.Error(), "broker_oauth.token_expiry_fallback must be positive") {
				t.Errorf("error does not identify token_expiry_fallback: %v", err)
			}
		})
	}
}

func TestValidateTokenExpiryFallback(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		fallback *time.Duration
		wantErr  bool
	}{
		{name: "omitted", fallback: nil},
		{name: "positive", fallback: ptr(time.Hour)},
		{name: "short positive", fallback: ptr(time.Nanosecond)},
		{name: "zero", fallback: ptr(time.Duration(0)), wantErr: true},
		{name: "negative", fallback: ptr(-time.Second), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := validateTokenExpiryFallback(tc.fallback)
			if tc.wantErr && len(errs) == 0 {
				t.Error("validateTokenExpiryFallback() = no errors, want at least one")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("validateTokenExpiryFallback() = %v, want none", errs)
			}
		})
	}
}
