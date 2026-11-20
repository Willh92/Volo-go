# Volo-go E2E Test Suite

Opaque-box, requirement-driven end-to-end test suite for Volo-go covering CLI/boot, Setup Wizard, auth/2FA/PAT, all 9 settings domains, content CRUD, plugin lifecycle, Markdown rendering/XSS sanitization, Admin SPA/CM6 editor, and multi-viewport/theme browser verification.

## Prerequisites

- **Go** (`1.27.4`+): used to build `./cmd/volo` with `CGO_ENABLED=0` and `GOEXPERIMENT=nojsonv2`.
- **Node.js** (`E:\Developers\node-v24.22.3\node.exe` or Node 24+ ESM): executes `tests/e2e/run.mjs` via `node:test`.
- **Microsoft Edge + Playwright ESM** (required for browser tests):
  - Playwright package: `C:\Users\92wil\AppData\Local\ms-playwright-go\1.50.1\package\index.mjs`
  - Default browser path: `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`

## Running Tests

```powershell
# Run all tiers (builds volo once and cleans up afterward)
E:\Developers\node-v24.22.3\node.exe tests/e2e/run.mjs

# Run a single tier (1, 2, 3, or 4)
E:\Developers\node-v24.22.3\node.exe tests/e2e/run.mjs --tier=1

# Filter by file path substring or test name pattern
E:\Developers\node-v24.22.3\node.exe tests/e2e/run.mjs --filter=settings

# Adjust per-tier file concurrency and keep temporary server directories on failure
E:\Developers\node-v24.22.3\node.exe tests/e2e/run.mjs --tier=2 --concurrency=2 --keep-tmp

# Reuse an existing pre-built binary
$env:VOLO_E2E_BIN = "C:\path\to\volo.exe"
E:\Developers\node-v24.22.3\node.exe tests/e2e/run.mjs --tier=1
```

## Environment Variables

| Variable | Description | Default |
|---|---|---|
| `VOLO_E2E_BIN` | Path to a pre-built `volo` binary (skips `go build` when set) | Built once per run in `os.tmpdir()` |
| `VOLO_E2E_BROWSER` | Path to Chromium/Edge executable for Playwright | `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe` |
| `VOLO_E2E_KEEP_TMP` | Set to `"1"` to preserve temporary `workDir` / `data/` directories after `stop()` | `"0"` (cleaned up) |
| `VOLO_E2E_CONCURRENCY` | Default file concurrency per tier when `--concurrency` is omitted | `"1"` |

## Directory Layout

```text
tests/e2e/
├── run.mjs               # CLI test runner, binary builder, and per-tier summary reporter
├── README.md             # Suite documentation
├── lib/
│   ├── server.mjs        # Binary build/reuse, port allocation, isolated server lifecycle
│   ├── client.mjs        # Fetch client with cookie jar, CSRF/Console headers, TOTP & PoW helpers
│   ├── browser.mjs       # Edge Playwright launcher, pageerror/console diagnostics, geometry checks
│   └── assertions.mjs    # HTTP, JSON, cookie, redirect, XSS, SSRF, and settings assertions
├── tier1/                # Feature coverage suites
├── tier2/                # Boundary, error, security, and corner-case suites
├── tier3/                # Cross-feature pairwise combination suites
└── tier4/                # Real-world workflows and browser viewport/theme matrix
```

## Isolation Design

- **Single Build, Ephemeral Instances**: `run.mjs` builds `./cmd/volo` once via `ensureBuiltBinary()` and exports `VOLO_E2E_BIN` to each test file process.
- **Loopback Ports**: Every `startVoloServer()` call binds `127.0.0.1` on an ephemeral port obtained via `allocateFreePort()`.
- **Filesystem Isolation**: Each server instance gets its own temporary `workDir` containing an isolated `data/` directory (SQLite DB) and copied `themes/`, `plugins/`, and `locales/` directories so repository files are never mutated.
- **Process Hygiene**: `stop()` terminates the server gracefully, waits for exit, and removes the temporary `workDir` unless `VOLO_E2E_KEEP_TMP=1`.
