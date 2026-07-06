// Package sempv1 implements a client for the Solace broker's legacy SEMPv1
// XML-over-HTTP management API. It sits alongside the sempv2 client as a peer
// protocol; individual tools choose which to call.
//
// The error model in this file is v1-specific and deliberately separate from
// sempv2.SEMPError.
package sempv1

import (
	"fmt"
	"unicode/utf8"
)

// maxErrorTextLen bounds broker-controlled error text (Error.Message) captured
// from an <rpc-reply>. The response body is capped at 16 MiB
// (defaults.MaxSEMPResponseBytes), so without this bound a misbehaving broker
// or intermediary can push a multi-MiB string into every sink that renders the
// message: Error.Error() (logged as the tool-result "detail" field) and the
// agent-facing message built by the tools layer (which also runs it through
// regex sanitization). Truncating at capture bounds all of them at once. This
// mirrors sempv2's maxErrorTextLen; 4 KiB comfortably holds any real SEMP error
// text. (Error.Body is deliberately not truncated here — it is a debug-only
// field that no sink renders.)
const maxErrorTextLen = 4096

// truncationMarker is appended to error text cut at maxErrorTextLen.
const truncationMarker = "... [truncated]"

// truncateErrorText caps s at maxErrorTextLen bytes, backing up to a rune
// boundary so the result stays valid UTF-8, and appends truncationMarker. The
// cut path concatenates, which allocates a fresh string, so an oversized
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

// ErrorKind classifies a SEMPv1 failure so callers can branch without parsing
// the raw response body. A zero-valued Kind (ErrorKindUnknown) indicates a
// malformed or unclassified response.
type ErrorKind int

const (
	ErrorKindUnknown     ErrorKind = iota // zero value; malformed envelope or unclassified response
	ErrorKindHTTP                         // HTTP-layer failure (non-2xx: 401, 403, 404, 5xx, etc.)
	ErrorKindParse                        // <parse-error> element in the <rpc-reply> envelope
	ErrorKindPermission                   // <permission-error> element in the <rpc-reply> envelope
	ErrorKindLimit                        // <limit-error> element in the <rpc-reply> envelope
	ErrorKindExecuteFail                  // <execute-result code="fail" .../> in the <rpc-reply> envelope
)

// String returns a short, greppable name for the kind. Exists so Error()
// output and log lines render a readable name (e.g. "http") instead of the
// raw integer. Callers should still branch on the enum value, not the string.
func (k ErrorKind) String() string {
	switch k {
	case ErrorKindHTTP:
		return "http"
	case ErrorKindParse:
		return "parse"
	case ErrorKindPermission:
		return "permission"
	case ErrorKindLimit:
		return "limit"
	case ErrorKindExecuteFail:
		return "execute-fail"
	default:
		return "unknown"
	}
}

// Error is returned for any SEMPv1 failure — either an HTTP-layer error
// (non-2xx response) or an envelope-layer error (HTTP 200 + error element
// inside <rpc-reply>). Callers branch on Kind.
//
// Field semantics by Kind:
//   - Kind == ErrorKindHTTP: StatusCode is the real HTTP status; Message is
//     empty; ReasonCode is zero; Body holds the raw response.
//   - Kind == ErrorKindExecuteFail: StatusCode is 200; Message is the reason
//     attribute; ReasonCode is the reasonCode attribute.
//   - Kind == ErrorKindParse / Permission / Limit: StatusCode is 200;
//     Message is the text content of the error element; ReasonCode is zero.
//   - Kind == ErrorKindUnknown: response did not match any known shape;
//     Body holds the raw bytes for debugging.
//
// Body is always preserved as a safety net for information the client did
// not structurally extract.
type Error struct {
	Kind       ErrorKind
	StatusCode int
	Message    string
	ReasonCode int
	Body       []byte
}

// Error implements the error interface. Format is stable and greppable:
//
//	sempv1: <kind>: <message> (status=<n>, reason=<n>)
//
// Message and reasonCode are omitted from the suffix when zero-valued.
func (e *Error) Error() string {
	if e == nil {
		return "sempv1: <nil>"
	}

	suffix := fmt.Sprintf("status=%d", e.StatusCode)
	if e.ReasonCode != 0 {
		suffix = fmt.Sprintf("%s, reason=%d", suffix, e.ReasonCode)
	}

	if e.Message == "" {
		return fmt.Sprintf("sempv1: %s (%s)", e.Kind, suffix)
	}
	return fmt.Sprintf("sempv1: %s: %s (%s)", e.Kind, e.Message, suffix)
}
