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

// Command mock-semp is a replayer that pretends to be N Solace brokers, so
// MCP can be load-tested without spinning up real brokers.
//
// Flags:
//
//	-listen-addr         interface to bind for broker ports (localhost or 0.0.0.0)  default localhost
//	-listen-start        first broker port (inclusive)             default 8081
//	-listen-count        number of broker ports to bind             default 50
//	-config-listen-addr  interface to bind for /_mock/*             default localhost
//	                     (kept separate from -listen-addr so opening broker ports
//	                     to the LAN doesn't also expose the injection knob).
//	-config-port         /_mock/config and /_mock/hits port         default 9000
//	-default-latency-ms  fixed sleep before every response, all ports (default 0).
//	                     Overridden per-port by POST /_mock/config.
//	-canned-dir          directory of captured SEMP responses to replay.
//	                     Default: <exe-dir>/../mock-semp/canned, which is where
//	                     build.sh + regen-golden.sh put them. The captures are
//	                     lab data and are not in git — an empty or missing dir
//	                     is a startup fatal pointing at regen-golden.sh.
//
// About the latency knob: MCP caps in-flight SEMP calls per broker via a
// semaphore sized by `semp.max_concurrent_per_broker` (see
// internal/semp/broker.go — one Semaphore(N) per broker, shared across
// SEMPv1 and SEMPv2). At zero latency the mock replies in microseconds, so
// slots are released as fast as they're taken and no queue ever forms
// inside MCP even under heavy load. Setting a latency here holds each slot
// for that duration, so once the loadgen sends more than N concurrent
// calls to the same broker, the extra callers park inside MCP waiting to
// acquire a slot. That parked-goroutine queue is the thing this suite is
// built to observe (backpressure behavior, timeout handling, memory under
// contention) — the flag is how you make it appear on demand.
//
// The mock serves four tools' worth of canned responses:
//   - get-broker-status  (4 SEMPv1 POSTs to /SEMP, 5 on an appliance)
//   - list-queues        (SEMPv2 GET, paginated)
//   - list-rdps          (SEMPv2 GET, paginated)
//   - get-rdp-status     (SEMPv2 GET x3, for one pinned RDP only)
//
// Any request that matches no rule returns 404 and logs a miss. The process
// exits non-zero at shutdown if any miss was logged — a silent 404 in a log
// file is easy to skim past; a failed exit code is a hard gate. See plan
// step 3. That gate is what makes the pinned-RDP rules safe: a request for an
// RDP the capture doesn't cover fails the run instead of being answered with
// the pinned RDP's body.
//
// Shutdown also logs how many requests each rule served. That is the harness's
// own measurement of per-tool SEMP fan-out — the factor that converts
// loadgen's tool calls/s into the SEMP requests/s the performance targets are
// written in. GET /_mock/hits reports the same counts mid-run; POST reports
// and zeroes them, which is how the run scripts keep the fidelity gate's dozen
// requests out of the load phase's totals.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

