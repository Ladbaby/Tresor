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
admin_password: "secret"        # Optional: admin password for cookie-based session auth
proxy_api_keys:                 # Optional: require clients to authenticate proxy requests
  - sk-proxy-123
proxy_mode: "auto"              # Outbound proxy: auto, env, windows, none
default_tab: "downstreams"      # Dashboard tab on load (values: downstreams, aliases, rules, settings, about)
log_level: "info"               # Request logging verbosity: debug, info, warn, error

downstreams:                    # LLM provider endpoints
  - id: my-provider
    name: My Provider
    base_url: https://api.example.com
    api_key: sk-...
    api_formats: [openai]       # API format(s): openai, anthropic, openai_responses, gemini
    output_model_ids:           # Models this downstream can handle (required for forwarding)
      - gpt-4o

rules:                          # Routing rules (OPTIONAL — only for transforms)
  - id: my-rule
    name: Chat Rule
    pattern_path: /v1/chat/completions
    pattern_model: gpt-4o       # Optional model filter
    match_format: [openai]      # Match input request format
    match_downstream_format: [anthropic]  # Match downstream API format
    match_downstreams: [my-provider]      # Match specific downstreams
    pipeline_config:            # Transformation steps
      - plugin_id: openai2anthropic
    is_enabled: true

aliases:                        # Model name mappings (hot-switchable, ordered by array position)
  - input_model_id: gpt-4o
    is_regex: false             # Treat input_model_id as regex (group-level, optional)
    options:
      - id: alias-gpt4o-anthropic
        downstream_id: my-provider
        output_model_id: claude-sonnet-4-20250514
