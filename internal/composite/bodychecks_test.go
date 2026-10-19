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

package composite

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// secretSentinel is a recognisable secret value. No error text, and nothing a
// check returns, may ever contain it.
const secretSentinel = "SECRET-SENTINEL-d41d8cd9"

// bridgeBodyFields is a body-field set shaped like MsgVpnKafkaReceiver's: the
// identity, the scheme/enabled pair the rules look at, one non-secret and every
// secret.
func bridgeBodyFields() map[string]bool {
	f := map[string]bool{
		"msgVpnName": true, "kafkaReceiverName": true, "authenticationScheme": true,
		"enabled": true, "authenticationBasicUsername": true,
	}
	for _, s := range kafkaSecretFields {
		f[s] = true
	}
	return f
}

func bridgeOps(opID, method string, bodyFields map[string]bool) map[string]*sempv2.Operation {
	params := []sempv2.Parameter{
		{Name: "msgVpnName", In: "path", Type: "string", Required: true},
		{Name: "body", In: "body", Type: "object", Required: true},
	}
	path := "/SEMP/v2/__private_config__/msgVpns/{msgVpnName}/kafkaReceivers"
	if method == "PATCH" || method == "PUT" {
		params = []sempv2.Parameter{
			{Name: "msgVpnName", In: "path", Type: "string", Required: true},
			{Name: "kafkaReceiverName", In: "path", Type: "string", Required: true},
			{Name: "body", In: "body", Type: "object", Required: true},
		}
		path += "/{kafkaReceiverName}"
	}
	return map[string]*sempv2.Operation{
		"config/" + opID: {ID: opID, Method: method, Path: path, Parameters: params, BodyFields: bodyFields},
	}
}

func bridgeTool(opID string, checks []string, followPages bool) CompositeTool {
	args := map[string]string{"msgVpnName": "{{.Params.msgVpnName}}"}
	if strings.HasPrefix(opID, "update") || strings.HasPrefix(opID, "replace") {
		args["kafkaReceiverName"] = "{{.Params.kafkaReceiverName}}"
	}
	return CompositeTool{
		Name: "t",
		Steps: []Step{{
			ID: "s", Operation: "config/" + opID, Args: args,
			BodyChecks: checks, FollowPages: followPages,
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
}

func runBridge(t *testing.T, opID, method string, bodyFields map[string]bool, checks []string, params map[string]any) ([]callRecord, error) {
	t.Helper()
	var recorded []callRecord
	var mu sync.Mutex
	client := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}
	_, err := NewCompositeExecutor(bridgeOps(opID, method, bodyFields)).Execute(
		context.Background(), bridgeTool(opID, checks, false), client, params)
	return recorded, err
}

var kafkaChecks = []string{"rejectKafkaSecrets", "createDisabledWhenAuthenticated"}

func assertRejectedBeforeSEMP(t *testing.T, recorded []callRecord, err error, wantInMsg ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the body to be rejected, got nil error")
	}
	var bodyErr *RequestBodyError
	if !errors.As(err, &bodyErr) {
		t.Errorf("error = %T, want a *RequestBodyError in the chain", err)
	}
	if len(recorded) != 0 {
		t.Errorf("expected no SEMP calls, got %d", len(recorded))
	}
	if strings.Contains(err.Error(), secretSentinel) {
		t.Errorf("error text leaked the secret value: %v", err)
	}
	for _, w := range wantInMsg {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q should contain %q", err, w)
		}
	}
}

