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

// Package audit is the home for the server's audit log. Skeleton
// (SOL-151278): only the capability gate exists today; the audit-record
// emission lands in a later story. The v1 default is OFF (door-closing
// policy) — operators opt in. Emitted records will carry
// schema.AuditSchemaVersion.
package audit

import "github.com/SolaceDev/solace-broker-mcp/internal/config"

// Enabled reports whether the audit log is turned on, reading the
// OBS_AUDIT_LOG_ENABLED flag off the loaded config. Later wiring consults this
// before emitting audit records.
func Enabled(cfg *config.ServerConfig) bool {
	return cfg.Observability.AuditLogEnabled
}
