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

package sempv2_test

import (
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

func TestParseSpecs_OperationCount(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	if len(ops) == 0 {
		t.Fatal("ParseSpecs() returned zero operations")
	}

	t.Logf("Parsed %d operations from embedded specs", len(ops))
}

func TestParseSpecs_OperationFields(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	op, ok := ops["monitor/getMsgVpnQueue"]
	if !ok {
		t.Fatal("expected operation monitor/getMsgVpnQueue not found")
	}

	if op.Method != "GET" {
		t.Errorf("monitor/getMsgVpnQueue.Method = %q, want %q", op.Method, "GET")
	}

	if !strings.Contains(op.Path, "/msgVpns/{msgVpnName}/queues/{queueName}") {
		t.Errorf("monitor/getMsgVpnQueue.Path = %q, want it to contain /msgVpns/{msgVpnName}/queues/{queueName}", op.Path)
	}

	if !strings.HasPrefix(op.Path, "/SEMP/v2/__private_monitor__/") {
		t.Errorf("monitor/getMsgVpnQueue.Path = %q, expected it to start with /SEMP/v2/__private_monitor__/", op.Path)
	}

	if len(op.Parameters) == 0 {
		t.Error("monitor/getMsgVpnQueue should have parameters")
	}

	paramNames := make(map[string]bool)
	for _, p := range op.Parameters {
		paramNames[p.Name] = true
	}
	for _, expected := range []string{"msgVpnName", "queueName"} {
		if !paramNames[expected] {
			t.Errorf("monitor/getMsgVpnQueue missing expected parameter %q", expected)
		}
	}

	// No ghost parameters — every parameter must have a non-empty Name.
	for i, p := range op.Parameters {
		if p.Name == "" {
			t.Errorf("monitor/getMsgVpnQueue param[%d] has empty Name (unresolved $ref ghost)", i)
		}
	}
}

func TestParseSpecs_BodyFields(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// A config write op resolves its body schema to a concrete field set.
	create, ok := ops["config/createMsgVpn"]
	if !ok {
		t.Fatal("expected operation config/createMsgVpn not found")
	}
	if create.BodyFields == nil {
		t.Fatal("config/createMsgVpn.BodyFields should be populated from the MsgVpn schema")
	}
	for _, field := range []string{"msgVpnName", "enabled", "maxConnectionCount"} {
		if !create.BodyFields[field] {
			t.Errorf("config/createMsgVpn.BodyFields missing expected field %q", field)
		}
	}

	// SchemaVersion carries the spec's info.version for use in unknown-attribute errors.
	if create.SchemaVersion == "" {
		t.Error("config/createMsgVpn.SchemaVersion should be populated from the spec's info.version")
	}

	// A GET op has no request body, so BodyFields is nil (validation skipped).
	get, ok := ops["monitor/getMsgVpnQueue"]
	if !ok {
		t.Fatal("expected operation monitor/getMsgVpnQueue not found")
	}
	if get.BodyFields != nil {
		t.Errorf("monitor/getMsgVpnQueue.BodyFields = %v, want nil for a bodyless op", get.BodyFields)
	}
}

// TestParseSpecs_ResponseFields covers extractResponseFields — SOL-150785's
// foundation for schema-generated output validation. Response schemas were
// parsed by the underlying library but never surfaced before this; these
// assertions are load-bearing for that new surface, not incidental.
func TestParseSpecs_ResponseFields(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// Single-object GET: data is a $ref straight to the item schema (MsgVpnQueue).
	get, ok := ops["monitor/getMsgVpnQueue"]
	if !ok {
		t.Fatal("expected operation monitor/getMsgVpnQueue not found")
	}
	if get.ResponseFields == nil {
		t.Fatal("monitor/getMsgVpnQueue.ResponseFields should be populated from the MsgVpnQueue schema")
	}
	wantTypes := map[string]string{
		"bindCount":     "integer",
		"msgSpoolUsage": "integer",
		"accessType":    "string",
		"durable":       "boolean",
	}
	for field, wantType := range wantTypes {
		gotType, present := get.ResponseFields[field]
		if !present {
			t.Errorf("monitor/getMsgVpnQueue.ResponseFields missing expected field %q", field)
			continue
		}
		if gotType != wantType {
			t.Errorf("monitor/getMsgVpnQueue.ResponseFields[%q] = %q, want %q", field, gotType, wantType)
		}
	}

	// List GET: data is an array whose items $ref the same item schema. Must
	// resolve to the identical field set as the single-object GET above —
	// this is the array-unwrap path extractResponseFields adds on top of the
	// single-object path.
	list, ok := ops["monitor/getMsgVpnQueues"]
	if !ok {
		t.Fatal("expected operation monitor/getMsgVpnQueues not found")
	}
	if list.ResponseFields == nil {
		t.Fatal("monitor/getMsgVpnQueues.ResponseFields should be populated by unwrapping the array item schema")
	}
	for field, wantType := range wantTypes {
		if gotType := list.ResponseFields[field]; gotType != wantType {
			t.Errorf("monitor/getMsgVpnQueues.ResponseFields[%q] = %q, want %q", field, gotType, wantType)
		}
	}

	// A delete op's 200 response is SempMetaOnlyResponse — meta only, no
	// "data" property. ResponseFields must be nil (unknown), not an empty
	// map or a crash, so callers correctly skip response-shape validation
	// rather than treating "no fields" as "reject everything."
	del, ok := ops["config/deleteMsgVpnQueue"]
	if !ok {
		t.Fatal("expected operation config/deleteMsgVpnQueue not found")
	}
	if del.ResponseFields != nil {
		t.Errorf("config/deleteMsgVpnQueue.ResponseFields = %v, want nil for a data-less response", del.ResponseFields)
	}
}

func TestParseSpecs_RefParametersResolved(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// getMsgVpnQueue in the monitor spec has a $ref to selectQuery.
	// After resolution, "select" should appear as a query parameter.
	op := ops["monitor/getMsgVpnQueue"]
	if op == nil {
		t.Fatal("monitor/getMsgVpnQueue not found")
		return
	}

	found := false
	for _, p := range op.Parameters {
		if p.Name == "select" && p.In == "query" {
			found = true
			break
		}
	}
	if !found {
		t.Error("monitor/getMsgVpnQueue missing resolved $ref parameter: select (in=query)")
	}
}

func TestParseSpecs_NoGhostParameters(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// No operation should have parameters with empty Name or empty In.
	for key, op := range ops {
		for i, p := range op.Parameters {
			if p.Name == "" {
				t.Errorf("%s param[%d] has empty Name", key, i)
			}
			if p.In == "" {
				t.Errorf("%s param[%d] (%s) has empty In", key, i, p.Name)
			}
		}
	}
}

func TestParseSpecs_MonitorOperationsPresent(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	for _, key := range []string{"monitor/getMsgVpnQueue", "monitor/getMsgVpnClient", "monitor/getMsgVpnClientSubscriptions"} {
		if _, ok := ops[key]; !ok {
			t.Errorf("missing expected operation: %s", key)
		}
	}
}

func TestParseSpecs_PathIncludesBasePath(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	op := ops["monitor/getMsgVpnQueue"]
	if op == nil {
		t.Fatal("monitor/getMsgVpnQueue not found")
		return
	}

	if !strings.HasPrefix(op.Path, "/SEMP/v2/__private_monitor__/") {
		t.Errorf("monitor/getMsgVpnQueue.Path = %q, expected it to start with /SEMP/v2/__private_monitor__/", op.Path)
	}
}

func TestParseSpecs_NoDuplicateKeys(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// Currently two specs are embedded: the private monitor spec contributes monitor/ keys
	// and the private config spec contributes config/ keys.
	monitorOp := ops["monitor/getMsgVpnQueue"]
	if monitorOp == nil {
		t.Error("monitor/getMsgVpnQueue not found")
	}
	if monitorOp != nil && !strings.HasPrefix(monitorOp.Path, "/SEMP/v2/__private_monitor__/") {
		t.Errorf("monitor op path = %q, want /SEMP/v2/__private_monitor__/ prefix", monitorOp.Path)
	}
}

func TestParseSpecs_SpecTypeDerivation(t *testing.T) {
	ops, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs() error: %v", err)
	}

	// Every key must use a known normalized spec-type prefix, and every embedded
	// private spec must contribute at least one operation under its normalized
	// prefix — proving each __private_*__ basePath is wired into the spec-type
	// maps.
	validPrefixes := []string{"monitor/", "config/", "action/"}
	seen := make(map[string]bool, len(validPrefixes))
	for key := range ops {
		matched := false
		for _, prefix := range validPrefixes {
			if strings.HasPrefix(key, prefix) {
				seen[prefix] = true
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("operation key %q does not have a valid spec type prefix", key)
		}
	}
	for _, prefix := range validPrefixes {
		if !seen[prefix] {
			t.Errorf("no %s operations parsed; private %s spec not loaded", prefix, strings.TrimSuffix(prefix, "/"))
		}
	}
}
