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

// Command semp-tap is a recording reverse proxy placed between the MCP server
// and a real Solace broker, so an e2e test can measure the SEMP request rate
// and in-flight concurrency the broker actually sees.
//
// The broker stays real: the tap forwards every request upstream unmodified and
// returns the broker's own response. It only observes. Point a broker alias's
// url at the tap and leave the rest of the suite pointed straight at the broker,
// and the record contains exactly the traffic that alias generated.
//
// # Why not just time the tool calls
//
// One MCP tool call is not one SEMP request: a composite tool can fan out to
// several, pagination adds more, and retries add attempts the client never sees.
// Client-side timing therefore cannot say what rate the broker experienced. The
// tap can, because it sits where the broker does.
//
// # The measurement window — this is the subtle part
//
// resilience.Sender releases its per-broker in-flight semaphore slot when Do
// returns, and Do returns as soon as the response HEADERS are available; the
// body is handed back to the caller still open (see the `defer func() { <-d.sem
// }()` in internal/semp/resilience/sender.go). So a request stops occupying a
// slot at header time, not when its body finishes streaming.
//
// The tap must therefore measure in-flight over
//
//	[request received  →  response headers handed back downstream]
//
// and NOT over the full body-proxying duration. Measuring the wider window
// would report a correct cap of 2 as an occasional 3, because the server is
// entitled to admit a new request while a previous body is still draining.
// ModifyResponse fires at exactly the right moment, which is why the end
// timestamp is taken there rather than after ServeHTTP returns.
//
// With the end timestamp taken there, the error can only run one way. The
// server's Do does not return until it has read the response headers off the
// wire, which is strictly after the tap stamped its end time and handed those
// headers downstream. The semaphore slot is therefore held over a superset of
// the window the tap measures, so the tap can only ever UNDER-count overlap
// relative to the semaphore, never over-count it. A cap assertion built on this
// record cannot produce a false violation; the worst it can do is miss one,
// which is what the control phase exists to rule out.
//
// For the same reason the tap counts REQUESTS, never connections.
// semp.max_concurrent_per_broker also sizes the HTTP transport's
// MaxConnsPerHost per protocol client (internal/semp/resilience/transport.go),
// so up to 2x the cap in TCP connections can legitimately exist against one
// broker. Connections are not the enforced bound; the shared semaphore is.
//
// # -delay
//
// With no delay, whether N concurrent requests genuinely overlap depends on how
// fast the broker answers, which makes a concurrency assertion luck-dependent on
// a fast runner. -delay holds each response for a fixed period after the broker
// has answered and before the headers go back downstream. That widens the
// semaphore hold and the measured window by the same amount, so overlap becomes
// arithmetic rather than luck. The broker still does the real work; only the
// measurement window is stretched. Apply the same delay to a phase and its
// control so the comparison stays honest.
//
// # Record format
//
// One CSV line per request, no header row, appended in completion order:
//
//	seq,start_unix_nanos,end_unix_nanos,status,method,path
//
// start is when the tap received the request; end is stamped inside
// ModifyResponse: after any -delay, once the upstream headers are available, and
// before ReverseProxy writes them downstream. The record line is written and
// fsynced in between, so end always sits slightly before the downstream header
// write, never after. A request whose upstream call failed is recorded with
// status 0 so a run that silently errored cannot masquerade as a well-paced one.
//
// Usage:
//
//	semp-tap -listen :8084 -upstream http://localhost:8080 -record /path/rec.csv [-delay 150ms]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// startKey carries a request's arrival timestamp from the Director to
// ModifyResponse. A context value is the only channel the two share, since
// ReverseProxy gives ModifyResponse the *http.Response and nothing else.
type startKey struct{}

// userinfoPattern matches the "user:pass@" segment of a URL. httputil's
// ReverseProxy hands ErrorHandler the raw dial/roundtrip error, and a
// *url.Error prints its URL verbatim — this module is stdlib-only (own
// go.mod) and can't reach internal/config's SanitizeURLString, so errors
// get their own narrow scrub before they reach the log.
var userinfoPattern = regexp.MustCompile(`://[^/@\s]*@`)

// sanitizeErrForLog strips embedded userinfo from an error's message before
// logging. The tap points at test brokers today, but the upstream URL is
// operator-supplied (-upstream), so nothing here should assume it never
// carries credentials.
func sanitizeErrForLog(err error) string {
	return userinfoPattern.ReplaceAllString(err.Error(), "://")
}

// recorder serialises record lines to the record file. Every line is written
// and flushed as it completes, so a run that is killed mid-phase still leaves a
// usable partial record for diagnosis rather than an empty file.
type recorder struct {
	mu  sync.Mutex
	f   *os.File
	seq atomic.Int64
}

func newRecorder(path string) (*recorder, error) {
	// Truncate: each phase gets its own file and must not inherit a previous
	// run's arrivals, which would corrupt every gap and overlap computation.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open record file: %w", err)
	}
	return &recorder{f: f}, nil
}

