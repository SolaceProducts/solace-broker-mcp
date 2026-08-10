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

package sempv1_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
)

func TestError_Error(t *testing.T) {
	cases := []struct {
		name         string
		err          *sempv1.Error
		wantContains []string
	}{
		{
			name: "HTTP 401",
			err: &sempv1.Error{
				Kind:       sempv1.ErrorKindHTTP,
				StatusCode: 401,
				Body:       []byte("unauthorized"),
			},
			wantContains: []string{"sempv1", "http", "status=401"},
		},
		{
			name: "parse error with message",
			err: &sempv1.Error{
				Kind:       sempv1.ErrorKindParse,
				StatusCode: 200,
				Message:    "invalid message",
			},
			wantContains: []string{"sempv1", "parse", "invalid message", "status=200"},
		},
		{
			name: "permission error with message",
			err: &sempv1.Error{
				Kind:       sempv1.ErrorKindPermission,
				StatusCode: 200,
				Message:    "not authorized",
			},
			wantContains: []string{"sempv1", "permission", "not authorized", "status=200"},
		},
		{
			name: "limit error with message",
			err: &sempv1.Error{
				Kind:       sempv1.ErrorKindLimit,
				StatusCode: 200,
				Message:    "response too big",
			},
			wantContains: []string{"sempv1", "limit", "response too big", "status=200"},
		},
		{
			name: "execute-fail with reason code",
			err: &sempv1.Error{
				Kind:       sempv1.ErrorKindExecuteFail,
				StatusCode: 200,
				Message:    "config invalid",
				ReasonCode: 431,
			},
			wantContains: []string{"sempv1", "execute-fail", "config invalid", "status=200", "reason=431"},
		},
		{
			name:         "unknown zero-value kind",
			err:          &sempv1.Error{},
			wantContains: []string{"sempv1", "unknown", "status=0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, expected to contain %q", got, want)
				}
			}
		})
	}
}

func TestError_Error_Nil(t *testing.T) {
	var e *sempv1.Error
	got := e.Error()
	if got == "" {
		t.Error("nil *Error.Error() returned empty string; expected sentinel output")
	}
	if !strings.Contains(got, "sempv1") {
		t.Errorf("nil *Error.Error() = %q, expected sempv1 prefix", got)
	}
}

func TestError_ErrorsAs(t *testing.T) {
	inner := &sempv1.Error{
		Kind:       sempv1.ErrorKindExecuteFail,
		StatusCode: 200,
		Message:    "broker rejected",
		ReasonCode: 17,
	}
	wrapped := fmt.Errorf("tool context: %w", inner)

	var got *sempv1.Error
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As failed to extract *sempv1.Error from wrapped error")
	}
	if got.Kind != sempv1.ErrorKindExecuteFail {
		t.Errorf("Kind = %v, want ErrorKindExecuteFail", got.Kind)
	}
	if got.ReasonCode != 17 {
		t.Errorf("ReasonCode = %d, want 17", got.ReasonCode)
	}
	if got.Message != "broker rejected" {
		t.Errorf("Message = %q, want %q", got.Message, "broker rejected")
	}
}

func TestErrorKind_String(t *testing.T) {
	cases := []struct {
		kind sempv1.ErrorKind
		want string
	}{
		{sempv1.ErrorKindUnknown, "unknown"},
		{sempv1.ErrorKindHTTP, "http"},
		{sempv1.ErrorKindParse, "parse"},
		{sempv1.ErrorKindPermission, "permission"},
		{sempv1.ErrorKindLimit, "limit"},
		{sempv1.ErrorKindExecuteFail, "execute-fail"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.kind.String(); got != tc.want {
				t.Errorf("ErrorKind(%d).String() = %q, want %q", int(tc.kind), got, tc.want)
			}
		})
	}
}
