# Architecture — Solace Broker MCP Server

This document describes the architecture as implemented. For design rationale, future plans, and detailed scenarios, see [specs/001-architecture-diagrams.md](../specs/001-architecture-diagrams.md).

---

## Folder Structure

```
solace-broker-mcp/
├── cmd/server/
│   └── main.go                    # Entry point, MCP server startup
│
├── internal/
│   ├── defaults/
│   │   └── defaults.go            # All default values, assumption annotations
│   │
│   ├── config/
│   │   └── config.go              # YAML config, env_prefix credentials, validation
│   │
│   ├── semp/
│   │   ├── broker.go              # BrokerClient — holds per-broker sempv2 client
│   │   ├── pool.go                # BrokerPool — lazy creation, thread-safe (RWMutex)
│   │   └── sempv2/
│   │       ├── client.go          # Client interface + HTTPClient (HTTP calls, Basic Auth)
│   │       ├── operation.go       # Operation type + OpenAPI spec parser
│   │       └── specs/
│   │           ├── embed.go       # //go:embed for spec files
│   │           └── *.json         # Swagger 2.0 specs (monitor, config, action)
│   │
│   ├── composite/                 # (Task 4 — not yet implemented)
│   │   ├── definition.go          # Tool/step/parameter structs
│   │   ├── loader.go              # YAML loading + validation
│   │   ├── executor.go            # Step orchestration, templates, result strategies
│   │   └── definitions/
│   │       └── tools.yaml         # Tool definitions (queue-replay-recovery)
│   │
│   └── registry/                  # (Task 5 — not yet implemented)
│       └── registry.go            # MCP tool registration, broker resolution
│
├── docs/                          # This directory
└── specs/                         # Design specs, task breakdowns, test plans
```

---

## Component Overview

```mermaid
graph TB
    subgraph "cmd/server"
        MAIN["main.go<br/>Wires everything at startup"]
    end

    subgraph "internal/config"
        CONFIG["config.go<br/>YAML loading, env_prefix credentials, validation"]
    end

    subgraph "internal/defaults"
        DEFAULTS["defaults.go<br/>All default values, assumption annotations"]
    end

    subgraph "internal/semp"
        POOL["pool.go<br/>Lazy BrokerClient creation<br/>Thread-safe (RWMutex)"]
        BROKER["broker.go<br/>Holds sempv2 client<br/>One per broker (lazy)"]

        subgraph "sempv2"
            CLIENT["client.go<br/>sempv2.Client interface<br/>HTTPClient implementation<br/>Basic Auth"]
            OPERATION["operation.go<br/>Operation type<br/>OpenAPI spec parser<br/>799 operations"]
            SPECS["specs/<br/>Embedded Swagger JSON<br/>Monitor + Config + Action"]
        end
    end

    subgraph "internal/composite (Task 4)"
        EXECUTOR["executor.go<br/>Step orchestration<br/>Template resolution"]
        LOADER["loader.go<br/>YAML tool definitions"]
    end

    subgraph "internal/registry (Task 5)"
        REGISTRY["registry.go<br/>MCP tool registration<br/>Broker resolution per call"]
    end

    subgraph "External"
        MCP_SDK["Go MCP SDK"]
        BROKER_X["Solace Broker X"]
        BROKER_Y["Solace Broker Y"]
    end

    MAIN --> CONFIG
    MAIN --> POOL
    MAIN --> LOADER
    MAIN --> EXECUTOR
    MAIN --> REGISTRY
    CONFIG --> DEFAULTS
    POOL --> BROKER
    BROKER --> CLIENT
    CLIENT --> OPERATION
    OPERATION --> SPECS
    REGISTRY --> MCP_SDK
    REGISTRY --> POOL
    REGISTRY --> EXECUTOR
    EXECUTOR --> CLIENT
    CLIENT --> BROKER_X
    CLIENT --> BROKER_Y

    style EXECUTOR stroke-dasharray: 5 5
    style LOADER stroke-dasharray: 5 5
    style REGISTRY stroke-dasharray: 5 5
```

*Dashed borders = not yet implemented.*

---

## Request Flow — Single Tool Call

