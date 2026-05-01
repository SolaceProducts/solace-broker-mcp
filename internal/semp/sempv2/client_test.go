package sempv2_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*sempv2.HTTPClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}
	return client, server
}

func testOp(method string, params ...sempv2.Parameter) *sempv2.Operation {
	return &sempv2.Operation{
		ID:         "testOp",
		Method:     method,
		Path:       "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}",
		Parameters: params,
	}
}

func TestClient_Execute_Success(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"queueName": "test-queue"}})
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	result, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "default",
		"queueName":  "test-queue",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", result.StatusCode)
	}

	data, ok := result.Data["data"].(map[string]any)
	if !ok {
		t.Fatal("result.Data[\"data\"] is not a map")
	}
	if data["queueName"] != "test-queue" {
		t.Errorf("queueName = %v, want test-queue", data["queueName"])
	}
}

func TestClient_Execute_PathParams(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/msgVpns/my-vpn/queues/my-queue") {
			t.Errorf("URL path = %q, expected path params substituted", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := testOp("GET",
		sempv2.Parameter{Name: "msgVpnName", In: "path"},
		sempv2.Parameter{Name: "queueName", In: "path"},
	)

	_, err := client.Execute(context.Background(), op, map[string]any{
		"msgVpnName": "my-vpn",
		"queueName":  "my-queue",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_QueryParams(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("select") != "queueName,spoolUsage" {
			t.Errorf("query select = %q, want queueName,spoolUsage", r.URL.Query().Get("select"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
		Parameters: []sempv2.Parameter{
			{Name: "select", In: "query"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"select": "queueName,spoolUsage",
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_RequestBody(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("Method = %s, want PUT", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding body: %v", err)
		}
		if body["replayLogName"] != "replay-log-1" {
			t.Errorf("body.replayLogName = %v, want replay-log-1", body["replayLogName"])
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "PUT",
		Path:   "/SEMP/v2/action/test",
		Parameters: []sempv2.Parameter{
			{Name: "body", In: "body"},
		},
	}

	_, err := client.Execute(context.Background(), op, map[string]any{
		"body": `{"replayLogName": "replay-log-1"}`,
	})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_BasicAuth(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			t.Fatal("BasicAuth not present on request")
		}
		if user != "admin" || pass != "secret" {
			t.Errorf("BasicAuth = (%q, %q), want (admin, secret)", user, pass)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_404(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"meta":{"error":{"status":"NOT_FOUND"}}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, expected it to contain status code 404", err.Error())
	}
}

func TestClient_Execute_500(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}

	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, expected it to contain status code 500", err.Error())
	}
}

func TestClient_Execute_SEMPErrorParsed(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"meta":{"error":{"code":6,"status":"NOT_FOUND","description":"Message VPN Not Found"},"responseCode":404}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "getMsgVpn",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{"msgVpnName": "missing"})
	if err == nil {
		t.Fatal("expected error for 404 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	if sempErr.Description != "Message VPN Not Found" {
		t.Errorf("Description = %q, want %q", sempErr.Description, "Message VPN Not Found")
	}
	if sempErr.SEMPCode != 6 {
		t.Errorf("SEMPCode = %d, want 6", sempErr.SEMPCode)
	}
	if sempErr.SEMPStatus != "NOT_FOUND" {
		t.Errorf("SEMPStatus = %q, want %q", sempErr.SEMPStatus, "NOT_FOUND")
	}
	if !strings.Contains(sempErr.Error(), "Message VPN Not Found") {
		t.Errorf("Error() = %q, want it to contain the Description", sempErr.Error())
	}
}

func TestClient_Execute_SEMPErrorMalformedBody(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html>Access Denied</html>"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 403 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	// Structured fields should be zero when body is not valid JSON.
	if sempErr.Description != "" {
		t.Errorf("Description = %q, want empty for non-JSON body", sempErr.Description)
	}
	if sempErr.SEMPCode != 0 {
		t.Errorf("SEMPCode = %d, want 0 for non-JSON body", sempErr.SEMPCode)
	}
	if sempErr.Body != "<html>Access Denied</html>" {
		t.Errorf("Body = %q, want raw HTML preserved", sempErr.Body)
	}
	if !strings.Contains(sempErr.Error(), "<html>Access Denied</html>") {
		t.Errorf("Error() = %q, want it to fall back to Body", sempErr.Error())
	}
}

func TestClient_Execute_SEMPErrorPartialMeta(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"meta":{"error":{"status":"INVALID_PARAMETER"}}}`))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		t.Fatalf("expected *SEMPError, got %T: %v", err, err)
	}

	if sempErr.SEMPStatus != "INVALID_PARAMETER" {
		t.Errorf("SEMPStatus = %q, want %q", sempErr.SEMPStatus, "INVALID_PARAMETER")
	}
	if sempErr.Description != "" {
		t.Errorf("Description = %q, want empty when not in response", sempErr.Description)
	}
	if sempErr.SEMPCode != 0 {
		t.Errorf("SEMPCode = %d, want 0 when not in response", sempErr.SEMPCode)
	}
}

func TestClient_Execute_InvalidJSON(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not json</html>"))
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}

	if !strings.Contains(err.Error(), "parsing JSON") {
		t.Errorf("error = %q, expected it to mention JSON parsing", err.Error())
	}
}

func TestClient_Execute_PathEncoding(t *testing.T) {
	cases := []struct {
		name          string
		clientName    string
		wantInPath    string
		wantNotInPath string
	}{
		{
			name:       "plain value unchanged",
			clientName: "my-consumer",
			wantInPath: "/clients/my-consumer",
		},
		{
			name:          "slash encoded",
			clientName:    "app/client-1",
			wantInPath:    "/clients/app%2Fclient-1",
			wantNotInPath: "/clients/app/client-1",
		},
		{
			name:       "space encoded",
			clientName: "my client",
			wantInPath: "/clients/my%20client",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.RequestURI
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{})
			})
			defer server.Close()

			op := &sempv2.Operation{
				ID:     "testOp",
				Method: "GET",
				Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/clients/{clientName}",
				Parameters: []sempv2.Parameter{
					{Name: "msgVpnName", In: "path"},
					{Name: "clientName", In: "path"},
				},
			}

			_, _ = client.Execute(context.Background(), op, map[string]any{
				"msgVpnName": "default",
				"clientName": tc.clientName,
			})

			if !strings.Contains(gotPath, tc.wantInPath) {
				t.Errorf("path = %q, want it to contain %q", gotPath, tc.wantInPath)
			}
			if tc.wantNotInPath != "" && strings.Contains(gotPath, tc.wantNotInPath) {
				t.Errorf("path = %q, must not contain unencoded %q", gotPath, tc.wantNotInPath)
			}
		})
	}
}

func TestClient_Execute_BearerAuth(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token is sent
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer my-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"meta":{"error":{"status":"UNAUTHORIZED"}}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data": {"status": "ok"}}`))
	})
	_ = client // not using the basic auth client from newTestClient
	defer server.Close()

	// Create a bearer auth client pointing at the same test server.
	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:  "bearer",
			Token: "my-test-token",
		},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	bearerClient, err := sempv2.NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	result, err := bearerClient.Execute(context.Background(), op, map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected 200, got %d", result.StatusCode)
	}
}

func TestClient_Execute_CookieJar(t *testing.T) {
	callCount := 0
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if callCount == 0 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123"})
		} else {
			cookie, err := r.Cookie("session")
			if err != nil {
				t.Error("cookie not sent back on second request")
			} else if cookie.Value != "abc123" {
				t.Errorf("cookie value = %q, want abc123", cookie.Value)
			}
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("first Execute() error: %v", err)
	}
	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("second Execute() error: %v", err)
	}

	if callCount != 2 {
		t.Errorf("handler called %d times, want 2", callCount)
	}
}

func TestClient_Execute_UserAgent(t *testing.T) {
	client, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if !strings.HasPrefix(ua, "solace/broker-mcp-server/") {
			t.Errorf("User-Agent = %q, want prefix solace/broker-mcp-server/", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{})
	})
	defer server.Close()

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	if _, err := client.Execute(context.Background(), op, map[string]any{}); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestClient_Execute_Timeout(t *testing.T) {
	_, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	})
	defer server.Close()

	// Create a client with a very short timeout.
	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Mode:     "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	retries := 0
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	client, err := sempv2.NewHTTPClient(brokerCfg, sempCfg)
	if err != nil {
		t.Fatalf("NewHTTPClient() error: %v", err)
	}

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, execErr := client.Execute(context.Background(), op, map[string]any{})
	if execErr == nil {
		t.Fatal("expected timeout error")
	}
}
