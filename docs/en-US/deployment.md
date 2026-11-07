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

## 2. Linux Service Installation (`systemd` & `OpenRC`)

[`scripts/install-service-linux.sh`](../../scripts/install-service-linux.sh) auto-detects Debian / Ubuntu / RHEL / Rocky / Arch (`systemd`) as well as Alpine / Gentoo (`OpenRC`). It provisions a locked `nologin` `volo` system account, creates `0750` data directories, generates a 256-bit random `VOLO_SECRET_KEY` in `0640` `/etc/volo/volo.env`, and enables kernel sandboxing (`ProtectSystem=strict`, `ProtectHome=true`, `NoNewPrivileges=true`).

### 2.1 Standard TCP Loopback Mode (Behind Nginx / Caddy / Traefik)

```bash
# Preview generated unit files and commands safely without root (--dry-run)
bash scripts/install-service-linux.sh install \
  --dry-run \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret

# Install, enable on boot, and start the hardened service
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret
```

### 2.2 Zero-TCP Unix Domain Socket Mode (Same-Host Nginx / Caddy)

Set `--group` to the reverse proxy's group (`www-data` on Debian/Ubuntu, `nginx` or `caddy` on RHEL/Rocky/Alpine):

```bash
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --group www-data \
  --listen-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --public-url https://blog.example.com
```

### 2.3 Seamless Upgrades, Diagnostics & Uninstallation

```bash
# Atomic binary upgrade + restart (preserves /etc/volo/volo.env and /var/lib/volo)
sudo bash scripts/install-service-linux.sh upgrade --binary ./volo

# Check service status & run comprehensive diagnostics
sudo bash scripts/install-service-linux.sh status
sudo /usr/local/bin/volo doctor --env-file /etc/volo/volo.env

# Uninstall service (keeps data by default; pass --purge-data to delete /var/lib/volo)
sudo bash scripts/install-service-linux.sh uninstall
```

Reference unit templates are also available directly under [`deploy/systemd/volo.service`](../../deploy/systemd/volo.service) and [`deploy/openrc/volo.initd`](../../deploy/openrc/volo.initd).

---

## 3. macOS Service Installation (`launchd`)

[`scripts/install-service-macos.sh`](../../scripts/install-service-macos.sh) supports both Intel (`darwin/amd64`) and Apple Silicon (`darwin/arm64`) Macs:

```bash
# Mode A: Per-user LaunchAgent (recommended for Mac mini home servers; no sudo required)
bash scripts/install-service-macos.sh install \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com

# Mode B: System-wide LaunchDaemon (starts at boot before user login; requires sudo)
sudo bash scripts/install-service-macos.sh install --system \
  --binary ./volo \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com

# Status / Upgrade / Uninstall
bash scripts/install-service-macos.sh status
bash scripts/install-service-macos.sh upgrade --binary ./volo
bash scripts/install-service-macos.sh uninstall
```

---

## 4. Windows Service Installation (PowerShell)

Run [`scripts/install-service-windows.ps1`](../../scripts/install-service-windows.ps1) in an Administrator PowerShell window:
- If `nssm.exe` is available on `PATH`, it registers a native `SERVICE_AUTO_START` Windows Service;
- Otherwise, it registers a zero-dependency boot-time Scheduled Task running as least-privilege `NT AUTHORITY\NETWORK SERVICE` with automatic crash recovery and restricts NTFS ACLs on `C:\ProgramData\Volo\volo.env` via `icacls.exe`.

```powershell
# Preview actions without modifying the system (-DryRun)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 `
  -Action Install -BinaryPath .\volo.exe -DryRun

# Install and start service (binds 127.0.0.1:8080 behind IIS / Caddy / Nginx)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 `
  -Action Install `
  -BinaryPath .\volo.exe `
  -ListenAddr "127.0.0.1:8080" `
  -PublicUrl "https://blog.example.com" `
  -AdminPath "/studio-secret"

# Status / Upgrade / Uninstall
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Status
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Upgrade -BinaryPath .\volo.exe
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 -Action Uninstall
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
