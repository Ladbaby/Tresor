# Plan: Rename TrafficShim → Tresor

## Scope

Completely rename the program and project from "TrafficShim" to "Tresor". This touches 6 categories of changes across ~40+ files.

## Audit of Current References

All occurrences of "trafficshim" / "TrafficShim" found in the codebase:

### Category 1: Go Module Path (go.mod + all import paths)
- `go.mod` line 1: `module trafficshim` → `module tresor`
- **Every `.go` file** with `"trafficshim/internal/..."` imports → `"tresor/internal/..."`
  - Affected files: `cmd/root.go`, `cmd/rule.go`, `cmd/run.go`, `main.go`, all files under `internal/api/`, `internal/config/`, `internal/engine/`, `internal/middleware/`, `internal/plugins/`, `internal/proxy/`, `internal/store/`, and `e2e/e2e_test.go`
  - ~30+ import statements across ~25 files

### Category 2: CLI Binary & Command Names
- `cmd/root.go` line 13: `Use: "trafficshim"` → `Use: "tresor"`
- `cmd/root.go` line 15-17: Long description mentions "TrafficShim" → "Tresor"
- `cmd/root.go` line 28: flag help text `$HOME/.trafficshim.yaml` → `$HOME/.tresor.yaml`
- `cmd/run.go` line 23-30: Short/Long descriptions, example commands (`trafficshim run`) → `tresor run`
- `cmd/run.go` line 45: flag help text `.trafficshim.yaml` → `.tresor.yaml`
- `cmd/run.go` lines 94, 116: log messages "TrafficShim proxy listening" / "TrafficShim admin socket" → "Tresor ..."

### Category 3: Configuration File Paths & Defaults
- `internal/config/config.go`:
  - Line 11: comment "for TrafficShim" → "for Tresor"
  - Lines 87, 92, 150, 162: fallback path `.trafficshim.yaml` → `.tresor.yaml`
  - Lines 189, 191: default DB path `./trafficshim.db` / `.trafficshim.db` → `./tresor.db` / `.tresor.db`
- `config.example.yaml`:
  - Line 1: comment "# TrafficShim Configuration" → "# Tresor Configuration"
  - Lines 6-7: example values `/tmp/trafficshim.sock` → `/tmp/tresor.sock`, `./trafficshim.db` → `./tresor.db`

### Category 4: Web UI (embedded SPA)
- `internal/api/web/index.html`:
  - Line 6: `<title>TrafficShim — LLM Gateway</title>` → `<title>Tresor — LLM Gateway</title>`
  - Lines 15, 32: `<span class="brand-name">TrafficShim</span>` → `<span class="brand-name">Tresor</span>` (sidebar + mobile nav)
- `internal/api/web/app.js`:
  - Line 1: comment "TrafficShim Web UI" → "Tresor Web UI"
  - Lines 49, 80, 119, 146: sessionStorage key `trafficshim_token` → `tresor_token`
- `internal/api/web/style.css`:
  - Line 2: comment "TrafficShim Web UI" → "Tresor Web UI"

### Category 5: Documentation
- `README.md`: ~20+ occurrences of "TrafficShim" and "trafficshim.exe" throughout
- `CLAUDE.md`: ~30+ occurrences in descriptions, command examples, data flow
- `PLAN.md`: project description references

### Category 6: Runtime Artifacts & Test Files
- `e2e/e2e_test.go`:
  - Line 17: comment "TrafficShim daemon" → "Tresor daemon"
  - Line 19: comment about binary name
  - Line 22: temp dir prefix `trafficshim-e2e-*` → `tresor-e2e-*`
  - Line 78: binary path `../trafficshim.exe` → `../tresor.exe`
  - Line 83: log message
- Test files with temp file prefixes:
  - `internal/engine/engine_test.go` line 63: `trafficshim-engine-test-*` → `tresor-engine-test-*`
  - `internal/api/rules_test.go` line 18: `trafficshim-api-test-*` → `tresor-api-test-*`
  - `internal/store/store_test.go` line 10: `trafficshim-test-*` → `tresor-test-*`
- `internal/config/config_test.go`: comments about `~/.trafficshim.yaml`
- `internal/engine/engine.go` line 553: `"owned_by": "trafficshim"` → `"owned_by": "tresor"` (telemetry header)
- `internal/store/write_config.go` line 142: temp file `.trafficshim-config.tmp` → `.tresor-config.tmp`
- `internal/store/write_config_test.go` line 136: same temp file name

