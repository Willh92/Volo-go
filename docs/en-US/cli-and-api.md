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
| `--listen-socket <path[,mode]>` (alias `--unix-socket`) | `VOLO_LISTEN_SOCKET` | Unix Domain Socket path (with optional octal mode suffix, e.g., `/run/volo/volo.sock,0660`) |
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
| `--tls-cert <file>` | `VOLO_TLS_CERT_FILE` | Native HTTPS certificate chain PEM (enables TLS together with `--tls-key`; hot-reloaded on change; not combinable with `--listen-socket`) |
| `--tls-key <file>` | `VOLO_TLS_KEY_FILE` | Native HTTPS private key PEM |
| `--tls-min-version <1.2\|1.3>` | `VOLO_TLS_MIN_VERSION` | Minimum TLS version (default `1.2`) |
| `--tls-http-redirect-addr <addr>` | `VOLO_TLS_HTTP_REDIRECT_ADDR` | Optional plain-HTTP listener (e.g. `:80`) that `308`-redirects to HTTPS |

### 1.1 `volo serve` (Default Command)

Starts the HTTP or Unix Domain Socket server, runs idempotent schema migrations (`v9`), warms the full-text search index and public chrome snapshots, starts the scheduled-article publisher, plugin Cron scheduler, and backup scheduler, and traps `SIGINT` / `SIGTERM` for graceful connection draining and deterministic analytics + plugin KV flushing.

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

# Read-only /healthz probe (never creates .secret.key; auto-discovers TCP ListenAddr or Unix Socket http://unix/healthz)
./volo healthcheck
./volo healthcheck --url http://127.0.0.1:8080/healthz

