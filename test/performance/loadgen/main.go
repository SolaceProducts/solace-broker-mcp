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

// Command loadgen drives concurrent MCP tool calls at a configured MCP
// server for a fixed duration, then reports throughput, error rate, and
// latency percentiles. Companion to the mock-semp and fidelity commands
// in test/performance/.
//
// Each -clients unit is a distinct MCP session so the load pattern mirrors
// realistic multi-agent usage (one session per agent), matching the client
// pattern used by test/e2e-basic-mcp/agent and test/performance/fidelity.
//
// Usage:
//
//	loadgen \
//	  -mcp-url  http://localhost:9090 \
//	  -brokers  my-broker \
//	  -clients  32 \
//	  -duration 60s \
//	  -tools    get-broker-status,list-queues
//
// Auth: reads MCP_DEV_TOKEN from env and sends it as Bearer if set.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	mcpURL := flag.String("mcp-url", "http://localhost:9090", "MCP server URL")
	brokersCSV := flag.String("brokers", "", "comma-separated broker aliases; each client is pinned to one, round-robin. Mutually exclusive with -broker-count.")
	brokerCount := flag.Int("broker-count", 0, "shortcut: generate <prefix>-01..<prefix>-N as the alias list. Zero means use -brokers instead.")
	brokerPrefix := flag.String("broker-prefix", "broker", "prefix used with -broker-count (e.g. -broker-count=50 => broker-01..broker-50)")
	vpn := flag.String("vpn", "default", "msgVpnName argument for tools that need it (e.g. list-queues)")
	clients := flag.Int("clients", 32, "number of concurrent MCP sessions")
	duration := flag.Duration("duration", 60*time.Second, "steady-state run duration")
	warmup := flag.Duration("warmup", 0, "duration excluded from stats before steady state; useful for skipping session warmup jitter")
	toolsCSV := flag.String("tools", "get-broker-status,list-queues", "comma-separated tool names; each client rotates through them")
	rps := flag.Float64("rps", 0, "per-client req/s cap; 0 = unlimited (client fires as fast as MCP responds). Mutually exclusive with -total-rps.")
	totalRPS := flag.Float64("total-rps", 0, "aggregate req/s across all clients; divided evenly so per-client = total-rps/clients. Mutually exclusive with -rps.")
	connectTimeout := flag.Duration("connect-timeout", 30*time.Second, "deadline for opening each MCP session before the run starts")
	flag.Parse()

	brokers, err := resolveBrokers(*brokersCSV, *brokerCount, *brokerPrefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}
	tools := splitCSV(*toolsCSV)
	if len(tools) == 0 || *clients < 1 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "loadgen: -tools, -clients, -duration are required and non-empty")
		os.Exit(2)
	}
	perClientRPS, err := resolveRPS(*rps, *totalRPS, *clients)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(2)
	}

	// Signal-cancelable ctx so Ctrl-C during a long run stops cleanly and still
	// prints whatever partial stats we've gathered.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := runConfig{
		mcpURL:         *mcpURL,
		brokers:        brokers,
		vpn:            *vpn,
		tools:          tools,
		clients:        *clients,
		duration:       *duration,
		warmup:         *warmup,
		rps:            perClientRPS,
		connectTimeout: *connectTimeout,
	}
	if err = run(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

type runConfig struct {
	mcpURL         string
	brokers        []string
	vpn            string
	tools          []string
	clients        int
	duration       time.Duration
	warmup         time.Duration
	rps            float64
	connectTimeout time.Duration
}

// sample is one recorded tool call. Zero-value err means success. Kept as a
// value type (not pointer) so per-client slices stay in one contiguous
// allocation — with tens of thousands of samples that meaningfully cuts GC
// pressure on the fast path.
type sample struct {
	latency time.Duration
	err     string // "" on success; short tag on failure (see classifyErr)
}

func run(ctx context.Context, cfg runConfig) error {
	fmt.Printf("loadgen: %d clients, %s duration (%s warmup), tools=%v, brokers=%v\n",
		cfg.clients, cfg.duration, cfg.warmup, cfg.tools, cfg.brokers)

	// One shared Transport across all sessions so connection pools aren't
	// fragmented per-client and the idle-conn table isn't allocated N times
	// at high -clients. Safe for concurrent use per net/http docs.
	transport := &http.Transport{
		MaxIdleConns:        4096,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
	}

	// Phase 1: dial all sessions before starting the clock. If MCP can't accept
	// N sessions, we want to fail loudly here rather than during timing.
	sessions, err := dialAll(ctx, cfg, transport)
	if err != nil {
		return err
	}
	defer func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}()
	fmt.Printf("connected %d sessions.\n", len(sessions))

	// Phase 2: fan out clients. Each pins to one broker (round-robin) and
	// rotates through the tool list on each call. Steady-state samples are
	// merged at the end; warmup samples are discarded per --warmup.
	var (
		totalCalls atomic.Int64
		totalErrs  atomic.Int64
	)
	startBarrier := make(chan struct{})
	perClient := make([][]sample, cfg.clients)
	var wg sync.WaitGroup

	for i := 0; i < cfg.clients; i++ {
		broker := cfg.brokers[i%len(cfg.brokers)]
		wg.Go(func() {
			perClient[i] = clientLoop(ctx, sessions[i], clientJob{
				id:       i,
				broker:   broker,
				vpn:      cfg.vpn,
				tools:    cfg.tools,
				warmup:   cfg.warmup,
				duration: cfg.duration,
				rps:      cfg.rps,
				start:    startBarrier,
				calls:    &totalCalls,
				errs:     &totalErrs,
			})
		})
	}

	// Phase 3: release the barrier and run a 1Hz progress ticker for the
	// operator watching stdout. The steady-state deadline is
	// warmup + duration; ctx cancellation (Ctrl-C) trumps that.
	runStart := time.Now()
	close(startBarrier)
	tickerDone := make(chan struct{})
	go progressTicker(ctx, runStart, cfg.warmup, cfg.duration, &totalCalls, &totalErrs, tickerDone)

	wg.Wait()
	close(tickerDone)

	// Phase 4: aggregate. Discard nils from any client that failed to record
	// samples (shouldn't happen but let's not panic on it).
	var all []sample
	for _, s := range perClient {
		all = append(all, s...)
	}
	if len(all) == 0 {
		return errors.New("no samples recorded — did the run terminate before warmup ended?")
	}
	summary := summarize(all, cfg.duration)
	summary.print(cfg)

	// No hard pass/fail on error rate. Fidelity is the correctness gate
	// (runs before the load, deep-equals tool output vs golden); loadgen
	// numbers are observations for the operator. A bar here would either
	// fire spuriously (Ctrl-C mid-run tags in-flight calls as canceled) or
	// contradict itself (ERROR_RATE injection guarantees a "failure").
	return nil
}

