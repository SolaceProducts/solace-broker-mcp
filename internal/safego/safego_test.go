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

package safego_test

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/SolaceDev/solace-broker-mcp/internal/safego"
)

// TestGo_ConvertsPanicToError is the core guarantee: a panic in a worker
// goroutine must not escape to crash the process; it must surface as the
// group's error. Without the recover in safego.Go this panic escapes the
// errgroup goroutine and crashes the test binary (errgroup does not recover).
func TestGo_ConvertsPanicToError(t *testing.T) {
	var g errgroup.Group
	safego.Go(&g, func() error {
		panic("boom")
	})

	if err := g.Wait(); err == nil {
		t.Fatal("expected an error from a panicking goroutine, got nil")
	}
}

// TestGo_PanicErrorOmitsValueText guards the secure-logging contract: the panic
// value's text must never reach the returned error (only its Go type may).
func TestGo_PanicErrorOmitsValueText(t *testing.T) {
	var g errgroup.Group
	safego.Go(&g, func() error {
		panic("super-secret-token-value")
	})

	err := g.Wait()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if strings.Contains(err.Error(), "super-secret-token-value") {
		t.Fatalf("panic value text leaked into error: %q", err.Error())
	}
}

// TestGo_PassesThroughNormalError confirms the helper is transparent on the
// non-panic error path — the fn's own error flows through unchanged.
func TestGo_PassesThroughNormalError(t *testing.T) {
	sentinel := errors.New("normal failure")

	var g errgroup.Group
	safego.Go(&g, func() error {
		return sentinel
	})

	if err := g.Wait(); !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to pass through, got %v", err)
	}
}

// TestGo_SuccessReturnsNil confirms the happy path is unaffected: fn runs and
// a nil return stays nil.
func TestGo_SuccessReturnsNil(t *testing.T) {
	var g errgroup.Group
	ran := false
	safego.Go(&g, func() error {
		ran = true
		return nil
	})

	if err := g.Wait(); err != nil {
		t.Fatalf("expected nil error on success, got %v", err)
	}
	if !ran {
		t.Fatal("fn was never executed")
	}
}
