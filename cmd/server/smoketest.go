// TEMPORARY: This file registers smoke test tools to validate the SEMP client
// layer end-to-end. Delete this file when Task 5 (registry) is implemented.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// getQueuesParams defines the input for the temporary get-queues smoke test tool.
type getQueuesParams struct {
	Broker     string `json:"broker"`
	MsgVpnName string `json:"msgVpnName"`
}

// registerSmokeTestTools registers temporary tools for end-to-end validation.
// Remove this call from main.go and delete this file when Task 5 is implemented.
func registerSmokeTestTools(server *mcp.Server, pool *semp.BrokerPool, operations map[string]*sempv2.Operation) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get-queues",
		Description: "List queues in a message VPN on a Solace broker. This is a temporary smoke test tool.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getQueuesParams) (*mcp.CallToolResult, any, error) {
		if args.Broker == "" {
			return nil, nil, fmt.Errorf("broker parameter is required, available brokers: %v", pool.Aliases())
		}
		if args.MsgVpnName == "" {
			return nil, nil, fmt.Errorf("msgVpnName parameter is required")
		}

		client, err := pool.GetSempV2(args.Broker)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving broker %q: %w (available: %v)", args.Broker, err, pool.Aliases())
		}

		op, ok := operations["monitor/getMsgVpnQueues"]
		if !ok {
			return nil, nil, fmt.Errorf("operation monitor/getMsgVpnQueues not found in specs")
		}

		result, err := client.Execute(ctx, op, map[string]any{
			"msgVpnName": args.MsgVpnName,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("SEMP call failed: %w", err)
		}

		resultJSON, err := json.MarshalIndent(result.Data, "", "  ")
		if err != nil {
			return nil, nil, fmt.Errorf("marshalling result: %w", err)
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		}, nil, nil
	})

	log.Printf("Registered smoke test tool: get-queues")
}
