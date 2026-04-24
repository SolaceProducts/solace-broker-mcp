package sempv1

import (
	"encoding/xml"
)

// rpcReply models the <rpc-reply> envelope returned by the broker.
type rpcReply struct {
	XMLName xml.Name `xml:"rpc-reply"`
	Rpc     *struct {
		InnerXML []byte `xml:",innerxml"`
	} `xml:"rpc"`
	MoreCookie      string         `xml:"more-cookie"`
	ExecuteResult   *executeResult `xml:"execute-result"`
	ParseError      *string        `xml:"parse-error"`
	PermissionError *string        `xml:"permission-error"`
	LimitError      *string        `xml:"limit-error"`
}

// executeResult captures the attributes of <execute-result>:
// code ("ok"|"fail"), reason, and reasonCode.
type executeResult struct {
	Code       string `xml:"code,attr"`
	Reason     string `xml:"reason,attr"`
	ReasonCode int    `xml:"reasonCode,attr"`
}

// parseReply inspects a raw <rpc-reply>...</rpc-reply> body and returns either
// the inner <rpc> XML bytes (success) or a classified *Error (failure).
//
// Pre-condition: the caller is responsible for HTTP-level status checks.
// parseReply is only called when HTTP status is 2xx — any StatusCode on a
// returned *Error will therefore be 200.
func parseReply(body []byte) ([]byte, *Error) {
	var r rpcReply

	if err := xml.Unmarshal(body, &r); err != nil {
		return nil, &Error{
			Kind:       ErrorKindUnknown,
			StatusCode: 200,
			Body:       body,
		}
	}

	// Error-element checks run in priority order per design spec §8.2:
	// parse > permission > limit > execute-fail. First match wins; later
	// checks do not run even if their element is also present.
	if r.ParseError != nil {
		return nil, &Error{
			Kind:       ErrorKindParse,
			StatusCode: 200,
			Message:    *r.ParseError,
			Body:       body,
		}
	}
	if r.PermissionError != nil {
		return nil, &Error{
			Kind:       ErrorKindPermission,
			StatusCode: 200,
			Message:    *r.PermissionError,
			Body:       body,
		}
	}
	if r.LimitError != nil {
		return nil, &Error{
			Kind:       ErrorKindLimit,
			StatusCode: 200,
			Message:    *r.LimitError,
			Body:       body,
		}
	}
	// <execute-result> is also emitted on success (code="ok"), so presence
	// alone is not an error — we require code="fail" specifically.
	if r.ExecuteResult != nil && r.ExecuteResult.Code == "fail" {
		return nil, &Error{
			Kind:       ErrorKindExecuteFail,
			StatusCode: 200,
			Message:    r.ExecuteResult.Reason,
			ReasonCode: r.ExecuteResult.ReasonCode,
			Body:       body,
		}
	}

	// Success: no error elements, <rpc> present. Return its inner bytes
	// verbatim; callers parse the <rpc> payload themselves.
	if r.Rpc != nil {
		return r.Rpc.InnerXML, nil
	}

	// Envelope decoded, no errors, no <rpc> — an unclassifiable response.
	// Common trigger: broker emits <rpc-reply/>. Flag as Unknown so callers
	// can decide what to do.
	return nil, &Error{
		Kind:       ErrorKindUnknown,
		StatusCode: 200,
		Message:    "empty reply",
		Body:       body,
	}
}
