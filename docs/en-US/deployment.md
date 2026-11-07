# Deployment & OS Service Installers

> 中文版本：[部署与各系统服务安装指南](../zh-CN/deployment.md)

`Volo-go` compiles into a single zero-dependency static binary (`CGO_ENABLED=0`) ready for production on **Linux (`systemd` / `OpenRC`), macOS (`launchd`), Windows (`NSSM` / Scheduled Tasks)**, and **Docker / Kubernetes**.

---

## 1. 4-Layer `.env` Configuration Precedence & Variable Reference

Configuration is resolved deterministically in four layers (highest priority first):

`CLI Flags (--listen-addr, etc.) > OS Environment (VOLO_*) > .env File > Built-in Safe Defaults`

| Environment Variable | CLI Flag | Default | Description |
| :--- | :--- | :--- | :--- |
| `VOLO_LISTEN_ADDR` | `--listen-addr` | `0.0.0.0:8080` | TCP bind address (`127.0.0.1:8080` recommended behind a local reverse proxy) |
| `VOLO_LISTEN_SOCKET` | `--listen-socket` | *(empty)* | Unix Domain Socket path (e.g. `/run/volo/volo.sock`; takes precedence over TCP when set) |
| `VOLO_LISTEN_SOCKET_MODE` | `--listen-socket-mode` | `0660` | Octal file mode applied to the Unix Domain Socket |
| `VOLO_PUBLIC_URL` | `--public-url` | `http://localhost:8080` | Canonical public URL (used for Canonical tags, RSS, Sitemap, WebAuthn RP ID, and `Secure` cookies) |
| `VOLO_TRUSTED_PROXIES` | `--trusted-proxies` | `127.0.0.1/32,::1/128` | Comma-separated trusted proxy IP/CIDR allowlist protecting `X-Forwarded-For` resolution |
| `VOLO_ADMIN_PATH` | `--admin-path` | `/console` | Stealth admin console prefix (requests to default `/console` return an indistinguishable `404` when changed) |
| `VOLO_SECRET_KEY` | `--secret-key` | *(auto-generated)* | Master secret (`>= 32` random bytes recommended; auto-generates `0600` `VOLO_DATA_DIR/.secret.key` if unset) |
| `VOLO_DB_DRIVER` | `--db-driver` | `sqlite` | Database engine: `sqlite`, `postgres`, or `mysql` |
| `VOLO_DB_DSN` | `--db-dsn` | `./data/volo.db` | Database connection string (SQLite path, PostgreSQL URI, or MySQL DSN) |
| `VOLO_DB_MAX_OPEN_CONNS` | — | `25` | Max open DB connections (SQLite writer pool is automatically pinned to `1`) |
| `VOLO_DB_MAX_IDLE_CONNS` | — | `10` | Max idle DB connections |
| `VOLO_GEO_ACCESS_ENABLED` | `--geo-access-enabled` | `true` | Master kill-switch for GeoIP access rules (set `false` to emergency-recover from accidental geo-lockout) |
| `VOLO_SHUTDOWN_TIMEOUT` | `--shutdown-timeout` | `15s` | Graceful shutdown deadline for draining requests and flushing buffered analytics |
| `VOLO_DEFAULT_LOCALE` | `--default-locale` | `zh-CN` | Default UI locale (`zh-CN` or `en-US`) |
| `VOLO_DATA_DIR` | `--data-dir` | `./data` | Core runtime data directory |
| `VOLO_UPLOAD_DIR` | `--upload-dir` | `./data/uploads` | Local media upload storage directory |
| `VOLO_BACKUP_DIR` | `--backup-dir` | `./data/backups` | Automated and manual backup archive directory |
| `VOLO_GEOIP_DIR` | `--geoip-dir` | `./data/geoip` | Offline GeoIP directory (`GeoLite2-City.mmdb` or `ip2region.xdb`) |
| `VOLO_PLUGINS_DIR` | `--plugins-dir` | `./plugins` | External plugins directory |
| `VOLO_THEMES_DIR` | `--themes-dir` | `./themes` | External themes directory |
| `VOLO_LOCALES_DIR` | `--locales-dir` | `./locales` | External locale override JSON directory |

---

### 2. Linux Service Installation & Zero-Flag Online Upgrades (`systemd` System/User & `OpenRC`)

[`scripts/install-service.sh`](../../scripts/install-service.sh) provides a unified cross-platform service manager for Linux and macOS (with `install-service-linux.sh` and `install-service-macos.sh` kept as compatibility shims):
- **Zero-Flag Ergonomics**: Running `install` automatically uses a local binary when present or downloads the matching OS/Arch release from GitHub Releases; running `upgrade` (or `update`) requires **zero extra flags** — it automatically downloads the latest release, verifies `SHA256SUMS.txt` (fail-closed), runs a `volo version` preflight check, preserves existing paths and `/etc/volo/volo.env`, and restarts the service;
- **Full Production Hardening**: Supports system-wide `systemd` (`ProtectSystem=strict`, `NoNewPrivileges=true`), rootless `--user-mode` (`systemd --user`), Alpine/Gentoo `OpenRC`, Unix Domain Socket group ACLs, `--mirror` download acceleration, `--bin` / `--archive` offline installs, and `--dry-run` previews.

### 2.1 One-Line Remote Install & Zero-Flag Online Upgrade (Recommended)

