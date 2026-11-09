# Deployment & OS Service Installers

> 中文版本：[部署与各系统服务安装指南](../zh-CN/deployment.md)

`Volo-go` compiles with Go `1.27.3` (`CGO_ENABLED=0`, `GOEXPERIMENT=nojsonv2`) into a single zero-dependency static binary (`< 20 MiB` on `linux/amd64`, `< 21 MiB` across all targets) across **6 supported static release targets**:
- **Linux**: `linux/amd64`, `linux/arm64` (`systemd` system/user units & `OpenRC`)
- **macOS**: `darwin/amd64`, `darwin/arm64` (`launchd` `LaunchDaemon` / `LaunchAgent`)
- **Windows**: `windows/amd64`, `windows/arm64` (native `sc.exe` Windows Service via `svc_windows.go`, `NSSM`, and Scheduled Task fallback)
- **Containers**: Docker Compose / GHCR Distroless non-root image (`UID/GID 65532`)

---

## 1. Configuration Precedence & Environment Variable Reference

### 1.1 Precedence Rules

- **Core Bootstrap Settings (4-Layer Order)**:
  `CLI Flags (--listen-addr, etc.) > OS Environment (VOLO_*) > .env File > Built-in Safe Defaults`
- **Hot-Reloadable `VOLO_UPLOAD_DIR` & `VOLO_GEOIP_DIR` Paths (5-Layer Order)**:
  `CLI Flags (--upload-dir / --geoip-dir) > OS ENV (VOLO_UPLOAD_DIR / VOLO_GEOIP_DIR) > .env File > Admin DB Settings (storage.local_dir / security.geoip_dir) > Default (${VOLO_DATA_DIR}/uploads or ${VOLO_DATA_DIR}/geoip)`
  > Leaving these unpinned in CLI/ENV/`.env` lets administrators hot-switch the local media directory and offline GeoIP directory live from the Admin Console; setting them in any of the top 3 layers pins the path.
- **Setup Wizard Database Persistence (`data/db.json`)**:
  Leaving `VOLO_DB_DRIVER` (and `VOLO_DB_DSN`) unset/commented out in `.env` and `docker-compose.yml` allows the `/setup` wizard's `AES-256-GCM` encrypted `${VOLO_DATA_DIR}/db.json` (`SQLite` / `PostgreSQL` / `MySQL`) to persist across container recreations and service restarts. Setting `VOLO_DB_DRIVER` or `VOLO_DB_DSN` explicitly overrides `db.json`.

### 1.2 Full Environment Variable & CLI Flag Matrix