// clientJob is the immutable per-goroutine config; the mutable atomics for
// live counters are passed alongside.
type clientJob struct {
	id       int
	broker   string
	vpn      string
	tools    []string
	warmup   time.Duration
	duration time.Duration
	rps      float64
	start    <-chan struct{}
	calls    *atomic.Int64
	errs     *atomic.Int64
}

// clientLoop fires tool calls back-to-back for warmup+duration, recording
// steady-state samples only. Each call gets its own short context tied to
// the parent so a global cancel unblocks even an in-flight tool call.
func clientLoop(ctx context.Context, session *mcp.ClientSession, j clientJob) []sample {
	<-j.start
	loopStart := time.Now()
	warmupEnd := loopStart.Add(j.warmup)
	deadline := warmupEnd.Add(j.duration)

	// Pre-size the sample slice so the steady state isn't punctuated by
	// reallocs. Assume ~1ms per call as an optimistic upper bound on
	// cardinality, then cap it: the estimate scales with -duration but the
	// allocation is paid per client, so an uncapped 60s run at the -clients
	// 2000 this suite advertises would reserve ~2.9 GB before the first call
	// fires. Past the cap, append's doubling costs a handful of copies —
	// cheaper than an OOM in the process that is supposed to be measuring
	// someone else's memory.
	const maxPrealloc = 4096
	estimate := min(int(j.duration.Seconds()*1000)+64, maxPrealloc)
	out := make([]sample, 0, estimate)

	var (
		gap      time.Duration
		nextFire time.Time
	)
	if j.rps > 0 {
		gap = time.Duration(float64(time.Second) / j.rps)
		nextFire = loopStart
	}

	callIdx := 0
	for {
		if ctx.Err() != nil {
			return out
		}
		now := time.Now()
		if !now.Before(deadline) {
			return out
		}
		if j.rps > 0 {
			if now.Before(nextFire) {
				// Sleep on a timer so ctx cancellation unblocks us;
				// time.Sleep would ignore ctx.
				t := time.NewTimer(nextFire.Sub(now))
				select {
				case <-ctx.Done():
					t.Stop()
					return out
				case <-t.C:
				}
			}
			// Advance every iteration, even when the call took longer than
			// gap. Otherwise nextFire silently freezes in the past and
			// subsequent fast calls run unpaced. If we've fallen far behind,
			// snap nextFire forward to now so we don't dump a burst catching
			// up.
			nextFire = nextFire.Add(gap)
			if nextFire.Before(now) {
				nextFire = now.Add(gap)
			}
		}

		tool := j.tools[callIdx%len(j.tools)]
		callIdx++

		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		t0 := time.Now()
		err := callOnce(callCtx, session, tool, j.broker, j.vpn)
		latency := time.Since(t0)
		cancel()

		// If our parent ctx has been canceled (Ctrl-C / SIGTERM), any error
		// from this call is almost certainly our own doing — dropping the
		// tool call because we're shutting down. Recording it would inflate
		// the error rate for a reason that has nothing to do with the system
		// under test. Return without recording; the next-iteration ctx.Err()
		// check would exit anyway. Deadlines from callCtx's own 30s timeout
		// still get recorded (parent ctx.Err() is nil in that case).
		if ctx.Err() != nil {
			return out
		}

		// Discard samples during warmup, but still count them in the live
		// counters so the progress ticker's early rate is meaningful.
		if t0.Before(warmupEnd) {
			j.calls.Add(1)
			if err != nil {
				j.errs.Add(1)
			}
			continue
		}
		out = append(out, sample{latency: latency, err: classifyErr(err)})
		j.calls.Add(1)
		if err != nil {
			j.errs.Add(1)
		}
	}
}

