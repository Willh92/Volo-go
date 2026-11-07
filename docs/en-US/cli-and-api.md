# CLI & API Token Reference

> 中文版本：[CLI 运维命令与 API 令牌接入指南](../zh-CN/cli-and-api.md)

`Volo-go` embeds a full Swiss-Army CLI inside the same static binary and issues fine-grained scoped Personal Access Tokens (`volo_pat_*`) so Obsidian, PicGo, CI/CD pipelines, and external scripts can interact statelessly without browser cookies.

---

## 1. CLI Subcommands Reference

Every subcommand honors the 4-layer configuration hierarchy (`CLI Flags > VOLO_* Env > .env File > Safe Defaults`) and accepts `--env-file /path/to/volo.env`.

### 1.1 `volo serve` (Default Command)

Starts the HTTP or Unix Domain Socket server, runs idempotent schema migrations (`v2`), warms full-text search and public chrome snapshots, and traps `SIGINT` / `SIGTERM` for graceful connection draining and buffered analytics flushing.

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

### 1.2 `volo admin reset-password` (Emergency Password Reset & Session Revocation)

Resets a user's password to a fresh `Argon2id` hash directly from the terminal, clears any legacy hash, revokes all active `user_sessions` for that account, and writes an entry to `audit_logs`:

```bash
# Generate a strong temporary password and print it to stdout
./volo admin reset-password --user admin --env-file /etc/volo/volo.env

# Read the new password safely from stdin (avoids shell history exposure)
printf '%s\n' 'MyNewStrongPass#2026' | ./volo admin reset-password --user admin --password-stdin
```

### 1.3 `volo admin disable-2fa` (Emergency 2FA Lockout Recovery)

Clears `totp` and `recovery_codes` credentials for a locked-out user and records an audit log entry:

```bash
./volo admin disable-2fa --user admin@example.com --env-file /etc/volo/volo.env
```

### 1.4 `volo backup create` & `volo backup restore` (Streaming Cross-DB Backup)

See [Legacy Migration & Cross-DB Streaming Backup](./migration-and-backup.md) for architectural details.

```bash
# Stream a full database + media backup archive (constant O(1) memory)
./volo backup create --output ./volo-backup.zip --with-media=true

# Pre-verify all NDJSON SHA-256 digests and row counts without modifying DB
./volo backup restore ./volo-backup.zip --dry-run

# Restore across SQLite ⇄ PostgreSQL ⇄ MySQL seamlessly
./volo backup restore ./volo-backup.zip
```

### 1.5 `volo import-legacy` (Streaming Legacy Archive ETL)

```bash
# Validate a legacy 15-table ZIP/JSON archive without writing
./volo import-legacy ./legacy-solo-backup.zip --dry-run

# Execute streaming normalization and preserve all historical .html permalinks
./volo import-legacy ./legacy-solo-backup.zip
```

### 1.6 `volo doctor` & `volo healthcheck` (Diagnostics & Container Liveness)

```bash
# Human-readable environment, KeyRing, SQLite WAL, and filesystem report
./volo doctor

# Machine-readable JSON output for monitoring pipelines
./volo doctor --json

# Probe live /healthz over TCP or Unix Domain Socket (built for Distroless containers)
./volo healthcheck
```

---

## 2. Scoped API Tokens (`volo_pat_*`) & External Integrations

Create scoped Personal Access Tokens with optional expiration dates in **Console -> System & Security -> Account & Security -> API Tokens**.

- Only the `SHA-256` digest (`token_hash`) and 12-character display prefix (`token_prefix`) are persisted in the database; raw secrets are shown once upon creation.
- Requests carrying `Authorization: Bearer volo_pat_<secret>` authenticate statelessly without requiring browser session cookies or `X-CSRF-Token`.

### 2.1 Scope Matrix

| Scope | Permitted Operations | Typical Use Case |
| :--- | :--- | :--- |
| `articles:read` | Read profile (`/api/admin/me`), overview stats, articles, standalone pages, and media list | Read-only dashboards, automated Markdown export scripts |
| `articles:write` | Create, update, and delete articles; manage categories, tags, series, and bookshelf | Obsidian publishing plugins, Git-to-blog CI pipelines |
| `pages:write` | Create, update, and delete standalone Markdown pages (`/about`, `/now`) | Automated status page updaters |
| `media:write` | Upload media assets (auto GPS EXIF stripping, orientation fix, adaptive WebP) | PicGo / ShareX / uPic direct image host |
| `backup:read` | Stream full backup archives (`GET /api/admin/backups/export`) | Offsite encrypted backup pull cron jobs |

> **Privilege Boundary**: System settings, user RBAC management, WAF bans, and plugin installation strictly require an interactive staff session (enforcing 2FA policy) and can never be invoked with an API token.

### 2.2 Publishing an Article via `curl` / Obsidian (`articles:write`)

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
    "summary_markdown": "Static compilation, SQLite WAL isolation, and revision-tracked caches.",
    "content_markdown": "## Design Goals\n\nDeliver sub-millisecond reads inside a 35 MiB RSS envelope.",
    "status": "published",
    "allow_comment": true,
    "pinned": false
  }'
```

### 2.3 Uploading Media via PicGo / CLI (`media:write`)

```bash
curl -X POST "${VOLO_URL}/api/admin/media/upload" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -F "file=@./screenshot.png"
```

Example response (EXIF stripped, dimensions extracted, WebP encoded):

```json
{
  "id": "0196ed8c4b2a70008000000001",
  "url": "/uploads/2026/11/0196ed8c4b2a70008000000001.webp",
  "mime_type": "image/webp",
  "width": 1920,
  "height": 1080,
  "size_bytes": 84210
}
```

---

## 3. Public JSON v1 & Machine Distribution Endpoints

| Endpoint | Method | Purpose |
| :--- | :--- | :--- |
| `/healthz` | `GET` | Process liveness & read-only DB probe (`{"status":"ok","db":"ok"}`) |
| `/rss.xml` / `/atom.xml` | `GET` | Full-text RSS 2.0 & Atom 1.0 feeds (strong `ETag` / `304`, privacy-redacted) |
| `/podcast.xml` | `GET` | Apple Podcasts / Spotify compatible iTunes Podcast RSS feed |
| `/sitemap.xml` / `/robots.txt` | `GET` | Search engine XML sitemap (posts + published standalone pages) & robots directives |
| `/llms.txt` / `/llms-full.txt` | `GET` | Structured LLM index & public Markdown corpus (toggleable in SEO settings) |
| `/posts/{slug}.md` | `GET` | Clean raw Markdown endpoint for public posts (returns `403` when locked) |
| `/api/v1/search?q=<keyword>` | `GET` | Two-stage full-text search with highlighted snippets and zero lock leakage |
| `/api/v1/pow/challenge` | `GET` | Self-hosted `HMAC + SHA-256` Proof-of-Work challenge issuer |
| `/api/v1/guest/passport` | `POST` | Issue or self-heal the dual-track Guest Passport (`__volo_gp`) |
| `/api/v1/comments` | `GET` / `POST` | Fetch threaded comments or submit a PoW-verified comment |
| `/api/v1/reactions` | `POST` | Lightweight emoji reactions on articles and comments |
