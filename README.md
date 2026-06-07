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

### How Tresor Compares

| | **Tresor** | [**cc-switch**](https://github.com/farion1231/cc-switch) | [**LiteLLM**](https://github.com/BerriAI/litellm) |
|---|---|---|---|
| **What it is** | HTTP proxy + format translator for LLM API traffic | Desktop app that manages config files for AI coding tools (Claude Code, Codex, Gemini CLI, etc.) | Full-featured AI gateway platform (proxy + SDK + dashboard) |
| **Who it's for** | Anyone running apps that call LLM APIs — developers, power users, small teams | Developers who use multiple AI coding assistants and want a unified config UI | ML Platform teams managing LLM access at enterprise scale |
| **Size** | ~1,000 lines of Go | ~40,000 lines (Rust + TypeScript) | ~200,000+ lines (Python + TypeScript) |
| **Runtime** | Single compiled binary, zero dependencies | Tauri desktop app (requires install) | Python runtime + optional Docker deploy + database |
| **Setup time** | 5 minutes: build, write YAML config, start | Install app, import presets, restart CLI tools | `pip install`, configure, optionally deploy as a service |
| **Format translation** | OpenAI ↔ Anthropic (built-in) | Proxy mode with format conversion for coding tools | 100+ providers, auto-detects and translates formats |
| **Hot-switching** | Yes — switch backends instantly via web UI or CLI without restarting anything | Partial — requires terminal restart for most tools (except Claude Code) | Via virtual key reconfiguration or dashboard |
| **Cost tracking** | No (not the focus) | No | Yes — spend limits, usage dashboards, billing alerts |
| **Guardrails / safety** | No | No | Yes — input/output filtering, PII detection |
| **Self-hosted** | Yes — runs on any machine, no cloud needed | Local desktop app only | Self-hosted or SaaS (enterprise features behind paid tier) |

**Tresor picks a different spot:** smaller than LiteLLM, more flexible than cc-switch. It's a focused tool for one job — routing and transforming LLM traffic — done right with minimal overhead.

- **Don't need** enterprise features (billing, guardrails, 100 providers)? Tresor is lighter and simpler than LiteLLM.
- **Don't want** to restart your tools after switching providers? Tresor hot-switches without any restart.
- **Want something that runs anywhere?** Single binary, no runtime, no install — just `./tresor run`.

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
