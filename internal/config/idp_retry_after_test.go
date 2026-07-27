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

// Reuses breakerYAMLPrefix/Suffix from idp_circuit_breaker_test.go — both
// sub-blocks nest under the same broker_oauth block.
func TestLoadConfig_RetryAfter_ParsesFromYAML(t *testing.T) {
	silenceLogs(t)
	yaml := breakerYAMLPrefix + `  retry_after:
    max_honored_duration: 90s
` + breakerYAMLBrokerSuffix

	cfg, err := LoadConfig(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ra := cfg.BrokerOAuth.RetryAfter
	if ra == nil {
		t.Fatal("RetryAfter is nil, want parsed block")
	}
	if ra.MaxHonoredDuration == nil || *ra.MaxHonoredDuration != 90*time.Second {
		t.Errorf("MaxHonoredDuration = %v, want 90s", ra.MaxHonoredDuration)
	}
}

func TestLoadConfig_RetryAfter_OmittedBlockIsNil(t *testing.T) {
	silenceLogs(t)
	cfg, err := LoadConfig(writeTemp(t, breakerYAMLPrefix+breakerYAMLBrokerSuffix))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BrokerOAuth.RetryAfter != nil {
		t.Errorf("RetryAfter = %+v, want nil when block omitted", cfg.BrokerOAuth.RetryAfter)
	}
}

// Same message contract as the circuit-breaker validator's equivalent test.
func TestLoadConfig_RetryAfter_BadValueRejected(t *testing.T) {
	silenceLogs(t)
	yaml := breakerYAMLPrefix + "  retry_after:\n    max_honored_duration: 2h\n" + breakerYAMLBrokerSuffix

	_, err := LoadConfig(writeTemp(t, yaml))
	if err == nil {
		t.Fatal("expected error for an over-cap max_honored_duration, got nil")
	}
	msg := err.Error()
	const wantField = "broker_oauth.retry_after.max_honored_duration"
	if !strings.Contains(msg, wantField) {
		t.Errorf("error does not name the field %q: %s", wantField, msg)
	}
	if !strings.Contains(msg, "1h0m0s") {
		t.Errorf("error does not state the valid range ceiling %q: %s", "1h0m0s", msg)
	}
	if !strings.Contains(msg, "2h0m0s") {
		t.Errorf("error does not echo the offending value %q: %s", "2h0m0s", msg)
	}
}

func TestValidateIdPRetryAfter(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		ra      *IdPRetryAfterConfig
		wantErr bool
	}{
		{"nil block valid", nil, false},
		{"empty block valid (all defaults)", &IdPRetryAfterConfig{}, false},

		{"zero invalid (indistinguishable from omitted once translated)", &IdPRetryAfterConfig{MaxHonoredDuration: ptr(time.Duration(0))}, true},
		{"negative invalid", &IdPRetryAfterConfig{MaxHonoredDuration: ptr(-time.Second)}, true},
		{"positive under cap valid", &IdPRetryAfterConfig{MaxHonoredDuration: ptr(90 * time.Second)}, false},
		{"at cap valid", &IdPRetryAfterConfig{MaxHonoredDuration: ptr(MaxIdPRetryAfterHonoredDuration)}, false},
		{"over cap invalid", &IdPRetryAfterConfig{MaxHonoredDuration: ptr(MaxIdPRetryAfterHonoredDuration + time.Second)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := validateIdPRetryAfter(tc.ra)
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("validateIdPRetryAfter() = no errors, want at least one")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("validateIdPRetryAfter() = %v, want none", errs)
			}
		})
	}
}
