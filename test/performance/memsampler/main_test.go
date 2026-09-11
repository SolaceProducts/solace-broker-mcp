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

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The parse is the only place in this tool where a wrong answer is silent. A
// mis-parsed VmRSS does not crash the run; it writes a plausible number into a
// CSV that a later comparison trusts, which is the failure mode the whole run
// record exists to prevent. Hence a test per shape that could produce one:
// unit suffix, unexpected whitespace, a field that is absent, and a status
// file that names none of the three.

func TestParseStatus(t *testing.T) {
	// Trimmed from a real /proc/<pid>/status. The interleaved unrelated fields
	// are kept deliberately: the parser keys on the name before the colon, and
	// a substring match would pick VmRSS out of nothing but also match a
	// hypothetical VmRSSAnon, so the neighbours are part of the fixture.
	const status = `Name:	mcp-server
Umask:	0022
State:	S (sleeping)
Tgid:	4242
Pid:	4242
VmPeak:	  1841232 kB
VmSize:	  1775696 kB
VmLck:	       0 kB
VmHWM:	   84120 kB
VmRSS:	   79284 kB
RssAnon:	   61240 kB
RssFile:	   18044 kB
Threads:	23
SigQ:	0/62481
`

	got, err := parseStatus(status)
	if err != nil {
		t.Fatalf("parseStatus: unexpected error: %v", err)
	}
	if got.rssKB != 79284 {
		t.Errorf("rssKB = %d, want 79284 (VmRSS, not VmHWM/RssAnon)", got.rssKB)
	}
	if got.vmKB != 1775696 {
		t.Errorf("vmKB = %d, want 1775696 (VmSize, not VmPeak)", got.vmKB)
	}
	if got.threads != 23 {
		t.Errorf("threads = %d, want 23", got.threads)
	}
	// parseStatus reads /proc/<pid>/status only; the descriptor count comes
	// from a separate read, so a parsed sample must not claim a count of zero.
	if got.openFDs != fdUnavailable {
		t.Errorf("openFDs = %d, want fdUnavailable (%d) — status carries no fd count",
			got.openFDs, fdUnavailable)
	}
}

func TestParseStatusFieldVariants(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		wantRSS     int
		wantVM      int
		wantThreads int
	}{
		{
			name:        "space separated instead of tab",
			status:      "VmSize: 2048 kB\nVmRSS: 1024 kB\nThreads: 7\n",
			wantRSS:     1024,
			wantVM:      2048,
			wantThreads: 7,
		},
		{
			name: "no trailing newline on the last field",
			// The final line has no "\n"; SplitSeq must still yield it, or the
			// last field of a real status file could be dropped.
			status:      "VmRSS:\t1024 kB\nVmSize:\t2048 kB\nThreads:\t7",
			wantRSS:     1024,
			wantVM:      2048,
			wantThreads: 7,
		},
		{
			name: "Threads absent",
			// Threads is not optional in practice, but a zero here must come
			// from the field being absent rather than from a parse failure
			// that also loses the memory numbers.
			status:      "VmRSS:\t1024 kB\nVmSize:\t2048 kB\n",
			wantRSS:     1024,
			wantVM:      2048,
			wantThreads: 0,
		},
		{
			name: "VmSize absent but VmRSS present",
			// A kernel thread has no VmSize. RSS alone is still a usable
			// sample, so this must not be an error.
			status:      "VmRSS:\t1024 kB\nThreads:\t2\n",
			wantRSS:     1024,
			wantVM:      0,
			wantThreads: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatus(tc.status)
			if err != nil {
				t.Fatalf("parseStatus: unexpected error: %v", err)
			}
			if got.rssKB != tc.wantRSS {
				t.Errorf("rssKB = %d, want %d", got.rssKB, tc.wantRSS)
			}
			if got.vmKB != tc.wantVM {
				t.Errorf("vmKB = %d, want %d", got.vmKB, tc.wantVM)
			}
			if got.threads != tc.wantThreads {
				t.Errorf("threads = %d, want %d", got.threads, tc.wantThreads)
			}
		})
	}
}

