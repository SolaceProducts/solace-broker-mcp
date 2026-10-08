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

package sempv2_test

import (
	"reflect"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

func TestScrubEnvelope(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{
			name: "nil map does not panic",
			in:   nil,
			want: nil,
		},
		{
			name: "map-valued links stripped (single-resource shape)",
			in: map[string]any{
				"data":  map[string]any{"queueName": "orders"},
				"links": map[string]any{"uri": "https://broker:943/SEMP/v2/config/msgVpns/default/queues/orders"},
			},
			want: map[string]any{
				"data": map[string]any{"queueName": "orders"},
			},
		},
		{
			name: "array-valued links stripped (collection shape)",
			in: map[string]any{
				"data":  []any{map[string]any{"queueName": "orders"}},
				"links": []any{map[string]any{"uri": "https://broker:943/SEMP/v2/monitor/msgVpns/default/queues/orders"}},
			},
			want: map[string]any{
				"data": []any{map[string]any{"queueName": "orders"}},
			},
		},
		{
			name: "non-map meta is left alone rather than panicking",
			in: map[string]any{
				"data": map[string]any{"queueName": "orders"},
				"meta": "not-an-object",
			},
			want: map[string]any{
				"data": map[string]any{"queueName": "orders"},
				"meta": "not-an-object",
			},
		},
		{
			name: "meta.request.uri and nested meta.paging.nextPageUri both stripped, responseCode survives",
			in: map[string]any{
				"data": map[string]any{"queueName": "orders"},
				"meta": map[string]any{
					"request":      map[string]any{"method": "GET", "uri": "https://broker:943/SEMP/v2/monitor/msgVpns/default/queues/orders"},
					"paging":       map[string]any{"nextPageUri": "https://broker:943/SEMP/v2/monitor/msgVpns/default/queues?cursor=abc"},
					"responseCode": float64(200),
				},
			},
			want: map[string]any{
				"data": map[string]any{"queueName": "orders"},
				"meta": map[string]any{
					"request":      map[string]any{"method": "GET"},
					"paging":       map[string]any{},
					"responseCode": float64(200),
				},
			},
		},
		{
			name: "data untouched even when it holds a URI-shaped key",
			in: map[string]any{
				"data":  map[string]any{"queueName": "orders", "fooUri": "not-a-broker-link"},
				"links": map[string]any{"uri": "https://broker:943/SEMP/v2/config/msgVpns/default/queues/orders"},
			},
			want: map[string]any{
				"data": map[string]any{"queueName": "orders", "fooUri": "not-a-broker-link"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sempv2.ScrubEnvelope(tt.in)
			if !reflect.DeepEqual(tt.in, tt.want) {
				t.Errorf("got %+v, want %+v", tt.in, tt.want)
			}
		})
	}
}
