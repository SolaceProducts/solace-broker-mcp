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
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
	"github.com/hashicorp/go-retryablehttp"
)

// Client executes operations against a Solace broker's SEMPv2 API.
// Implementations: HTTPClient (real), mock (tests).
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

// retryStateKey is the context key for per-request retry state.
type retryStateKey struct{}

// retryState tracks per-request retry decisions to enforce "retry once" limits.
// Each Execute() call creates its own instance via context, so concurrent
// requests to the same client are safe.
type retryState struct {
	auth401Retried  bool // true after first 401 re-auth attempt (basic auth only)
	other5xxRetried bool // true after first non-429/503 5xx retry
}

// opIDKey is the context key for the operation ID, used by errorHandler.
type opIDKey struct{}

// getRetryState retrieves the per-request retryState from the context.
func getRetryState(ctx context.Context) *retryState {
	if s, ok := ctx.Value(retryStateKey{}).(*retryState); ok {
		return s
	}
	return &retryState{}
}

// HTTPClient implements the Client interface by making real HTTP calls to a
// Solace broker's SEMPv2 API. It is configured per-broker with the broker's
// URL, TLS settings, and authentication credentials.
//
// Rate limiting and retry logic are handled via hashicorp/go-retryablehttp,
// matching the approach used by the Solace Terraform Provider.
type HTTPClient struct {
	retryClient *retryablehttp.Client // retryable HTTP client wrapping httpClient
	httpClient  *http.Client          // underlying client for cookie jar access
	baseURL     string
	authMode    string
	username    string
	password    string
	token       string
	rateLimiter <-chan time.Time // per-broker rate limiting; nil-safe via closed channel when disabled
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
// tuning appropriate for concurrent SEMP calls, and configures retryablehttp
// for retry logic with exponential backoff and a per-broker rate limiter.
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) (*HTTPClient, error) {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: brokerCfg.InsecureSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}

	if brokerCfg.InsecureSkipVerify {
		slog.Warn("INSECURE: TLS verification disabled for broker",
			slog.String("url", brokerCfg.URL))
	}

	httpClient := &http.Client{
		Jar:       jar,
		Timeout:   sempCfg.RequestTimeoutDuration,
		Transport: transport,
	}

	client := &HTTPClient{
		httpClient: httpClient,
		baseURL:    strings.TrimSuffix(brokerCfg.URL, "/"),
		authMode:   brokerCfg.Auth.Mode,
		username:   brokerCfg.Auth.Username,
		password:   brokerCfg.Auth.Password,
		token:      brokerCfg.Auth.Token,
	}

	// Configure retryablehttp client with the underlying http.Client.
	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = httpClient
	retryClient.RetryMax = *sempCfg.Retries
	retryClient.RetryWaitMin = sempCfg.RetryMinInterval
	retryClient.RetryWaitMax = sempCfg.RetryMaxInterval
	retryClient.CheckRetry = client.checkRetry
	retryClient.ErrorHandler = client.errorHandler
	retryClient.Logger = nil // manual logging in checkRetry; see design decisions
	client.retryClient = retryClient

	// Per-broker rate limiter: ticker-based interval enforcement.
	// When interval > 0, each Execute() blocks until the ticker fires.
	// When interval == 0, the closed channel makes receives non-blocking (no rate limit).
	if *sempCfg.RequestMinInterval > 0 {
		client.rateLimiter = time.NewTicker(*sempCfg.RequestMinInterval).C
	} else {
		ch := make(chan time.Time)
		close(ch)
		client.rateLimiter = ch
	}

	return client, nil
}

// Execute makes an authenticated HTTP request to the broker's SEMPv2 API for
// the given operation and arguments. Path parameters in the operation's URL
// template are substituted from args, query parameters are appended, and body
// parameters are sent as JSON. Returns a structured Result on success or an
// error with status code context on failure.
//
// Rate limiting is enforced before the request (new requests only, not retries).
// Retry logic is handled by retryablehttp with a custom CheckRetry policy.
func (c *HTTPClient) Execute(ctx context.Context, op *Operation, args map[string]any) (*Result, error) {
	reqURL := c.buildURL(op, args)

	req, err := c.buildRequest(ctx, op, reqURL, args)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", op.ID, err)
	}

	c.addAuth(req)

	// Rate limit: wait for the per-broker interval before sending a new request.
	// This does NOT apply to retries — retryablehttp handles those internally.
	<-c.rateLimiter
	slog.Debug("rate limiter: request permitted",
		slog.String("broker", c.baseURL),
		slog.String("operation", op.ID))

	// Attach per-request retry state and operation ID for checkRetry/errorHandler.
	ctx = context.WithValue(ctx, retryStateKey{}, &retryState{})
	ctx = context.WithValue(ctx, opIDKey{}, op.ID)
	req = req.WithContext(ctx)

	retryReq, err := retryablehttp.FromRequest(req)
	if err != nil {
		return nil, fmt.Errorf("wrapping request for %s: %w", op.ID, err)
	}

	resp, err := c.retryClient.Do(retryReq)
	if err != nil {
		return nil, fmt.Errorf("executing %s: %w", op.ID, err)
	}
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