// Every secret, in every param shape that reaches the body, on create and update.
func TestBodyChecks_RejectsEverySecretInEveryShape(t *testing.T) {
	shapes := map[string]func(secret string) map[string]any{
		"inside the config object": func(s string) map[string]any {
			return map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r",
				"kafkaReceiverConfig": map[string]any{s: secretSentinel}}
		},
		"top-level scalar param": func(s string) map[string]any {
			return map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r", s: secretSentinel}
		},
		"under a wrongly named object": func(s string) map[string]any {
			return map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r",
				"config": map[string]any{s: secretSentinel}}
		},
	}
	modes := []struct{ opID, method string }{
		{"createMsgVpnKafkaReceiver", "POST"}, {"updateMsgVpnKafkaReceiver", "PATCH"}, {"replaceMsgVpnKafkaReceiver", "PUT"},
	}

	for _, m := range modes {
		for shape, build := range shapes {
			for _, secret := range kafkaSecretFields {
				t.Run(m.opID+"/"+shape+"/"+secret, func(t *testing.T) {
					rec, err := runBridge(t, m.opID, m.method, bridgeBodyFields(), kafkaChecks, build(secret))
					assertRejectedBeforeSEMP(t, rec, err, `"`+secret+`"`, "rotate")
				})
			}
		}
	}
}

func TestBodyChecks_SecretMatchIsCaseInsensitiveAndNamesTheCanonicalField(t *testing.T) {
	// BodyFields nil: the unknown-field check is skipped, so only the body check
	// stands between a case-variant secret and the broker.
	rec, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", nil, kafkaChecks, map[string]any{
		"msgVpnName": "v", "kafkaReceiverName": "r",
		"kafkaReceiverConfig": map[string]any{"AUTHENTICATIONBASICPASSWORD": secretSentinel},
	})
	assertRejectedBeforeSEMP(t, rec, err, `"authenticationBasicPassword"`)
	if strings.Contains(err.Error(), "AUTHENTICATIONBASICPASSWORD") {
		t.Errorf("error should name the canonical field, not echo the caller's key: %v", err)
	}
}

// The body checks run before the unknown-field check, so a miscased secret key
// gets the "set it out of band, rotate it" guidance rather than a bare "not a
// known attribute", even when the operation's body fields are known.
func TestBodyChecks_MiscasedSecretGetsGuidanceWhenBodyFieldsAreKnown(t *testing.T) {
	rec, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", bridgeBodyFields(), kafkaChecks, map[string]any{
		"msgVpnName": "v", "kafkaReceiverName": "r",
		"kafkaReceiverConfig": map[string]any{"AuthenticationBasicPassword": secretSentinel},
	})
	assertRejectedBeforeSEMP(t, rec, err, `"authenticationBasicPassword"`, "rotate")
	if strings.Contains(err.Error(), "not a known attribute") {
		t.Errorf("a miscased secret must get the secret guidance, not the unknown-field message: %v", err)
	}
}

func TestBodyChecks_RunWhenBodyFieldsUnknown(t *testing.T) {
	rec, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", nil, kafkaChecks, map[string]any{
		"msgVpnName": "v", "kafkaReceiverName": "r",
		"kafkaReceiverConfig": map[string]any{"authenticationScramPassword": secretSentinel},
	})
	assertRejectedBeforeSEMP(t, rec, err, `"authenticationScramPassword"`)
}

func TestBodyChecks_RunOnPaginatedPath(t *testing.T) {
	var recorded []callRecord
	var mu sync.Mutex
	client := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}
	_, err := NewCompositeExecutor(bridgeOps("createMsgVpnKafkaReceiver", "POST", bridgeBodyFields())).Execute(
		context.Background(), bridgeTool("createMsgVpnKafkaReceiver", kafkaChecks, true), client, map[string]any{
			"msgVpnName": "v", "kafkaReceiverName": "r",
			"kafkaReceiverConfig": map[string]any{"authenticationBasicPassword": secretSentinel},
		})
	assertRejectedBeforeSEMP(t, recorded, err, `"authenticationBasicPassword"`)
}

