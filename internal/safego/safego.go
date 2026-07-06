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

// Package safego runs work on new goroutines with panic recovery.
//
// errgroup does not recover panics in the goroutines it starts, and the
// request-path recovery nets — recovery.HTTPMiddleware (internal/middleware/
// recovery) and withRecovery (internal/tools) — trap panics only on their OWN
// goroutine. A panic on a goroutine that a handler SPAWNS is therefore caught
// nowhere and crashes the whole multi-session process. safego.Go is that
// missing net, applied at the point of goroutine creation (SOL-151514).
package safego

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"golang.org/x/sync/errgroup"
)

// Go runs fn on a new goroutine managed by g, recovering any panic in fn and
// converting it into the goroutine's returned error. Use it in place of
// g.Go(fn) wherever a worker goroutine runs code that could panic: without the
// recover, a panic escapes the errgroup goroutine and crashes the process,
// because errgroup does not recover the goroutines it starts.
//
// On a recovered panic it logs at ERROR with event="panic_recovered", the panic
// value's Go TYPE (panic_type) and a stack trace, and returns a generic error
// carrying only that type. It never logs or returns the panic value's text:
// panic values are unaudited and can carry arbitrary strings, the same
// secure-logging rule recovery.HTTPMiddleware and withRecovery apply
// (docs/internal/secure-logging-rules.md). The stack pinpoints the panic site
// without echoing the value.
func Go(g *errgroup.Group, fn func() error) {
	g.Go(func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered panic in worker goroutine",
					slog.String("event", "panic_recovered"),
					slog.String("panic_type", fmt.Sprintf("%T", r)),
					slog.String("stack", string(debug.Stack())))
				err = fmt.Errorf("worker goroutine panicked (%T)", r)
			}
		}()
		return fn()
	})
}
