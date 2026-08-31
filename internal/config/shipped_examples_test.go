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
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The configs this package ships for operators to copy — the root example and
// the Kubernetes ConfigMap — are only useful if the server will actually start
// on them. Neither is exercised anywhere else, so a new required field added to
// validate() silently breaks both: `kubectl apply -f deploy/kubernetes/` yields
// a crash-looping pod, and a user copying the example gets the same failure at
// startup. These tests run the shipped bytes through the very same LoadConfig
// the server calls, so that drift fails here instead of in the user's cluster.
//
// Scope: these tests cover the ACTIVE configuration in each file. The
// commented-out sample blocks (oauth, static) are not exercised — env
// substitution skips comments — so a typo inside one still ships silently.
// The DEV_TOKEN the ConfigMap references is supplied here; the Secret ships it
// empty on purpose, so an unedited deploy fails closed rather than running on a
// credential published in this repository.

// prepareExampleEnv points the loader at an empty .env and sets the ${VAR}
// references the shipped configs use. Pinning ENV_FILE matters: without it
// LoadConfig picks up a developer's repo-root .env, so the test would pass or
// fail based on untracked local state that CI does not have.
func prepareExampleEnv(t *testing.T) {
	t.Helper()

	emptyEnv := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(emptyEnv, nil, 0o600); err != nil {
		t.Fatalf("write empty env file: %v", err)
	}
	t.Setenv("ENV_FILE", emptyEnv)

	for k, v := range map[string]string{
		"BROKER_USERNAME": "example-user",
		"BROKER_PASSWORD": "example-password",
		"DEV_TOKEN":       "example-dev-token",
	} {
		t.Setenv(k, v)
	}
}

// repoPath resolves a path relative to the repository root.
func repoPath(rel string) string {
	return filepath.Join("..", "..", rel)
}

// TestShippedExampleConfigLoads guards the root broker-config.example.yaml that
// the README and docs tell operators to copy.
func TestShippedExampleConfigLoads(t *testing.T) {
	prepareExampleEnv(t)

	cfg, err := LoadConfig(repoPath("broker-config.example.yaml"))
	if err != nil {
		t.Fatalf("broker-config.example.yaml must load with the server's own validator, "+
			"otherwise an operator copying it gets a server that refuses to start: %v", err)
	}
	assertStartsWithoutIdentityProvider(t, cfg, "broker-config.example.yaml")
	assertNeverUnauthenticatedOnTheNetwork(t, cfg, "broker-config.example.yaml")
}

// assertStartsWithoutIdentityProvider guards the property LoadConfig cannot:
// a shipped config that is valid may still fail at startup. Under
// mcp_client_auth.mode: oauth the server builds an auth middleware that
// contacts the issuer's OIDC discovery endpoint, so a shipped default of oauth
// with a placeholder issuer loads cleanly here and then crash-loops in the
// operator's cluster — which is exactly what these files used to do. The
// shipped defaults must therefore stay in a dev mode.
func assertStartsWithoutIdentityProvider(t *testing.T, cfg *ServerConfig, name string) {
	t.Helper()

	if cfg.IsProductionMode() {
		t.Fatalf("%s ships mcp_client_auth.mode: oauth, so starting it requires a reachable "+
			"identity provider; the shipped default must run without one. Keep oauth as "+
			"commented-out sample lines instead.", name)
	}
}

// assertNeverUnauthenticatedOnTheNetwork guards the risk specific to the
// standalone example, which ships mode: disabled. That mode is safe there for
// exactly one reason: the dev modes bind 127.0.0.1, so nothing off the host can
// reach a server that asks callers for nothing. allow_remote_unauthenticated is
// the single opt-in that removes that protection, and setting it would put
// broker-admin-backed tools on the network with no client authentication at
// all. It is legal, it loads, and no other assertion here would catch it.
func assertNeverUnauthenticatedOnTheNetwork(t *testing.T, cfg *ServerConfig, name string) {
	t.Helper()

	if cfg.AllowRemoteUnauthenticated {
		t.Fatalf("%s sets allow_remote_unauthenticated: true, which lifts the loopback-only "+
			"bind that is the sole reason mode: disabled is safe here. The shipped example "+
			"must never acknowledge that risk on an operator's behalf.", name)
	}
}

// assertShipsASharedToken pins what the Kubernetes ConfigMap must be. A pod has
// to bind all interfaces for the Service and the kubelet probes to reach it, so
// it cannot fall back on the loopback bind that makes mode: disabled safe for a
// local binary — and mode: oauth cannot start without a reachable identity
// provider. Of the three modes that leaves exactly one, so pin it directly
// rather than ruling the other two out separately. With the mode pinned,
// allow_remote_unauthenticated cannot take effect and needs no assertion of its
// own: it only applies under mode: disabled, which this check already refuses.
func assertShipsASharedToken(t *testing.T, cfg *ServerConfig, name string) {
	t.Helper()

	if cfg.MCPClientAuth.Mode != AuthModeStatic {
		t.Fatalf("%s ships mcp_client_auth.mode: %q; the Kubernetes default must be %q. "+
			"A pod binds a routable address, so it cannot ship unauthenticated, and oauth "+
			"would require a reachable identity provider to start. Keep oauth as "+
			"commented-out sample lines instead.", name, cfg.MCPClientAuth.Mode, AuthModeStatic)
	}
}

// TestShippedKubernetesConfigMapLoads guards the config.yaml embedded in
// deploy/kubernetes/configmap.yaml.
func TestShippedKubernetesConfigMapLoads(t *testing.T) {
	prepareExampleEnv(t)

	raw, err := os.ReadFile(repoPath("deploy/kubernetes/configmap.yaml"))
	if err != nil {
		t.Fatalf("read Kubernetes ConfigMap: %v", err)
	}

	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatalf("parse Kubernetes ConfigMap: %v", err)
	}
	embedded, ok := cm.Data["config.yaml"]
	if !ok {
		t.Fatal(`deploy/kubernetes/configmap.yaml has no data["config.yaml"] key; ` +
			"the Deployment mounts that key as the server's config file")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(embedded), 0o600); err != nil {
		t.Fatalf("write embedded config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("the config.yaml embedded in deploy/kubernetes/configmap.yaml must load with "+
			"the server's own validator, otherwise `kubectl apply -f deploy/kubernetes/` "+
			"produces a crash-looping pod: %v", err)
	}
	assertShipsASharedToken(t, cfg, "deploy/kubernetes/configmap.yaml")
}