// callOnce invokes one tool and reports the first error encountered. It
// intentionally does not read/parse the tool result body — this is a load
// test, not a fidelity check; that's what fidelity/main.go is for. Tool
// error responses (IsError=true) surface as errors in the reported error
// rate so the operator can see broker unavailability, timeouts, MCP-side
// rate limits, etc. — but the number is observational, not a gate.
func callOnce(ctx context.Context, session *mcp.ClientSession, tool, broker, vpn string) error {
	args := map[string]any{"broker": broker}
	if tool == "list-queues" {
		args["msgVpnName"] = vpn
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New("tool returned IsError")
	}
	return nil
}

// classifyErr collapses errors into a small set of tags so the summary can
// break down failures without dumping raw messages. Short-circuit on the
// most common shapes; anything else falls through to "other".
func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "context deadline"):
		return "timeout"
	case strings.Contains(msg, "context canceled"):
		return "canceled"
	case strings.Contains(msg, "connection refused"):
		return "conn-refused"
	case strings.Contains(msg, "IsError"):
		return "tool-error"
	case strings.Contains(msg, "EOF") || strings.Contains(msg, "connection reset"):
		return "conn-reset"
	default:
		return "other"
	}
}

// dialAll opens `clients` MCP sessions in parallel. If any fail we close the
// ones that succeeded and return — no point starting a partial run.
func dialAll(ctx context.Context, cfg runConfig, transport http.RoundTripper) ([]*mcp.ClientSession, error) {
	dialCtx, cancel := context.WithTimeout(ctx, cfg.connectTimeout)
	defer cancel()

	out := make([]*mcp.ClientSession, cfg.clients)
	errs := make([]error, cfg.clients)
	var wg sync.WaitGroup
	for i := 0; i < cfg.clients; i++ {
		wg.Go(func() {
			s, err := connect(dialCtx, cfg.mcpURL, i, transport)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = s
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			for _, s := range out {
				if s != nil {
					_ = s.Close()
				}
			}
			return nil, fmt.Errorf("client %d: connect: %w", i, err)
		}
	}
	return out, nil
}

// progressTicker prints a one-line-per-second status while the run is in
// progress. Counters are read atomically so the printout doesn't need to
// coordinate with client goroutines.
func progressTicker(ctx context.Context, start time.Time, warmup, duration time.Duration, calls, errs *atomic.Int64, done <-chan struct{}) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	warmupEnd := start.Add(warmup)
	deadline := warmupEnd.Add(duration)
	var lastCalls, lastErrs int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case now := <-t.C:
			c := calls.Load()
			e := errs.Load()
			dc := c - lastCalls
			de := e - lastErrs
			lastCalls, lastErrs = c, e
			phase := "steady"
			if now.Before(warmupEnd) {
				phase = "warmup"
			} else if !now.Before(deadline) {
				phase = "drain "
			}
			fmt.Printf("[t+%4.0fs] %s calls/s=%5d errs/s=%3d total=%d errs=%d\n",
				now.Sub(start).Seconds(), phase, dc, de, c, e)
		}
	}
}

