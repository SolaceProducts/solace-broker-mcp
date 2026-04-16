package sempv2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// Client executes operations against a Solace broker's SEMPv2 API.
// Implementations: HTTPClient (real), mock (tests).
// Future: cached or retry decorators wrapping this interface.
type Client interface {
	Execute(ctx context.Context, op *Operation, args map[string]any) (*Result, error)
}

// Result holds the response from a SEMP API call.
type Result struct {
	Data       map[string]any // parsed JSON response body
	StatusCode int            // HTTP status code
}

// SEMPError is a structured error returned when a SEMP API call receives a
// non-2xx HTTP response. It preserves the HTTP status code, operation ID, and
// raw response body as separate fields so callers can extract structured data
// via errors.As() instead of parsing error strings.
//
// This type is the foundation for Story 13B's TranslateSEMPError() function
// and aligns with Story 4's requirement to parse .meta.error.code and
// .meta.error.description from SEMP responses. Future fields (SEMPErrorCode,
// SEMPMessage) can be added without breaking existing callers.
type SEMPError struct {
	Operation  string // operationId that failed (e.g., "getMsgVpnQueue")
	StatusCode int    // HTTP status code (e.g., 404)
	Body       string // raw response body from broker
}

// Error implements the error interface. The output matches the previous
// fmt.Errorf format so existing error messages and test assertions are
// unchanged.
func (e *SEMPError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d: %s", e.Operation, e.StatusCode, e.Body)
}

// HTTPClient implements the Client interface by making real HTTP calls to a
// Solace broker's SEMPv2 API. It is configured per-broker with the broker's
// URL, TLS settings, and authentication credentials.
type HTTPClient struct {
	httpClient *http.Client
	baseURL    string
	username   string
	password   string
}

// LogValue implements slog.LogValuer for HTTPClient. It exposes only the base
// URL — username and password are deliberately excluded. Although these fields
// are unexported, this provides defense in depth against reflection-based logging.
// See docs/secure-logging-rules.md Rule 2.
func (c *HTTPClient) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.baseURL),
	)
}

// NewHTTPClient creates an HTTPClient configured for a specific broker.
// It sets up a per-broker HTTP transport with TLS settings and connection pool
// tuning appropriate for concurrent SEMP calls.
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) *HTTPClient {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: brokerCfg.TLSSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
	}

	return &HTTPClient{
		httpClient: &http.Client{
			Timeout:   time.Duration(sempCfg.RequestTimeoutSeconds) * time.Second,
			Transport: transport,
		},
		baseURL:  strings.TrimSuffix(brokerCfg.URL, "/"),
		username: brokerCfg.Auth.Username,
		password: brokerCfg.Auth.Password,
	}
}

// Execute makes an authenticated HTTP request to the broker's SEMPv2 API for
// the given operation and arguments. Path parameters in the operation's URL
// template are substituted from args, query parameters are appended, and body
// parameters are sent as JSON. Returns a structured Result on success or an
// error with status code context on failure.
func (c *HTTPClient) Execute(ctx context.Context, op *Operation, args map[string]any) (*Result, error) {
	reqURL := c.buildURL(op, args)

	req, err := c.buildRequest(ctx, op, reqURL, args)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", op.ID, err)
	}

	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing %s: %w", op.ID, err)
	}
	// Close the response body on any return path after this point to release
	// the underlying TCP connection back to the pool.
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response for %s: %w", op.ID, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &SEMPError{Operation: op.ID, StatusCode: resp.StatusCode, Body: string(body)}
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parsing JSON response for %s: %w", op.ID, err)
	}

	return &Result{
		Data:       data,
		StatusCode: resp.StatusCode,
	}, nil
}

// buildURL substitutes path parameter placeholders in the operation's path
// template with values from args and prepends the broker's base URL.
func (c *HTTPClient) buildURL(op *Operation, args map[string]any) string {
	path := op.Path

	for key, value := range args {
		placeholder := "{" + key + "}"
		if strings.Contains(path, placeholder) {
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(fmt.Sprintf("%v", value)))
		}
	}

	return c.baseURL + "/" + strings.TrimPrefix(path, "/")
}

// buildRequest constructs the HTTP request with query parameters and JSON body
// based on the operation's parameter definitions.
func (c *HTTPClient) buildRequest(ctx context.Context, op *Operation, reqURL string, args map[string]any) (*http.Request, error) {
	queryParams := url.Values{}
	var bodyData []byte

	for _, param := range op.Parameters {
		switch param.In {
		case "path":
			continue
		case "query":
			if val, ok := args[param.Name]; ok {
				queryParams.Set(param.Name, fmt.Sprintf("%v", val))
			}
		case "body":
			if body, ok := args["body"]; ok {
				var err error
				switch v := body.(type) {
				case string:
					bodyData = []byte(v)
				default:
					bodyData, err = json.Marshal(v)
					if err != nil {
						return nil, fmt.Errorf("marshalling body: %w", err)
					}
				}
			}
		}
	}

	if len(queryParams) > 0 {
		reqURL = reqURL + "?" + queryParams.Encode()
	}

	var bodyReader io.Reader
	if bodyData != nil {
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

// addAuth sets the Basic Auth header on the request.
func (c *HTTPClient) addAuth(req *http.Request) {
	if c.username != "" && c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
}
