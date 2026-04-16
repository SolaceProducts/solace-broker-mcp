package sempv2_test

import (
	"context"
	"encoding/json"
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
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
	}
	client := sempv2.NewHTTPClient(brokerCfg, sempCfg)
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
	sempCfg := &config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second}
	bearerClient := sempv2.NewHTTPClient(brokerCfg, sempCfg)

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
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: time.Second,
	}
	client := sempv2.NewHTTPClient(brokerCfg, sempCfg)

	op := &sempv2.Operation{
		ID:     "testOp",
		Method: "GET",
		Path:   "/SEMP/v2/monitor/test",
	}

	_, err := client.Execute(context.Background(), op, map[string]any{})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
