package composite

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess/postprocesstest"
)

func TestLoadTools_Valid(t *testing.T) {
	yaml := `
tools:
  - name: test-tool
    description: A test tool
    parameters:
      - name: vpnName
        type: string
        required: true
        description: "The VPN name"
    steps:
      - id: step1
        operation: monitor/getVpn
        args:
          vpnName: "{{.Params.vpnName}}"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	tool := tools[0]
	if tool.Name != "test-tool" {
		t.Errorf("expected name %q, got %q", "test-tool", tool.Name)
	}
	if tool.Description != "A test tool" {
		t.Errorf("expected description %q, got %q", "A test tool", tool.Description)
	}
	if len(tool.Parameters) != 1 {
		t.Fatalf("expected 1 parameter, got %d", len(tool.Parameters))
	}
	if tool.Parameters[0].Name != "vpnName" {
		t.Errorf("expected parameter name %q, got %q", "vpnName", tool.Parameters[0].Name)
	}
	if !tool.Parameters[0].Required {
		t.Error("expected parameter to be required")
	}
	if len(tool.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(tool.Steps))
	}
	if tool.Steps[0].ID != "step1" {
		t.Errorf("expected step ID %q, got %q", "step1", tool.Steps[0].ID)
	}
	if tool.Steps[0].Operation != "monitor/getVpn" {
		t.Errorf("expected operation %q, got %q", "monitor/getVpn", tool.Steps[0].Operation)
	}
	if tool.Result.Strategy != "collect" {
		t.Errorf("expected strategy %q, got %q", "collect", tool.Result.Strategy)
	}
}

func TestLoadTools_MissingName(t *testing.T) {
	yaml := `
tools:
  - description: No name tool
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
}

func TestLoadTools_MissingSteps(t *testing.T) {
	yaml := `
tools:
  - name: no-steps
    description: Tool without steps
    steps: []
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing steps, got nil")
	}
}

func TestLoadTools_DuplicateStepIDs(t *testing.T) {
	yaml := `
tools:
  - name: dup-steps
    description: Tool with duplicate step IDs
    steps:
      - id: same
        operation: monitor/getVpn
      - id: same
        operation: action/doSomething
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for duplicate step IDs, got nil")
	}
}

func TestLoadTools_EmptyOperation(t *testing.T) {
	yaml := `
tools:
  - name: empty-op
    description: Tool with empty operation
    steps:
      - id: s1
        operation: ""
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for empty operation, got nil")
	}
}

func TestLoadTools_UnsupportedStrategy(t *testing.T) {
	strategies := []string{"merge", "unwrap", "invalid"}
	for _, strategy := range strategies {
		t.Run(strategy, func(t *testing.T) {
			yaml := fmt.Sprintf(`
tools:
  - name: bad-strategy
    description: Tool with unsupported strategy
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: %s
`, strategy)
			fsys := fstest.MapFS{
				"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
			}

			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatalf("expected error for strategy %q, got nil", strategy)
			}
		})
	}
}

func TestLoadTools_MissingStrategy(t *testing.T) {
	yaml := `
tools:
  - name: no-strategy
    description: Tool with no strategy specified
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing strategy, got nil")
	}
}

func TestLoadTools_MissingDescription(t *testing.T) {
	yaml := `
tools:
  - name: no-desc
    steps:
      - id: s1
        operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing description, got nil")
	}
}

func TestLoadTools_MissingStepID(t *testing.T) {
	yaml := `
tools:
  - name: no-step-id
    description: Tool with missing step ID
    steps:
      - operation: monitor/getVpn
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	_, err := LoadTools(fsys, "tools.yaml")
	if err == nil {
		t.Fatal("expected error for missing step ID, got nil")
	}
}

func TestLoadTools_Annotations(t *testing.T) {
	yaml := `
tools:
  - name: monitor-tool
    description: A monitoring tool
    annotations:
      readOnly: true
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0].Annotations.ReadOnly == nil || !*tools[0].Annotations.ReadOnly {
		t.Error("expected ReadOnly = true")
	}
	if tools[0].Annotations.Destructive != nil {
		t.Error("expected Destructive = nil (omitted)")
	}
}

func TestLoadTools_AnnotationsDefault(t *testing.T) {
	yaml := `
tools:
  - name: no-annotations
    description: A tool without annotations
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
`
	fsys := fstest.MapFS{
		"tools.yaml": &fstest.MapFile{Data: []byte(yaml)},
	}

	tools, err := LoadTools(fsys, "tools.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// All annotation fields should default to nil (omitted).
	ann := tools[0].Annotations
	if ann.ReadOnly != nil || ann.Destructive != nil || ann.Idempotent != nil || ann.OpenWorld != nil {
		t.Errorf("expected all annotations to default to nil, got %+v", ann)
	}
}

func TestLoadTools_UnknownFieldRejected(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			name: "unknown top-level tool field",
			yaml: `
tools:
  - name: test-tool
    description: A test tool
    bogusField: oops
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
`,
		},
		{
			name: "unknown step field",
			yaml: `
tools:
  - name: test-tool
    description: A test tool
    steps:
      - id: s1
        operation: monitor/getVpn
        paralel: true
    result:
      strategy: collect
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"tools.yaml": &fstest.MapFile{Data: []byte(tc.yaml)},
			}
			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatal("expected error for unknown field, got nil")
			}
		})
	}
}

