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

// Package schema is the single source of truth for the observability output
// schema: the version constants, and the closed vocabularies more than one
// signal renders. Emitters (metrics, audit log) stamp the relevant version
// constant onto every record they produce so downstream consumers can detect
// breaking changes. Bump a version here when, and only when, the shape of the
// corresponding output changes incompatibly.
//
// A vocabulary belongs here when two or more signals carry it and a consumer's
// saved query breaks if they disagree — see AuthFailureReason, which the
// auth_failure audit record and the mcp_auth_failure_total counter share. A
// vocabulary only one signal renders stays with that signal (audit's
// error_type, its authz-denied reason sets). This package is a leaf on
// purpose: it imports nothing of ours, so every producer and every emitter can
// import it without a cycle.
package schema

// MetricsSchemaVersion is stamped onto every metrics record the server emits.
// Bump the minor component on an additive change (a new metric), the major
// component on anything else — see docs/observability.md, "The schema is
// additive-only within a major version". 1.1 (SOL-152093): added the
// mcp_semp_request_total and mcp_semp_request_duration_seconds families.
// 1.2 (SOL-152099): added the mcp_auth_failure_total and mcp_authz_denied_total
// families. 1.3 (SOL-152088): added mcp_broker_reachable,
// mcp_broker_unreachable_reason, and mcp_broker_last_result_timestamp_seconds.
// 1.5 (SOL-152418): added the mcp_otel_metrics_exported_total and
// mcp_otel_metrics_dropped_total families. (1.4 was reserved for
// SOL-153332, but SOL-152418 merged first and took it, so SOL-153332
// shipped as 1.6 instead — 1.4 was never a real, emitted value.)
// 1.6 (SOL-153332): added the mcp_broker_authz_denied_total family.
// 1.7 (SOL-152284): added mcp_token_exchange_circuit_breaker_state.
const MetricsSchemaVersion = "1.7"

// AuditSchemaVersion is stamped onto every audit-log record the server emits.
// Bump the minor component on an additive change (a new field), the major
// component on anything else — see docs/observability.md, "The schema is
// additive-only within a major version". 1.1 (SOL-152090): audit_drop gained
// the optional dropped_audit_event_type, tool, and broker fields.
// 1.2 (SOL-153332): audit_event_type grew to seven (added
// broker_authz_denied), and error_type and reason each gained a value
// (broker_permission_denied, permission_denied) for a hop-2 (broker-side)
// authorization denial. Both additions previously shipped without a version
// bump; correcting that now costs nothing; per v0.8.0 (the latest tag),
// AuditSchemaVersion was still 1.0 there, so no released consumer has ever
// pinned to a version that saw the smaller closed sets.
const AuditSchemaVersion = "1.2"
