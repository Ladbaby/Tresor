# LLM Gateway — Extensible Architectural Plan

This updated implementation plan refines the LLM Gateway into an extensible LLM traffic overwriting and routing engine. Instead of a hardcoded "alias to downstream" proxy, the architecture uses a pipeline-based middleware model. This accommodates arbitrary request/response translations (e.g., converting Anthropic API calls to OpenAI targets, or vice-versa), runtime plugins, and multi-interface control (Web UI, REST API, CLI, or local sockets).

---

## Architectural Concept

```
                        [ CLI / Native App / Web UI ]
                                     │
                             (Control via API/UDS)
                                     │
                                     ▼
[ Client Request ] ──► [ Interceptor / Engine ] ──► [ Target Downstream ]
                              │          ▲
                              │          │
                     (Request Pipeline) (Response Pipeline)
                              │          │
                              ▼          │
                        [ Plugins / Transformers ]
```

The gateway treats every incoming request as a data payload passing through a configurable execution pipeline. Each rule defines a sequence of steps (request transformers, routing resolvers, and response transformers) that modify headers, paths, bodies, and streaming events.

---

## Technology Choices

| Concern | Choice | Reason |
|---|---|---|
| Language | Go 1.26+ | Fast compilation, cross-platform CLI capabilities, native concurrency (go.mod: go 1.26.4). |
| Router | `chi` (`github.com/go-chi/chi/v5`) | Simple routing, easy middleware chaining. |
| CLI Framework | `cobra` (`github.com/spf13/cobra`) | De facto standard for robust Go CLI applications. |
| Inter-process | HTTP/REST or Unix Sockets | Allows local native utilities to communicate with the daemon securely. |
| Plugin Hook | Go Interface | Native Go interfaces for built-in transformers. WASM-based dynamic plugins (e.g., via `wazero`) are deferred to a future milestone. |

---

## Updated Directory Structure

```
llm-gateway/
├── main.go                  # CLI entry point (dispatches to 'run', 'switch', etc.)
├── go.mod
├── go.sum
│
├── cmd/                     # Command-line interface definitions
│   ├── root.go
│   ├── run.go               # Starts the daemon/gateway
│   └── rule.go              # CLI commands to view/manipulate routing rules
│
├── internal/
│   ├── config/
│   │   └── config.go        # Supports environment variables and config files
│   │
│   ├── store/
│   │   ├── store.go         # SQLite engine and schema migrations
│   │   ├── rule.go          # Custom routing rules / pipeline storage
│   │   └── downstream.go    # Downstream provider registry
│   │
│   ├── engine/
│   │   ├── engine.go        # Main HTTP proxy handler and pipeline executor
│   │   ├── pipeline.go      # Request/Response transformation orchestrator
│   │   └── types.go         # Shared interfaces (Transformer, Matcher)
│   │
│   ├── plugins/
│   │   ├── registry.go      # Registers built-in and dynamic plugins
│   │   ├── openai2anthropic/# Transform OpenAI format to Anthropic
│   │   ├── anthropic2openai/# Transform Anthropic format to OpenAI (reverse translation)
│   │   └── custom_header/   # Arbitrary header injectors
│   │
│   ├── api/
│   │   ├── router.go        # Admin API & local IPC router
│   │   ├── rules.go         # REST endpoints for rules
│   │   └── downstreams.go   # REST endpoints for downstreams
│   │
│   └── middleware/
│       └── auth.go          # Optional local/JWT authentication
│
└── web/                     # Single-page control panel (embedded)
    ├── index.html
    ├── style.css
    └── app.js
```

---

## Step-by-Step Implementation

### Step 1 — Scaffold and CLI Setup

The gateway binary serves as both the daemon (the long-running server) and the client CLI that controls it.

Initialize dependencies:
```bash
go get github.com/spf13/cobra
go get github.com/go-chi/chi/v5
go get modernc.org/sqlite
```

Define the entrypoint in `main.go` to delegate execution to Cobra:
```go
package main

import "github.com/yourname/llm-gateway/cmd"

func main() {
    cmd.Execute()
}
```

In `cmd/run.go`, implement the command to boot the gateway server. In `cmd/rule.go`, implement commands like `llm-gateway rule switch <rule-id> <downstream-id>` to allow local tools to control the server without using the Web UI.

---

### Step 2 — Configuration & Local Socket Support

The server can listen on a standard TCP port, a local Unix Domain Socket (UDS), or both. Unix sockets provide local command-line tools and native applications a secure, credential-free channel to control the gateway.

`internal/config/config.go`:
```go
type Config struct {
    BindAddr      string // e.g., "127.0.0.1:8080"
    SocketPath    string // e.g., "/var/run/llm-gateway.sock" (optional)
    DBPath        string // SQLite path
    AdminPassword string
    JWTSecret     []byte
}
```

---

### Step 3 — Extensible Data Schema

To support generic traffic rewriting, we transition from strict "aliases" to flexible "Routing Rules". A rule defines how to match an incoming request and which plugin chain to execute.

