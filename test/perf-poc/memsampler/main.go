// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

// Command memsampler polls /proc/<pid>/status at a fixed interval and
// writes a CSV row per sample. Meant to run alongside test/perf-poc/loadgen
// so a plot of RSS vs. wall-clock during a load run reveals whether MCP's
// footprint climbs (leak) or holds steady (per plan step 8's <10% drift
// over 30s bar).
//
// The plan calls for HeapAlloc / Sys from Go's runtime alongside RSS, but
// MCP does not expose pprof/expvar today. RSS + VmSize + thread count from
// /proc is what we can get without touching production code. Add a
// Go-heap column here if MCP later exposes /debug/vars or /debug/pprof.
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

func run(ctx context.Context, pid int, interval, duration time.Duration, outPath string, quiet bool) error {
	// Open the CSV sink first so we fail fast on a bad path before starting
	// the ticker. stdout support keeps ad-hoc invocation cheap.
	w, closeFn, err := openCSV(outPath)
	if err != nil {
		return err
	}
	defer closeFn()

	if err := w.Write([]string{"t_sec", "wall_ts", "rss_kb", "vm_kb", "threads"}); err != nil {
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

	var last procSample
	last = first
	samples := 1
	rssMin, rssMax := first.rssKB, first.rssKB

	for {
		select {
		case <-ctx.Done():
			return finish(w, start, samples, rssMin, rssMax, first, last, quiet, "context canceled")
		case now := <-t.C:
			if duration > 0 && !now.Before(deadline) {
				return finish(w, start, samples, rssMin, rssMax, first, last, quiet, "duration elapsed")
			}
			s, err := sampleProc(pid)
			if err != nil {
				// Distinguish "process gone" from a transient parse error;
				// the former is a clean end-of-run, the latter shouldn't
				// swallow silently.
				if errors.Is(err, os.ErrNotExist) {
					return finish(w, start, samples, rssMin, rssMax, first, last, quiet, "process exited")
				}
				fmt.Fprintf(os.Stderr, "memsampler: sample error at t+%s: %v\n", now.Sub(start).Round(time.Millisecond), err)
				continue
			}
			writeRow(w, now.Sub(start), now, s)
			w.Flush()
			samples++
			last = s
			if s.rssKB < rssMin {
				rssMin = s.rssKB
			}
			if s.rssKB > rssMax {
				rssMax = s.rssKB
			}
		}
	}
}

// procSample is what one poll of /proc/<pid>/status extracts. Kept small on
// purpose — anything beyond RSS/VmSize/Threads should be added deliberately
// with a matching CSV column.
type procSample struct {
	rssKB   int
	vmKB    int
	threads int
}

// sampleProc parses the handful of fields we care about out of
// /proc/<pid>/status. Not using gopsutil to keep the PoC dependency-free.
func sampleProc(pid int) (procSample, error) {
	path := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return procSample{}, err
	}
	var s procSample
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "VmRSS":
			s.rssKB, _ = parseKB(val)
		case "VmSize":
			s.vmKB, _ = parseKB(val)
		case "Threads":
			s.threads, _ = strconv.Atoi(val)
		}
	}
	if s.rssKB == 0 && s.vmKB == 0 {
		return procSample{}, fmt.Errorf("no VmRSS/VmSize in %s", path)
	}
	return s, nil
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
		f.Close()
	}, nil
}

func finish(w *csv.Writer, start time.Time, samples, rssMin, rssMax int, first, last procSample, quiet bool, reason string) error {
	w.Flush()
	if quiet {
		return nil
	}

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
	fmt.Printf("  threads:   start %d, end %d\n", first.threads, last.threads)
	return nil
}