func main() {
	listenAddr := flag.String("listen-addr", "localhost", "interface to bind broker ports (e.g. localhost or 0.0.0.0)")
	listenStart := flag.Int("listen-start", 8081, "first broker port (inclusive)")
	listenCount := flag.Int("listen-count", 50, "number of broker ports to bind")
	configListenAddr := flag.String("config-listen-addr", "localhost", "interface to bind /_mock/config; keep on localhost even when -listen-addr is 0.0.0.0 so LAN peers can't inject errors")
	configPort := flag.Int("config-port", 9000, "port for /_mock/config")
	defaultLatencyMs := flag.Int("default-latency-ms", 0, "fixed sleep before every response (all ports); overridden per-port by /_mock/config")
	cannedDir := flag.String("canned-dir", "", "directory of captured SEMP responses to replay (default <exe-dir>/../mock-semp/canned)")
	flag.Parse()

	if *listenCount < 1 {
		log.Fatalf("listen-count must be >= 1")
	}
	if *defaultLatencyMs < 0 {
		log.Fatalf("default-latency-ms must be >= 0")
	}

	// Load the captured responses before anything binds a port: newHandler
	// reads every fixture up front, so a bad or absent capture fails here
	// rather than as a 404 in the middle of a load run.
	dir, err := openCanned(*cannedDir)
	if err != nil {
		log.Fatalf("mock-semp: %v", err)
	}
	log.Printf("mock-semp: replaying canned responses from %s", dir)

	ports := make([]int, *listenCount)
	for i := 0; i < *listenCount; i++ {
		ports[i] = *listenStart + i
	}
	cfg := newConfigStore(ports)
	// Seed every broker port with the flag's latency. Individual ports can
	// still be overridden later via POST /_mock/config; the flag just spares
	// the caller from a curl on startup when a uniform latency is all they
	// need (the common case — see run.sh LATENCY_MS).
	if *defaultLatencyMs > 0 {
		for _, p := range ports {
			cfg.set(p, portOverride{latencyMs: *defaultLatencyMs})
		}
	}
	handler := newHandler(cfg)

	// Every broker port shares the same handler. Middleware injects latency
	// and error overrides based on the port the request landed on.
	brokerServers := make([]*http.Server, 0, *listenCount)
	for i := 0; i < *listenCount; i++ {
		port := *listenStart + i
		mux := http.NewServeMux()
		mux.Handle("/", handler.withInjection(port))
		srv := &http.Server{
			Addr:              net.JoinHostPort(*listenAddr, strconv.Itoa(port)),
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		brokerServers = append(brokerServers, srv)
	}

	configMux := http.NewServeMux()
	configMux.Handle("/_mock/config", cfg.handler())
	// Same port as /_mock/config: both are operator knobs, and neither should
	// be reachable from the LAN when broker ports are.
	configMux.Handle("/_mock/hits", handler.hitsHandler())
	configSrv := &http.Server{
		Addr:              net.JoinHostPort(*configListenAddr, strconv.Itoa(*configPort)),
		Handler:           configMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	var wg sync.WaitGroup
	startServer := func(srv *http.Server, label string) {
		wg.Go(func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("%s: %v", label, err)
			}
		})
	}
	for _, srv := range brokerServers {
		startServer(srv, "broker "+srv.Addr)
	}
	startServer(configSrv, "config "+configSrv.Addr)

	log.Printf("mock-semp: %d broker ports %s:%d..%d, config on %s:%d, default-latency-ms=%d",
		*listenCount, *listenAddr, *listenStart, *listenStart+*listenCount-1,
		*configListenAddr, *configPort, *defaultLatencyMs)

	// Wait for shutdown signal, then let in-flight requests drain.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("mock-semp: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range brokerServers {
		_ = srv.Shutdown(shutdownCtx)
	}
	_ = configSrv.Shutdown(shutdownCtx)
	wg.Wait()

	// Per-rule counts before the gate: on a failing run they say which rules
	// did fire, which is the first thing you want next to the MISS lines.
	handler.logHitSummary()

	// Hard gate: if any request matched no rule, exit non-zero. The plan
	// spells this out explicitly — a silent miss looks identical to success
	// in a log file.
	misses := handler.missCount()
	if misses > 0 {
		fmt.Fprintf(os.Stderr, "mock-semp: %d unmatched requests during run — see logs\n", misses)
		os.Exit(1)
	}
}

// openCanned points the package-level canned FS at a directory of captured
// SEMP responses and returns the resolved path. An empty dir argument
// resolves to <exe-dir>/../mock-semp/canned — build.sh drops the binary at
// bin/mock-semp, so that lands on the source canned/.
//
// The captures are lab data and deliberately untracked (see
// mock-semp/canned/README.md), so "directory has no fixtures" is the normal
// state of a fresh clone rather than a corrupted install. Both that and a
// missing directory return an error naming regen-golden.sh — the only
// supported way to produce them.
func openCanned(dir string) (string, error) {
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolving executable path to locate canned/: %w (pass -canned-dir)", err)
		}
		dir = filepath.Join(filepath.Dir(exe), "..", "mock-semp", "canned")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading canned dir %s: %w — capture fixtures with ./regen-golden.sh", dir, err)
	}
	fixtures := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext == ".json" || ext == ".xml" {
			fixtures++
		}
	}
	if fixtures == 0 {
		return "", fmt.Errorf("no .json/.xml fixtures in %s — these are captures from a real broker and are not in git; run ./regen-golden.sh against a lab broker first", dir)
	}
	canned = os.DirFS(dir)
	return dir, nil
}
