# Tresor

> **LLM traffic interception and routing engine** — a single Go binary that sits between your applications and LLM providers, intercepting API calls, routing them to different backends, and transforming request/response formats on the fly.

<div align="center">

[![Go Version](https://img.shields.io/badge/Go-1.26-violet)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue)](LICENSE)

</div>

---

## At a Glance

Tresor has two faces in one binary:

| Mode | What It Does |
|------|-------------|
| **Daemon** | Long-running HTTP proxy + admin REST API + embedded web UI |
| **CLI** | Command-line client for managing rules, downstreams, and aliases |

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│   Your App   │────▶│  Tresor │────▶│  LLM Provider │
│              │     │  (proxy)     │     │  (OpenAI,    │
│              │◀────│              │◀────│  Anthropic..) │
└──────────────┘     └──────────────┘     └──────────────┘
                         │
                         ├── Admin REST API
                         ├── Embedded Web UI
                         └── CLI Commands
```

## Core Features

- **Protocol Translation** — Convert between OpenAI and Anthropic API formats transparently. Send an OpenAI request, receive it from an Anthropic backend, without changing a line of application code.
- **Per-Path Routing** — Route different API paths (and models) to different downstream providers based on configurable rules.
- **Model Aliasing** — Map one model name to another at the proxy layer. Your app sends `gpt-4o`, Tresor forwards as `claude-sonnet-4`. Hot-switch between backends without restarting.
- **Plugin Pipeline** — Chain multiple transformation plugins per rule (header injection, format conversion, image extraction, and more).
- **Embedded Web UI** — Manage everything from a browser-based dashboard served by the daemon itself. No separate frontend needed.
- **Single Config File** — All downstreams, rules, aliases, and settings live in one portable YAML file. Changes via API or UI write back automatically.
- **Zero External Dependencies** — Pure Go SQLite (no CGO), no external web framework, single compiled binary.

---

## Quick Start

### 1. Build

```bash
go build -o tresor.exe .
```

### 2. Create a Config File

```bash
cp config.example.yaml config.yaml
# Edit config.yaml with your provider API keys and endpoints
```

### 3. Start the Daemon

```bash
./tresor.exe run --config config.yaml
```

The daemon starts listening on the configured `bind_addr` (default `127.0.0.1:8080`). Point your application's LLM API calls to this address.

### 4. Open the Web UI

Navigate to `http://127.0.0.1:8080` in your browser to manage downstreams, rules, aliases, and settings visually.

---

## Configuration

Tresor is configured via a single YAML file. Auto-detected in this order:

1. `--config <path>` flag
2. `./config.yaml` in the current directory
3. `~/.tresor.yaml` in your home directory

### Complete Example

```yaml
# Server
bind_addr: "127.0.0.1:8080"
db_path: "./tresor.db"
admin_password: "secret"          # Bearer token for admin API / web UI
proxy_mode: auto                  # auto | env | windows | none

# LLM provider endpoints
downstreams:
  - id: openai
    name: OpenAI
    base_url: https://api.openai.com/v1
    api_key: sk-...

  - id: anthropic
    name: Anthropic
    base_url: https://api.anthropic.com/v1
    api_key: sk-ant-...

# Routing rules — match incoming requests and route to a downstream
rules:
  - id: chat-rule
    name: Chat Completions
    pattern_path: /v1/chat/completions
    active_downstream: openai
    pipeline_config:
      - plugin_id: openai2anthropic
    is_enabled: true

# Model aliases — rewrite model names on the fly
aliases:
  - id: alias-gpt4o-sonnet
    input_model_id: gpt-4o
    downstream_id: anthropic
    output_model_id: claude-sonnet-4-20250514
    is_active: true
```

On startup, YAML data is **upserted** into SQLite — existing rows are updated, new rows are inserted, and rows that exist only in the database are preserved. This means you can manage everything from the web UI or CLI, and your changes persist alongside the config file.

---

## Architecture

### Request Flow

```
Client Request
      │
      ▼
┌───────────────┐
│ Model Resolve  │  ← Extract model name from request body
└──────┬────────┘
       │
       ├─ Active alias? → Use alias downstream, rewrite model name
       │
       └─ No alias? → Find downstream by output_model_ids
                      (if not found → 404 unknown model)
       │
       ▼
┌───────────────┐
│ Optional Rule  │  ← Match rule for pipeline transforms only
└──────┬────────┘      (rule can override downstream when no alias)
       │
       ▼
┌─────────────┐
│   Pipeline   │  ← Run request transformers (format conversion, headers...)
└──────┬──────┘
       ▼
┌─────────────┐
│  Downstream  │  ← Forward transformed request to LLM provider
└──────┬──────┘
       ▼
┌─────────────┐
│   Pipeline   │  ← Run response transformers (convert format back)
└──────┬──────┘
       ▼
   Client Response
```

**Key change:** Model resolution is the forwarding gate. Rules are optional policy layers for transforms only — they are no longer required for normal proxy forwarding.

### Rule Matching Priority

When a matching rule exists, Tresor selects the best one:

1. **Exact path + exact model** (e.g., `/v1/chat/completions` + `gpt-4o`)
2. **Exact path, no model filter** (e.g., `/v1/chat/completions`)
3. **Wildcard** (`*` matches everything)

If no rule matches, forwarding still proceeds with an empty pipeline.

### Plugin System

Each rule can define a pipeline of transformers that modify the request and/or response before forwarding:

| Plugin | Description |
|--------|-------------|
| `openai2anthropic` | Convert OpenAI Chat Completion format → Anthropic Messages format (and vice versa for responses). Handles both JSON and SSE streaming. |
| `anthropic2openai` | The reverse: Anthropic Messages → OpenAI Chat Completion format. |
| `custom_header` | Inject arbitrary HTTP headers into forwarded requests. |
| `fix_anthropic_images` | Extract image parts from nested `tool_result.content[]` arrays and promote them to top-level message content. For llama.cpp-compatible backends. |

Pipeline config is stored as JSON per rule:

```json
[
  {"plugin_id": "custom_header", "config": {"headers": {"X-Custom": "value"}}},
  {"plugin_id": "openai2anthropic"}
]
```

---

## CLI Reference

All commands connect to a running daemon. The `--config` flag tells the CLI which daemon to talk to.

### Daemon

```bash
# Start the daemon
./tresor.exe run --config config.yaml
```

### Rules

```bash
# List all routing rules
./tresor.exe rule list

# Create a new rule
./tresor.exe rule create "My Rule" "/v1/chat/completions" openai

# Switch a rule's active downstream
./tresor.exe rule switch chat-rule anthropic
```

### Aliases

```bash
# List all alias groups
./tresor.exe alias list

# Create a new alias
./tresor.exe alias create gpt-4o anthropic claude-sonnet-4-20250514

# Activate an alias (deactivates siblings in the same group)
./tresor.exe alias activate alias-gpt4o-sonnet

# Delete an alias
./tresor.exe alias delete alias-gpt4o-sonnet
```

---

## Admin API Reference

All admin endpoints are protected by optional Bearer-token auth (set via `admin_password` in config).

### Rules

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/rules` | List all rules |
| `POST` | `/api/rules` | Create a new rule |
| `GET` | `/api/rules/{id}` | Get a single rule |
| `PUT` | `/api/rules/{id}` | Update enabled state |
| `DELETE` | `/api/rules/{id}` | Delete a rule |
| `PUT` | `/api/rules/{id}/switch` | Change active downstream |

### Downstreams

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/downstreams` | List all downstreams |
| `POST` | `/api/downstreams` | Create a new downstream |
| `GET` | `/api/downstreams/{id}` | Get a single downstream |
| `PUT` | `/api/downstreams/{id}` | Update a downstream |
| `DELETE` | `/api/downstreams/{id}` | Delete a downstream |
| `POST` | `/api/downstreams/{id}/models` | Add an output model ID |
| `DELETE` | `/api/downstreams/{id}/models/{model_id}` | Remove a model |
| `POST` | `/api/downstreams/{id}/fetch-models` | Auto-discover models from provider |

### Aliases

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/aliases` | List all alias groups |
| `POST` | `/api/aliases` | Create a new alias option |
| `GET` | `/api/aliases/{id}` | Get a single alias |
| `PUT` | `/api/aliases/{id}` | Update alias fields |
| `DELETE` | `/api/aliases/{id}` | Delete an alias option |
| `PUT` | `/api/aliases/{id}/activate` | Activate this alias for its group |
| `DELETE` | `/api/aliases/group/{inputModelId}` | Delete entire alias group |

### Other

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/health` | Health check |
| `GET` | `/api/plugins` | List registered plugins with config schemas |
| `GET` | `/api/config` | Get current proxy mode |
| `PUT` | `/api/config` | Update proxy mode at runtime |

---

## Use Cases

### Transparent Provider Switching

Your application talks OpenAI. You want to route traffic to Anthropic without code changes:

1. Define an Anthropic downstream in config
2. Create a rule matching `/v1/chat/completions` with the `openai2anthropic` plugin
3. Tresor converts your OpenAI requests to Anthropic format, forwards them, and converts the response back

### A/B Testing Backends

Route the same endpoint to different providers based on model:

```yaml
rules:
  - id: gpt4o-rule
    pattern_path: /v1/chat/completions
    pattern_model: gpt-4o
    active_downstream: openai

  - id: sonnet-rule
    pattern_path: /v1/chat/completions
    pattern_model: claude-sonnet-4
    active_downstream: anthropic
```

### Hot-Switching Models

Define multiple aliases for the same input model and activate whichever you want — no restart needed:

```yaml
aliases:
  - id: alias-gpt4o-sonnet
    input_model_id: gpt-4o
    downstream_id: anthropic
    output_model_id: claude-sonnet-4-20250514
    is_active: true        # ← currently active

  - id: alias-gpt4o-opus
    input_model_id: gpt-4o
    downstream_id: anthropic
    output_model_id: claude-opus-4-20250514
    is_active: false       # ← switch with one API call
```

---

## Project Structure

```
├── cmd/                        # CLI commands (Cobra)
│   ├── root.go                # Root command, --config flag
│   ├── run.go                 # Daemon startup
│   └── rule.go                # Rule & alias subcommands
├── internal/
│   ├── config/                # YAML config loader
│   ├── engine/                # Core proxy handler + pipeline
│   ├── middleware/            # Bearer-token auth
│   ├── plugins/               # Plugin registry + built-in transformers
│   ├── proxy/                 # System proxy resolution (Windows/env)
│   └── store/                 # SQLite data layer + YAML write-back
├── e2e/                       # End-to-end integration tests
├── config.example.yaml        # Complete configuration example
└── go.mod
```

---

## Development

```bash
# Build
go build -o tresor.exe .

# Run unit tests
go test ./...

# Run e2e smoke tests (requires a running daemon)
go test -tags=integration ./e2e/...
```

---

## Key Design Decisions

- **Single YAML config** — all settings, downstreams, rules, and aliases in one portable file
- **SQLite** (`modernc.org/sqlite`) — pure Go, no CGO dependency, WAL mode enabled
- **Upsert on startup** — YAML data merges into SQLite; DB-only rows are preserved
- **No external web framework** — uses `net/http` ServeMux directly
- **Single binary** — daemon and CLI are the same executable, dispatched by subcommand
- **Web UI embedded at compile time** — via Go's `embed.FS`, no separate frontend deployment
