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

package sempv1

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
)

// See internal/composite/executor_span_test.go for why the provider is
// installed exactly once per test binary and callers filter by span name.
var (
	sharedSpanRecorder *tracetest.SpanRecorder
	installSpanTracer  sync.Once
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installSpanTracer.Do(func() {
		sharedSpanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedSpanRecorder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		))
	})
	// Reset per test: the recorder is shared (only the first
	// SetTracerProvider is honored, so it cannot be swapped per test), and
	// without this it accumulates across tests and across `go test -count`
	// iterations — a count assertion then passes on the first run and fails on
	// the second. Safe because nothing in this package calls t.Parallel.
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// panickingAuthenticator panics from AddAuth, which Execute calls after the
// span has been started.
type panickingAuthenticator struct{}

func (panickingAuthenticator) AddAuth(context.Context, *http.Request) error {
	panic("deliberate panic below sempv1 Execute")
}

func (panickingAuthenticator) HandleAuthFailure(context.Context, http.Header) auth.AuthFailureResult {
	return auth.AuthFailureResult{}
}

// A panic below Execute must not leave the span claiming success. Go does not
// populate named returns on an unwound panic, so `err` is still nil when the
// deferred span-closing function runs: without the recover in Execute the span
// would be exported as `outcome: success` moments before the panic takes the
// process down. Mirrors tokenexchange.Exchange, where the same defect was
// caught in review, and the sempv2 and composite equivalents.
func TestExecute_PanicMarksSpanAsErrorAndRepanics(t *testing.T) {
	sr := recordSpans(t)
	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		t.Fatal(err)
	}
	client := newTestClientWithURL(t, "http://broker.invalid:8080", panickingAuthenticator{}, jar)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("panic did not propagate out of Execute; the recover must re-panic, not swallow")
			}
		}()
		_, _ = client.Execute(context.Background(), "<rpc><show><version/></show></rpc>")
	}()

	var span sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "semp.request" {
			span = s
		}
	}
	if span == nil {
		t.Fatal("no ended span named semp.request")
	}
	attrs := map[string]string{}
	for _, kv := range span.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["outcome"] != "error" {
		t.Errorf("span outcome = %q, want %q: a panicked call must not be exported as a success", attrs["outcome"], "error")
	}
	if attrs["semp.version"] != "v1" {
		t.Errorf("semp.version = %q, want v1 (the attribute set must still be recorded on the panic path)", attrs["semp.version"])
	}
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status = %v, want Error", got)
	}
	if desc := span.Status().Description; desc != "" {
		t.Errorf("span status description = %q, want empty (the panic value is unvouched text)", desc)
	}
}