```sql
-- Represents a target endpoint or model provider
CREATE TABLE IF NOT EXISTS downstreams (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    base_url   TEXT NOT NULL,
    api_key    TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Represents an active interception/routing policy
CREATE TABLE IF NOT EXISTS rules (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    pattern_path        TEXT NOT NULL,        -- e.g., "/v1/chat/completions" or "*"
    pattern_model       TEXT,                 -- Match "model" field in request JSON (optional)
    active_downstream   TEXT REFERENCES downstreams(id),
    
    -- Ordered list of plugins to execute, stored as JSON:
    -- [{"plugin_id": "openai2anthropic"}, {"plugin_id": "header_inject", "config": {"X-Custom": "Value"}}]
    pipeline_config     TEXT NOT NULL, 
    
    is_enabled          INTEGER DEFAULT 1,
    created_at          DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

---

### Step 4 — Pipeline Engine & Plugin Architecture

The engine uses standard Go interfaces to represent plugins. Any structure that implements request or response modification can join the pipeline.

`internal/engine/types.go`:
```go
package engine

import "net/http"

type PipelineContext struct {
    TargetDownstream *Downstream
    Variables        map[string]interface{}
}

type RequestTransformer interface {
    TransformRequest(req *http.Request, body []byte, ctx *PipelineContext) (*http.Request, []byte, error)
}

type ResponseTransformer interface {
    TransformResponse(resp *http.Response, body []byte, ctx *PipelineContext) ([]byte, error)
}
```

#### Executing the Pipeline:
When a request arrives at the proxy handler:

1. **Analyze:** Inspect the request path and parse the request body to extract the model name.
2. **Match:** Search the SQLite store for a matching rule based on path and model name.
3. **Instantiate:** Load the plugins defined in `pipeline_config`.
4. **Transform Request:** Run the body and headers through each `RequestTransformer` sequentially.
5. **Forward:** Send the transformed payload to the target downstream.
6. **Transform Response:** Run the downstream response through any `ResponseTransformer` before returning it to the user.

---

### Step 5 — Writing Transformations as Plugins

With this design, supporting translations in either direction (e.g., OpenAI to Anthropic, or Anthropic to OpenAI) is a matter of implementing the interfaces.

#### Example: Anthropic-to-OpenAI Transformer
If a native application expects an OpenAI-compatible target but you want to route to Anthropic, you create a plugin matching these interfaces:

```go
package plugins

import (
    "bytes"
    "encoding/json"
    "net/http"
    "github.com/yourname/llm-gateway/internal/engine"
)

type AnthropicToOpenAI struct{}

func (t *AnthropicToOpenAI) TransformRequest(req *http.Request, body []byte, ctx *engine.PipelineContext) (*http.Request, []byte, error) {
    // 1. Parse Anthropic request format (messages, system, max_tokens)
    // 2. Map fields into standard OpenAI Chat Completion JSON payload
    // 3. Update Request Headers (Authorization: Bearer <key>)
    // 4. Return new http.Request and modified body bytes
    return req, body, nil
}

func (t *AnthropicToOpenAI) TransformResponse(resp *http.Response, body []byte, ctx *engine.PipelineContext) ([]byte, error) {
    // 1. Parse Anthropic response JSON (or handle SSE stream)
    // 2. Map to OpenAI Chat Completion response structure
    // 3. Return updated bytes
    return body, nil
}
```

We register these in `internal/plugins/registry.go` so they are available dynamically based on the database rule config.

---

### Step 6 — Unified Admin Interface (REST + UDS CLI)

To allow external desktop software, CLI scripts, and the Web UI to work in harmony, the HTTP API routes are duplicated over the Unix Domain Socket when enabled.

#### Web & Local API Design:
- `GET /api/rules`: Retrieve all defined interception rules.
- `POST /api/rules`: Create a rule with an custom ordered pipeline configuration.
- `PUT /api/rules/{id}/switch`: Re-bind a rule to a different downstream.
- `GET /api/plugins`: List available pipeline plugins and their configuration schemas.

#### CLI Usage Example:
A local desktop app or script can switch the downstream model using the CLI tool:

```bash
# Swaps the target of rule 'default-gpt' to Claude Sonnet
llm-gateway rule switch default-gpt downstream-claude-3-7
```

Under the hood, this CLI command connects to the daemon's Unix socket, executes the transaction directly in the shared SQLite database, or makes a local loopback API call, keeping all interfaces in sync.

---

### Step 7 — Extensible Web UI

The single-page Web UI exposes the pipeline builder:

1. **Rule Editor**: Instead of a simple target dropdown, the interface displays an editable sequence of steps.
2. **Step Sorter**: Allows users to add, remove, and drag-and-drop transformation plugins (e.g., "Add custom header injection" -> "Add Anthropic to OpenAI converter").
3. **Save/Apply**: Writes the compiled pipeline list back to SQLite via the API.

---

## Verification Plan

| Component | Method | Verification Goal |
|---|---|---|
| **Pipeline Runner** | Unit tests | Verify that multiple `RequestTransformer` components run in sequence, correctly accumulating headers and body modifications. |
| **Bidirectional Translators** | Table-driven tests | Test translation accuracy from OpenAI input shapes to Anthropic formats, and vice-versa, for both static and streaming responses. |
| **CLI / Daemon Sync** | Integration test | Launch the daemon, trigger a downstream switch using the CLI binary, and assert that subsequent HTTP proxy requests routing through the updated rule reflect the change. |