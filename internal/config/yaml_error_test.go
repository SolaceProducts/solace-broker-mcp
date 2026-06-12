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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// yaml.v3 type errors embed the offending node value in the message
// (e.g. "cannot unmarshal !!str `hunter2...` into int"). The decode error is
// wrapped and logged at startup, so a credential that lands in a non-string
// field — most plausibly via a wrong ${VAR} reference, since env substitution
// runs before decode — would flow into the error log field, a Never-Log List
// item that the slog ReplaceAttr net cannot catch because the secret rides
// inside the error string value. (Note: a plain numeric scalar into a string
// field is silently coerced by yaml.v3, not echoed — the type error needs a
// genuinely mismatched target type.) Decode errors must be scrubbed before
// wrapping.

func TestLoadConfig_YAMLTypeError_DoesNotEchoCredentialValue(t *testing.T) {
	// A secret value in a non-string field: what an operator gets by
	// referencing the wrong env var (e.g. port: ${BROKER_PASSWORD}), which
	// substituteEnvVars expands before the YAML decode runs.
	cfg := `port: hunter2secret
client_auth:
  mode: disabled
brokers:
  prod:
    url: https://broker.example.com:1943
    auth:
      mode: basic
      username: admin
      password: ok
`
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected YAML type error for string value in int field")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("decode error echoes the credential value: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "parsing config YAML") {
		t.Errorf("error %q lost its parse-failure context", err.Error())
	}
	// Operators still need to find the broken line.
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q lost line-number diagnostics", err.Error())
	}
}

func TestSanitizeYAMLError_StripsBacktickedValues(t *testing.T) {
	in := errors.New("yaml: unmarshal errors:\n  line 9: cannot unmarshal !!int `1234567` into string\n  line 12: cannot unmarshal !!bool `true` into string")
	got := sanitizeYAMLError(in).Error()

	if strings.Contains(got, "1234567") || strings.Contains(got, "`true`") {
		t.Errorf("sanitized error still contains node values: %q", got)
	}
	for _, keep := range []string{"line 9", "line 12", "!!int", "into string"} {
		if !strings.Contains(got, keep) {
			t.Errorf("sanitized error %q lost diagnostic detail %q", got, keep)
		}
	}
}
