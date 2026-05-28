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
// (publish, consume, sustain rates) that fixtures F3-F6 need so the
// monitoring tools have something real to observe.
//
// Skeleton only — subcommands are populated as F3-F6 land.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "broker-driver: no subcommands implemented yet (placeholder until F3-F6 land)")
	os.Exit(2)
}
