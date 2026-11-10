# Architecture & Performance Internals

> 中文版本：[架构设计与性能内幕](../zh-CN/architecture.md)

`Volo-go` is a pure-Go (`CGO_ENABLED=0`, Go `1.27.3`, `GOEXPERIMENT=nojsonv2`) sovereign blog engine engineered from scratch to deliver multi-database dialect portability, real-time non-destructive image rendering, a 10-surface zero-leak privacy perimeter, first-party pure-Go ES6 plugin sandboxing, and microsecond-class read throughput inside a **strictly `< 21 MiB` static single binary across all 6 release targets** (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, `windows/arm64`) and **`< 35 MiB` resident memory (`VmRSS`)** envelope.

---

## 1. Layered Runtime Topology

```
┌─────────────────────────────────────────────────────────────────────────┐
│                HTTP / Unix Domain Socket Ingress Layer                  │
│  WAFEngine (CIDR Bans / Scanner Honeypots / Subnet RL / Multi-DB GeoIP) │
│  Dynamic Stealth Admin Gate (Hot-Reload Prefix + Indistinguishable 404) │
│  RedirectGuardian (O(1) In-Memory Snapshot: Custom Rules + .html 301s)  │
│  BREACH-Safe Dynamic Gzip (Compresses Public HTML/JSON, Skips Secrets)  │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        ▼                       ▼                       ▼
┌───────────────┐     ┌───────────────────┐     ┌──────────────────┐
│ Public SSR    │     │ Studio Admin SPA  │     │ Machine & AI     │
│ volo-studio & │     │ Preact + Signals  │     │ RSS/Atom/Podcast │
│ ./themes/*    │     │ IR/WYSIWYG/SV CM6 │     │ Sharded Sitemap  │
└───────┬───────┘     └─────────┬─────────┘     └────────┬─────────┘
        └───────────────────────┼────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                   Domain Services & Security Perimeter                  │
│  • ContentVisibilityGuard (10-Surface Redaction + 4 MiB Render LRU)     │
│  • SearchIndex (8 MiB Paginated Two-Stage Index + UTF-8 Snippets)       │
│  • Auth & RBAC (Argon2id / Passkey / TOTP / Ban-Mute-Shielding / PATs)  │
│  • GuestPassport & PoW (3-Tier Self-Healing Passport + SHA-256 PoW)     │
│  • Media & ImageView (<=64MP / EXIF Strip / Real-Time ?imageView2 LRU)  │
│  • Plugin Manager (Pure-Go Frozen-Proto VM + Priority Chains + AsyncKV) │
└───────────────────────────────┬─────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│              Multi-Dialect SQL & Revision-Tracked Store (`sqlx`)        │
│  • Dialect: SQLite (modernc WAL Read/Write Split) | PG (pgwire) | MySQL │
│  • Non-Generic Scan Plans + PG Rebind Cache + Atomic DataRevision Bus   │
│  • Write-Behind View & KV Buffers (Real-Time Overlay + Periodic Flush)  │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Normalized Relational Schema (`v8`: 33 Data Tables + 9 Settings Domains)

`Volo-go` replaces fragmented key-value JSON blobs and indirect tag-derived categorization with a normalized relational schema (`internal/db/migrate/schema.go`, current schema version **`v8`**, tracked via `schema_migrations` across idempotent steps `v1`–`v8`, with **33 normalized data tables** in `CanonicalTables`):

| Domain | Tables (33 Data Tables + `schema_migrations`) | Key Design Choices |
| :--- | :--- | :--- |
| **System Settings (1)** | `settings` | Hosts all **9 hot-reloadable settings domains** (`site`, `reading`, `feed`, `seo`, `storage`, `appearance`, `notifications`, `security`, `backup`) with optimistic-locked partial `PATCH` and column-level `AES-256-GCM` + AAD secret sealing in `secret_ciphertext` |
| **Identity & Access (4)** | `users`, `user_credentials`, `user_sessions`, `api_tokens` | Physical `admin` / `editor` / `reader` RBAC separation; `v8` adds permanent/timed suspension (`ban_until`), permanent/timed mute (`muted_until`), `ban_reason`, and reversible historical comment shielding (`shield_comments` / `shield_text`); Passkeys, TOTP, and recovery codes live in `user_credentials`; scoped `volo_pat_*` tokens |
| **Content & Taxonomy (8)** | `categories`, `tags`, `articles`, `article_tags`, `article_revisions`, `article_backlinks`, `pages`, `nav_items` | Direct `articles.category_id` foreign binding; standalone Markdown `pages` (with custom `template` binding) decoupled from hierarchical `nav_items` so pages never pollute post feeds or archives |
| **Series & Bookshelf (4)** | `series`, `series_articles`, `books`, `book_chapters` | First-class relational tables for ordered multi-part series, `reading_status`, batch drag-and-drop reordering, and cached multi-source book chapter trees (`book_chapters`) |
| **Engagement & Syndication (6)** | `comments`, `reactions`, `links`, `follows`, `follow_items`, `redirects` | Comments record `guest_id_hash` + `env_fp_hash` for one-click claiming, enforce per-identity rune/line caps with vertical height-bombing defense; `reactions` store emoji counters; `follows` / `follow_items` aggregate feeds; `redirects` + `legacy_permalink` guarantee zero `404`s |
| **Media, Webhooks, Security, Analytics & Plugins (10)** | `media_assets`, `webhooks`, `webhook_deliveries`, `analytics_daily`, `analytics_top_items`, `security_ip_bans`, `security_geo_rules`, `audit_logs`, `plugins`, `plugin_kv` | `media_assets` tracks dimensions, `alt_text`, and `exif_stripped` (with query-string-safe orphan scanning for `?imageView2` URLs); `webhook_deliveries` stores bounded delivery history; `security_ip_bans` / `security_geo_rules` power WAF & multi-DB GeoIP; `plugins` / `plugin_kv` isolate plugin secrets and KV entries |

### All 9 Hot-Reload Settings Domains (`internal/domain/settings.go`)

| Settings Domain | Core Responsibilities & Representative Keys |
| :--- | :--- |
| `site` | Title/subtitle, default locale, timezone, notice board, social links, license template, global `custom_vars` map |
| `reading` | Timeline `list_style` (`summary \| title_only \| full`), pagination counts, PJAX, instant search, comment access & smart moderation, per-identity comment rune/line bounds (`comment_min_length`, `comment_max_length_{guest,user,staff}`, `comment_max_lines_{guest,user,staff}`), PoW difficulty, CDN prefixes |
| `feed` | RSS/Atom output mode (`summary \| full`), `item_count` (shared across RSS, Atom, and Podcast), `/llms.txt` toggle, `.md` raw view toggle, Apple/Spotify `podcast` channel metadata |
| `seo` | Meta keywords/description, default OG image, 4 canonical `post_permalink_style` templates (`/posts/{slug}`, `/archives/{slug}`, `/p/{slug}`, `/{year}/{month}/{slug}` with instant cache & search rebuild), IndexNow key, cached GitHub showcase repos |
| `storage` | Active driver (`local \| s3`), custom local media directory `local_dir`, `max_upload_mb`, EXIF stripping, adaptive WebP quality, S3/R2/MinIO settings & encrypted `s3_secret_key` |
| `appearance` | Desktop `active_theme`, optional `mobile_theme`, `accent` palette, `default_mode`, site-wide `custom_css`, per-theme `theme_configs` |
| `notifications` | Pure-Go SMTP client configuration & encrypted `smtp_password`, admin comment alerts, and reader reply email notifications (including `NotifyCommentReplyApproved` when pending replies are approved) |
| `security` | Risk control & live `WAF.UpdateConfig` window limits, offline GeoIP toggle & hot-reloadable `geoip_dir`, mandatory TOTP scope, `totp_remember_days` (`0` disables device remember) & `totp_window_steps`, zero-restart stealth path `dynamic_login_path` (`admin_path`), and encrypted `share_token_secret` |
| `backup` | Scheduled local rolling backup interval/retention/media toggle, remote replication (`s3 \| webdav`), and encrypted `webdav_password` / `s3_secret_key` |

---

## 3. Pure-Go Multi-Dialect SQL & SQLite WAL Isolation

1. **Unified Pure-Go Dialect Layer (`internal/db/dialect`)**:
   - Rebinds `?` placeholders to PostgreSQL `$1..$N` via a bounded lock-free `sync.Map` cache (**`21.8 ns/op`, `0 allocs/op`** on hot queries) and supports `--db-simple-protocol` (`VOLO_DB_SIMPLE_PROTOCOL`) for PgBouncer / Supabase transaction poolers;
   - Abstracts portable `UpsertSQL` (`ON CONFLICT DO UPDATE` vs `ON DUPLICATE KEY UPDATE`), `PaginateSQL`, and DDL `TypeMap`.
2. **Physical SQLite Read/Write Pool Split (`internal/db/sqlx`)**:
   - `WriteDB` runs with `MaxOpenConns=1`, `_txlock=immediate`, and `busy_timeout=5000`, eliminating concurrent writer `SQLITE_BUSY` deadlocks;
   - `ReadDB` enforces `PRAGMA query_only = ON` and `cache_size = -4096` (`4 MiB` bounded page cache), guaranteeing at the database driver level that read-only queries never mutate disk state.
3. **Non-Generic Zero-Closure Struct Scanning (`GetInto` / `SelectInto`)**: Struct column plans are precompiled per `(reflect.Type, Columns)` pair without per-row reflection closures or generic monomorphization bloat.

---

## 4. 10-Surface Zero-Leak Privacy Perimeter (`ContentVisibilityGuard`)

Password-protected posts (`password_ciphertext`) and partial encrypted blocks (`:::lock`) are enforced fail-closed across all **10 outbound surfaces** in `internal/visibility/guard.go`:

1. **Feed Card Excerpts & Full Timelines**: Locked posts never auto-excerpt or render protected body text (even when `reading.list_style = "full"`);
2. **Hero & OpenGraph Images**: Locked posts never expose cover or inline image URLs, falling back to deterministic local geometric SVG covers;
3. **Previous / Next Post Links**: Expose minimal title/permalink metadata only — never body previews;
4. **Comment API & Recent Comments**: `/api/v1/comments` returns `403 Forbidden` until unlocked, and sidebar recent comments exclude locked posts;
5. **Paginated Two-Stage Full-Text Search**: Both the `8 MiB` in-memory index and SQL fallback strip locked posts and `:::lock` blocks prior to indexing/matching;
6. **RSS / Atom / Podcast Feeds**: Password-protected posts are excluded from syndication XML; partial `:::lock` blocks render localized lock notices;
7. **Sitemap (Including Sharded Indexes) & IndexNow**: Password-protected posts are omitted from `/sitemap.xml` and any `/sitemap-N.xml` shards, excluded from IndexNow pings, and served with `noindex, nofollow, noarchive` + `Cache-Control: private, no-store`;
8. **Bidirectional Backlinks (`[[slug]]`)**: Locked posts neither emit outbound backlinks nor appear in target post backlink lists;
9. **`/llms.txt` & `.md` Raw Endpoints**: Excluded from LLM corpus indexes, and unauthenticated `.md` requests return `403`;
10. **Plugin Hooks & Federation Dispatch**: Plugins without explicit `articles:read_protected` permission and automatic `federation` dispatches never receive protected payloads or nested `:::lock` content, and `render.post_html` / custom directive outputs are re-sanitized through the single-pass HTML5 AST sanitizer.

---

## 5. Zero-Stale Revision-Tracked Caches, Real-Time ImageView & Memory Budget

To achieve microsecond response times within a strict `< 35 MiB` `VmRSS` ceiling without external Redis dependencies, `Volo-go` pairs **`Store.DataRevision()`** with **`SettingsManager.Revision()`**:

- **Instant Invalidation on Every Mutation**: Every foreground `ExecContext` / `WithTx` atomically increments `dataRev`. The O(1) redirect snapshot, public chrome snapshot, and pre-compressed RSS/Atom/Podcast/Sitemap/LLMs documents (`CachedDocument`, capped at `2 MiB`) validate `(dataRev, settingsRev)` on read — guaranteeing **zero stale reads** while serving strong `ETag` / `304 Not Modified` responses.
- **Real-Time Non-Destructive ImageView (`ImageViewService`)**: Unparameterized `/uploads/...` requests stream source files directly with zero overhead; parameterized `?imageView2/...` or KV requests coalesce concurrent transforms via `singleflight` into a strict `8 MiB` byte-bounded LRU cache keyed on `(path, mtime, size, spec)` + `304 ETag` negotiation without mutating source files on disk or S3.
- **Write-Behind Analytics & Plugin KV Buffers (`viewAnalyticsBuffer` & `kvBufferHub`)**: High-frequency view increments and `volo.kv.incrAsync` counters coalesce in memory and flush deterministically in periodic batches and on `SIGTERM` shutdown.
- **Bounded Resident Memory Allocation (Measured Warm `VmRSS = 24.23 MiB < 35.00 MiB`)**:
  - Go runtime, HTTP stack, zero-reflection `tinyxml`, embedded assets & pooled pure-Go ES6 VMs: `~11.5 MiB`
  - SQLite WAL page cache (`cache_size = -4096`): `<= 4 MiB`
  - Two-stage full-text search index (`SearchIndex`): `<= 8 MiB`
  - Markdown render LRU (`<= 4 MiB`), on-demand ImageView LRU (`<= 8 MiB`), and pre-compressed feed cache (`<= 2 MiB`).