```mermaid
sequenceDiagram
    participant User as MCP Client<br/>(Claude Code)
    participant SDK as MCP SDK
    participant Reg as Registry Handler
    participant Pool as BrokerPool
    participant Exec as Composite Executor
    participant HC as HTTPClient<br/>(prod-us)
    participant Broker as Solace Broker<br/>(prod-us)

    User->>SDK: POST /mcp<br/>tool: queue-replay-recovery<br/>broker: "prod-us"
    SDK->>Reg: Handle(ctx, params)

    Note over Reg: Extract broker="prod-us"<br/>Remove from params
    Reg->>Pool: GetSempV2("prod-us")
    Note over Pool: Lazy creation:<br/>RLock check → not found →<br/>Lock → double-check → create
    Pool-->>Reg: sempv2.Client

    Reg->>Exec: Execute(ctx, tool, client, params)

    Note over Exec: Step 1: monitor/getMsgVpnQueue
    Exec->>HC: Execute(ctx, op, args)
    HC->>Broker: GET /SEMP/v2/monitor/.../queues/{queue}<br/>Authorization: Basic ...
    Broker-->>HC: 200 OK + JSON
    HC-->>Exec: Result

    Note over Exec: Step 2: action/doMsgVpnQueueCancelReplay
    Exec->>HC: Execute(ctx, op, args)
    HC->>Broker: PUT /SEMP/v2/action/.../cancelReplay
    Broker-->>HC: 200 OK
    HC-->>Exec: Result

    Note over Exec: Step 3: action/doMsgVpnQueueStartReplay
    Exec->>HC: Execute(ctx, op, args)
    HC->>Broker: PUT /SEMP/v2/action/.../startReplay
    Broker-->>HC: 200 OK
    HC-->>Exec: Result

    Note over Exec: Apply collect strategy
    Exec-->>Reg: Combined result (3 steps)
    Reg-->>SDK: MCP CallToolResult
    SDK-->>User: Tool response
```

---

## Concurrency — Multiple Users, Same Broker

```mermaid
sequenceDiagram
    participant A as User A
    participant B as User B
    participant Pool as BrokerPool
    participant HC as HTTPClient<br/>(prod-us)
    participant Broker as Solace Broker<br/>(prod-us)

    Note over Pool,HC: Same HTTPClient instance<br/>Same TCP connection pool<br/>(created lazily on first call)

    par User A tool call
        A->>Pool: GetSempV2("prod-us")
        Pool-->>A: sempv2.Client (shared)
        A->>HC: Execute(ctx, op, args)
        HC->>Broker: GET /SEMP/v2/monitor/...
    and User B tool call (concurrent)
        B->>Pool: GetSempV2("prod-us")
        Pool-->>B: sempv2.Client (same instance)
        B->>HC: Execute(ctx, op, args)
        HC->>Broker: GET /SEMP/v2/monitor/...
    end

    Broker-->>HC: Response to User A
    Broker-->>HC: Response to User B

    Note over HC: http.Client is goroutine-safe<br/>Each request has its own headers/body<br/>TCP connections shared via pool
```

---

## Concurrency — Multiple Users, Different Brokers

```mermaid
graph LR
    subgraph "User A"
        A["tool call<br/>broker=prod-us"]
    end

    subgraph "User B"
        B["tool call<br/>broker=prod-eu"]
    end

    subgraph "BrokerPool"
        subgraph "BrokerClient: prod-us"
            HC_US["HTTPClient<br/>(separate TCP pool)"]
        end
        subgraph "BrokerClient: prod-eu"
            HC_EU["HTTPClient<br/>(separate TCP pool)"]
        end
    end

    BROKER_US["Solace Broker<br/>prod-us"]
    BROKER_EU["Solace Broker<br/>prod-eu"]

    A -->|"GetSempV2(prod-us)"| HC_US
    B -->|"GetSempV2(prod-eu)"| HC_EU
    HC_US --> BROKER_US
    HC_EU --> BROKER_EU
```

Completely independent — load on prod-us does not affect prod-eu.

---

## Key Design Decisions

| Decision | Why |
|---|---|
| **Lazy broker client creation** | With 500 configured brokers, only active ones allocate HTTP clients and TCP connections |
| **sempv2.Client is an interface** | Enables mock testing of executor without HTTP; enables future OAuth wrapper without changing executor |
| **Operation IDs prefixed with spec type** | `monitor/getMsgVpnQueue` vs `config/getMsgVpnQueue` — 150+ duplicate operationIds across specs are disambiguated |
| **$ref parameters resolved at parse time** | Shared query params (select, where, count, cursor) are available to all operations, not silently lost |
| **Handler resolves broker, executor receives client** | Executor is pure orchestration — no knowledge of brokers, auth, or pools. Auth changes (OAuth) don't touch executor. |
| **Broker param is always required** | No default broker concept. The LLM always specifies which broker to target. |

---

## Component Responsibilities

| Component | Knows about | Does NOT know about |
|---|---|---|
| **Registry Handler** | BrokerPool, broker aliases, MCP SDK, composite executor | HTTP calls, SEMP protocol |
| **Composite Executor** | Tool definitions, steps, templates, result strategies | Brokers, HTTP, auth |
| **BrokerClient** | sempv2 client | Tools, steps, MCP protocol |
| **sempv2.HTTPClient** | HTTP calls, auth headers, JSON parsing | Tools, brokers, MCP protocol |
| **BrokerPool** | Map of configs, lazy client creation, RWMutex | Tools, MCP protocol, HTTP details |
| **Config** | YAML parsing, env_prefix credential resolution, validation | Everything else |

---
