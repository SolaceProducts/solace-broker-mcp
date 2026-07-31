// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command mock-semp is a throwaway PoC replayer that pretends to be N Solace
// brokers, so MCP can be load-tested without spinning up real brokers.
//
// See docs/plans/2026-07-22-semp-mock-perf-poc.md.
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
//
// About the latency knob: MCP caps in-flight SEMP calls per broker via a
// semaphore sized by `semp.max_concurrent_per_broker` (see
// internal/semp/broker.go — one Semaphore(N) per broker, shared across
// SEMPv1 and SEMPv2). At zero latency the mock replies in microseconds, so
// slots are released as fast as they're taken and no queue ever forms
// inside MCP even under heavy load. Setting a latency here holds each slot
// for that duration, so once the loadgen sends more than N concurrent
// calls to the same broker, the extra callers park inside MCP waiting to
// acquire a slot. That parked-goroutine queue is the thing this PoC is
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
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	flag.Parse()

	if *listenCount < 1 {
		log.Fatalf("listen-count must be >= 1")
	}
	if *defaultLatencyMs < 0 {
		log.Fatalf("default-latency-ms must be >= 0")
	}

	ports := make([]int, *listenCount)
	for i := 0; i < *listenCount; i++ {
		ports[i] = *listenStart + i
	}
	cfg := newConfigStore(ports)
	// Seed every broker port with the flag's latency. Individual ports can
	// still be overridden later via POST /_mock/config; the flag just spares
	// the caller from a curl on startup when a uniform latency is all they
	// need (the common PoC case — see run.sh LATENCY_MS).
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
