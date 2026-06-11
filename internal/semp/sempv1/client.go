package sempv1

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
)

// Client executes SEMPv1 XML commands against a Solace broker's /SEMP endpoint.
// Implementations: HTTPClient (real), mock (tests).
type Client interface {
	Execute(ctx context.Context, xml string) (*Result, error)
}

// Result holds a successful SEMPv1 response payload.
// On success, InnerXML contains only the <rpc>...</rpc> inner bytes —
// callers never see <rpc-reply>, <execute-result>, or any error envelope.
type Result struct {
	InnerXML []byte
}

// HTTPClient implements the Client interface by making real HTTP calls to a
// Solace broker's /SEMP endpoint. It is configured per-broker with the
// broker's URL, TLS settings, and authentication credentials.
//
// Rate limiting and retry logic are delegated to the shared resilience.Sender.
type HTTPClient struct {
	sender    *resilience.Sender
	baseURL string
	authCfg config.AuthConfig
}

// LogValue implements slog.LogValuer for HTTPClient. It exposes only the base
// URL — username, password, and token are deliberately excluded. Although
// these fields are unexported, this provides defense in depth against
// reflection-based logging.
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

// NewHTTPClient creates an HTTPClient configured for a specific broker. It
// sets up a per-broker HTTP transport with TLS settings and a cookie jar, and
// delegates retry and rate limiting to a shared resilience.Sender. No network
// I/O happens here — connection setup is lazy on the first Execute call.
func NewHTTPClient(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) (*HTTPClient, error) {
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
		sender:    resilience.New(httpClient, jar, sempCfg, brokerCfg.Auth, baseURL),
		baseURL: baseURL,
		authCfg: brokerCfg.Auth,
	}, nil
}

// invalidInput returns an *Error{Kind: ErrorKindUnknown} for request-level
// validation failures (nil context, empty XML). Used only by Execute's guard
// clauses; broader "unknown" errors from envelope parsing are produced inside
// parseReply.
func invalidInput(msg string) *Error {
	return &Error{Kind: ErrorKindUnknown, Message: msg}
}

// isShowCommand reports whether the SEMPv1 request payload is a read-only
// <show> command, i.e. the first element inside the <rpc> envelope is <show>.
// Deliberately conservative: anything unrecognized returns false, which keeps
// the Sender's no-retry posture for mutating commands.
func isShowCommand(xml string) bool {
	s := strings.TrimSpace(xml)
	if !strings.HasPrefix(s, "<rpc") || len(s) < 6 {
		return false
	}
	// "<rpc" must be a complete tag name: next byte is '>' or whitespace
	// (attributes like semp-version), not e.g. "<rpcx" or self-closing "<rpc/>".
	switch s[4] {
	case '>', ' ', '\t', '\n', '\r':
	default:
		return false
	}
	end := strings.IndexByte(s, '>')
	if end < 0 {
		return false
	}
	rest := strings.TrimSpace(s[end+1:])
	if !strings.HasPrefix(rest, "<show") || len(rest) < 6 {
		return false
	}
	// "<show" must be a complete tag name: next byte is '>', '/' (self-closing
	// "<show/>"), or whitespace (attributes), not e.g. "<showcase". Same check
	// as the "<rpc" guard above.
	switch rest[5] {
	case '>', '/', ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

// Execute sends an XML request to the broker's /SEMP endpoint and returns the
// inner <rpc> bytes on success or a classified error on failure. The xml
// argument is the complete <rpc>...</rpc> payload as a string; the caller is
// responsible for building it.
//
// Rate limiting is enforced before the request (new requests only, not retries).
// Retry logic is handled by the shared resilience.Sender.
func (c *HTTPClient) Execute(ctx context.Context, xml string) (*Result, error) {
	if ctx == nil {
		return nil, invalidInput("nil context")
	}
	if xml == "" {
		return nil, invalidInput("empty xml")
	}

	reqURL := c.baseURL + "/SEMP"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(xml))

	if err != nil {
		return nil, fmt.Errorf("building SEMPv1 request: %w", err)
	}

	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("User-Agent", "solace/broker-mcp-server/"+version.Version())

	if err := auth.AddAuth(ctx, req, c.authCfg); err != nil {
		return nil, fmt.Errorf("applying SEMPv1 auth: %w", err)
	}

	// Attach operation ID for the Sender's logging context.
	ctx = context.WithValue(ctx, resilience.OperationIDKey{}, "SEMPv1")

	// SEMPv1 travels exclusively over POST, so the Sender's method-based
	// idempotency guard would otherwise deny every request all retries
	// (429/503 backoff, connection errors, 401 cookie-clear re-auth).
	// Read-only <show> commands are semantically idempotent — mark them
	// retry-safe. Anything else (mutating admin/create/... commands, none
	// issued today) keeps the no-retry posture.
	if isShowCommand(xml) {
		ctx = resilience.WithRetrySafe(ctx)
	}
	req = req.WithContext(ctx)

	resp, err := c.sender.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("executing SEMPv1 request: %w", err)
	}
	defer resp.Body.Close()

	body, err := resilience.ReadCappedBody(resp.Body, defaults.MaxSEMPResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("reading SEMPv1 response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{
			Kind:       ErrorKindHTTP,
			StatusCode: resp.StatusCode,
			Body:       body,
		}
	}

	inner, parseErr := parseReply(body)
	if parseErr != nil {
		return nil, parseErr
	}

	return &Result{InnerXML: inner}, nil
}
