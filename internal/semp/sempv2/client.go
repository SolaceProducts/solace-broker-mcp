package sempv2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/correlationhdr"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
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
// parsed meta.error fields so callers can extract structured data via
// errors.As() instead of parsing error strings.
type SEMPError struct {
	Operation   string // operationId that failed (e.g., "getMsgVpnQueue")
	StatusCode  int    // HTTP status code (e.g., 404)
	Description string // meta.error.description — broker's human-readable message
	SEMPCode    int    // meta.error.code (6=NOT_FOUND, 72=UNAUTHORIZED, etc.)
	SEMPStatus  string // meta.error.status ("NOT_FOUND", "FAIL")
	Body        string // raw response body preserved as fallback
}

// Error implements the error interface.
func (e *SEMPError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s returned HTTP %d: %s", e.Operation, e.StatusCode, e.Description)
	}
	return fmt.Sprintf("%s returned HTTP %d: %s", e.Operation, e.StatusCode, e.Body)
}

// HTTPClient implements the Client interface by making real HTTP calls to a
// Solace broker's SEMPv2 API. It is configured per-broker with the broker's
// URL and TLS settings and authenticates outbound requests through the
// supplied auth.Authenticator. One Authenticator instance is shared between
// this client and the broker's SEMPv1 client; see semp.NewBrokerClient.
//
// Rate limiting and retry logic are delegated to the shared resilience.Sender.
type HTTPClient struct {
	sender        *resilience.Sender
	baseURL       string
	authenticator auth.Authenticator
}

// LogValue implements slog.LogValuer for HTTPClient. It exposes only the base
// URL — credentials are deliberately excluded.
// See docs/secure-logging-rules.md Rule 2.
func (c *HTTPClient) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", c.baseURL),
	)
}

// Close releases resources held by the HTTPClient (rate limiter ticker).
// Safe to call multiple times.
func (c *HTTPClient) Close() {
	c.sender.Close()
}

// NewHTTPClient creates an HTTPClient configured for a specific broker.
// It sets up a per-broker HTTP transport with TLS settings and connection pool
// tuning appropriate for concurrent SEMP calls, and delegates retry and rate
// limiting to a shared resilience.Sender.
//
// authn is the per-broker Authenticator (built once by semp.NewBrokerClient
// and shared with the SEMPv1 client by pointer). It must be non-nil.
//
// sem is the broker's shared in-flight semaphore and must be non-nil
// (resilience.New panics otherwise); see semp.NewBrokerClient, which shares
// one semaphore across both protocol clients of a broker.
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig, sem resilience.Semaphore, authn auth.Authenticator) (*HTTPClient, error) {
	// Programmer-error precondition: there is no runtime path that yields a
	// nil Authenticator here. semp.NewBrokerClient is the single production
	// caller and constructs authn via auth.NewAuthenticator before this call.
	// A nil at this point means someone ignored an error from NewAuthenticator
	// or passed nil explicitly — both bugs in calling code, not recoverable
	// runtime conditions. Panicking surfaces the bug at construction time
	// instead of as a delayed nil-pointer panic deep inside AddAuth at request
	// time.
	if authn == nil {
		panic("sempv2.NewHTTPClient: nil authenticator")
	}
	transport := resilience.NewTunedTransport(brokerCfg, sempCfg)

	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}

	if brokerCfg.InsecureSkipVerify {
		slog.Warn("INSECURE: TLS verification disabled for broker",
			slog.String("broker", brokerCfg.DisplayName()))
	}

	httpClient := &http.Client{
		Jar:       jar,
		Timeout:   sempCfg.RequestTimeoutDuration,
		Transport: transport,
	}

	baseURL := strings.TrimSuffix(brokerCfg.URL, "/")

	return &HTTPClient{
		sender:        resilience.New(httpClient, jar, sempCfg, brokerCfg.Auth, baseURL, sem),
		baseURL:       baseURL,
		authenticator: authn,
	}, nil
}

