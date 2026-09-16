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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
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

// TestShippedGOMEMLIMITTracksMemoryLimit guards the coupling between
// GOMEMLIMIT and limits.memory in deploy/kubernetes/deployment.yaml
// (SOL-154328, measured under SOL-154158). Two ways that pairing rots, both of
// which reach the operator as a broken pod rather than a review comment:
//
//   - Someone raises limits.memory and leaves GOMEMLIMIT behind. The manifest
//     comment says to move them together, and a comment is not a guard. The
//     pod still starts, so nothing fails — it just quietly gives back the
//     headroom the limit was raised to buy.
//   - Someone writes the Kubernetes byte suffix. Go's parser accepts MiB and
//     not Mi, and rejects a malformed GOMEMLIMIT by calling throw() during
//     schedinit, so the process dies before main with no config error and no
//     fallback: the pod CrashLoopBackOffs on a one-character edit.
//
// The ratio is a recommendation, so this asserts the shipped values match it
// rather than that any particular ratio is correct. Changing the shipped ratio
// deliberately means changing it here too.
func TestShippedGOMEMLIMITTracksMemoryLimit(t *testing.T) {
	raw, err := os.ReadFile(repoPath("deploy/kubernetes/deployment.yaml"))
	if err != nil {
		t.Fatalf("read Kubernetes Deployment: %v", err)
	}

	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"env"`
						Resources struct {
							Limits map[string]string `yaml:"limits"`
						} `yaml:"resources"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &dep); err != nil {
		t.Fatalf("parse Kubernetes Deployment: %v", err)
	}
	containers := dep.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("deploy/kubernetes/deployment.yaml has %d containers, want 1; "+
			"this test reads the first one's env and resources", len(containers))
	}
	c := containers[0]

	var gomemlimit string
	found := false
	for _, e := range c.Env {
		if e.Name == "GOMEMLIMIT" {
			gomemlimit, found = e.Value, true
			break
		}
	}
	if !found {
		t.Fatal("deploy/kubernetes/deployment.yaml no longer sets GOMEMLIMIT. The Go runtime " +
			"never reads a cgroup memory limit, so without it the heap target has no ceiling " +
			"and the pod settles near limits.memory instead of below it (SOL-154158)")
	}

	// Go accepts B, KiB, MiB, GiB, TiB (runtime/extern.go). Kubernetes' own
	// Mi/Gi are NOT among them, and are the likely mistake here.
	limitBytes, err := parseGoByteCount(gomemlimit)
	if err != nil {
		t.Fatalf("GOMEMLIMIT=%q in deploy/kubernetes/deployment.yaml is not a byte count the "+
			"Go runtime accepts (%v). Suffixes are B, KiB, MiB, GiB, TiB — note MiB, not "+
			"Kubernetes' Mi. The runtime throws on a malformed value during startup, so this "+
			"ships a pod that crash-loops before main with no config error", gomemlimit, err)
	}

	memLimit, ok := c.Resources.Limits["memory"]
	if !ok {
		t.Fatal("deploy/kubernetes/deployment.yaml sets no resources.limits.memory; " +
			"GOMEMLIMIT is expressed as a fraction of it")
	}
	capBytes, err := parseK8sByteCount(memLimit)
	if err != nil {
		t.Fatalf("resources.limits.memory=%q: %v", memLimit, err)
	}

	// The shipped recommendation: GOMEMLIMIT at 75% of limits.memory.
	const wantRatio = 0.75
	want := int64(float64(capBytes) * wantRatio)
	if limitBytes != want {
		t.Fatalf("GOMEMLIMIT=%q (%d bytes) is %.1f%% of limits.memory=%q (%d bytes); the "+
			"shipped recommendation is %.0f%% (%d bytes). Raise limits.memory and GOMEMLIMIT "+
			"together and keep the ratio — see docs/observability.md § \"Resource requests "+
			"and limits\" (SOL-154328)",
			gomemlimit, limitBytes, 100*float64(limitBytes)/float64(capBytes),
			memLimit, capBytes, 100*wantRatio, want)
	}
}

// The shipped label every manifest in deploy/kubernetes/ selects on. The Service
// and the NetworkPolicy select pods by it; the ServiceMonitor selects the
// Service by it.
const shippedSelectorLabel = "app.kubernetes.io/name"

// shippedMonitoringNamespace is the namespace networkpolicy.yaml admits
// /metrics scrapes from, matched on the kubernetes.io/metadata.name label that
// Kubernetes sets on every namespace automatically.
const shippedMonitoringNamespace = "monitoring"