func TestBodyChecks_NonSecretBodiesPassAndReachTheClient(t *testing.T) {
	rec, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", bridgeBodyFields(), kafkaChecks, map[string]any{
		"msgVpnName": "v", "kafkaReceiverName": "r",
		"kafkaReceiverConfig": map[string]any{
			"authenticationScheme": "basic", "authenticationBasicUsername": "u", "enabled": false,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rec) != 1 {
		t.Fatalf("calls = %d, want 1", len(rec))
	}
	body, _ := rec[0].args["body"].(map[string]any)
	if body["authenticationBasicUsername"] != "u" || body["kafkaReceiverName"] != "r" {
		t.Errorf("body = %v, want the fields to reach the client unchanged", body)
	}
}

// The enabled/scheme rules, for all three modes.
func TestBodyChecks_AuthenticatedBridgeEnabledRules(t *testing.T) {
	cfg := func(scheme any, enabled any) map[string]any {
		m := map[string]any{}
		if scheme != nil {
			m["authenticationScheme"] = scheme
		}
		if enabled != nil {
			m["enabled"] = enabled
		}
		return m
	}
	const (
		create  = "createMsgVpnKafkaReceiver"
		update  = "updateMsgVpnKafkaReceiver"
		replace = "replaceMsgVpnKafkaReceiver"
	)
	methodFor := map[string]string{create: "POST", update: "PATCH", replace: "PUT"}
	cases := []struct {
		name   string
		opID   string
		config map[string]any
		reject bool
		want   string // substring of the rejection, when reject
	}{
		// create
		{"create: none + enabled", create, cfg("none", true), false, ""},
		{"create: scheme omitted + enabled", create, cfg(nil, true), false, ""},
		{"create: basic + enabled omitted", create, cfg("basic", nil), false, ""},
		{"create: basic + enabled false", create, cfg("basic", false), false, ""},
		{"create: basic + enabled true", create, cfg("basic", true), true, "enabled:false"},
		{"create: scram + enabled true", create, cfg("scram", true), true, "enabled:false"},
		// replace: the body is the whole object, and an omitted enabled means false
		{"replace: none + enabled", replace, cfg("none", true), false, ""},
		{"replace: scheme omitted + enabled", replace, cfg(nil, true), false, ""},
		{"replace: basic + enabled omitted", replace, cfg("basic", nil), false, ""},
		{"replace: basic + enabled false", replace, cfg("basic", false), false, ""},
		{"replace: basic + enabled true", replace, cfg("basic", true), true, "enabled:false"},
		// update
		{"update: enable only", update, cfg(nil, true), false, ""},
		{"update: disable only", update, cfg(nil, false), false, ""},
		{"update: scheme none + enabled true", update, cfg("none", true), false, ""},
		{"update: scheme none, enabled omitted", update, cfg("none", nil), false, ""},
		{"update: basic + enabled omitted", update, cfg("basic", nil), true, "enabled:false"},
		{"update: basic + enabled true", update, cfg("basic", true), true, "enabled:false"},
		{"update: basic + enabled false", update, cfg("basic", false), false, ""},
		// A wrong-typed value is rejected as such, never treated as absent: an
		// update with "enabled":"false" must not be told to "set enabled:false".
		{"update: enabled as a string", update, cfg("basic", "false"), true, "must be true or false"},
		{"create: enabled as a string", create, cfg("basic", "true"), true, "must be true or false"},
		{"create: enabled as a number", create, cfg("basic", 1), true, "must be true or false"},
		{"create: scheme as a number", create, cfg(5, true), true, "must be a string"},
		{"update: scheme as an object", update, cfg(map[string]any{"x": "y"}, false), true, "must be a string"},
		// With no scheme in the body the enabled value is the broker's to type-check.
		{"update: no scheme, enabled as a string", update, cfg(nil, "yes"), false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := runBridge(t, tc.opID, methodFor[tc.opID], bridgeBodyFields(), kafkaChecks, map[string]any{
				"msgVpnName": "v", "kafkaReceiverName": "r", "kafkaReceiverConfig": tc.config,
			})
			if tc.reject {
				assertRejectedBeforeSEMP(t, rec, err, tc.want)
				return
			}
			if err != nil {
				t.Fatalf("unexpected rejection: %v", err)
			}
			if len(rec) != 1 {
				t.Errorf("calls = %d, want 1", len(rec))
			}
		})
	}
}

