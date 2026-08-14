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
	"strings"
	"testing"
)

func TestComputeDeduplicationKey_Deterministic(t *testing.T) {
	t.Parallel()

	key1 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "tokenA", BrokerAlias: "brokerA"})
	key2 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "tokenA", BrokerAlias: "brokerA"})

	if key1 != key2 {
		t.Errorf("same inputs produced different keys: %q vs %q", key1, key2)
	}
	if len(key1) != 64 {
		t.Errorf("key length = %d, want 64 (SHA-256 hex)", len(key1))
	}
}

func TestComputeDeduplicationKey_DifferentTokensDifferentKeys(t *testing.T) {
	t.Parallel()

	key1 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "tokenA", BrokerAlias: "broker"})
	key2 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "tokenB", BrokerAlias: "broker"})

	if key1 == key2 {
		t.Errorf("different subjectTokens produced same key: %q", key1)
	}
}

func TestComputeDeduplicationKey_DifferentAliasesDifferentKeys(t *testing.T) {
	t.Parallel()

	key1 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "token", BrokerAlias: "brokerA"})
	key2 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "token", BrokerAlias: "brokerB"})

	if key1 == key2 {
		t.Errorf("different brokerAliases produced same key: %q", key1)
	}
}

// TestComputeDeduplicationKey_DifferentAudiencesDifferentKeys is the
// regression test for SOL-152981: Audience must participate in the key like
// every other field, per this struct's own documented contract.
func TestComputeDeduplicationKey_DifferentAudiencesDifferentKeys(t *testing.T) {
	t.Parallel()

	key1 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "token", BrokerAlias: "broker", Audience: "aud-a"})
	key2 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "token", BrokerAlias: "broker", Audience: "aud-b"})

	if key1 == key2 {
		t.Errorf("different audiences produced same key: %q", key1)
	}
}

func TestComputeDeduplicationKey_EmptyInputsProduceValidKey(t *testing.T) {
	t.Parallel()

	key := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "", BrokerAlias: "", Audience: ""})

	if len(key) != 64 {
		t.Errorf("key length = %d, want 64", len(key))
	}

	h := sha256.New()
	h.Write([]byte{0x00})
	h.Write([]byte{0x00})
	want := hex.EncodeToString(h.Sum(nil))
	if key != want {
		t.Errorf("key = %q, want SHA-256 of two NUL bytes = %q", key, want)
	}
}

// NUL separator prevents collisions between ("ab","c") and ("a","bc").
func TestComputeDeduplicationKey_NULSeparatorPreventsCollision(t *testing.T) {
	t.Parallel()

	key1 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "ab", BrokerAlias: "c"})
	key2 := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "a", BrokerAlias: "bc"})

	if key1 == key2 {
		t.Errorf("NUL separator failed: (\"ab\",\"c\") and (\"a\",\"bc\") produced same key: %q", key1)
	}
}

func TestComputeDeduplicationKey_OutputIsLowercaseHex(t *testing.T) {
	t.Parallel()

	key := computeDeduplicationKey(DeduplicationKeyInput{SubjectToken: "any-token", BrokerAlias: "any-broker"})

	decoded, err := hex.DecodeString(key)
	if err != nil {
		t.Fatalf("key %q is not valid hex: %v", key, err)
	}
	if len(decoded) != sha256.Size {
		t.Errorf("decoded length = %d, want %d (SHA-256)", len(decoded), sha256.Size)
	}

	if key != strings.ToLower(key) {
		t.Errorf("key %q is not lowercase", key)
	}
}
