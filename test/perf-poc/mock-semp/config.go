// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
)

// portOverride is the per-port latency and error configuration applied by
// the injection middleware. Zero values = no injection, so the default
// (nothing sent to /_mock/config) yields a pass-through mock.
type portOverride struct {
	latencyMs      int              // fixed sleep before responding
	errorRate      float64          // probability [0,1] of returning an error instead of the canned response
	errorStatuses  []weightedStatus // status codes to pick from when injecting; empty = 500
	errorBudgetTot int64            // total errors this config permits; 0 = unlimited
	errorBudget    *atomic.Int64    // remaining error count; nil when errorBudgetTot == 0
}

// weightedStatus is one entry in the error-injection status pool. Weight
// is a relative share (not a probability); the handler picks with
// cumulative-weight roulette.
type weightedStatus struct {
	code   int
	weight int
}

// configStore holds the latest per-port overrides. Each entry is an
// atomic.Pointer so a POST /_mock/config swaps the pointer once and the
// next request sees the new config with no lock contention on the hot
// path. The map itself is populated once at construction and never
// mutated after, so request-path reads race only against pointer swaps,
// not map growth.
type configStore struct {
	perPort map[int]*atomic.Pointer[portOverride]
}

// newConfigStore pre-registers every broker port at construction. After
// this, the map is read-only — POST /_mock/config only Store()s into
// the pre-allocated atomic pointers, so the request-path get() never
// races against map growth.
func newConfigStore(ports []int) *configStore {
	s := &configStore{perPort: make(map[int]*atomic.Pointer[portOverride], len(ports))}
	for _, p := range ports {
		s.perPort[p] = &atomic.Pointer[portOverride]{}
	}
	return s
}

// get returns the current override for a port; zero-valued if none set.
// Never returns nil.
func (s *configStore) get(port int) portOverride {
	p, ok := s.perPort[port]
	if !ok {
		return portOverride{}
	}
	if v := p.Load(); v != nil {
		return *v
	}
	return portOverride{}
}

// set replaces the override for a port. Returns false if the port was
// not pre-registered — callers on the POST /_mock/config path should
// reject unknown ports with 400 rather than growing the map on the fly.
func (s *configStore) set(port int, o portOverride) bool {
	p, ok := s.perPort[port]
	if !ok {
		return false
	}
	p.Store(&o)
	return true
}

// configRequest is the wire shape accepted by POST /_mock/config.
// Ports omitted from the map are left unchanged (not zeroed) — a
// partial update is a common case (e.g. "add latency to broker 3").
type configRequest struct {
	Ports map[int]portOverrideJSON `json:"ports"`
}

type portOverrideJSON struct {
	LatencyMs     int                  `json:"latency_ms"`
	ErrorRate     float64              `json:"error_rate"`
	ErrorStatuses []weightedStatusJSON `json:"error_statuses,omitempty"`
	// ErrorCount caps how many errors are injected in total for this
	// config. 0 = unlimited (keep injecting at ErrorRate forever). A
	// re-POST resets the counter, so callers can "arm" a fresh budget.
	ErrorCount int64 `json:"error_count,omitempty"`
}

type weightedStatusJSON struct {
	Code   int `json:"code"`
	Weight int `json:"weight"`
}

// retryableStatuses are the HTTP codes the MCP resilience layer treats as
// retryable. Any other code short-circuits the retry chain, so injecting
// it wouldn't exercise the code path this PoC cares about.
var retryableStatuses = map[int]bool{
	429: true, // full backoff chain, honors Retry-After
	500: true, // one retry
	502: true, // one retry
	503: true, // full backoff chain, honors Retry-After — realistic "broker busy"
	504: true, // one retry
}

// handler returns the HTTP handler for POST /_mock/config. GET returns
// 405 — the endpoint is write-only, and a GET response would need to
// snapshot every port's config, which is more machinery than the PoC
// needs.
func (s *configStore) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req configRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		for port, o := range req.Ports {
			if o.ErrorRate < 0 || o.ErrorRate > 1 {
				http.Error(w, "error_rate must be in [0,1]", http.StatusBadRequest)
				return
			}
			if o.ErrorCount < 0 {
				http.Error(w, "error_count must be >= 0", http.StatusBadRequest)
				return
			}
			statuses := make([]weightedStatus, 0, len(o.ErrorStatuses))
			for _, es := range o.ErrorStatuses {
				if !retryableStatuses[es.Code] {
					http.Error(w, "error_statuses[].code must be one of 429,500,502,503,504", http.StatusBadRequest)
					return
				}
				if es.Weight <= 0 {
					http.Error(w, "error_statuses[].weight must be > 0", http.StatusBadRequest)
					return
				}
				statuses = append(statuses, weightedStatus{code: es.Code, weight: es.Weight})
			}
			ov := portOverride{
				latencyMs:      o.LatencyMs,
				errorRate:      o.ErrorRate,
				errorStatuses:  statuses,
				errorBudgetTot: o.ErrorCount,
			}
			if o.ErrorCount > 0 {
				var budget atomic.Int64
				budget.Store(o.ErrorCount)
				ov.errorBudget = &budget
			}
			if !s.set(port, ov) {
				http.Error(w, fmt.Sprintf("unknown port %d — not in -listen-start..+listen-count range", port), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
