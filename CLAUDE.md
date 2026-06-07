# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with the Tresor codebase.

## Project Overview

Tresor is a single-binary LLM gateway for switching providers at scale with one click. It sits between client applications and LLM providers (OpenAI, Anthropic, etc.), transforming requests and responses via a configurable plugin pipeline. The binary serves dual roles: a **daemon** (long-running HTTP gateway + admin API) and a **CLI client** for controlling the daemon.

See [PLAN.md](PLAN.md) for the full architectural design.

## Configuration

Tresor is configured entirely via a single YAML config file. See [config.example.yaml](config.example.yaml) for a complete example.

### Config File Locations (auto-detected in order)

1. Path specified by `--config` flag
2. `./config.yaml` in current directory
3. `$HOME/.tresor.yaml`

To get started: `cp config.example.yaml config.yaml`, then edit as needed.

### YAML Structure

```yaml
bind_addr: "127.0.0.1:11510"         # TCP address for gateway
socket_path: "/tmp/tresor.sock"  # Optional Unix socket (admin only)
db_path: "./tresor.db"          # SQLite database path
admin_password: "secret"              # Optional Bearer token auth

downstreams:                          # LLM provider endpoints
  - id: my-provider
    name: My Provider
    base_url: https://api.example.com/v1
    api_key: sk-...
    output_model_ids:                 # Models this downstream can handle (required for forwarding)
      - gpt-4o

rules:                                # Routing rules (OPTIONAL — only for transforms)
  - id: my-rule
    name: Chat Rule
    pattern_path: /v1/chat/completions
    pattern_model: gpt-4o             # Optional model filter
    active_downstream: my-provider
    pipeline_config:                  # Transformation steps
      - plugin_id: openai2anthropic
    is_enabled: true

aliases:                              # Model name mappings (hot-switchable)
  - id: alias-gpt4o-anthropic
    input_model_id: gpt-4o
    downstream_id: my-provider
    output_model_id: claude-sonnet-4-20250514
    is_active: true
```

On startup, the YAML data is **upserted** into SQLite: existing rows (matched by ID) are updated, new rows are inserted. Rows only in the DB are preserved. If no downstreams/rules/aliases are defined in YAML, built-in defaults are seeded.

**Rules are optional.** Forwarding works if `downstreams` + `output_model_ids` are configured. Rules only add plugin pipelines and can optionally override the resolved downstream when no alias exists.

## Build and Run

```bash
# Build
go build -o tresor.exe .

# Run unit tests (all packages)
go test ./...

# Run a single test file
go test ./internal/store/...
go test ./internal/engine/...
go test ./internal/plugins/...

# Run the daemon (uses config.yaml from current dir or $HOME/.tresor.yaml)
./tresor.exe run

# Daemon with explicit config path
./tresor.exe run --config /path/to/config.yaml

# CLI commands (connect to running daemon via config)
./tresor.exe rule list
./tresor.exe rule switch <rule-id> <downstream-id>
./tresor.exe rule create <name> <pattern_path> <downstream_id>

# Alias CLI commands
./tresor.exe alias list
./tresor.exe alias activate <alias-id>
./tresor.exe alias create <input-model-id> <downstream-id> <output-model-id>
./tresor.exe alias delete <alias-id>

# E2E smoke test (requires //go:build integration tag)
go test -tags=integration ./e2e/...
```

## Architecture

The codebase follows a clean layered structure with three core concerns:

### Package Layout

| Package | Purpose |
|---|---|
| `cmd/` | Cobra CLI commands (`root.go`, `run.go`, `rule.go`) |
| `internal/config/` | YAML configuration loader with auto-detect path resolution |
| `internal/store/` | SQLite data layer: `store.go` (schema/migrations/upsert), `rule.go` (CRUD + matching), `downstream.go` (CRUD), `alias.go` (alias CRUD + grouping) |
| `internal/engine/` | Core gateway handler: `engine.go` (gateway forwarding + alias override), `pipeline.go` (transformer orchestration), `types.go` (interfaces) |
| `internal/plugins/` | Plugin registry and built-in transformers: `registry.go`, `custom_header.go`, `openai2anthropic.go`, `anthropic2openai.go` |
| `internal/api/` | Admin REST API: `router.go` (routing), `rules.go` (rule endpoints), `downstreams.go` (downstream endpoints + plugins list), `aliases.go` (alias endpoints), `embed.go` (embedded web UI) |
| `internal/middleware/` | Bearer-token auth middleware for admin API |
| `internal/api/web/` | Embedded SPA: `index.html`, `style.css`, `app.js` |

### Data Flow (Gateway Request)