// TestLoadTools_StrategyConfig covers the cross-field rules between
// result.strategy and result.postProcess plus the reserved "summary" step ID
// and the args.select / select coexistence guard.
func TestLoadTools_StrategyConfig(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "postProcess without name",
			yaml: `
tools:
  - name: bad
    description: missing handler name
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: postProcess
`,
			wantSub: `postProcess is required when strategy is "postProcess"`,
		},
		{
			name: "collect with postProcess set",
			yaml: `
tools:
  - name: bad
    description: postProcess on collect strategy
    steps:
      - id: s1
        operation: monitor/getVpn
    result:
      strategy: collect
      postProcess: someHandler
`,
			wantSub: `postProcess must be empty when strategy is "collect"`,
		},
		{
			name: "summary step ID reserved under postProcess",
			yaml: `
tools:
  - name: bad
    description: reserved step ID
    steps:
      - id: summary
        operation: monitor/getVpn
    result:
      strategy: postProcess
      postProcess: someHandler
`,
			wantSub: `step ID "summary" is reserved`,
		},
		{
			name: "both args.select and select set",
			yaml: `
tools:
  - name: bad
    description: select set twice
    steps:
      - id: s1
        operation: monitor/getVpn
        args:
          select: "a, b"
        select:
          - a
          - b
    result:
      strategy: collect
`,
			wantSub: "cannot set both args.select and select",
		},
		{
			// args.select under postProcess would silently bypass the
			// RequiredFields cross-check (it only reads structured select),
			// so the message must point at the real cause — not the field.
			name: "args.select under postProcess is rejected",
			yaml: `
tools:
  - name: bad
    description: args.select under postProcess
    steps:
      - id: s1
        operation: monitor/getVpn
        args:
          select: "a, b"
    result:
      strategy: postProcess
      postProcess: someHandler
`,
			wantSub: `args.select is not allowed when strategy is "postProcess"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(tc.yaml)}}
			_, err := LoadTools(fsys, "tools.yaml")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidatePostProcess covers the boot-time cross-check between a
// postProcess tool's handler RequiredFields and the union of its steps'
// `select:` arrays. Builds CompositeTool values directly rather than going
// through YAML so the test isolates the validation logic.
func TestValidatePostProcess(t *testing.T) {
	postprocesstest.Register(t, "__test_pp_handler", postprocess.Handler{
		Fn:             func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredFields: []string{"a", "b"},
	})

	t.Run("collect tool is ignored", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "collect-tool",
			Steps:  []Step{{ID: "s1", Operation: "x"}},
			Result: ResultStrategy{Strategy: "collect"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("collect tool should be skipped, got %v", err)
		}
	})

	t.Run("happy path with required fields covered", func(t *testing.T) {
		tools := []CompositeTool{{
			Name: "ok-tool",
			Steps: []Step{
				{ID: "s1", Operation: "x", Select: []string{"a"}},
				{ID: "s2", Operation: "y", Select: []string{"b", "c"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_handler"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("missing required field surfaces templated error", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "bad-tool",
			Steps:  []Step{{ID: "s1", Operation: "x", Select: []string{"a"}}}, // b missing
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_handler"},
		}}
		err := ValidatePostProcess(tools)
		want := `tool "bad-tool": postprocessor "__test_pp_handler" reads "b" but it is not in select`
		if err == nil || err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %v", want, err)
		}
	})

	t.Run("unregistered handler", func(t *testing.T) {
		tools := []CompositeTool{{
			Name:   "no-handler",
			Steps:  []Step{{ID: "s1", Operation: "x"}},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "nope"},
		}}
		err := ValidatePostProcess(tools)
		if err == nil || !strings.Contains(err.Error(), `postprocessor "nope" not registered`) {
			t.Fatalf("got %v", err)
		}
	})
}

// TestValidatePostProcess_MultiStep covers the multi-step form: a handler
// declaring RequiredFieldsPerStep must be validated against each step's own
// `select:`, not the union across steps. This guards the property that added
// with the first multi-step handler (getRdpStatus) — the old union check
// would silently accept a config where step A reads field X but only step B
// selects it.
func TestValidatePostProcess_MultiStep(t *testing.T) {
	postprocesstest.Register(t, "__test_pp_multi", postprocess.Handler{
		Fn:            func(map[string]map[string]any) (map[string]any, error) { return nil, nil },
		RequiredSteps: []string{"a", "b"},
		RequiredFieldsPerStep: map[string][]string{
			"a": {"x"},
			"b": {"y"},
		},
	})

	t.Run("each step covers its own required fields", func(t *testing.T) {
		tools := []CompositeTool{{
			Name: "ok-multi",
			Steps: []Step{
				{ID: "a", Operation: "op-a", Select: []string{"x"}},
				{ID: "b", Operation: "op-b", Select: []string{"y"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_multi"},
		}}
		if err := ValidatePostProcess(tools); err != nil {
			t.Fatalf("expected ok, got %v", err)
		}
	})

	t.Run("field only in sibling select is rejected", func(t *testing.T) {
		// The union across steps includes both x and y, so the old
		// (pre-multi-step) check would have passed. The per-step check must
		// fail because step "a" reads x but step "a"'s select does not.
		tools := []CompositeTool{{
			Name: "bad-multi",
			Steps: []Step{
				{ID: "a", Operation: "op-a", Select: []string{"y"}},
				{ID: "b", Operation: "op-b", Select: []string{"x", "y"}},
			},
			Result: ResultStrategy{Strategy: "postProcess", PostProcess: "__test_pp_multi"},
		}}
		err := ValidatePostProcess(tools)
		want := `tool "bad-multi": postprocessor "__test_pp_multi" reads "x" on step "a" but it is not in that step's select`
		if err == nil || err.Error() != want {
			t.Fatalf("\nwant: %s\ngot:  %v", want, err)
		}
	})
}
