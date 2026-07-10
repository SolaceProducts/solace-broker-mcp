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

// TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue pins
// the happy-path contract: on a 401, HandleAuthFailure calls jar.Clear()
// exactly once and returns retry=true so the Sender re-sends with fresh
// Basic credentials. Covered transitively by resilience.Sender tests, but
// isolating it here defends the auth-side contract against refactors of
// the retry loop that might silently drop the Clear() call.
func TestBasicAuthenticator_HandleAuthFailure_ClearsJarAndReturnsTrue(t *testing.T) {
	jar := &recordingJar{}
	a := NewBasicAuthenticator("admin", "s3cret", jar)

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
	a := NewBasicAuthenticator("admin", "s3cret", jar)

	if a.HandleAuthFailure(context.Background(), nil) {
		t.Error("HandleAuthFailure returned true, want false when jar.Clear() fails")
	}
	if jar.clearCalls != 1 {
		t.Errorf("jar.Clear() called %d times, want 1", jar.clearCalls)
	}
}