# Print binary version, Go toolchain, and OS/Arch runtime metadata
./volo version
```

---

## 2. Scoped API Tokens (`volo_pat_*`) & Admin REST Integrations

Create scoped Personal Access Tokens with optional expiration dates in **Console -> System & Security -> Account & Security -> API Tokens**.

- Only the `SHA-256` digest (`token_hash`) and 12-character display prefix (`token_prefix`) are persisted in the database; raw secrets are displayed once upon creation.
- Requests carrying `Authorization: Bearer volo_pat_<secret>` authenticate statelessly to `/api/admin/*` without requiring browser session cookies or `X-CSRF-Token` (browser Admin Console session bootstrap calls to `GET /api/admin/me` pass `X-Volo-Console: 1` to obtain the CSRF token).

### 2.1 Scope Matrix

| Scope | Permitted Operations | Typical Use Case |
| :--- | :--- | :--- |
| `articles:read` | Read profile (`/api/admin/me`), overview metrics, articles, standalone pages, and media list | Read-only dashboards, automated Markdown export scripts |
| `articles:write` | Create, update, and delete articles; manage and reorder categories (`/api/admin/categories/reorder`), tags, series, and bookshelf (`/api/admin/books/reorder`) | Obsidian publishing plugins, Git-to-blog CI pipelines |
| `pages:write` | Create, update, and delete standalone Markdown pages (including custom theme `template` bindings such as `/about`, `/now`) | Automated status page updaters |
| `media:write` | Upload media assets (auto GPS EXIF stripping, orientation normalization, adaptive WebP) | PicGo / ShareX / uPic direct image host |
| `backup:read` | Stream full backup archives (`GET /api/admin/backups/export`) | Offsite encrypted backup pull cron jobs |

> **Privilege Boundary**: System settings mutations (`PATCH /api/admin/settings/{domain}`), user ban/mute/RBAC management (`PATCH /api/admin/users`), WAF bans, and plugin install/export strictly require an interactive admin session (enforcing 2FA policy) and can never be invoked with an API token.

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

### 2.4 Account & IP Ban Governance, Log Retention, Batch Reordering & Plugin Chain Endpoints

- **`GET /api/admin/users`** & **`PATCH /api/admin/users`** (requires interactive admin session): Discourse-style account moderation supporting `action: "ban" | "unban" | "mute" | "unmute" | "shield"`, `duration_hours` (`0` = indefinite, `> 0` = timed auto-expiry), `reason`, `shield_comments` (`boolean`), and `shield_text` (non-destructively masking historical comments until unbanned/unmuted or expired).
- **`GET / POST /api/admin/security/ip-bans`**, **`PATCH / DELETE /api/admin/security/ip-bans/{id}`**, **`POST /api/admin/security/ip-bans/{id}/unban`**, **`POST /api/admin/security/ip-bans/batch`**, & **`POST /api/admin/security/ip-bans/purge-expired`** (requires interactive admin session): Full IP/CIDR ban lifecycle returning `counts` (`active`, `expired`, `all`), `source` (`manual` / `auto_waf`), cumulative `occurrences`, and offline GeoIP `location`. Single/batch `unban` early-expires bans while preserving historical records; `PATCH` and batch `set_duration` update `expires_minutes` (`0` = permanent, `> 0` = minutes from now) or `note` (and re-ban expired entries), and single-record `PATCH` also supports explicitly adjusting or resetting cumulative `occurrences` (`0`–`1000000`); `DELETE`, batch `delete`, and `purge-expired` physically remove rows. Comment moderation (`POST /api/admin/comments/{id}/ban-ip` and `POST /api/admin/comments/batch` with `action: "ban_ip"`) accepts matching `expires_minutes`.
- **`DELETE /api/admin/audit-logs`** & **`DELETE /api/admin/plugins/{id}/logs`** (requires interactive admin session): One-click physical clear for admin audit logs and per-plugin runtime logs (alongside automatic scheduled cleanup controlled by `security.ip_ban_retention_days`, `security.audit_log_retention_days`, and `security.plugin_log_retention_days`, where `0` = retain indefinitely).
- **`POST /api/admin/categories/reorder`** & **`POST /api/admin/books/reorder`**: Atomically update `sort_order` in a single transaction.
- **`POST /api/admin/plugins/reorder`** & **`PATCH /api/admin/plugins/{id}/priority`** (requires interactive admin session): Reorder the plugin execution chain or set an exact numeric `priority` (`-1000`–`10000`).
- **`GET /api/admin/plugins/cron`**, **`POST /api/admin/plugins/{id}/trigger-cron`**, and **`GET /api/admin/plugins/{id}/export`**.

---

## 3. Real-Time Non-Destructive ImageView Reference (`/uploads/...?...`)

Unparameterized `/uploads/...` URLs stream the original file directly with zero overhead. Appending query parameters enables real-time non-destructive rendering supporting **both Qiniu Cloud native path parameters** (e.g., `?imageView2/1/w/960/h/540/format/webp/q/85/interlace/1/strip/ignore-error/1`) and **standard key-value parameters** (e.g., `?mode=1&w=960&h=540&format=webp&interlace=1&q=85&colors=128&strip&ignore-error=1`) in any combination — **never modifying source files on disk or S3**, backed by `singleflight` + `8 MiB` byte-bounded LRU + strong `ETag` / `304` negotiation (and recognized by orphan media scanning and CLS dimension prediction):

| Parameter | Meaning | Supported Values / Behavior |
| :--- | :--- | :--- |
| `mode` | Resize / crop mode | `0` (fit long edge `w` / short edge `h`), `1` (center crop `w×h`), `2` (fit inside max `w×h`), `3` (min `w×h` outer fit), `4` (long/short edge fit + pad), `5` (long/short edge outer fit + center crop) |
| `w` / `h` | Width (or long edge) / Height (or short edge) | `1`–`4096` pixels |
| `format` | Output format | `jpg` (`jpeg`), `png`, `webp`, `avif` (pure-Go builds gracefully emit `image/webp` with `X-Volo-Format-Fallback: avif->webp` when `avif` is requested) |
| `interlace` | Progressive / interlaced output | `0`, `1` |
| `q` | Encoding quality | `1`–`100` (default `85`) |
| `colors` | GIF/PNG palette color count | `2`–`256` quantized palette colors |
| `strip` / `meta-keep-list` | Metadata stripping | Re-encoded outputs strip metadata by default |
| `ignore-error` | Error fallback | `0`, `1` (when `1`, invalid parameters or non-raster assets stream the original file cleanly instead of `400`) |

---

## 4. Public JSON v1 & Machine Distribution Endpoints

| Endpoint | Method | Purpose |
| :--- | :--- | :--- |
| `/healthz` | `GET` / `HEAD` | Process liveness & DB readiness probe (`200` `{"status":"ok","database":"ok",...}` or `503`) |
| `/rss.xml` / `/atom.xml` / `/podcast.xml` | `GET` | RSS 2.0, Atom 1.0, and Apple/Spotify iTunes Podcast feeds (honoring `feed.item_count`, strong `ETag` / `304`, and lock redaction) |
| `/sitemap.xml` / `/robots.txt` | `GET` | XML sitemap (auto-shards into `<sitemapindex>` above `10,000` URLs) & `robots.txt` (omits admin `Disallow` when stealth admin path is active) |
| `/llms.txt` / `/llms-full.txt` | `GET` | Structured LLM index & public Markdown corpus (toggleable via `feed.enable_llms_txt`) |
| `/posts/{slug}.md` (plus `/archives/{slug}.md`, `/p/{slug}.md`, `/{year}/{month}/{slug}.md`) | `GET` | Clean raw Markdown endpoint matching active `seo.post_permalink_style` (controlled by `feed.enable_raw_markdown`) |
| `/api/v1/search?q=<kw>&page=<n>` | `GET` | Paginated two-stage full-text search with zero lock leakage |
| `/api/v1/i18n/{locale}` | `GET` | Resolved public locale bundle merged with custom overrides (`ETag` negotiated) |
| `/api/v1/pow/challenge` / `/api/v1/guest/passport` | `GET` / `POST` | Self-hosted `HMAC + SHA-256` PoW challenge & 3-tier Guest Passport (`__volo_gp`) |
| `/api/v1/reader/profile` | `PATCH` | Self-service display name, avatar, and bio updates for signed-in readers |
| `/api/v1/articles/{id}/unlock` | `POST` | Verify a post or standalone page (`page:<id>`) password and issue a scoped unlock cookie |
| `/api/v1/comments` | `GET` / `POST` | Fetch threaded comments (including caller `comment_min_length`, `comment_max_length`, `comment_max_lines`, `viewer_muted`, and shielded comment placeholders) or post a comment |
| `/api/v1/comments/preview` | `POST` | Sanitized live Markdown preview for the public reply editor (enforcing identical HTML5 AST & height-bomb defenses) |
| `/api/v1/reactions` | `POST` | Lightweight emoji reactions on articles and comments (blocked when muted/banned) |
