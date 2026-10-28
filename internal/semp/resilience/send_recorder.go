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
	"context"
	"net/http"
	"sync/atomic"
)

// SendRecorder records whether any write issued under a context reached the
// broker, for the operation audit record's request_sent field (SOL-156509).
//
// It is separate from RetriesExhaustedError.NonIdempotent on purpose. That
// flag is set only for POST/PATCH, because it also drives the retry rule, and
// only on a RetriesExhaustedError — which a cancel during backoff (a bare
// ctx.Err()) or a broker error reply never produces. The recorder is set from
// the same WroteHeaders hook as retryState.requestWritten, for every write
// method, and outlives Do so the caller can read it whatever error came back.
//
// A write is any method other than GET/HEAD that the caller did not mark
// WithRetrySafe (SEMPv1 <show> reads travel over POST). Safe for concurrent
// use: net/http calls the hook on its connection write goroutine.
type SendRecorder struct {
	sent atomic.Bool
}

type sendRecorderKey struct{}

// WithSendRecorder returns a context carrying a fresh SendRecorder, and the
// recorder. Every Sender.Do under the returned context reports to it.
func WithSendRecorder(ctx context.Context) (context.Context, *SendRecorder) {
	rec := &SendRecorder{}
	return context.WithValue(ctx, sendRecorderKey{}, rec), rec
}

// WriteSent reports whether a write reached the broker. A nil recorder
// reports false.
func (r *SendRecorder) WriteSent() bool {
	return r != nil && r.sent.Load()
}

// sendRecorderFrom returns the recorder on ctx, or nil.
func sendRecorderFrom(ctx context.Context) *SendRecorder {
	rec, _ := ctx.Value(sendRecorderKey{}).(*SendRecorder)
	return rec
}

// isWriteRequest reports whether a request with this method and retry-safe
// marker is a write for SendRecorder's purposes.
func isWriteRequest(method string, retrySafe bool) bool {
	return method != http.MethodGet && method != http.MethodHead && !retrySafe
}
