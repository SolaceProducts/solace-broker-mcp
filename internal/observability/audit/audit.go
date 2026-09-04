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

// Package audit owns the server's audit-record schema.
//
// docs/observability.md is the customer-facing authority on that schema; this
// package is its executable form. NewEvent (event.go) is the single
// constructor, and it enforces the field-applicability table rather than
// leaving each emission site to reproduce it — the reason a reviewer's query
// can rely on record shape, and the reason two emitters cannot drift into two
// shapes of the same record. HashArgs (hash.go) is the only way arguments
// reach a record: as an RFC 8785 SHA-256 digest, never as values.
//
// The v1 default is OFF (door-closing policy) — operators opt in with
// OBS_AUDIT_LOG_ENABLED, checked through Enabled below. Every record carries
// schema.AuditSchemaVersion.
package audit

import "github.com/SolaceProducts/solace-broker-mcp/internal/config"

// Enabled reports whether the audit log is turned on, reading the
// OBS_AUDIT_LOG_ENABLED flag off the observability config. Later wiring
// consults this before emitting audit records.
func Enabled(cfg config.ObservabilityConfig) bool {
	return cfg.AuditLogEnabled
}
