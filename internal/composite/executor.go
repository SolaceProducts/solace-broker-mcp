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

package composite

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"text/template"

	"golang.org/x/sync/errgroup"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
	"github.com/SolaceProducts/solace-broker-mcp/internal/safego"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// ExecuteContext holds state during composite tool execution. Each tool
// invocation gets its own context — not shared across calls. Params holds
// the input parameters (with the broker param already stripped). StepResults
// accumulates results from each completed step, keyed by step ID.
//
// Item is set only during a fan-out iteration (see fetchFanOut) and holds
// the parent-step row that the current iteration is bound to. Templates
// reference it as .Item, e.g. `{{.Item.msgVpnName}}`. It is nil for
// non-fan-out steps; missingkey=error in ResolveArgs catches templates that
// reference .Item outside a fan-out.
type ExecuteContext struct {
	Params      map[string]any
	StepResults map[string]map[string]any
	Item        map[string]any
}

// ExecutionBatch groups steps for execution. Sequential batches contain a
// single step run in the current goroutine. Parallel batches contain
// multiple adjacent parallel steps run concurrently via errgroup.
type ExecutionBatch struct {
	Sequential bool
	Steps      []Step
}

// CompositeExecutor orchestrates multi-step tool execution. It holds the
// operation catalog (parsed from OpenAPI specs) for looking up step operations.
// It does not hold a client — the client is passed per-call via Execute()
// because different calls may target different brokers.
type CompositeExecutor struct {
	operations map[string]*sempv2.Operation
	// maxPages is the hard ceiling on pagination loop iterations, defending
	// against a broker (or adversarial input) that returns a perpetual
	// nextPageUri. In practice the item-count cap (capMax) terminates the
	// loop first; maxPages is a safety net for edge cases like very large
	// page sizes or bugs in item extraction. Stored on the executor rather
	// than as a package-level var so tests can override it without racing
	// other parallel tests on a shared global.
	maxPages int
}

// defaultMaxPages is the production value of CompositeExecutor.maxPages.
const defaultMaxPages = 1000

// Operations returns the executor's operation catalog. Exposed for callers
// that need to introspect an operation outside of execution — e.g.
// CompositeToolHandler.Metadata() building an output schema from a step's
// resolved response fields (BuildStrictOutputSchema).
func (ce *CompositeExecutor) Operations() map[string]*sempv2.Operation {
	return ce.operations
}

// NewCompositeExecutor creates an executor with the given operation catalog.
// The operations map is keyed by prefixed operation ID (e.g.,
// "monitor/getMsgVpnQueue") matching the format produced by sempv2.ParseSpecs().
func NewCompositeExecutor(operations map[string]*sempv2.Operation) *CompositeExecutor {
	return &CompositeExecutor{
		operations: operations,
		maxPages:   defaultMaxPages,
	}
}

// Execute runs all steps of a composite tool against the given client and
// returns the combined result. The client is resolved per-call by the registry
// handler — the executor does not know about brokers or pools.
//
// It defensively strips the "broker" key from params before building the
// template context, so YAML templates cannot reference {{.Params.broker}}.
func (ce *CompositeExecutor) Execute(ctx context.Context, tool CompositeTool, client sempv2.Client, params map[string]any) (map[string]any, error) {
	// Strip broker from params defensively. The handler should already do this,
	// but the executor ensures it regardless.
	execParams := make(map[string]any, len(params))
	for k, v := range params {
		if k != "broker" {
			execParams[k] = v
		}
	}

	// Carry the tool's declared non-idempotency down to the retry policy. The
	// SEMP layer otherwise infers replay safety from the HTTP method, which is
	// wrong for the action API: it routes destructive RPC over PUT, so a
	// replayed delete-queue-messages purges whatever was spooled since the
	// caller's request. Only an explicit `idempotent: false` marks the request;
	// omitted or true leaves the existing policy untouched.
	if tool.Annotations.Idempotent != nil && !*tool.Annotations.Idempotent {
		ctx = resilience.WithRetryUnsafe(ctx)
	}

	execCtx := &ExecuteContext{
		Params:      execParams,
		StepResults: make(map[string]map[string]any),
	}

	batches := GroupStepsIntoBatches(tool.Steps)

	for _, batch := range batches {
		if batch.Sequential {
			if err := ce.executeStep(ctx, batch.Steps[0], client, execCtx); err != nil {
				return nil, err
			}
		} else {
			if err := ce.executeBatch(ctx, batch, client, execCtx); err != nil {
				return nil, err
			}
		}
	}

	return ApplyResultStrategy(tool.Result, execCtx.StepResults)
}

