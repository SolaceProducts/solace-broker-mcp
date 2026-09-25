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

// Package panicstest_test guards the scaffolding itself. These tests are the
// regression for SOL-154365: they exercise the cross-package path that was
// broken — registering against a provider from outside package panics and
// having that registration end with the test — which the panics package's own
// white-box tests cannot cover, because they reach the unexported counter
// directly instead of going through the helper.
//
// Deliberately an external test package (panicstest_test, not panicstest): the
// point is to use the helper exactly as internal/middleware/recovery,
// internal/tools, internal/observability/metrics and cmd/server use it.
//
// Neither test calls t.Parallel, and neither may: both mutate the
// process-level counter. See panicstest.Register's doc.
package panicstest_test

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics/panicstest"
)

// TestRegisterCleanupReturnsCounterToNoOp is the direct regression guard: a
// registration must not outlive the test that made it.
//
// The subtest's reader is captured and kept live on purpose. Once the subtest
// ends its t.Cleanup has run, so a later record call must reach nothing — and
// because the reader is still collectable, a write that DID still reach the
// instrument shows up as a count here rather than vanishing. Before
// SOL-154365 this test fails: with no way to unregister, the post-cleanup
// write lands on the still-installed instrument and the final count is 2.
func TestRegisterCleanupReturnsCounterToNoOp(t *testing.T) {
	var reader *sdkmetric.ManualReader

	t.Run("scoped", func(t *testing.T) {
		reader = panicstest.InstallReader(t)
		panics.RecoveredHTTP(context.Background())

		if got := panicstest.Counts(t, reader)["http"]; got != 1 {
			t.Fatalf("http count inside the scoped test = %d, want 1", got)
		}
	})

	// Under a -run filter that deselects the subtest, its body never executes
	// and reader stays nil. Checking t.Run's bool does NOT catch that — it
	// returns true for a filtered-out subtest (testing.T.Run: "if !ok ||
	// shouldFailFast() { return true }"), so only the invariant itself will
	// do. Without this the assertions below dereference nil and panic, which
	// takes the whole test binary down instead of failing one test.
	if reader == nil {
		t.Skip("scoped subtest filtered out by -run; nothing to assert against")
	}

	panics.RecoveredHTTP(context.Background())

	if got := panicstest.Counts(t, reader)["http"]; got != 1 {
		t.Errorf("http count after the scoped test = %d, want 1 — the registration outlived its test", got)
	}
}

// TestInstallReader_FreshReaderSeesOnlyItsOwnCounts pins the consequence that
// matters to callers: a test that installs its own reader sees only what it
// recorded, never a previous test's totals. It also asserts the documented
// two-series cardinality bound, so a reader that somehow observed nothing at
// all cannot pass by reading zero.
func TestInstallReader_FreshReaderSeesOnlyItsOwnCounts(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		first := panicstest.InstallReader(t)
		panics.RecoveredTool(context.Background())
		panics.RecoveredTool(context.Background())

		if got := panicstest.Counts(t, first)["tool"]; got != 2 {
			t.Fatalf("tool count in the first test = %d, want 2", got)
		}
	})

	second := panicstest.InstallReader(t)
	panics.RecoveredTool(context.Background())

	got := panicstest.Counts(t, second)
	if len(got) != 2 {
		t.Errorf("series count = %d, want 2 (both boundaries seeded)", len(got))
	}
	if got["tool"] != 1 {
		t.Errorf("tool count in the second test = %d, want 1 — it saw the first test's totals", got["tool"])
	}
	if got["http"] != 0 {
		t.Errorf("http count in the second test = %d, want 0", got["http"])
	}
}