1. Client sends request to Tresor gateway → `HandleProxy` reads body, extracts model name
2. **Model Resolution** (the forwarding gate):
   - Try active alias via `FindActiveAlias(model)` — if found, use alias's downstream and rewrite the model name in the body
   - If no alias, try direct resolution via `FindDownstreamByOutputModel(model)` — matches against downstream `output_model_ids`
   - If neither resolves a downstream → return 404 "unknown model"
3. **Optional Rule Lookup** (for pipeline transforms only):
   - `FindMatchingRule(path, model)` queries SQLite for the best matching enabled rule (priority: exact path+model > exact path > wildcard `*`)
   - If a rule matched AND no alias exists, let the rule override the downstream
   - Use the rule's `pipeline_config` if present, otherwise use an empty pipeline (`[]`)
4. Parse `pipeline_config` JSON → instantiate plugin transformers via registry
5. Execute request transformers sequentially (body + headers may change)
6. Forward transformed request to downstream server
7. Execute response transformers sequentially on the downstream's response
8. Return final response to client

### Plugin System

Plugins implement Go interfaces: `RequestTransformer` (modifies outgoing requests) and `ResponseTransformer` (modifies incoming responses). Three built-in plugins exist:

- **custom_header**: Injects arbitrary HTTP headers into forwarded requests
- **openai2anthropic**: Converts OpenAI Chat Completion format to Anthropic Messages format (and vice versa for responses)
- **anthropic2openai**: Converts Anthropic Messages format to OpenAI Chat Completion format (and vice versa for responses)

Pipeline config is stored as JSON in the `rules.pipeline_config` column: `[{"plugin_id": "...", "config": {...}}]`

### Admin API Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/health` | Health check |
| GET | `/api/rules` | List all rules |
| POST | `/api/rules` | Create a new rule |
| GET | `/api/rules/{id}` | Get single rule |
| PUT | `/api/rules/{id}` | Partial update (enabled state only) |
| DELETE | `/api/rules/{id}` | Delete a rule |
| PUT | `/api/rules/{id}/switch` | Change rule's active downstream |
| GET | `/api/downstreams` | List all downstreams |
| POST | `/api/downstreams` | Create a new downstream |
| GET | `/api/downstreams/{id}` | Get single downstream |
| PUT | `/api/downstreams/{id}` | Update downstream |
| DELETE | `/api/downstreams/{id}` | Delete downstream (nullifies referencing rules) |
| GET | `/api/plugins` | List registered plugins |
| GET | `/api/aliases` | List all alias groups (grouped view) |
| POST | `/api/aliases` | Create a new alias option |
| GET | `/api/aliases/{id}` | Get single alias |
| PUT | `/api/aliases/{id}` | Update alias fields |
| DELETE | `/api/aliases/{id}` | Delete an alias option |
| PUT | `/api/aliases/{id}/activate` | Hot-switch: activate this alias for its group |

Admin API is protected by optional Bearer-token auth (configured via `admin_password` in YAML). Web UI is embedded via `//go:embed web/*`.

### Listening Modes

The daemon can listen on TCP (gateway + admin API + web UI) and/or Unix Domain Socket (admin API only). CLI commands connect to whichever interface is configured in the YAML.

## Key Design Decisions

- Single YAML config — all settings, downstreams, rules, and aliases in one portable file
- SQLite (modernc.org/sqlite) — pure Go, no CGO dependency, WAL mode enabled
- Upsert on startup — YAML data merges into SQLite; DB-only rows preserved
- No external web framework — uses `net/http` ServeMux directly for routing
- Single binary — daemon and CLI are the same executable, dispatched by subcommand
- Web UI is embedded at compile time via Go's `embed.FS`

## Browser Automation

Use `agent-browser` for web automation. Run `agent-browser --help` for all commands.

Core workflow:

1. `agent-browser open <url>` - Navigate to page
2. `agent-browser snapshot -i` - Get interactive elements with refs (@e1, @e2)
3. `agent-browser click @e1` / `fill @e2 "text"` - Interact using refs
4. Re-snapshot after page changes

## Testing Mandate

**Always verify code changes end-to-end before declaring work complete.** Follow this sequence:

1. **Build**: `go build ./...` — ensure everything compiles
2. **Unit tests**: `go test ./...` — all package tests must pass (clear the cache with `go clean -testcache` first if changes touch tested packages)
3. **E2E smoke test**: Start the daemon with a YAML config (`./tresor.exe run --config <path>`) in a background process, then run `go test -tags=integration ./e2e/...` against it
4. **Browser verification**: Use `agent-browser` to verify web UI changes visually — navigate to each affected tab, perform core interactions (create, edit, delete, hot-switch), and confirm the UI reflects the expected state

Skipping step 3 or 4 is not acceptable for any change that affects API behavior, gateway logic, or the web UI. Unit tests alone cannot catch rendering bugs, JavaScript errors, or integration issues between layers.