// GroupStepsIntoBatches groups steps into sequential and parallel execution
// batches. Each non-parallel step gets its own sequential batch. Adjacent
// parallel steps are grouped into a single parallel batch.
func GroupStepsIntoBatches(steps []Step) []ExecutionBatch {
	var batches []ExecutionBatch
	var currentBatch *ExecutionBatch

	for _, step := range steps {
		if step.Parallel {
			if currentBatch == nil || currentBatch.Sequential {
				batches = append(batches, ExecutionBatch{
					Sequential: false,
					Steps:      []Step{step},
				})
				currentBatch = &batches[len(batches)-1]
			} else {
				currentBatch.Steps = append(currentBatch.Steps, step)
			}
		} else {
			batches = append(batches, ExecutionBatch{
				Sequential: true,
				Steps:      []Step{step},
			})
			currentBatch = &batches[len(batches)-1]
		}
	}

	return batches
}

// executeStep executes a single step: resolves template args, looks up the
// operation, constructs the request body if needed, calls the client, and
// stores the result in the execution context.
func (ce *CompositeExecutor) executeStep(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) error {
	data, err := ce.runStep(ctx, step, client, execCtx)
	if err != nil {
		return err
	}
	execCtx.StepResults[step.ID] = data
	return nil
}

// runStep dispatches a step to the fan-out, paginated, or single-call fetch
// and returns the resulting data map. It does not write to execCtx.StepResults
// so that parallel callers can collect results without a shared-map race.
func (ce *CompositeExecutor) runStep(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) (map[string]any, error) {
	if step.ForEach != "" {
		return ce.fetchFanOut(ctx, step, client, execCtx)
	}
	return ce.runSingle(ctx, step, client, execCtx)
}

// runSingle runs a non-fan-out step. It honors FollowPages for paginated
// list operations and otherwise issues a single SEMP call. Called both
// directly by runStep and per-iteration by fetchFanOut.
func (ce *CompositeExecutor) runSingle(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) (map[string]any, error) {
	if step.FollowPages {
		return ce.fetchPaginated(ctx, step, client, execCtx)
	}

	args, err := ResolveArgs(step.Args, execCtx)
	if err != nil {
		return nil, fmt.Errorf("tool step %s: failed to resolve args: %w", step.ID, err)
	}
	applySelect(args, step.Select)

	op, ok := ce.operations[step.Operation]
	if !ok {
		return nil, fmt.Errorf("tool step %s: operation %q not found in operation catalog", step.ID, step.Operation)
	}

	args, err = ce.constructRequestBody(op, args, execCtx.Params)
	if err != nil {
		return nil, fmt.Errorf("tool step %s: %w", step.ID, err)
	}

	result, err := client.Execute(ctx, op, args)
	if err != nil {
		return nil, fmt.Errorf("tool step %s: %w", step.ID, err)
	}

	return result.Data, nil
}

// Fan-out concurrency defaults. Both are framework-level: fanOutDefaultConcurrency
// is what a step gets when it doesn't set Concurrency; fanOutMaxConcurrency caps
// what the YAML author can request, defending broker HTTP handlers from a
// mistaken (or hostile) high value in a tool definition.
const (
	fanOutDefaultConcurrency = 8
	fanOutMaxConcurrency     = 32
)