```


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
./tresor.exe rule create [name] [pattern_path] [--match-format openai,anthropic]
./tresor.exe version

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
| `cmd/` | Cobra CLI commands (`root.go`, `run.go`, `rule.go`, `version.go`) |
| `internal/config/` | YAML configuration loader with auto-detect path resolution |
| `internal/store/` | SQLite data layer: `store.go` (schema/migrations/upsert), `rule.go` (CRUD + matching), `downstream.go` (CRUD + model IDs), `alias.go` (alias CRUD + grouping) |
| `internal/engine/` | Core gateway handler: `engine.go` (proxy, model resolution, auto-translation, model listing), `pipeline.go` (transformer orchestration), `types.go` (interfaces), `logger.go` (request logging + SSE) |
| `internal/plugins/` | Plugin registry and built-in transformers: `registry.go`, `custom_header.go`, `openai2anthropic.go`, `anthropic2openai.go`, `fix_anthropic_images.go`, `openai2responses.go`, `responses2openai.go`, `responses2anthropic.go`, `anthropic2responses.go` |
| `internal/api/` | Admin REST API: `router.go` (routing + auth), `rules.go` (rule endpoints), `downstreams.go` (downstream endpoints + plugins list + model fetch), `aliases.go` (alias endpoints + reorder), `logs.go` (log REST + SSE + log level), `config.go` (runtime config), `embed.go` (embedded web UI) |
| `internal/middleware/` | Cookie-based session auth middleware for admin API (with rate limiting and multi-session support) |
| `internal/proxy/` | Outbound proxy mode configuration (auto, env, windows, none) + URL validation |
| `internal/api/web/` | Embedded SPA: `index.html`, `style.css`, `app.js` |

### Data Flow (Gateway Request)

1. Client sends request to Tresor gateway → `HandleProxy` reads body, extracts model
2. **Proxy Auth** (if `proxy_api_keys` configured): validate `Authorization: Bearer` or `x-api-key` header, strip auth from forwarded request
3. **Model Resolution** (the forwarding gate):
   - Try active exact alias via `FindActiveAlias(model)` — first tries exact `input_model_id` match
   - If no exact match, try active regex aliases (`is_regex: true`) — pattern-matched against the model name
   - If no alias, try direct resolution via `FindDownstreamByOutputModel(model)` — matches against downstream `output_model_ids`
   - If neither resolves a downstream → return 404 "unknown model"
4. **Rule Matching** (for pipeline transforms):
   - `FindMatchingRules(path, model, inputFormat, dsID, dsFormats)` queries SQLite for all matching enabled rules (priority: exact path+model > exact path > wildcard `*`)
   - Rules are filtered by format criteria: `match_format`, `match_downstream_format`, `match_downstreams`
   - Multiple rules can match; their pipelines are concatenated in priority order
   - Rules NEVER override the downstream — they only contribute transformers
5. **Auto-translation**: If input format differs from downstream format, automatically insert the appropriate format transformer (e.g., OpenAI → Anthropic) before any rule transformers
6. Parse `pipeline_config` JSON → instantiate plugin transformers via registry
7. Execute request transformers sequentially (body + headers may change)
8. Forward transformed request to downstream server
9. Execute response transformers sequentially on the downstream's response
10. Return final response to client

### Plugin System

Plugins implement Go interfaces: `RequestTransformer` (modifies outgoing requests), `ResponseTransformer` (modifies incoming responses), and `StreamResponseTransformer` (transforms SSE events). Eight built-in plugins exist:

- **custom_header**: Injects arbitrary HTTP headers into forwarded requests
- **openai2anthropic**: Converts OpenAI Chat Completion format to Anthropic Messages format (and vice versa for responses)
- **fix_anthropic_images**: Extracts image parts from tool_result.content[] and promotes them to top-level message content for Anthropic-compatible backends
- **anthropic2openai**: Converts Anthropic Messages format to OpenAI Chat Completion format (and vice versa for responses)
- **responses2openai**: Converts OpenAI Responses API format to Chat Completions format (and vice versa)
- **responses2anthropic**: Converts OpenAI Responses API format to Anthropic Messages format (and vice versa)
- **openai2responses**: Converts OpenAI Chat Completion format to Responses API format
- **anthropic2responses**: Converts Anthropic Messages format to Responses API format

Pipeline config is stored as JSON in the `rules.pipeline_config` column: `[{"plugin_id": "...", "config": {...}}]`

**Auto-translation:** When input format (detected from path) differs from downstream format, the engine automatically inserts the appropriate transformer without any rule needed.

### Admin API Endpoints

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/auth/status` | Check if auth is enabled (public) |
| POST | `/api/auth/login` | Log in with password, get session cookie (public) |
| POST | `/api/auth/logout` | Clear current session cookie and token (public) |
| GET | `/api/health` | Health check (public) |
| GET | `/api/version` | Print version and build info (public) |
| GET/PUT | `/api/log_level` | Get/set request logging verbosity |
| GET/PUT | `/api/config` | Get/set runtime config (proxy_mode, proxy_api_keys, admin_password, default_tab, log_level) |
| POST | `/api/fetch-models` | Fetch available models from a provider (body: base_url + api_key) |
| GET | `/api/rules` | List all rules |
| POST | `/api/rules` | Create a new rule |
| GET | `/api/rules/{id}` | Get single rule |
| PUT | `/api/rules/{id}` | Partial update (enabled state or full rule fields) |
| DELETE | `/api/rules/{id}` | Delete a rule |
| GET | `/api/downstreams` | List all downstreams |
| POST | `/api/downstreams` | Create a new downstream |
| GET | `/api/downstreams/{id}` | Get single downstream |
| PUT | `/api/downstreams/{id}` | Update downstream |
| DELETE | `/api/downstreams/{id}` | Delete downstream (removes from rule match_downstreams, deletes referencing aliases) |
| POST | `/api/downstreams/{id}/models` | Add a model ID to downstream |
| DELETE | `/api/downstreams/{id}/models/{model_id}` | Remove a model ID from downstream |
| POST | `/api/downstreams/{id}/fetch-models` | Auto-discover models from provider |
| GET | `/api/plugins` | List registered plugins |
| GET | `/api/aliases` | List all alias groups (grouped view, ordered by group_order) |
| POST | `/api/aliases` | Create a new alias option |
| POST | `/api/aliases/reorder` | Reorder groups: `{"order": ["gpt-4o", "claude-sonnet"]}` |
| GET | `/api/aliases/{id}` | Get single alias |
| PUT | `/api/aliases/{id}` | Update alias fields |
| DELETE | `/api/aliases/{id}` | Delete an alias option |
| PUT | `/api/aliases/{id}/activate` | Hot-switch: activate this alias for its group |
| DELETE | `/api/aliases/group/{inputModelId}` | Delete all aliases for an input model |
| GET | `/api/logs` | Get recent log entries (newest first) |
| GET | `/api/logs/stream` | SSE stream of log entries |
| GET | `/v1/models` | Aggregated model listing (OpenAI format, gateway path) |


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