// summaryStats holds everything the final print block needs. Broken out so
// print stays a pure formatter.
type summaryStats struct {
	total     int
	errs      int
	errorRate float64
	rps       float64
	p50       time.Duration
	p95       time.Duration
	p99       time.Duration
	pMax      time.Duration
	errBreak  map[string]int
}

func summarize(samples []sample, duration time.Duration) summaryStats {
	// Errors don't contribute to latency percentiles — a fast failure would
	// otherwise pull p50 down and paint an optimistic picture. Still counted
	// in the reported error rate so the operator can see them; just kept out
	// of the latency histogram.
	lat := make([]time.Duration, 0, len(samples))
	errs := 0
	errBreak := make(map[string]int)
	for _, s := range samples {
		if s.err != "" {
			errs++
			errBreak[s.err]++
			continue
		}
		lat = append(lat, s.latency)
	}
	slices.Sort(lat)

	s := summaryStats{
		total:    len(samples),
		errs:     errs,
		errBreak: errBreak,
	}
	if len(samples) > 0 {
		s.errorRate = float64(errs) / float64(len(samples))
	}
	if duration > 0 {
		s.rps = float64(len(samples)) / duration.Seconds()
	}
	if n := len(lat); n > 0 {
		s.p50 = lat[pctIdx(n, 0.50)]
		s.p95 = lat[pctIdx(n, 0.95)]
		s.p99 = lat[pctIdx(n, 0.99)]
		s.pMax = lat[n-1]
	}
	return s
}

// pctIdx returns the index into a sorted slice of length n for percentile p
// (0.0..1.0). Uses nearest-rank (ceil(p*n) - 1) so p95 of 20 samples is
// samples[18], the classic pick for load-test reporting.
func pctIdx(n int, p float64) int {
	if n == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(n))) - 1
	return min(max(idx, 0), n-1)
}