// fetchFanOut iterates a prior step's data[] rows and issues a SEMP call per
// row concurrently, up to step.Concurrency (default fanOutDefaultConcurrency).
// Per-row templating exposes the current row as .Item so Args can reference
// row fields. Rows for which ForEachIf resolves to false are skipped and
// counted under "skipped". Results are keyed by row[ForEachKey] under "byKey".
// Fail-fast: the first per-row error cancels the errgroup and returns.
//
// Loader validation guarantees ForEach names a prior step and ForEachKey is
// non-empty; this method still defends against a parent step whose data field
// is the wrong type (broken definition), rows whose ForEachKey value is
// missing or non-string, and duplicate key values (last-writer-wins would
// silently drop a per-row result). An absent or empty data field on the
// parent is legal and yields an empty byKey. ForEachIf resolves under
// missingkey=error and must produce a strconv.ParseBool literal; any
// deviation is a hard error rather than a skip, so a broken predicate
// surfaces at load-time verification instead of silently filtering rows.
func (ce *CompositeExecutor) fetchFanOut(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) (map[string]any, error) {
	parent, ok := execCtx.StepResults[step.ForEach]
	if !ok {
		return nil, fmt.Errorf("tool step %s: forEach step %q has no result in context; loader should have caught this", step.ID, step.ForEach)
	}
	var rawItems []any
	if raw, present := parent["data"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("tool step %s: forEach parent %q data: want []any, got %T", step.ID, step.ForEach, raw)
		}
		rawItems = items
	}

	if _, ok := ce.operations[step.Operation]; !ok {
		return nil, fmt.Errorf("tool step %s: operation %q not found in operation catalog", step.ID, step.Operation)
	}

	concurrency := step.Concurrency
	if concurrency <= 0 {
		concurrency = fanOutDefaultConcurrency
	}

	byKey := make(map[string]any, len(rawItems))
	var byKeyMu sync.Mutex
	seenKeys := make(map[string]int, len(rawItems))
	skipped := 0

	g, gCtx := errgroup.WithContext(ctx)
	sem := make(chan struct{}, concurrency)

	for i, raw := range rawItems {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool step %s: forEach parent %q data[%d]: want object, got %T", step.ID, step.ForEach, i, raw)
		}

		if step.ForEachIf != "" {
			iterCtx := &ExecuteContext{Params: execCtx.Params, StepResults: execCtx.StepResults, Item: item}
			resolved, err := resolveTemplateString("forEachIf", step.ForEachIf, iterCtx)
			if err != nil {
				return nil, fmt.Errorf("tool step %s: forEach parent %q data[%d]: forEachIf: %w", step.ID, step.ForEach, i, err)
			}
			pass, err := strconv.ParseBool(strings.TrimSpace(resolved))
			if err != nil {
				return nil, fmt.Errorf("tool step %s: forEach parent %q data[%d]: forEachIf must resolve to a bool literal, got %q", step.ID, step.ForEach, i, resolved)
			}
			if !pass {
				skipped++
				continue
			}
		}

		keyVal, ok := item[step.ForEachKey]
		if !ok {
			return nil, fmt.Errorf("tool step %s: forEachKey %q missing on forEach parent %q data[%d]", step.ID, step.ForEachKey, step.ForEach, i)
		}
		keyStr, ok := keyVal.(string)
		if !ok {
			return nil, fmt.Errorf("tool step %s: forEachKey %q on forEach parent %q data[%d]: want string, got %T", step.ID, step.ForEachKey, step.ForEach, i, keyVal)
		}
		if prev, dup := seenKeys[keyStr]; dup {
			return nil, fmt.Errorf("tool step %s: duplicate forEachKey %q=%q on forEach parent %q at data[%d] (already seen at data[%d])", step.ID, step.ForEachKey, keyStr, step.ForEach, i, prev)
		}
		seenKeys[keyStr] = i

		// Acquire the concurrency slot in the dispatch loop, not inside the
		// spawned goroutine, so at most `concurrency` goroutines exist at any
		// moment rather than one per parent row. Matters when a fan-out
		// scans hundreds+ of rows (large VPN or client populations). Select
		// on gCtx.Done() so a peer error aborts the loop instead of blocking
		// on a semaphore whose slots will never drain.
		select {
		case sem <- struct{}{}:
		case <-gCtx.Done():
		}
		if gCtx.Err() != nil {
			break
		}

		idx := i
		row := item
		key := keyStr
		safego.Go(g, func() error {
			defer func() { <-sem }()

			iterCtx := &ExecuteContext{Params: execCtx.Params, StepResults: execCtx.StepResults, Item: row}
			data, err := ce.runSingle(gCtx, step, client, iterCtx)
			if err != nil {
				return fmt.Errorf("forEach %s[%d] key=%q: %w", step.ForEach, idx, key, err)
			}
			byKeyMu.Lock()
			byKey[key] = data
			byKeyMu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("tool step %s: %w", step.ID, err)
	}

	result := map[string]any{"byKey": byKey}
	if skipped > 0 {
		result["skipped"] = skipped
	}
	return result, nil
}

// resolveTemplateString parses and executes a single Go text/template against
// the execution context, mirroring ResolveArgs' safety guarantees (parse-error
// wrapping, panic recovery via safeTemplateExecute). Used by fetchFanOut for
// the ForEachIf predicate — a single template value, not the args map form.
func resolveTemplateString(name, tmplStr string, execCtx *ExecuteContext) (string, error) {
	tmpl, err := template.New(name).Option("missingkey=error").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	return safeTemplateExecute(tmpl, execCtx)
}

