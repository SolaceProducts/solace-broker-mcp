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
			Method:   "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := &config.SEMPConfig{
		RequestTimeoutSeconds: 5,
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

func TestClient_Execute_Timeout(t *testing.T) {
	_, server := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	})
	defer server.Close()

	// Create a client with a very short timeout.
	brokerCfg := &config.BrokerConfig{
		URL: server.URL,
		Auth: config.AuthConfig{
			Method:   "basic",
			Username: "admin",
			Password: "secret",
		},
	}
	sempCfg := &config.SEMPConfig{
		RequestTimeoutSeconds: 1,
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
