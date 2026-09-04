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

package audit

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
)

// Message is the slog message on every audit record. It is a routing anchor
// for log pipelines that key off msg rather than a field, so it is fixed and
// identical across record types — the record type lives in audit_event_type,
// never in the message (SOL-152090).
const Message = "audit"

// EventValue is the value of the event field on EVERY audit record, drops
// included. Customers select the audit sub-stream with the single predicate
// event="audit"; a record that changed this value to describe itself would
// fall outside that filter, which is precisely how the one record that says
// "you are missing audit records" would go unseen. Discriminate on
// audit_event_type, never on event.
const EventValue = "audit"

// EventType is the audit record discriminator, carried in audit_event_type.
//
// This is a closed set. Adding a member is an audit-schema change: extend
// applicabilityByType in the same commit or NewEvent will reject the new type,
// and check whether schema.AuditSchemaVersion must be bumped.
type EventType string

// The closed set of audit record types. Grown to seven by SOL-153332
// (Story 49), which added EventBrokerAuthzDenied for a hop-2 (broker-side)
// authorization denial, paired with hop 1's EventAuthzDenied.
const (
	// EventOperation records one completed tool invocation: what ran, against
	// which broker, on whose behalf, and how it ended.
	EventOperation EventType = "operation"
	// EventAuthSuccess records a token that verified.
	EventAuthSuccess EventType = "auth_success"
	// EventAuthFailure records a token that did not verify. The principal is
	// unknown by definition unless the token parsed far enough to yield it.
	EventAuthFailure EventType = "auth_failure"
	// EventAuthzDenied records a hop-1 authorization denial: the caller
	// authenticated, but this server refused the tool.
	EventAuthzDenied EventType = "authz_denied"
	// EventBrokerAuthzDenied records a hop-2 authorization denial: the broker
	// refused the exchanged identity. It names the broker that refused, and it
	// coexists with the EventOperation record for the same call rather than
	// replacing it, because execution had already started (SOL-153332).
	EventBrokerAuthzDenied EventType = "broker_authz_denied"
	// EventBrokerAuthRetry records a broker-side authentication retry.
	EventBrokerAuthRetry EventType = "broker_auth_retry"
	// EventAuditDrop records that an audit record could not be written. It
	// carries only the common fields — there is no surviving record to
	// describe.
	EventAuditDrop EventType = "audit_drop"
)

// Outcome is the value of the outcome field. Three values, per ADR-009.
// An authorization denial is a record TYPE, not an outcome (Q-015), and a
// recovered panic is OutcomeError with error_type="panic" — there is no
// "denied" and no "panic" outcome.
type Outcome string

const (
	// OutcomeSuccess: the operation completed as asked.
	OutcomeSuccess Outcome = "success"
	// OutcomeError: the operation failed. error_type carries the cause.
	OutcomeError Outcome = "error"
	// OutcomeCancelled: the caller went away before the operation finished.
	// The enum surface ships here; SOL-152426 (Story 42) adds the branch that
	// classifies a call as cancelled.
	OutcomeCancelled Outcome = "cancelled"
)

// errorTypeVocabulary is the closed error_type set, and the single source of
// truth for it.
//
// These are exactly the values internal/tools computes and passes to
// logToolResult today. They are NOT re-derived here from a ticket: the count
// has drifted twice already because each revision counted only the call site
// it happened to be looking at. internal/tools/audit_error_type_drift_test.go
// walks the AST of every emit site in that package and fails if the two sets
// diverge, so adding a value there without adding it here breaks the build
// rather than silently shipping a record the constructor rejects.
//
// Per-site provenance (verified against internal/tools at SOL-152090):
//
//	manager.go              panic, unknown_tool, missing_broker,
//	                        unknown_broker, broker_init_error,
//	                        validation_error, execution_error, nil_result,
//	                        output_validation_error, marshal_error
//	register.go             bad_request, panic, marshal_error
//	describe_semp_schema.go bad_request, not_found, panic, marshal_error
var errorTypeVocabulary = map[string]struct{}{
	"panic":                   {},
	"unknown_tool":            {},
	"missing_broker":          {},
	"unknown_broker":          {},
	"broker_init_error":       {},
	"validation_error":        {},
	"execution_error":         {},
	"nil_result":              {},
	"output_validation_error": {},
	"marshal_error":           {},
	"bad_request":             {},
	"not_found":               {},
}

