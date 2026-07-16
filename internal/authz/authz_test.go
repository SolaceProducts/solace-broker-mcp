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

package authz

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// --- helpers ----------------------------------------------------------------

func boolPtr(v bool) *bool { return &v }

func mkConfig(groups map[string][]string) config.ToolAuthorizationConfig {
	return config.ToolAuthorizationConfig{
		Enabled:           boolPtr(true),
		AccessLevelGroups: groups,
	}
}

func mustNewPolicy(t *testing.T, groups map[string][]string) *Policy {
	t.Helper()
	p, err := NewPolicy(mkConfig(groups))
	if err != nil {
		t.Fatalf("NewPolicy: unexpected error: %v", err)
	}
	return p
}

// --- A. TokenInfoExtraKeyGroups constant ------------------------------------

func TestTokenInfoExtraKeyGroups_value(t *testing.T) {
	if TokenInfoExtraKeyGroups != "groups" {
		t.Errorf("TokenInfoExtraKeyGroups = %q, want %q", TokenInfoExtraKeyGroups, "groups")
	}
}

// --- B. NewPolicy — construction --------------------------------------------

func TestNewPolicy_singleGroupSingleTool(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	if !reflect.DeepEqual(d.MatchedGroups, []string{"Ops"}) {
		t.Errorf("MatchedGroups = %v, want [Ops]", d.MatchedGroups)
	}
}

func TestNewPolicy_multipleGroupsOverlappingTools(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues", "delete-queue"},
		"Monitoring": {"list-queues", "get-broker-status"},
	})

	d := p.Authorize([]string{"Ops", "Monitoring"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow for list-queues")
	}
	if len(d.MatchedGroups) != 2 {
		t.Fatalf("MatchedGroups len = %d, want 2", len(d.MatchedGroups))
	}

	d = p.Authorize([]string{"Monitoring"}, "delete-queue")
	if d.Allowed {
		t.Error("Monitoring should not grant delete-queue")
	}

	d = p.Authorize([]string{"Ops"}, "get-broker-status")
	if d.Allowed {
		t.Error("Ops should not grant get-broker-status")
	}
}

func TestNewPolicy_errorIsNilInV1(t *testing.T) {
	_, err := NewPolicy(mkConfig(map[string][]string{
		"Ops": {"list-queues"},
	}))
	if err != nil {
		t.Errorf("expected nil error in v1, got: %v", err)
	}
}

func TestNewPolicy_sameToolInMultipleGroups(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues"},
		"Monitoring": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops", "Monitoring"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	want := []string{"Monitoring", "Ops"}
	if !reflect.DeepEqual(d.MatchedGroups, want) {
		t.Errorf("MatchedGroups = %v, want %v (sorted)", d.MatchedGroups, want)
	}
}

// --- C. NewPolicy — caller-mutation isolation --------------------------------

func TestNewPolicy_callerMutationDoesNotAffectPolicy(t *testing.T) {
	groups := map[string][]string{
		"Ops": {"list-queues"},
	}
	p := mustNewPolicy(t, groups)

	// Mutate the caller's map: add a new group, append a tool, overwrite.
	groups["Evil"] = []string{"delete-queue"}
	groups["Ops"] = append(groups["Ops"], "get-broker-status")

	d := p.Authorize([]string{"Evil"}, "delete-queue")
	if d.Allowed {
		t.Error("mutation after NewPolicy should not grant Evil access")
	}

	d = p.Authorize([]string{"Ops"}, "get-broker-status")
	if d.Allowed {
		t.Error("mutation after NewPolicy should not extend Ops grants")
	}

	d = p.Authorize([]string{"Ops"}, "list-queues")
	if !d.Allowed {
		t.Error("original grant should still work after caller mutation")
	}
}

// --- D. Authorize — allow scenarios (union semantics) -----------------------

func TestAuthorize_singleMatchingGroup(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues"},
		"Monitoring": {"get-broker-status"},
	})
	d := p.Authorize([]string{"Ops"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	if !reflect.DeepEqual(d.MatchedGroups, []string{"Ops"}) {
		t.Errorf("MatchedGroups = %v, want [Ops]", d.MatchedGroups)
	}
}

func TestAuthorize_multipleMatchingGroups(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues"},
		"Monitoring": {"list-queues"},
		"Admin":      {"list-queues"},
	})
	d := p.Authorize([]string{"Admin", "Ops", "Monitoring"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	want := []string{"Admin", "Monitoring", "Ops"}
	if !reflect.DeepEqual(d.MatchedGroups, want) {
		t.Errorf("MatchedGroups = %v, want %v (sorted)", d.MatchedGroups, want)
	}
}

func TestAuthorize_extraNonGrantingGroupsExcluded(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops", "Developers", "Marketing"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	if !reflect.DeepEqual(d.MatchedGroups, []string{"Ops"}) {
		t.Errorf("MatchedGroups = %v, want [Ops] — non-granting groups should be excluded", d.MatchedGroups)
	}
}

// --- E. Authorize — deny scenarios ------------------------------------------