// Execute makes an authenticated HTTP request to the broker's SEMPv2 API for
// the given operation and arguments. Path parameters in the operation's URL
// template are substituted from args, query parameters are appended, and body
// parameters are sent as JSON. Returns a structured Result on success or an
// error with status code context on failure.
//
// Rate limiting is enforced before the request (new requests only, not retries).
// Retry logic is handled by the shared resilience.Sender.
func (c *HTTPClient) Execute(ctx context.Context, op *Operation, args map[string]any) (*Result, error) {
	reqURL, err := c.buildURL(op, args)
	if err != nil {
		return nil, err
	}

	req, err := c.buildRequest(ctx, op, reqURL, args)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", op.ID, err)
	}

	if err := c.authenticator.AddAuth(ctx, req); err != nil {
		return nil, fmt.Errorf("applying auth for %s: %w", op.ID, err)
	}

	// Forward the request-correlation ID (if any) as outbound headers, strictly
	// after auth so it cannot clobber or be clobbered by auth headers. No-op
	// when correlation is off (From(ctx) == "").
	correlationhdr.Set(ctx, req)

	// Attach operation ID for the Sender's logging context.
	ctx = context.WithValue(ctx, resilience.OperationIDKey{}, op.ID)
	req = req.WithContext(ctx)

	resp, err := c.sender.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("executing %s: %w", op.ID, err)
	}
	defer resp.Body.Close()

	body, err := resilience.ReadCappedBody(resp.Body, defaults.MaxSEMPResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("reading response for %s: %w", op.ID, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseSEMPError(op.ID, resp.StatusCode, body)
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
// Returns an error if any {placeholder} tokens remain unfilled after
// substitution — this catches missing required path parameters before a
// silently-wrong HTTP call is made.
func (c *HTTPClient) buildURL(op *Operation, args map[string]any) (string, error) {
	path := op.Path

	for key, value := range args {
		placeholder := "{" + key + "}"
		if strings.Contains(path, placeholder) {
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(fmt.Sprintf("%v", value)))
		}
	}

	// Detect any unfilled {placeholder} tokens left in the path. A "{" with no
	// matching "}" means the path template itself is malformed — in that case
	// fall through and let the broker surface the bad URL via a 4xx, since the
	// problem isn't a missing argument.
	missing := unfilledPlaceholders(path)
	if len(missing) > 0 {
		return "", fmt.Errorf("operation %s (path %q): unfilled path parameters: %s", op.ID, op.Path, strings.Join(missing, ", "))
	}

	return c.baseURL + "/" + strings.TrimPrefix(path, "/"), nil
}

// unfilledPlaceholders returns the de-duplicated list of "{name}" tokens
// remaining in path after argument substitution. Each placeholder is reported
// once even if it appears multiple times in the template.
func unfilledPlaceholders(path string) []string {
	var missing []string
	seen := make(map[string]struct{})
	for rest := path; len(rest) > 0; {
		start := strings.Index(rest, "{")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], "}")
		if end < 0 {
			break
		}
		placeholder := rest[start : start+end+1]
		if _, dup := seen[placeholder]; !dup {
			seen[placeholder] = struct{}{}
			missing = append(missing, placeholder)
		}
		rest = rest[start+end+1:]
	}
	return missing
}