// k8sSelector is the podSelector/namespaceSelector shape these manifests use.
type k8sSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

// npPeer is one entry of a NetworkPolicy ingress rule's `from` list. All three
// peer kinds are decoded, not just the one we ship, so a rule that swaps a
// namespaceSelector for a wide-open ipBlock is visible to the assertions below
// rather than silently indistinguishable from it.
type npPeer struct {
	NamespaceSelector *k8sSelector `yaml:"namespaceSelector"`
	PodSelector       *k8sSelector `yaml:"podSelector"`
	IPBlock           *struct {
		CIDR string `yaml:"cidr"`
	} `yaml:"ipBlock"`
}

type shippedDeployment struct {
	Spec struct {
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []struct {
					Ports []struct {
						Name          string `yaml:"name"`
						ContainerPort int    `yaml:"containerPort"`
					} `yaml:"ports"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type shippedService struct {
	Metadata struct {
		Labels map[string]string `yaml:"labels"`
	} `yaml:"metadata"`
	Spec struct {
		Ports []struct {
			Name       string `yaml:"name"`
			Port       int    `yaml:"port"`
			TargetPort string `yaml:"targetPort"`
		} `yaml:"ports"`
		Selector map[string]string `yaml:"selector"`
	} `yaml:"spec"`
}

type shippedNetworkPolicy struct {
	Spec struct {
		PodSelector k8sSelector `yaml:"podSelector"`
		PolicyTypes []string    `yaml:"policyTypes"`
		Egress      []any       `yaml:"egress"`
		Ingress     []struct {
			From  []npPeer `yaml:"from"`
			Ports []struct {
				Port int `yaml:"port"`
			} `yaml:"ports"`
		} `yaml:"ingress"`
	} `yaml:"spec"`
}

type shippedServiceMonitor struct {
	Spec struct {
		Selector  k8sSelector `yaml:"selector"`
		Endpoints []struct {
			Port          string `yaml:"port"`
			Path          string `yaml:"path"`
			Interval      string `yaml:"interval"`
			ScrapeTimeout string `yaml:"scrapeTimeout"`
		} `yaml:"endpoints"`
	} `yaml:"spec"`
}

// containerPorts returns the Deployment's single container's named ports,
// failing the test if the manifest stops having exactly one container (these
// tests read the first one).
func (d shippedDeployment) containerPorts(t *testing.T) map[string]int {
	t.Helper()
	containers := d.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("deploy/kubernetes/deployment.yaml has %d containers, want 1; "+
			"these tests read the first one's ports", len(containers))
	}
	ports := map[string]int{}
	for _, p := range containers[0].Ports {
		ports[p.Name] = p.ContainerPort
	}
	return ports
}

// loadShippedManifests parses the four manifests these tests reason about
// together. servicemonitor.yaml.example is included even though the
// directory-wide `kubectl apply` skips it: an operator applies it by hand, so
// it has to agree with the Service it scrapes.
func loadShippedManifests(t *testing.T) (shippedDeployment, shippedService, shippedNetworkPolicy, shippedServiceMonitor) {
	t.Helper()
	var dep shippedDeployment
	var svc shippedService
	var np shippedNetworkPolicy
	var sm shippedServiceMonitor
	readManifest(t, "deploy/kubernetes/deployment.yaml", &dep)
	readManifest(t, "deploy/kubernetes/service.yaml", &svc)
	readManifest(t, "deploy/kubernetes/networkpolicy.yaml", &np)
	readManifest(t, "deploy/kubernetes/servicemonitor.yaml.example", &sm)
	return dep, svc, np, sm
}

// TestShippedPortsTrackCompiledDefaults guards the two ports deploy/kubernetes/
// spells in four places — the embedded ConfigMap's `port`, the Deployment's
// named containerPorts, the Service's ports, and the NetworkPolicy's allow
// rules — against the server's own compiled defaults (SOL-152424). Only
// manifest comments say they must agree, and a comment is not a guard.
//
// Both directions of drift reach an operator as silence rather than an error,
// but they are not symmetric, which is why the policy is checked against the
// compiled default and not merely against the other manifests:
//
//   - A stale containerPort is caught loudly by Kubernetes itself, because the
//     probes target `port: http` — the pod crash-loops or never goes Ready.
//   - A stale port in networkpolicy.yaml has no such safety net. Kubelet probes
//     reach the pod on the node path, which the CNI does not police, so the pod
//     reports Ready while real client traffic to the true port is denied. For
//     :9091 that is a scrape that stops; for :9090 it is the MCP endpoint
//     itself, with a larger blast radius.
func TestShippedPortsTrackCompiledDefaults(t *testing.T) {
	_, portStr, err := net.SplitHostPort(defaults.DefaultMetricsBindAddress)
	if err != nil {
		t.Fatalf("defaults.DefaultMetricsBindAddress=%q is not host:port: %v",
			defaults.DefaultMetricsBindAddress, err)
	}
	wantMetrics, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("defaults.DefaultMetricsBindAddress=%q has a non-numeric port: %v",
			defaults.DefaultMetricsBindAddress, err)
	}
	wantHTTP := defaults.DefaultPort

	dep, svc, np, _ := loadShippedManifests(t)

	// The ConfigMap's server port. deploy/kubernetes/README.md documents editing
	// this as the supported way to move the MCP port, so it is the value the
	// other three files have to follow.
	var cm struct {
		Data struct {
			ConfigYAML string `yaml:"config.yaml"`
		} `yaml:"data"`
	}
	readManifest(t, "deploy/kubernetes/configmap.yaml", &cm)
	var embedded struct {
		Port int `yaml:"port"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data.ConfigYAML), &embedded); err != nil {
		t.Fatalf("parse the config.yaml embedded in deploy/kubernetes/configmap.yaml: %v", err)
	}
	if embedded.Port != wantHTTP {
		t.Errorf("deploy/kubernetes/configmap.yaml sets port: %d; the compiled default is %d. "+
			"The shipped example is meant to run on the defaults — if the default moved, move "+
			"this and the http port in deployment.yaml, service.yaml, and networkpolicy.yaml with it",
			embedded.Port, wantHTTP)
	}

	ports := dep.containerPorts(t)
	for _, tc := range []struct {
		name string
		want int
		hint string
	}{
		{"http", wantHTTP, "defaults.DefaultPort"},
		{"metrics", wantMetrics, "defaults.DefaultMetricsBindAddress"},
	} {
		got, ok := ports[tc.name]
		if !ok {
			t.Errorf("deploy/kubernetes/deployment.yaml declares no containerPort named %q; "+
				"service.yaml targets it by that name", tc.name)
			continue
		}
		if got != tc.want {
			t.Errorf("deploy/kubernetes/deployment.yaml %q containerPort is %d, want %d (%s)",
				tc.name, got, tc.want, tc.hint)
		}
	}

	svcPorts := map[string]int{}
	for _, p := range svc.Spec.Ports {
		svcPorts[p.Name] = p.Port
		// targetPort names the containerPort rather than repeating the number,
		// so the Service and Deployment cannot drift apart independently.
		if p.TargetPort != p.Name {
			t.Errorf("deploy/kubernetes/service.yaml port %q has targetPort %q, want the "+
				"identically named containerPort so the two cannot drift apart", p.Name, p.TargetPort)
		}
	}
	for name, want := range map[string]int{"http": wantHTTP, "metrics": wantMetrics} {
		got, ok := svcPorts[name]
		if !ok {
			t.Errorf("deploy/kubernetes/service.yaml exposes no port named %q", name)
			continue
		}
		if got != want {
			t.Errorf("deploy/kubernetes/service.yaml %q port is %d, want %d", name, got, want)
		}
	}

	policyPorts := map[int]bool{}
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			policyPorts[p.Port] = true
		}
	}
	for _, want := range []int{wantHTTP, wantMetrics} {
		if !policyPorts[want] {
			t.Errorf("deploy/kubernetes/networkpolicy.yaml has no ingress rule for port %d. With "+
				"the pod isolated for ingress, a port the policy does not list is denied, not "+
				"merely unprotected, and the kubelet probes do not reveal it", want)
		}
	}
}

