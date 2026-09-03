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

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

const postureRequestExtraEnabled = "request extra middleware is enabled"

// TestInstallRequestMiddleware_LogsRequestExtraEnabled pins that production
// wiring always installs the Extra.Header → handler ctx middleware. The SDK
// keeps the middleware chain unexported, so the startup line is the observable
// contract (same approach as tools/list filtering).
//
// It calls installRequestMiddleware, the one place registration order is
// expressed, rather than registering the middleware itself — so dropping the
// registration from production wiring fails here. Config is the auth-disabled
// default, in which the filter registers nothing: this asserts the Extra.Header
// middleware is unconditional.
func TestInstallRequestMiddleware_LogsRequestExtraEnabled(t *testing.T) {
	buf, restore := captureStartupLog(t)
	defer restore()
	installRequestMiddleware(newTestServer(), &config.ServerConfig{}, nil, "")

	var found int
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if msg, _ := entry["msg"].(string); msg == postureRequestExtraEnabled {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("found %d %q lines, want 1:\n%s", found, postureRequestExtraEnabled, buf.String())
	}
}
