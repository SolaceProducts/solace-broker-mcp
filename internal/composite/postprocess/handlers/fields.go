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

package handlers

import "encoding/json"

// numField accepts any of the numeric shapes JSON decoders produce: float64
// (encoding/json default for map[string]any), json.Number (decoder with
// UseNumber), int / int64 (custom unmarshalers). Keeps the handler insulated
// from future SEMP-client decode-mode changes. Returns ok=false for missing,
// nil, or unexpected types so the caller can skip the row rather than abort.
func numField(item map[string]any, name string) (float64, bool) {
	switch v := item[name].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// stringField returns the named string from item. Returns ok=false for missing,
// nil, or unexpected types so the caller can skip the row rather than abort.
func stringField(item map[string]any, name string) (string, bool) {
	v, ok := item[name].(string)
	return v, ok
}

// boolField returns the named bool from item. Returns ok=false for missing,
// nil, or unexpected types so the caller can skip the row rather than abort.
func boolField(item map[string]any, name string) (bool, bool) {
	v, ok := item[name].(bool)
	return v, ok
}
