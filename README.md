# Volo-go

[![CI](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml)
[![Security](https://github.com/Willh92/Volo-go/actions/workflows/security.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/security.yml)
[![Release](https://github.com/Willh92/Volo-go/actions/workflows/release.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/release.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-246b51.svg)](./LICENSE)

**Pure-Go Sovereign Independent Blog Engine**

`CGO_ENABLED=0` static single binary across 6 OS/Arch targets · Strictly **`< 21 MiB`** across all 6 targets · **`24.23 MiB` (< 35 MiB)** warm `VmRSS` · Native multi-dialect SQL (SQLite WAL / PostgreSQL / MySQL) · Real-time non-destructive ImageView (`?imageView2` + KV) · 10-surface zero-leakage privacy guard · 3-mode (`IR` / `WYSIWYG` / `SV`) Studio Editor & fullscreen Markdown comment composer

[English](./README.md) | [简体中文](./README.zh-CN.md) · [Changelog](./CHANGELOG.md) | [版本更新日志](./CHANGELOG.zh-CN.md)

---

## Documentation Index

| Guide | Topics Covered |
| :--- | :--- |
| [Architecture & Performance Internals (`docs/en-US/architecture.md`)](./docs/en-US/architecture.md) | `v8` schema (33 normalized data tables + 9 hot-reload settings domains), real-time ImageView pipeline, multi-dialect SQL & split WAL pools, 10-surface privacy perimeter, memory budget |
| [Deployment & OS Service Installers (`docs/en-US/deployment.md`)](./docs/en-US/deployment.md) | Multi-layer config precedence, 6 static release targets, Linux (`systemd` / `OpenRC` privilege separation), macOS (`launchd`), Windows native service (`sc.exe` / `NSSM`), auto-rollback upgrades, Docker / GHCR |
| [Reverse Proxy & CDN Guide (`docs/en-US/reverse-proxy.md`)](./docs/en-US/reverse-proxy.md) | TCP vs Unix Domain Socket (`--unix-socket` / `--socket-mode` / `--socket-group`), `VOLO_TRUSTED_PROXIES` anti-spoofing CIDRs, Nginx / Caddy / Traefik / Apache / Cloudflare recipes |
| [CLI & Open API Reference (`docs/en-US/cli-and-api.md`)](./docs/en-US/cli-and-api.md) | Built-in `volo` CLI subcommands, scoped `volo_pat_*` tokens, real-time `?imageView2` & KV image query parameters, account ban/mute APIs, paginated search, and REST endpoints |
| [Themes & Plugin SDK (`docs/en-US/themes-and-plugins.md`)](./docs/en-US/themes-and-plugins.md) | `theme.json`, fullscreen reply Markdown editor, pure-Go ES6 plugin sandbox (5-stage lifecycle, unified `before_*` chains, `volo.kv.incrAsync`, `CronRegistry`, `wordFilter`), ZIP install/export |
| [Theme Skin Migration Guide (`docs/en-US/theme-migration.md`)](./docs/en-US/theme-migration.md) | Standalone full-HTML & layout inheritance modes, full `PageContext`, `config_schema` fields, and template FuncMap reference |
| [Language Pack Extension Guide (`docs/en-US/locale-extension.md`)](./docs/en-US/locale-extension.md) | Hot-installable JSON / `.zip` language packs (with complete `zh-TW` & `ja-JP` packs), `admin.nav.*` namespace, and starter template export |
| [Legacy Migration & Cross-DB Backup (`docs/en-US/migration-and-backup.md`)](./docs/en-US/migration-and-backup.md) | Streaming legacy 15-table ETL, `301` `.html` permalink preservation, `SQLite ⇄ PostgreSQL ⇄ MySQL` 33-table streaming backups |
| [Changelog (`CHANGELOG.md`)](./CHANGELOG.md) | Release history and upgrade notes |

---

## Core Capabilities & Measured Benchmarks

| Metric / Benchmark (`CGO_ENABLED=0`) | Measured Result | Hard Budget / Notes |
| :--- | :--- | :--- |
| Static Single Binary Size (`linux/amd64`) | **`19.68 MiB`** | Strictly **`< 21.00 MiB`** across all 6 OS/Arch targets (`< 8.00 MiB` archives) |
| Warm Resident Memory (`VmRSS`) | **`24.23 MiB`** | `< 35.00 MiB` including embedded themes, plugin sandbox, `wordFilter` engine, and warm caches |
| Admin Console Initial Core (Gzip) | **`~85 KB`** (`< 300 KB` total) | Workspaces & CodeMirror 6 3-mode editor core are lazy-loaded (`66%+` smaller initial payload) |
| Warm Article Card Cache (`ToCard`) | **`1.19 µs/op`** (`864 B/op`) | 4 MiB digest-verified LRU (`758×` faster than cold Markdown render) |
| Warm Article Detail Cache (`ToDetail`) | **`4.64 µs/op`** (`3.3 KB/op`) | Strictly excludes password-locked & `?preview=` requests (zero cross-user leakage) |
| Pre-Compressed RSS / Sitemap + `ETag` | **`7.69 µs/op`** (`~130k req/s`) | `1ns` revision invalidation; auto-shards into `<sitemapindex>` above `10,000` URLs |
| Concurrent Article Read + Write-Behind (`RecordView`) | **`454.9 µs/op`** (2-core SSR+Gzip) | Real-time pending counter merge + 2s/shutdown batched flush (zero SQLite write lock contention) |
| Two-Stage Full-Text Search (`SearchIndex`) | **`29.4 µs/op`** | 8 MiB in-memory index + UTF-8 Rune-safe snippet windowing + paginated `/api/v1/search` |

1. **Sub-`21 MiB` Pure-Go Single-File Delivery Across All 6 Targets (`CGO_ENABLED=0`)**: Built with Go `1.27.3` (`GOEXPERIMENT=nojsonv2`), zero-reflection `tinyxml`, and pure-Go SMTP/PGv3 drivers for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`. Bundles themes, Studio Admin SPA, locale catalogs (plus external `zh-TW` & `ja-JP`), and Ops CLI with zero-copy `http.ServeContent` `Range` (`206 Partial Content`) media streaming, BREACH-safe pooled Gzip, deterministic `SIGTERM` drain, and read-only safe `volo healthcheck`.
2. **Multi-Layer Config & Pure-Go Multi-Dialect DB**: Base bootstrap flags resolve `CLI Flags > OS VOLO_* Env > .env > Defaults`, while runtime upload and GeoIP directories follow `CLI Flags (--upload-dir / --geoip-dir) > Admin Console DB Settings > OS ENV / .env > Defaults`. Leaving `VOLO_DB_DRIVER` unset lets the `/setup` wizard's `AES-256-GCM` encrypted `data/db.json` (`SQLite` / `PostgreSQL` / `MySQL`) persist across container/service restarts. Supports **SQLite WAL** (split writer/read-only pools), **PostgreSQL** (first-party pure-Go `pgwire` PGv3 driver), and **MySQL 8+ / MariaDB / TiDB**.
3. **Normalized Domain Model (`v8` Schema, 33 Data Tables + 9 Settings Domains), Studio Editor & Fullscreen Comment Editor**: Atomic hot-reload across all 9 domains (including all 4 `seo.post_permalink_style` URL templates); features the **3-Mode Studio Editor** (`IR`, `WYSIWYG`, `SV`) in the Admin Console plus a unified mobile-safe **Public Reply Markdown Editor** (toolbar, sanitized live preview, distraction-free fullscreen mode, configurable per-identity rune/line bounds, and vertical height-bomb defense).
4. **Dynamic Stealth Admin Path, 3-Tier WAF Risk Control, Multi-DB Offline GeoIP & Account/IP Governance**: Zero-downtime hot-reloadable stealth admin path (`security.admin_path`), configurable 3-tier scanner risk control + IP/CIDR whitelist (`risk_whitelist_cidrs`) + history-preserving IP ban lifecycle & retention policies (`ip_ban` / `audit_log` / `plugin_log`), and side-by-side `GeoLite2` + `ip2region` GeoIP engine; `admin` / `editor` / `reader` RBAC, `Argon2id`, WebAuthn Passkeys, TOTP, Step-Up re-authentication, scoped `volo_pat_*` tokens, 3-tier self-healing `GuestPassport` + stateless PoW, **permanent/timed account bans, mutes, and non-destructive historical comment shielding**, plus a **comment trash** (lossless restore, permanent delete with full PII scrub, configurable auto-purge, thread-preserving placeholders) and admin guest-identity editing for migrated comments.
5. **Real-Time Non-Destructive ImageView (`?imageView2` & KV), 10-Surface Privacy & Chained Plugin SDK**: `/uploads/...` (and legacy Solo image paths) support both Qiniu Cloud path syntax (`?imageView2/<mode>/w/960/h/540/format/webp/q/85/...`) and KV query parameters (`?mode=1&w=960&h=540&format=webp&q=85&colors=128&strip&ignore-error=1`) without mutating source files (`singleflight` + `8 MiB` LRU + `304 ETag` + orphan-scanner & CLS linkage, with zero-overhead passthrough when unparameterized); `ContentVisibilityGuard` guarantees zero leakage across all 10 surfaces; plugins run in a bounded pure-Go ES6 sandbox with frozen prototypes, 5-stage lifecycle, `before_*` chains, `volo.kv.incrAsync` write-behind counters, `CronRegistry`, and native `wordFilter`.

---

## Quick Start

### 1. Build from Source & Zero-Config Launch (Go `1.27.3` + Node.js `24 LTS`)

```bash
# 1. Build embedded Admin Console frontend assets
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. Compile pure-Go static binary (< 21 MiB)
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath \
  -gcflags="all=-l" -ldflags="-s -w -buildid=" ./cmd/volo

# 3. Start with zero config (auto-creates ./data/volo.db and 0600 master key)
./volo serve
```

Open `http://localhost:8080` to launch the `/setup` wizard (configure/test your database, create the admin account, or stream-import a legacy backup archive).

### 2. One-Command OS Service Installers & Auto-Rollback Upgrades

[`scripts/install-service.sh`](./scripts/install-service.sh) (Linux `systemd` system/user, `OpenRC` `/run/volo/volo.pid`, macOS `launchd`) and [`scripts/install-service.ps1`](./scripts/install-service.ps1) (native Windows Service `sc.exe` via `svc_windows.go`, `NSSM`, and Scheduled Task fallback) automatically create a pre-upgrade backup (skippable via `--skip-backup` / `-SkipBackup`), preserve `volo.bak`, verify `SHA256SUMS.txt`, and run `volo healthcheck` after restart — rolling back automatically if health verification fails:

```bash
# Linux / macOS one-line install (listens on 127.0.0.1:8080 by default)
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# Zero-flag online upgrade (pre-upgrade backup + SHA-256 check + healthcheck auto-rollback; supports --mirror & --skip-backup)
sudo ./scripts/install-service.sh upgrade

# Linux + Nginx zero-TCP Unix Domain Socket mode
sudo ./scripts/install-service.sh install \
  --unix-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --socket-group www-data \
  --public-url https://blog.example.com

# macOS rootless current-user LaunchAgent install & upgrade
./scripts/install-service.sh install --public-url https://blog.example.com
./scripts/install-service.sh upgrade

# Windows (Run in elevated PowerShell: native sc.exe Windows Service / NSSM, -Mirror fallback, and -Action Logs)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Install -PublicUrl "https://blog.example.com"
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
```

Production reverse-proxy templates for **Nginx (TCP & Unix Socket), Caddy v2, Traefik v3, and Apache httpd** are in [`deploy/reverse-proxy/`](./deploy/reverse-proxy/) — see the [Deployment Guide](./docs/en-US/deployment.md) and [Reverse Proxy Guide](./docs/en-US/reverse-proxy.md).

### 3. Native HTTPS (Optional)

Volo-go can terminate TLS itself for small single-host deployments without a reverse proxy. Setting both `VOLO_TLS_CERT_FILE` (`--tls-cert`, PEM certificate chain) and `VOLO_TLS_KEY_FILE` (`--tls-key`, PEM private key) enables HTTPS on `VOLO_LISTEN_ADDR`:

```bash
VOLO_LISTEN_ADDR=0.0.0.0:443 \
VOLO_PUBLIC_URL=https://blog.example.com \
VOLO_TLS_CERT_FILE=/etc/volo/tls/fullchain.pem \
VOLO_TLS_KEY_FILE=/etc/volo/tls/privkey.pem \
VOLO_TLS_MIN_VERSION=1.2 \
VOLO_TLS_HTTP_REDIRECT_ADDR=:80 \
./volo serve
```

- **Hot reload**: renewed certificate/key files (acme.sh / certbot / lego) are detected automatically (checked at most every 30 s) — no restart; a broken half-written renewal keeps serving the last good certificate.
- **HTTP/2** is negotiated via ALPN (`h2`, with HTTP/1.1 fallback); `VOLO_TLS_MIN_VERSION` accepts `1.2` (default) or `1.3`.
- **Optional redirect**: `VOLO_TLS_HTTP_REDIRECT_ADDR` (`--tls-http-redirect-addr`, e.g. `:80`) starts a plain-HTTP listener that `308`-redirects to the `VOLO_PUBLIC_URL` host (the client `Host` header is never trusted).
- **Ports below 1024** need privileges: for the bundled systemd unit add a drop-in (`sudo systemctl edit volo`) with `[Service]` `AmbientCapabilities=CAP_NET_BIND_SERVICE`; outside systemd use `sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/volo` (re-run after every binary upgrade; file capabilities are ignored under `NoNewPrivileges=true`). The service user must be able to read the key file (e.g. copy it with an ACME deploy hook instead of pointing at root-only `/etc/letsencrypt/live`).
- Native TLS **cannot be combined with a Unix socket** listener (`VOLO_LISTEN_SOCKET`); `volo doctor` reports certificate validity/expiry and `volo healthcheck` probes `https://` automatically.
- For multi-site hosts, CDN fronting, or automatic ACME issuance, an **Nginx / Caddy reverse proxy remains the recommended deployment**.

### 4. Docker Compose Deployment

Mounts the named volume `volo_data:/app/data` by default and leaves `VOLO_DB_DRIVER` unset so the `/setup` wizard's `data/db.json` persists across container recreations (if switching to a host bind mount `./data:/app/data`, run `mkdir -p data && chown 65532:65532 data` for the Distroless `nonroot` UID/GID):

```bash
cp .env.example .env
docker compose up -d --build
```

### 5. Built-in Ops CLI & Verification Suite

```bash
volo serve                                                   # Start HTTP or Unix Domain Socket server
volo admin reset-password --user <u|email> [--password <pw>] # Reset Argon2id password (alias: volo passwd) & revoke sessions + tokens
volo admin disable-2fa --user <u|email>                      # Emergency-disable TOTP & recovery codes for a user
volo backup create [--output backup.zip] [--with-media=true] # Create cross-DB streaming NDJSON + media ZIP backup
volo backup restore <backup.zip> [--dry-run]                 # Verify SHA-256 & restore cross-DB backup
volo import-legacy <legacy.zip|json> [--dry-run] \
  [--old-site-url URL] [--new-site-url URL] [--storage-local] # Stream & normalize legacy 15-table backup archive
volo doctor [--json]                                         # Run crypto, SQLite WAL, GeoIP & proxy diagnostics
volo healthcheck [--url http://127.0.0.1:8080/healthz]       # Probe /healthz over TCP or Unix Socket (Distroless ready)
volo version                                                 # Print version & Go OS/Arch runtime info
```

```bash
# Local build, unit tests, race detector, benchmarks & binary budget verification
npm --prefix web/admin ci && npm --prefix web/admin run build
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build ./... && CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go vet ./...
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go test -count=1 ./...
CGO_ENABLED=1 GOEXPERIMENT=nojsonv2 go test -race -count=1 ./...
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go test -run=^$ -bench=. -benchmem ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
GOEXPERIMENT=nojsonv2 bash scripts/verify-binary-budget.sh
```

---

## License

Licensed under the **Apache License, Version 2.0** ([LICENSE](./LICENSE)). See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for third-party component attributions and [SECURITY.md](./SECURITY.md) for vulnerability reporting.