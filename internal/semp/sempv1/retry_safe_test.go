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

package sempv1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestIsShowCommand(t *testing.T) {
	cases := []struct {
		name string
		xml  string
		want bool
	}{
		{"show with child", `<rpc><show><version/></show></rpc>`, true},
		{"self-closing show", `<rpc><show/></rpc>`, true},
		{"leading whitespace", "  \n\t<rpc><show><memory/></show></rpc>", true},
		{"rpc with semp-version attr", `<rpc semp-version="soltr/10_8VMR"><show><redundancy/></show></rpc>`, true},
		{"whitespace between rpc and show", `<rpc> <show><version/></show></rpc>`, true},
		{"tab after show", "<rpc><show\t><version/></show></rpc>", true},
		{"newline after show", "<rpc><show\n><version/></show></rpc>", true},
		{"crlf after show", "<rpc><show\r\n><version/></show></rpc>", true},
		{"mutating command", `<rpc><admin><shutdown/></admin></rpc>`, false},
		{"create command", `<rpc><create><queue><name>q1</name></queue></create></rpc>`, false},
		{"show not first element", `<rpc><admin><show/></admin></rpc>`, false},
		{"showcase element is not show", `<rpc><showcase/></rpc>`, false},
		{"showX element is not show", `<rpc><showX/></rpc>`, false},
		{"shower element is not show", `<rpc><shower><version/></shower></rpc>`, false},
		{"unterminated show tag", `<rpc><show`, false},
		{"empty rpc", `<rpc/>`, false},
		{"empty string", ``, false},
		{"not rpc", `<show><version/></show>`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isShowCommand(tc.xml); got != tc.want {
				t.Errorf("isShowCommand(%q) = %v, want %v", tc.xml, got, tc.want)
			}
		})
	}
}

func TestExecute_ShowCommand_RetriesOn503(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(successEnvelope))
	}))
	defer srv.Close()
	client := newTestClient(t, srv)
	defer client.Close()

	result, err := client.Execute(context.Background(), `<rpc><show><version/></show></rpc>`)
	if err != nil {
		t.Fatalf("Execute() error: %v — show command not retried through transient 503", err)
	}
	if result == nil || len(result.InnerXML) == 0 {
		t.Fatal("expected parsed result after retry")
	}
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + retry), got %d", requestCount.Load())
	}
}

func TestExecute_NonShowCommand_NotRetried(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client := newTestClient(t, srv)
	defer client.Close()

	_, err := client.Execute(context.Background(), `<rpc><admin><shutdown/></admin></rpc>`)
	if err == nil {
		t.Fatal("expected error for 503 on non-show command")
	}
	if requestCount.Load() != 1 {
		t.Errorf("expected exactly 1 request for non-show command, got %d", requestCount.Load())
	}
}
