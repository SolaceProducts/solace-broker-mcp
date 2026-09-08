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
// schema versions. Emitters (metrics, audit log) stamp the relevant version
// constant onto every record they produce so downstream consumers can detect
// breaking changes. Bump a version here when, and only when, the shape of the
// corresponding output changes incompatibly.
package schema

// MetricsSchemaVersion is stamped onto every metrics record the server emits.
// V1 baseline. Bump on any backwards-incompatible change to the metrics shape.
const MetricsSchemaVersion = "1.0"

// AuditSchemaVersion is stamped onto every audit-log record the server emits.
// Bump the minor component on an additive change (a new field), the major
// component on anything else — see docs/observability.md, "The schema is
// additive-only within a major version". 1.1 (SOL-152090): audit_drop gained
// the optional dropped_audit_event_type, tool, and broker fields.
const AuditSchemaVersion = "1.1"
