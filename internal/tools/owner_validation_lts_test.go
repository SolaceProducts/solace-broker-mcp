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

package tools

import (
	"os"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// TestOwnerValidation_OperationsExistInLTSSpec fails when an owner
// pre-flight read uses an operation the 10.8.1 LTS broker does not support.
func TestOwnerValidation_OperationsExistInLTSSpec(t *testing.T) {
	const dir = "../semp/sempv2/specs/testdata/10.8.1.599"
	lts, err := sempv2.ParseSpecs(os.DirFS(dir))
	if err != nil {
		t.Fatalf("ParseSpecs(%s): %v", dir, err)
	}
	for _, id := range []string{getClientUsernameOperationID, getMsgVpnOperationID} {
		if lts[id] == nil {
			t.Errorf("operation %s missing from 10.8.1 spec", id)
		}
	}
}
