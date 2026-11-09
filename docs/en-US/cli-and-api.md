# CLI & API Token Reference

> 中文版本：[CLI 运维命令与 API 令牌接入指南](../zh-CN/cli-and-api.md)

`Volo-go` embeds a full Swiss-Army Ops CLI inside the same static binary and issues fine-grained scoped Personal Access Tokens (`volo_pat_*`) so Obsidian, PicGo, CI/CD pipelines, and external clients can interact statelessly without browser cookies.

---

## 1. CLI Subcommands Reference

Every subcommand honors the unified configuration hierarchy (`CLI Flags > OS VOLO_* Env > .env File > Safe Defaults`, with `upload_dir` and `geoip_dir` additionally reading persisted Admin DB settings when unpinned by the top 3 layers) and accepts the following common flags:

| Common CLI Flag | Environment Variable | Description |
| :--- | :--- | :--- |
| `--env-file <path>` | `VOLO_ENV_FILE` | Path to `.env` file (default `./.env`) |
| `--listen-addr <host:port>` | `VOLO_LISTEN_ADDR` | TCP listen address (default `0.0.0.0:8080`, mutually exclusive with `--listen-socket`) |
| `--listen-socket <path[,mode]>` | `VOLO_LISTEN_SOCKET` | Unix Domain Socket path (with optional octal mode suffix, e.g., `/run/volo/volo.sock,0660`) |
| `--socket-mode <octal>` / `--socket-group <group>` | `VOLO_SOCKET_MODE` / `VOLO_SOCKET_GROUP` | Unix socket octal permissions (default `0660`) and group ownership (e.g., `www-data`, `nginx`) |
| `--public-url <url>` | `VOLO_PUBLIC_URL` | Canonical public base URL (used for canonical tags, RSS/Podcast, Sitemap, WebAuthn Origin, and email links) |
| `--trusted-proxies <cidrs\|unix>` | `VOLO_TRUSTED_PROXIES` | Comma-separated trusted reverse proxy IPs/CIDRs or `unix` (default `127.0.0.1,::1`) |
| `--admin-path </prefix>` | `VOLO_ADMIN_PATH` | Custom stealth Admin Console base prefix (default `/console`) |
| `--secret-key <secret>` | `VOLO_SECRET_KEY` | Master secret (`>= 16` chars; if empty, auto-generates `0600` `${VOLO_DATA_DIR}/.secret.key`) |
| `--geo-access-enabled=true\|false` | `VOLO_GEO_ACCESS_ENABLED` | Enable or emergency-bypass offline GeoIP access rules |
| `--default-locale <code>` | `VOLO_DEFAULT_LOCALE` | Default fallback locale (e.g., `zh-CN`, `en-US`) |
| `--data-dir` / `--upload-dir` / `--backup-dir` / `--geoip-dir` | `VOLO_DATA_DIR` / `VOLO_UPLOAD_DIR` / `VOLO_BACKUP_DIR` / `VOLO_GEOIP_DIR` | Data, uploaded media, rolling backup, and offline GeoIP database directories |
| `--themes-dir` / `--plugins-dir` / `--locales-dir` | `VOLO_THEMES_DIR` / `VOLO_PLUGINS_DIR` / `VOLO_LOCALES_DIR` | External hot-mountable themes, plugins, and locale packs directories |
| `--db-driver <sqlite\|postgres\|mysql>` | `VOLO_DB_DRIVER` | Database driver (when unset, loads the `/setup` wizard's sealed `${VOLO_DATA_DIR}/db.json` if present) |
| `--db-dsn <dsn>` | `VOLO_DB_DSN` | Database DSN (defaults to `${VOLO_DATA_DIR}/volo.db` on SQLite when empty) |
| `--db-simple-protocol` | `VOLO_DB_SIMPLE_PROTOCOL` | Enable PostgreSQL pure-Go `pgwire` simple query protocol for transaction poolers (Supabase / PgBouncer) |
| `--shutdown-timeout <sec>` | `VOLO_SHUTDOWN_TIMEOUT_SEC` | Graceful shutdown drain timeout in seconds (`1`–`120`, default `15`) |

### 1.1 `volo serve` (Default Command)

Starts the HTTP or Unix Domain Socket server, runs idempotent schema migrations (`v7`), warms the full-text search index and public chrome snapshots, starts the scheduled-article publisher, plugin Cron scheduler, and backup scheduler, and traps `SIGINT` / `SIGTERM` for graceful connection draining and deterministic analytics flushing.

```bash
# Zero-config startup (binds 0.0.0.0:8080 and initializes ./data/volo.db)
./volo serve

# Explicit production flags with stealth admin console prefix
./volo serve \
  --env-file /etc/volo/volo.env \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret
```

### 1.2 `volo admin reset-password` / `volo passwd` (Emergency Password Reset & Session/Token Revocation)

Resets a user's password to a fresh `Argon2id` hash directly from the terminal, clears any legacy password hash, revokes all active browser sessions (`user_sessions`) **and** Personal Access Tokens (`api_tokens`) for that user, and writes an entry to `audit_logs` (`volo passwd` is a built-in alias):

```bash
# Omit --password to auto-generate a strong temporary password (e.g. Volo-<hex>9a) printed to stdout
./volo admin reset-password --user admin --env-file /etc/volo/volo.env

# Explicitly specify a new password (or invoke via the `volo passwd` alias)
./volo admin reset-password --user admin@example.com --password 'MyNewStrongPass#2026' --env-file /etc/volo/volo.env
```

> **Note**: This command accepts only `--user <username|email>` (required) and optional `--password <new_password>` (plus common config flags). On production servers, omitting `--password` is recommended so a one-time random password is generated without exposing credentials in shell history.

### 1.3 `volo admin disable-2fa` (Emergency 2FA Lockout Recovery)

Clears `totp` and `recovery_codes` credentials for a locked-out user and records a local CLI audit log entry:

```bash
./volo admin disable-2fa --user admin@example.com --env-file /etc/volo/volo.env
```

### 1.4 `volo backup create` & `volo backup restore` (Streaming Cross-DB Backup)

See [Legacy Migration & Cross-DB Streaming Backup](./migration-and-backup.md) for architectural details.

```bash
# Stream a full database + media backup archive (--with-media=true by default; defaults to ${VOLO_BACKUP_DIR}/volo-backup-<UTC>.zip)
./volo backup create --output ./volo-backup.zip --with-media=true

# Export only the 33 normalized NDJSON table streams without media files
./volo backup create --output ./volo-db-only.zip --with-media=false

# Pre-verify all NDJSON SHA-256 digests and row counts without modifying the database
./volo backup restore ./volo-backup.zip --dry-run

# Restore seamlessly across SQLite ⇄ PostgreSQL ⇄ MySQL
./volo backup restore ./volo-backup.zip
```

### 1.5 `volo import-legacy` (Streaming Legacy Archive ETL)

Streams and normalizes legacy 15-table `.zip` or `data.json` archives with built-in URL rewriting and storage fallback options:

| Flag | Description |
| :--- | :--- |
| `--dry-run` | Parse and emit a structured JSON migration preview report without writing to DB or disk |
| `--old-site-url <urls>` | Comma- or whitespace-separated legacy site URL prefixes to rewrite into site-relative paths |
| `--new-site-url <url>` | New canonical site base URL override persisted after import |
| `--storage-local` | Force media storage driver back to local disk (`storage.active_driver = "local"`) after import |

```bash
# Validate a legacy 15-table ZIP/JSON archive without writing
./volo import-legacy ./legacy-solo-backup.zip --dry-run

# Execute streaming import while rewriting old domain links and forcing local storage
./volo import-legacy ./legacy-solo-backup.zip \
  --old-site-url "https://old.example.com,http://old.example.com" \
  --new-site-url "https://blog.example.com" \
  --storage-local
```

### 1.6 `volo doctor`, `volo healthcheck`, & `volo version` (Diagnostics & Container Liveness)

```bash
# Human-readable report (checks KeyRing permissions, admin path status, SQLite WAL, multi-DB GeoIP status, FS writability, and proxy CIDRs)
./volo doctor

# Machine-readable JSON diagnostics (exits 1 when unhealthy; ideal for CI & monitoring)
./volo doctor --json

# Probe live /healthz (auto-discovers configured TCP ListenAddr or Unix Domain Socket http://unix/healthz when --url is omitted)
./volo healthcheck
./volo healthcheck --url http://127.0.0.1:8080/healthz

# Print binary version, Go toolchain, and OS/Arch runtime metadata
./volo version
```

---

## 2. Scoped API Tokens (`volo_pat_*`) & Admin REST Integrations

Create scoped Personal Access Tokens with optional expiration dates in **Console -> System & Security -> Account & Security -> API Tokens**.

- Only the `SHA-256` digest (`token_hash`) and 12-character display prefix (`token_prefix`) are persisted in the database; raw secrets are displayed once upon creation.
- Requests carrying `Authorization: Bearer volo_pat_<secret>` authenticate statelessly to `/api/admin/*` without requiring browser session cookies or `X-CSRF-Token`.

### 2.1 Scope Matrix

| Scope | Permitted Operations | Typical Use Case |
| :--- | :--- | :--- |
| `articles:read` | Read profile (`/api/admin/me`), overview metrics, articles, standalone pages, and media list | Read-only dashboards, automated Markdown export scripts |
| `articles:write` | Create, update, and delete articles; manage and reorder categories (`/api/admin/categories/reorder`), tags, series, and bookshelf (`/api/admin/books/reorder`) | Obsidian publishing plugins, Git-to-blog CI pipelines |
| `pages:write` | Create, update, and delete standalone Markdown pages (including custom theme `template` bindings such as `/about`, `/now`) | Automated status page updaters |
| `media:write` | Upload media assets (auto GPS EXIF stripping, orientation normalization, adaptive WebP) | PicGo / ShareX / uPic direct image host |
| `backup:read` | Stream full backup archives (`GET /api/admin/backups/export`) | Offsite encrypted backup pull cron jobs |

> **Privilege Boundary**: System settings mutations (`PATCH /api/admin/settings/{domain}`), user RBAC management, WAF bans, and plugin install/export (`POST /api/admin/plugins/install`, `GET /api/admin/plugins/{id}/export`) strictly require an interactive admin session (enforcing 2FA policy) and can never be invoked with an API token.

### 2.2 Publishing & Updating Articles via `curl` / Obsidian (`articles:write`)

Both `POST /api/admin/articles` and `PUT /api/admin/articles/{id}` accept canonical field names — note that summary, body, and pin flag are **`summary_md`**, **`content_md`**, and **`is_pinned`**:

```bash
export VOLO_URL="https://blog.example.com"
export VOLO_TOKEN="volo_pat_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

curl -X POST "${VOLO_URL}/api/admin/articles" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "Designing a Low-Memory Pure-Go Blog Engine",
    "slug": "pure-go-blog-architecture",
    "category_id": "",
    "tags": ["Go", "Architecture"],
    "summary_md": "Static compilation, SQLite WAL isolation, and revision-tracked caches.",
    "content_md": "## Design Goals\n\nDeliver sub-millisecond reads inside a 35 MiB RSS envelope.",
    "status": "published",
    "allow_comment": true,
    "is_pinned": false
  }'
```

### 2.3 Uploading Media via PicGo / CLI (`media:write`)

```bash
curl -X POST "${VOLO_URL}/api/admin/media/upload" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -F "file=@./screenshot.png"
```

Successful `201 Created` response returning both the full `asset` record and a top-level convenience `url` field (directly compatible with PicGo JSON path `url`):

```json
{
  "asset": {
    "id": "0196ed8c4b2a70008000000001",
    "storage_driver": "local",
    "object_key": "2026/11/0196ed8c4b2a70008000000001.webp",
    "public_url": "/uploads/2026/11/0196ed8c4b2a70008000000001.webp",
    "original_name": "screenshot.png",
    "alt_text": "",
    "mime_type": "image/webp",
    "size_bytes": 84210,
    "width": 1920,
    "height": 1080,
    "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
    "exif_stripped": true,
    "uploader_id": "0196ed80000070008000000001",
    "created_at": 1762627200000
  },
  "url": "/uploads/2026/11/0196ed8c4b2a70008000000001.webp"
}
```

### 2.4 Category/Bookshelf Batch Reordering, Plugin Chain Priority & Cron Endpoints

- **`POST /api/admin/categories/reorder`** & **`POST /api/admin/books/reorder`**: Atomically update `sort_order` in a single transaction according to the submitted ID array:
  ```bash
  curl -X POST "${VOLO_URL}/api/admin/categories/reorder" \
    -H "Authorization: Bearer ${VOLO_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"ids": ["0196ed8c01", "0196ed8c02", "0196ed8c03"]}'
  ```
- **`POST /api/admin/plugins/reorder`** & **`PATCH /api/admin/plugins/{id}/priority`** (requires interactive admin session): Reorder the plugin execution chain or set an exact numeric `priority` (`-10000`–`10000`; lower numbers run earlier across all `before_*` interceptable hooks, lifecycle events, UI slots, and same-type `Provider` chains).
- **`GET /api/admin/plugins/cron`** & **`POST /api/admin/plugins/{id}/trigger-cron`** (requires interactive admin session): Inspect live `CronRegistry` background job metrics (last duration, next run timestamp, recent errors) or trigger a task immediately.
- **`GET /api/admin/plugins/{id}/export`** (requires interactive admin session): Exports an installed plugin directory as a clean `.zip` package (stripping runtime temp files and never bundling database `secret_ciphertext` values), paired with `POST /api/admin/plugins/install?overwrite=true` for in-place upgrades that preserve existing configuration and encrypted secrets.

---

## 3. Public JSON v1 & Machine Distribution Endpoints

| Endpoint | Method | Purpose |
| :--- | :--- | :--- |
| `/healthz` | `GET` / `HEAD` | Process liveness & DB readiness probe: returns `200` `{"status":"ok","database":"ok","uptime_sec":120}` when healthy, or `503` `{"status":"degraded","database":"unavailable","uptime_sec":120}` when DB ping fails |
| `/rss.xml` / `/atom.xml` | `GET` | RSS 2.0 & Atom 1.0 feeds (strong `ETag` / `304`, full privacy redaction of locked posts & `:::lock` blocks) |
| `/podcast.xml` | `GET` | Apple Podcasts / Spotify compatible iTunes Podcast RSS feed |
| `/sitemap.xml` / `/robots.txt` | `GET` | Search engine XML sitemap with `hreflang` alternates (serves `<urlset>` directly up to `10,000` public URLs, and **automatically shards into a `<sitemapindex>` serving `/sitemap-1.xml`, `/sitemap-2.xml`, … when exceeding `10,000` URLs**) & `robots.txt` (omits admin `Disallow` when stealth admin path is active) |
| `/llms.txt` / `/llms-full.txt` | `GET` | Structured LLM index & public Markdown corpus (toggleable via `feed.enable_llms_txt`) |
| `/posts/{slug}.md` | `GET` | Clean raw Markdown endpoint for public posts (controlled by `feed.enable_raw_markdown`; returns `403` when locked) |
| `/api/v1/search?q=<kw>&page=<n>` | `GET` | Paginated two-stage full-text search accepting `q` and `page`, returning highlighted `hits` and pagination metadata with zero lock leakage |
| `/api/v1/i18n/{locale}` | `GET` | Resolved public locale bundle merged with custom overrides (`ETag` negotiated) |
| `/api/v1/pow/challenge` | `GET` | Self-hosted `HMAC + SHA-256` Proof-of-Work challenge issuer |
| `/api/v1/guest/passport` | `GET` / `POST` | Issue or self-heal the 3-tier signed Guest Passport (`__volo_gp`) |
| `/api/v1/reader/profile` | `PATCH` | Self-service display name, avatar, and bio updates for signed-in readers (validated through the union of host blocked keywords, `provider:antispam` plugins, and `user.before_update_profile` chained interception) |
| `/api/v1/articles/{id}/unlock` | `POST` | Verify a post or standalone page (`page:<id>`) password and issue a scoped unlock cookie |
| `/api/v1/comments` | `GET` / `POST` | Fetch paginated threaded comments or submit a PoW-verified, antispam-screened comment |
| `/api/v1/reactions` | `POST` | Lightweight emoji reactions on articles and comments |