// TestShippedNetworkPolicyRestrictsMetricsIngress pins the security invariant
// networkpolicy.yaml exists for, which the port test above cannot see: that
// :9091 is admitted from the monitoring namespace and nowhere else, while
// :9090 stays reachable and egress stays untouched.
//
// Asserting the selector's contents rather than merely that a `from` list
// exists is the point. A typo'd label value, a different label key, or a
// `from: [{ipBlock: {cidr: 0.0.0.0/0}}]` all leave a non-empty `from` while
// defeating the policy's whole purpose.
func TestShippedNetworkPolicyRestrictsMetricsIngress(t *testing.T) {
	_, portStr, err := net.SplitHostPort(defaults.DefaultMetricsBindAddress)
	if err != nil {
		t.Fatalf("defaults.DefaultMetricsBindAddress=%q is not host:port: %v",
			defaults.DefaultMetricsBindAddress, err)
	}
	metricsPort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("defaults.DefaultMetricsBindAddress=%q has a non-numeric port: %v",
			defaults.DefaultMetricsBindAddress, err)
	}

	dep, _, np, _ := loadShippedManifests(t)

	// Ingress-only. An egress allow-list default-denies every destination it
	// omits (brokers, IdP, DNS), and because policies are additive it could not
	// override a customer's own default-deny anyway — so the OTLP egress rule
	// stays a commented template. See docs/observability.md § "Scraping and
	// securing the metrics endpoint".
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Ingress" || len(np.Spec.Egress) != 0 {
		t.Errorf("deploy/kubernetes/networkpolicy.yaml policyTypes=%v with %d egress rules; the "+
			"shipped policy is ingress-only. Adding an egress section default-denies brokers, "+
			"IdP, and DNS — keep the OTLP rule a commented template",
			np.Spec.PolicyTypes, len(np.Spec.Egress))
	}

	// The policy must select the pods it is meant to protect. A selector that
	// matches nothing fails the isolation *open*, with nothing else to catch it.
	podLabels := dep.Spec.Template.Metadata.Labels
	for k, v := range np.Spec.PodSelector.MatchLabels {
		if podLabels[k] != v {
			t.Errorf("deploy/kubernetes/networkpolicy.yaml podSelector wants %s=%q but "+
				"deployment.yaml's pod template has %s=%q; a policy that selects no pods "+
				"enforces nothing and fails open", k, v, k, podLabels[k])
		}
	}
	if len(np.Spec.PodSelector.MatchLabels) == 0 {
		t.Error("deploy/kubernetes/networkpolicy.yaml has an empty podSelector, which selects " +
			"every pod in the namespace rather than this server's")
	}

	httpPort := dep.containerPorts(t)["http"]
	var sawHTTP, sawMetrics bool
	for _, rule := range np.Spec.Ingress {
		for _, p := range rule.Ports {
			switch p.Port {
			case httpPort:
				sawHTTP = true
				// Deliberately reachable from anywhere: the kubelet's probes
				// arrive from the node's own address, which no selector can name.
				if len(rule.From) != 0 {
					t.Errorf("deploy/kubernetes/networkpolicy.yaml restricts port %d to %d peer(s); "+
						"it ships open to all sources because the kubelet probes cannot be named "+
						"by any selector. Narrowing it is an operator's choice, not the shipped default",
						httpPort, len(rule.From))
				}
			case metricsPort:
				sawMetrics = true
				assertMetricsPeersRestricted(t, metricsPort, rule.From)
			}
		}
	}
	if !sawHTTP {
		t.Errorf("deploy/kubernetes/networkpolicy.yaml has no ingress rule for the http port %d", httpPort)
	}
	if !sawMetrics {
		t.Errorf("deploy/kubernetes/networkpolicy.yaml has no ingress rule for the metrics port %d", metricsPort)
	}
}

