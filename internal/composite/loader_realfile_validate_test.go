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

package composite

import (
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/definitions"
	// Blank import so the real production postprocess handlers self-register
	// via init() before ValidatePostProcess runs below — without this, every
	// postProcess tool would fail with "postprocessor not registered" even
	// though the YAML itself is fine, since this test's package doesn't
	// otherwise pull in the handlers package the way cmd/server/main.go does.
	_ "github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/handlers"
)

// TestRealToolsYAML_PassesPostProcessValidation pins SOL-152125: a
// postProcess handler reading a field its step doesn't `select:` must be
// caught by `go test ./internal/composite/...` alone, not only at full
// server startup (cmd/server/main.go calls ValidatePostProcess at boot) or by
// unit tests against synthetic fixtures (loader_schema_test.go). Both of
// those already exist; what was missing was a fast, isolated check against
// the real, committed tools.yaml — this is that check.
func TestRealToolsYAML_PassesPostProcessValidation(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools error: %v", err)
	}
	if err := ValidatePostProcess(tools); err != nil {
		t.Fatalf("ValidatePostProcess error against the real embedded tools.yaml: %v", err)
	}
}
