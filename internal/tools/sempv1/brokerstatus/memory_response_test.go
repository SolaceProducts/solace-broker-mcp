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

package brokerstatus

import (
	"math"
	"testing"
)

// TestMemoryResponse_RoundTrip decodes the live show-memory fixture into
// memoryResponse, marshals to JSON, and asserts the two curated percentage
// fields carry their fixture values with sufficient precision (the broker
// emits decimals like 60.2646).
func TestMemoryResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_memory.xml")

	var resp memoryResponse
	out := decodeAndMarshal(t, inner, "memory", &resp)

	got, ok := out["physicalMemoryUsagePercent"].(float64)
	if !ok {
		t.Fatalf("physicalMemoryUsagePercent not a float64: %T", out["physicalMemoryUsagePercent"])
	}
	if math.Abs(got-60.2646) > 1e-9 {
		t.Errorf("physicalMemoryUsagePercent = %v, want 60.2646", got)
	}

	got, ok = out["subscriptionMemoryUsagePercent"].(float64)
	if !ok {
		t.Fatalf("subscriptionMemoryUsagePercent not a float64: %T", out["subscriptionMemoryUsagePercent"])
	}
	if math.Abs(got-0.00786782) > 1e-12 {
		t.Errorf("subscriptionMemoryUsagePercent = %v, want 0.00786782", got)
	}

	// Sanity — uncurated fields should not bleed through.
	for _, dropped := range []string{"physicalMemory", "subscriptionsMemory", "ipcBuffers"} {
		if _, present := out[dropped]; present {
			t.Errorf("uncurated field %q should not appear in JSON output", dropped)
		}
	}
}
