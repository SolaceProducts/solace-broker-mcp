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

package brokerhealth

import "testing"

// TestSystemResponse_RoundTrip decodes the live show-system fixture into
// systemResponse, marshals to JSON, and asserts both populated and absent
// curated fields behave correctly. The standalone test broker only emits a
// subset of the curated 17 fields; the rest must be ABSENT (not zero) so
// older brokers degrade gracefully.
func TestSystemResponse_RoundTrip(t *testing.T) {
	inner := extractInnerRPC(t, "testdata/show_system.xml")

	var resp systemResponse
	out := decodeAndMarshal(t, inner, "system", &resp)

	// Populated fields — broker emits these; verify camelCase + values.
	expected := map[string]any{
		"systemUptimeSeconds":       float64(87143),
		"lastRestartReason":         "",
		"maxBridges":                float64(25),
		"maxConnections":            float64(100),
		"maxQueueMessages":          float64(100),
		"maxKafkaBridges":           float64(0),
		"maxKafkaBrokerConnections": float64(0),
		"maxSubscriptions":          float64(50000),
		"cpuCores":                  float64(5),
	}
	for k, want := range expected {
		got, present := out[k]
		if !present {
			t.Errorf("expected key %q in JSON output, got absent", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %v, want %v", k, got, want)
		}
	}

	// Omitted-by-broker fields — must be ABSENT (graceful degradation).
	// The standalone VMR test broker doesn't emit any of these; pointer +
	// omitempty must make them disappear from JSON rather than appear as
	// zero values.
	for _, dropped := range []string{
		"maxGuaranteedMessageSize",
		"cpuCoresRequired",
		"hostVirtualMemory",
		"hostVirtualMemoryRequired",
		"memoryCgroupLimit",
		"memoryCgroupLimitRequired",
		"sharedMemory",
		"sharedMemoryRequired",
	} {
		if _, present := out[dropped]; present {
			t.Errorf("field %q should be absent (broker did not emit) but appeared in JSON", dropped)
		}
	}
}