// checkRetry is the custom retry policy for retryablehttp. It implements the
// ticket's retry rules:
//   - 401: basic auth → clear cookie jar + retry once; bearer → fail immediately
//   - 429, 503: full retries with exponential backoff
//   - Other 5xx: retry once only (likely a bug, not transient)
//   - Connection errors: delegate to retryablehttp's default policy
//   - All other status codes (4xx): no retry
func (c *HTTPClient) checkRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// Context cancellation: never retry.
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	// Connection errors: delegate to retryablehttp's default policy which
	// handles network errors, DNS failures, TLS handshake errors, etc.
	if err != nil {
		return retryablehttp.DefaultRetryPolicy(ctx, resp, err)
	}

	if resp == nil {
		return false, nil
	}

	state := getRetryState(ctx)

	switch {
	case resp.StatusCode == http.StatusUnauthorized: // 401
		if c.authMode == config.AuthModeBasic && !state.auth401Retried {
			state.auth401Retried = true
			// Stale session cookie is the most likely cause. Clear the jar so
			// the retried request re-sends Basic Auth credentials from scratch.
			jar, _ := cookiejar.New(nil)
			c.httpClient.Jar = jar
			slog.Warn("retrying: 401 received, clearing session cookies",
				slog.String("broker", c.baseURL),
				slog.String("auth_mode", c.authMode))
			return true, nil
		}
		// Bearer token (static, can't refresh) or already retried basic auth.
		return false, nil

	case resp.StatusCode == http.StatusTooManyRequests, // 429
		resp.StatusCode == http.StatusServiceUnavailable: // 503
		// Transient broker conditions: full retries with exponential backoff.
		slog.Debug("retrying: transient broker error",
			slog.String("broker", c.baseURL),
			slog.Int("status", resp.StatusCode))
		return true, nil

	case resp.StatusCode >= 500:
		// Other 5xx (500, 502, 504, etc.): likely a bug or infrastructure issue.
		// Retry once to catch momentary glitches, but don't hammer a broken broker.
		if !state.other5xxRetried {
			state.other5xxRetried = true
			slog.Debug("retrying: server error, will retry once",
				slog.String("broker", c.baseURL),
				slog.Int("status", resp.StatusCode))
			return true, nil
		}
		return false, nil

	default:
		// 4xx (except 401/429): client errors, no retry.
		return false, nil
	}
}

// errorHandler is called by retryablehttp when retries are exhausted (RetryMax
// reached while CheckRetry kept returning true). At this point the response
// body has been drained/closed by retryablehttp's retry loop, so we construct
// a SEMPError from the status code and attempt count.
func (c *HTTPClient) errorHandler(resp *http.Response, err error, numTries int) (*http.Response, error) {
	if err != nil {
		return nil, fmt.Errorf("request failed after %d attempts: %w", numTries, err)
	}

	opID := "unknown"
	if resp != nil && resp.Request != nil {
		if id, ok := resp.Request.Context().Value(opIDKey{}).(string); ok {
			opID = id
		}
	}

	if resp != nil {
		slog.Error("request failed after retries exhausted",
			slog.String("broker", c.baseURL),
			slog.String("operation", opID),
			slog.Int("status", resp.StatusCode),
			slog.Int("attempts", numTries))
		return nil, &SEMPError{
			Operation:  opID,
			StatusCode: resp.StatusCode,
			Body:       fmt.Sprintf("request failed after %d attempts with status %d", numTries, resp.StatusCode),
		}
	}

	return nil, fmt.Errorf("request failed after %d attempts", numTries)
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
	req.Header.Set("User-Agent", "solace/broker-mcp-server/"+version.Version())

	return req, nil
}

// addAuth sets the authentication header on the request based on the configured
// auth mode. Basic auth sends Authorization: Basic base64(user:pass). Bearer
// sends Authorization: Bearer <token>.
//
// By the time this runs, config validation has guaranteed that authMode is one
// of validAuthModes and the corresponding credential fields are non-empty, so
// no defensive emptiness checks are needed here. If a new auth mode is added,
// config.validAuthModes must be updated AND a new case must be added below.
func (c *HTTPClient) addAuth(req *http.Request) {
	switch c.authMode {
	case config.AuthModeBasic:
		req.SetBasicAuth(c.username, c.password)
	case config.AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
