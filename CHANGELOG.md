# Changelog / 版本更新日志

All notable changes to `Volo-go` are documented in this file. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and [Semantic Versioning](https://semver.org/).

---

## [1.0.0] - 2026-11-06

### Added / 新增特性
- **Pure-Go Single-Binary Engine (`CGO_ENABLED=0`)**:
  - Cross-platform static single binary (`18.52 MiB` on `linux/amd64`, `< 20 MiB` across all 5 OS/Arch targets) with `< 35 MiB` live `VmRSS` (`28.60 MiB` measured), lock-free CAS monotonic ID generator, TCP & Unix Domain Socket listeners, graceful `SIGTERM` shutdown, `/healthz`, and native Distroless `volo healthcheck`.
  - 4-layer `.env` configuration loader (`CLI Flags > VOLO_* Env > .env > Defaults`), `HKDF-SHA256` key derivation, and `AES-256-GCM` database secret encryption at rest.
  - Unified pure-Go `Dialect` layer supporting **SQLite WAL** (`modernc.org/sqlite` with physically separated writer & `query_only=ON` reader pools and bounded `4 MiB` page cache), **PostgreSQL** (`pgx/v5`), and **MySQL 8+ / MariaDB / TiDB**, plus idempotent schema migrations (v1–v2).
- **Normalized Domain Model & Streaming Migration / Backup**:
  - 23 normalized tables replacing legacy key-value document sharding: direct `articles.category_id`, decoupled standalone Markdown `pages` and hierarchical `nav_items`, `series`, `books`, `analytics_daily`, `webhooks`, `redirects`, and `plugin_kv`.
  - Two-phase streaming legacy 15-table ZIP/JSON ETL importer (`volo import-legacy` & Web Setup/System uploader) with automatic category derivation and `301` `.html` permalink preservation.
  - Database-agnostic `NDJSON` + media streaming ZIP backup/restore (`SQLite ⇄ PostgreSQL ⇄ MySQL`) and scheduled local + S3/WebDAV rolling backups.
- **Sovereign Identity, Persistent Guest Passport & Anti-Abuse**:
  - `admin` / `editor` (author-isolated comment moderation) / `reader` RBAC, `Argon2id` password hashing (with transparent `PBKDF2` upgrade on login), WebAuthn Passkeys, TOTP + recovery codes, session management, and scoped `volo_pat_*` API tokens.
  - 3-tier self-healing HMAC `GuestPassport` + peppered `env_fp` + stateless `SHA-256` PoW + one-click guest comment claiming upon registration, plus `/32` IPv4 & `/64` IPv6 WAF rate limiting, scanner traps, and offline MaxMind/ip2region GeoIP control.
- **10-Surface Zero-Leak Privacy Perimeter & Media Pipeline**:
  - `ContentVisibilityGuard` enforcing zero leakage of password-protected posts and `:::lock` blocks across all 10 surfaces, plus stateless timed `ShareToken` preview links.
  - Privacy-first media pipeline (`<= 64 MP` decompression-bomb cap, EXIF orientation normalization, 100% GPS/EXIF metadata stripping, adaptive pure-Go WebP encoding, intrinsic width/height zero-CLS injection, pure-Go AWS SigV4 S3/R2/MinIO upload, and 9-scope orphan media pruning).
- **Studio Admin Console, Themes & Sandboxed Plugin Ecosystem**:
  - Embedded Preact + Signals 5-workspace Studio Console with lazy-loaded CodeMirror 6 split Markdown editor (`35.34 KB` initial Gzip), 6 atomic `PATCH` hot-reload settings domains, revisions, Podcast & Bookshelf managers, and Notification Center.
  - Built-in `volo-studio` theme, external `./themes/volo-journal` theme, pure-Go `goja` ES6 sandboxed plugin pool + signed external HTTP webhook runtime, ZIP hot-install, and `./plugins/kanbanniang` companion plugin.
- **High-Throughput Runtime Optimizations**:
  - Write-behind `viewAnalyticsBuffer` coalescing `view_count`, redirect `hit_count`, and `analytics_daily` increments into batched background transactions with real-time pending read merge and deterministic shutdown flush.
  - Revision-coherent O(1) redirect lookup table, sidebar/navigation chrome snapshot cache, `4 MiB` digest-verified Markdown render LRU (`758×` faster warm card rendering), pre-compressed RSS/Atom/Podcast/Sitemap/LLMs documents with strong `ETag` (`304 Not Modified`), BREACH-safe pooled dynamic HTTP Gzip, zero-closure `sqlx` row scanner, and cached PostgreSQL placeholder rebinding (`0 allocs/op`).
- **Multi-OS Service Installers, Reverse Proxy Templates & Open-Source Governance**:
  - System service installers and templates for Linux (`systemd` & `OpenRC`), macOS (`launchd`), and Windows (`PowerShell` NSSM / Scheduled Task), plus Nginx (TCP & Unix Socket), Caddy v2, Traefik v3, and Apache httpd reverse proxy configs.
  - GitHub Actions workflows (`ci.yml`, `release.yml`, `security.yml`, `benchmark.yml`), Dependabot, bilingual Issue/PR templates, full Chinese & English documentation suite (`docs/zh-CN/` & `docs/en-US/`), and **Apache License 2.0** (`LICENSE` & `THIRD_PARTY_NOTICES.md`).