// buildRequest constructs the HTTP request with query parameters and JSON body
// based on the operation's parameter definitions.
func (c *HTTPClient) buildRequest(ctx context.Context, op *Operation, reqURL string, args map[string]any) (*http.Request, error) {
	queryParams := url.Values{}
	var arrayQueryParts []string // pre-encoded "name=v1,v2,v3" segments for array-typed params
	var bodyData []byte

	for _, param := range op.Parameters {
		switch param.In {
		case "path":
			continue
		case "query":
			val, ok := args[param.Name]
			if !ok {
				continue
			}
			if param.Type == "array" {
				// SEMP v2 declares select/where as type=array, collectionFormat=csv: a single
				// query param whose value is a comma-joined list. url.Values.Encode percent-
				// encodes the commas to %2C, which the broker rejects ("'a,b,c' not a valid
				// attribute"). Encode each element individually and join with raw commas.
				arrayQueryParts = append(arrayQueryParts, encodeCSVQueryParam(param.Name, val))
				continue
			}
			queryParams.Set(param.Name, fmt.Sprintf("%v", val))
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

	queryString := queryParams.Encode()
	if len(arrayQueryParts) > 0 {
		joined := strings.Join(arrayQueryParts, "&")
		if queryString != "" {
			queryString += "&" + joined
		} else {
			queryString = joined
		}
	}
	if queryString != "" {
		reqURL = reqURL + "?" + queryString
	}

	var bodyReader io.Reader
	if bodyData != nil {
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequestWithContext(ctx, op.Method, reqURL, bodyReader)
	if err != nil {
		return nil, err
	}

	if bodyData != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "solace/broker-mcp-server/"+version.Version())

	return req, nil
}

// encodeCSVQueryParam serializes an array-typed query parameter in SEMP v2's
// collectionFormat=csv shape: each element is percent-encoded individually,
// then joined with literal commas (e.g. "select=a,b,c"). The input value is
// either a comma-joined string (the typical YAML path) or a slice; both are
// flattened to a single comma-separated query param value.
func encodeCSVQueryParam(name string, val any) string {
	var parts []string
	switch v := val.(type) {
	case []string:
		parts = v
	case []any:
		parts = make([]string, len(v))
		for i, e := range v {
			parts[i] = fmt.Sprintf("%v", e)
		}
	default:
		parts = strings.Split(fmt.Sprintf("%v", v), ",")
	}

	encoded := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		encoded = append(encoded, url.QueryEscape(p))
	}
	return url.QueryEscape(name) + "=" + strings.Join(encoded, ",")
}

// maxErrorTextLen bounds broker-controlled error text (Description and the
// raw Body fallback) captured into a SEMPError. The body read is capped at
// 16 MiB (defaults.MaxSEMPResponseBytes), so without this bound a misbehaving
// broker or intermediary can push a multi-MiB string into every downstream
// sink: SEMPError.Error(), the tool-result log's detail field, and the
// agent-facing MCP error message. Truncating at capture bounds all of them
// at once. 4 KiB comfortably holds any real SEMP error description.
const maxErrorTextLen = 4096

// truncationMarker is appended to error text cut at maxErrorTextLen.
const truncationMarker = "... [truncated]"

// truncateErrorText caps s at maxErrorTextLen bytes, backing up to a rune
// boundary so the result stays valid UTF-8, and appends truncationMarker.
// The cut path concatenates, which allocates a fresh string, so an oversized
// input's backing array is never retained.
func truncateErrorText(s string) string {
	if len(s) <= maxErrorTextLen {
		return s
	}
	cut := maxErrorTextLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

// truncateErrorBytes is truncateErrorText for a []byte source. Truncating
// before the string conversion avoids transiently copying an oversized body
// (up to defaults.MaxSEMPResponseBytes) just to throw most of it away.
func truncateErrorBytes(b []byte) string {
	if len(b) <= maxErrorTextLen {
		return string(b)
	}
	cut := maxErrorTextLen
	for cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return string(b[:cut]) + truncationMarker
}

// parseSEMPError creates a SEMPError with best-effort extraction of the
// broker's meta.error fields (code, status, description). If the body is not
// valid JSON or the meta.error structure is absent, the structured fields stay
// zero-valued and Body carries the raw response. Description and Body are
// broker-controlled and truncated at maxErrorTextLen.
func parseSEMPError(op string, statusCode int, body []byte) *SEMPError {
	e := &SEMPError{
		Operation:  op,
		StatusCode: statusCode,
		Body:       truncateErrorBytes(body),
	}

	var envelope struct {
		Meta struct {
			Error struct {
				Code        int    `json:"code"`
				Status      string `json:"status"`
				Description string `json:"description"`
			} `json:"error"`
		} `json:"meta"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		e.SEMPCode = envelope.Meta.Error.Code
		e.SEMPStatus = envelope.Meta.Error.Status
		e.Description = truncateErrorText(envelope.Meta.Error.Description)
	}

	return e
}
