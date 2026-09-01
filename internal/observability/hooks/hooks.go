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

// RunAll runs every registered hook concurrently and returns once they have
// all finished or ctx's deadline passes, whichever comes first. Each hook
// gets ctx directly, so every hook effectively gets the same full budget
// regardless of how many others are registered — one slow or blocked hook
// cannot shrink another's share. A hook still running when ctx expires is
// abandoned, not waited on; RunAll returns anyway, bounding the total wall
// time to ctx's deadline no matter how many hooks are registered. A failing
// hook is logged and does not affect the caller's shutdown outcome.
//
// RunAll is a no-op on a nil Registry or one with no hooks registered, so an
// unused registry costs nothing.
//
// Each hook runs via safego.Go: a panicking hook is recovered and logged
// rather than crashing the process mid-shutdown, the same guarantee
// safego gives every other worker goroutine in this codebase.
func (r *Registry) RunAll(ctx context.Context) {
	if r == nil || len(r.hooks) == 0 {
		return
	}

	var g errgroup.Group
	for _, h := range r.hooks {
		safego.Go(&g, func() error {
			if err := h.fn(ctx); err != nil {
				slog.Error("shutdown hook failed", slog.String("hook", h.name), slog.String("error", err.Error()))
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
