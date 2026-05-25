package sempv1

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
)

// newTestClientWith returns an HTTPClient pointed at srv using the supplied
// AuthConfig. The caller is responsible for srv.Close().
func newTestClientWith(t *testing.T, srv *httptest.Server, auth config.AuthConfig) *HTTPClient {
	t.Helper()

	brokerCfg := &config.BrokerConfig{
		URL:  srv.URL,
		Auth: auth,
	}
	retries := 1
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 2 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}

	client, err := NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}
	return client
}

// newTestClient is a convenience wrapper that uses basic auth with canonical
// "user"/"pass" credentials. Tests that care about auth should use
// newTestClientWith directly and supply their own AuthConfig.
func newTestClient(t *testing.T, srv *httptest.Server) *HTTPClient {
	t.Helper()
	return newTestClientWith(t, srv, config.AuthConfig{
		Mode:     config.AuthModeBasic,
		Username: "user",
		Password: "pass",
	})
}

const successEnvelope = `<rpc-reply><rpc><show><version/></show></rpc><execute-result code="ok"/></rpc-reply>`

// TestExecute_OversizedResponseBody_ReturnsTypedError verifies that a broker
// (or MITM) streaming more than MaxSEMPResponseBytes fails fast with
// ErrResponseTooLarge rather than OOMing the process.
func TestExecute_OversizedResponseBody_ReturnsTypedError(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 64*1024) // 64 KiB
	totalBytes := defaults.MaxSEMPResponseBytes + len(chunk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stream chunks until we've sent more than the cap. Using a streaming
		// write avoids allocating 16+ MiB in a single Go buffer.
		written := 0
		for written < totalBytes {
			n, err := w.Write(chunk)
			if err != nil {
				return // client disconnected, which is what we expect
			}
			written += n
		}
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Execute(context.Background(), `<rpc/>`)
	if err == nil {
		t.Fatal("expected error for oversized response body, got nil")
	}
	if !errors.Is(err, resilience.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(_, resilience.ErrResponseTooLarge)", err)
	}
}

// TestExecute_RequestConstruction asserts that Execute builds the HTTP request
// with the correct method, path, content type, user agent, body, and auth
// header for both supported auth modes. It also verifies context cancellation
// aborts the in-flight request.
func TestExecute_RequestConstruction(t *testing.T) {
	t.Run("basic auth sets Authorization: Basic header", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(successEnvelope))
		}))
		defer srv.Close()

		client := newTestClientWith(t, srv, config.AuthConfig{
			Mode:     config.AuthModeBasic,
			Username: "user",
			Password: "pass",
		})

		_, err := client.Execute(context.Background(), `<rpc><show/></rpc>`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// base64("user:pass") == "dXNlcjpwYXNz"
		wantAuth := "Basic dXNlcjpwYXNz"
		if gotAuth != wantAuth {
			t.Errorf("Authorization header mismatch\n got:  %q\n want: %q", gotAuth, wantAuth)
		}
	})

	t.Run("bearer auth sets Authorization: Bearer header", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(successEnvelope))
		}))
		defer srv.Close()

		client := newTestClientWith(t, srv, config.AuthConfig{
			Mode:  config.AuthModeBearer,
			Token: "abc123",
		})

		_, err := client.Execute(context.Background(), `<rpc><show/></rpc>`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantAuth := "Bearer abc123"
		if gotAuth != wantAuth {
			t.Errorf("Authorization header mismatch\n got:  %q\n want: %q", gotAuth, wantAuth)
		}
	})

	t.Run("headers, method, path, and body are set correctly", func(t *testing.T) {
		var (
			gotMethod      string
			gotPath        string
			gotContentType string
			gotUserAgent   string
			gotBody        []byte
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotContentType = r.Header.Get("Content-Type")
			gotUserAgent = r.Header.Get("User-Agent")
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(successEnvelope))
		}))
		defer srv.Close()

		client := newTestClient(t, srv)

		wantBody := `<rpc><show><version/></show></rpc>`
		_, err := client.Execute(context.Background(), wantBody)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if gotMethod != http.MethodPost {
			t.Errorf("method mismatch: got %q, want POST", gotMethod)
		}
		if gotPath != "/SEMP" {
			t.Errorf("path mismatch: got %q, want /SEMP", gotPath)
		}
		if gotContentType != "application/xml" {
			t.Errorf("content-type mismatch: got %q, want application/xml", gotContentType)
		}
		wantUA := "solace/broker-mcp-server/" + version.Version()
		if gotUserAgent != wantUA {
			t.Errorf("user-agent mismatch\n got:  %q\n want: %q", gotUserAgent, wantUA)
		}
		if string(gotBody) != wantBody {
			t.Errorf("body mismatch\n got:  %q\n want: %q", string(gotBody), wantBody)
		}
	})

	t.Run("context cancellation aborts in-flight request", func(t *testing.T) {
		// Server waits for either client disconnect or a short fallback so
		// that srv.Close() at test end can't hang. The handler exiting on its
		// own is a test-cleanup concern; the assertion we care about below
		// is that the client returns quickly after cancellation.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
		}))
		defer srv.Close()

		client := newTestClient(t, srv)

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		_, err := client.Execute(ctx, `<rpc><show/></rpc>`)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatalf("expected cancellation error, got nil")
		}
		// Returning within a second proves cancellation propagated; the
		// configured client timeout is 2s and we'd otherwise wait for that.
		if elapsed > 1*time.Second {
			t.Errorf("cancellation took too long: %s (client timeout is 2s)", elapsed)
		}
		// Transport errors should NOT be *sempv1.Error — callers need to
		// branch on errors.Is(err, context.Canceled) etc.
		var sempErr *Error
		if errors.As(err, &sempErr) {
			t.Errorf("expected transport error, got *sempv1.Error: %v", sempErr)
		}
	})
}

