package composite

import (
	"fmt"
	"testing"
	"testing/fstest"
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
