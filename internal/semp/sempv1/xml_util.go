package sempv1

import (
	"bytes"
	"encoding/xml"
)

// Escape returns s with XML special characters replaced by entity references,
// suitable for embedding as the text content of an XML element. MANDATORY for
// all externally-sourced values (VPN name, queue name, operator input) before
// concatenating them into a SEMPv1 request string.
//
// Text content only. For attribute values, a separate EscapeAttr helper
// should be added rather than reusing this one — the escape rules differ.
// See docs/semp/sempv1-client-design.md §9.3.
//
// Example:
//
//	xmlReq := fmt.Sprintf(
//	    `<rpc><show><queue><name>%s</name></queue></show></rpc>`,
//	    sempv1.Escape(queueName),
//	)
func Escape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