func (r *recorder) record(start, end time.Time, status int, method, path string) {
	// A zero start means the arrival stamp did not survive to here. Most
	// ErrorHandler call sites in net/http/httputil hand back the INBOUND request
	// rather than the one the Director stamped, so this is reachable on some
	// error paths. Left alone, time.Time{}.UnixNano() is a large negative number
	// that would sort ahead of every real arrival and poison every gap and
	// overlap computation for the phase. Collapse it to a zero-duration event at
	// the right point in time instead, and say so.
	if start.IsZero() {
		log.Printf("semp-tap: no arrival timestamp for %s %s; recording it as zero-duration", method, path)
		start = end
	}
	n := r.seq.Add(1)
	line := fmt.Sprintf("%d,%d,%d,%d,%s,%s\n", n, start.UnixNano(), end.UnixNano(), status, method, path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.f.WriteString(line); err != nil {
		log.Printf("semp-tap: write record: %v", err)
		return
	}
	// Sync rather than rely on process exit: the analysing shell reads this file
	// immediately after the tap is signalled, and an unflushed tail would show up
	// as missing arrivals — which reads as a passing pacer test.
	if err := r.f.Sync(); err != nil {
		log.Printf("semp-tap: sync record: %v", err)
	}
}

func (r *recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}

func main() {
	listen := flag.String("listen", "", "address to listen on, e.g. :8084 (required)")
	upstream := flag.String("upstream", "", "real broker base URL to forward to, e.g. http://localhost:8080 (required)")
	record := flag.String("record", "", "path to write the CSV request record to (required)")
	delay := flag.Duration("delay", 0, "hold each response for this long after the broker answers and before returning headers downstream; widens the semaphore hold and the measured window identically so concurrency is arithmetic rather than luck")
	ready := flag.String("ready-file", "", "optional path to write the listening address to once the socket is bound; the harness polls for it instead of sleeping")
	flag.Parse()

	if *listen == "" || *upstream == "" || *record == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *delay < 0 {
		log.Fatalf("semp-tap: -delay must be >= 0, got %s", *delay)
	}

	// The suite always passes a credential-free broker URL, but nothing stops a
	// future caller passing http://user:pass@host. Neither the raw flag value
	// nor url.Parse's error (which embeds the URL it was given) is safe to log,
	// and URL.String() prints userinfo in the clear — hence Redacted() at the
	// one place the upstream is echoed, and no URL at all in these two errors.
	// Rule C-05 in docs/internal/secure-logging-rules.md.
	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal("semp-tap: -upstream is not a valid URL")
	}
	if target.Scheme == "" || target.Host == "" {
		log.Fatal("semp-tap: -upstream must be an absolute URL with scheme and host")
	}

	rec, err := newRecorder(*record)
	if err != nil {
		log.Fatalf("semp-tap: %v", err)
	}
	defer func() { _ = rec.Close() }()

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Stamp the arrival time and rewrite Host. NewSingleHostReverseProxy sets
	// URL.Scheme/Host but leaves req.Host as the tap's own address; forwarding
	// the upstream's Host keeps the broker seeing what it would see directly.
	inner := proxy.Director
	proxy.Director = func(req *http.Request) {
		inner(req)
		req.Host = target.Host
		*req = *req.WithContext(context.WithValue(req.Context(), startKey{}, time.Now()))
	}

	// End of the measurement window. Fires once the upstream response headers are
	// available and before they are written downstream, which is strictly before
	// the MCP server's Sender.Do returns and drops its semaphore slot (see "The
	// measurement window" above).
	proxy.ModifyResponse = func(resp *http.Response) error {
		if *delay > 0 {
			select {
			case <-time.After(*delay):
			case <-resp.Request.Context().Done():
			}
		}
		start, _ := resp.Request.Context().Value(startKey{}).(time.Time)
		rec.record(start, time.Now(), resp.StatusCode, resp.Request.Method, resp.Request.URL.Path)
		return nil
	}

	// An upstream failure must still land in the record. Without this the
	// request simply vanishes, and a phase where every call errored would show
	// zero arrivals — which every "gap >= interval" assertion passes vacuously.
	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		start, _ := req.Context().Value(startKey{}).(time.Time)
		rec.record(start, time.Now(), 0, req.Method, req.URL.Path)
		log.Printf("semp-tap: upstream error for %s %s: %s", req.Method, req.URL.Path, sanitizeErrForLog(err))
		w.WriteHeader(http.StatusBadGateway)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("semp-tap: listen on %s: %v", *listen, err)
	}

	srv := &http.Server{
		Handler: proxy,
		// No ReadHeaderTimeout would trip gosec G112. The suite's requests are
		// local and immediate, so a short bound is safe and keeps a wedged peer
		// from pinning a goroutine for the whole phase.
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Readiness is the bound socket, not a sleep: the harness starts the MCP
	// server as soon as this file appears, and a race there shows up as a
	// connection-refused flake in the first tool call of a phase.
	if *ready != "" {
		if err := os.WriteFile(*ready, []byte(ln.Addr().String()+"\n"), 0o600); err != nil {
			log.Fatalf("semp-tap: write -ready-file: %v", err)
		}
	}
	log.Printf("semp-tap: listening on %s, forwarding to %s, recording to %s (delay=%s)",
		ln.Addr(), target.Redacted(), *record, *delay)

	// SIGTERM is how the harness stops a phase. Shut down gracefully so an
	// in-flight request still gets its record line written.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("semp-tap: serve: %v", err)
	}

	// Serve returns the instant Shutdown is CALLED, not when it finishes.
	// Returning here would end main, run the deferred file close, and kill every
	// in-flight handler before it could write its record line — so a request
	// still at the broker when the harness stopped the tap would vanish from the
	// sample and show up as an unexplained short record. Wait for the drain.
	<-drained
}
