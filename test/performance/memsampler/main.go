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

// Command memsampler polls /proc/<pid>/status and /proc/<pid>/fd at a fixed
// interval and writes a CSV row per sample. Meant to run alongside test/performance/loadgen
// so a plot of RSS vs. wall-clock during a load run reveals whether MCP's
// footprint climbs (leak) or holds steady (per plan step 8's <10% drift
// over 30s bar).
//
// The plan calls for HeapAlloc / Sys from Go's runtime alongside RSS, but
// MCP does not expose pprof/expvar today. RSS + VmSize + thread count +
// open-descriptor count from /proc is what we can get without touching
// production code. Add a Go-heap column here if MCP later exposes
// /debug/vars or /debug/pprof.
//
// Descriptors are sampled here rather than scraped off /metrics, and the
// reason is not convenience. Collecting the process collector's fd gauge
// requires running with OBS_METRICS_ENABLED on, which changes the thing under
// test — a histogram observation per tool invocation plus a scrape listener —
// and makes the numbers non-comparable with every run measured so far, all of
// which had it off. Reading /proc costs nothing and perturbs nothing. What it
// does not give is the goroutine count and GC internals; the soak's pass
// criteria (RSS drift, threads flat, descriptors flat) do not need them, and
// GODEBUG=gctrace=1 answers "was that memory collectable garbage?" for free.
//
// Usage:
//
//	memsampler -pid $(pgrep -f mcp-server) -interval 1s -out mem.csv -duration 90s
//
// Runs until -duration elapses, the process disappears, or SIGINT.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	pid := flag.Int("pid", 0, "PID to sample (required)")
	interval := flag.Duration("interval", 1*time.Second, "poll interval")
	duration := flag.Duration("duration", 0, "total run duration; 0 = until Ctrl-C or the process disappears")
	out := flag.String("out", "mem.csv", "output CSV path (- for stdout)")
	quiet := flag.Bool("quiet", false, "suppress the summary printed at the end")
	flag.Parse()

	if *pid <= 0 {
		fmt.Fprintln(os.Stderr, "memsampler: -pid is required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *pid, *interval, *duration, *out, *quiet); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

// csvHeader is the CSV's column contract. lib.sh's fd/thread peak reader and
// summary.sh both index these columns positionally, so a new column goes on
// the end and never in the middle.
var csvHeader = []string{"t_sec", "wall_ts", "rss_kb", "vm_kb", "threads", "open_fds"}

func run(ctx context.Context, pid int, interval, duration time.Duration, outPath string, quiet bool) error {
	// Open the CSV sink first so we fail fast on a bad path before starting
	// the ticker. stdout support keeps ad-hoc invocation cheap.
	w, closeFn, err := openCSV(outPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := w.Write(csvHeader); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}
	w.Flush()

	// One initial sample before the ticker fires so a short run always has
	// at least one row on disk, and so the operator sees the process is
	// actually being read.
	start := time.Now()
	first, err := sampleProc(pid)
	if err != nil {
		return fmt.Errorf("first sample: %w", err)
	}
	writeRow(w, 0, start, first)
	w.Flush()

	var deadline time.Time
	if duration > 0 {
		deadline = start.Add(duration)
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	st := newStats(first)

	for {
		select {
		case <-ctx.Done():
			return finish(w, start, st, quiet, "context canceled")
		case now := <-t.C:
			if duration > 0 && !now.Before(deadline) {
				return finish(w, start, st, quiet, "duration elapsed")
			}
			s, err := sampleProc(pid)
			if err != nil {
				// Distinguish "process gone" from a transient parse error;
				// the former is a clean end-of-run, the latter shouldn't
				// swallow silently.
				if errors.Is(err, os.ErrNotExist) {
					return finish(w, start, st, quiet, "process exited")
				}
				fmt.Fprintf(os.Stderr, "memsampler: sample error at t+%s: %v\n", now.Sub(start).Round(time.Millisecond), err)
				continue
			}
			writeRow(w, now.Sub(start), now, s)
			w.Flush()
			st.observe(s)
		}
	}
}

// stats is the running roll-up the end-of-run summary prints. Peaks matter as
// much as the drift figure: a descriptor count that touched its ceiling
// mid-run is invisible in a start-vs-end comparison, and that is exactly the
// failure the >50-broker and concurrency-cap runs are looking for.
type stats struct {
	samples     int
	first, last procSample
	rssMin      int
	rssMax      int
	threadsMax  int
	fdMax       int // fdUnavailable until a readable sample arrives
}

func newStats(first procSample) *stats {
	return &stats{
		samples:    1,
		first:      first,
		last:       first,
		rssMin:     first.rssKB,
		rssMax:     first.rssKB,
		threadsMax: first.threads,
		fdMax:      first.openFDs,
	}
}

func (st *stats) observe(s procSample) {
	st.samples++
	st.last = s
	if s.rssKB < st.rssMin {
		st.rssMin = s.rssKB
	}
	if s.rssKB > st.rssMax {
		st.rssMax = s.rssKB
	}
	if s.threads > st.threadsMax {
		st.threadsMax = s.threads
	}
	if s.openFDs > st.fdMax {
		st.fdMax = s.openFDs
	}
}

// procSample is what one poll of /proc/<pid> extracts. Kept small on purpose —
// anything beyond RSS/VmSize/Threads/open descriptors should be added
// deliberately with a matching CSV column.
//
// openFDs is fdUnavailable when the descriptor directory could not be read
// (another user's process, or the process exiting between the two reads).
// A sentinel rather than 0: "no descriptors" and "we could not look" are
// different facts, and a 0 in a run record would read as the former.
type procSample struct {
	rssKB   int
	vmKB    int
	threads int
	openFDs int
}

// fdUnavailable marks a sample whose descriptor count could not be read.
// Written to the CSV as "NA", the same token sampler.sh already uses.
const fdUnavailable = -1

// sampleProc reads one sample: the status fields, then the descriptor count.
// Not using gopsutil to keep this tool dependency-free.
//
// A failure to read the descriptor directory is not fatal to the sample. The
// status read is what proves the process is alive, so an fd read that races a
// process exit degrades that one row to NA and lets the next poll report the
// exit properly, instead of dropping a row of memory data we did have.
func sampleProc(pid int) (procSample, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return procSample{}, err
	}
	s, err := parseStatus(string(data))
	if err != nil {
		return procSample{}, fmt.Errorf("%s: %w", path, err)
	}
	s.openFDs, err = countOpenFDs(pid)
	if err != nil {
		s.openFDs = fdUnavailable
		warnFDOnce(pid, err)
	}
	return s, nil
}

// parseStatus pulls the fields we record out of /proc/<pid>/status content.
// Split from the file read so it can be tested without a live process — the
// format is stable but the parse is where a silent zero would come from.
func parseStatus(data string) (procSample, error) {
	var s procSample
	s.openFDs = fdUnavailable
	// A field that is present but will not parse is an error, not a zero. The
	// both-fields-bad case would be caught by the guard below, but a malformed
	// VmRSS beside a valid VmSize used to record rss_kb=0 and carry on —
	// writing a plausible number into a CSV a later comparison trusts, which
	// is the exact silent zero splitting this parse out was meant to prevent.
	for line := range strings.SplitSeq(data, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		var err error
		switch key {
		case "VmRSS":
			if s.rssKB, err = parseKB(val); err != nil {
				return procSample{}, fmt.Errorf("parsing VmRSS %q: %w", val, err)
			}
		case "VmSize":
			if s.vmKB, err = parseKB(val); err != nil {
				return procSample{}, fmt.Errorf("parsing VmSize %q: %w", val, err)
			}
		case "Threads":
			if s.threads, err = strconv.Atoi(val); err != nil {
				return procSample{}, fmt.Errorf("parsing Threads %q: %w", val, err)
			}
		}
	}
	if s.rssKB == 0 && s.vmKB == 0 {
		return procSample{}, errors.New("no VmRSS/VmSize")
	}
	return s, nil
}

// countOpenFDs counts the entries in /proc/<pid>/fd.
//
// Readdirnames rather than os.ReadDir: it needs no stat per entry, so a
// descriptor closing mid-read drops out of the listing instead of producing an
// error. Partial results are still counted — a count one short of the truth is
// a better answer than none, at a sampling interval of one second.
//
// The open directory handle holds a descriptor of its own, and it is visible in
// the listing only when we are sampling ourselves. Subtract it in exactly that
// case rather than unconditionally, which would under-report every real run
// by one.
func countOpenFDs(pid int) (int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return fdUnavailable, err
	}
	defer f.Close()

	names, err := f.Readdirnames(-1)
	if err != nil && len(names) == 0 {
		// Nothing read at all. Report it as unavailable *with* the error, so
		// the caller warns. Applying the self-handle correction first would
		// turn a zero-name read of our own /proc/self/fd into (-1, nil) —
		// the unavailable sentinel arrived at by arithmetic accident, with no
		// error to explain it, which is the one diagnostic that would tell an
		// operator why a whole fd column reads NA.
		return fdUnavailable, err
	}
	n := len(names)
	if pid == os.Getpid() {
		n--
	}
	return n, nil
}

// warnFDOnce reports an unreadable descriptor directory a single time. A
// permission problem repeats on every poll, and a warning per second would
// bury the summary the operator is actually reading.
func warnFDOnce(pid int, err error) {
	fdWarnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "memsampler: cannot read /proc/%d/fd (%v); open_fds recorded as NA\n", pid, err)
	})
}

