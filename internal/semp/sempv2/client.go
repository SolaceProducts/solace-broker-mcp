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

package sempv2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/correlationhdr"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/version"
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
	// Body is the raw response body, preserved for debugging when
	// parseSEMPError could not unmarshal a meta.error envelope out of it
	// (Description stays empty in that case). Unlike Description, Body is
	// NOT necessarily broker-authored: a non-SEMP response reaching this
	// client — a reverse proxy, API gateway, or WAF's error page — lands
	// here verbatim and has never been reviewed by this codebase (SOL-153766).
	// Deliberately not rendered by Error() or any other sink for that reason
	// (mirrors sempv1.Error.Body's equivalent, documented debug-only status).
	Body string
}

// Error implements the error interface. It never renders Body: Body is
// unaudited external content (see the field doc above), and Error() is what
// manager.go's audit logging puts into the "detail" log field for the broker
// error types it trusts (SOL-153766). Only Description — parsed from the
// broker's own meta.error.description — is broker-authored and safe to
// include; when it's empty (Body-fallback case) Error() reports just the
// operation and status.
func (e *SEMPError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s returned HTTP %d: %s", e.Operation, e.StatusCode, e.Description)
	}
	return fmt.Sprintf("%s returned HTTP %d", e.Operation, e.StatusCode)
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
// URL — credentials are deliberately excluded. baseURL is routed through
// SanitizeURLString for the same reason BrokerConfig.LogValue routes URL
// through it: validation already rejects credentialed URLs, but this keeps
// that guarantee from being the only thing standing between a broker URL and
// the log stream (SOL-152979).
// See docs/secure-logging-rules.md Rule 2.
func (c *HTTPClient) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("base_url", config.SanitizeURLString(c.baseURL)),
	)
}

// NewHTTPClient creates an HTTPClient configured for a specific broker.
// It sets up a per-broker HTTP transport with TLS settings and connection pool
// tuning appropriate for concurrent SEMP calls, and delegates retry and rate
// limiting to a shared resilience.Sender.
//
// authn is the per-broker Authenticator (built once by semp.NewBrokerClient
// and shared with the SEMPv1 client by pointer). It must be non-nil.
//
// jar is the per-broker cookie jar for session cookie management. Pass nil
// for auth modes that don't use session cookies (bearer, OAuth).
//
// sem is the broker's shared in-flight semaphore and must be non-nil
// (resilience.New panics otherwise); see semp.NewBrokerClient, which shares
// one semaphore across both protocol clients of a broker.
//
// limiter is the broker's shared rate limiter and must likewise be non-nil
// (resilience.New panics otherwise). It carries the same sharing requirement:
// one per broker, passed to both protocol clients, because a per-client limiter
// admits up to 2x the configured rate (SOL-152401). Its lifetime belongs to the
// caller — BrokerClient.Close() stops it, so this client must not.
//
// opts are forwarded verbatim to the Sender. Variadic so existing call sites
// stay untouched; semp.NewBrokerClient is the only caller that passes any.
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig, sem resilience.Semaphore, limiter *resilience.RateLimiter, authn auth.Authenticator, jar *resilience.SafeCookieJar, opts ...resilience.Option) (*HTTPClient, error) {
	if authn == nil {
		panic("sempv2.NewHTTPClient: nil authenticator")
	}
	transport := resilience.NewTunedTransport(brokerCfg, sempCfg)

	if brokerCfg.InsecureSkipVerify {
		slog.Warn("INSECURE: TLS verification disabled for broker",
			slog.String("broker", brokerCfg.DisplayName()))
	}

	httpClient := &http.Client{
		Timeout:   sempCfg.RequestTimeoutDuration,
		Transport: transport,
	}
	if jar != nil {
		httpClient.Jar = jar
	}

	baseURL := strings.TrimSuffix(brokerCfg.URL, "/")

	return &HTTPClient{
		sender:        resilience.New(httpClient, sempCfg, authn, baseURL, sem, limiter, opts...),
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
		// The Sender consumed the final response to release the connection, so
		// the broker's own explanation only survives on the error. Parse it here,
		// where SEMPv2 framing is understood, and hang the description off the
		// error rather than replacing it: the RetriesExhaustedError identity is
		// what carries NonIdempotent, which upper layers key their retryable flag
		// off. Losing that to gain a description would trade a safety property
		// for a diagnostic.
		var exhausted *resilience.RetriesExhaustedError
		if errors.As(err, &exhausted) && len(exhausted.Body) > 0 {
			if sempErr := parseSEMPError(op.ID, exhausted.StatusCode, exhausted.Body); sempErr.Description != "" {
				exhausted.Detail = sempErr.Description
			}
		}
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
			s := fmt.Sprintf("%v", value)
			// Reject empty and dot-segment values before substitution. url.PathEscape
			// escapes "/", so a path-parameter value is always exactly one URL segment;
			// "", "." and ".." are therefore the complete set of values that could
			// produce an empty or dot segment and let a proxy or broker that normalizes
			// dot-segments collapse the request onto an unintended (e.g. parent) path.
			if s == "" || s == "." || s == ".." {
				return "", fmt.Errorf("operation %s (path %q): path parameter %q has invalid value %q: empty and dot segments (\".\", \"..\") are not allowed", op.ID, op.Path, key, s)
			}
			path = strings.ReplaceAll(path, placeholder, url.PathEscape(s))
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

// PathParamNames returns the de-duplicated list of path-parameter names
// (without braces) declared in an operation's path template — e.g.
// "msgVpnName" and "topicEndpointName" for
// "/msgVpns/{msgVpnName}/topicEndpoints/{topicEndpointName}". Exported so
// callers outside this package (e.g. the composite loader's catalog tests)
// can verify every declared path parameter is actually supplied by a tool's
// step args, without duplicating this scan.
func PathParamNames(path string) []string {
	var names []string
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
		name := rest[start+1 : start+end]
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			names = append(names, name)
		}
		rest = rest[start+end+1:]
	}
	return names
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

// maxErrorTextLen bounds Description, the one field captured into a SEMPError
// that is both broker-authored and rendered by a sink: SEMPError.Error() (in
// turn the tool-result log's "detail" field) and the agent-facing MCP error
// message. The body read is capped at 16 MiB (defaults.MaxSEMPResponseBytes),
// so without this bound a misbehaving broker could still push a multi-MiB
// description into both sinks; truncating at capture bounds them at once. 4
// KiB comfortably holds any real SEMP error description.
//
// Body is truncated to the same bound purely to cap SEMPError's own memory
// footprint — not because any sink renders it. Body is unaudited, possibly
// non-broker content (see its field doc), so Error() never includes it
// (SOL-153766); it exists only as a debug-only struct field, the same
// posture sempv1.Error.Body documents for its own equivalent.
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
// zero-valued and Body carries the raw response. Description is
// broker-controlled; Body may not be (see the field doc). Both are
// truncated at maxErrorTextLen.
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
