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
	"strings"
	"testing"
)

func TestValidateParams_Valid(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string"},
			"queueName":  map[string]any{"type": "string"},
		},
		"required": []string{"msgVpnName", "queueName"},
	}
	params := map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	}

	if err := ValidateParams(params, schema); err != nil {
		t.Fatalf("expected valid params to pass, got: %v", err)
	}
}

func TestValidateParams_MissingRequired(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string"},
			"queueName":  map[string]any{"type": "string"},
		},
		"required": []string{"msgVpnName", "queueName"},
	}
	params := map[string]any{
		"msgVpnName": "default",
		// queueName missing
	}

	err := ValidateParams(params, schema)
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if !strings.Contains(err.Error(), "queueName") {
		t.Errorf("error should mention missing field 'queueName', got: %v", err)
	}
	if !strings.Contains(err.Error(), "parameter validation failed") {
		t.Errorf("error should have correct prefix, got: %v", err)
	}
}

func TestValidateParams_WrongType(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"maxResults": map[string]any{"type": "integer"},
		},
	}
	params := map[string]any{
		"maxResults": "not-a-number",
	}

	err := ValidateParams(params, schema)
	if err == nil {
		t.Fatal("expected validation error for wrong type")
	}
	if !strings.Contains(err.Error(), "maxResults") {
		t.Errorf("error should mention field 'maxResults', got: %v", err)
	}
}

func TestValidateParams_MultipleErrors(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string"},
			"queueName":  map[string]any{"type": "string"},
			"maxResults": map[string]any{"type": "integer"},
		},
		"required": []string{"msgVpnName", "queueName"},
	}
	params := map[string]any{
		// Both required fields missing, plus wrong type on optional field.
		"maxResults": "not-a-number",
	}

	err := ValidateParams(params, schema)
	if err == nil {
		t.Fatal("expected validation errors")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "msgVpnName") {
		t.Errorf("error should mention 'msgVpnName', got: %v", errMsg)
	}
	if !strings.Contains(errMsg, "queueName") {
		t.Errorf("error should mention 'queueName', got: %v", errMsg)
	}
	if !strings.Contains(errMsg, "maxResults") {
		t.Errorf("error should mention 'maxResults', got: %v", errMsg)
	}
}

func TestValidateParams_EmptyParamsNoRequired(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"optional": map[string]any{"type": "string"},
		},
	}
	params := map[string]any{}

	if err := ValidateParams(params, schema); err != nil {
		t.Fatalf("empty params with no required fields should pass, got: %v", err)
	}
}

func TestValidateParams_BooleanType(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"enabled": map[string]any{"type": "boolean"},
		},
	}

	// Valid boolean.
	if err := ValidateParams(map[string]any{"enabled": true}, schema); err != nil {
		t.Fatalf("expected boolean to pass, got: %v", err)
	}

	// Invalid: string instead of boolean.
	err := ValidateParams(map[string]any{"enabled": "yes"}, schema)
	if err == nil {
		t.Fatal("expected validation error for string instead of boolean")
	}
}

func TestValidateOutput_Valid(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "object"},
	}
	result := map[string]any{
		"step1": map[string]any{"data": "value"},
	}

	if err := ValidateOutput(result, schema); err != nil {
		t.Fatalf("expected valid output to pass, got: %v", err)
	}
}

func TestValidateOutput_Invalid(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "object"},
	}
	result := map[string]any{
		"step1": "not-an-object",
	}

	err := ValidateOutput(result, schema)
	if err == nil {
		t.Fatal("expected output validation error")
	}
	if !strings.Contains(err.Error(), "output validation failed") {
		t.Errorf("error should have correct prefix, got: %v", err)
	}
}
