# Legacy Migration & Cross-DB Streaming Backup

> 中文版本：[历史迁移与跨库流式备份指南](../zh-CN/migration-and-backup.md)

`Volo-go` provides two constant-memory streaming pipelines:
1. **Legacy Archive Streaming ETL (`internal/etl`)**: Imports legacy `bolo-solo` / `Solo` 15-table `.zip` or `JSON` archives (or connects read-only to a legacy database), normalizes denormalized structures into relational tables, and **guarantees zero `404` dead links for historical `.html` permalinks**;
2. **Native Cross-Database Streaming Backup (`internal/backup`)**: Uses the dialect-agnostic `volo-backup-v1` archive format (**33 normalized data table** `NDJSON` streams in foreign-key order + `SHA-256` manifest + media stream) for lossless **SQLite ⇄ PostgreSQL ⇄ MySQL** portability.

---

## 1. Lossless Migration from Legacy `bolo-solo`

### 1.1 Six Automated Normalization Rules

Legacy backups carry historical schema debt (sharded 10KB key-value JSON strings, indirect tag-based categorization, legacy password hashes, and `.html` permalinks). `Volo-go`'s streaming `json.Decoder` pipeline transforms them inside an atomic transaction without buffering the entire archive in memory:

| Legacy Structure | `Volo-go` Normalized Target (`v6` Schema) | Transformation Behavior |
| :--- | :--- | :--- |
| Sharded key-value JSON documents (Series, Bookshelf, Daily Analytics, Webhooks) | `series`, `series_articles`, `books`, `book_chapters`, `analytics_daily`, `webhooks` | Reassembles multi-shard JSON payloads into first-class relational tables with foreign keys |
| Indirect `category` + `category_tag` + `tag_article` categorization | `articles.category_id` + `article_tags` | Derives each post's primary category directly from its tag bindings while preserving all freeform tags |
| Flat untyped `option` rows | All 9 typed `settings` domains (`site`, `reading`, `feed`, `seo`, `storage`, `appearance`, `notifications`, `security`, `backup`) | Maps site title, subtitle, social links, footer, custom global variables (`customVars` -> `site.custom_vars`), and timeline style (`articleListStyle` -> `reading.list_style`); retired legacy skins cleanly fall back to `volo-studio` |
| Custom navigation links | `nav_items` & `pages` | Preserves navigation order, icons, and target attributes |
| Legacy post URLs (e.g. `/articles/2021/05/12/1620812.html`) | `articles.legacy_permalink` + `redirects` | Generates clean semantic slugs while storing the original path in `legacy_permalink`; requests to old `.html` URLs resolve in `O(1)` memory with **`301 Moved Permanently`** redirects |
| Legacy `PBKDF2` password hashes | `users.legacy_hash` -> `Argon2id` | Imported users sign in without password resets; first successful login transparently upgrades the hash to `Argon2id` and clears `legacy_hash` |

### 1.2 Running the Migration (CLI or Web UI)

#### Option A: Via CLI (Recommended for Large Archives & Domain Switches)

```bash
# Step 1: Dry-run validation — inspect counts of posts, categories, comments, series, and books without writing
./volo import-legacy /path/to/legacy-backup.zip --dry-run

# Step 2: Execute streaming ETL import (optionally rewriting old domain URLs and forcing local storage)
./volo import-legacy /path/to/legacy-backup.zip \
  --old-site-url "https://old.example.com,http://old.example.com" \
  --new-site-url "https://blog.example.com" \
  --storage-local
```

#### Option B: Direct Old-DB Connection in Setup Wizard (Zero Manual Export)