// TestExecute_ResponseClassification asserts that Execute correctly converts
// every response shape into the expected return value: *Result on success,
// *sempv1.Error for HTTP-layer and envelope-layer failures.
func TestExecute_ResponseClassification(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		body           string
		wantInnerXML   string // set for success; empty means expect *Error
		wantKind       ErrorKind
		wantStatusCode int
		wantMessage    string
		wantReasonCode int
	}{
		{
			name:         "200 with success envelope returns Result",
			status:       http.StatusOK,
			body:         successEnvelope,
			wantInnerXML: `<show><version/></show>`,
		},
		{
			name:           "200 with parse-error returns *Error{Kind:Parse}",
			status:         http.StatusOK,
			body:           `<rpc-reply><parse-error>invalid message</parse-error></rpc-reply>`,
			wantKind:       ErrorKindParse,
			wantStatusCode: 200,
			wantMessage:    "invalid message",
		},
		{
			name:           "200 with permission-error returns *Error{Kind:Permission}",
			status:         http.StatusOK,
			body:           `<rpc-reply><permission-error>not authorized</permission-error></rpc-reply>`,
			wantKind:       ErrorKindPermission,
			wantStatusCode: 200,
			wantMessage:    "not authorized",
		},
		{
			name:           "200 with limit-error returns *Error{Kind:Limit}",
			status:         http.StatusOK,
			body:           `<rpc-reply><limit-error>response too big</limit-error></rpc-reply>`,
			wantKind:       ErrorKindLimit,
			wantStatusCode: 200,
			wantMessage:    "response too big",
		},
		{
			name:           "200 with execute-result code=fail returns *Error{Kind:ExecuteFail}",
			status:         http.StatusOK,
			body:           `<rpc-reply><execute-result code="fail" reason="foo" reasonCode="431"/></rpc-reply>`,
			wantKind:       ErrorKindExecuteFail,
			wantStatusCode: 200,
			wantMessage:    "foo",
			wantReasonCode: 431,
		},
		{
			name:           "401 returns *Error{Kind:HTTP, StatusCode:401}",
			status:         http.StatusUnauthorized,
			body:           "unauthorized",
			wantKind:       ErrorKindHTTP,
			wantStatusCode: 401,
		},
		{
			name:           "403 returns *Error{Kind:HTTP, StatusCode:403}",
			status:         http.StatusForbidden,
			body:           "forbidden",
			wantKind:       ErrorKindHTTP,
			wantStatusCode: 403,
		},
		{
			name:           "404 returns *Error{Kind:HTTP, StatusCode:404}",
			status:         http.StatusNotFound,
			body:           "not found",
			wantKind:       ErrorKindHTTP,
			wantStatusCode: 404,
		},
		{
			name:           "500 returns *Error{Kind:HTTP, StatusCode:500}",
			status:         http.StatusInternalServerError,
			body:           "internal error",
			wantKind:       ErrorKindHTTP,
			wantStatusCode: 500,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			client := newTestClient(t, srv)
			result, err := client.Execute(context.Background(), `<rpc><show/></rpc>`)

			if tc.wantInnerXML != "" {
				// Success case.
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
				if string(result.InnerXML) != tc.wantInnerXML {
					t.Errorf("inner XML mismatch\n got:  %q\n want: %q", string(result.InnerXML), tc.wantInnerXML)
				}
				return
			}

			// Failure case.
			if result != nil {
				t.Errorf("expected nil result, got %+v", result)
			}
			var sempErr *Error
			if !errors.As(err, &sempErr) {
				t.Fatalf("expected *sempv1.Error, got %T: %v", err, err)
			}
			if sempErr.Kind != tc.wantKind {
				t.Errorf("Kind mismatch: got %v, want %v", sempErr.Kind, tc.wantKind)
			}
			if sempErr.StatusCode != tc.wantStatusCode {
				t.Errorf("StatusCode mismatch: got %d, want %d", sempErr.StatusCode, tc.wantStatusCode)
			}
			if sempErr.Message != tc.wantMessage {
				t.Errorf("Message mismatch: got %q, want %q", sempErr.Message, tc.wantMessage)
			}
			if sempErr.ReasonCode != tc.wantReasonCode {
				t.Errorf("ReasonCode mismatch: got %d, want %d", sempErr.ReasonCode, tc.wantReasonCode)
			}
			// HTTP-layer errors preserve the raw body for debugging.
			if tc.wantKind == ErrorKindHTTP && string(sempErr.Body) != tc.body {
				t.Errorf("Body mismatch\n got:  %q\n want: %q", string(sempErr.Body), tc.body)
			}
		})
	}
}