// assertMetricsPeersRestricted checks that the metrics-port rule admits exactly
// the monitoring namespace: at least one peer, every peer a namespaceSelector
// naming that namespace, and no ipBlock peer widening it.
func assertMetricsPeersRestricted(t *testing.T, port int, peers []npPeer) {
	t.Helper()
	if len(peers) == 0 {
		t.Errorf("deploy/kubernetes/networkpolicy.yaml admits port %d with no `from`, i.e. from "+
			"every source; restricting it to the monitoring namespace is the policy's whole purpose", port)
		return
	}
	for i, peer := range peers {
		if peer.IPBlock != nil {
			t.Errorf("deploy/kubernetes/networkpolicy.yaml from[%d] on port %d is an ipBlock (%s); "+
				"the shipped policy admits a namespace, and a CIDR peer can silently widen that "+
				"to the whole cluster", i, port, peer.IPBlock.CIDR)
			continue
		}
		if peer.NamespaceSelector == nil {
			t.Errorf("deploy/kubernetes/networkpolicy.yaml from[%d] on port %d has no "+
				"namespaceSelector; without one the peer matches pods in this server's own "+
				"namespace instead of the monitoring namespace", i, port)
			continue
		}
		const nsLabel = "kubernetes.io/metadata.name"
		if got := peer.NamespaceSelector.MatchLabels[nsLabel]; got != shippedMonitoringNamespace {
			t.Errorf("deploy/kubernetes/networkpolicy.yaml from[%d] on port %d selects %s=%q, "+
				"want %q. docs/observability.md and deploy/kubernetes/README.md both document "+
				"this namespace as the shipped default, and a wrong value is a scrape that "+
				"silently never happens", i, port, nsLabel, got, shippedMonitoringNamespace)
		}
	}
}

