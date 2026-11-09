# Volo-go

[![CI](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml)
[![Security](https://github.com/Willh92/Volo-go/actions/workflows/security.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/security.yml)
[![Release](https://github.com/Willh92/Volo-go/actions/workflows/release.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/release.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-246b51.svg)](./LICENSE)

**Pure-Go Sovereign Independent Blog Engine · 纯 Go 现代独立主权博客引擎**

`CGO_ENABLED=0` 跨 6 大平台静态单二进制 · 体积 **`< 20 MiB`**（`linux/amd64`，全 6 平台 `< 21 MiB`） · 温热常驻内存 **`25.17 MiB` (< 35 MiB)** · 原生三方言（SQLite WAL / PostgreSQL / MySQL）· 10 大出口零死角防泄密 · 三模式（`IR` / `WYSIWYG` / `SV`）Studio 编辑器与 5 大工作区管理台

[简体中文](#简体中文) | [English](#english)

---

## 简体中文

### 文档导航

| 主题文档 | 说明 |
| :--- | :--- |
| [架构设计与性能内幕 (`docs/zh-CN/architecture.md`)](./docs/zh-CN/architecture.md) | `v6` Schema（33 张正规化数据表 + 9 大热重载设置域）、三方言 SQL 与读写分离池、10 大出口防泄密守卫、多级一致性缓存与内存预算 |
| [部署与系统服务安装指南 (`docs/zh-CN/deployment.md`)](./docs/zh-CN/deployment.md) | 多层配置优先级、6 大静态发布平台、Linux (`systemd` / `OpenRC`)、macOS (`launchd`)、Windows 原生服务 (`sc.exe` / `NSSM`)、升级自动回滚与 Docker 部署 |
| [反向代理与 CDN 配置指南 (`docs/zh-CN/reverse-proxy.md`)](./docs/zh-CN/reverse-proxy.md) | TCP vs Unix Socket (`--socket-mode` / `--socket-group`)、`VOLO_TRUSTED_PROXIES` 防伪造模型、Nginx / Caddy / Traefik / Apache / Cloudflare 完整配置 |
| [CLI 瑞士军刀与开放 API (`docs/zh-CN/cli-and-api.md`)](./docs/zh-CN/cli-and-api.md) | `volo` 全子命令、细粒度 `volo_pat_*` API Token、Obsidian / PicGo 直连、分页搜索、Sitemap 自动分片与管理端/公开端 REST 接口 |
| [主题定制与插件开发 (`docs/zh-CN/themes-and-plugins.md`)](./docs/zh-CN/themes-and-plugins.md) | `theme.json`（`page_templates`、`customVar`、`ListStyle`、无框 `error.html`）、纯 Go ES6 插件沙箱（`registerDirective`、`type: "secret"`、`federation` 分发）与 ZIP 热插拔/导出 |
| [旧版主题皮肤迁移指南 (`docs/zh-CN/theme-migration.md`)](./docs/zh-CN/theme-migration.md) | 独立整页 HTML / 布局继承双模式、`PageContext` 全量数据模型、`config_schema` 自定义配置与模板函数速查 |
| [多语言包扩展指南 (`docs/zh-CN/locale-extension.md`)](./docs/zh-CN/locale-extension.md) | 单文件 JSON / `.zip` 语言包热安装、`admin.nav.*` 无冲突命名空间、内置词条覆盖与完整模板导出 |
| [旧版无损迁移与跨库备份 (`docs/zh-CN/migration-and-backup.md`)](./docs/zh-CN/migration-and-backup.md) | 历史 15 表备份流式 ETL 清洗、`301` 永久链接守护、`SQLite ⇄ PostgreSQL ⇄ MySQL` 33 表跨库流式搬家 |

---

### 核心能力与性能实测

| 指标 / 基准测试项 (`CGO_ENABLED=0`) | 实测表现 | 硬性预算 / 说明 |
| :--- | :--- | :--- |
| 静态单二进制体积 (`linux/amd64`) | **`19.99 MiB`** (`arm64` `18.70 MiB`) | `linux/amd64` `< 20.00 MiB`，跨 6 大平台目标均严格 `< 21.00 MiB` |
| 温热运行常驻内存 (`VmRSS`) | **`25.17 MiB`** | 含内嵌主题、插件沙箱与热缓存 `< 35.00 MiB` |
| 管理台首屏核心体积 (Gzip) | **`84.82 KB`** (总内嵌 `< 300 KB`) | 工作区与 CodeMirror 6 三模式编辑器核心按需懒加载，首屏提速 `66%+` |
| 文章卡片摘要热缓存 (`ToCard`) | **`1.19 µs/op`** (`864 B/op`) | 4 MiB 摘要自校验 LRU，较冷渲染提速 `758×` |
| 文章正文详情热缓存 (`ToDetail`) | **`4.64 µs/op`** (`3.3 KB/op`) | 严格排除密码与限时预览令牌，零串号风险 |
| RSS / Sitemap 预压缩 + `ETag` 协商 | **`7.69 µs/op`** (`~130k req/s`) | 数据版本号 `1ns` 自动失效，超 `10,000` URL 自动 `<sitemapindex>` 分片 |
| 并发文章阅读 + 异步写合并 (`RecordView`) | **`454.9 µs/op`** (双核全链路 SSR+Gzip) | 内存增量实时叠加 + 2s/停机批量落盘，零 SQLite 写锁争用 |
| 二阶段全文检索 (`SearchIndex`) | **`29.4 µs/op`** | 8 MiB 常驻索引 + UTF-8 零切片窗口摘要提取 + 分页 `/api/v1/search` |

1. **极致纯净的 6 平台单文件交付 (`CGO_ENABLED=0`)**：基于 Go `1.27.3` 零 C 依赖静态编译，覆盖 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`；内置前台主题、管理台 SPA、多语言字典与运维 CLI；`http.ServeContent` 零拷贝音视频 `Range` (`206 Partial Content`) 传输、BREACH 免疫动态 Gzip、优雅停机（`VOLO_SHUTDOWN_TIMEOUT_SEC`）确定性刷盘与 Distroless 原生 `volo healthcheck`。
2. **多层配置底座与三方言存储引擎**：基础参数按 `CLI Flags > VOLO_* 环境变量 > .env > 安全默认值` 解析（`VOLO_UPLOAD_DIR` 与 `VOLO_GEOIP_DIR` 支持 `CLI > OS ENV > .env > 管理台数据库设置 > 默认值` 五级覆盖）；不显式固定 `VOLO_DB_DRIVER` 时，`/setup` 向导经 `AES-256-GCM` 加密保存的 `data/db.json`（SQLite / PostgreSQL / MySQL）可在容器与服务重启后持久生效；原生支持 **SQLite WAL**（读写连接池物理隔离）、**PostgreSQL**（`pgx/v5`，含 `VOLO_DB_SIMPLE_PROTOCOL`）与 **MySQL 8+ / MariaDB / TiDB**。
3. **正规化领域模型（`v6` Schema，33 张数据表 + 9 大设置域）与三模式创作编辑器**：涵盖 `site`、`reading`、`feed`、`seo`、`storage`、`appearance`、`notifications`、`security`、`backup` 9 大原子热重载设置域；内置 **IR（即时渲染，含长图滚动稳像与 `~~~` 围栏）、WYSIWYG（所见即所得富文本往返，含 `<audio>`/`<video>` 光标落点）、SV（分屏同步预览）** 三模式编辑器，原生支持 `<video>` 视频、LaTeX 公式 AST 保护与浏览器语音录制。
4. **动态隐身后台、多库共存离线 GeoIP 与主权账户防刷**：支持管理台免重启热切换隐身路径（`security.admin_path` / `dynamic_login_path`），以及 `GeoLite2-City.mmdb` + `GeoLite2-Country.mmdb` + `GeoLite2-ASN.mmdb` + `ip2region` XDB 多库共存与 `security.geoip_dir` 热重载（含 `geo_default_action` / `geo_unknown_action`）；`admin` / `editor` / `reader` 三级 RBAC、`Argon2id`、WebAuthn Passkey、TOTP、细粒度 `volo_pat_*` Token 及三端自愈 `GuestPassport` + 动态 PoW。
5. **10 大出口零死角防泄密、隐私媒体管线与主题/插件生态**：`ContentVisibilityGuard` 守护 10 大出口 **100% 零泄露**；上传图片强制 `<= 64 MP`、自动摆正并 **100% 剥离 EXIF/GPS**、自适应转 WebP、直传 S3/R2/MinIO；插件支持纯 Go ES6 沙箱、`volo.registerDirective(name, fn)` 自定义指令、`type: "secret"` 加密字段、`.zip` 导出与 `overwrite=true` 原地升级及文章发布自动 `federation` 分发；主题支持 `{{customVar}}`、`page_templates`、`Reading.ListStyle`、无框独立 `error.html` 与纯净骨架 `.zip` 导出。

---

### 快速开始

#### 1. 源码构建与零配置启动（Go `1.27.3` + Node.js `24 LTS`）

```bash
# 1. 构建内嵌管理台前端产物
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. 纯 Go 静态编译（< 20 MiB 单二进制）
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath \
  -gcflags="all=-l" -ldflags="-s -w" ./cmd/volo

# 3. 零配置直接启动（自动创建 ./data/volo.db 与 0600 主密钥文件）
./volo serve
```

访问 `http://localhost:8080` 进入首次建站向导 `/setup`（支持选库测活、初始化管理员或一键上传导入旧版备份包）。

#### 2. 跨操作系统服务安装与带自动回滚的在线更新

内置跨平台服务管理脚本 [`scripts/install-service.sh`](./scripts/install-service.sh)（Linux `systemd` 系统/用户级、`OpenRC` `/run/volo/volo.pid` 及 macOS `launchd`）与 [`scripts/install-service.ps1`](./scripts/install-service.ps1)（Windows 原生服务 `sc.exe` / `svc_windows.go`、`NSSM` 及计划任务回退）。执行 `upgrade` 时会自动创建升级前数据库备份（可通过 `--skip-backup` / `-SkipBackup` 跳过）、保留 `volo.bak`，并在启动后调用 `volo healthcheck` 探活——若探活失败自动回滚旧版二进制与服务：

```bash
# Linux / macOS 远程一行安装（默认监听 127.0.0.1:8080）
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# 日常极简在线更新（自动备份 + SHA-256 校验 + healthcheck 失败自动回滚；支持 --mirror 与 --skip-backup）
sudo ./scripts/install-service.sh upgrade

# Linux + Nginx 零 TCP 开销 Unix Domain Socket 高级模式
sudo ./scripts/install-service.sh install \
  --unix-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --socket-group www-data \
  --public-url https://blog.example.com

# macOS 当前用户免 sudo 安装（LaunchAgent）与在线更新
./scripts/install-service.sh install --public-url https://blog.example.com
./scripts/install-service.sh upgrade

# Windows (PowerShell 管理员运行：原生支持 sc.exe Windows Service / NSSM、-Mirror 加速与 -Action Logs)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Install -PublicUrl "https://blog.example.com"
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
```

配套 **Nginx（TCP 与 Unix Socket）、Caddy v2、Traefik v3、Apache httpd** 反代配置位于 [`deploy/reverse-proxy/`](./deploy/reverse-proxy/)，详尽说明请参阅 [部署指南](./docs/zh-CN/deployment.md) 与 [反向代理指南](./docs/zh-CN/reverse-proxy.md)。

#### 3. Docker Compose 部署

默认挂载命名卷 `volo_data:/app/data`，并将 `VOLO_DB_DRIVER` 留空，使 `/setup` 向导保存的 `data/db.json` 跨容器重建持久生效（若改用宿主机目录绑定挂载 `./data:/app/data`，请先执行 `mkdir -p data && chown 65532:65532 data` 授权 Distroless `nonroot` 用户）：

```bash
cp .env.example .env
docker compose up -d --build
```

#### 4. 内置 CLI 运维命令与开发校验

```bash
volo serve                                                   # 启动 HTTP 或 Unix Domain Socket 服务
volo admin reset-password --user <u|email> [--password <pw>] # 重置 Argon2id 密码（别名 volo passwd）并吊销全部会话与 Token
volo admin disable-2fa --user <u|email>                      # 紧急关闭指定账户 TOTP 与恢复码
volo backup create [--output backup.zip] [--with-media=true] # 创建跨库流式 NDJSON + 媒体 ZIP 备份
volo backup restore <backup.zip> [--dry-run]                 # 校验 SHA-256 并跨库无损恢复备份
volo import-legacy <legacy.zip|json> [--dry-run] \
  [--old-site-url URL] [--new-site-url URL] [--storage-local] # 流式清洗导入历史 15 表备份包
volo doctor [--json]                                         # 自检密钥权限、SQLite WAL、GeoIP 目录与反代配置
volo healthcheck [--url http://127.0.0.1:8080/healthz]       # 原生探活（自动兼容 TCP 与 Unix Socket）
volo version                                                 # 打印版本与运行平台信息
```

```bash
# 本地构建、单测、竞态检测、基准测试与体积预算校验
npm --prefix web/admin ci && npm --prefix web/admin run build
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build ./... && CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test -run=^$ -bench=. -benchmem ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
bash scripts/verify-binary-budget.sh
```

---

## English

### Documentation Index

| Guide | Topics Covered |
| :--- | :--- |
| [Architecture & Performance Internals (`docs/en-US/architecture.md`)](./docs/en-US/architecture.md) | `v6` schema (33 normalized data tables + 9 hot-reload settings domains), multi-dialect SQL & split WAL pools, 10-surface privacy perimeter, revision-coherent caches, memory budget |
| [Deployment & OS Service Installers (`docs/en-US/deployment.md`)](./docs/en-US/deployment.md) | Multi-layer config precedence, 6 static release targets, Linux (`systemd` / `OpenRC`), macOS (`launchd`), Windows native service (`sc.exe` / `NSSM`), auto-rollback upgrades, Docker / GHCR |
| [Reverse Proxy & CDN Guide (`docs/en-US/reverse-proxy.md`)](./docs/en-US/reverse-proxy.md) | TCP vs Unix Domain Socket (`--socket-mode` / `--socket-group`), `VOLO_TRUSTED_PROXIES` anti-spoofing CIDRs, Nginx / Caddy / Traefik / Apache / Cloudflare recipes |
| [CLI & Open API Reference (`docs/en-US/cli-and-api.md`)](./docs/en-US/cli-and-api.md) | Built-in `volo` CLI subcommands, scoped `volo_pat_*` tokens, Obsidian / PicGo automation, paginated search, Sitemap sharding, and Admin/Public REST endpoints |
| [Themes & Plugin SDK (`docs/en-US/themes-and-plugins.md`)](./docs/en-US/themes-and-plugins.md) | `theme.json` (`page_templates`, `customVar`, `ListStyle`, frameless `error.html`), pure-Go ES6 plugin sandbox (`registerDirective`, `type: "secret"`, `federation` dispatch), ZIP install/export |
| [Theme Skin Migration Guide (`docs/en-US/theme-migration.md`)](./docs/en-US/theme-migration.md) | Standalone full-HTML & layout inheritance modes, full `PageContext`, `config_schema` fields, and template FuncMap reference |
| [Language Pack Extension Guide (`docs/en-US/locale-extension.md`)](./docs/en-US/locale-extension.md) | Hot-installable JSON / `.zip` language packs, `admin.nav.*` namespace, built-in overrides, and full starter template export |
| [Legacy Migration & Cross-DB Backup (`docs/en-US/migration-and-backup.md`)](./docs/en-US/migration-and-backup.md) | Streaming legacy 15-table ETL, `301` `.html` permalink preservation, `SQLite ⇄ PostgreSQL ⇄ MySQL` 33-table streaming backups |

---

### Core Capabilities & Measured Benchmarks

| Metric / Benchmark (`CGO_ENABLED=0`) | Measured Result | Hard Budget / Notes |
| :--- | :--- | :--- |
| Static Single Binary Size (`linux/amd64`) | **`19.99 MiB`** (`arm64` `18.70 MiB`) | `< 20.00 MiB` on `linux/amd64`; strictly `< 21.00 MiB` across all 6 targets |
| Warm Resident Memory (`VmRSS`) | **`25.17 MiB`** | `< 35.00 MiB` including embedded themes, plugin sandbox, and warm caches |
| Admin Console Initial Core (Gzip) | **`84.82 KB`** (`< 300 KB` total) | Workspaces & CodeMirror 6 3-mode editor core are lazy-loaded (`66%+` smaller initial payload) |
| Warm Article Card Cache (`ToCard`) | **`1.19 µs/op`** (`864 B/op`) | 4 MiB digest-verified LRU (`758×` faster than cold Markdown render) |
| Warm Article Detail Cache (`ToDetail`) | **`4.64 µs/op`** (`3.3 KB/op`) | Strictly excludes password-locked & `?preview=` requests (zero cross-user leakage) |
| Pre-Compressed RSS / Sitemap + `ETag` | **`7.69 µs/op`** (`~130k req/s`) | `1ns` revision invalidation; auto-shards into `<sitemapindex>` above `10,000` URLs |
| Concurrent Article Read + Write-Behind (`RecordView`) | **`454.9 µs/op`** (2-core SSR+Gzip) | Real-time pending counter merge + 2s/shutdown batched flush (zero SQLite write lock contention) |
| Two-Stage Full-Text Search (`SearchIndex`) | **`29.4 µs/op`** | 8 MiB in-memory index + UTF-8 Rune-safe snippet windowing + paginated `/api/v1/search` |

1. **Pure-Go Single-File Delivery Across 6 Targets (`CGO_ENABLED=0`)**: Built with Go `1.27.3` for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`. Bundles themes, Studio Admin SPA, locale catalogs, and Ops CLI with zero-copy `http.ServeContent` `Range` (`206 Partial Content`) media streaming, BREACH-safe pooled Gzip, deterministic `SIGTERM` drain (`VOLO_SHUTDOWN_TIMEOUT_SEC`), and Distroless `volo healthcheck`.
2. **Multi-Layer Config & Pure-Go Multi-Dialect DB**: Resolves `CLI Flags > OS VOLO_* Env > .env > Defaults` (with 5-layer `CLI > OS ENV > .env > Admin DB Settings > Default` precedence for `VOLO_UPLOAD_DIR` and `VOLO_GEOIP_DIR`). Leaving `VOLO_DB_DRIVER` unset lets the `/setup` wizard's `AES-256-GCM` encrypted `data/db.json` (`SQLite` / `PostgreSQL` / `MySQL`) persist across container/service restarts. Supports **SQLite WAL** (split writer/read-only pools), **PostgreSQL** (`pgx/v5` + `VOLO_DB_SIMPLE_PROTOCOL`), and **MySQL 8+ / MariaDB / TiDB**.
3. **Normalized Domain Model (`v6` Schema, 33 Data Tables + 9 Settings Domains) & 3-Mode Studio Editor**: Atomic hot-reload across `site`, `reading`, `feed`, `seo`, `storage`, `appearance`, `notifications`, `security`, and `backup`; features the **3-Mode Studio Editor** — **IR** (Instant Rendering with tall-image scroll stabilization & `~~~` fences), **WYSIWYG** (rich round-trip with `<audio>`/`<video>` cursor landing pads), and **SV** (synchronized split preview) — plus native `<video>`, LaTeX math protection, and browser voice recording.
4. **Dynamic Stealth Admin Path, Multi-DB Offline GeoIP & Sovereign Identity**: Zero-downtime hot-reloadable stealth admin path (`security.admin_path` / `dynamic_login_path`) and side-by-side `GeoLite2-City.mmdb` + `GeoLite2-Country.mmdb` + `GeoLite2-ASN.mmdb` + `ip2region` XDB GeoIP engine with hot-reloadable `security.geoip_dir` (`geo_default_action` & `geo_unknown_action`); `admin` / `editor` / `reader` RBAC, `Argon2id`, WebAuthn Passkeys, TOTP, scoped `volo_pat_*` tokens, and 3-tier self-healing `GuestPassport` + stateless PoW.
5. **10-Surface Zero-Leak Privacy, Media Pipeline & Extensible Theme/Plugin SDK**: `ContentVisibilityGuard` guarantees zero leakage across all 10 surfaces; uploads enforce `<= 64 MP` caps, upright orientation, **100% EXIF/GPS stripping**, pure-Go WebP, and SigV4 S3/R2/MinIO storage. Plugins support pure-Go ES6 sandboxing, `volo.registerDirective(name, fn)`, `type: "secret"` encrypted fields, `.zip` export, `overwrite=true` in-place ZIP upgrade, and automatic `federation` dispatch; themes support `{{customVar}}`, `page_templates`, `Reading.ListStyle`, standalone frameless `error.html`, and clean starter `.zip` export.

---

### Quick Start

#### 1. Build from Source & Zero-Config Launch (Go `1.27.3` + Node.js `24 LTS`)

```bash
# 1. Build embedded Admin Console frontend assets
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. Compile pure-Go static binary (< 20 MiB)
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath \
  -gcflags="all=-l" -ldflags="-s -w" ./cmd/volo

# 3. Start with zero config (auto-creates ./data/volo.db and 0600 master key)
./volo serve
```

Open `http://localhost:8080` to launch the `/setup` wizard (configure/test your database, create the admin account, or stream-import a legacy backup archive).

#### 2. One-Command OS Service Installers & Auto-Rollback Upgrades

[`scripts/install-service.sh`](./scripts/install-service.sh) (Linux `systemd` system/user, `OpenRC` `/run/volo/volo.pid`, macOS `launchd`) and [`scripts/install-service.ps1`](./scripts/install-service.ps1) (native Windows Service `sc.exe` via `svc_windows.go`, `NSSM`, and Scheduled Task fallback) automatically create a pre-upgrade backup (skippable via `--skip-backup` / `-SkipBackup`), preserve `volo.bak`, verify `SHA256SUMS.txt`, and run `volo healthcheck` after restart — rolling back automatically if health verification fails:

```bash
# Linux / macOS one-line install (listens on 127.0.0.1:8080 by default)
curl -fsSL https://raw.githubusercontent.com/Willh92/Volo-go/main/scripts/install-service.sh | sudo sh

# Zero-flag online upgrade (pre-upgrade backup + SHA-256 check + healthcheck auto-rollback; supports --mirror & --skip-backup)
sudo ./scripts/install-service.sh upgrade

# Linux + Nginx zero-TCP Unix Domain Socket mode
sudo ./scripts/install-service.sh install \
  --unix-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --socket-group www-data \
  --public-url https://blog.example.com

# macOS rootless current-user LaunchAgent install & upgrade
./scripts/install-service.sh install --public-url https://blog.example.com
./scripts/install-service.sh upgrade

# Windows (Run in elevated PowerShell: native sc.exe Windows Service / NSSM, -Mirror fallback, and -Action Logs)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Install -PublicUrl "https://blog.example.com"
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Upgrade
powershell -ExecutionPolicy Bypass -File .\scripts\install-service.ps1 -Action Logs
```

Production reverse-proxy templates for **Nginx (TCP & Unix Socket), Caddy v2, Traefik v3, and Apache httpd** are in [`deploy/reverse-proxy/`](./deploy/reverse-proxy/) — see the [Deployment Guide](./docs/en-US/deployment.md) and [Reverse Proxy Guide](./docs/en-US/reverse-proxy.md).

#### 3. Docker Compose Deployment

Mounts the named volume `volo_data:/app/data` by default and leaves `VOLO_DB_DRIVER` unset so the `/setup` wizard's `data/db.json` persists across container recreations (if switching to a host bind mount `./data:/app/data`, run `mkdir -p data && chown 65532:65532 data` for the Distroless `nonroot` UID/GID):

```bash
cp .env.example .env
docker compose up -d --build
```

#### 4. Built-in Ops CLI & Verification Suite

```bash
volo serve                                                   # Start HTTP or Unix Domain Socket server
volo admin reset-password --user <u|email> [--password <pw>] # Reset Argon2id password (alias: volo passwd) & revoke sessions + tokens
volo admin disable-2fa --user <u|email>                      # Emergency-disable TOTP & recovery codes for a user
volo backup create [--output backup.zip] [--with-media=true] # Create cross-DB streaming NDJSON + media ZIP backup
volo backup restore <backup.zip> [--dry-run]                 # Verify SHA-256 & restore cross-DB backup
volo import-legacy <legacy.zip|json> [--dry-run] \
  [--old-site-url URL] [--new-site-url URL] [--storage-local] # Stream & normalize legacy 15-table backup archive
volo doctor [--json]                                         # Run crypto, SQLite WAL, GeoIP & proxy diagnostics
volo healthcheck [--url http://127.0.0.1:8080/healthz]       # Probe /healthz over TCP or Unix Socket (Distroless ready)
volo version                                                 # Print version & Go OS/Arch runtime info
```

```bash
# Local build, unit tests, race detector, benchmarks & binary budget verification
npm --prefix web/admin ci && npm --prefix web/admin run build
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build ./... && CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test -run=^$ -bench=. -benchmem ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
bash scripts/verify-binary-budget.sh
```

---

## License / 开源协议

Licensed under the **Apache License, Version 2.0** ([LICENSE](./LICENSE)). See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for third-party component attributions and [SECURITY.md](./SECURITY.md) for vulnerability reporting.