func TestAuthorize_noCallerGroupIsGrantor(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{"Developers", "Marketing"}, "list-queues")
	if d.Allowed {
		t.Error("expected deny")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil on deny", d.MatchedGroups)
	}
}

func TestAuthorize_unknownToolName(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops"}, "not-a-real-tool")
	if d.Allowed {
		t.Error("expected deny for unknown tool")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil", d.MatchedGroups)
	}
}

func TestAuthorize_emptyToolName(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops"}, "")
	if d.Allowed {
		t.Error("expected deny for empty tool name")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil", d.MatchedGroups)
	}
}

func TestAuthorize_nilGroups(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize(nil, "list-queues")
	if d.Allowed {
		t.Error("expected deny for nil groups")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil", d.MatchedGroups)
	}
}

func TestAuthorize_emptyGroups(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{}, "list-queues")
	if d.Allowed {
		t.Error("expected deny for empty groups")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil", d.MatchedGroups)
	}
}

func TestAuthorize_emptyStringGroup(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops": {"list-queues"},
	})
	d := p.Authorize([]string{""}, "list-queues")
	if d.Allowed {
		t.Error("expected deny — empty-string group cannot match any policy key")
	}
	if d.MatchedGroups != nil {
		t.Errorf("MatchedGroups = %v, want nil", d.MatchedGroups)
	}
}

// --- F. Authorize — dedup & ordering ----------------------------------------

func TestAuthorize_duplicateCallerGroupDeduped(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues"},
		"Monitoring": {"list-queues"},
	})
	d := p.Authorize([]string{"Ops", "Ops", "Monitoring"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	want := []string{"Monitoring", "Ops"}
	if !reflect.DeepEqual(d.MatchedGroups, want) {
		t.Errorf("MatchedGroups = %v, want %v (each group once, sorted)", d.MatchedGroups, want)
	}
}

func TestAuthorize_matchedGroupsSortedLexicographically(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Zebra":    {"list-queues"},
		"Alpha":    {"list-queues"},
		"Middle":   {"list-queues"},
	})
	d := p.Authorize([]string{"Zebra", "Middle", "Alpha"}, "list-queues")
	if !d.Allowed {
		t.Fatal("expected allow")
	}
	want := []string{"Alpha", "Middle", "Zebra"}
	if !reflect.DeepEqual(d.MatchedGroups, want) {
		t.Errorf("MatchedGroups = %v, want %v (lexicographic)", d.MatchedGroups, want)
	}
}

// --- G. Policy.LogValue -----------------------------------------------------

func TestPolicy_LogValue_nonNil(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues", "delete-queue"},
		"Monitoring": {"list-queues", "get-broker-status"},
	})

	v := p.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want Group", v.Kind())
	}

	attrs := make(map[string]int64)
	for _, a := range v.Group() {
		attrs[a.Key] = a.Value.Int64()
	}

	if len(attrs) != 2 {
		t.Fatalf("expected exactly 2 attrs, got %d: %v", len(attrs), attrs)
	}
	if got := attrs["tool_grant_count"]; got != 3 {
		t.Errorf("tool_grant_count = %d, want 3", got)
	}
	if got := attrs["group_count"]; got != 2 {
		t.Errorf("group_count = %d, want 2", got)
	}
}

func TestPolicy_LogValue_nil(t *testing.T) {
	var p *Policy
	v := p.LogValue()
	if v.Kind() != slog.KindGroup {
		t.Fatalf("LogValue kind = %v, want Group", v.Kind())
	}
	if len(v.Group()) != 0 {
		t.Errorf("nil Policy should emit empty group, got %d attrs", len(v.Group()))
	}
}

// --- H. Decision — data-protection drift tests ------------------------------

func TestDecision_doesNotImplementStringer(t *testing.T) {
	var d Decision
	stringerType := reflect.TypeOf((*fmt.Stringer)(nil)).Elem()
	if reflect.TypeOf(d).Implements(stringerType) {
		t.Fatal("Decision must not implement fmt.Stringer — MatchedGroups would leak through fmt verbs")
	}
}

func TestDecision_doesNotImplementJSONMarshaler(t *testing.T) {
	var d Decision
	marshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	if reflect.TypeOf(d).Implements(marshalerType) {
		t.Fatal("Decision must not implement json.Marshaler — MatchedGroups would leak through json.Marshal")
	}
}

// --- I. Authorize — thread safety -------------------------------------------

func TestAuthorize_concurrentAccess(t *testing.T) {
	p := mustNewPolicy(t, map[string][]string{
		"Ops":        {"list-queues", "delete-queue"},
		"Monitoring": {"list-queues", "get-broker-status"},
		"Admin":      {"list-queues", "delete-queue", "get-broker-status"},
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			groups := []string{"Ops", "Monitoring"}
			if id%3 == 0 {
				groups = []string{"Admin"}
			}

			d := p.Authorize(groups, "list-queues")
			if !d.Allowed {
				t.Errorf("goroutine %d: expected allow for list-queues", id)
			}

			d = p.Authorize(groups, "not-a-tool")
			if d.Allowed {
				t.Errorf("goroutine %d: expected deny for not-a-tool", id)
			}
		}(i)
	}
	wg.Wait()
}
