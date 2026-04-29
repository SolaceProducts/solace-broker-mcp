package sempv1

import "testing"

// TestParseReply_Success covers the success fixture from design spec §8.4:
// a valid <rpc-reply> containing <rpc> and <execute-result code="ok"/>.
// parseReply should return the inner <rpc> bytes verbatim and no error.
func TestParseReply_Success(t *testing.T) {
	body := []byte(`<rpc-reply><rpc><show><version/></show></rpc><execute-result code="ok"/></rpc-reply>`)

	inner, err := parseReply(body)

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	want := `<show><version/></show>`
	if string(inner) != want {
		t.Errorf("inner XML mismatch\n got: %q\n want: %q", string(inner), want)
	}
}

// TestParseReply_Errors covers the six failure fixtures from design spec §8.4:
// <parse-error>, <permission-error>, <limit-error>, <execute-result code="fail">,
// malformed XML, and an empty <rpc-reply/>. For each fixture, parseReply should
// return nil bytes and a *Error with the expected Kind, Message, and ReasonCode.
// StatusCode is asserted to be 200 for every row, per the envelope-only contract.
func TestParseReply_Errors(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantKind       ErrorKind
		wantMessage    string
		wantReasonCode int
	}{
		{
			name:        "parse error",
			body:        `<rpc-reply><parse-error>invalid message</parse-error></rpc-reply>`,
			wantKind:    ErrorKindParse,
			wantMessage: "invalid message",
		},
		{
			name:        "permission error",
			body:        `<rpc-reply><permission-error>not authorized</permission-error></rpc-reply>`,
			wantKind:    ErrorKindPermission,
			wantMessage: "not authorized",
		},
		{
			name:        "limit error",
			body:        `<rpc-reply><limit-error>response too big</limit-error></rpc-reply>`,
			wantKind:    ErrorKindLimit,
			wantMessage: "response too big",
		},
		{
			name:           "execute fail",
			body:           `<rpc-reply><execute-result code="fail" reason="foo" reasonCode="431"/></rpc-reply>`,
			wantKind:       ErrorKindExecuteFail,
			wantMessage:    "foo",
			wantReasonCode: 431,
		},
		{
			name:     "malformed xml",
			body:     `<rpc-reply><unclosed>`,
			wantKind: ErrorKindUnknown,
		},
		{
			name:        "empty rpc-reply",
			body:        `<rpc-reply/>`,
			wantKind:    ErrorKindUnknown,
			wantMessage: "empty reply",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inner, err := parseReply([]byte(tc.body))

			if inner != nil {
				t.Errorf("expected nil inner XML, got %q", string(inner))
			}
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if err.Kind != tc.wantKind {
				t.Errorf("error kind mismatch got: %v want: %v", err.Kind, tc.wantKind)
			}
			if err.Message != tc.wantMessage {
				t.Errorf("error message mismatch got: %q want: %q", err.Message, tc.wantMessage)
			}
			if err.ReasonCode != tc.wantReasonCode {
				t.Errorf("error reason code mismatch got: %d want: %d", err.ReasonCode, tc.wantReasonCode)
			}
			if err.StatusCode != 200 {
				t.Errorf("error status code mismatch got: %d want: 200", err.StatusCode)
			}
		})
	}
}
