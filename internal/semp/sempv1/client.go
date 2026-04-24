package sempv1

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/version"
)

// TODO(Story 5): wrap with rate-limiting + retry decorator (shared with v2).
// Current implementation is the transport layer only; resilience lives in a
// separate layer so both v1 and v2 benefit without duplication.

// Client executes SEMPv1 XML commands against a Solace broker's /SEMP endpoint.
// Implementations: HTTPClient (real), mock (tests). Future: rate-limited or
// retry decorators wrapping this interface (Story 5).
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
type HTTPClient struct {
	httpClient *http.Client
	baseURL    string
	authMode   string
	username   string
	password   string
	token      string
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

// NewHTTPClient creates an HTTPClient configured for a specific broker. It
// sets up a per-broker HTTP transport with TLS settings and a cookie jar, and
// applies the configured request timeout. No network I/O happens here —
// connection setup is lazy on the first Execute call.
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
	return &HTTPClient{
		httpClient: &http.Client{
			Jar:       jar,
			Timeout:   sempCfg.RequestTimeoutDuration,
			Transport: transport,
		},
		baseURL:  strings.TrimSuffix(brokerCfg.URL, "/"),
		authMode: brokerCfg.Auth.Mode,
		username: brokerCfg.Auth.Username,
		password: brokerCfg.Auth.Password,
		token:    brokerCfg.Auth.Token,
	}, nil
}

// addAuth sets the authentication header on the request based on the configured
// auth mode. Basic auth sends Authorization: Basic base64(user:pass). Bearer
// sends Authorization: Bearer <token>.
//
// By the time this runs, config validation has guaranteed that authMode is one
// of validAuthModes and the corresponding credential fields are non-empty, so
// no defensive emptiness checks are needed here. If a new auth mode is added,
// config.validAuthModes must be updated AND a new case must be added below.
//
// This duplicates sempv2.addAuth deliberately: the logic is identical, but
// extracting to a shared helper would couple the two clients through a new
// internal package. Per T3 scope ("duplicate with sync comment"), we keep
// them in parallel and rely on the config.AuthMode* constants as the shared
// contract. If one is changed, the other must change too.
func (c *HTTPClient) addAuth(req *http.Request) {
	switch c.authMode {
	case config.AuthModeBasic:
		req.SetBasicAuth(c.username, c.password)
	case config.AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// invalidInput returns an *Error{Kind: ErrorKindUnknown} for request-level
// validation failures (nil context, empty XML). Used only by Execute's guard
// clauses; broader "unknown" errors from envelope parsing are produced inside
// parseReply.
func invalidInput(msg string) *Error {
	return &Error{Kind: ErrorKindUnknown, Message: msg}
}

// Execute sends an XML request to the broker's /SEMP endpoint and returns the
// inner <rpc> bytes on success or a classified error on failure. The xml
// argument is the complete <rpc>...</rpc> payload as a string; the caller is
// responsible for building it.
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

	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing SEMPv1 request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
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
