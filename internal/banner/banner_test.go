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

package banner

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog replaces slog.Default with a TextHandler writing to buf.
// Returns a restore func; defer it.
func captureSlog(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	return buf, func() { slog.SetDefault(prev) }
}

func Test_StartupAuthMode_Disabled(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	LogStartupAuthMode("disabled", "", "127.0.0.1:9090")
	out := buf.String()
	for _, want := range []string{
		"level=WARN",
		"INSECURE MODE",
		"mcp_client_auth.mode = disabled",
		"Client → MCP server auth is OFF",
		"Broker auth is unaffected",
		"NOT FOR PRODUCTION USE",
		"listen_address=127.0.0.1:9090",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func Test_StartupAuthMode_Static(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	LogStartupAuthMode("static", "", "127.0.0.1:9090")
	out := buf.String()
	for _, want := range []string{
		"level=WARN",
		"INSECURE MODE",
		"mcp_client_auth.mode = static",
		"Client → MCP server auth uses a static dev token",
		"Broker auth is unaffected",
		"NOT FOR PRODUCTION USE",
		"listen_address=127.0.0.1:9090",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func Test_StartupAuthMode_OAuth(t *testing.T) {
	buf, restore := captureSlog(t)
	defer restore()
	LogStartupAuthMode("oauth", "https://idp.example.com", ":9090")
	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("expected INFO log for oauth mode, got: %s", out)
	}
	if strings.Contains(out, "INSECURE MODE") {
		t.Errorf("oauth mode should not emit insecure banner, got: %s", out)
	}
	if !strings.Contains(out, "https://idp.example.com") {
		t.Errorf("oauth log should name the issuer, got: %s", out)
	}
	if !strings.Contains(out, "listen_address=:9090") {
		t.Errorf("startup should log the effective bind address, got: %s", out)
	}
}
