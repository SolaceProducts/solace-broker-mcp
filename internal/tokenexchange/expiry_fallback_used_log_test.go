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

package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

const expiryFallbackUsedLogMsg = "broker OAuth token expiry fallback supplied a lifetime"

func successJSONNullExpiry(accessToken string) string {
	return fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","issued_token_type":%q,"expires_in":null}`,
		accessToken, URNTokenTypeAccessToken)
}

func fallbackParams(t *testing.T, tokenURL string, fallback time.Duration) Params {
	t.Helper()
	p := validParams(t)
	p.TokenURL = tokenURL
	p.TokenExpiryFallback = fallback
	return p
}

func idpServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func usageRecords(t *testing.T, logs *jsonLogBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range logs.records(t) {
		if r["msg"] == expiryFallbackUsedLogMsg {
			out = append(out, r)
		}
	}
	return out
}

func TestExchange_ExpiryFallbackUsedINFO_FirstFireVisibleAtInfo(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	cases := []struct {
		name string
		body string
	}{
		{name: "omit expires_in", body: successJSONWithoutExpiry("omit-tok")},
		{name: "zero expires_in", body: successJSON("zero-tok", 0)},
		{name: "null expires_in", body: successJSONNullExpiry("null-tok")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureJSONLogsAt(t, slog.LevelInfo)
			srv := idpServer(t, tc.body)
			e, err := New(fallbackParams(t, srv.URL, time.Hour))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			input := validInput()
			input.BrokerAlias = "first-fire-" + tc.name
			if _, err := e.Exchange(context.Background(), input); err != nil {
				t.Fatalf("Exchange: %v", err)
			}

			got := usageRecords(t, logs)
			if len(got) != 1 {
				t.Fatalf("usage INFO count = %d, want 1 after first live fallback (records=%v)", len(got), logs.records(t))
			}
			rec := got[0]
			if rec["level"] != "INFO" {
				t.Errorf("level = %v, want INFO", rec["level"])
			}
			if got, ok := rec["expiry_fallback"].(float64); !ok || got != float64(time.Hour) {
				t.Errorf("expiry_fallback = %v (float=%v), want %d", rec["expiry_fallback"], ok, time.Hour)
			}
			if _, ok := rec["broker"]; ok {
				t.Errorf("broker present on usage INFO: %v", rec["broker"])
			}
			if _, ok := rec["correlation_id"]; ok {
				t.Errorf("correlation_id present on usage INFO: %v", rec["correlation_id"])
			}
			if findMsg(logs.records(t), "identity provider issued broker token") != nil {
				t.Error("issued Debug line visible at Info handler; used_fallback must stay Debug")
			}
		})
	}
}