- **Fresh Install**: Open `/setup` in your browser and select **1-Click Legacy Migration -> Direct Old DB Migration**:
  1. Configure your new destination database (`SQLite` / `PostgreSQL` / `MySQL`) in `1A` and enter your legacy source database DSN (`Solo` / `Bolo` / `Volo-solo` or an existing `Volo-go` database) in `1B`;
  2. Click **Inspect Source DB** — Volo-go connects strictly read-only, auto-detects the schema and table prefix (such as `b3_solo_`, `solo_`, or native Volo-go tables), and reports post, comment, and user counts;
  3. Optionally run a **Dry-Run** validation first, then stream-migrate directly into the new database with full `301` historical permalink preservation and zero writes to the old database.

#### Option C: Upload Archive via Setup Wizard or Admin Console

- **Fresh Install**: Open `/setup`, choose **Upload Archive (.zip / .json)**, and select your legacy `.zip` or `.json` archive;
- **Existing Instance**: Upload the archive under **Console -> System & Security -> Backup & Migration -> Legacy Import** to inspect the live transformation summary.

---

## 2. Native Cross-Database Streaming Backup (`volo-backup-v1`)

### 2.1 Archive Layout & Integrity Guarantees

Every native `.zip` archive streams all **33 normalized data tables** in `migrate.CanonicalTables` dependency order with cryptographic verification metadata:

```text
volo-backup-20261108-010000.zip
├── manifest.json                 # Records format, schema_version (v6), source_dialect, byte sizes, row counts & SHA-256 digests
├── tables/
│   ├── 01_settings.ndjson        # Dialect-neutral NDJSON streams for all 33 canonical tables in FK order
│   ├── 02_users.ndjson
│   ├── ...
│   └── 33_plugin_kv.ndjson
└── media/                        # Optional (--with-media=true) uploaded images, audio/video clips & podcast episodes
    └── 2026/11/...
```

Built-in restore protections:
- **Pre-Write Digest Verification**: Every stream's byte length and `SHA-256` hash is verified before mutating database state; corrupted or tampered archives abort cleanly;
- **Zip-Bomb & Path Traversal Guards**: Enforces `512 MiB` per-entry and `2 GiB` total unpacked caps while sanitizing relative media paths against `../` traversal;
- **Batched Transactional Rollback**: Streams `200`-row batches inside a topological transaction so any failure rolls back atomically.

### 2.2 Cross-Database Migration Walkthrough (e.g., SQLite -> PostgreSQL)

Moving an active blog from single-node SQLite to managed PostgreSQL (Supabase / Neon / self-hosted PG) or MySQL 8+ / TiDB takes three commands:

```bash
# 1. Export a full streaming backup from the SQLite instance
./volo backup create --env-file /etc/volo/volo.env --output /tmp/site-full.zip --with-media=true

# 2. Point the target environment file at PostgreSQL:
#    VOLO_DB_DRIVER=postgres
#    VOLO_DB_DSN=postgres://volo:secret@db.internal:5432/volo?sslmode=verify-full
#    IMPORTANT: Keep the same VOLO_SECRET_KEY (or data/.secret.key) so AES-256-GCM encrypted S3/SMTP/WebDAV/Webhook/plugin secrets decrypt seamlessly!

# 3. Pre-verify with --dry-run, then stream-restore into PostgreSQL
./volo backup restore /tmp/site-full.zip --env-file /etc/volo/volo-pg.env --dry-run
./volo backup restore /tmp/site-full.zip --env-file /etc/volo/volo-pg.env
```

---

## 3. Automated Rolling Backups & Offsite Replication (S3 / R2 / WebDAV)

In **Console -> System & Security -> Backup & Migration** (backed by the `backup` settings domain):
1. Enable scheduled periodic local backups with configurable interval, media inclusion (`include_media_in_auto`), and local retention count so older archives are rotated automatically;
2. When **S3-compatible object storage (Cloudflare R2 / AWS S3 / MinIO)** or **WebDAV** is configured (with credentials sealed via `AES-256-GCM` in `settings.secret_ciphertext`), newly created `.zip` snapshots replicate offsite automatically for `3-2-1` disaster recovery.
