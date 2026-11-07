# Architecture & Performance Internals

> 中文版本：[架构设计与性能内幕](../zh-CN/architecture.md)

`Volo-go` is a pure-Go (`CGO_ENABLED=0`) sovereign blog engine engineered from scratch to deliver multi-database dialect portability, a 10-surface zero-leak privacy perimeter, dual-mode plugin sandboxing, and microsecond-class read throughput inside a **`< 20 MiB` static single binary** and **`< 35 MiB` resident memory (`VmRSS`)** envelope.

---

## 1. Layered Runtime Topology

```
┌─────────────────────────────────────────────────────────────────────────┐
│                HTTP / Unix Domain Socket Ingress Layer                  │
│  WAFEngine (CIDR Bans / Scanner Honeypots / Subnet RL / Offline GeoIP)  │
│  RedirectGuardian (O(1) In-Memory Snapshot: Custom Rules + .html 301s)  │
│  BREACH-Safe Dynamic Gzip (Compresses Public HTML/JSON, Skips Secrets)  │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        ▼                       ▼                       ▼
┌───────────────┐     ┌───────────────────┐     ┌──────────────────┐
│ Public SSR    │     │ Studio Admin SPA  │     │ Machine & AI     │
│ volo-studio & │     │ Preact + Signals  │     │ RSS/Atom/Podcast │
│ ./themes/*    │     │ Lazy-Loaded CM6   │     │ Sitemap / llms   │
└───────┬───────┘     └─────────┬─────────┘     └────────┬─────────┘
        └───────────────────────┼────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                   Domain Services & Security Perimeter                  │
│  • ContentVisibilityGuard (10-Surface Redaction + 4 MiB Render LRU)     │
│  • SearchIndex (8 MiB Two-Stage Index + Zero-Slice UTF-8 Snippets)      │
│  • Auth & RBAC (Argon2id / Passkey / TOTP / Device Sessions / PATs)     │
│  • GuestPassport & PoW (3-Tier Self-Healing Passport + SHA-256 PoW)     │
│  • Media Pipeline (<=64MP Bomb Guard / EXIF Strip / WebP / SigV4 S3)    │
│  • Plugin Manager (Pooled goja VMs + HMAC Webhooks + Circuit Breakers)  │
└───────────────────────────────┬─────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│              Multi-Dialect SQL & Revision-Tracked Store (`sqlx`)        │
│  • Dialect: SQLite (modernc WAL Read/Write Split) | PG (pgx/v5) | MySQL │
│  • Zero-Closure Scan Plans + PG Rebind Cache + Atomic DataRevision Bus  │
│  • Write-Behind View Buffer (Real-Time Overlay + 2s/Shutdown Flush)     │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Normalized Relational Schema (23 Core Tables)

`Volo-go` replaces fragmented key-value JSON blobs and indirect tag-derived categorization with a normalized relational schema (`internal/db/migrate/schema.go`, current schema version `v2`):

| Domain | Tables | Key Design Choices |
| :--- | :--- | :--- |
| **Identity & Access** | `users`, `user_credentials`, `user_sessions`, `api_tokens`, `email_verifications` | Physical `admin` / `editor` / `reader` RBAC separation; multi-device named Passkeys & TOTP in dedicated rows; fine-grained scoped PATs |
| **Content & Taxonomy** | `categories`, `tags`, `articles`, `article_tags`, `article_revisions`, `article_backlinks`, `pages`, `nav_items` | Direct `articles.category_id` foreign binding; standalone Markdown `pages` decoupled from `nav_items` so pages never pollute post feeds or archives |
| **Series & Bookshelf** | `series`, `series_articles`, `books` | First-class relational tables for ordered multi-part series and cached multi-source book TOCs |
| **Engagement & Routing** | `comments`, `links`, `follows`, `follow_items`, `redirects` | Comments record `guest_id_hash` + `env_fp_hash` for one-click claiming upon registration; `redirects` + `legacy_permalink` guarantee zero `404`s |
| **System & Extensions** | `media_assets`, `settings`, `webhooks`, `analytics_daily`, `analytics_top_items`, `plugin_kv`, `security_ip_bans`, `audit_logs` | Optimistic-locked domain `PATCH` settings with column-level `AES-256-GCM` secret sealing; zero-PII daily PV/UV rollups |

---

## 3. Pure-Go Multi-Dialect SQL & SQLite WAL Isolation

1. **Unified Pure-Go Dialect Layer (`internal/db/dialect`)**:
   - Rebinds `?` placeholders to PostgreSQL `$1..$N` via a bounded lock-free `sync.Map` cache (**`21.8 ns/op`, `0 allocs/op`** on hot queries);
   - Abstracts portable `UpsertSQL` (`ON CONFLICT DO UPDATE` vs `ON DUPLICATE KEY UPDATE`), `PaginateSQL`, and DDL `TypeMap`.
2. **Physical SQLite Read/Write Pool Split (`internal/db/sqlx`)**:
   - `WriteDB` runs with `MaxOpenConns=1`, `_txlock=immediate`, and `busy_timeout=5000`, eliminating concurrent writer `SQLITE_BUSY` deadlocks;
   - `ReadDB` enforces `PRAGMA query_only = ON` and `cache_size = -4096` (`4 MiB` bounded page cache), guaranteeing at the database engine level that read-only queries never mutate disk state.
3. **Zero-Closure Struct Scanning**: Struct column plans are precompiled per `(reflect.Type, Columns)` pair and scanned via preallocated index slices without per-row reflection closures.

---

## 4. 10-Surface Zero-Leak Privacy Perimeter (`ContentVisibilityGuard`)

Password-protected posts (`password_ciphertext`) and partial encrypted blocks (`:::lock`) are enforced fail-closed across all **10 outbound surfaces** in `internal/visibility/guard.go`:

1. **Feed Card Excerpts**: Locked posts never auto-excerpt body text;
2. **Hero & OpenGraph Images**: Locked posts never expose cover or inline image URLs, falling back to deterministic local geometric SVG covers;
3. **Previous / Next Post Links**: Expose minimal title/permalink metadata only — never body previews;
4. **Comment API & Recent Comments**: `/api/v1/comments` returns `403 Forbidden` until unlocked, and sidebar recent comments exclude locked posts;
5. **Two-Stage Full-Text Search**: Both the `8 MiB` in-memory index and SQL fallback strip locked posts and `:::lock` blocks prior to indexing/matching;
6. **RSS / Atom / Podcast Feeds**: Password-protected posts are excluded from syndication XML; partial `:::lock` blocks render localized lock notices;
7. **Sitemap & IndexNow**: Password-protected posts are omitted from `sitemap.xml`, excluded from IndexNow pings, and served with `noindex, nofollow, noarchive` + `Cache-Control: private, no-store`;
8. **Bidirectional Backlinks (`[[slug]]`)**: Locked posts neither emit outbound backlinks nor appear in target post backlink lists;
9. **`/llms.txt` & `.md` Raw Endpoints**: Excluded from LLM corpus indexes, and unauthenticated `.md` requests return `403`;
10. **Plugin Hook Redaction**: Plugins without explicit `articles:read_protected` permission never receive protected content, and `render.post_html` output is re-sanitized through `bluemonday`.

---

## 5. Zero-Stale Revision-Tracked Caches & Memory Budget

To achieve microsecond response times within a strict `< 35 MiB` `VmRSS` ceiling without external Redis dependencies, `Volo-go` pairs **`Store.DataRevision()`** with **`SettingsManager.Revision()`**:

- **Instant Invalidation on Every Mutation**: Every foreground `ExecContext` / `WithTx` atomically increments `dataRev`. The O(1) redirect snapshot, public chrome snapshot (navigation tree, categories, tags, recent comments), and pre-compressed RSS/Atom/Podcast/Sitemap/LLMs documents (`CachedDocument`, capped at `2 MiB`) validate `(dataRev, settingsRev)` on read — guaranteeing **zero stale reads** while serving strong `ETag` / `304 Not Modified` responses in microseconds.
- **Write-Behind View Analytics Buffer (`viewAnalyticsBuffer`)**: High-frequency `articles.view_count`, `pages.view_count`, `redirects.hit_count`, and `analytics_daily` increments are coalesced lock-free in memory, overlaid in real time on reads (`PendingArticleViews`), flushed every `2s` via `WithBgTx` (without invalidating content caches), and deterministically drained on `/api/admin/overview` and `SIGTERM` shutdown.
- **Bounded Resident Memory Allocation (Measured Warm `VmRSS = 28.60 MiB`)**:
  - Go runtime, HTTP stack, embedded assets & pooled `goja` VMs: `~12 MiB`
  - SQLite WAL page cache (`cache_size = -4096`): `<= 4 MiB`
  - Two-stage full-text search index (`SearchIndex`): `<= 8 MiB`
  - SHA-256 verified Markdown render LRU (`boundedRenderLRU`): `<= 4 MiB`
  - Pre-compressed syndication feed cache (`<= 2 MiB`) + pooled Gzip/image buffers.
