# Volo-go

**Pure-Go Sovereign Independent Blog Engine · 纯 Go 现代独立主权博客引擎**

`CGO_ENABLED=0` 跨平台静态单二进制 · 体积 **< 20 MiB** · 空载内存 **< 35 MiB** · 原生多方言（SQLite WAL / PostgreSQL / MySQL）· 10 大出口零死角防泄密 · 内嵌 `volo-studio` 主题与 5 大扁平工作区管理台

[简体中文](#简体中文) | [English](#english)

---

## 简体中文

### 核心特性

1. **极致纯净的单文件交付 (`CGO_ENABLED=0`)**
   - 零 C 依赖静态编译，单二进制内置前台主题、管理台 SPA（`< 300 KB` Gzip）、多语言包与完整运维 CLI；
   - 无休眠 CAS 单调唯一 ID、`http.ServeContent` 零拷贝音频/媒体 `Range` (`206 Partial Content`) 传输、静态资源预压缩 `.gz` + 强 `ETag` 协商、优雅停机与原生 Distroless `volo healthcheck` 探针。
2. **`.env` 四层配置底座与三方言存储引擎**
   - 严格按 `CLI Flags > 系统环境变量 (VOLO_*) > .env 文件 > 安全默认值` 解析，支持零配置一键启动；
   - 主密钥 `VOLO_SECRET_KEY` 未配置时自动生成 `0600` 权限 `.secret.key`，通过 `HKDF-SHA256` 派生独立子密钥，数据库敏感凭据（SMTP、S3、Webhook、TOTP、插件密钥）全程 `AES-256-GCM` 落盘加密；
   - 统一 `Dialect` 抽象，原生支持 **SQLite**（默认 WAL 读写池物理隔离，`ReadDB` 强制 `query_only=ON`）、**PostgreSQL 生态**（`pgx/v5`，兼容 Supabase / Neon / CockroachDB）与 **MySQL 生态**（兼容 MySQL 8+ / MariaDB / TiDB）。
3. **正规化领域模型与无损流式迁移**
   - 22+ 张正规化关系表：文章直接绑定分类、真正的独立 Markdown 页面（不污染时间流与归档）、动态导航树、专题系列、多源电子书架、播客托管与每日趋势统计；
   - 内置 `bolo-solo` 旧版备份两阶段流式清洗导入器（自动拆解历史分片数据、自动推导文章分类、100% 保留 `.html` 历史链接 `301` 守护零死链）及 `NDJSON` 跨数据库无损流式备份/恢复。
4. **主权账户体系、双轨持久游客与无感反垃圾**
   - `admin` / `editor`（协作者评论严格按文章作者隔离） / `reader`（纯前台读者，物理阻断后台）三级 RBAC；
   - `Argon2id` 密码哈希（兼容旧备份哈希首次登录静默升级）、WebAuthn Passkey、TOTP + 恢复码、活跃设备会话踢下线与细粒度 Scope `API Token`；
   - 三端自愈 `HMAC` 签名 `GuestPassport` + 加 Pepper `env_fp` 环境指纹 + 动态难度 `SHA-256` PoW 工作量证明，支持游客注册转正后一键认领历史评论。
5. **10 大出口零死角防泄密与隐私媒体管线**
   - `ContentVisibilityGuard` 统一收口密码文章与未解锁 `:::lock` 块，在列表摘要、首图/OG 图、上下篇、未解锁评论、二阶段搜索、RSS/Atom/Podcast、Sitemap、反向链接、`/llms.txt` & `.md` 及非授信插件钩子 10 大出口 **100% 零泄露**，支持限时免密 `ShareToken`；
   - 上传图片魔数校验、`<= 64 MP` 防解压炸弹、自动依据 EXIF `Orientation` 旋转摆正并 **100% 剥离 GPS/相机 EXIF 隐私元数据**、纯 Go 自适应转 WebP、提取宽高防 CLS、纯 Go AWS SigV4 直传 S3 / R2 / MinIO 及全库 9 域孤儿媒体扫描清理。
6. **现代创作工作台、双模插件沙箱与热挂载主题**
   - 扁平 5 大工作区管理台（概览、内容与媒体、外观与导航、分发与连接、系统与安全）+ 6 大分域原子 `PATCH` 热配置 + CodeMirror 6 双栏防丢稿编辑器；
   - 纯 Go `goja` ES6 沙箱插件运行时（细粒度权限声明、防 SSRF 拨号器、超时打断、连续故障自动熔断、`plugin_kv` 持久化与 `.zip` 热装），随仓附带外部插件 `./plugins/kanbanniang` 与外部主题 `./themes/volo-journal`。

### 快速开始

#### 方式一：单文件零配置启动

```bash
# 1. 构建管理台静态产物（开发/源码构建时执行一次）
npm --prefix web/admin ci && npm --prefix web/admin run build

# 2. 纯 Go 静态编译
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.Version=v1.0.0" -o volo ./cmd/volo

# 3. 零配置直接启动（自动初始化 ./data/volo.db 与 0600 密钥文件）
./volo serve
```

浏览器访问 `http://localhost:8080` 即可进入首次建站向导（支持测活数据库、创建管理员或一键导入历史备份包）。

#### 方式二：Docker Compose 部署

```bash
cp .env.example .env
docker compose up -d --build
```

如需切换 PostgreSQL 或 MySQL 后端，可修改 `.env` 中的 `VOLO_DB_DRIVER` / `VOLO_DB_DSN` 并启用对应 profile：

```bash
docker compose --profile postgres up -d
```

### 内置 CLI 运维命令

```bash
volo serve                                         # 启动 HTTP 或 Unix Domain Socket 服务
volo admin reset-password --user <username|email>  # 重置密码为 Argon2id 并吊销全部活跃会话
volo admin disable-2fa --user <username|email>     # 紧急关闭指定账户 TOTP 与恢复码
volo backup create [--output backup.zip]           # 创建跨库流式 NDJSON + 媒体 ZIP 备份
volo backup restore <backup.zip> [--dry-run]       # 校验 SHA-256 并跨库无损恢复备份
volo import-legacy <legacy.zip|json> [--dry-run]   # 流式清洗导入旧版 bolo-solo 备份包
volo doctor [--json]                               # 检查密钥权限、SQLite WAL、目录与安全配置
volo healthcheck [--url http://127.0.0.1:8080/healthz] # 原生探活（兼容 TCP 与 Unix Socket）
volo version                                       # 打印版本与构建信息
```

---

## English

### Highlights

- **Single Static Binary (`CGO_ENABLED=0`)**: `< 20 MiB` stripped binary, `< 35 MiB` idle RSS, lock-free monotonic IDs, zero-copy `http.ServeContent` `Range` streaming, pre-compressed `.gz` negotiation, graceful shutdown, and built-in Distroless `volo healthcheck`.
- **4-Layer `.env` Config & Pure-Go Multi-Dialect DB**: `CLI Flags > VOLO_* Env > .env > Defaults`, `HKDF-SHA256` + `AES-256-GCM` credential encryption at rest, stealth `VOLO_ADMIN_PATH` (returns indistinguishable `404` on default entrypoints), trusted-proxy IP extraction, and unified `Dialect` supporting **SQLite WAL** (split writer/read-only pools), **PostgreSQL** (`pgx/v5`), and **MySQL / MariaDB / TiDB**.
- **Sovereign Identity, Persistent Guest Passport & Stateless PoW**: Strict `admin` / `editor` (author-isolated comment moderation) / `reader` RBAC, `Argon2id` with transparent legacy hash upgrade, WebAuthn Passkeys, TOTP + recovery codes, scoped `API Tokens`, 3-tier self-healing HMAC `GuestPassport`, peppered `env_fp`, dynamic `SHA-256` PoW, and one-click guest comment claiming.
- **10-Surface Zero-Leak Privacy & Privacy Media Pipeline**: Fail-closed `:::lock` blocks and password-protected posts redacted across all 10 surfaces (cards, covers/OG, neighbors, comments, two-stage search, RSS/Atom/Podcast, Sitemap, backlinks, `/llms.txt` & `.md`, untrusted plugins) plus timed `ShareToken` links. Uploads enforce magic-byte & `<= 64 MP` checks, upright EXIF orientation correction, **100% EXIF/GPS stripping**, adaptive pure-Go WebP, pure-Go SigV4 S3/R2/MinIO storage, and 9-scope orphan media pruning.
- **Sandboxed Plugins & Hot-Mountable Themes**: Pure-Go `goja` ES6 plugin pool with capability permissions, DNS-pinning SSRF guard, timeout circuit-breaker, `plugin_kv`, and `.zip` hot-install (`./plugins/kanbanniang` & `./themes/volo-journal` included out of the box).

### Verification & Budget Gate

```bash
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=1 go test -race -count=1 ./...
bash scripts/verify-binary-budget.sh
```