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

package tokenexchange

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// DeduplicationKeyInput defines the fields that determine whether two
// token-exchange requests are logically identical. Every field in this
// struct participates in the key — if a field is here, it matters for
// deduplication; if it's not here, it doesn't.
//
// Used by singleflight (in-flight deduplication) and the future token
// cache (cross-time deduplication).
type DeduplicationKeyInput struct {
	SubjectToken string
	BrokerAlias  string
}

// String, GoString, and LogValue redact SubjectToken so
// DeduplicationKeyInput never leaks it through fmt formatting or slog
// reflection. Value receivers so *DeduplicationKeyInput is covered too.
// Pattern mirrors cache.CachedCredential.
func (d DeduplicationKeyInput) String() string {
	return fmt.Sprintf("DeduplicationKeyInput{BrokerAlias: %q}", d.BrokerAlias)
}

func (d DeduplicationKeyInput) GoString() string {
	return d.String()
}

func (d DeduplicationKeyInput) LogValue() slog.Value {
	return slog.GroupValue(slog.String("broker_alias", d.BrokerAlias))
}

// computeDeduplicationKey produces a deterministic, collision-resistant
// hex string from the input fields. Same inputs always produce the same
// key. Fields are separated by a NUL byte to prevent ("ab","c") from
// colliding with ("a","bc").
func computeDeduplicationKey(input DeduplicationKeyInput) string {
	h := sha256.New()
	h.Write([]byte(input.SubjectToken))
	h.Write([]byte{0x00})
	h.Write([]byte(input.BrokerAlias))
	return hex.EncodeToString(h.Sum(nil))
}