// applySelect joins step.Select (if non-empty) into the comma-separated
// "select" arg the SEMP client sends as a query parameter. The ", " separator
// is cosmetic — SEMPv2 accepts either "a,b" or "a, b". The structured form on
// Step is what lets ValidatePostProcess cross-check handler RequiredFields
// without re-parsing the joined string. validateTool in loader.go rejects the
// case where args["select"] is also templated, so this overwrite is safe.
func applySelect(args map[string]any, fields []string) {
	if len(fields) == 0 {
		return
	}
	args["select"] = strings.Join(fields, ", ")
}

// fetchPaginated executes a step that returns paginated results. It follows
// SEMP's nextPageUri links until one of three termination conditions fires:
// (1) the merged item count reaches maxResults, (2) the broker stops returning
// a nextPageUri or an empty page, or (3) the loop has run for
// CompositeExecutor.maxPages iterations (a defense-in-depth backstop against
// a perpetual nextPageUri).
// It returns a map containing the merged data array under "data" plus a
// truncated flag indicating whether more results exist beyond what was
// returned. The caller is responsible for storing the result in execCtx.
func (ce *CompositeExecutor) fetchPaginated(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) (map[string]any, error) {
	baseArgs, err := ResolveArgs(step.Args, execCtx)
	if err != nil {
		return nil, fmt.Errorf("tool step %s: failed to resolve args: %w", step.ID, err)
	}
	applySelect(baseArgs, step.Select)

	op, ok := ce.operations[step.Operation]
	if !ok {
		return nil, fmt.Errorf("tool step %s: operation %q not found in operation catalog", step.ID, step.Operation)
	}

	baseArgs, err = ce.constructRequestBody(op, baseArgs, execCtx.Params)
	if err != nil {
		return nil, fmt.Errorf("tool step %s: %w", step.ID, err)
	}

	maxResults := resolveMaxResults(execCtx.Params)
	allItems := make([]any, 0)
	truncated := false
	pageLimitHit := false
	args := baseArgs

	for pageCount := 0; ; pageCount++ {
		if pageCount >= ce.maxPages {
			// Hard safety cap: stop following pages and surface an incomplete result
			// rather than looping forever against a broker that returns a perpetual
			// nextPageUri (broker bug or adversarial input).
			slog.Warn("pagination page cap reached; result may be incomplete",
				slog.String("step", step.ID),
				slog.Int("pages", pageCount),
				slog.Int("items_collected", len(allItems)))
			truncated = true
			pageLimitHit = true
			break
		}

		result, err := client.Execute(ctx, op, args)
		if err != nil {
			return nil, fmt.Errorf("tool step %s: %w", step.ID, err)
		}

		items := extractDataItems(result.Data)
		if len(items) == 0 {
			break
		}

		remaining := maxResults - len(allItems)
		if len(items) >= remaining {
			allItems = append(allItems, items[:remaining]...)
			// Truncated if the page had more items than we needed, or if more pages exist.
			truncated = len(items) > remaining || extractNextPageURI(result.Data) != ""
			break
		}

		allItems = append(allItems, items...)

		nextURI := extractNextPageURI(result.Data)
		if nextURI == "" {
			break
		}

		cursor, parseErr := parseCursorFromURI(nextURI)
		if parseErr != nil {
			return nil, fmt.Errorf("tool step %s: failed to parse pagination cursor from nextPageUri %q: %w", step.ID, nextURI, parseErr)
		}
		if cursor == "" {
			return nil, fmt.Errorf("tool step %s: empty pagination cursor extracted from nextPageUri %q", step.ID, nextURI)
		}

		args = appendCursor(baseArgs, cursor)
	}

	result := map[string]any{
		"data":      allItems,
		"truncated": truncated,
	}
	if truncated {
		switch {
		case pageLimitHit:
			result["truncatedMessage"] = fmt.Sprintf(
				"Pagination stopped after %d pages; result is incomplete. This usually indicates a broker issue with a persistent nextPageUri — narrow the query (e.g., by msgVpnName or a where filter) or contact your broker administrator.",
				ce.maxPages)
		case maxResults >= capMax:
			result["truncatedMessage"] = fmt.Sprintf(
				"More results exist but the maximum limit of %d has been reached. Not all results are shown.",
				capMax)
		default:
			result["truncatedMessage"] = fmt.Sprintf(
				"Results limited to %d. Use maxResults (up to %d) to retrieve more.",
				maxResults, capMax)
		}
	}
	return result, nil
}