// TestShippedServiceMonitorMatchesService ties servicemonitor.yaml.example to
// the Service it scrapes. kubeconform proves the object is well-formed against
// the CRD schema; it cannot tell that a port name, label selector, or path
// points somewhere real. Each of these failures leaves Prometheus with no
// target and nothing logged anywhere.
func TestShippedServiceMonitorMatchesService(t *testing.T) {
	_, svc, _, sm := loadShippedManifests(t)

	// The ServiceMonitor selects the Service by label, so its selector has to be
	// satisfied by the Service's own metadata labels (not the Service's pod selector).
	for k, v := range sm.Spec.Selector.MatchLabels {
		if svc.Metadata.Labels[k] != v {
			t.Errorf("servicemonitor.yaml.example selects %s=%q but service.yaml's labels have "+
				"%s=%q; the ServiceMonitor would match no Service and Prometheus would show no target",
				k, v, k, svc.Metadata.Labels[k])
		}
	}
	if len(sm.Spec.Selector.MatchLabels) == 0 {
		t.Error("servicemonitor.yaml.example has an empty selector")
	}
	if _, ok := sm.Spec.Selector.MatchLabels[shippedSelectorLabel]; !ok {
		t.Errorf("servicemonitor.yaml.example does not select on %s, the label every other "+
			"shipped manifest keys off", shippedSelectorLabel)
	}

	if n := len(sm.Spec.Endpoints); n != 1 {
		t.Fatalf("servicemonitor.yaml.example declares %d endpoints, want 1", n)
	}
	ep := sm.Spec.Endpoints[0]

	// endpoints[].port names a SERVICE port, not a container port — the single
	// most common way to author a ServiceMonitor that never produces a target.
	svcPortNames := map[string]bool{}
	for _, p := range svc.Spec.Ports {
		svcPortNames[p.Name] = true
	}
	if !svcPortNames[ep.Port] {
		t.Errorf("servicemonitor.yaml.example scrapes port %q, which service.yaml does not "+
			"expose (it has %v). endpoints[].port names a Service port", ep.Port, svcPortNames)
	}
	if ep.Path != "/metrics" {
		t.Errorf("servicemonitor.yaml.example scrapes path %q, want /metrics — the only path the "+
			"metrics listener serves (cmd/server serveMetricsEndpoint)", ep.Path)
	}
	// AC 3 of SOL-152424: sensible scrape defaults, and the timeout must stay
	// under the interval or Prometheus rejects the scrape config outright.
	if ep.Interval != "15s" || ep.ScrapeTimeout != "10s" {
		t.Errorf("servicemonitor.yaml.example has interval=%q scrapeTimeout=%q, want 15s/10s as "+
			"documented; scrapeTimeout must also stay below interval or Prometheus rejects the config",
			ep.Interval, ep.ScrapeTimeout)
	}
}

// readManifest parses one shipped manifest into out, failing the test on a
// read or YAML error.
func readManifest(t *testing.T, rel string, out any) {
	t.Helper()
	raw, err := os.ReadFile(repoPath(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
}

// parseGoByteCount accepts exactly what the Go runtime accepts for GOMEMLIMIT:
// a decimal integer with an optional B/KiB/MiB/GiB/TiB suffix (IEC, powers of
// two). Deliberately strict — the point is to reject Kubernetes' Mi/Gi, which
// the runtime rejects too, fatally and at startup.
func parseGoByteCount(s string) (int64, error) {
	for _, u := range []struct {
		suffix string
		scale  int64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1},
	} {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a decimal integer before the %s suffix", digits, u.suffix)
		}
		return n * u.scale, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("no recognized suffix (B, KiB, MiB, GiB, TiB) and not a bare byte count")
	}
	return n, nil
}

// parseK8sByteCount handles the Kubernetes quantity suffixes this manifest
// plausibly uses for memory. Only what is needed to read limits.memory.
func parseK8sByteCount(s string) (int64, error) {
	for _, u := range []struct {
		suffix string
		scale  int64
	}{
		{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"k", 1e3},
	} {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a decimal integer before the %s suffix", digits, u.suffix)
		}
		return n * u.scale, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognized Kubernetes quantity %q", s)
	}
	return n, nil
}
