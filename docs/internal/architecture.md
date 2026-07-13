# Architecture — Solace Broker MCP Server

This document describes the architecture as implemented.

---

## Package Structure

One line per package; for current file-level detail, read the package itself.

```
cmd/server/            Entry point — config loading, auth middleware, MCP server startup
internal/
├── auth/              Client (inbound) auth: OAuth/OIDC JWT validation, static dev token, mode banner
├── config/            YAML config, ${VAR} env substitution, validation, broker alias canonicalization
├── defaults/          All default values, with assumption annotations
├── semp/              BrokerPool + BrokerClient — lazy per-broker client creation, thread-safe (RWMutex)
│   ├── auth/          Broker (outbound) auth
│   ├── resilience/    Rate limiting, retries, cookie jar, per-broker in-flight cap
│   ├── sempv1/        SEMPv1 client — XML envelope protocol
│   └── sempv2/        SEMPv2 client — HTTP + embedded OpenAPI spec (private monitor only)
├── composite/         YAML-driven composite tool engine: loader, validator, step executor
│   └── definitions/   tools.yaml — composite tool definitions (source of truth for the tool list)
├── tools/             MCP registration, routing, param validation, identity extraction
│   └── sempv1/        Native Go tool handlers, one package per tool
└── version/           Build version stamping
```

The full tool list is defined by `internal/composite/definitions/tools.yaml`
(composite), the packages under `internal/tools/sempv1/` (native), plus the
built-in `list-brokers` (registered directly in `internal/tools/register.go`) —
count them there, not here.

---

## Component Overview

```mermaid
graph TB
    subgraph "cmd/server"
        MAIN["main.go<br/>Wires everything at startup"]
    end

    subgraph "internal/config"
        CONFIG["config.go<br/>YAML loading, env var substitution, validation"]
    end

    subgraph "internal/defaults"
        DEFAULTS["defaults.go<br/>All default values, assumption annotations"]
    end

    subgraph "internal/semp"
        POOL["pool.go<br/>Lazy BrokerClient creation<br/>Thread-safe (RWMutex)"]
        BROKER["broker.go<br/>Holds sempv2 client<br/>One per broker (lazy)"]

        subgraph "sempv2"
            CLIENT["client.go<br/>sempv2.Client interface<br/>HTTPClient implementation<br/>Basic Auth"]
            OPERATION["operation.go<br/>Operation type<br/>OpenAPI spec parser"]
            SPECS["specs/<br/>Embedded Swagger JSON<br/>Private monitor spec only"]
        end
    end

    subgraph "internal/composite"
        EXECUTOR["executor.go<br/>Step orchestration<br/>Template resolution"]
        LOADER["loader.go<br/>YAML tool definitions"]
    end

    subgraph "internal/tools"
        REGISTRY["tools/<br/>ToolManager, register.go<br/>MCP tool registration<br/>Broker resolution per call"]
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

```


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

    User->>SDK: POST /mcp<br/>tool: get-rdp-status<br/>broker: "prod-us"
    SDK->>Reg: Handle(ctx, params)

    Note over Reg: Extract broker="prod-us"<br/>Remove from params
    Reg->>Pool: GetSempV2("prod-us")
    Note over Pool: Lazy creation:<br/>RLock check → not found →<br/>Lock → double-check → create
    Pool-->>Reg: sempv2.Client

    Reg->>Exec: Execute(ctx, tool, client, params)

    Note over Exec: Step 1: monitor/getMsgVpnRestDeliveryPoint
    Exec->>HC: Execute(ctx, op, args)
    HC->>Broker: GET /SEMP/v2/__private_monitor__/.../restDeliveryPoints/{rdp}<br/>Authorization: Basic ...
    Broker-->>HC: 200 OK + JSON
    HC-->>Exec: Result

    Note over Exec: Steps 2 + 3: parallel batch (parallel: true)
    par monitor/getMsgVpnRestDeliveryPointQueueBindings
        Exec->>HC: Execute(ctx, op, args)
        HC->>Broker: GET .../restDeliveryPoints/{rdp}/queueBindings
        Broker-->>HC: 200 OK + JSON
        HC-->>Exec: Result
    and monitor/getMsgVpnRestDeliveryPointRestConsumers
        Exec->>HC: Execute(ctx, op, args)
        HC->>Broker: GET .../restDeliveryPoints/{rdp}/restConsumers
        Broker-->>HC: 200 OK + JSON
        HC-->>Exec: Result
    end

    Note over Exec: Apply collect strategy
    Exec-->>Reg: Combined result (3 steps)
    Reg-->>SDK: MCP CallToolResult
    SDK-->>User: Tool response
```

The executor supports two `result.strategy` values:

- **`collect`** — returns the raw step-result map keyed by step ID.
- **`postProcess`** — runs a Go handler registered in [`internal/composite/postprocess/`](../../internal/composite/postprocess/) over the step results and merges its summary map under a top-level `"summary"` key alongside the raw results. Handlers register from `init()`; `cmd/server/main.go` blank-imports the handlers package so registration happens before startup validation. At boot the server cross-checks each handler's `RequiredFields` against the union of its tool's step `select:` arrays and refuses to start on a mismatch (template: `tool %q: postprocessor %q reads %q but it is not in select`). This catches SEMP field-name drift before any tool call.

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
        HC->>Broker: GET /SEMP/v2/__private_monitor__/...
    and User B tool call (concurrent)
        B->>Pool: GetSempV2("prod-us")
        Pool-->>B: sempv2.Client (same instance)
        B->>HC: Execute(ctx, op, args)
        HC->>Broker: GET /SEMP/v2/__private_monitor__/...
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
| **Private monitor, config, and action specs are embedded** | Exposes extended fields (e.g. `bindCount`) absent from the public specs. `validSpecTypes` in `internal/semp/sempv2/operation.go` is the gate for adding any future spec. |
| **Operation IDs prefixed with spec type** | Keys like `monitor/getMsgVpnQueue` stay unambiguous — operationIds repeat across the SEMP monitor/config/action APIs, so re-embedding a spec later can't collide |
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
| **Config** | YAML parsing, env var substitution (${VAR_NAME}), validation | Everything else |

---
