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

// Package version holds the server version string, set at build time via
// ldflags. See docs/packaging-release.md for details.
package version

// version is set at build time via:
//
//	go build -ldflags "-X github.com/SolaceProducts/solace-broker-mcp/internal/version.version=X.Y.Z" ./cmd/server
var version = "dev"

// Version returns the server version string.
func Version() string { return version }
