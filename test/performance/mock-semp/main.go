// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command mock-semp is a replayer that pretends to be N Solace brokers, so
// MCP can be load-tested without spinning up real brokers.
//
// Flags:
//
//	-listen-addr         interface to bind for broker ports (localhost or 0.0.0.0)  default localhost
//	-listen-start        first broker port (inclusive)             default 8081
//	-listen-count        number of broker ports to bind             default 50
//	-config-listen-addr  interface to bind for /_mock/config        default localhost
//	                     (kept separate from -listen-addr so opening broker ports
//	                     to the LAN doesn't also expose the injection knob).
//	-config-port         /_mock/config endpoint port                default 9000
//	-default-latency-ms  fixed sleep before every response, all ports (default 0).
//	                     Overridden per-port by POST /_mock/config.
//	-no-canned-check     skip the canned staleness check (default: on, resolves
//	                     source canned/ at <exe-dir>/../mock-semp/canned). Use
//	                     when the source tree isn't reachable — go install,
//	                     copied binary, etc.
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
// The mock serves two tools' worth of canned responses:
//   - get-broker-status  (4 SEMPv1 POSTs to /SEMP)
//   - list-queues        (SEMPv2 GET, 2 pages)
//
// Any request that matches no rule returns 404 and logs a miss. The process
// exits non-zero at shutdown if any miss was logged — a silent 404 in a log
// file is easy to skim past; a failed exit code is a hard gate. See plan
// step 3.
package main

import (
	"bytes"
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
	noCannedCheck := flag.Bool("no-canned-check", false, "skip the canned staleness check (default: on, resolves source canned/ at <exe-dir>/../mock-semp/canned). Use when the source tree isn't reachable — go install, copied binary, etc.")
	flag.Parse()

	if *listenCount < 1 {
		log.Fatalf("listen-count must be >= 1")
	}
	if *defaultLatencyMs < 0 {
		log.Fatalf("default-latency-ms must be >= 0")
	}

	if !*noCannedCheck {
		src, err := resolveCannedSrc()
		if err != nil {
			log.Fatalf("mock-semp: %v", err)
		}
		if err := checkCannedStaleness(src); err != nil {
			log.Fatalf("mock-semp: %v", err)
		}
	}

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

	// Hard gate: if any request matched no rule, exit non-zero. The plan
	// spells this out explicitly — a silent miss looks identical to success
	// in a log file.
	misses := handler.missCount()
	if misses > 0 {
		fmt.Fprintf(os.Stderr, "mock-semp: %d unmatched requests during run — see logs\n", misses)
		os.Exit(1)
	}
}

// resolveCannedSrc locates the source canned/ directory the staleness
// check should compare against, relative to the running binary. build.sh
// drops the binary at bin/mock-semp with source at ../mock-semp/canned,
// so that's what we look for. If it isn't there, returns an error telling
// the caller to opt out with -no-canned-check — silently skipping would
// defeat the point of the check (catching stale embedded canned/*).
func resolveCannedSrc() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving executable path for canned staleness check: %w (pass -no-canned-check to skip)", err)
	}
	guess := filepath.Join(filepath.Dir(exe), "..", "mock-semp", "canned")
	if _, err := os.Stat(guess); err != nil {
		return "", fmt.Errorf("canned staleness check enabled but source canned/ not found at %s (pass -no-canned-check to skip)", guess)
	}
	return guess, nil
}

// checkCannedStaleness compares every file in the embedded canned/ tree
// against its on-disk counterpart in srcDir. It fatals on the first
// mismatch. The failure mode this catches: someone re-runs capture.sh
// (or edits a canned file by hand) but skips the mock rebuild, so go:embed
// still holds the old bytes and the mock silently replays yesterday's
// broker. Runs by default; disable with -no-canned-check when the source
// tree isn't reachable (e.g. after `go install` or a copied binary).
//
// Bytes-equal rather than mtime-equal — git checkout resets mtimes on
// clean working trees and would fire false alarms.
func checkCannedStaleness(srcDir string) error {
	entries, err := canned.ReadDir("canned")
	if err != nil {
		return fmt.Errorf("reading embedded canned/: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		embedded, err := canned.ReadFile("canned/" + name)
		if err != nil {
			return fmt.Errorf("reading embedded canned/%s: %w", name, err)
		}
		srcPath := filepath.Join(srcDir, name)
		onDisk, err := os.ReadFile(srcPath)
		if err != nil {
			return fmt.Errorf("canned staleness check: reading %s: %w", srcPath, err)
		}
		if !bytes.Equal(embedded, onDisk) {
			return fmt.Errorf("canned/%s: embedded copy differs from %s — rebuild mock-semp (go build ./mock-semp) so go:embed picks up the fresh capture", name, srcPath)
		}
	}
	return nil
}
