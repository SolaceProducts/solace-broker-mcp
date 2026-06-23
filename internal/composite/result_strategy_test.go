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

package composite_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess/postprocesstest"
)

func TestApplyResultStrategy_Collect(t *testing.T) {
	stepResults := map[string]map[string]any{
		"a": {"x": 1},
		"b": {"y": 2},
	}
	got, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "collect"}, stepResults)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": map[string]any{"x": 1},
		"b": map[string]any{"y": 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestApplyResultStrategy_PostProcess_MergesSummary(t *testing.T) {
	postprocesstest.Register(t, "__test_summary", postprocess.Handler{
		Fn: func(map[string]map[string]any) (map[string]any, error) {
			return map[string]any{"count": 7}, nil
		},
	})
	stepResults := map[string]map[string]any{
		"queues": {"data": []any{}},
	}
	got, err := composite.ApplyResultStrategy(
		composite.ResultStrategy{Strategy: "postProcess", PostProcess: "__test_summary"},
		stepResults,
	)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := got["summary"].(map[string]any)
	if !ok || summary["count"] != 7 {
		t.Fatalf("summary: got %+v", got["summary"])
	}
	queues, ok := got["queues"].(map[string]any)
	if !ok || !reflect.DeepEqual(queues["data"], []any{}) {
		t.Fatalf("queues: got %+v", got["queues"])
	}
}

func TestApplyResultStrategy_Unsupported(t *testing.T) {
	_, err := composite.ApplyResultStrategy(composite.ResultStrategy{Strategy: "bogus"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("got %v", err)
	}
}
