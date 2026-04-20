package sempv2_test

import (
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2/specs"
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

	// Only the private monitor spec is embedded — all keys must use the monitor/ prefix.
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

	// Every key must start with monitor/ (private monitor normalized).
	for key := range ops {
		if !strings.HasPrefix(key, "monitor/") {
			t.Errorf("operation key %q does not have a valid spec type prefix", key)
		}
	}
}