```bash
# One-line remote install & enable on boot (binds 127.0.0.1:8080 by default)
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# Or customize public URL and stealth admin path on initial install
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh \
  | sudo sh -s -- install --public-url https://blog.example.com --admin-path /studio-secret

# Zero-flag online upgrade (preserves /etc/volo/volo.env & /var/lib/volo; supports --version <tag> and --mirror https://ghfast.top)
sudo ./scripts/install-service.sh upgrade
# Or run upgrade directly from remote:
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh -s -- upgrade
```

### 2.2 Advanced Options: Unix Domain Sockets, User Mode & Offline Bundles

```bash
# Zero-TCP Unix Domain Socket mode granting group access to Nginx / Caddy (www-data / nginx)
sudo ./scripts/install-service.sh install \
  --socket-group www-data \
  --supplementary-groups www-data \
  --unix-socket /run/volo/volo.sock \
  --unix-socket-perm 0660 \
  --public-url https://blog.example.com

# Non-root user service (systemd --user)
./scripts/install-service.sh install --user-mode --port 8080

# Offline install/upgrade from local binary (--bin) or release bundle (--archive), plus --dry-run preview
./scripts/install-service.sh install --dry-run --bin ./volo --public-url https://blog.example.com
sudo ./scripts/install-service.sh upgrade --bin ./volo
```

### 2.3 Lifecycle Management & Diagnostics

```bash
sudo ./scripts/install-service.sh status      # Inspect service status
sudo ./scripts/install-service.sh logs        # Tail service logs
sudo ./scripts/install-service.sh restart     # Restart service (also supports start / stop)
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env
sudo ./scripts/install-service.sh uninstall   # Remove service (keeps data; pass --purge-data to wipe)
```

Reference unit templates are also available directly under [`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) and [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd).

---

## 3. macOS Service Installation & Upgrades (`launchd`)

The same [`scripts/install-service.sh`](../../scripts/install-service.sh) supports Intel (`darwin/amd64`) and Apple Silicon (`darwin/arm64`) Macs — running without `sudo` automatically installs a per-user `LaunchAgent`, while `sudo` (or `--system`) installs a boot-time `LaunchDaemon`:

```bash
# Mode A: Per-user LaunchAgent (no sudo; installs under ~/.local/bin/volo & ~/Library/Application Support/Volo)
./scripts/install-service.sh install --public-url https://blog.example.com

# Mode B: System-wide LaunchDaemon (starts at boot before user login)
sudo ./scripts/install-service.sh install --system --public-url https://blog.example.com

# Zero-flag online upgrade / status / logs / uninstall
./scripts/install-service.sh upgrade
./scripts/install-service.sh status
./scripts/install-service.sh logs
./scripts/install-service.sh uninstall
```

---

## 4. Windows Service Installation & Upgrades (PowerShell)

Run [`scripts/install-service.ps1`](../../scripts/install-service.ps1) in an Administrator PowerShell window (`install-service-windows.ps1` is also retained as a compatibility wrapper):
- Uses a local `volo.exe` automatically when present, or downloads from GitHub Releases with mandatory `SHA256SUMS.txt` verification;
- Registers a native Windows Service when `nssm.exe` is on `PATH`, or a zero-dependency boot-time Scheduled Task under `NT AUTHORITY\NetworkService` with NTFS ACL hardening via `icacls.exe`.

```powershell
# Install and start auto-start service (supports -Version <tag>, -Mirror "https://ghfast.top", -BinaryPath .\volo.exe, -DryRun)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 `
  -Action Install `
  -HostAddr "127.0.0.1" -Port 8080 `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# Zero-flag online upgrade (gracefully stops service, smoke-tests volo.exe version, swaps binary, preserves volo.env & data\)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade

# Lifecycle management
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Restart
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Uninstall
```

---

## 5. Docker Compose & Multi-Database DSN Examples

### 5.1 Distroless Non-Root Container

```bash
cp .env.example .env
# Edit .env to set VOLO_PUBLIC_URL and VOLO_ADMIN_PATH
docker compose up -d --build
```

The production image uses `gcr.io/distroless/static-debian12:nonroot` (`UID 65532`) with a built-in shell-free `HEALTHCHECK CMD ["/usr/local/bin/volo", "healthcheck"]`.

### 5.2 Database Engine DSN Examples

1. **SQLite (Default — zero maintenance for tens of thousands of posts)**:
   ```dotenv
   VOLO_DB_DRIVER=sqlite
   VOLO_DB_DSN=/var/lib/volo/data/volo.db
   ```
2. **PostgreSQL / Supabase / Neon / CockroachDB**:
   ```dotenv
   VOLO_DB_DRIVER=postgres
   VOLO_DB_DSN=postgres://volo:strong_password@127.0.0.1:5432/volo?sslmode=verify-full
   ```
3. **MySQL 8+ / MariaDB / TiDB**:
   ```dotenv
   VOLO_DB_DRIVER=mysql
   VOLO_DB_DSN=volo:strong_password@tcp(127.0.0.1:3306)/volo?charset=utf8mb4&parseTime=true&loc=UTC
   ```

> **Next Step**: Continue to the [Reverse Proxy & CDN Guide](./reverse-proxy.md) to configure HTTPS, spoof-proof client IP forwarding, and zero-copy podcast audio range streaming.
