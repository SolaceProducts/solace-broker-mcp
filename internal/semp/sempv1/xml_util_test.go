package sempv1

import (
	"encoding/xml"
	"testing"
)

// TestEscape covers the eight fixture cases from design spec §9.5 — the
// escape table the XML-safety helper must satisfy before any tool builder
// can embed user-supplied values in a SEMPv1 request string.
func TestEscape(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "angle brackets",
			input: `<foo>`,
			want:  `&lt;foo&gt;`,
		},
		{
			name:  "ampersand",
			input: `a & b`,
			want:  `a &amp; b`,
		},
		{
			name:  "single and double quotes",
			input: `'x' "y"`,
			want:  `&#39;x&#39; &#34;y&#34;`,
		},
		{
			name:  "mixed hostile payload is fully entity-encoded",
			input: `</rpc><evil/>`,
			want:  `&lt;/rpc&gt;&lt;evil/&gt;`,
		},
		{
			name:  "empty string",
			input: ``,
			want:  ``,
		},
		{
			name:  "ASCII without specials passes through unchanged",
			input: `queue-1_A`,
			want:  `queue-1_A`,
		},
		{
			name:  "UTF-8 non-ASCII passes through unchanged",
			input: `vpn-名前`,
			want:  `vpn-名前`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Escape(tc.input)
			if got != tc.want {
				t.Errorf("Escape(%q)\n  got:  %q\n  want: %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestEscape_RoundTrip wraps Escape's output in a minimal XML element and
// parses it with encoding/xml. If Escape produced malformed output, the
// decode fails; if Escape dropped data, the decoded text content won't
// match the original input. This is the "proves the escape is XML-safe in
// practice" bullet from the T5 Definition of Done.
func TestEscape_RoundTrip(t *testing.T) {
	inputs := []string{
		`<foo>`,
		`a & b`,
		`'x' "y"`,
		`</rpc><evil/>`,
		``,
		`queue-1_A`,
		`vpn-名前`,
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			wrapped := "<x>" + Escape(in) + "</x>"

			var decoded struct {
				XMLName xml.Name `xml:"x"`
				Value   string   `xml:",chardata"`
			}
			if err := xml.Unmarshal([]byte(wrapped), &decoded); err != nil {
				t.Fatalf("wrapped output failed to parse: %v\n  wrapped: %q", err, wrapped)
			}
			if decoded.Value != in {
				t.Errorf("round-trip mismatch\n  original: %q\n  decoded:  %q\n  wrapped:  %q",
					in, decoded.Value, wrapped)
			}
		})
	}
}
