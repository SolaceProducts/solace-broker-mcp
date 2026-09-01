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

// Package hooks owns the shutdown-hook registry (SOL-152449): a place for
// OTel providers to register a bounded flush without each editing
// cmd/server's shutdown path.
package hooks

import (
	"context"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/SolaceProducts/solace-broker-mcp/internal/safego"
)

// namedHook pairs a hook with the name it logs under.
type namedHook struct {
	name string
	fn   func(context.Context) error
}

// Registry holds shutdown hooks registered at startup and run once at
// shutdown. The zero value has no hooks; a nil *Registry is also valid and
// runs no hooks (see RunAll).
type Registry struct {
	hooks []namedHook
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a shutdown hook under name. Registration happens at startup,
// before any shutdown can begin, so it is not safe for concurrent use with
// RunAll.
func (r *Registry) Register(name string, fn func(context.Context) error) {
	r.hooks = append(r.hooks, namedHook{name: name, fn: fn})
}

// RunAll runs every registered hook concurrently, returning once they all
// finish or ctx's deadline passes. Each hook gets ctx directly, so every hook
// gets the full budget regardless of how many others are registered. A slow
// hook is abandoned rather than waited on; a panicking one is recovered (via
// safego.Go) instead of crashing the process. Either way the rest keep
// running, and a failing hook never changes the caller's shutdown outcome.
//
// A nil or empty Registry is a no-op — an unused registry costs nothing.
func (r *Registry) RunAll(ctx context.Context) {
	if r == nil || len(r.hooks) == 0 {
		return
	}

	var g errgroup.Group
	for _, h := range r.hooks {
		safego.Go(&g, func() error {
			if err := h.fn(ctx); err != nil {
				// No err.Error(): hooks wrap external systems (an OTel
				// collector), whose error text is unaudited (docs/internal/
				// secure-logging-rules.md Rule 5).
				slog.Error("shutdown hook failed", slog.String("hook", h.name))
				return err
			}
			return nil
		})
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = g.Wait()
	}()

	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("shutdown hook budget exceeded; abandoning any hooks still running")
	}
}
