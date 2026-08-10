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