### Category 7: Build Artifacts & DB Files
- `trafficshim.exe` (compiled binary) → will be rebuilt as `tresor.exe`
- `trafficshim.db` (SQLite database) → rename to `tresor.db`
- `daemon.log` — no rename needed (generic name)

### Category 8: Project Directory
- Rename folder `TrafficShim/` → `Tresor/` via filesystem move

---

## Execution Plan

### Phase 1: Code Changes (in-code references)

**Step 1: Update go.mod module path**
- Edit `go.mod` line 1: `module trafficshim` → `module tresor`

**Step 2: Update all import paths**
- Global find-replace in ALL `.go` files: `"trafficshim/internal/` → `"tresor/internal/`
- This affects ~25 files across `cmd/`, `internal/`, and `e2e/`

**Step 3: Update CLI command names & descriptions**
- `cmd/root.go`: `Use`, `Long`, flag help text
- `cmd/rule.go`: no direct references (imports only)
- `cmd/run.go`: `Short`, `Long`, example commands, log messages, flag help

**Step 4: Update config defaults**
- `internal/config/config.go`: fallback paths, default DB paths, comments
- `config.example.yaml`: header comment, example socket/db paths

**Step 5: Update web UI**
- `internal/api/web/index.html`: title, brand-name spans (2 occurrences)
- `internal/api/web/app.js`: top comment, sessionStorage keys (4 occurrences)
- `internal/api/web/style.css`: top comment

**Step 6: Update test files**
- Temp file prefixes in `e2e/e2e_test.go`, `engine/engine_test.go`, `api/rules_test.go`, `store/store_test.go`
- Binary path reference in `e2e/e2e_test.go`
- Comments in `config/config_test.go`

**Step 7: Update runtime strings**
- `internal/engine/engine.go`: telemetry `"owned_by"` header value
- `internal/store/write_config.go`: atomic write temp file name

### Phase 2: Documentation Updates

**Step 8: Update README.md**
- Replace all "TrafficShim" → "Tresor", "trafficshim.exe" → "tresor.exe" throughout
- ~20+ occurrences

**Step 9: Update CLAUDE.md**
- Replace all "TrafficShim" → "Tresor", "trafficshim.exe" → "tresor.exe" throughout
- Update config file references (`.trafficshim.yaml` → `.tresor.yaml`, etc.)
- ~30+ occurrences

**Step 10: Update PLAN.md**
- Replace project name references

### Phase 3: Build, Test, Verify

**Step 11: Build**
```bash
go build -o tresor.exe .
```
Verify compilation succeeds (no import errors).

**Step 12: Run unit tests**
```bash
go clean -testcache
go test ./...
```
All tests must pass.

**Step 13: E2E smoke test**
Start daemon with new binary name, run integration tests against it.

### Phase 4: Directory & Artifact Rename

**Step 14: Rename runtime artifacts**
- Rename `trafficshim.exe` → `tresor.exe` (or just rebuild)
- Rename `trafficshim.db` → `tresor.db`

**Step 15: Rename project directory**
- Move folder `TrafficShim/` → `Tresor/`
- This is done LAST to avoid path issues during editing

### Phase 5: Memory & Final Cleanup

**Step 16: Update memory files**
- Update `MEMORY.md` index path references
- Update memory file contents that reference "TrafficShim"

---

## Risk Assessment

| Risk | Mitigation |
|------|-----------|
| Missed import path | `go build ./...` will catch any remaining `trafficshim/internal/` imports — they'll fail to resolve |
| Missed CLI string | Manual review + grep verification post-change |
| Git history disruption | Directory rename preserves git history; file content changes are normal commits. No `git filter-repo` needed — just regular commits |
| Existing config files break | Users with `~/.trafficshim.yaml` or `./config.yaml` pointing to `trafficshim.db` will need to update. The YAML auto-detect fallback changes from `.trafficshim.yaml` to `.tresor.yaml`. **Mitigation:** Keep backward-compatible fallback — check BOTH `.trafficshim.yaml` AND `.tresor.yaml` during a transition period, with deprecation warning |
| Existing DB files | Old `trafficshim.db` won't auto-migrate. Users need to rename manually or reconfigure |

## Backward Compatibility

**Clean break.** No backward compatibility layer. Old config files (`~/.trafficshim.yaml`) and databases (`trafficshim.db`) will no longer be auto-detected. Users must migrate to the new naming on their own.