// ErrorTypes returns the closed error_type vocabulary, sorted, as a fresh
// slice the caller may mutate. Tests assert against len(ErrorTypes()) rather
// than a hardcoded count so adding a value cannot silently pass a stale
// assertion.
func ErrorTypes() []string { return sortedKeys(errorTypeVocabulary) }

// EventTypes returns the closed audit_event_type set, sorted, as a fresh
// slice. Derived from applicabilityByType, so the discriminator set and the
// applicability table cannot disagree.
func EventTypes() []string {
	out := make([]string, 0, len(applicabilityByType))
	for t := range applicabilityByType {
		out = append(out, string(t))
	}
	sort.Strings(out)
	return out
}

// The reason vocabularies. There are three, one per record type that carries
// reason, and they are validated separately: an authentication reason on an
// authorization record (or a hop-1 reason on a hop-2 record) is a bug that
// would corrupt a reviewer's query, so the constructor rejects it rather than
// letting the vocabularies bleed into one another.
var (
	authFailureReasons = map[string]struct{}{
		"invalid_token":     {},
		"expired":           {},
		"audience_mismatch": {},
		"signature_invalid": {},
		"missing":           {},
	}
	authzDeniedReasons = map[string]struct{}{
		"missing_claim": {},
		"not_permitted": {},
	}
	brokerAuthzDeniedReasons = map[string]struct{}{
		"permission_denied": {},
	}
)

// applicability says whether a field may, must, or must not appear on a given
// record type. Encoding the table once here is what keeps the emission sites
// (SOL-152096, SOL-152097, SOL-153332) from drifting into different shapes of
// the same record.
type applicability uint8

const (
	// fieldForbidden: setting the field is an error.
	fieldForbidden applicability = iota
	// fieldOptional: the field is emitted when set, omitted when not.
	fieldOptional
	// fieldRequired: the field must be set.
	fieldRequired
	// fieldOnErrorOnly: required when outcome is "error", forbidden otherwise.
	fieldOnErrorOnly
)

// rules is one row of the field-applicability table.
type rules struct {
	outcome         applicability
	allowedOutcomes map[Outcome]struct{} // nil means "every Outcome"
	errorType       applicability
	reason          applicability
	reasonVocab     map[string]struct{}
	tool            applicability
	argumentsHash   applicability
	broker          applicability
	timing          applicability // started_at / duration_ms
	identity        applicability // principal.sub / agent_client_id
}

// allOutcomes is the unrestricted outcome set: success, error, cancelled.
var allOutcomes = map[Outcome]struct{}{
	OutcomeSuccess:   {},
	OutcomeError:     {},
	OutcomeCancelled: {},
}

// retryOutcomes: a broker auth retry either worked or it did not. There is no
// caller to cancel it.
var retryOutcomes = map[Outcome]struct{}{
	OutcomeSuccess: {},
	OutcomeError:   {},
}

// applicabilityByType is the field-applicability table from SOL-152090, and
// the authority on audit record shape. docs/observability.md renders the same
// table for customers; change both together.
//
// Reading the rows: auth_success, auth_failure, authz_denied and
// broker_authz_denied carry NO outcome, because the record type already says
// what happened. authz_denied carries tool but no arguments_hash, because the
// call was refused before its arguments were dispatched; broker_authz_denied
// differs from it in exactly one column, broker, which is set because a hop-2
// denial names the broker that refused. audit_drop carries only the common
// fields.
//
// identity is fieldOptional rather than fieldRequired everywhere it applies:
// with auth mode "disabled" there is no principal to name, and a constructor
// that rejected the record in that mode would delete the audit trail on
// exactly the deployments that have no other identity signal.
var applicabilityByType = map[EventType]rules{
	EventOperation: {
		outcome: fieldRequired, allowedOutcomes: allOutcomes,
		errorType: fieldOnErrorOnly,
		reason:    fieldForbidden,
		tool:      fieldRequired, argumentsHash: fieldRequired,
		broker: fieldRequired, timing: fieldRequired, identity: fieldOptional,
	},
	EventAuthSuccess: {
		outcome: fieldForbidden, errorType: fieldForbidden, reason: fieldForbidden,
		tool: fieldForbidden, argumentsHash: fieldForbidden,
		broker: fieldForbidden, timing: fieldForbidden, identity: fieldOptional,
	},
	EventAuthFailure: {
		outcome: fieldForbidden, errorType: fieldForbidden,
		reason: fieldRequired, reasonVocab: authFailureReasons,
		tool: fieldForbidden, argumentsHash: fieldForbidden,
		broker: fieldForbidden, timing: fieldForbidden, identity: fieldOptional,
	},
	EventAuthzDenied: {
		outcome: fieldForbidden, errorType: fieldForbidden,
		reason: fieldRequired, reasonVocab: authzDeniedReasons,
		tool: fieldRequired, argumentsHash: fieldForbidden,
		broker: fieldForbidden, timing: fieldForbidden, identity: fieldOptional,
	},
	EventBrokerAuthzDenied: {
		outcome: fieldForbidden, errorType: fieldForbidden,
		reason: fieldRequired, reasonVocab: brokerAuthzDeniedReasons,
		tool: fieldRequired, argumentsHash: fieldForbidden,
		broker: fieldRequired, timing: fieldForbidden, identity: fieldOptional,
	},
	EventBrokerAuthRetry: {
		outcome: fieldRequired, allowedOutcomes: retryOutcomes,
		errorType: fieldForbidden, reason: fieldForbidden,
		tool: fieldForbidden, argumentsHash: fieldForbidden,
		broker: fieldRequired, timing: fieldOptional, identity: fieldOptional,
	},
	EventAuditDrop: {
		outcome: fieldForbidden, errorType: fieldForbidden, reason: fieldForbidden,
		tool: fieldForbidden, argumentsHash: fieldForbidden,
		broker: fieldForbidden, timing: fieldForbidden, identity: fieldForbidden,
	},
}

