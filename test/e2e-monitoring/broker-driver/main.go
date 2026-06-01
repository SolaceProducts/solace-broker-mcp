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

// Package main is the broker-driver binary: it connects directly to the
// Solace brokers via the messaging client and generates the live traffic
// (publish, consume, sustain rates) that fixtures F3, F4, and F6 need so the
// monitoring tools have something real to observe.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "connected-client":
		os.Exit(runConnectedClient(args))
	case "publisher":
		os.Exit(runPublisher(args))
	case "publish-batch":
		os.Exit(runPublishBatch(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "broker-driver: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `broker-driver — drives broker state for E2E fixtures.

Usage: broker-driver <subcommand> [flags]

Subcommands:
  connected-client    F3: persistent receiver + client subscriptions, idles until signal
  publisher           F4: persistent publisher at fixed rate, idles until signal
  publish-batch       F6: one-shot persistent publisher, exits after --count messages sent`)
}

// fatalf prints to stderr and exits non-zero. Used by subcommands to surface
// configuration/runtime errors with a uniform prefix.
func fatalf(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "broker-driver: "+format+"\n", args...)
	return 1
}

// requireEnv reads an env var and returns an error message via fatalf-style
// formatting if it is empty. Centralises the "missing X in .env" message so
// every subcommand fails the same way.
func requireEnv(name string) (string, bool) {
	v := os.Getenv(name)
	if v == "" {
		fmt.Fprintf(os.Stderr, "broker-driver: required env var %s is unset (source test/e2e-monitoring/.env first)\n", name)
		return "", false
	}
	return v, true
}

// stringFlag wraps flag.String to make subcommand flag-set wiring less noisy.
func stringFlag(fs *flag.FlagSet, name, def, usage string) *string {
	return fs.String(name, def, usage)
}