// A status file with neither memory field is not a sample worth writing — it
// means we read something that is not a process status (or read it as the
// process was being reaped). Failing loudly is what stops a CSV filling with
// zero rows that look like a process using no memory.
func TestParseStatusRejectsMissingMemory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"empty", ""},
		{"no memory fields", "Name:\tmcp-server\nThreads:\t23\n"},
		{"unparseable values", "VmRSS:\tnot-a-number kB\nVmSize:\tnope kB\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseStatus(tc.status); err == nil {
				t.Fatal("parseStatus: want error, got nil")
			}
		})
	}
}

// A field that is present but malformed must fail, even when the other fields
// are fine. This is the case a "no VmRSS/VmSize at all" guard cannot catch: a
// bad VmRSS beside a good VmSize would otherwise be recorded as rss_kb=0 — a
// plausible number written into a CSV that a later comparison trusts, which is
// strictly worse than no sample at all.
func TestParseStatusRejectsMalformedFieldBesideGoodOnes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{
			name:   "bad VmRSS, good VmSize",
			status: "VmRSS:\tnot-a-number kB\nVmSize:\t2048 kB\nThreads:\t7\n",
		},
		{
			name:   "good VmRSS, bad VmSize",
			status: "VmRSS:\t1024 kB\nVmSize:\tnope kB\nThreads:\t7\n",
		},
		{
			name:   "bad Threads, good memory",
			status: "VmRSS:\t1024 kB\nVmSize:\t2048 kB\nThreads:\tmany\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatus(tc.status)
			if err == nil {
				t.Fatalf("parseStatus: want error, got %+v — a malformed field became a silent zero", got)
			}
		})
	}
}