// Fields is the caller-supplied content of one audit record. Every field is
// optional at the type level and validated against the record type's row in
// applicabilityByType, so a call site cannot invent a shape.
//
// Deliberately absent: any field that would carry raw tool arguments. The only
// representation of arguments an audit record may hold is ArgumentsHash, from
// HashArgs.
type Fields struct {
	// Type is the record discriminator. Required.
	Type EventType
	// Outcome is how the operation ended.
	Outcome Outcome
	// ErrorType is the cause when Outcome is OutcomeError. Must be a member of
	// ErrorTypes().
	ErrorType string
	// Reason is why a denial or an authentication failure happened. Validated
	// against the vocabulary for Type.
	Reason string
	// Tool is the MCP tool name.
	Tool string
	// Broker is the broker alias in its configured (display) casing.
	Broker string
	// ArgumentsHash is HashArgs over the tool arguments. Never the arguments
	// themselves.
	ArgumentsHash string
	// StartedAt is when the operation began; rendered as started_at_utc, an
	// RFC 3339 UTC instant.
	StartedAt time.Time
	// Duration is how long it ran; rendered as duration_ms, whole milliseconds.
	Duration time.Duration
	// PanicRecovered marks a record whose error came from a recovered panic.
	// Only meaningful with Outcome OutcomeError and ErrorType "panic".
	PanicRecovered bool
	// Timestamp is when the record was created. Zero means "now"; tests set it
	// to make output deterministic.
	Timestamp time.Time
}

// Event is a validated audit record: attributes in a fixed order, plus the
// level to emit them at. Construct it with NewEvent — the zero value is not a
// valid record.
type Event struct {
	attrs []slog.Attr
	level slog.Level
	typ   EventType
}

// Type returns the record's audit_event_type.
func (e Event) Type() EventType { return e.typ }

// Level returns the slog level the record should be emitted at.
func (e Event) Level() slog.Level { return e.level }

// Attrs returns the record's attributes, in emission order, as a fresh slice.
// A copy, not the Event's own backing array: slog.Record.Add appends, and a
// caller that grew the returned slice would otherwise write into the Event.
func (e Event) Attrs() []slog.Attr {
	out := make([]slog.Attr, len(e.attrs))
	copy(out, e.attrs)
	return out
}

// levelByType is the emission level per record type. Denials are WARN because
// they want an operator's attention; an operation record is INFO regardless of
// outcome, since outcome carries the meaning and the operational "tool
// invoked" line already reports failures at ERROR.
//
// An operation record at INFO means audit records vanish if the server's log
// level is above INFO. That is a drop, and it is reported as one — see Emit's
// Enabled check — rather than being left to silently erase the audit trail.
//
// EventAuditDrop is ERROR, and that choice is load-bearing rather than
// editorial. "error" is the highest level an operator may configure
// (config.validLogLevels), so a drop notice at ERROR survives EVERY supported
// log level. At WARN it would not: a server at log_level=error would filter
// out the operation record AND the notice reporting it, producing exactly the
// total silence the notice exists to prevent, on a configuration we support.
// The record is also honestly an error condition — an audit record was lost.
var levelByType = map[EventType]slog.Level{
	EventOperation:         slog.LevelInfo,
	EventAuthSuccess:       slog.LevelInfo,
	EventAuthFailure:       slog.LevelWarn,
	EventAuthzDenied:       slog.LevelWarn,
	EventBrokerAuthzDenied: slog.LevelWarn,
	EventBrokerAuthRetry:   slog.LevelInfo,
	EventAuditDrop:         slog.LevelError,
}