func (s summaryStats) print(cfg runConfig) {
	fmt.Println()
	fmt.Println("========================= loadgen summary =========================")
	fmt.Println("Run")
	fmt.Printf("  clients   : %d\n", cfg.clients)
	fmt.Printf("  duration  : %s (warmup %s excluded)\n", cfg.duration, cfg.warmup)
	fmt.Printf("  brokers   : %v\n", cfg.brokers)
	fmt.Printf("  tools     : %v\n", cfg.tools)
	fmt.Println()
	fmt.Println("Throughput")
	fmt.Printf("  requests  : %d\n", s.total)
	fmt.Printf("  rate      : %.1f req/s\n", s.rps)
	fmt.Printf("  errors    : %d (%.3f%%)\n", s.errs, s.errorRate*100)
	if len(s.errBreak) > 0 {
		keys := make([]string, 0, len(s.errBreak))
		for k := range s.errBreak {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("              %-14s %d\n", k, s.errBreak[k])
		}
	}
	fmt.Println()
	fmt.Println("Latency (round-trip time per request)")
	fmt.Printf("  p50 (typical)     : %s   -- half of requests finished under this\n", fmtLatency(s.p50))
	fmt.Printf("  p95               : %s   -- 95%% of requests finished under this\n", fmtLatency(s.p95))
	fmt.Printf("  p99 (tail)        : %s   -- 99%% of requests finished under this\n", fmtLatency(s.p99))
	fmt.Printf("  max (worst)       : %s   -- single slowest request\n", fmtLatency(s.pMax))
	fmt.Println()
	fmt.Println("Note: all numbers above are observations, not pass/fail bars.")
	fmt.Println("Correctness is gated up front by the fidelity check; latency,")
	fmt.Println("throughput, and error rate are for the operator to interpret.")
	fmt.Println("===================================================================")
}

// fmtLatency prints a duration with a unit chosen for readability: ms for
// anything in the millisecond range (which is basically everything under
// load), us for sub-millisecond, s for anything painfully large. Fixed
// width so the "-- half of requests..." column lines up.
func fmtLatency(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%7.2f s ", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%7.1f ms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%7d us", d.Microseconds())
	}
}

// resolveBrokers chooses between the explicit CSV and the count-shortcut,
// forbidding both to prevent silent divergence between "what the user typed"
// and "what got used". Names are zero-padded to two digits to match the
// checked-in broker-config.mock.yaml (broker-01..broker-50).
func resolveBrokers(csv string, count int, prefix string) ([]string, error) {
	explicit := splitCSV(csv)
	switch {
	case count > 0 && len(explicit) > 0:
		return nil, errors.New("-brokers and -broker-count are mutually exclusive")
	case count > 0:
		out := make([]string, count)
		for i := range count {
			out[i] = fmt.Sprintf("%s-%02d", prefix, i+1)
		}
		return out, nil
	case len(explicit) > 0:
		return explicit, nil
	default:
		return nil, errors.New("provide -brokers <csv> or -broker-count <N>")
	}
}

// resolveRPS picks between per-client -rps and aggregate -total-rps, forbidding
// both so intent stays unambiguous. When -total-rps is set, the aggregate is
// divided evenly across clients — matching the "N clients × per-client rps"
// mental model without making the caller do the math.
func resolveRPS(perClient, total float64, clients int) (float64, error) {
	if perClient < 0 || total < 0 {
		return 0, errors.New("-rps and -total-rps must be non-negative")
	}
	if perClient > 0 && total > 0 {
		return 0, errors.New("-rps and -total-rps are mutually exclusive")
	}
	if total > 0 {
		return total / float64(clients), nil
	}
	return perClient, nil
}

// splitCSV parses "a,b,c" into ["a","b","c"], dropping empties and trimming
// whitespace so a user-typed " a, b " doesn't produce phantom aliases.
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// connect opens one MCP session over the streamable HTTP transport. Mirrors
// test/performance/fidelity so both performance tools consume MCP the same way. The
// clientIdx is only used to disambiguate the session name in server logs.
func connect(ctx context.Context, url string, clientIdx int, transport http.RoundTripper) (*mcp.ClientSession, error) {
	// Shared transport across all sessions — caller sets up pool sizing
	// (MaxIdleConnsPerHost, etc.). Over real TCP the stdlib default of 2
	// idle conns per host exhausts the ephemeral port range in ~30s at 200+
	// clients as sockets pile up in TIME_WAIT.
	httpClient := &http.Client{Transport: transport}
	if tok := os.Getenv("MCP_DEV_TOKEN"); tok != "" {
		httpClient.Transport = &bearer{token: tok, next: transport}
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    fmt.Sprintf("loadgen-%d", clientIdx),
		Version: "0.1",
	}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: httpClient,
	}, nil)
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	c := req.Clone(req.Context())
	c.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(c)
}