func TestBodyChecks_StepWithoutChecksIsUnaffected(t *testing.T) {
	rec, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", bridgeBodyFields(), nil, map[string]any{
		"msgVpnName": "v", "kafkaReceiverName": "r",
		"kafkaReceiverConfig": map[string]any{"authenticationBasicPassword": "x", "authenticationScheme": "basic", "enabled": true},
	})
	if err != nil || len(rec) != 1 {
		t.Fatalf("a step with no bodyChecks must behave exactly as before; err=%v calls=%d", err, len(rec))
	}
}

func TestBodyChecks_MultipleSecretsReportDeterministically(t *testing.T) {
	var first string
	for i := 0; i < 20; i++ {
		_, err := runBridge(t, "createMsgVpnKafkaReceiver", "POST", nil, kafkaChecks, map[string]any{
			"msgVpnName": "v", "kafkaReceiverName": "r",
			"kafkaReceiverConfig": map[string]any{
				"authenticationScramPassword": "a", "authenticationBasicPassword": "b", "authenticationOauthClientSecret": "c",
			},
		})
		if err == nil {
			t.Fatal("expected rejection")
		}
		if i == 0 {
			first = err.Error()
		} else if err.Error() != first {
			t.Fatalf("error text varied between runs (map order leak): %q vs %q", first, err.Error())
		}
	}
}

func TestRunBodyChecks_ProgrammingErrorsAreReportedNotSkipped(t *testing.T) {
	if err := runBodyChecks([]string{"noSuchCheck"}, "createX", map[string]any{}); err == nil {
		t.Error("an unknown check name must fail, not be skipped")
	}
	if err := runBodyChecks([]string{"rejectKafkaSecrets"}, "deleteX", map[string]any{}); err == nil {
		t.Error("a check on a non create/update operation must fail, not be skipped")
	}
	if err := runBodyChecks(nil, "deleteX", map[string]any{}); err != nil {
		t.Errorf("no checks configured must be a no-op, got %v", err)
	}
}

// --- loader validation ---

func loadBodyChecksYAML(t *testing.T, yaml string) error {
	t.Helper()
	_, err := LoadTools(fstest.MapFS{"t.yaml": {Data: []byte(yaml)}}, "t.yaml")
	return err
}

func toolYAML(operation, checks string) string {
	return "tools:\n  - name: t\n    description: d\n    steps:\n      - id: s\n        operation: " + operation +
		"\n        bodyChecks: " + checks + "\n    result:\n      strategy: collect\n"
}