// NewEvent validates f against the field-applicability table for f.Type and
// returns the audit record it describes. It is the only way to build an Event,
// so every emission site produces the same shape of the same record.
//
// Identity comes from ctx, never from f: principal.sub and agent_client_id are
// read from auth.PrincipalFrom(ctx), the principal the auth middleware attached
// for THIS request (SOL-152087). A call site cannot name a different caller
// than the one that was authenticated, and no claim beyond sub and client_id is
// written (decision D2 / Q-013). correlation_id likewise comes from
// correlation.From(ctx).
//
// Returns an error, never a partial record: a record that does not satisfy the
// table is not written at all. Callers that cannot proceed should emit a drop
// notice rather than a malformed record.
//
// Contract on the returned errors: callers log them VERBATIM (see
// internal/tools/manager.go's emitOperationAudit), so they must never embed
// caller-supplied free text. They name fields, record types, and
// closed-vocabulary values only — never Tool, Broker, ArgumentsHash, or
// anything derived from tool arguments. Keep it that way when adding a rule.
func NewEvent(ctx context.Context, f Fields) (Event, error) {
	r, ok := applicabilityByType[f.Type]
	if !ok {
		return Event{}, fmt.Errorf("audit: unknown audit_event_type %q; must be one of %v", f.Type, EventTypes())
	}
	if err := validate(f, r); err != nil {
		return Event{}, err
	}

	ts := f.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	// Common fields first, in a fixed order, so every audit record starts the
	// same way regardless of type.
	attrs := make([]slog.Attr, 0, 16)
	attrs = append(attrs,
		slog.String("event", EventValue),
		slog.String("audit_event_type", string(f.Type)),
		slog.String("audit_schema_version", schema.AuditSchemaVersion),
		slog.String("timestamp_utc", ts.UTC().Format(time.RFC3339Nano)),
	)
	// Omitted when empty rather than emitted as "", matching every other log
	// site in this repo: an empty correlation ID is indistinguishable from an
	// absent one. When the correlation slog handler is installed it stamps the
	// ID itself; setting it here too is safe because that handler never
	// overwrites or duplicates an ID the record already carries.
	if id := correlation.From(ctx); id != "" {
		attrs = append(attrs, slog.String("correlation_id", id))
	}

	if r.identity != fieldForbidden {
		if p := auth.PrincipalFrom(ctx); p.Present() {
			// A nested group, not a flat principal field: adding a claim later
			// is then a pure addition rather than the rename the schema freeze
			// forbids. v1 writes sub and nothing else into the group.
			attrs = append(attrs, slog.Group("principal", slog.String("sub", p.Sub())))
			if cid := p.ClientID(); cid != "" {
				attrs = append(attrs, slog.String("agent_client_id", cid))
			}
		}
	}

	if f.Tool != "" {
		attrs = append(attrs, slog.String("tool", f.Tool))
	}
	if f.Broker != "" {
		attrs = append(attrs, slog.String("broker", f.Broker))
	}
	if f.ArgumentsHash != "" {
		attrs = append(attrs, slog.String("arguments_hash", f.ArgumentsHash))
	}
	if f.Outcome != "" {
		attrs = append(attrs, slog.String("outcome", string(f.Outcome)))
	}
	if f.ErrorType != "" {
		attrs = append(attrs, slog.String("error_type", f.ErrorType))
	}
	if f.PanicRecovered {
		attrs = append(attrs, slog.Bool("panic_recovered", true))
	}
	if f.Reason != "" {
		attrs = append(attrs, slog.String("reason", f.Reason))
	}
	// started_at_utc, not started_at: docs/observability.md freezes the naming
	// rule "_utc for an instant, _ms for a duration" for every audit field, and
	// that document is the customer-facing authority on this schema.
	if !f.StartedAt.IsZero() {
		attrs = append(attrs,
			slog.String("started_at_utc", f.StartedAt.UTC().Format(time.RFC3339Nano)),
			slog.Int64("duration_ms", f.Duration.Milliseconds()))
	}

	return Event{attrs: attrs, level: levelByType[f.Type], typ: f.Type}, nil
}

