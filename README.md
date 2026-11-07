# Volo-go

[![CI](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml)
[![Security](https://github.com/Willh92/Volo-go/actions/workflows/security.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/security.yml)
[![Release](https://github.com/Willh92/Volo-go/actions/workflows/release.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/release.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-246b51.svg)](./LICENSE)

**Pure-Go Sovereign Independent Blog Engine · 纯 Go 现代独立主权博客引擎**

`CGO_ENABLED=0` 跨平台静态单二进制 · 体积 **`18.52 MiB` (< 20 MiB)** · 温热常驻内存 **`28.60 MiB` (< 35 MiB)** · 原生三方言（SQLite WAL / PostgreSQL / MySQL）· 10 大出口零死角防泄密 · 内嵌 `volo-studio` 主题与按需分包 5 大工作区管理台

[简体中文](#简体中文) | [English](#english)

---

## 简体中文

### 文档导航

| 主题文档 | 说明 |
| :--- | :--- |
| [架构设计与性能内幕 (`docs/zh-CN/architecture.md`)](./docs/zh-CN/architecture.md) | 23 张正规化表、三方言 SQL 与读写分离池、10 大出口防泄密守卫、多级一致性缓存与内存预算 |
| [部署与系统服务安装指南 (`docs/zh-CN/deployment.md`)](./docs/zh-CN/deployment.md) | `.env` 四层配置全表、Linux (`systemd` / `OpenRC`)、macOS (`launchd`)、Windows 服务脚本与 Docker 部署 |
| [反向代理与 CDN 配置指南 (`docs/zh-CN/reverse-proxy.md`)](./docs/zh-CN/reverse-proxy.md) | TCP vs Unix Socket、`VOLO_TRUSTED_PROXIES` 防伪造模型、Nginx / Caddy / Traefik / Apache / Cloudflare 完整配置 |
| [CLI 瑞士军刀与开放 API (`docs/zh-CN/cli-and-api.md`)](./docs/zh-CN/cli-and-api.md) | `volo` 全子命令、细粒度 `volo_pat_*` API Token、Obsidian / PicGo 直连与公开 `/api/v1/*` 接口 |
| [主题定制与插件开发 (`docs/zh-CN/themes-and-plugins.md`)](./docs/zh-CN/themes-and-plugins.md) | `theme.json` + `--volo-*` 设计变量、`goja` 沙箱与外部 HTTP Webhook 插件协议、ZIP 热插拔 |
| [旧版主题皮肤迁移指南 (`docs/zh-CN/theme-migration.md`)](./docs/zh-CN/theme-migration.md) | 独立整页 HTML / 布局继承双模式、`PageContext` 全量数据模型、`config_schema` 自定义配置与模板函数速查 |
| [多语言包扩展指南 (`docs/zh-CN/locale-extension.md`)](./docs/zh-CN/locale-extension.md) | 单文件 JSON / `.zip` 语言包热安装、`admin.nav.*` 无冲突命名空间、内置词条覆盖与完整模板导出 |
| [旧版无损迁移与跨库备份 (`docs/zh-CN/migration-and-backup.md`)](./docs/zh-CN/migration-and-backup.md) | 历史备份流式 ETL 清洗、`301` 永久链接守护、`SQLite ⇄ PostgreSQL ⇄ MySQL` 跨库流式搬家 |

---

### 核心能力与性能实测

| 指标 / 基准测试项 (`CGO_ENABLED=0`) | 实测表现 | 硬性预算 / 说明 |
| :--- | :--- | :--- |
| 静态单二进制体积 (`linux/amd64`) | **`18.52 MiB`** | 跨 5 大平台目标均严格 `< 20.00 MiB` |
| 温热运行常驻内存 (`VmRSS`) | **`28.60 MiB`** | 含内嵌主题、插件沙箱与热缓存 `< 35.00 MiB` |
| 管理台首屏核心体积 (Gzip) | **`35.34 KB`** (总包 `212.3 KB`) | CodeMirror 6 按需懒加载，首屏提速 `83.5%` |
| 文章卡片摘要热缓存 (`ToCard`) | **`1.19 µs/op`** (`864 B/op`) | 4 MiB 摘要自校验 LRU，较冷渲染提速 `758×` |
| 文章正文详情热缓存 (`ToDetail`) | **`4.64 µs/op`** (`3.3 KB/op`) | 严格排除密码与限时预览令牌，零串号风险 |
| RSS / Sitemap 预压缩 + `ETag` 协商 | **`7.69 µs/op`** (`~130k req/s`) | 数据版本号 `1ns` 自动失效，`If-None-Match` 零 CPU `304` |
| 并发文章阅读 + 异步写合并 (`RecordView`) | **`454.9 µs/op`** (双核全链路 SSR+Gzip) | 内存增量实时叠加 + 2s/停机批量落盘，零 SQLite 写锁争用 |
| 二阶段全文检索 (`SearchIndex`) | **`29.4 µs/op`** | 8 MiB 常驻索引 + UTF-8 零切片窗口摘要提取 |

1. **极致纯净的单文件交付 (`CGO_ENABLED=0`)**：零 C 依赖静态编译，内置前台主题、管理台 SPA、多语言字典与运维 CLI；`http.ServeContent` 零拷贝音频/媒体 `Range` (`206 Partial Content`) 传输、BREACH 免疫动态 Gzip、优雅停机确定性刷盘与 Distroless 原生 `volo healthcheck`。
2. **`.env` 四层配置底座与三方言存储引擎**：严格按 `CLI Flags > VOLO_* 环境变量 > .env > 安全默认值` 加载；`HKDF-SHA256` 派生子密钥 + 数据库敏感凭据 `AES-256-GCM` 落盘加密；原生支持 **SQLite WAL**（读写连接池物理隔离，`ReadDB` 强制 `query_only=ON`）、**PostgreSQL**（`pgx/v5`，兼容 Supabase / Neon / CockroachDB）与 **MySQL 8+ / MariaDB / TiDB**。
3. **正规化领域模型与无损跨库迁移**：23 张正规化关系表；文章直接绑定分类、独立 Markdown 页面（不污染博客时间流与归档）、动态导航菜单、专题系列、多源电子书架与播客托管；内置历史备份两阶段流式清洗导入器（100% 保留 `.html` 老链接 `301` 守护零死链）及跨数据库流式备份恢复。
4. **主权账户、双轨持久游客与无感反垃圾**：`admin` / `editor`（协作者评论按作者物理隔离） / `reader` 三级 RBAC；`Argon2id`（兼容历史 `PBKDF2` 登录静默升级）、WebAuthn Passkey、TOTP + 恢复码、活跃设备踢下线、细粒度 `volo_pat_*` API Token；三端自愈 `GuestPassport` + 加 Pepper `env_fp` + 动态 `SHA-256` PoW + 游客转正历史评论一键认领。
5. **10 大出口零死角防泄密与隐私媒体管线**：`ContentVisibilityGuard` 在列表摘要、封面/OG 图、上下篇、评论、全文搜索、RSS/Atom/Podcast、Sitemap、反向链接、`/llms.txt` & `.md` 及非授信插件钩子 10 大出口 **100% 零泄露**，且 HTTP 动态压缩层对任何含会话/解锁 Cookie 或 `?preview=` 响应物理旁路；上传图片强制 `<= 64 MP` 防解压炸弹、自动摆正并 **100% 剥离 GPS/相机 EXIF 元数据**、自适应转 WebP、直传 S3/R2/MinIO 及 9 域孤儿媒体清理。

---

### 快速开始

#### 1. 源码构建与零配置启动

```bash
# 1. 构建内嵌管理台前端产物
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. 纯 Go 静态编译（Selective Inlining 兼顾 <20 MiB 体积与核心内联性能）
CGO_ENABLED=0 go build -trimpath \
  -gcflags="all=-l" -gcflags="github.com/Willh92/Volo-go/...=" \
  -ldflags="-s -w -X main.Version=v1.0.0" \
  -o volo ./cmd/volo

# 3. 零配置直接启动（自动创建 ./data/volo.db 与 0600 主密钥文件）
./volo serve
```

访问 `http://localhost:8080` 进入首次建站向导（支持选库测活、初始化管理员或一键上传导入旧版备份包）。

#### 2. 各操作系统一键安装为后台服务

仓库内置跨平台服务安装脚本与服务单元模板（位于 [`scripts/`](./scripts/) 与 [`deploy/`](./deploy/)，均支持 `--dry-run` 预演）：

```bash
# Linux (自动识别 systemd 与 Alpine/Gentoo OpenRC，支持 TCP 或 Unix Socket)
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --public-url https://blog.example.com \
  --listen-addr 127.0.0.1:8080

# Linux + Nginx 零 TCP 开销 Unix Domain Socket 模式
sudo bash scripts/install-service-linux.sh install \
  --binary ./volo \
  --group www-data \
  --listen-socket /run/volo/volo.sock \
  --socket-mode 0660 \
  --public-url https://blog.example.com

# macOS (默认安装为当前用户 LaunchAgent 免 sudo；加 --system 安装为系统级 LaunchDaemon)
bash scripts/install-service-macos.sh install --binary ./volo --public-url https://blog.example.com

# Windows (PowerShell 管理员运行：自动适配 NSSM 或系统启动计划任务 + NTFS ACL 锁死 .env)
powershell -ExecutionPolicy Bypass -File .\scripts\install-service-windows.ps1 `
  -Action Install -BinaryPath .\volo.exe -PublicUrl "https://blog.example.com"
```

配套开箱即用的 **Nginx（TCP 与 Unix Socket）、Caddy v2、Traefik v3、Apache httpd** 反代配置位于 [`deploy/reverse-proxy/`](./deploy/reverse-proxy/)，详尽说明请参阅 [反向代理与 CDN 配置指南](./docs/zh-CN/reverse-proxy.md)。

#### 3. Docker Compose 部署

```bash
cp .env.example .env
docker compose up -d --build
```

#### 4. 内置 CLI 运维命令

```bash
volo serve                                             # 启动 HTTP 或 Unix Domain Socket 服务
volo admin reset-password --user <username|email>      # 重置密码为 Argon2id 并吊销全部活跃会话
volo admin disable-2fa --user <username|email>         # 紧急关闭指定账户 TOTP 与恢复码
volo backup create [--output backup.zip]               # 创建跨库流式 NDJSON + 媒体 ZIP 备份
volo backup restore <backup.zip> [--dry-run]           # 校验 SHA-256 并跨库无损恢复备份
volo import-legacy <legacy.zip|json> [--dry-run]       # 流式清洗导入历史备份包
volo doctor [--json]                                   # 自检密钥权限、SQLite WAL、目录与反代配置
volo healthcheck [--url http://127.0.0.1:8080/healthz] # 原生探活（兼容 TCP 与 Unix Socket）
volo version                                           # 打印版本与构建信息
```

---

## English

### Documentation Index

| Guide | Topics Covered |
| :--- | :--- |
| [Architecture & Performance Internals (`docs/en-US/architecture.md`)](./docs/en-US/architecture.md) | 23 normalized tables, multi-dialect SQL & split WAL pools, 10-surface privacy perimeter, revision-coherent caches, memory budget |
| [Deployment & OS Service Installers (`docs/en-US/deployment.md`)](./docs/en-US/deployment.md) | 4-layer `.env` reference, Linux (`systemd` / `OpenRC`), macOS (`launchd`), Windows service PowerShell installer, Docker / GHCR |
| [Reverse Proxy & CDN Guide (`docs/en-US/reverse-proxy.md`)](./docs/en-US/reverse-proxy.md) | TCP vs Unix Domain Socket, `VOLO_TRUSTED_PROXIES` anti-spoofing CIDRs, Nginx / Caddy / Traefik / Apache / Cloudflare recipes |
| [CLI & Open API Reference (`docs/en-US/cli-and-api.md`)](./docs/en-US/cli-and-api.md) | Built-in `volo` CLI subcommands, scoped `volo_pat_*` tokens, Obsidian / PicGo automation, and `/api/v1/*` endpoints |
| [Themes & Plugin SDK (`docs/en-US/themes-and-plugins.md`)](./docs/en-US/themes-and-plugins.md) | `theme.json`, `--volo-*` CSS tokens, `goja` ES6 sandbox & signed external HTTP webhooks, ZIP hot-install |
| [Theme Skin Migration Guide (`docs/en-US/theme-migration.md`)](./docs/en-US/theme-migration.md) | Standalone full-HTML & layout inheritance modes, full `PageContext`, `config_schema` fields, and template FuncMap reference |
| [Language Pack Extension Guide (`docs/en-US/locale-extension.md`)](./docs/en-US/locale-extension.md) | Hot-installable JSON / `.zip` language packs, `admin.nav.*` namespace, built-in overrides, and full starter template export |
| [Legacy Migration & Cross-DB Backup (`docs/en-US/migration-and-backup.md`)](./docs/en-US/migration-and-backup.md) | Streaming legacy 15-table ETL, `301` `.html` permalink preservation, `SQLite ⇄ PostgreSQL ⇄ MySQL` streaming backups |

---

### Highlights

- **Single Static Binary (`CGO_ENABLED=0`)**: `18.52 MiB` stripped binary (`< 20 MiB` across all 5 OS/Arch targets), `28.60 MiB` warm RSS (`< 35 MiB`), lazy code-split Admin Console (`35.34 KB` initial Gzip), zero-copy `http.ServeContent` `Range` streaming, BREACH-safe pooled Gzip, ETag `304` pre-compressed feeds, and deterministic graceful shutdown.
- **4-Layer `.env` Config & Pure-Go Multi-Dialect DB**: `CLI Flags > VOLO_* Env > .env > Defaults`, `HKDF-SHA256` + `AES-256-GCM` credential encryption at rest, stealth `VOLO_ADMIN_PATH`, right-to-left `VOLO_TRUSTED_PROXIES` IP extraction, and unified `Dialect` supporting **SQLite WAL** (split writer/read-only pools), **PostgreSQL** (`pgx/v5`), and **MySQL 8+ / MariaDB / TiDB**.
- **Sovereign Identity, Persistent Guest Passport & Stateless PoW**: Strict `admin` / `editor` (author-isolated comment moderation) / `reader` RBAC, `Argon2id` with seamless legacy hash upgrade, WebAuthn Passkeys, TOTP + recovery codes, scoped `volo_pat_*` API tokens, 3-tier self-healing `GuestPassport`, peppered `env_fp`, dynamic `SHA-256` PoW, and one-click guest comment claiming.
- **10-Surface Zero-Leak Privacy & Privacy Media Pipeline**: Fail-closed `:::lock` blocks and password-protected posts redacted across all 10 surfaces plus timed `ShareToken` links. Uploads enforce magic-byte & `<= 64 MP` checks, upright EXIF orientation correction, **100% EXIF/GPS stripping**, adaptive pure-Go WebP, SigV4 S3/R2/MinIO storage, and 9-scope orphan media pruning.
- **Multi-OS Service Scripts & Reverse Proxy Templates**: Ready-to-run installers for Linux (`systemd` + `OpenRC`), macOS (`launchd`), and Windows (`PowerShell` NSSM / Scheduled Task) under [`scripts/`](./scripts/) alongside Nginx, Caddy, Traefik, and Apache templates under [`deploy/`](./deploy/).

### Verification & Benchmark Suite

```bash
npm --prefix web/admin ci && npm --prefix web/admin run build
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
CGO_ENABLED=0 go test -run=^$ -bench=. -benchmem ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
bash scripts/verify-binary-budget.sh
```

---

## License / 开源协议

Licensed under the **Apache License, Version 2.0** ([LICENSE](./LICENSE)). See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for third-party component attributions and [SECURITY.md](./SECURITY.md) for vulnerability reporting.