func TestExchange_ExpiryFallbackUsedINFO_SecondLiveDoesNotRepeat(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)
	srv := idpServer(t, successJSONWithoutExpiry("again-tok"))
	e, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := validInput()
	first.BrokerAlias = "once-a"
	first.SubjectToken = "subject-a"
	if _, err := e.Exchange(context.Background(), first); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	second := validInput()
	second.BrokerAlias = "once-b"
	second.SubjectToken = "subject-b"
	if _, err := e.Exchange(context.Background(), second); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}

	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 1 {
		t.Errorf("usage INFO count = %d, want 1 after two live fallback exchanges", n)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_CacheHitDoesNotRepeat(t *testing.T) {
	// NOT parallel: Debug capture so a second issued line would be visible.
	logs := captureJSONLogs(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSONWithoutExpiry("cached-fb"))
	}))
	t.Cleanup(srv.Close)

	e, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := validInput()
	input.BrokerAlias = "cache-hit-fb"
	if _, err := e.Exchange(context.Background(), input); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if _, err := e.Exchange(context.Background(), input); err != nil {
		t.Fatalf("second Exchange: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("IdP calls = %d, want 1", got)
	}
	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 1 {
		t.Errorf("usage INFO count = %d, want 1 after cache hit", n)
	}
	issued := 0
	for _, r := range forBroker(logs.records(t), input.BrokerAlias) {
		if r["msg"] == "identity provider issued broker token" {
			issued++
		}
	}
	if issued != 1 {
		t.Errorf("issued Debug count = %d, want 1 (cache hit must not parse again)", issued)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_PositiveIdPNeverEmits(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)
	srv := idpServer(t, successJSON("idp-tok", 3600))
	e, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Exchange(context.Background(), validInput()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 0 {
		t.Errorf("usage INFO count = %d, want 0 when IdP expires_in is positive", n)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_FailClosedAndInvalidNeverEmit(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)

	t.Run("omit without fallback", func(t *testing.T) {
		srv := idpServer(t, successJSONWithoutExpiry("no-fb"))
		e, err := New(fallbackParams(t, srv.URL, 0))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = e.Exchange(context.Background(), validInput())
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Exchange error = %v, want ErrInvalidResponse", err)
		}
	})
	t.Run("negative expires_in", func(t *testing.T) {
		srv := idpServer(t, successJSON("neg", -7))
		e, err := New(fallbackParams(t, srv.URL, time.Hour))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = e.Exchange(context.Background(), validInput())
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Exchange error = %v, want ErrInvalidResponse", err)
		}
	})
	t.Run("overflow expires_in", func(t *testing.T) {
		srv := idpServer(t, successJSON("over", maxExpiresInSeconds+1))
		e, err := New(fallbackParams(t, srv.URL, time.Hour))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = e.Exchange(context.Background(), validInput())
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Exchange error = %v, want ErrInvalidResponse", err)
		}
	})

	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 0 {
		t.Errorf("usage INFO count = %d, want 0 on fail-closed and invalid expires_in", n)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_NoCorrelationID(t *testing.T) {
	// NOT parallel: captureJSONLogs swaps the global logger.
	logs := captureJSONLogs(t)
	srv := idpServer(t, successJSONWithoutExpiry("corr-fb"))
	e, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	input := validInput()
	input.BrokerAlias = "corr-fb-broker"
	ctx := correlation.With(context.Background(), "fallback-corr-id")
	if _, err := e.Exchange(ctx, input); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	usage := findMsg(logs.records(t), expiryFallbackUsedLogMsg)
	if usage == nil {
		t.Fatal("usage INFO not captured")
	}
	if v, ok := usage["correlation_id"]; ok {
		t.Errorf("usage INFO correlation_id = %v, want key absent", v)
	}
	issued := findMsg(forBroker(logs.records(t), input.BrokerAlias), "identity provider issued broker token")
	if issued == nil {
		t.Fatal("issued Debug not captured")
	}
	if got, _ := issued["correlation_id"].(string); got != "fallback-corr-id" {
		t.Errorf("issued Debug correlation_id = %q, want fallback-corr-id", got)
	}
	if got, ok := issued["used_fallback"].(bool); !ok || !got {
		t.Errorf("used_fallback = %v (bool=%v), want true", issued["used_fallback"], ok)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_PerExchangerNotProcessGlobal(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)
	srv := idpServer(t, successJSONWithoutExpiry("two-ex"))
	a, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	b, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	inA := validInput()
	inA.BrokerAlias = "exchanger-a"
	inB := validInput()
	inB.BrokerAlias = "exchanger-b"
	if _, err := a.Exchange(context.Background(), inA); err != nil {
		t.Fatalf("Exchange a: %v", err)
	}
	if _, err := b.Exchange(context.Background(), inB); err != nil {
		t.Fatalf("Exchange b: %v", err)
	}
	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 2 {
		t.Errorf("usage INFO count = %d, want 2 (once per Exchanger)", n)
	}
}

func TestNew_ExpiryFallbackUsedINFO_AbsentUntilExchange(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)
	if _, err := New(fallbackParams(t, "https://idp.example.com/token", time.Hour)); err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 0 {
		t.Errorf("usage INFO count = %d, want 0 at New (armed is a different line in cmd/server)", n)
	}
}

func TestExchange_ExpiryFallbackUsedINFO_ConcurrentFirstFireOnce(t *testing.T) {
	// NOT parallel: captureJSONLogsAt swaps the global logger.
	logs := captureJSONLogsAt(t, slog.LevelInfo)
	srv := idpServer(t, successJSONWithoutExpiry("race-tok"))
	e, err := New(fallbackParams(t, srv.URL, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i, alias := range []string{"race-a", "race-b"} {
		wg.Add(1)
		go func(i int, alias string) {
			defer wg.Done()
			in := validInput()
			in.BrokerAlias = alias
			in.SubjectToken = fmt.Sprintf("subject-%d", i)
			if _, err := e.Exchange(context.Background(), in); err != nil {
				errCh <- err
			}
		}(i, alias)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("Exchange: %v", err)
	}
	if n := countMsg(logs.records(t), expiryFallbackUsedLogMsg); n != 1 {
		t.Errorf("usage INFO count = %d, want 1 under concurrent first fire", n)
	}
}