// validate enforces one row of the field-applicability table. It checks every
// field and reports the first violation; the table, not the call site, is the
// authority on what a record type may carry.
func validate(f Fields, r rules) error {
	// Outcome: presence per the table, and membership in the type's own
	// allowed set (a broker auth retry has no caller, so it cannot be
	// cancelled).
	if err := checkPresence("outcome", f.Outcome != "", r.outcome, f.Type); err != nil {
		return err
	}
	if f.Outcome != "" {
		allowed := r.allowedOutcomes
		if allowed == nil {
			allowed = allOutcomes
		}
		if _, ok := allowed[f.Outcome]; !ok {
			return fmt.Errorf("audit: outcome %q is not valid on audit_event_type %q; must be one of %v",
				f.Outcome, f.Type, sortedOutcomes(allowed))
		}
	}

	// error_type: on an operation record it is required exactly when the
	// outcome is an error and forbidden otherwise, so a success can never
	// carry a cause and a failure can never omit one.
	errorTypeRule := r.errorType
	if errorTypeRule == fieldOnErrorOnly {
		if f.Outcome == OutcomeError {
			errorTypeRule = fieldRequired
		} else {
			errorTypeRule = fieldForbidden
		}
	}
	if err := checkPresence("error_type", f.ErrorType != "", errorTypeRule, f.Type); err != nil {
		return err
	}
	if f.ErrorType != "" {
		if _, ok := errorTypeVocabulary[f.ErrorType]; !ok {
			return fmt.Errorf("audit: error_type %q is outside the closed vocabulary %v", f.ErrorType, ErrorTypes())
		}
	}

	// panic_recovered is a claim about the cause, so it must agree with it.
	if f.PanicRecovered && (f.Outcome != OutcomeError || f.ErrorType != "panic") {
		return fmt.Errorf("audit: panic_recovered requires outcome=%q and error_type=\"panic\", got outcome=%q error_type=%q",
			OutcomeError, f.Outcome, f.ErrorType)
	}

	// reason: presence per the table, and membership in THIS record type's
	// vocabulary. The three vocabularies are checked separately so an
	// authentication reason cannot appear on an authorization record, nor a
	// hop-1 reason on a hop-2 record.
	if err := checkPresence("reason", f.Reason != "", r.reason, f.Type); err != nil {
		return err
	}
	if f.Reason != "" {
		if _, ok := r.reasonVocab[f.Reason]; !ok {
			return fmt.Errorf("audit: reason %q is not valid on audit_event_type %q; must be one of %v",
				f.Reason, f.Type, sortedKeys(r.reasonVocab))
		}
	}

	if err := checkPresence("tool", f.Tool != "", r.tool, f.Type); err != nil {
		return err
	}
	if err := checkPresence("arguments_hash", f.ArgumentsHash != "", r.argumentsHash, f.Type); err != nil {
		return err
	}
	if err := checkPresence("broker", f.Broker != "", r.broker, f.Type); err != nil {
		return err
	}
	// started_at gates the timing pair; duration_ms is meaningless without it,
	// so both are emitted together or not at all.
	if err := checkPresence("started_at", !f.StartedAt.IsZero(), r.timing, f.Type); err != nil {
		return err
	}
	if f.StartedAt.IsZero() && f.Duration != 0 {
		return fmt.Errorf("audit: duration_ms set without started_at on audit_event_type %q", f.Type)
	}
	if f.Duration < 0 {
		return fmt.Errorf("audit: duration must not be negative, got %s", f.Duration)
	}
	return nil
}

// checkPresence reports whether a field's presence matches its applicability.
// fieldOnErrorOnly is resolved by the caller before it gets here.
func checkPresence(field string, present bool, a applicability, t EventType) error {
	switch a {
	case fieldRequired:
		if !present {
			return fmt.Errorf("audit: %s is required on audit_event_type %q", field, t)
		}
	case fieldForbidden:
		if present {
			return fmt.Errorf("audit: %s must not be set on audit_event_type %q", field, t)
		}
	}
	return nil
}

// sortedKeys returns a set's members sorted, for deterministic error messages.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedOutcomes is sortedKeys for the Outcome-keyed sets.
func sortedOutcomes(m map[Outcome]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
