# Volo-go

[![CI](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/ci.yml)
[![Security](https://github.com/Willh92/Volo-go/actions/workflows/security.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/security.yml)
[![Release](https://github.com/Willh92/Volo-go/actions/workflows/release.yml/badge.svg)](https://github.com/Willh92/Volo-go/actions/workflows/release.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache_2.0-246b51.svg)](./LICENSE)

**纯 Go 现代独立主权博客引擎**

`CGO_ENABLED=0` 跨 6 大平台静态单二进制 · 全 6 平台体积均 **`< 21 MiB`** · 温热常驻内存 **`24.23 MiB` (< 35 MiB)** · 原生三方言（SQLite WAL / PostgreSQL / MySQL）· 实时无损图片变换（`?imageView2` + KV）· 10 大出口零死角防泄密 · 三模式（`IR` / `WYSIWYG` / `SV`）Studio 编辑器与全屏 Markdown 评论框

[简体中文](./README.zh-CN.md) | [English](./README.md) · [版本更新日志](./CHANGELOG.zh-CN.md) | [Changelog](./CHANGELOG.md)

---

## 文档导航

| 主题文档 | 说明 |
| :--- | :--- |
| [架构设计与性能内幕 (`docs/zh-CN/architecture.md`)](./docs/zh-CN/architecture.md) | `v8` Schema（33 张正规化数据表 + 9 大热重载设置域）、实时无损 ImageView 管线、三方言 SQL 与读写分离池、10 大出口防泄密守卫、多级一致性缓存与内存预算 |
| [部署与系统服务安装指南 (`docs/zh-CN/deployment.md`)](./docs/zh-CN/deployment.md) | 多层配置优先级、6 大静态发布平台、Linux (`systemd` / `OpenRC` 权限分离)、macOS (`launchd`)、Windows 原生服务 (`sc.exe` / `NSSM`)、升级自动回滚与 Docker 部署 |
| [反向代理与 CDN 配置指南 (`docs/zh-CN/reverse-proxy.md`)](./docs/zh-CN/reverse-proxy.md) | TCP vs Unix Socket (`--unix-socket` / `--socket-mode` / `--socket-group`)、`VOLO_TRUSTED_PROXIES` 防伪造模型、Nginx / Caddy / Traefik / Apache / Cloudflare 完整配置 |
| [CLI 瑞士军刀与开放 API (`docs/zh-CN/cli-and-api.md`)](./docs/zh-CN/cli-and-api.md) | `volo` 全子命令、细粒度 `volo_pat_*` API Token、实时图片裁剪参数（`?imageView2` & KV）、账户封禁/禁言接口、分页搜索与管理端/公开端 REST 接口 |
| [主题定制与插件开发 (`docs/zh-CN/themes-and-plugins.md`)](./docs/zh-CN/themes-and-plugins.md) | `theme.json`、增强型全屏评论 Markdown 编辑器、纯 Go ES6 插件沙箱（5 阶段生命周期、`before_*` 链式拦截、`volo.kv.incrAsync`、`CronRegistry`、`wordFilter`）与 ZIP 热插拔/导出 |
| [旧版主题皮肤迁移指南 (`docs/zh-CN/theme-migration.md`)](./docs/zh-CN/theme-migration.md) | 独立整页 HTML / 布局继承双模式、`PageContext` 全量数据模型、`config_schema` 自定义配置与模板函数速查 |
| [多语言包扩展指南 (`docs/zh-CN/locale-extension.md`)](./docs/zh-CN/locale-extension.md) | 单文件 JSON / `.zip` 语言包热安装（附完整 `zh-TW` / `ja-JP` 语言包）、`admin.nav.*` 命名空间与模板导出 |
| [旧版无损迁移与跨库备份 (`docs/zh-CN/migration-and-backup.md`)](./docs/zh-CN/migration-and-backup.md) | 历史 15 表备份流式 ETL 清洗、`301` 永久链接守护、`SQLite ⇄ PostgreSQL ⇄ MySQL` 33 表跨库流式搬家 |
| [版本更新日志 (`CHANGELOG.zh-CN.md`)](./CHANGELOG.zh-CN.md) | 各版本新特性、修复记录与升级说明 |

---

## 核心能力与性能实测

| 指标 / 基准测试项 (`CGO_ENABLED=0`) | 实测表现 | 硬性预算 / 说明 |
| :--- | :--- | :--- |
| 静态单二进制体积 (`linux/amd64`) | **`19.68 MiB`** | 跨全部 6 大平台目标（Linux / macOS / Windows `amd64` & `arm64`）均严格 **`< 21.00 MiB`**（压缩包 `< 8.00 MiB`） |
| 温热运行常驻内存 (`VmRSS`) | **`24.23 MiB`** | 含内嵌主题、插件沙箱、`wordFilter` 引擎与有界热缓存 `< 35.00 MiB` |
| 管理台首屏核心体积 (Gzip) | **`~85 KB`** (总内嵌 `< 300 KB`) | 工作区与 CodeMirror 6 三模式编辑器核心按需懒加载，首屏提速 `66%+` |
| 文章卡片摘要热缓存 (`ToCard`) | **`1.19 µs/op`** (`864 B/op`) | 4 MiB 摘要自校验 LRU，较冷渲染提速 `758×` |
| 文章正文详情热缓存 (`ToDetail`) | **`4.64 µs/op`** (`3.3 KB/op`) | 严格排除密码与限时预览令牌，零串号风险 |
| RSS / Sitemap 预压缩 + `ETag` 协商 | **`7.69 µs/op`** (`~130k req/s`) | 数据版本号 `1ns` 自动失效，超 `10,000` URL 自动 `<sitemapindex>` 分片 |
| 并发文章阅读 + 异步写合并 (`RecordView`) | **`454.9 µs/op`** (双核全链路 SSR+Gzip) | 内存增量实时叠加 + 2s/停机批量落盘，零 SQLite 写锁争用 |
| 二阶段全文检索 (`SearchIndex`) | **`29.4 µs/op`** | 8 MiB 常驻索引 + UTF-8 零切片窗口摘要提取 + 分页 `/api/v1/search` |

1. **极致纯净的 6 平台 `< 21 MiB` 单文件交付 (`CGO_ENABLED=0`)**：基于 Go `1.27.4`（`GOEXPERIMENT=nojsonv2`）零 C 依赖静态编译，自研零反射 `tinyxml` 与纯 Go SMTP/PGv3 驱动，覆盖 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`；内置前台主题、管理台 SPA、多语言字典（附外置 `zh-TW` / `ja-JP`）与运维 CLI；`http.ServeContent` 零拷贝音视频 `Range` (`206 Partial Content`) 传输、BREACH 免疫动态 Gzip、优雅停机（`VOLO_SHUTDOWN_TIMEOUT_SEC`）确定性刷盘与只读安全 `volo healthcheck`。
2. **多层配置底座与自研纯 Go 三方言存储引擎**：基础启动参数按 `CLI Flags > VOLO_* 环境变量 > .env > 安全默认值` 解析，上传目录与 GeoIP 目录支持 `CLI 参数 (--upload-dir / --geoip-dir) > 管理台数据库设置 > 环境变量 / .env > 默认值` 灵活覆盖（附目录读写自检）；不显式固定 `VOLO_DB_DRIVER` 时，`/setup` 向导经 `AES-256-GCM` 加密保存的 `data/db.json`（SQLite / PostgreSQL / MySQL）可在容器与服务重启后持久生效；原生支持 **SQLite WAL**（读写连接池物理隔离）、**PostgreSQL**（自研零依赖 `pgwire` PGv3 驱动）与 **MySQL 8+ / MariaDB / TiDB**。
3. **正规化领域模型（`v8` Schema，33 张数据表 + 9 大设置域）、创作编辑器与全屏评论框**：涵盖 9 大原子热重载设置域（含 4 种 `seo.post_permalink_style` 固定链接风格实时切换）；后台内置 **IR / WYSIWYG / SV** 三模式 Studio 编辑器，前台内置移动端软键盘友好的统一 **Markdown 评论/回复编辑器**（工具栏、净化实时预览、全屏沉浸编辑、按身份字数/行数门禁与超高评论防刷屏折叠）。
4. **动态隐身后台、三档阶梯自动风控、多库离线 GeoIP 与账户/IP 治理**：支持管理台免重启热切换隐身路径（`security.admin_path`）、可配置三档阶梯自动风控与 IP/CIDR 免封白名单（`risk_whitelist_cidrs`）、保留轨迹的 IP 封禁全生命周期管理与封禁/审计/插件日志保留策略，以及 `GeoLite2` + `ip2region` 多库共存热重载；`admin` / `editor` / `reader` RBAC、`Argon2id`、WebAuthn Passkey、TOTP、Step-Up 二次验证、`volo_pat_*` Token、三端自愈 `GuestPassport` + 动态 PoW，**永久/限时封禁、禁言与可逆历史评论一键文案屏蔽（解封或到期自动无损恢复）**，并提供**评论回收站**（完整恢复、彻底删除时脱敏、可配置自动清理、保留楼层占位）与迁移评论游客信息编辑。
5. **实时无损图片变换（`?imageView2` & KV）、10 大出口防泄密与链式插件体系**：`/uploads/...`（及 Solo 历史图片路径）同时兼容七牛云原生 `?imageView2/<mode>/w/960/h/540/format/webp/q/85/...` 与标准键值对 `?mode=1&w=960&h=540&format=webp&q=85&colors=128&strip&ignore-error=1` 任意组合（不改原图、无参零开销直出、`singleflight` 合并 + `8 MiB` LRU + `304 ETag`，并联动孤儿扫描防误删与 CLS 宽高推导）；`ContentVisibilityGuard` 守护 10 大出口 **100% 零泄露**；插件支持纯 Go ES6 沙箱（原型冻结与深度护栏）、5 阶段生命周期、`before_*` 链式拦截、`volo.kv.incrAsync` 异步写合并、`CronRegistry` 与原生 `wordFilter` 敏感词引擎。

---

## 快速开始

### 1. 源码构建与零配置启动（Go `1.27.4` + Node.js `24 LTS`）

```bash
# 1. 构建内嵌管理台前端产物
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. 纯 Go 静态编译（< 21 MiB 单二进制）
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build -buildvcs=false -trimpath \
  -gcflags="all=-l" -ldflags="-s -w -buildid=" ./cmd/volo

# 3. 零配置直接启动（自动创建 ./data/volo.db 与 0600 主密钥文件）
./volo serve
```

访问 `http://localhost:8080` 进入首次建站向导 `/setup`（支持选库测活、初始化管理员或一键上传导入旧版备份包）。

### 2. 跨操作系统服务安装与带自动回滚的在线更新

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

### 3. 原生 HTTPS（可选）

小型单机部署可不依赖反向代理，由 Volo-go 直接终止 TLS。同时设置 `VOLO_TLS_CERT_FILE`（`--tls-cert`，PEM 证书链）与 `VOLO_TLS_KEY_FILE`（`--tls-key`，PEM 私钥）即在 `VOLO_LISTEN_ADDR` 上启用 HTTPS：

```bash
VOLO_LISTEN_ADDR=0.0.0.0:443 \
VOLO_PUBLIC_URL=https://blog.example.com \
VOLO_TLS_CERT_FILE=/etc/volo/tls/fullchain.pem \
VOLO_TLS_KEY_FILE=/etc/volo/tls/privkey.pem \
VOLO_TLS_MIN_VERSION=1.2 \
VOLO_TLS_HTTP_REDIRECT_ADDR=:80 \
./volo serve
```

- **证书热重载**：acme.sh / certbot / lego 续期覆盖证书或私钥文件后自动生效（最多每 30 秒检测一次），无需重启；续期文件写入不完整时继续使用上一份有效证书。
- **HTTP/2** 通过 ALPN（`h2`，自动回退 HTTP/1.1）协商；`VOLO_TLS_MIN_VERSION` 可选 `1.2`（默认）或 `1.3`。
- **可选跳转**：`VOLO_TLS_HTTP_REDIRECT_ADDR`（`--tls-http-redirect-addr`，如 `:80`）额外监听明文 HTTP，并以 `308` 跳转到 `VOLO_PUBLIC_URL` 主机（从不信任客户端 `Host` 头）。
- **监听 1024 以下端口**需要授权：使用内置 systemd 单元时通过 `sudo systemctl edit volo` 添加 `[Service]` `AmbientCapabilities=CAP_NET_BIND_SERVICE`；非 systemd 场景可执行 `sudo setcap 'cap_net_bind_service=+ep' /usr/local/bin/volo`（每次升级二进制后需重新执行；`NoNewPrivileges=true` 下文件 capability 不生效）。服务用户须能读取私钥文件（建议通过 ACME deploy hook 复制，而非直接指向仅 root 可读的 `/etc/letsencrypt/live`）。
- 原生 TLS **不能与 Unix Socket 监听**（`VOLO_LISTEN_SOCKET`）同时使用；`volo doctor` 会检查证书有效性与到期时间，`volo healthcheck` 自动改用 `https://` 探测。
- 多站点共用主机、接入 CDN 或需要自动签发 ACME 证书时，**仍推荐使用 Nginx / Caddy 反向代理部署**。

### 4. Docker Compose 部署

默认挂载命名卷 `volo_data:/app/data`，并将 `VOLO_DB_DRIVER` 留空，使 `/setup` 向导保存的 `data/db.json` 跨容器重建持久生效（若改用宿主机目录绑定挂载 `./data:/app/data`，请先执行 `mkdir -p data && chown 65532:65532 data` 授权 Distroless `nonroot` 用户）：

```bash
cp .env.example .env
docker compose up -d --build
```

### 5. 内置 CLI 运维命令与开发校验

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
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go build ./... && CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go vet ./...
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go test -count=1 ./...
CGO_ENABLED=1 GOEXPERIMENT=nojsonv2 go test -race -count=1 ./...
CGO_ENABLED=0 GOEXPERIMENT=nojsonv2 go test -run=^$ -bench=. -benchmem ./internal/render ./internal/visibility ./internal/db/sqlx ./internal/server
GOEXPERIMENT=nojsonv2 bash scripts/verify-binary-budget.sh
```

---

## 开源协议

本项目基于 **Apache License, Version 2.0**（[LICENSE](./LICENSE)）开源。第三方组件声明见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)，安全漏洞上报方式见 [SECURITY.md](./SECURITY.md)。