func TestLoader_BodyChecksValidation(t *testing.T) {
	cases := []struct {
		name, operation, checks, wantErr string
	}{
		{"valid create", "config/createMsgVpnKafkaReceiver", "[rejectKafkaSecrets]", ""},
		{"valid update", "config/updateMsgVpnKafkaReceiver", "[rejectKafkaSecrets, createDisabledWhenAuthenticated]", ""},
		{"valid replace", "config/replaceMsgVpnKafkaReceiver", "[rejectKafkaSecrets, createDisabledWhenAuthenticated]", ""},
		{"unknown name", "config/createMsgVpnKafkaReceiver", "[noSuchCheck]", `unknown bodyCheck "noSuchCheck"`},
		{"on a delete", "config/deleteMsgVpnKafkaReceiver", "[rejectKafkaSecrets]", "bodyChecks require a config/create*, config/update* or config/replace*"},
		{"on a monitor op", "monitor/getMsgVpnKafkaReceiver", "[rejectKafkaSecrets]", "bodyChecks require a config/create*, config/update* or config/replace*"},
		{"on an action op", "action/doMsgVpnQueueDeleteMsgs", "[rejectKafkaSecrets]", "bodyChecks require a config/create*, config/update* or config/replace*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := loadBodyChecksYAML(t, toolYAML(tc.operation, tc.checks))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// --- schema-driven drift tests ---

func writeOnlySet(op *sempv2.Operation) []string {
	var out []string
	for f := range op.WriteOnlyFields {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// The hand-kept Kafka secret list must equal the spec's own write-only set for
// all four create/update operations. A spec bump that adds, removes or renames a
// write-only attribute fails here instead of silently leaving a secret unguarded.
func TestBodyChecks_KafkaSecretFieldsMatchSpec(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	want := append([]string(nil), kafkaSecretFields...)
	sort.Strings(want)
	for _, id := range []string{
		"config/createMsgVpnKafkaReceiver", "config/updateMsgVpnKafkaReceiver", "config/replaceMsgVpnKafkaReceiver",
		"config/createMsgVpnKafkaSender", "config/updateMsgVpnKafkaSender", "config/replaceMsgVpnKafkaSender",
	} {
		op, ok := ops[id]
		if !ok {
			t.Fatalf("operation %s missing from the embedded spec", id)
		}
		got := writeOnlySet(op)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s write-only attributes = %v\nkafkaSecretFields = %v\nUpdate kafkaSecretFields (and review tool descriptions) to match the spec.", id, got, want)
		}
	}
}

// Every Kafka secret must also be masked by the audit-hash redaction, so the
// two protections cannot drift apart.
func TestBodyChecks_KafkaSecretsAreRedactedInAuditHash(t *testing.T) {
	for _, s := range kafkaSecretFields {
		got := audit.RedactSensitive(map[string]any{s: secretSentinel})
		if got[s] == secretSentinel {
			t.Errorf("audit.RedactSensitive does not mask %q; extend its key lists", s)
		}
	}
}

// knownNonSecretWriteOnly lists attributes the spec marks "absent from a GET"
// that are NOT secrets and so need no check. Keep this list explicit: a new
// write-only attribute must be a conscious decision (guard it, or list it here).
var knownNonSecretWriteOnly = map[string]string{
	"replicationEnabledQueueBehavior": "MsgVpn: a one-shot behaviour flag, not a credential",
}

// exemptionKey names one unguarded write-only field of one tool.
type exemptionKey struct{ tool, field string }

// temporaryWriteOnlyExemptions are unguarded write-only fields that a follow-up
// story is closing, keyed by (tool, field) so an entry hides exactly the field it
// names and nothing else on that tool. Each entry must name its ticket; the story
// that closes it deletes the entry. The mechanism is kept (and the "no longer
// needed" assertion below with it) so a future spec bump that adds a write-only
// attribute to a shipped tool can be staged behind a ticketed exemption instead of
// a red build.
var temporaryWriteOnlyExemptions = map[exemptionKey]string{}

// Every write-only attribute of every shipped config create, update or replace
// step must be covered by that step's body checks, listed as a known non-secret, or
// carry a ticketed temporary exemption for exactly that (tool, field). And every
// secret a check forbids must still be write-only in the spec (no stale entries).
//
// "Unknown" must never pass as "none": a step whose operation is missing from the
// catalog, or whose body schema did not resolve (nil WriteOnlyFields), fails here.
func TestBodyChecks_EveryWriteOnlyFieldIsGuarded(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	exemptionUsed := map[exemptionKey]bool{}
	for _, tool := range tools {
		for _, step := range tool.Steps {
			opID, isConfig := strings.CutPrefix(step.Operation, "config/")
			if !isConfig {
				continue
			}
			if _, isWrite := bodyModeFor(opID); !isWrite {
				continue // a delete takes no body
			}
			op, ok := ops[step.Operation]
			if !ok {
				t.Errorf("tool %q step %q: operation %q is not in the embedded catalog, so its write-only fields cannot be checked", tool.Name, step.ID, step.Operation)
				continue
			}
			if op.WriteOnlyFields == nil {
				t.Errorf("tool %q step %q (%s): the body schema did not resolve, so its write-only fields are unknown; unknown must not pass as none", tool.Name, step.ID, op.ID)
				continue
			}
			covered := map[string]bool{}
			for _, name := range step.BodyChecks {
				for _, f := range bodyChecks[name].secretFields {
					covered[strings.ToLower(f)] = true
					if !op.WriteOnlyFields[f] {
						t.Errorf("tool %q step %q: check %q forbids %q, which is no longer a write-only attribute of %s — stale entry", tool.Name, step.ID, name, f, op.ID)
					}
				}
			}
			for f := range op.WriteOnlyFields {
				if covered[strings.ToLower(f)] {
					continue
				}
				if _, ok := knownNonSecretWriteOnly[f]; ok {
					continue
				}
				key := exemptionKey{tool.Name, f}
				if _, ok := temporaryWriteOnlyExemptions[key]; ok {
					exemptionUsed[key] = true
					continue
				}
				t.Errorf("tool %q step %q (%s): write-only attribute %q is not guarded by any bodyChecks entry. Add a check that rejects it, or, if it is not a secret, add it to knownNonSecretWriteOnly with a reason.", tool.Name, step.ID, op.ID, f)
			}
		}
	}
	for key, ticket := range temporaryWriteOnlyExemptions {
		if !exemptionUsed[key] {
			t.Errorf("temporary exemption for %s.%s (%s) is no longer needed; delete it", key.tool, key.field, ticket)
		}
	}
}

// Replace (PUT) operations take the same body as a create, including every
// write-only secret, so a future tool built on one must be guardable. This pins
// that the loader accepts bodyChecks on a replace step and the drift test above
// walks it: a replace step carrying write-only fields with no check fails there.
func TestBodyChecks_ReplaceStepsAreCoveredByTheDriftWalk(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	for _, id := range []string{"config/replaceMsgVpnKafkaReceiver", "config/replaceMsgVpnKafkaSender"} {
		op, ok := ops[id]
		if !ok {
			t.Fatalf("%s missing from the embedded spec", id)
		}
		if len(op.WriteOnlyFields) == 0 {
			t.Errorf("%s should carry write-only fields; the replace guard would be moot", id)
		}
		if _, ok := bodyModeFor(op.ID); !ok {
			t.Errorf("bodyModeFor(%q) does not recognise a replace operation", op.ID)
		}
	}
}

// A body check on an operation that takes no body must fail, not be skipped: a
// silently skipped security check is worse than a failed call.
func TestBodyChecks_ConfiguredOnOperationWithoutBodyFails(t *testing.T) {
	ops := map[string]*sempv2.Operation{
		"config/createNoBody": {
			ID: "createNoBody", Method: "POST", Path: "/SEMP/v2/__private_config__/x",
			Parameters: []sempv2.Parameter{{Name: "msgVpnName", In: "path", Type: "string", Required: true}},
		},
	}
	tool := CompositeTool{
		Name:   "t",
		Steps:  []Step{{ID: "s", Operation: "config/createNoBody", Args: map[string]string{"msgVpnName": "v"}, BodyChecks: kafkaChecks}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	var recorded []callRecord
	var mu sync.Mutex
	client := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}
	_, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, client, map[string]any{"msgVpnName": "v"})
	if err == nil || !strings.Contains(err.Error(), "takes no request body") {
		t.Fatalf("error = %v, want a refusal because the body checks have nothing to inspect", err)
	}
	if len(recorded) != 0 {
		t.Errorf("expected no SEMP calls, got %d", len(recorded))
	}
}

// The hand-kept MsgVpn secret list must equal the spec's write-only set minus the
// explicitly allowlisted non-secrets, for both operations create-/update-message-vpn use.
func TestBodyChecks_MsgVpnSecretFieldsMatchSpec(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	want := append([]string(nil), msgVpnSecretFields...)
	sort.Strings(want)
	for _, id := range []string{"config/createMsgVpn", "config/updateMsgVpn"} {
		op, ok := ops[id]
		if !ok {
			t.Fatalf("operation %s missing from the embedded spec", id)
		}
		var got []string
		for _, f := range writeOnlySet(op) {
			if _, nonSecret := knownNonSecretWriteOnly[f]; !nonSecret {
				got = append(got, f)
			}
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s write-only secrets = %v\nmsgVpnSecretFields = %v", id, got, want)
		}
	}
}

func TestBodyChecks_MsgVpnSecretsAreRedactedInAuditHash(t *testing.T) {
	for _, s := range msgVpnSecretFields {
		if got := audit.RedactSensitive(map[string]any{s: secretSentinel}); got[s] == secretSentinel {
			t.Errorf("audit.RedactSensitive does not mask %q; extend its key lists", s)
		}
	}
}

// The real create-/update-message-vpn definitions, against the real embedded spec:
// a replication-bridge secret never reaches the client, in any shape, and an
// ordinary msgVpnConfig still does.
func TestRealMessageVPNTools_RejectReplicationBridgeSecrets(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	byName := map[string]CompositeTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}

	for _, name := range []string{"create-message-vpn", "update-message-vpn"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("%s missing from the embedded catalog", name)
		}
		for _, secret := range msgVpnSecretFields {
			for shape, params := range map[string]map[string]any{
				"config object": {"msgVpnName": "v", "msgVpnConfig": map[string]any{secret: secretSentinel}},
				"top-level":     {"msgVpnName": "v", "msgVpnConfig": map[string]any{}, secret: secretSentinel},
			} {
				t.Run(name+"/"+shape+"/"+secret, func(t *testing.T) {
					var recorded []callRecord
					var mu sync.Mutex
					client := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}
					_, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, client, params)
					assertRejectedBeforeSEMP(t, recorded, err, `"`+secret+`"`)
				})
			}
		}

		t.Run(name+"/ordinary config still goes through", func(t *testing.T) {
			var recorded []callRecord
			var mu sync.Mutex
			client := &argCapturingClient{inner: newMockClient(), recorded: &recorded, mu: &mu}
			_, err := NewCompositeExecutor(ops).Execute(context.Background(), tool, client, map[string]any{
				"msgVpnName":   "v",
				"msgVpnConfig": map[string]any{"maxConnectionCount": float64(10), "replicationEnabledQueueBehavior": "fail-on-all-errors"},
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(recorded) != 1 {
				t.Errorf("calls = %d, want 1", len(recorded))
			}
		})
	}
}

// A caller-supplied authenticationScheme is not a secret field, but it is free
// text the caller controls: a credential pasted into the wrong field must not be
// echoed back to the agent and into the log detail by a rejection message.
func TestBodyChecks_SchemeRejectionNeverEchoesTheSchemeValue(t *testing.T) {
	cases := []struct {
		name         string
		opID, method string
		config       map[string]any
	}{
		{"create enabled", "createMsgVpnKafkaReceiver", "POST", map[string]any{"authenticationScheme": secretSentinel, "enabled": true}},
		{"replace enabled", "replaceMsgVpnKafkaReceiver", "PUT", map[string]any{"authenticationScheme": secretSentinel, "enabled": true}},
		{"update without enabled:false", "updateMsgVpnKafkaReceiver", "PATCH", map[string]any{"authenticationScheme": secretSentinel}},
		{"update enabled", "updateMsgVpnKafkaReceiver", "PATCH", map[string]any{"authenticationScheme": secretSentinel, "enabled": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, err := runBridge(t, tc.opID, tc.method, bridgeBodyFields(), kafkaChecks, map[string]any{
				"msgVpnName": "v", "kafkaReceiverName": "r", "kafkaReceiverConfig": tc.config,
			})
			assertRejectedBeforeSEMP(t, rec, err, "enabled:false")
		})
	}
}
