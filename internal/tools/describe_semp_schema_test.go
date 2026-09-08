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
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// The tool describes writable request bodies; the monitor API is all GETs with
// no bodies. Loading it would just add empty entries and mislead agents into
// thinking monitor operations are queryable here.
func TestSempSchemaMap_ExcludesMonitorSpec(t *testing.T) {
	t.Parallel()
	reg, err := buildSempSchemaMap(specs.FS)
	if err != nil {
		t.Fatalf("buildSempSchemaMap: %v", err)
	}
	if _, ok := reg.specs["monitor"]; ok {
		t.Errorf("monitor spec should not be loaded")
	}
	for key := range reg.ops {
		if strings.HasPrefix(key, "monitor/") {
			t.Errorf("monitor operation indexed: %q", key)
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

	// queueName is the identifying path param: required on create, read-only on update.
	qn := byName["queueName"]
	if qn["identifying"] != true || qn["requiredForCreate"] != true {
		t.Errorf("queueName should be identifying + requiredForCreate: %+v", qn)
	}
	if qn["writableOnUpdate"] != false {
		t.Errorf("queueName should be read-only on update: %+v", qn)
	}

	// msgVpnName is read-only on create (injected from the path, not the request body).
	mvn := byName["msgVpnName"]
	if mvn["writableOnCreate"] != false {
		t.Errorf("msgVpnName should have writableOnCreate=false: %+v", mvn)
	}

	// permission carries the enum + default the trimmed view is meant to surface.
	perm := byName["permission"]
	if perm["default"] != "no-access" {
		t.Errorf("permission default: got %v want no-access", perm["default"])
	}
	enum, ok := perm["enum"].([]any)
	if !ok || len(enum) == 0 {
		t.Errorf("permission enum missing or empty: %+v", perm["enum"])
	}
	// Trimmed description drops the "minimum access scope" boilerplate but keeps
	// the <pre> enum block so callers know what each enum value means.
	desc, _ := perm["description"].(string)
	if strings.Contains(desc, "minimum access scope") {
		t.Errorf("trimmed description should not contain access-scope boilerplate; got: %q", desc)
	}
	if !strings.Contains(desc, "<pre>") {
		t.Errorf("trimmed description should include <pre> enum block; got: %q", desc)
	}

	// permission.x-autoDisable is non-empty: changing permission on a live queue
	// briefly sets egressEnabled=false. An agent must see this before invoking update.
	if perm["autoDisable"] == nil {
		t.Errorf("permission should have autoDisable field: %+v", perm)
	}

	// eventBindCountThreshold is a bare $ref — must resolve to nested properties,
	// not fabricate writability flags from absent extensions.
	thresh := byName["eventBindCountThreshold"]
	if thresh == nil {
		t.Fatalf("eventBindCountThreshold missing from trimmed output")
	}
	if thresh["type"] != "object" {
		t.Errorf("$ref property should have type=object: %+v", thresh)
	}
	if _, hasWritable := thresh["writableOnCreate"]; hasWritable {
		t.Errorf("$ref property must not carry fabricated writableOnCreate: %+v", thresh)
	}
	nestedProps, ok := thresh["properties"].([]map[string]any)
	if !ok || len(nestedProps) == 0 {
		t.Errorf("$ref property should have resolved nested properties: %+v", thresh)
	}
}

func TestDescribeSempSchema_EmitsAuditLog(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	if err := RegisterDescribeSempSchema(server, specs.FS, nil); err != nil {
		t.Fatalf("RegisterDescribeSempSchema: %v", err)
	}

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "describe-semp-schema",
		Arguments: map[string]any{"operation": "config/createMsgVpnQueue"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("describe-semp-schema returned IsError: %v", res.Content)
	}

	audits := auditLines(t, &logBuf, "describe-semp-schema")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for describe-semp-schema, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "success" {
		t.Errorf("audit outcome = %v, want %q", got, "success")
	}
	// describe-semp-schema resolves no broker; the audit line carries the "none"
	// sentinel, matching the metric label rather than omitting the field.
	if got := audits[0]["broker"]; got != "none" {
		t.Errorf("audit broker = %v, want %q", got, "none")
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
