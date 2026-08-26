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

package tokenexchange

import (
	"errors"
	"log/slog"
	"time"
)

// maxRetryAfterRawLogLen caps a malformed Retry-After's logged raw value —
// it comes from the IdP and must never be trusted to log unbounded content.
const maxRetryAfterRawLogLen = 64

// gateCheck is consulted before the circuit breaker so a gated call touches
// no breaker bookkeeping and makes no IdP round-trip.
func (e *Exchanger) gateCheck() bool {
	until := e.gatedUntil.Load()
	return until > 0 && e.nowFunc().UnixNano() < until
}

// clampRetryAfter falls back to defaultMaxHonoredRetryAfter when the
// operator hasn't configured max_honored_duration.
func (e *Exchanger) clampRetryAfter(delay time.Duration) (clamped time.Duration, wasClamped bool) {
	ceiling := e.maxHonoredRetryAfter
	if ceiling <= 0 {
		ceiling = defaultMaxHonoredRetryAfter
	}
	if delay > ceiling {
		return ceiling, true
	}
	return delay, false
}

// raiseGate only ever extends the gate (CAS-max), never shortens it — a
// shorter Retry-After from one chain must not clobber a longer one already
// set by a concurrent chain. Returns the gate's actual resulting instant
// and whether THIS call is what raised it (false if delay was non-positive,
// or if a concurrent chain's later value already won) — callers use this
// to avoid logging "gate set" when nothing was actually raised.
func (e *Exchanger) raiseGate(delay time.Duration) (effectiveUntil time.Time, raised bool) {
	if delay <= 0 {
		return time.Time{}, false
	}
	newUntil := e.nowFunc().Add(delay).UnixNano()
	for {
		cur := e.gatedUntil.Load()
		if newUntil <= cur {
			return time.Unix(0, cur), false
		}
		if e.gatedUntil.CompareAndSwap(cur, newUntil) {
			return time.Unix(0, newUntil), true
		}
	}
}

// raiseGateOnExhaustedRateLimit only acts on an EXHAUSTED rate-limited
// chain — a 429 that a later retry in the same chain resolves never reaches
// here, so an intermediate throttle doesn't pre-emptively block other
// callers.
func (e *Exchanger) raiseGateOnExhaustedRateLimit(err error, brokerAlias string) {
	if !errors.Is(err, ErrExchangeRetriesExhausted) {
		return
	}
	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) || exchErr.FailureClass != FailureClassRateLimited {
		return
	}
	if exchErr.RetryAfterResult == nil {
		return
	}

	if !exchErr.RetryAfterResult.ok {
		logGateNotSet(brokerAlias, exchErr.RetryAfterResult.raw)
		return
	}

	delay, wasClamped := e.clampRetryAfter(exchErr.RetryAfterResult.delay)
	until, raised := e.raiseGate(delay)
	if !raised {
		// delay <= 0 (e.g. Retry-After: 0, or a past date parseRetryAfter
		// already floored to zero) or a concurrent chain's later value
		// already won — either way, nothing changed, so logging "gate set"
		// or "clamped" here would be misleading.
		return
	}
	if wasClamped {
		logGateClamped(brokerAlias, exchErr.RetryAfterResult.delay, delay)
	} else {
		logGateSet(brokerAlias, delay, until)
	}
}

// logGateSet: the IdP asked us to wait; every caller is now paced back, not
// just the one that hit it.
func logGateSet(brokerAlias string, honored time.Duration, gatedUntil time.Time) {
	slog.Warn("token exchange rate limited: honoring IdP Retry-After for all callers",
		slog.String("broker", brokerAlias),
		slog.String("retry_after", honored.String()),
		slog.Time("gated_until", gatedUntil))
}

// logGateClamped: the IdP asked for longer than the configured cap allows.
func logGateClamped(brokerAlias string, requested, clampedTo time.Duration) {
	slog.Warn("token exchange Retry-After exceeded configured cap, clamping",
		slog.String("broker", brokerAlias),
		slog.String("requested", requested.String()),
		slog.String("clamped_to", clampedTo.String()))
}

// logGateNotSet: the IdP gave us nothing usable, so the gate can't engage —
// distinguishes absent from malformed since the latter is a more
// interesting signal.
func logGateNotSet(brokerAlias string, raw string) {
	if raw == "" {
		slog.Warn("token exchange rate limited: IdP sent no Retry-After, cannot pace subsequent callers",
			slog.String("broker", brokerAlias))
		return
	}
	slog.Warn("token exchange rate limited: IdP sent an unparseable Retry-After, cannot pace subsequent callers",
		slog.String("broker", brokerAlias),
		slog.String("retry_after_raw", capString(raw, maxRetryAfterRawLogLen)))
}

// capString truncates the body to at most n bytes, then appends a
// "...(truncated)" marker — so the total returned length can exceed n —
// so a truncated value in logs is never mistaken for the complete one.
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