var fdWarnOnce sync.Once

// fdField renders openFDs for the CSV, as NA when it was not readable.
func fdField(n int) string {
	if n == fdUnavailable {
		return "NA"
	}
	return strconv.Itoa(n)
}

// parseKB pulls the integer off strings like "12345 kB". /proc/<pid>/status
// always uses kB for memory fields on Linux; assuming that avoids a
// dependency on unit-string parsing.
func parseKB(s string) (int, error) {
	fields := strings.Fields(s)
	if len(fields) < 1 {
		return 0, fmt.Errorf("empty value")
	}
	return strconv.Atoi(fields[0])
}

func writeRow(w *csv.Writer, elapsed time.Duration, wall time.Time, s procSample) {
	_ = w.Write([]string{
		strconv.FormatFloat(elapsed.Seconds(), 'f', 3, 64),
		wall.Format(time.RFC3339Nano),
		strconv.Itoa(s.rssKB),
		strconv.Itoa(s.vmKB),
		strconv.Itoa(s.threads),
		fdField(s.openFDs),
	})
}

func openCSV(path string) (*csv.Writer, func(), error) {
	if path == "-" {
		return csv.NewWriter(os.Stdout), func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", path, err)
	}
	w := csv.NewWriter(f)
	return w, func() {
		w.Flush()
		_ = f.Close()
	}, nil
}