// Item-count limits used by resolveMaxResults and truncation messages. The
// page-count ceiling lives on CompositeExecutor.maxPages (see the struct
// definition above) because the test override needs to be per-instance.
const (
	defaultMax = 100
	capMax     = 500
)

// resolveMaxResults reads maxResults from the execution params, applying a
// default of 100 and a cap of 500.
func resolveMaxResults(params map[string]any) int {

	raw, ok := params["maxResults"]
	if !ok {
		return defaultMax
	}

	var n int
	switch v := raw.(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	case int64:
		n = int(v)
	default:
		return defaultMax
	}

	if n <= 0 {
		return defaultMax
	}
	if n > capMax {
		return capMax
	}
	return n
}

// extractDataItems returns the data array from a SEMP list response, or nil
// if the response contains no data field or it is not a slice.
func extractDataItems(data map[string]any) []any {
	raw, ok := data["data"]
	if !ok {
		return nil
	}
	items, _ := raw.([]any)
	return items
}

// extractNextPageURI returns the nextPageUri from SEMP pagination metadata,
// or an empty string if the response has no further pages.
func extractNextPageURI(data map[string]any) string {
	meta, ok := data["meta"].(map[string]any)
	if !ok {
		return ""
	}
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		return ""
	}
	uri, _ := paging["nextPageUri"].(string)
	return uri
}

// parseCursorFromURI extracts the cursor query parameter from a SEMP nextPageUri.
func parseCursorFromURI(rawURI string) (string, error) {
	u, err := url.Parse(rawURI)
	if err != nil {
		return "", err
	}
	return u.Query().Get("cursor"), nil
}

// appendCursor returns a copy of baseArgs with the cursor key added for the
// next paginated SEMP request.
func appendCursor(baseArgs map[string]any, cursor string) map[string]any {
	next := make(map[string]any, len(baseArgs)+1)
	for k, v := range baseArgs {
		next[k] = v
	}
	next["cursor"] = cursor
	return next
}

// executeBatch runs multiple parallel steps concurrently using errgroup. If any
// step fails, the errgroup cancels the context for remaining steps. Results are
// collected via a buffered channel and stored in the execution context after all
// goroutines complete.
func (ce *CompositeExecutor) executeBatch(ctx context.Context, batch ExecutionBatch, client sempv2.Client, execCtx *ExecuteContext) error {
	g, gCtx := errgroup.WithContext(ctx)

	type stepResult struct {
		id   string
		data map[string]any
	}
	resultsChan := make(chan stepResult, len(batch.Steps))

	for _, step := range batch.Steps {
		safego.Go(g, func() error {
			data, err := ce.runStep(gCtx, step, client, execCtx)
			if err != nil {
				return err
			}
			resultsChan <- stepResult{id: step.ID, data: data}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}
	close(resultsChan)

	for result := range resultsChan {
		execCtx.StepResults[result.id] = result.data
	}

	return nil
}

// ResolveArgs resolves Go text/template expressions in step arguments against
// the execution context. Each arg value is parsed and executed as a template
// with access to .Params (input parameters) and .StepResults (prior step results).
// Template execution is wrapped in a recover to catch panics from nil map
// traversal (e.g., {{.StepResults.missing.data}}).
func ResolveArgs(args map[string]string, execCtx *ExecuteContext) (map[string]any, error) {
	resolved := make(map[string]any, len(args))

	for key, templateStr := range args {
		tmpl, err := template.New(key).Option("missingkey=error").Parse(templateStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse template for arg %s: %w", key, err)
		}

		result, err := safeTemplateExecute(tmpl, execCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to execute template for arg %s: %w", key, err)
		}

		resolved[key] = result
	}

	return resolved, nil
}

// safeTemplateExecute executes a template with a recover wrapper to catch panics
// from nil map traversal in template expressions. Without this, a template like
// {{.StepResults.missing.data}} would panic and crash the server.
func safeTemplateExecute(tmpl *template.Template, data any) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("template execution panicked: %v", r)
		}
	}()

	var buf bytes.Buffer
	err = tmpl.Execute(&buf, data)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// constructRequestBody assembles the request body for a write operation from the
