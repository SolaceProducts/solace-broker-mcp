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

package postprocess

import (
	"strings"
	"testing"
)

func TestRegister_Duplicate_Panics(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	Register("h", Handler{Fn: func(map[string]map[string]any) (map[string]any, error) { return nil, nil }})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
		if !strings.Contains(r.(string), "already registered") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	Register("h", Handler{Fn: func(map[string]map[string]any) (map[string]any, error) { return nil, nil }})
}

func TestApply_UnknownHandler(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	_, err := Apply("missing", nil)
	if err == nil || !strings.Contains(err.Error(), `postprocessor "missing" not registered`) {
		t.Fatalf("got %v", err)
	}
}

func TestApply_DispatchesToHandler(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	Register("ok", Handler{Fn: func(stepResults map[string]map[string]any) (map[string]any, error) {
		return map[string]any{"count": len(stepResults)}, nil
	}})
	got, err := Apply("ok", map[string]map[string]any{"a": {}, "b": {}})
	if err != nil {
		t.Fatal(err)
	}
	if got["count"] != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestValidateTool(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	Register("h", Handler{
		Fn:             func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps:  []string{"s1"},
		RequiredFields: []string{"a", "b"},
	})

	t.Run("unknown handler", func(t *testing.T) {
		err := ValidateTool("tool-x", "missing", []string{"s1"}, map[string][]string{"s1": {"a"}})
		if err == nil || !strings.Contains(err.Error(), `postprocessor "missing" not registered`) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("missing required step", func(t *testing.T) {
		err := ValidateTool("tool-x", "h", []string{"other"}, map[string][]string{"other": {"a", "b"}})
		if err == nil {
			t.Fatal("expected error")
		}
		want := `tool "tool-x": postprocessor "h" reads step "s1" but no such step is defined`
		if err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %s", want, err.Error())
		}
	})

	t.Run("missing required field", func(t *testing.T) {
		err := ValidateTool("tool-x", "h", []string{"s1"}, map[string][]string{"s1": {"a"}}) // b is missing
		if err == nil {
			t.Fatal("expected error")
		}
		want := `tool "tool-x": postprocessor "h" reads "b" but it is not in select`
		if err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %s", want, err.Error())
		}
	})

	t.Run("ok", func(t *testing.T) {
		if err := ValidateTool("tool-x", "h", []string{"s1"}, map[string][]string{"s1": {"a", "b", "c"}}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestValidateTool_PerStep pins the multi-step contract: a handler declaring
// RequiredFieldsPerStep is checked against each step's OWN select, not the
// union — a field that only appears in a sibling step's select must still fail
// validation for the step that reads it.
func TestValidateTool_PerStep(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	Register("multi", Handler{
		Fn:            func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps: []string{"a", "b"},
		RequiredFieldsPerStep: map[string][]string{
			"a": {"x"},
			"b": {"y"},
		},
	})

	t.Run("ok", func(t *testing.T) {
		if err := ValidateTool("tool-m", "multi",
			[]string{"a", "b"},
			map[string][]string{"a": {"x"}, "b": {"y"}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("field only in sibling select is rejected", func(t *testing.T) {
		// x is selected on b, not on a — the union check would pass but the
		// per-step check must reject this.
		err := ValidateTool("tool-m", "multi",
			[]string{"a", "b"},
			map[string][]string{"a": {}, "b": {"x", "y"}})
		if err == nil {
			t.Fatal("expected error")
		}
		want := `tool "tool-m": postprocessor "multi" reads "x" on step "a" but it is not in that step's select`
		if err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %s", want, err.Error())
		}
	})
}

// TestValidateTool_PerStepTakesPrecedence pins the documented contract: when a
// handler declares both RequiredFields and RequiredFieldsPerStep, the per-step
// form wins and the flat form is ignored. Guards against a future edit that
// swaps the branch order or accidentally unions the two.
func TestValidateTool_PerStepTakesPrecedence(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	Register("both", Handler{
		Fn:            func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps: []string{"s1"},
		// Flat form asks for "flat-only" — if it were consulted, the ok
		// subtest below would fail because no step selects "flat-only".
		RequiredFields:        []string{"flat-only"},
		RequiredFieldsPerStep: map[string][]string{"s1": {"x"}},
	})

	t.Run("ok when per-step select covers per-step fields, flat ignored", func(t *testing.T) {
		if err := ValidateTool("tool-b", "both",
			[]string{"s1"},
			map[string][]string{"s1": {"x"}}); err != nil {
			t.Fatalf("per-step should win; flat requirement %q must be ignored: %v", "flat-only", err)
		}
	})

	t.Run("per-step failure surfaces even when flat would pass", func(t *testing.T) {
		// Select covers the flat "flat-only" field but not the per-step "x".
		// If the union fallback ran, this would pass; per-step must reject.
		err := ValidateTool("tool-b", "both",
			[]string{"s1"},
			map[string][]string{"s1": {"flat-only"}})
		if err == nil || !strings.Contains(err.Error(), `"x" on step "s1"`) {
			t.Fatalf("per-step check must run when RequiredFieldsPerStep is set, got %v", err)
		}
	})
}

// TestRegister_InconsistentPerStepStep_Panics pins the init-time consistency
// check: a handler that declares RequiredFieldsPerStep for a step not in its
// own RequiredSteps is a programming error and must panic at Register().
func TestRegister_InconsistentPerStepStep_Panics(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when RequiredFieldsPerStep references a step not in RequiredSteps")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "is not in RequiredSteps") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	Register("bad", Handler{
		Fn:                    func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps:         []string{"a"},
		RequiredFieldsPerStep: map[string][]string{"stray": {"x"}},
	})
}
