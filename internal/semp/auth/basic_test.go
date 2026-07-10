package auth

import (
	"context"
	"errors"
	"testing"
)

// recordingJar records Clear() invocations and can be configured to return
// an error from Clear() to exercise the failure branch of HandleAuthFailure.
type recordingJar struct {
	clearErr   error
	clearCalls int
}

func (r *recordingJar) Clear() error {
	r.clearCalls++
	return r.clearErr
}

// TestNewBasicAuthenticator_PanicsOnNilJar pins the constructor contract:
// a nil jar is a wiring bug and must fail fast at construction, not later
// at first 401 recovery attempt. Same rationale as
// NewOAuthAuthenticator's nil-exchanger panic.
//
// The panic message is part of the assertion so a future refactor that
// panics for a different reason (bad username, etc.) can't quietly pass
// this test while dropping the nil-jar guard.
func TestNewBasicAuthenticator_PanicsOnNilJar(t *testing.T) {
	const wantMsg = "NewBasicAuthenticator: jar must be non-nil"
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewBasicAuthenticator(_, _, _, nil) did not panic")
		}
		got, ok := r.(string)
		if !ok || got != wantMsg {
			t.Fatalf("panic value = %#v, want string %q", r, wantMsg)
		}
	}()
	NewBasicAuthenticator("admin", "s3cret", "test-broker", nil)
}

// TestNewBasicAuthenticator_TypedNilJar_DoesNotPanic pins the accepted
// limit of the nil-jar guard: a typed-nil implementation (Go's classic
// "nil interface vs. nil concrete" gotcha — see
// https://go.dev/doc/faq#nil_error) produces a non-nil interface value
// and slips past the == nil check.
//
// This is intentional and matches the ecosystem convention: defend at
// the producer, not the consumer. Production wiring (newCookieJar →
// newAuthenticator) never emits a typed-nil, so the case is unreachable
// in production. If we ever adopt reflect-based detection or a linter
// that upgrades this to a compile-time error, this test will fail — and
// that failure is the intended signal that we're deliberately changing
// the contract.
func TestNewBasicAuthenticator_TypedNilJar_DoesNotPanic(t *testing.T) {
	var typedNil *recordingJar // nil pointer, but wrapping it in the interface produces a non-nil header
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewBasicAuthenticator with typed-nil jar panicked unexpectedly: %v", r)
		}
	}()
	_ = NewBasicAuthenticator("admin", "s3cret", "test-broker", typedNil)
}

// TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue pins
// the happy-path contract: on a 401, HandleAuthFailure calls jar.Clear()
// exactly once and returns retry=true so the Sender re-sends with fresh
// Basic credentials. Covered transitively by resilience.Sender tests, but
// isolating it here defends the auth-side contract against refactors of
// the retry loop that might silently drop the Clear() call.
func TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue(t *testing.T) {
	jar := &recordingJar{}
	a := NewBasicAuthenticator("admin", "s3cret", "test-broker", jar)

	if !a.HandleAuthFailure(context.Background(), nil) {
		t.Error("HandleAuthFailure returned false, want true on successful clear")
	}
	if jar.clearCalls != 1 {
		t.Errorf("jar.Clear() called %d times, want 1", jar.clearCalls)
	}
}

// TestBasicAuthenticator_HandleAuthFailure_JarClearError pins the failure
// branch: if jar.Clear() fails, HandleAuthFailure returns retry=false so
// the Sender does not retry with a possibly-inconsistent jar. This branch
// was previously untested at any level.
func TestBasicAuthenticator_HandleAuthFailure_JarClearError(t *testing.T) {
	sentinel := errors.New("clear failed")
	jar := &recordingJar{clearErr: sentinel}
	a := NewBasicAuthenticator("admin", "s3cret", "test-broker", jar)

	if a.HandleAuthFailure(context.Background(), nil) {
		t.Error("HandleAuthFailure returned true, want false when jar.Clear() fails")
	}
	if jar.clearCalls != 1 {
		t.Errorf("jar.Clear() called %d times, want 1", jar.clearCalls)
	}
}
