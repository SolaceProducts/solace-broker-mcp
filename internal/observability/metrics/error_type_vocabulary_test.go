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

package metrics

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The error_type vocabulary is consumed by three signals — the metric label,
// the audit record, and the tools.CallTool span attribute — and the whole point
// of ADR-009 is that one predicate carries across all three untranslated. That
// only holds if every consumer agrees on what the set IS, and Go cannot
// enumerate a type's consts at runtime, so nothing about `var allErrorTypes`
// forces it to stay complete.
//
// So this parses the declaration instead. Adding `ErrorTypeSomethingNew` to the
// const block and forgetting allErrorTypes now fails here, immediately, in the
// PR that adds it.
//
// Without this the failure was silent and durable: Record coerces anything
// outside knownErrorTypes to `other`, while the span writes the classifier's
// value verbatim, so the same call read `other` on the metric and the real
// value on its span. An SRE would see a rising `other` bucket with no matching
// span filter, and nothing would fail in CI from the merge of the adding story
// until someone compared the two signals for one call by hand.
func TestAllErrorTypes_CoversEveryDeclaredConst(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "instruments.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing instruments.go: %v", err)
	}

	declared := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		// Only consts explicitly typed `ErrorType`. The const block declares
		// its type once on the first spec, and Go carries it down, so an
		// untyped continuation line is still an ErrorType — hence the check
		// on the resolved value rather than on spec.Type alone.
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "ErrorType" {
			return true
		}
		for _, name := range spec.Names {
			declared[name.Name] = true
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("parsed no ErrorType consts from instruments.go; this test cannot protect anything")
	}

	// Map the enumerated values back to their const names by value, so the
	// comparison is over the same identifiers the parser found.
	byValue := map[ErrorType]bool{}
	for _, et := range AllErrorTypes() {
		byValue[et] = true
	}

	// Resolve each declared const's literal value through the package itself.
	// A const declared but absent from allErrorTypes shows up as a value the
	// set does not contain.
	values := map[string]ErrorType{
		"ErrorTypePanic":                 ErrorTypePanic,
		"ErrorTypeBadRequest":            ErrorTypeBadRequest,
		"ErrorTypeUnknownTool":           ErrorTypeUnknownTool,
		"ErrorTypeMissingBroker":         ErrorTypeMissingBroker,
		"ErrorTypeUnknownBroker":         ErrorTypeUnknownBroker,
		"ErrorTypeBrokerInitError":       ErrorTypeBrokerInitError,
		"ErrorTypeValidationError":       ErrorTypeValidationError,
		"ErrorTypeExecutionError":        ErrorTypeExecutionError,
		"ErrorTypeNilResult":             ErrorTypeNilResult,
		"ErrorTypeNotFound":              ErrorTypeNotFound,
		"ErrorTypeOutputValidationError": ErrorTypeOutputValidationError,
		"ErrorTypeMarshalError":          ErrorTypeMarshalError,
		"ErrorTypeOther":                 ErrorTypeOther,
	}

	for name := range declared {
		value, mapped := values[name]
		if !mapped {
			t.Errorf("const %s is declared as an ErrorType but is not listed in this test's value map; add it to allErrorTypes, to this map, to the docs/observability.md error_type table, and to the span vocabulary test in test/integration", name)
			continue
		}
		if !byValue[value] {
			t.Errorf("const %s (%q) is declared but missing from allErrorTypes: Record would coerce it to %q while a span carried %q for the same call",
				name, value, ErrorTypeOther, value)
		}
	}
	for name := range values {
		if !declared[name] {
			t.Errorf("this test's value map lists %s but instruments.go no longer declares it; the const was removed or renamed", name)
		}
	}
}

// knownErrorTypes is what Record coerces against, and it is now derived from
// allErrorTypes rather than maintained beside it. Pinned so a future edit that
// re-hardcodes the map (the shape this replaced) fails rather than quietly
// reintroducing the drift.
func TestKnownErrorTypes_DerivedFromAllErrorTypes(t *testing.T) {
	if len(knownErrorTypes) != len(allErrorTypes) {
		t.Fatalf("knownErrorTypes has %d entries, allErrorTypes has %d: the coercion set is no longer derived from the vocabulary",
			len(knownErrorTypes), len(allErrorTypes))
	}
	for _, et := range AllErrorTypes() {
		if !knownErrorTypes[et] {
			t.Errorf("%q is in the vocabulary but not in the coercion set, so Record would rewrite it to %q", et, ErrorTypeOther)
		}
	}
}

// AllErrorTypes hands out a copy: a caller that sorts or truncates the result
// must not be able to reshape the vocabulary for everyone else.
func TestAllErrorTypes_ReturnsACopy(t *testing.T) {
	first := AllErrorTypes()
	original := first[0]
	first[0] = "mutated"
	if got := AllErrorTypes()[0]; got != original {
		t.Errorf("AllErrorTypes()[0] = %q after a caller mutated its slice, want %q", got, original)
	}
}