| Environment Variable | CLI Flag | Default | Description |
| :--- | :--- | :--- | :--- |
| `VOLO_ENV_FILE` | `--env-file` | `.env` | Path to `.env` configuration file (`/etc/volo/volo.env` in system services) |
| `VOLO_LISTEN_ADDR` | `--listen-addr` | `0.0.0.0:8080` | TCP bind address (`127.0.0.1:8080` recommended behind a local reverse proxy) |
| `VOLO_LISTEN_SOCKET` | `--listen-socket` | *(empty)* | Unix Domain Socket path (`/run/volo/volo.sock` or `/run/volo/volo.sock,0660`; disables TCP listener and acquires a `0600` `.sock.lock` file lock) |
| `VOLO_SOCKET_MODE` | `--socket-mode` | `0660` | Octal permission bits applied to the Unix Domain Socket |
| `VOLO_SOCKET_GROUP` | `--socket-group` | *(empty)* | Group ownership for the Unix Domain Socket (e.g., `www-data` on Debian/Ubuntu, `nginx` on RHEL) |
| `VOLO_PUBLIC_URL` | `--public-url` | `http://localhost:8080` | Canonical public URL (used for Canonical tags, RSS/Podcast, Sitemap, WebAuthn Origin, and email links) |
| `VOLO_TRUSTED_PROXIES` | `--trusted-proxies` | `127.0.0.1,::1` | Comma-separated trusted proxy IPs/CIDRs or `unix` (automatically trusts `unix` peers when UDS is active) |
| `VOLO_ADMIN_PATH` | `--admin-path` | `/console` | Stealth admin console base prefix (also hot-reloadable without restart via `security.admin_path` / `dynamic_login_path`; default `/console`, `/login`, `/admin` return indistinguishable `404`s when customized) |
| `VOLO_SECRET_KEY` | `--secret-key` | *(auto-generated)* | Master secret (`>= 32` high-entropy chars recommended; auto-generates `0600` `${VOLO_DATA_DIR}/.secret.key` if empty) |
| `VOLO_DB_DRIVER` | `--db-driver` | *(unset -> `db.json`, fallback `sqlite`)* | Database driver: `sqlite`, `postgres`, or `mysql` (leave unset to preserve `/setup` wizard's `data/db.json`) |
| `VOLO_DB_DSN` | `--db-dsn` | `${VOLO_DATA_DIR}/volo.db` | Database connection string (SQLite path, PostgreSQL URI, or MySQL DSN) |
| `VOLO_DB_SIMPLE_PROTOCOL` | `--db-simple-protocol` | `false` | Enable PostgreSQL pure-Go `pgwire` simple query protocol for transaction poolers (Supabase / PgBouncer) |
| `VOLO_DB_MAX_OPEN_CONNS` | — | `25` | Max open DB connections (`1`–`500`; SQLite writer pool is pinned to `1`) |
| `VOLO_DB_MAX_IDLE_CONNS` | — | `5` | Max idle DB connections (`0`–`500`) |
| `VOLO_DB_CONN_MAX_LIFETIME_SEC` | — | `1800` | Max DB connection lifetime in seconds (`10`–`86400`) |
| `VOLO_GEO_ACCESS_ENABLED` | `--geo-access-enabled` | `true` | Master circuit breaker for offline GeoIP rules (set `false` to emergency-bypass geo lockouts without deleting DB rules) |
| `VOLO_SHUTDOWN_TIMEOUT_SEC` | `--shutdown-timeout` | `15` | Graceful shutdown drain timeout in seconds (`1`–`120`) |
| `VOLO_DEFAULT_LOCALE` | `--default-locale` | `zh-CN` | Fallback default locale (built-in `zh-CN`, `en-US`; extensible via `VOLO_LOCALES_DIR`) |
| `VOLO_DATA_DIR` | `--data-dir` | `./data` | Base runtime data directory (`volo.db`, `db.json`, `.secret.key`, logs) |
| `VOLO_UPLOAD_DIR` | `--upload-dir` | `${VOLO_DATA_DIR}/uploads` | Local uploaded media directory (5-layer precedence with hot-reloadable `storage.local_dir`) |
| `VOLO_BACKUP_DIR` | `--backup-dir` | `${VOLO_DATA_DIR}/backups` | Local automated & manual rolling backup archive directory |
| `VOLO_GEOIP_DIR` | `--geoip-dir` | `${VOLO_DATA_DIR}/geoip` | Offline multi-DB GeoIP directory (`GeoLite2-City.mmdb`, `GeoLite2-Country.mmdb`, `GeoLite2-ASN.mmdb`, `ip2region_v4.xdb`, `ip2region_v6.xdb`; hot-reloadable via `security.geoip_dir`) |
| `VOLO_THEMES_DIR` | `--themes-dir` | `./themes` | External hot-mountable themes directory |
| `VOLO_PLUGINS_DIR` | `--plugins-dir` | `./plugins` | External hot-pluggable plugins directory |
| `VOLO_LOCALES_DIR` | `--locales-dir` | `./locales` | External locale override JSON directory |

---

## 2. Linux Service Installation & Auto-Rollback Upgrades (`systemd` & `OpenRC`)

[`scripts/install-service.sh`](../../scripts/install-service.sh) provides unified service management for Linux and macOS (retaining `install-service-linux.sh` and `install-service-macos.sh` compatibility shims):
- **Pre-Upgrade Backup & Automatic Healthcheck Rollback**: During `upgrade` (or `update`), the installer automatically creates a pre-upgrade database backup via `volo backup create` (skippable with `--skip-backup`), preserves the previous binary as `volo.bak`, verifies `SHA256SUMS.txt` fail-closed, restarts the service, and runs `volo healthcheck` — **automatically restoring `volo.bak` and restarting the previous version if post-upgrade health verification fails**;
- **Init System Coverage**: Supports system-wide `systemd` (`ExecStart=/usr/local/bin/volo serve --env-file /etc/volo/volo.env`), rootless `--user-mode` (`systemd --user`), and Alpine / Gentoo `OpenRC` (`pidfile="/run/volo/volo.pid"`).

### 2.1 One-Line Install, Unix Socket Mode & Auto-Rollback Upgrades

```bash
# One-line remote install & enable on boot (binds 127.0.0.1:8080 by default)
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# Zero-flag online upgrade (pre-upgrade backup + volo.bak + healthcheck auto-rollback)
sudo ./scripts/install-service.sh upgrade
# Skip pre-upgrade DB backup if desired, or specify --mirror with automatic GitHub fallback:
sudo ./scripts/install-service.sh upgrade --skip-backup --mirror https://ghfast.top

# Zero-TCP Unix Domain Socket mode for local Nginx / Caddy (/var/lib/volo & /etc/volo/volo.env stay isolated under volo:volo)
sudo ./scripts/install-service.sh install \
  --unix-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --socket-group www-data \
  --public-url https://blog.example.com

# Rootless user-level service (systemd --user)
./scripts/install-service.sh install --user-mode --port 8080
```

### 2.2 `systemd` Hardening & Custom External Directories (`ReadWritePaths` Override)

[`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) enables `ProtectSystem=strict`, `ProtectHome=true`, and `ReadWritePaths=/var/lib/volo /run/volo` by default.
When configuring custom directories outside `/var/lib/volo` (for example, `upload_dir` / `storage.local_dir`, `geoip_dir` / `security.geoip_dir`, `VOLO_BACKUP_DIR`, `VOLO_THEMES_DIR`, or `VOLO_PLUGINS_DIR`), grant write access via a `systemd` drop-in override:

```bash
sudo systemctl edit volo
```

Add the custom paths and reload (`sudo systemctl daemon-reload && sudo systemctl restart volo`):

```ini
[Service]
ReadWritePaths=/custom/uploads /custom/geoip
# If custom paths reside under /home/... (blocked by ProtectHome=true), also add:
# ProtectHome=read-only
```

### 2.3 `OpenRC` Init Script & Lifecycle Commands

The Alpine / Gentoo init script [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd) supervises the daemon using `command_args="serve --env-file /etc/volo/volo.env"` and `pidfile="/run/volo/volo.pid"` (provisioning `0755` `/run/volo` and `0750` `/var/lib/volo` & `/var/log/volo` in `start_pre`).

```bash
sudo ./scripts/install-service.sh status      # Inspect service status
sudo ./scripts/install-service.sh logs        # Tail service logs
sudo ./scripts/install-service.sh restart     # Restart service (also supports start / stop)
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env
sudo ./scripts/install-service.sh uninstall   # Remove service (keeps data; pass --purge-data to wipe)
```

---

## 3. macOS Service Installation & Upgrades (`launchd`)

The same [`scripts/install-service.sh`](../../scripts/install-service.sh) supports Intel (`darwin/amd64`) and Apple Silicon (`darwin/arm64`) Macs — running without `sudo` installs a per-user `LaunchAgent`, while `sudo` (or `--system`) installs a boot-time `LaunchDaemon`, complete with pre-upgrade backup, `volo.bak`, and `volo healthcheck` auto-rollback:

```bash
# Mode A: Per-user LaunchAgent (no sudo; installs under ~/.local/bin/volo & ~/Library/Application Support/Volo)
./scripts/install-service.sh install --public-url https://blog.example.com

# Mode B: System-wide LaunchDaemon (starts at boot before login)
sudo ./scripts/install-service.sh install --system --public-url https://blog.example.com

# Zero-flag online upgrade (supports --skip-backup) / status / logs / uninstall
./scripts/install-service.sh upgrade
./scripts/install-service.sh status
./scripts/install-service.sh logs
./scripts/install-service.sh uninstall
```

---

## 4. Windows Native Service Installation & Auto-Rollback Upgrades (`sc.exe` / `NSSM` / PowerShell)

Run [`scripts/install-service.ps1`](../../scripts/install-service.ps1) in an elevated PowerShell prompt (auto-detecting `windows/amd64` and `windows/arm64`):
- **Native Windows Service (`sc.exe`) + `NSSM` Support**: `volo.exe` embeds native Windows Service Control Manager integration via `svc_windows.go`, allowing `install-service.ps1` to register a native Windows Service via `sc.exe` without requiring third-party wrappers, while seamlessly supporting `NSSM` when present (and Scheduled Task fallback in restricted environments), alongside NTFS ACL hardening on `C:\ProgramData\Volo` via `icacls.exe`;
- **Mirror Acceleration Fallback, `-Action Logs` & Auto-Rollback**: `-Mirror` accelerates downloads and automatically falls back to direct GitHub Releases if a mirror fails. `-Action Upgrade` creates an automatic pre-upgrade database backup (opt out via `-SkipBackup`), saves `volo.exe.bak` (`volo.bak`), and runs post-upgrade `volo.exe healthcheck` — automatically rolling back to the backup binary if the health probe fails. Use `-Action Logs` to inspect service logs directly.

```powershell
# Install and start Windows Service (supports -Mirror "https://ghfast.top", -Version <tag>, -BinaryPath .\volo.exe, -DryRun)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 `
  -Action Install `
  -HostAddr "127.0.0.1" -Port 8080 `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# Zero-flag online upgrade (pre-upgrade backup + SHA-256 check + volo.bak + healthcheck auto-rollback; optional -SkipBackup)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade -SkipBackup -Mirror "https://ghfast.top"

# Inspect logs / status / restart / uninstall
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
```

---

## 5. Docker Compose & Multi-Database DSN Examples

### 5.1 Distroless Non-Root Container & Volume Persistence

[`docker-compose.yml`](../../docker-compose.yml) runs `gcr.io/distroless/static-debian12:nonroot` (`UID/GID 65532`) with a shell-free `HEALTHCHECK CMD ["/usr/local/bin/volo", "healthcheck"]`:

```bash
cp .env.example .env
# Edit .env to set VOLO_PUBLIC_URL (leave VOLO_DB_DRIVER unset so /setup's data/db.json persists across restarts)
docker compose up -d --build

# Optional: launch bundled PostgreSQL 16 or MySQL 8.4 profiles
docker compose --profile postgres up -d
# or: docker compose --profile mysql up -d
```

- **Default Named Volume (`volo_data:/app/data`)**: Works out-of-the-box without host UID adjustments, persisting `volo.db`, `db.json`, `.secret.key`, `uploads/`, `backups/`, and `geoip/`;
- **Host Bind-Mount Notes**:
  1. If replacing `volo_data:/app/data` with a host bind mount (`./data:/app/data`), grant ownership to the Distroless `nonroot` UID/GID first:
     ```bash
     mkdir -p data && sudo chown -R 65532:65532 data
     ```
  2. **Do not bind-mount empty `./themes` or `./plugins` host directories** onto `/app/themes` or `/app/plugins`, as empty host directories will shadow the image-bundled themes (`volo-journal`) and plugins (`kanbanniang`).

### 5.2 Database Engine DSN Examples (Explicit Pinning Mode)

If you prefer pinning the database via `.env` instead of using `/setup`'s encrypted `data/db.json`:

1. **SQLite (Default — zero maintenance for tens of thousands of posts)**:
   ```dotenv
   VOLO_DB_DRIVER=sqlite
   VOLO_DB_DSN=/var/lib/volo/data/volo.db
   ```
2. **PostgreSQL / Supabase / Neon / CockroachDB**:
   ```dotenv
   VOLO_DB_DRIVER=postgres
   VOLO_DB_DSN=postgres://volo:strong_password@127.0.0.1:5432/volo?sslmode=verify-full
   # Enable simple protocol when connecting through Supabase / PgBouncer transaction poolers:
   VOLO_DB_SIMPLE_PROTOCOL=true
   ```
3. **MySQL 8+ / MariaDB / TiDB**:
   ```dotenv
   VOLO_DB_DRIVER=mysql
   VOLO_DB_DSN=volo:strong_password@tcp(127.0.0.1:3306)/volo?charset=utf8mb4&parseTime=true&loc=UTC
   ```

> **Next Step**: Continue to the [Reverse Proxy & CDN Guide](./reverse-proxy.md) to configure HTTPS, Unix socket group permissions, spoof-proof client IP forwarding, and zero-copy podcast/video range streaming.
