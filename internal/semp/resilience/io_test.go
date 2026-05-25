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
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadCappedBody_UnderCap(t *testing.T) {
	body, err := ReadCappedBody(strings.NewReader("hello"), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}

func TestReadCappedBody_AtCap(t *testing.T) {
	// A body exactly at the cap must succeed (cap is inclusive).
	body, err := ReadCappedBody(strings.NewReader("1234567890"), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != "1234567890" {
		t.Errorf("body = %q, want exact cap content", body)
	}
}

func TestReadCappedBody_OverCap(t *testing.T) {
	// A body one byte past the cap must fail with ErrResponseTooLarge.
	body, err := ReadCappedBody(strings.NewReader("12345678901"), 10)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("err = %v, want errors.Is(_, ErrResponseTooLarge)", err)
	}
	if body != nil {
		t.Errorf("body = %q, want nil on over-cap read", body)
	}
}

func TestReadCappedBody_WellOverCap(t *testing.T) {
	// Confirm the cap holds when the source is much larger than the limit.
	large := bytes.Repeat([]byte("x"), 1024*1024) // 1 MiB
	_, err := ReadCappedBody(bytes.NewReader(large), 1024)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(_, ErrResponseTooLarge)", err)
	}
}

// errReader returns a configured error on every Read.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

func TestReadCappedBody_UnderlyingReadError_PropagatesWrapped(t *testing.T) {
	want := errors.New("nope")
	_, err := ReadCappedBody(&errReader{err: want}, 100)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want errors.Is(_, %v)", err, want)
	}
	if errors.Is(err, ErrResponseTooLarge) {
		t.Error("read error should not be classified as ErrResponseTooLarge")
	}
}

// Sanity check: io.EOF from the underlying reader is the normal end-of-stream
// signal and must NOT propagate as an error from ReadCappedBody. io.ReadAll
// already handles this, but the test pins the behaviour.
func TestReadCappedBody_EOFIsNotAnError(t *testing.T) {
	body, err := ReadCappedBody(io.MultiReader(strings.NewReader("abc")), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != "abc" {
		t.Errorf("body = %q, want %q", body, "abc")
	}
}
