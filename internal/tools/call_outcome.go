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

package tools

import (
	"context"
	"errors"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// classifyCallOutcome maps one tool call onto the shared success/error/
// cancelled vocabulary (ADR-009). CallTool decides it once and hands the same
// value to the metric, the audit record, the `tool invoked` line and the
// dispatch span, so the four cannot disagree (SOL-156509).
//
// ctx must be the context CallTool was given. A call is cancelled only when
// that context has ended AND toolErr is that ending (the caller cancelled, or
// its own deadline passed) — the rule the token-exchange span uses
// (SOL-153333):
//
//   - Our own SEMP retry budget is a child context inside Sender.Do, so its
//     timeout leaves ctx live and stays an error.
//   - A real failure that a cancel raced (a broker error reply, a panic) is
//     not that ending, so the failure is what gets reported.
//
// A nil toolErr is a success even if a cancel followed: the broker answered.
func classifyCallOutcome(ctx context.Context, toolErr error) metrics.Outcome {
	switch {
	case toolErr == nil:
		return metrics.OutcomeSuccess
	case ctx.Err() != nil && errors.Is(toolErr, ctx.Err()):
		return metrics.OutcomeCancelled
	default:
		return metrics.OutcomeError
	}
}
