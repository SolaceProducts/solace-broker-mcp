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

package queuemetrics

import (
	"os"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// TestSEMPv2Operation_MatchesLTSSpec fails when the inline SEMPv2 call uses a
// path or select attribute the 10.8.1 LTS broker does not support.
func TestSEMPv2Operation_MatchesLTSSpec(t *testing.T) {
	const dir = "../../semp/sempv2/specs/testdata/10.8.1.599"
	lts, err := sempv2.ParseSpecs(os.DirFS(dir))
	if err != nil {
		t.Fatalf("ParseSpecs(%s): %v", dir, err)
	}
	spec := lts["monitor/getMsgVpnQueue"]
	if spec == nil {
		t.Fatal("monitor/getMsgVpnQueue missing from 10.8.1 spec")
	}

	op := buildSEMPv2Operation()
	if op.Method != spec.Method || op.Path != spec.Path {
		t.Errorf("operation = %s %s, 10.8.1 spec = %s %s", op.Method, op.Path, spec.Method, spec.Path)
	}
	for _, f := range strings.Split(sempv2Select, ",") {
		if f = strings.TrimSpace(f); spec.ResponseFields[f] == "" {
			t.Errorf("select attribute %s missing from 10.8.1 spec", f)
		}
	}
}
