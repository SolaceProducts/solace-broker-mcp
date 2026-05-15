# PR Review Findings

Actionable findings from PR reviews that should be addressed.

## PR #20 — SOL-148429: VPN-Level Monitoring Tools

### Paginate + parallel validation gap

**Status:** Deferred (not caused by this PR)

`executeBatch` in `executor.go` has its own inline execution logic that never
calls `executeStep`. If a future tool definition combines `paginate: true` with
`parallel: true` on a step, pagination would be silently skipped — only a single
page of results returned with no error.

**Fix:** Add a validation rule in `loader.go` `validateTool()` to reject
`paginate: true` combined with `parallel: true` at load time:

```go
for _, step := range tool.Steps {
    if step.Paginate && step.Parallel {
        return fmt.Errorf("step %q: paginate and parallel cannot both be true", step.ID)
    }
}
```

**Why deferred:** The `executeBatch` code exists on main and is unchanged by
PR #20. No current tool combines the two flags. This is a future-proofing
concern, not a regression.

### Migrate `list-client-subscriptions` to the pagination engine

**Status:** Deferred (follow-up to PR #20)

The existing `list-client-subscriptions` tool handles `maxResults` via a Go
template expression in the `count` arg and returns raw SEMP pagination metadata
in the response. It does not follow `nextPageUri` links — it returns a single
page only.

The new pagination engine (`paginate: true` + `executePaginatedStep`) added by
PR #20 handles this automatically: follows all pages, enforces maxResults
default/cap, and returns a `truncated` flag.

**Fix:** Convert `list-client-subscriptions` to use `paginate: true` on its
step and `strategy: paginate` on its result, consistent with `list-vpns`,
`list-queues`, and `list-clients`.

**Why deferred:** Not in scope for PR #20. The existing tool works correctly
for single-page results. Migration is a consistency improvement.
