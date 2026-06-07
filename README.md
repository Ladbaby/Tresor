# Tresor

> **A lightweight LLM proxy for developers who want control, not complexity.**

<div align="center">

[![Go Version](https://img.shields.io/badge/Go-1.26-violet)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-blue)](LICENSE)

</div>

---

## Why Tresor?

The LLM landscape moves fast. Today you're on OpenAI, tomorrow you want to try Anthropic or a local model — but your application is locked into one provider's API format. Switching means rewriting code, updating SDKs, and potentially breaking things in production.

Tresor solves this by sitting between your apps and LLM providers as a transparent proxy. **Your application never knows the backend changed.**

### The Problem: Switching Providers at Scale

Imagine you have agents on three machines, all calling OpenAI. You want to switch them to Anthropic.

| Tool | What Happens |
|------|-------------|
| **cc-switch** | Install it on every machine, then switch each one individually. Restart most tools for changes to take effect. |
| **LiteLLM** | Deploy the proxy, define model aliases, and reconfigure downstream mappings — a heavy setup for a simple switch. |
| **Tresor** | Run it once on any reachable machine. Point all agents to it. Switch providers with one click in the web UI — every agent sees the change instantly. |

**One proxy. One config. One click.** That's Tresor.

---

## What Tresor Does

Tresor is a single binary with two modes:

| Mode | What It Does |
|------|-------------|
| **Daemon** | Long-running HTTP proxy + admin REST API + embedded web UI |
| **CLI** | Command-line client for managing the daemon |

```
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│   Your App   │────▶│    Tresor    │────▶│  LLM Provider│
│              │     │    (proxy)   │     │  (OpenAI,    │
│              │◀────│              │◀────│  Anthropic..) │
└──────────────┘     └──────────────┘     └──────────────┘
                         │
                         ├── Admin REST API
                         ├── Embedded Web UI
                         └── CLI Commands
```

### Key Capabilities

- **Protocol Translation** — Convert between OpenAI and Anthropic API formats transparently. Your app sends an OpenAI request; Tresor forwards it to Anthropic and converts the response back. No code changes needed.
- **Hot-Switch Models** — Map one model name to any backend model and switch on the fly. Your app requests `gpt-4o`; Tresor can route it to Claude Sonnet, Opus, or keep it on GPT-4o — all without restarting.
- **Per-Path Routing** — Route different API paths (and models) to different providers based on configurable rules.
- **Plugin Pipeline** — Chain transformation plugins per rule (header injection, format conversion, and more).
- **Embedded Web UI** — Manage everything from a browser dashboard. No separate frontend deployment.
- **Single Config File** — All settings in one portable YAML file. Changes via the web UI write back automatically.

---

## Getting Started

```bash
# Build (requires Go 1.26+)
go build -o tresor.exe .

# Copy the example config and add your API keys
cp config.example.yaml config.yaml

# Start the daemon
./tresor.exe run --config config.yaml

# Open http://127.0.0.1:8080 in your browser for the web UI
```

That's it. Point your application to `http://127.0.0.1:8080` and Tresor handles the rest.

---

## Documentation

Full documentation is available at **[ladbaby.github.io/tresor-docs/](https://ladbaby.github.io/tresor-docs/)**:

### For Users
- [Introduction](https://ladbaby.github.io/tresor-docs/docs/user/intro) — overview and architecture
- [Installation & Quick Start](https://ladbaby.github.io/tresor-docs/docs/user/getting-started/installation) — build, configure, run
- [Configuration Basics](https://ladbaby.github.io/tresor-docs/docs/user/configuration/basics) — YAML config file reference
- [Downstreams](https://ladbaby.github.io/tresor-docs/docs/user/configuration/downstreams) — configure LLM provider endpoints
- [Rules](https://ladbaby.github.io/tresor-docs/docs/user/configuration/rules) — define routing rules with transform pipelines
- [Model Aliases](https://ladbaby.github.io/tresor-docs/docs/user/configuration/aliases) — map model names and hot-switch backends
- [Proxy Modes](https://ladbaby.github.io/tresor-docs/docs/user/configuration/proxy-modes) — outbound proxy configuration
- [Web UI Guide](https://ladbaby.github.io/tresor-docs/docs/user/web-ui) — manage everything from the browser
- [CLI Reference](https://ladbaby.github.io/tresor-docs/docs/user/cli-reference) — all command-line commands

### Use Cases
- [Transparent Provider Switching](https://ladbaby.github.io/tresor-docs/docs/user/use-cases/provider-switching) — route OpenAI-format traffic to Anthropic
- [Model Aliasing](https://ladbaby.github.io/tresor-docs/docs/user/use-cases/model-aliasing) — hot-switch between backends
- [A/B Testing Backends](https://ladbaby.github.io/tresor-docs/docs/user/use-cases/ab-testing) — compare providers side by side

### For Developers
- [Architecture](https://ladbaby.github.io/tresor-docs/docs/dev/architecture) — codebase structure, request flow, data layer
- [Plugin System](https://ladbaby.github.io/tresor-docs/docs/dev/plugin-system) — building custom transformers
- [Testing](https://ladbaby.github.io/tresor-docs/docs/dev/testing) — test strategy and coverage
- [Contributing](https://ladbaby.github.io/tresor-docs/docs/dev/contributing) — how to contribute to Tresor

---

## License

MIT
