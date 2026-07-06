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
