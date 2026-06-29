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

// Package postprocesstest provides test-only registration for the postprocess
// registry. It lives in a sibling package so that production binaries
// importing postprocess do not pull in the testing package and its flag
// side effects.
package postprocesstest

import (
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// Register installs a postprocessor for the duration of one test, removing
// it via t.Cleanup so test runs don't leak entries into the package-global
// registry across t.Parallel / go test -count=N. Use this instead of calling
// postprocess.Register directly from tests outside the postprocess package.
func Register(t *testing.T, name string, h postprocess.Handler) {
	t.Helper()
	if postprocess.IsRegistered(name) {
		t.Fatalf("postprocess: postprocessor %q already registered", name)
	}
	postprocess.Register(name, h)
	t.Cleanup(func() { postprocess.Unregister(name) })
}
