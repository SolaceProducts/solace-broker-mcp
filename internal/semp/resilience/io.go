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

package resilience

import (
	"errors"
	"fmt"
	"io"
)

// ErrResponseTooLarge is returned by ReadCappedBody when the source reader
// yields more than the configured cap. Callers branch on this with errors.Is
// to distinguish "broker misbehaved / MITM" from generic I/O errors.
var ErrResponseTooLarge = errors.New("response body exceeded size cap")

// ReadCappedBody reads from r into memory, capped at limit bytes. Returns
// ErrResponseTooLarge if r yields more than limit bytes; the body buffer is
// returned as nil in that case (callers should not consume a partial read).
//
// The implementation asks for limit+1 bytes via io.LimitReader. If the
// result is longer than limit, the source had more data than the cap
// allowed.
func ReadCappedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%w (cap: %d bytes)", ErrResponseTooLarge, limit)
	}
	return body, nil
}
