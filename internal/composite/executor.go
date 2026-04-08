package composite

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"golang.org/x/sync/errgroup"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// ExecuteContext holds state during composite tool execution. Each tool
// invocation gets its own context — not shared across calls. Params holds
// the input parameters (with the broker param already stripped). StepResults
// accumulates results from each completed step, keyed by step ID.
type ExecuteContext struct {
	Params      map[string]any
	StepResults map[string]map[string]any
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
}

// NewCompositeExecutor creates an executor with the given operation catalog.
// The operations map is keyed by prefixed operation ID (e.g.,
// "monitor/getMsgVpnQueue") matching the format produced by sempv2.ParseSpecs().
func NewCompositeExecutor(operations map[string]*sempv2.Operation) *CompositeExecutor {
	return &CompositeExecutor{operations: operations}
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
// operation, calls the client, and stores the result in the execution context.
func (ce *CompositeExecutor) executeStep(ctx context.Context, step Step, client sempv2.Client, execCtx *ExecuteContext) error {
	args, err := ResolveArgs(step.Args, execCtx)
	if err != nil {
		return fmt.Errorf("tool step %s: failed to resolve args: %w", step.ID, err)
	}

	op, ok := ce.operations[step.Operation]
	if !ok {
		return fmt.Errorf("tool step %s: operation %q not found in operation catalog", step.ID, step.Operation)
	}

	result, err := client.Execute(ctx, op, args)
	if err != nil {
		return fmt.Errorf("tool step %s: %w", step.ID, err)
	}

	execCtx.StepResults[step.ID] = result.Data
	return nil
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
		step := step // capture loop variable
		g.Go(func() error {
			args, err := ResolveArgs(step.Args, execCtx)
			if err != nil {
				return fmt.Errorf("tool step %s: failed to resolve args: %w", step.ID, err)
			}

			op, ok := ce.operations[step.Operation]
			if !ok {
				return fmt.Errorf("tool step %s: operation %q not found in operation catalog", step.ID, step.Operation)
			}

			result, err := client.Execute(gCtx, op, args)
			if err != nil {
				return fmt.Errorf("tool step %s: %w", step.ID, err)
			}

			resultsChan <- stepResult{id: step.ID, data: result.Data}
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

// ApplyResultStrategy combines step results according to the tool's result
// strategy configuration. Currently only "collect" is supported. Additional
// strategies (merge, unwrap) are deferred pending design discussion around
// SEMP response envelope overlap and per-step data transformation needs.
func ApplyResultStrategy(strategy ResultStrategy, stepResults map[string]map[string]any) (map[string]any, error) {
	switch strategy.Strategy {
	case "collect":
		return collectStrategy(stepResults)
	default:
		return nil, fmt.Errorf("result strategy %q is not supported; only collect is currently supported", strategy.Strategy)
	}
}

// collectStrategy returns all step results keyed by step ID.
func collectStrategy(stepResults map[string]map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(stepResults))
	for stepID, res := range stepResults {
		result[stepID] = res
	}
	return result, nil
}