// tool's input params. It only acts on operations that declare a body parameter;
// for all others it returns args unchanged.
//
// Object params (like msgVpnConfig) are spread into the body as individual
// fields; scalar params (like msgVpnName) are set directly. Params the operation
// takes from the path or query are not part of the body — they reach the broker
// through args instead. The assembled body is added to args under the "body" key.
//
// Two input mistakes are rejected rather than passed to the broker:
//
//   - A body field set by more than one source. On create, msgVpnName is a body
//     field, so a dedicated msgVpnName param and a msgVpnName key inside
//     msgVpnConfig both target it. With no defined precedence the winner would
//     depend on Go's randomized map iteration order, so we fail loudly instead
//     of writing an ambiguous body.
//
//   - A path/query param name placed inside the config object. On update,
//     msgVpnName is a path param, so a msgVpnName key inside msgVpnConfig would
//     otherwise leak into the body. Its value must come from the dedicated param
//     (it identifies the object in the URL), so we reject it here — giving the
//     same clear, client-side error as the create case above.
func (ce *CompositeExecutor) constructRequestBody(op *sempv2.Operation, args, params map[string]any) (map[string]any, error) {
	hasBody := false
	nonBodyParams := make(map[string]bool) // params the op takes from path/query/header, not the body
	for _, p := range op.Parameters {
		if p.In == "body" {
			hasBody = true
		} else {
			nonBodyParams[p.Name] = true
		}
	}
	if !hasBody {
		return args, nil
	}

	body := make(map[string]any)
	set := func(field string, val any) error {
		if _, defined := body[field]; defined {
			return fmt.Errorf("ambiguous request body: field %q is defined more than once; remove it from the config object", field)
		}
		body[field] = val
		return nil
	}
	for name, val := range params {
		if nonBodyParams[name] {
			continue
		}
		if obj, isObj := val.(map[string]any); isObj {
			for k, v := range obj {
				if nonBodyParams[k] {
					return nil, fmt.Errorf("invalid request body: %q is taken from the path or query and must not appear in the %q config object; supply it via the dedicated parameter instead", k, name)
				}
				if err := set(k, v); err != nil {
					return nil, err
				}
			}
		} else {
			if err := set(name, val); err != nil {
				return nil, err
			}
		}
	}

	// Reject any field the operation's body schema doesn't declare. This catches
	// a tool-only param (e.g. a dryRun flag) that would otherwise be spread into
	// the body and rejected by the broker with an opaque "unknown attribute" 400.
	// op.BodyFields is nil when the schema couldn't be introspected, in which
	// case we skip the check and defer to the broker as before.
	//
	// A field can be unknown for three reasons: a wrong/typo'd name, a tool-only
	// param that should be declared as path/query/header, or a genuinely new
	// attribute on a broker newer than the embedded schema. The caller gets a
	// terse error naming all three; operators get the schema version in a
	// structured log line for fleet-level correlation.
	if op.BodyFields != nil {
		for field := range body {
			if _, known := op.BodyFields[field]; !known {
				slog.Debug("rejecting unknown body field",
					"field", field,
					"operation", op.ID,
					"schemaVersion", op.SchemaVersion)
				return nil, fmt.Errorf("request body field %q is not a known attribute of operation %q; check the name, ensure tool-only params are declared as path/query/header, or try a newer MCP server", field, op.ID)
			}
		}
	}

	args["body"] = body
	return args, nil
}

// ApplyResultStrategy combines step results according to the tool's result
// strategy configuration. "collect" returns all step results keyed by step ID.
// "postProcess" runs a registered Go postprocessor over the step results and
// merges its summary map under a top-level "summary" key alongside the raw
// results.
func ApplyResultStrategy(strategy ResultStrategy, stepResults map[string]map[string]any) (map[string]any, error) {
	switch strategy.Strategy {
	case "collect":
		return collectSteps(stepResults, 0), nil
	case "postProcess":
		summary, err := postprocess.Apply(strategy.PostProcess, stepResults)
		if err != nil {
			return nil, err
		}
		out := collectSteps(stepResults, 1)
		out["summary"] = summary
		return out, nil
	default:
		return nil, fmt.Errorf("result strategy %q is not supported; supported values: collect, postProcess", strategy.Strategy)
	}
}

// collectSteps returns a new map keyed by step ID. extraCap reserves space
// for keys the caller will add (e.g. "summary").
func collectSteps(stepResults map[string]map[string]any, extraCap int) map[string]any {
	out := make(map[string]any, len(stepResults)+extraCap)
	for stepID, res := range stepResults {
		out[stepID] = res
	}
	return out
}
