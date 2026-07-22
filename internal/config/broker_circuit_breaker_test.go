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
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// silenceLogs routes slog to a discard-level handler for the duration of a
// test, so the broker_oauth-provided-but-unused WARN does not clutter output.
func silenceLogs(t *testing.T) {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(old) })
}

// breakerYAMLPrefix is a full static-mode + broker_oauth config using a
// basic-mode broker, so the V1 oauth runtime guard does not fire and the test
// focuses on parsing/validating the circuit_breaker sub-block.
const breakerYAMLPrefix = `
mcp_client_auth:
  mode: static
  dev_token: test
broker_oauth:
  idp_token_endpoint: "http://idp.example.com/token"
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: shhh
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
`

const breakerYAMLBrokerSuffix = `
brokers:
  prod:
    url: "http://broker.example.com:8080"
    auth:
      mode: basic
      username: admin
      password: shhh
`

// TestLoadConfig_CircuitBreaker_ParsesFromYAML proves the circuit_breaker
// sub-block unmarshals end-to-end through LoadConfig, including duration
// strings, onto BrokerOAuthConfig.CircuitBreaker.
func TestLoadConfig_CircuitBreaker_ParsesFromYAML(t *testing.T) {
	silenceLogs(t)
	yaml := breakerYAMLPrefix + `  circuit_breaker:
    enabled: true
    failure_rate_window: 45s
    minimum_requests: 20
    failure_rate_threshold_percent: 75
    consecutive_failure_threshold: 3
    open_state_duration: 90s
    half_open_probe_requests: 1
` + breakerYAMLBrokerSuffix

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cb := cfg.BrokerOAuth.CircuitBreaker
	if cb == nil {
		t.Fatal("CircuitBreaker is nil, want parsed block")
	}
	if cb.Enabled == nil || !*cb.Enabled {
		t.Errorf("Enabled = %v, want true", cb.Enabled)
	}
	if cb.FailureRateWindow == nil || *cb.FailureRateWindow != 45*time.Second {
		t.Errorf("FailureRateWindow = %v, want 45s", cb.FailureRateWindow)
	}
	if cb.MinimumRequests == nil || *cb.MinimumRequests != 20 {
		t.Errorf("MinimumRequests = %v, want 20", cb.MinimumRequests)
	}
	if cb.OpenStateDuration == nil || *cb.OpenStateDuration != 90*time.Second {
		t.Errorf("OpenStateDuration = %v, want 90s", cb.OpenStateDuration)
	}
	if cb.HalfOpenProbeRequests == nil || *cb.HalfOpenProbeRequests != 1 {
		t.Errorf("HalfOpenProbeRequests = %v, want 1", cb.HalfOpenProbeRequests)
	}
}

// TestLoadConfig_CircuitBreaker_OmittedBlockIsNil confirms backwards
// compatibility: a broker_oauth block with no circuit_breaker leaves the field
// nil (the runtime then applies defaults).
func TestLoadConfig_CircuitBreaker_OmittedBlockIsNil(t *testing.T) {
	silenceLogs(t)
	cfg, err := LoadConfig(writeTemp(t, breakerYAMLPrefix+breakerYAMLBrokerSuffix))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BrokerOAuth.CircuitBreaker != nil {
		t.Errorf("CircuitBreaker = %+v, want nil when block omitted", cfg.BrokerOAuth.CircuitBreaker)
	}
}

// TestLoadConfig_CircuitBreaker_BadValueRejected proves an out-of-bounds
// breaker value fails at config load, not at Exchanger construction.
func TestLoadConfig_CircuitBreaker_BadValueRejected(t *testing.T) {
	silenceLogs(t)
	yaml := breakerYAMLPrefix + `  circuit_breaker:
    failure_rate_threshold_percent: 150
` + breakerYAMLBrokerSuffix

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for out-of-bounds failure_rate_threshold_percent, got nil")
	}
	if !strings.Contains(err.Error(), "failure_rate_threshold_percent") {
		t.Errorf("error %q does not mention the offending field", err.Error())
	}
}

// TestBreakerEnabled covers the presence semantics: the breaker is on unless an
// operator explicitly sets enabled: false. Both "no block" and "block with no
// enabled field" default to on.
func TestBreakerEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  *BrokerOAuthConfig
		want bool
	}{
		{"nil oauth block", nil, true},
		{"no circuit_breaker block", &BrokerOAuthConfig{}, true},
		{"block present, enabled omitted", &BrokerOAuthConfig{CircuitBreaker: &BrokerCircuitBreakerConfig{}}, true},
		{"enabled true", &BrokerOAuthConfig{CircuitBreaker: &BrokerCircuitBreakerConfig{Enabled: ptr(true)}}, true},
		{"enabled false", &BrokerOAuthConfig{CircuitBreaker: &BrokerCircuitBreakerConfig{Enabled: ptr(false)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cfg.BreakerEnabled(); got != tc.want {
				t.Errorf("BreakerEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateBrokerCircuitBreaker checks that only set fields are validated
// (an omitted field takes the default and is always valid), and that the bounds
// match the runtime validator.
func TestValidateBrokerCircuitBreaker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cb      *BrokerCircuitBreakerConfig
		wantErr bool
	}{
		{"nil block valid", nil, false},
		{"empty block valid (all defaults)", &BrokerCircuitBreakerConfig{}, false},
		{"enabled false alone valid", &BrokerCircuitBreakerConfig{Enabled: ptr(false)}, false},

		{"window at floor valid", &BrokerCircuitBreakerConfig{FailureRateWindow: ptr(10 * time.Millisecond)}, false},
		{"window below floor", &BrokerCircuitBreakerConfig{FailureRateWindow: ptr(time.Millisecond)}, true},
		{"window zero", &BrokerCircuitBreakerConfig{FailureRateWindow: ptr(time.Duration(0))}, true},

		{"minimum requests zero", &BrokerCircuitBreakerConfig{MinimumRequests: ptr(uint32(0))}, true},
		{"minimum requests one valid", &BrokerCircuitBreakerConfig{MinimumRequests: ptr(uint32(1))}, false},

		{"threshold zero", &BrokerCircuitBreakerConfig{FailureRateThresholdPercent: ptr(0.0)}, true},
		{"threshold over 100", &BrokerCircuitBreakerConfig{FailureRateThresholdPercent: ptr(100.5)}, true},
		{"threshold exactly 100 valid", &BrokerCircuitBreakerConfig{FailureRateThresholdPercent: ptr(100.0)}, false},

		{"consecutive zero valid (disables rule)", &BrokerCircuitBreakerConfig{ConsecutiveFailureThreshold: ptr(uint32(0))}, false},

		{"open duration zero", &BrokerCircuitBreakerConfig{OpenStateDuration: ptr(time.Duration(0))}, true},
		{"open duration positive valid", &BrokerCircuitBreakerConfig{OpenStateDuration: ptr(time.Second)}, false},

		{"probes zero", &BrokerCircuitBreakerConfig{HalfOpenProbeRequests: ptr(uint32(0))}, true},
		{"probes at cap valid", &BrokerCircuitBreakerConfig{HalfOpenProbeRequests: ptr(maxBrokerHalfOpenProbeRequests)}, false},
		{"probes over cap", &BrokerCircuitBreakerConfig{HalfOpenProbeRequests: ptr(maxBrokerHalfOpenProbeRequests + 1)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := validateBrokerCircuitBreaker(tc.cb)
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("validateBrokerCircuitBreaker() = no errors, want at least one")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("validateBrokerCircuitBreaker() = %v, want none", errs)
			}
		})
	}
}