// TestExecute_NetworkError verifies that a transport-level failure (the
// server hangs up on us before responding) surfaces as a wrapped error that
// is NOT a *sempv1.Error.
func TestExecute_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack failed: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	result, err := client.Execute(context.Background(), `<rpc><show/></rpc>`)

	if err == nil {
		t.Fatalf("expected transport error, got nil (result=%+v)", result)
	}
	var sempErr *Error
	if errors.As(err, &sempErr) {
		t.Errorf("expected transport error, got *sempv1.Error: %v", sempErr)
	}
	if !strings.Contains(err.Error(), "executing SEMPv1 request") {
		t.Errorf("expected wrapped transport error, got %q", err.Error())
	}
}

// TestExecute_InputValidation verifies that nil ctx and empty xml are
// rejected with a typed *sempv1.Error{Kind:Unknown} before any network I/O.
func TestExecute_InputValidation(t *testing.T) {
	// A handler that fails the test if it's ever called — catches regressions
	// where validation stops guarding the network call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called when inputs are invalid")
	}))
	defer srv.Close()

	client := newTestClient(t, srv)

	t.Run("nil context", func(t *testing.T) {
		//nolint:staticcheck // SA1012 — intentionally passing nil to verify the guard clause.
		_, err := client.Execute(nil, `<rpc><show/></rpc>`)
		var sempErr *Error
		if !errors.As(err, &sempErr) {
			t.Fatalf("expected *sempv1.Error, got %T: %v", err, err)
		}
		if sempErr.Kind != ErrorKindUnknown {
			t.Errorf("Kind mismatch: got %v, want ErrorKindUnknown", sempErr.Kind)
		}
		if sempErr.Message != "nil context" {
			t.Errorf("Message mismatch: got %q, want %q", sempErr.Message, "nil context")
		}
	})

	t.Run("empty xml", func(t *testing.T) {
		_, err := client.Execute(context.Background(), "")
		var sempErr *Error
		if !errors.As(err, &sempErr) {
			t.Fatalf("expected *sempv1.Error, got %T: %v", err, err)
		}
		if sempErr.Kind != ErrorKindUnknown {
			t.Errorf("Kind mismatch: got %v, want ErrorKindUnknown", sempErr.Kind)
		}
		if sempErr.Message != "empty xml" {
			t.Errorf("Message mismatch: got %q, want %q", sempErr.Message, "empty xml")
		}
	})
}

// TestHTTPClient_LogValue_ExcludesCredentials verifies that slog.Any on an
// *HTTPClient exposes the broker URL but NEVER the auth credentials. This is
// the secure-logging guarantee from docs/secure-logging-rules.md Rule 2 — if
// a future maintainer accidentally logs the client struct, the credentials
// stay out of the log stream.
//
// We install a JSON slog handler that writes to a buffer, log the client
// under a "client" key, and assert:
//   - base_url is present
//   - none of the sensitive field values appear anywhere in the output
func TestHTTPClient_LogValue_ExcludesCredentials(t *testing.T) {
	const (
		secretUser  = "SECRET_USERNAME_VAL"
		secretPass  = "SECRET_PASSWORD_VAL"
		secretToken = "SECRET_TOKEN_VAL"
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	client := newTestClientWith(t, srv, config.AuthConfig{
		Mode:     config.AuthModeBasic,
		Username: secretUser,
		Password: secretPass,
		Token:    secretToken, // not used by basic mode, but set to prove it also doesn't leak
	})

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	slog.Info("broker", slog.Any("client", client))

	out := buf.String()

	if !strings.Contains(out, "base_url") {
		t.Errorf("expected base_url in log output, got: %s", out)
	}
	if !strings.Contains(out, srv.URL) {
		t.Errorf("expected base URL %q in log output, got: %s", srv.URL, out)
	}

	for _, secret := range []string{secretUser, secretPass, secretToken} {
		if strings.Contains(out, secret) {
			t.Errorf("credential %q leaked into log output:\n%s", secret, out)
		}
	}
}
