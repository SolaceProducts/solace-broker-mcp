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
	if method == "PATCH" {
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
	if strings.HasPrefix(opID, "update") {
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
	modes := []struct{ opID, method string }{{"createMsgVpnKafkaReceiver", "POST"}, {"updateMsgVpnKafkaReceiver", "PATCH"}}

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

// The enabled/scheme rules, for both modes.
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
	cases := []struct {
		name   string
		create bool
		config map[string]any
		reject bool
	}{
		// create
		{"create: none + enabled", true, cfg("none", true), false},
		{"create: scheme omitted + enabled", true, cfg(nil, true), false},
		{"create: basic + enabled omitted", true, cfg("basic", nil), false},
		{"create: basic + enabled false", true, cfg("basic", false), false},
		{"create: basic + enabled true", true, cfg("basic", true), true},
		{"create: scram + enabled true", true, cfg("scram", true), true},
		// update
		{"update: enable only", false, cfg(nil, true), false},
		{"update: disable only", false, cfg(nil, false), false},
		{"update: scheme none + enabled true", false, cfg("none", true), false},
		{"update: scheme none, enabled omitted", false, cfg("none", nil), false},
		{"update: basic + enabled omitted", false, cfg("basic", nil), true},
		{"update: basic + enabled true", false, cfg("basic", true), true},
		{"update: basic + enabled false", false, cfg("basic", false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opID, method := "updateMsgVpnKafkaReceiver", "PATCH"
			if tc.create {
				opID, method = "createMsgVpnKafkaReceiver", "POST"
			}
			rec, err := runBridge(t, opID, method, bridgeBodyFields(), kafkaChecks, map[string]any{
				"msgVpnName": "v", "kafkaReceiverName": "r", "kafkaReceiverConfig": tc.config,
			})
			if tc.reject {
				assertRejectedBeforeSEMP(t, rec, err, "enabled:false")
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
		{"unknown name", "config/createMsgVpnKafkaReceiver", "[noSuchCheck]", `unknown bodyCheck "noSuchCheck"`},
		{"on a delete", "config/deleteMsgVpnKafkaReceiver", "[rejectKafkaSecrets]", "bodyChecks require a config/create* or config/update*"},
		{"on a monitor op", "monitor/getMsgVpnKafkaReceiver", "[rejectKafkaSecrets]", "bodyChecks require a config/create* or config/update*"},
		{"on an action op", "action/doMsgVpnQueueDeleteMsgs", "[rejectKafkaSecrets]", "bodyChecks require a config/create* or config/update*"},
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
		"config/createMsgVpnKafkaReceiver", "config/updateMsgVpnKafkaReceiver",
		"config/createMsgVpnKafkaSender", "config/updateMsgVpnKafkaSender",
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

// temporaryWriteOnlyExemptions are tools with unguarded write-only secrets that a
// follow-up story is closing. Each entry must name its ticket; the story that
// closes it deletes the entry. Remove nothing else.
var temporaryWriteOnlyExemptions = map[string]string{
	"create-message-vpn": "SOL-156210: replicationBridgeAuthentication* secrets",
	"update-message-vpn": "SOL-156210: replicationBridgeAuthentication* secrets",
}

// Every write-only attribute of every shipped config create/update step must be
// covered by that step's body checks, listed as a known non-secret, or belong to
// a tool with a ticketed temporary exemption. And every secret a check forbids
// must still be write-only in the spec (no stale entries).
func TestBodyChecks_EveryWriteOnlyFieldIsGuarded(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	exemptionUsed := map[string]bool{}
	for _, tool := range tools {
		for _, step := range tool.Steps {
			op, ok := ops[step.Operation]
			if !ok {
				continue
			}
			if _, isWrite := bodyModeFor(op.ID); !isWrite || !strings.HasPrefix(step.Operation, "config/") {
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
				if _, ok := temporaryWriteOnlyExemptions[tool.Name]; ok {
					exemptionUsed[tool.Name] = true
					continue
				}
				t.Errorf("tool %q step %q (%s): write-only attribute %q is not guarded by any bodyChecks entry. Add a check that rejects it, or, if it is not a secret, add it to knownNonSecretWriteOnly with a reason.", tool.Name, step.ID, op.ID, f)
			}
		}
	}
	for name, why := range temporaryWriteOnlyExemptions {
		if !exemptionUsed[name] {
			t.Errorf("temporary exemption for %q (%s) is no longer needed; delete it", name, why)
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
