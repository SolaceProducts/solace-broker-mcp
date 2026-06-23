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

// Package postprocess provides a registry of named Go postprocessors that the
// composite-tool executor can apply to step results. A handler returns a
// "summary" map (e.g., aggregate counts) which the executor merges with the
// raw step results under a top-level "summary" key.
//
// Handlers register from their package's init() block; main.go pulls them in
// via a blank import. Each handler declares the field names it reads off each
// item in RequiredFields; ValidateTool cross-checks those at startup against
// the SEMP step's `select:` clause so a missing-field bug is caught at boot,
// not at first invocation.
//
// Test-only registration is provided by the sibling postprocesstest package
// so production binaries importing postprocess do not pull in the testing
// package or its flag side effects.
package postprocess

import "fmt"

// Handler bundles a postprocessor function with the field names it expects
// to read off each item. Fn receives the raw step results (keyed by step ID)
// and returns the summary map the executor merges under "summary".
type Handler struct {
	Fn             func(stepResults map[string]map[string]any) (map[string]any, error)
	RequiredFields []string
}

var registry = map[string]Handler{}

// Register installs a handler under the given name. Panics on duplicate
// registration since handlers register from init() and a duplicate would
// indicate a programming error.
func Register(name string, h Handler) {
	if _, dup := registry[name]; dup {
		panic("postprocess: postprocessor already registered: " + name)
	}
	registry[name] = h
}

// Apply runs the named postprocessor against the given step results.
func Apply(name string, stepResults map[string]map[string]any) (map[string]any, error) {
	h, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("postprocess: postprocessor %q not registered", name)
	}
	return h.Fn(stepResults)
}

// ValidateTool checks that postprocessorName is registered and that every
// field in its RequiredFields appears in selectFields. Returns the
// ticket-specified error template on a missing field so the message is
// uniform across tools.
func ValidateTool(toolName, postprocessorName string, selectFields []string) error {
	h, ok := registry[postprocessorName]
	if !ok {
		return fmt.Errorf("tool %q: postprocessor %q not registered", toolName, postprocessorName)
	}
	selectSet := make(map[string]struct{}, len(selectFields))
	for _, f := range selectFields {
		selectSet[f] = struct{}{}
	}
	for _, f := range h.RequiredFields {
		if _, ok := selectSet[f]; !ok {
			return fmt.Errorf("tool %q: postprocessor %q reads %q but it is not in select", toolName, postprocessorName, f)
		}
	}
	return nil
}

// IsRegistered reports whether a handler is registered under name.
// Exported only to support the postprocesstest helper package; production
// code has no reason to call this.
func IsRegistered(name string) bool {
	_, ok := registry[name]
	return ok
}

// Unregister removes a handler from the registry. Exported only to support
// the postprocesstest helper package's t.Cleanup hook; production code must
// not call this — production handlers register once at init() and stay for
// the process lifetime.
func Unregister(name string) {
	delete(registry, name)
}

// resetForTest clears the registry. Test-only helper used by tests in this
// package; not exported because nothing outside should mutate the registry
// after init().
func resetForTest() { registry = map[string]Handler{} }
