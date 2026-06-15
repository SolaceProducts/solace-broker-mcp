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

package brokerstatus

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// extractInnerRPC strips the outer <rpc-reply>...<rpc>...</rpc>...</rpc-reply>
// envelope and returns the inner <show>...<X>...</X>...</show> bytes — the
// shape of result.InnerXML at runtime per parseReply.
func extractInnerRPC(t *testing.T, fixturePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", fixturePath, err)
	}
	s := string(raw)
	open := strings.Index(s, "<rpc>")
	close := strings.LastIndex(s, "</rpc>")
	if open < 0 || close < 0 || close < open {
		t.Fatalf("could not locate <rpc>...</rpc> in fixture %s", fixturePath)
	}
	return []byte(s[open+len("<rpc>") : close])
}

// decodeAndMarshal runs the same XML → struct → JSON → map pipeline the
// handler uses, so the round-trip test exercises real production paths
// rather than synthetic ones.
func decodeAndMarshal(t *testing.T, inner []byte, innerTag string, target any) map[string]any {
	t.Helper()

	switch tgt := target.(type) {
	case *versionResponse:
		var w struct {
			XMLName xml.Name        `xml:"show"`
			Inner   versionResponse `xml:"version"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
	case *systemResponse:
		var w struct {
			XMLName xml.Name       `xml:"show"`
			Inner   systemResponse `xml:"system"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
	case *memoryResponse:
		var w struct {
			XMLName xml.Name       `xml:"show"`
			Inner   memoryResponse `xml:"memory"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
	case *messageSpoolResponse:
		var w struct {
			XMLName xml.Name             `xml:"show"`
			Inner   messageSpoolResponse `xml:"message-spool"`
		}
		if err := xml.Unmarshal(inner, &w); err != nil {
			t.Fatalf("unmarshal %s: %v", innerTag, err)
		}
		*tgt = w.Inner
	default:
		t.Fatalf("decodeAndMarshal: unsupported target type %T", target)
	}

	asJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatalf("marshal %s: %v", innerTag, err)
	}
	var out map[string]any
	if err := json.Unmarshal(asJSON, &out); err != nil {
		t.Fatalf("re-unmarshal %s JSON: %v", innerTag, err)
	}
	return out
}

// TestVersionResponse_RoundTrip decodes the live show-version fixture into
// versionResponse, marshals to JSON, and asserts the curated keys carry
// their expected camelCase form and wire values.
func TestVersionResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_version.xml")

	var resp versionResponse
	out := decodeAndMarshal(t, inner, "version", &resp)

	if got := out["description"]; got != "Solace PubSub+ Software Enterprise Version 10.25.0.217" {
		t.Errorf("description = %v, want fixture value", got)
	}

	uptime, ok := out["uptime"].(map[string]any)
	if !ok {
		t.Fatalf("uptime not a map: %T", out["uptime"])
	}
	if got := uptime["totalSecs"]; got != float64(87143) {
		t.Errorf("uptime.totalSecs = %v, want 87143", got)
	}

	// Curated tool surface should NOT include uncurated fields.
	for _, dropped := range []string{"executables", "currentLoad", "loads", "solbases"} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated field %q should not appear in JSON output", dropped)
		}
	}
}