func TestParseKB(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "79284 kB", want: 79284},
		{in: "79284", want: 79284}, // no unit suffix
		{in: "0 kB", want: 0},
		{in: "", wantErr: true},
		{in: "kB", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, err := parseKB(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseKB(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKB(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseKB(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// The self-handle correction is the one piece of arithmetic in the fd path
// that can be silently wrong, so it gets an absolute assertion rather than a
// delta one.
//
// A delta test — open N files, require the count to move by N — looks like it
// covers this and does not: the correction is a constant, so it cancels out of
// any difference. Deleting `n--` from countOpenFDs leaves a delta test green.
// The count is therefore compared against a directly-read /proc/self/fd
// listing, which is the ground truth the function is correcting.
func TestCountOpenFDsSelfExcludesItsOwnHandle(t *testing.T) {
	// Hold a descriptor open so the counts are not incidentally zero.
	held, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer held.Close()

	got, err := countOpenFDs(os.Getpid())
	if err != nil {
		t.Fatalf("countOpenFDs: %v", err)
	}

	// Read the same directory the same way, immediately after, and keep the
	// handle open across the read so the listing includes it — exactly the
	// condition countOpenFDs has to correct for.
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		t.Fatalf("opening /proc/self/fd: %v", err)
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	raw := len(names)

	if raw <= 1 {
		t.Fatalf("/proc/self/fd listed %d entries — expected at least the standard streams", raw)
	}
	// The raw listing counts the reading handle; countOpenFDs must not.
	if got != raw-1 {
		t.Errorf("countOpenFDs(self) = %d, want %d (raw listing %d minus the directory handle) — "+
			"the self-handle correction is missing or applied when it should not be", got, raw-1, raw)
	}
}

// The correction must apply ONLY to a self-sample. A different process's fd
// table does not contain our directory handle, so subtracting unconditionally
// would under-report every real run of this tool by one — which is the whole
// point of keying it on the pid.
func TestCountOpenFDsChildProcessIsNotCorrected(t *testing.T) {
	// CommandContext, not Command: the repo lints for it (noctx), and the
	// context also guarantees the child dies with the test rather than
	// outliving a panic.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child process to sample: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)

	// Wait for the child's descriptor table to settle before comparing.
	//
	// A freshly started process is still closing the loader's descriptors, so
	// sampling it once and comparing against a second read taken a moment
	// later compares two different instants — which made this test flake (a
	// 4-vs-3 disagreement on one run in seven). Poll until two consecutive
	// direct reads agree, then compare that settled count against the
	// function's answer.
	var settled int
	for attempt := 0; attempt < 50; attempt++ {
		first, err := os.ReadDir(fdDir)
		if err != nil {
			t.Fatalf("reading %s: %v", fdDir, err)
		}
		second, err := os.ReadDir(fdDir)
		if err != nil {
			t.Fatalf("reading %s: %v", fdDir, err)
		}
		if len(first) == len(second) {
			settled = len(first)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if settled == 0 {
		t.Skip("the child's descriptor table never settled; nothing to compare against")
	}

	got, err := countOpenFDs(pid)
	if err != nil {
		t.Fatalf("countOpenFDs(%d): %v", pid, err)
	}
	if got != settled {
		t.Errorf("countOpenFDs(child) = %d, want %d (no correction applies to another process)",
			got, settled)
	}
}

// A PID that does not exist must report unavailable with an error, not zero:
// run() keys "process exited" off the status read, and a zero here would write
// a row claiming the process held no descriptors.
func TestCountOpenFDsMissingProcess(t *testing.T) {
	// PID 0 is never a userspace process, so /proc/0/fd never exists.
	got, err := countOpenFDs(0)
	if err == nil {
		t.Fatalf("countOpenFDs(0) = %d, want an error", got)
	}
	if got != fdUnavailable {
		t.Errorf("countOpenFDs(0) = %d, want fdUnavailable (%d)", got, fdUnavailable)
	}
}

// The CSV is consumed by awk in summary.sh and lib.sh, both of which treat a
// non-numeric field as "no data". "NA" is the token sampler.sh already uses,
// so an unreadable descriptor directory must render as that and not as -1,
// which would average into a run's numbers as a real value.
func TestFDField(t *testing.T) {
	if got := fdField(fdUnavailable); got != "NA" {
		t.Errorf("fdField(fdUnavailable) = %q, want \"NA\"", got)
	}
	if got := fdField(0); got != "0" {
		t.Errorf("fdField(0) = %q, want \"0\" — zero descriptors is a value, not a gap", got)
	}
	if got := fdField(137); got != "137" {
		t.Errorf("fdField(137) = %q, want \"137\"", got)
	}
}

// The stats roll-up feeds both the printed summary and (through the CSV) the
// run record's fd_peak. A peak that only tracked the last sample would hide
// exactly the mid-run spike these runs are hunting.
func TestStatsTracksPeaks(t *testing.T) {
	st := newStats(procSample{rssKB: 100, threads: 10, openFDs: 50})
	st.observe(procSample{rssKB: 300, threads: 40, openFDs: 900})
	st.observe(procSample{rssKB: 80, threads: 12, openFDs: 60})

	if st.samples != 3 {
		t.Errorf("samples = %d, want 3", st.samples)
	}
	if st.rssMin != 80 || st.rssMax != 300 {
		t.Errorf("rss min/max = %d/%d, want 80/300", st.rssMin, st.rssMax)
	}
	if st.threadsMax != 40 {
		t.Errorf("threadsMax = %d, want 40", st.threadsMax)
	}
	if st.fdMax != 900 {
		t.Errorf("fdMax = %d, want 900 (the mid-run peak, not the last sample)", st.fdMax)
	}
	if st.last.rssKB != 80 {
		t.Errorf("last.rssKB = %d, want 80", st.last.rssKB)
	}
}

// An unreadable descriptor directory must not poison the peak. fdUnavailable
// is negative precisely so it loses every max comparison; if the sentinel ever
// becomes a large value, a single unreadable sample would report as the peak.
func TestStatsIgnoresUnavailableFDs(t *testing.T) {
	st := newStats(procSample{rssKB: 100, openFDs: fdUnavailable})
	if st.fdMax != fdUnavailable {
		t.Fatalf("fdMax = %d, want fdUnavailable before any readable sample", st.fdMax)
	}
	st.observe(procSample{rssKB: 100, openFDs: 12})
	st.observe(procSample{rssKB: 100, openFDs: fdUnavailable})
	if st.fdMax != 12 {
		t.Errorf("fdMax = %d, want 12 — an unreadable sample must not beat a real one", st.fdMax)
	}
}

// Guards the CSV's column contract. lib.sh reads open_fds as column 6 and
// threads as column 5 by index, and summary.sh reads mem.csv the same way, so
// inserting a column here silently reassigns both.
func TestCSVHeaderColumnOrder(t *testing.T) {
	const want = "t_sec,wall_ts,rss_kb,vm_kb,threads,open_fds"
	got := strings.Join(csvHeader, ",")
	if got != want {
		t.Errorf("CSV header = %q, want %q (lib.sh and summary.sh index these columns)", got, want)
	}
}
