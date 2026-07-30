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

package tools

import (
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
)

// Smoke tests against the embedded specs: trimmed POST, trimmed PATCH,
// raw view, unknown operation.

func TestSempSchemaMap_BuildsFromEmbeddedSpecs(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	// All ops tools.yaml points at from create/update descriptions; a spec
	// upgrade that drops one turns the pointer into a dead link.
	for _, opKey := range []string{
		"config/createMsgVpn",
		"config/updateMsgVpn",
		"config/createMsgVpnQueue",
		"config/updateMsgVpnQueue",
		"config/createMsgVpnTopicEndpoint",
		"config/updateMsgVpnTopicEndpoint",
		"config/createMsgVpnRestDeliveryPoint",
		"config/updateMsgVpnRestDeliveryPoint",
	} {
		info, ok := reg.ops[opKey]
		if !ok {
			t.Errorf("operation %q missing from semp schema map", opKey)
			continue
		}
		if info.defName == "" {
			t.Errorf("operation %q has no body definition", opKey)
		}
	}
}

func TestSempSchemaMap_TrimmedView_CreateQueue(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/createMsgVpnQueue", "trimmed")
	if err != nil {
		t.Fatalf("describe(trimmed): %v", err)
	}
	if got["method"] != "POST" || got["definition"] != "MsgVpnQueue" {
		t.Errorf("unexpected header fields: %+v", got)
	}
	attrs := got["attributes"].([]map[string]any)
	byName := make(map[string]map[string]any, len(attrs))
	for _, a := range attrs {
		byName[a["name"].(string)] = a
	}

	// queueName is the identifying path param: required on create,
	// read-only on update.
	qn := byName["queueName"]
	if qn["identifying"] != true || qn["requiredForCreate"] != true {
		t.Errorf("queueName should be identifying + requiredForCreate: %+v", qn)
	}
	if qn["writableOnUpdate"] != false {
		t.Errorf("queueName should be read-only on update: %+v", qn)
	}

	// permission carries the enum + default the trimmed view is meant to
	// surface. If either drops out we lose the whole point of the tool.
	perm := byName["permission"]
	if perm["default"] != "no-access" {
		t.Errorf("permission default: got %v want no-access", perm["default"])
	}
	enum, ok := perm["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Errorf("permission enum missing or empty: %+v", perm["enum"])
	}
	// Trimmed description drops the "minimum access scope" boilerplate.
	if desc, _ := perm["description"].(string); strings.Contains(desc, "minimum access scope") {
		t.Errorf("trimmed description should not contain access-scope boilerplate; got: %q", desc)
	}
}

func TestSempSchemaMap_TrimmedView_UpdateReflectsMethod(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/updateMsgVpnQueue", "trimmed")
	if err != nil {
		t.Fatalf("describe(trimmed update): %v", err)
	}
	if got["method"] != "PATCH" {
		t.Errorf("method for updateMsgVpnQueue: got %v want PATCH", got["method"])
	}
}

func TestSempSchemaMap_RawView(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	got, err := reg.describe("config/createMsgVpnQueue", "raw")
	if err != nil {
		t.Fatalf("describe(raw): %v", err)
	}
	schema, ok := got["schema"].(map[string]any)
	if !ok {
		t.Fatalf("raw view missing schema object: %+v", got)
	}
	// Raw view must preserve properties AND the x-* extensions that
	// power the trimmed view — that is the round-trip guarantee.
	props := schema["properties"].(map[string]any)
	perm := props["permission"].(map[string]any)
	if _, hasExt := perm["x-default"]; !hasExt {
		t.Errorf("raw view should carry x-default extension; got: %+v", perm)
	}
}

func TestSempSchemaMap_UnknownOperation(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	_, err = reg.describe("config/thisDoesNotExist", "trimmed")
	if err == nil {
		t.Fatal("expected error for unknown operation, got nil")
	}
	if !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("error should identify unknown-operation cause; got: %v", err)
	}
}
