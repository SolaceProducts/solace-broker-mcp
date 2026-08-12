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

package semp

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// fakeCredentialedBrokerSource is a BrokerSource that hands back a
// BrokerConfig with a URL config.LoadConfig's validateBrokerURL would never
// let through in production (embedded userinfo). It exists only so
// TestGetOrCreate_SanitizesURLInLog can prove the "broker connection
// created" log line has its own independent defense, rather than relying
// solely on that upstream validation never being bypassed or weakened.
type fakeCredentialedBrokerSource struct {
	cfg *config.BrokerConfig
}

func (f fakeCredentialedBrokerSource) Broker(alias string) (*config.BrokerConfig, bool) {
	if alias != "creds" {
		return nil, false
	}
	return f.cfg, true
}

func (f fakeCredentialedBrokerSource) BrokerAliases() []string { return []string{"creds"} }

// minimalSEMPConfig satisfies resilience.New's non-nil-pointer contract
// (Retries, RequestMinInterval) with zero-cost values — no retries, no
// throttle — since this test never sends a request, only constructs a
// client.
func minimalSEMPConfig() *config.SEMPConfig {
	retries := 0
	minInterval := time.Duration(0)
	return &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
}

// TestGetOrCreate_SanitizesURLInLog is a regression test for SOL-152979.
// getOrCreate's "broker connection created" log line must never carry a
// broker URL's embedded userinfo. In every production path,
// config.LoadConfig's validateBrokerURL already rejects such a URL before a
// BrokerPool is ever built — this test bypasses that path deliberately (via
// a fake BrokerSource) to exercise getOrCreate's own sanitizing, which must
// hold on its own rather than depending entirely on validation upstream.
func TestGetOrCreate_SanitizesURLInLog(t *testing.T) {
	var buf bytes.Buffer
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	const secret = "s3cret-password"
	cfg := &config.BrokerConfig{
		URL:  "https://admin:" + secret + "@127.0.0.1:9999",
		Auth: config.AuthConfig{Mode: config.AuthModeBasic},
	}
	pool := &BrokerPool{
		clients: make(map[string]*BrokerClient),
		src:     fakeCredentialedBrokerSource{cfg: cfg},
		sempCfg: minimalSEMPConfig(),
	}

	if _, err := pool.getOrCreate("creds"); err != nil {
		t.Fatalf("getOrCreate() error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("log output leaked the broker URL's credential:\n%s", out)
	}
	if !strings.Contains(out, "https://127.0.0.1:9999") {
		t.Fatalf("log output is missing the sanitized URL:\n%s", out)
	}
}
