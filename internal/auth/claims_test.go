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

package auth

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestResolveGroups(t *testing.T) {
	tests := []struct {
		name       string
		claims     map[string]any
		claimName  string
		wantGroups []string
		wantOK     bool
	}{
		{
			name:      "key absent",
			claims:    map[string]any{"other": "value"},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:      "value is JSON null",
			claims:    map[string]any{"groups": nil},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:       "non-empty scalar string",
			claims:     map[string]any{"groups": "admins"},
			claimName:  "groups",
			wantGroups: []string{"admins"},
			wantOK:     true,
		},
		{
			name:      "empty scalar string",
			claims:    map[string]any{"groups": ""},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:       "string array",
			claims:     map[string]any{"groups": []any{"admin", "ops"}},
			claimName:  "groups",
			wantGroups: []string{"admin", "ops"},
			wantOK:     true,
		},
		{
			name:       "empty array",
			claims:     map[string]any{"groups": []any{}},
			claimName:  "groups",
			wantGroups: []string{},
			wantOK:     true,
		},
		{
			name:       "mixed array filters non-strings",
			claims:     map[string]any{"groups": []any{"admin", float64(42), true, nil, "ops"}},
			claimName:  "groups",
			wantGroups: []string{"admin", "ops"},
			wantOK:     true,
		},
		{
			name:      "array with nil elements only",
			claims:    map[string]any{"groups": []any{nil, nil}},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:      "array of all non-strings",
			claims:    map[string]any{"groups": []any{float64(1), float64(2), true}},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:      "nested object",
			claims:    map[string]any{"groups": map[string]any{"nested": "value"}},
			claimName: "groups",
			wantOK:    false,
		},
		{
			name:       "duplicates preserved",
			claims:     map[string]any{"groups": []any{"admin", "admin", "ops"}},
			claimName:  "groups",
			wantGroups: []string{"admin", "admin", "ops"},
			wantOK:     true,
		},
		{
			name:       "whitespace-only string treated as group",
			claims:     map[string]any{"groups": "   "},
			claimName:  "groups",
			wantGroups: []string{"   "},
			wantOK:     true,
		},
		{
			name:       "case preserved",
			claims:     map[string]any{"groups": []any{"Admin", "ADMIN", "admin"}},
			claimName:  "groups",
			wantGroups: []string{"Admin", "ADMIN", "admin"},
			wantOK:     true,
		},
		{
			name:       "order preserved",
			claims:     map[string]any{"groups": []any{"c", "a", "b"}},
			claimName:  "groups",
			wantGroups: []string{"c", "a", "b"},
			wantOK:     true,
		},
		{
			name:      "float64 value",
			claims:    map[string]any{"groups": float64(42)},
			claimName: "groups",
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, ok := ResolveGroups(tt.claims, tt.claimName)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if groups != nil {
					t.Errorf("groups = %v, want nil when ok is false", groups)
				}
				return
			}
			if len(groups) != len(tt.wantGroups) {
				t.Fatalf("groups len = %d, want %d; got %v", len(groups), len(tt.wantGroups), groups)
			}
			for i, g := range groups {
				if g != tt.wantGroups[i] {
					t.Errorf("groups[%d] = %q, want %q", i, g, tt.wantGroups[i])
				}
			}
		})
	}
}

func TestResolveGroups_SoftCap(t *testing.T) {
	elems := make([]any, 251)
	for i := range elems {
		elems[i] = "g"
	}
	claims := map[string]any{"groups": elems}

	var buf bytes.Buffer
	prev := slog.Default()
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	groups, ok := ResolveGroups(claims, "groups")
	if !ok {
		t.Fatal("expected ok = true")
	}
	if len(groups) != groupsSoftCap {
		t.Errorf("len(groups) = %d, want %d", len(groups), groupsSoftCap)
	}
	if !bytes.Contains(buf.Bytes(), []byte("exceeded cap")) {
		t.Error("expected WARN log about exceeded cap")
	}
}