func finish(w *csv.Writer, start time.Time, st *stats, quiet bool, reason string) error {
	w.Flush()
	if quiet {
		return nil
	}
	samples, rssMin, rssMax := st.samples, st.rssMin, st.rssMax
	first, last := st.first, st.last

	// Drift vs the first sample is what the plan's <10% steady-state bar
	// actually measures. Report both min/max span and end-vs-start delta;
	// they answer different questions (transient spike vs monotonic growth).
	elapsed := time.Since(start)
	fmt.Println()
	fmt.Println("=== memsampler summary ===")
	fmt.Printf("  reason:    %s\n", reason)
	fmt.Printf("  duration:  %s (%d samples)\n", elapsed.Round(time.Millisecond), samples)
	fmt.Printf("  RSS start: %d kB\n", first.rssKB)
	fmt.Printf("  RSS end:   %d kB\n", last.rssKB)
	fmt.Printf("  RSS min:   %d kB\n", rssMin)
	fmt.Printf("  RSS max:   %d kB\n", rssMax)
	if first.rssKB > 0 {
		endDrift := 100 * float64(last.rssKB-first.rssKB) / float64(first.rssKB)
		spanDrift := 100 * float64(rssMax-rssMin) / float64(first.rssKB)
		fmt.Printf("  RSS drift: end vs start %+.2f%%, span %.2f%%\n", endDrift, spanDrift)
		// The plan's steady-state bar is <10% RSS drift; call it out so a
		// human eyeballing the log doesn't have to compute.
		if endDrift > 10 || endDrift < -10 {
			fmt.Println("  verdict:   FAIL (>=10% RSS drift end-vs-start)")
		} else {
			fmt.Println("  verdict:   PASS (<10% RSS drift end-vs-start)")
		}
	}
	fmt.Printf("  threads:   start %d, end %d, peak %d\n", first.threads, last.threads, st.threadsMax)
	// Descriptors are the signal the concurrency-cap and >50-broker runs turn
	// on, so report the peak even when the run itself passed: a run that came
	// within a hair of RLIMIT_NOFILE is a result, not a footnote.
	fmt.Printf("  open fds:  start %s, end %s, peak %s\n",
		fdField(first.openFDs), fdField(last.openFDs), fdField(st.fdMax))
	return nil
}
