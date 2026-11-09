# CLI 运维命令与 API 令牌接入指南

> English version: [CLI & API Token Reference](../en-US/cli-and-api.md)

`Volo-go` 在单个静态二进制中内置完整的运维 CLI 子命令集，并提供细粒度权限范围（Scope）的个人访问令牌（Personal Access Token，`volo_pat_*`），支持 Obsidian、PicGo、CI/CD 脚本与第三方客户端免 Cookie 直连。

---

## 1. CLI 子命令全景速查

所有子命令共享统一的配置解析优先级（`CLI 参数 > VOLO_* 操作系统环境变量 > .env 文件 > 安全默认值`，其中 `upload_dir` 与 `geoip_dir` 在未通过前 3 层锁定时还会读取管理台数据库设置），并支持以下通用标志：

| 通用 CLI 标志 | 对应环境变量 | 说明 |
| :--- | :--- | :--- |
| `--env-file <path>` | `VOLO_ENV_FILE` | 指定 `.env` 配置文件路径（默认 `./.env`） |
| `--listen-addr <host:port>` | `VOLO_LISTEN_ADDR` | TCP 监听地址（默认 `0.0.0.0:8080`，与 `--listen-socket` 互斥） |
| `--listen-socket <path[,mode]>` | `VOLO_LISTEN_SOCKET` | Unix Domain Socket 路径（可选附带八进制权限后缀，如 `/run/volo/volo.sock,0660`） |
| `--socket-mode <octal>` / `--socket-group <group>` | `VOLO_SOCKET_MODE` / `VOLO_SOCKET_GROUP` | Unix Socket 权限掩码（默认 `0660`）与所属用户组（如 `www-data`、`nginx`） |
| `--public-url <url>` | `VOLO_PUBLIC_URL` | 站点规范公网根地址（用于 Canonical、RSS/Podcast、Sitemap、WebAuthn Origin 与邮件链接） |
| `--trusted-proxies <cidrs\|unix>` | `VOLO_TRUSTED_PROXIES` | 受信反向代理 IP/CIDR 列表或 `unix`（默认 `127.0.0.1,::1`） |
| `--admin-path </prefix>` | `VOLO_ADMIN_PATH` | 自定义隐身管理台基础前缀（默认 `/console`） |
| `--secret-key <secret>` | `VOLO_SECRET_KEY` | 主密钥（`>= 16` 字符；留空则自动生成 `0600` 权限 `${VOLO_DATA_DIR}/.secret.key`） |
| `--geo-access-enabled=true\|false` | `VOLO_GEO_ACCESS_ENABLED` | 启用或紧急熔断旁路离线 GeoIP 访问控制规则 |
| `--default-locale <code>` | `VOLO_DEFAULT_LOCALE` | 默认回退语言（如 `zh-CN`、`en-US`） |
| `--data-dir` / `--upload-dir` / `--backup-dir` / `--geoip-dir` | `VOLO_DATA_DIR` / `VOLO_UPLOAD_DIR` / `VOLO_BACKUP_DIR` / `VOLO_GEOIP_DIR` | 数据、上传媒体、滚动备份与离线 GeoIP 数据库目录 |
| `--themes-dir` / `--plugins-dir` / `--locales-dir` | `VOLO_THEMES_DIR` / `VOLO_PLUGINS_DIR` / `VOLO_LOCALES_DIR` | 外部主题、插件与语言包热挂载目录 |
| `--db-driver <sqlite\|postgres\|mysql>` | `VOLO_DB_DRIVER` | 数据库驱动（留空则优先读取 `/setup` 向导加密持久化的 `${VOLO_DATA_DIR}/db.json`） |
| `--db-dsn <dsn>` | `VOLO_DB_DSN` | 数据库连接串（SQLite 留空默认 `${VOLO_DATA_DIR}/volo.db`） |
| `--db-simple-protocol` | `VOLO_DB_SIMPLE_PROTOCOL` | 为 PostgreSQL 事务池（Supabase / PgBouncer）启用 `pgx` Simple Query 协议 |
| `--shutdown-timeout <sec>` | `VOLO_SHUTDOWN_TIMEOUT_SEC` | 优雅停机排空超时秒数（范围 `1`–`120`，默认 `15`） |

### 1.1 `volo serve`（默认命令）

启动 HTTP 或 Unix Domain Socket 服务，自动执行幂等数据库 Schema 迁移（`v6`）、预热全文检索索引与只读元数据快照，启动定时文章发布器与滚动备份调度器，并监听 `SIGINT` / `SIGTERM` 执行优雅停机（排空在途请求 + 确定性刷盘异步阅读计数）。

```bash
# 零配置启动（默认监听 0.0.0.0:8080，自动初始化 ./data/volo.db）
./volo serve

# 显式指定配置文件与隐身管理台入口
./volo serve \
  --env-file /etc/volo/volo.env \
  --listen-addr 127.0.0.1:8080 \
  --public-url https://blog.example.com \
  --admin-path /studio-secret
```

### 1.2 `volo admin reset-password` / `volo passwd`（紧急重置密码并吊销会话与令牌）

当管理员遗忘密码时，无需手工修改数据库，直接通过终端重置为高强度 `Argon2id` 哈希，自动清空历史遗留哈希、同时吊销该账户全部活跃浏览器会话（`user_sessions`）与个人访问令牌（`api_tokens`），并写入 `audit_logs` 安全审计记录（`volo passwd` 为等价快捷别名）：

```bash
# 省略 --password 时自动生成符合复杂度策略的高强度临时密码（如 Volo-<hex>9a）并打印至终端
./volo admin reset-password --user admin --env-file /etc/volo/volo.env

# 显式指定新密码（或使用等价快捷命令 volo passwd）
./volo admin reset-password --user admin@example.com --password 'MyNewStrongPass#2026' --env-file /etc/volo/volo.env
```

> **提示**：该命令仅接受 `--user <username|email>`（必填）与可选 `--password <new_password>`（及通用配置参数）；在生产服务器上推荐省略 `--password` 由程序生成一次性随机强密码并在登录后修改，避免明文密码残留在 Shell 历史记录中。

### 1.3 `volo admin disable-2fa`（紧急熔断双因素认证）

当手机丢失且恢复码遗失导致无法通过二次验证时，一键清除指定用户的 `totp` 与 `recovery_codes` 凭据并记录本地 CLI 审计日志：

```bash
./volo admin disable-2fa --user admin@example.com --env-file /etc/volo/volo.env
```

### 1.4 `volo backup create` 与 `volo backup restore`（跨库流式备份与恢复）

详见 [迁移与跨库流式备份指南](./migration-and-backup.md)。

```bash
# 创建包含媒体文件的流式 ZIP 备份包（默认 --with-media=true；未传 --output 时写入 ${VOLO_BACKUP_DIR}/volo-backup-<UTC时间戳>.zip）
./volo backup create --output ./volo-backup.zip --with-media=true

# 仅导出 33 张正规化业务表的 NDJSON 数据流（跳过本地媒体目录）
./volo backup create --output ./volo-db-only.zip --with-media=false

# 先执行 --dry-run 校验所有 NDJSON 分流 SHA-256 摘要与行数，零数据库改动
./volo backup restore ./volo-backup.zip --dry-run

# 正式流式恢复（支持 SQLite ⇄ PostgreSQL ⇄ MySQL 跨数据库无损搬迁）
./volo backup restore ./volo-backup.zip
```

### 1.5 `volo import-legacy`（历史旧版备份流式清洗导入）

支持直接流式解析历史 15 表 `.zip` 备份包或 `data.json` 归档，并提供域名重写与本地存储回退开关：

| 专属参数 | 说明 |
| :--- | :--- |
| `--dry-run` | 仅流式解析并输出结构化清洗预览报告 JSON，零数据库/磁盘写入 |
| `--old-site-url <urls>` | 逗号或空格分隔的历史旧域名列表（将正文与设置中的旧站绝对链接重写为站内相对路径） |
| `--new-site-url <url>` | 导入完成后覆盖写入的新站规范根 URL |
| `--storage-local` | 导入后强制将媒体存储驱动重置为本地磁盘（`storage.active_driver = "local"`） |

```bash
# 预览旧版 15 表 ZIP/JSON 归档清洗报告（零写入）
./volo import-legacy ./legacy-solo-backup.zip --dry-run

# 正式导入：重写历史域名链接、切换新站 URL 并强制回退本地存储驱动
./volo import-legacy ./legacy-solo-backup.zip \
  --old-site-url "https://old.example.com,http://old.example.com" \
  --new-site-url "https://blog.example.com" \
  --storage-local
```

### 1.6 `volo doctor`、`volo healthcheck` 与 `volo version`（自检诊断与容器探活）

```bash
# 人类可读诊断报告（检查主密钥权限、管理台路径状态、SQLite WAL 模式、离线 GeoIP 多库挂载、目录可写性与反代配置）
./volo doctor

# 输出机器可读 JSON（非健康状态退出码为 1，适合 CI / 监控采集）
./volo doctor --json

# 探测运行中实例的 /healthz（未指定 --url 时自动根据配置探测本地 TCP 端口或 Unix Domain Socket http://unix/healthz）
./volo healthcheck
./volo healthcheck --url http://127.0.0.1:8080/healthz

# 打印版本号、Go 工具链与目标架构信息
./volo version
```

---

## 2. 细粒度 API Token（`volo_pat_*`）与管理端接口直连

在管理台 **「系统与安全 -> 账户与安全 -> API 令牌」** 中可签发带过期时间与最小权限 Scope 的个人访问令牌。

- 数据库仅存储令牌的 `SHA-256` 摘要（`token_hash`）与前 12 位展示前缀（`token_prefix`），明文仅在创建时显示一次；
- 使用 `Authorization: Bearer volo_pat_<secret>` 请求 `/api/admin/*` 接口时自动走无状态鉴权，无需携带浏览器 Session Cookie 或 `X-CSRF-Token`。

### 2.1 权限范围（Scopes）对照表

| Scope 标识 | 允许访问的操作范围 | 典型集成场景 |
| :--- | :--- | :--- |
| `articles:read` | 读取个人信息（`/api/admin/me`）、概览指标、文章/独立页面/媒体列表 | 只读统计面板、文章本地备份脚本 |
| `articles:write` | 创建、更新、删除文章，管理并拖拽排序分类（`/api/admin/categories/reorder`）、标签、专题系列与电子书架（`/api/admin/books/reorder`） | Obsidian 发布插件、CI 自动同步 Markdown 仓库 |
| `pages:write` | 创建、更新、删除独立 Markdown 页面（支持绑定主题 `template` 模板，如 `/about`、`/now`） | 外部自动化更新个人状态页 |
| `media:write` | 上传媒体资源（自动剥离 GPS EXIF、摆正方向并自适应编码 WebP） | PicGo / ShareX / uPic 图床直传 |
| `backup:read` | 流式下载全站备份包（`GET /api/admin/backups/export`） | 异地定时拉取加密冷备脚本 |

> **安全边界**：系统设置热更新（`PATCH /api/admin/settings/{domain}`）、用户权限变更、WAF 封禁及插件安装/导出（`POST /api/admin/plugins/install`、`GET /api/admin/plugins/{id}/export`）等高危管理操作**强制要求交互式管理员会话（含 2FA 策略校验）**，任何 API Token 均无法越权调用。

### 2.2 `curl` / Obsidian 发布与更新文章示例（`articles:write`）

创建（`POST /api/admin/articles`）与更新（`PUT /api/admin/articles/{id}`）文章使用统一的规范 JSON 字段名（注意摘要、正文与置顶字段分别为 **`summary_md`**、**`content_md`** 与 **`is_pinned`**）：

```bash
export VOLO_URL="https://blog.example.com"
export VOLO_TOKEN="volo_pat_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

curl -X POST "${VOLO_URL}/api/admin/articles" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "title": "从零设计低内存纯 Go 博客引擎",
    "slug": "pure-go-blog-architecture",
    "category_id": "",
    "tags": ["Go", "Architecture"],
    "summary_md": "探讨单二进制静态编译、SQLite WAL 读写隔离与版本感知缓存。",
    "content_md": "## 核心目标\n\n在 35 MiB 内存预算内跑满高并发读吞吐。",
    "status": "published",
    "allow_comment": true,
    "is_pinned": false
  }'
```

### 2.3 PicGo / CLI 图床上传示例（`media:write`）

```bash
curl -X POST "${VOLO_URL}/api/admin/media/upload" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -F "file=@./screenshot.png"
```

成功响应（`201 Created`）同时返回完整 `asset` 元数据对象与顶层便捷字段 `url`（便于 PicGo 直接配置 JSON 路径 `url`）：

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

### 2.4 分类/书架批量重排与插件打包导出管理接口

除常规 CRUD 外，管理端提供以下高频编排与生态分发接口：

- **`POST /api/admin/categories/reorder`** 与 **`POST /api/admin/books/reorder`**：按传入 ID 顺序单事务批量重排分类或书架项目的 `sort_order`：
  ```bash
  curl -X POST "${VOLO_URL}/api/admin/categories/reorder" \
    -H "Authorization: Bearer ${VOLO_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"ids": ["0196ed8c01", "0196ed8c02", "0196ed8c03"]}'
  ```
- **`GET /api/admin/plugins/{id}/export`**（需交互式管理员会话）：将已安装插件目录打包导出为纯净 `.zip` 归档（自动剔除运行态临时文件与数据库内的 `secret_ciphertext` 机密值，安全用于跨站迁移或二次分发），配套 `POST /api/admin/plugins/install?overwrite=true` 支持保留配置与机密字段的原地热升级。

---

## 3. 前台公开接口与机器可读订阅端点

| 路径 | 方法 | 说明 |
| :--- | :--- | :--- |
| `/healthz` | `GET` / `HEAD` | 存活与数据库只读探针：正常返回 `200` `{"status":"ok","database":"ok","uptime_sec":120}`；数据库异常返回 `503` `{"status":"degraded","database":"unavailable","uptime_sec":120}` |
| `/rss.xml` / `/atom.xml` | `GET` | 标准 RSS 2.0 与 Atom 1.0 订阅源（支持强 `ETag` / `304`，自动脱敏加密文章与 `:::lock` 块） |
| `/podcast.xml` | `GET` | Apple Podcasts / Spotify 兼容 iTunes 播客订阅源 |
| `/sitemap.xml` / `/robots.txt` | `GET` | 搜索引擎 XML 站点地图（含多语言 `hreflang`；当公开 URL 总数 `<= 10,000` 时直接返回 `<urlset>`，**超过 `10,000` 条时自动分片输出 `<sitemapindex>` 并托管 `/sitemap-1.xml`、`/sitemap-2.xml` 等子分片**）与爬虫协议（启用自定义隐身管理台路径时自动隐藏后台 `Disallow` 条目） |
| `/llms.txt` / `/llms-full.txt` | `GET` | AI 大语言模型结构化索引与公开语料汇总（可于订阅设置 `feed.enable_llms_txt` 一键关闭） |
| `/posts/{slug}.md` | `GET` | 单篇公开文章的纯净原始 Markdown 视图（受 `feed.enable_raw_markdown` 控制，密码锁文章未授权返回 `403`） |
| `/api/v1/search?q=<kw>&page=<n>` | `GET` | 二阶段毫秒级分页全文检索（支持 `q` 关键词与 `page` 页码参数，返回高亮摘要片段 `hits` 与分页元数据，100% 剥离密码文章与 `:::lock` 内容） |
| `/api/v1/i18n/{locale}` | `GET` | 获取合并自定义覆盖词条后的前端多语言字典包（支持 `ETag` 缓存协商） |
| `/api/v1/pow/challenge` | `GET` | 获取无第三方依赖的 `HMAC + SHA-256` 工作量证明挑战令牌 |
| `/api/v1/guest/passport` | `GET` / `POST` | 签发或自愈刷新三端持久游客签名通行证（`__volo_gp`） |
| `/api/v1/articles/{id}/unlock` | `POST` | 校验文章或独立页面（`page:<id>`）阅读口令并下发限定作用域的解锁凭证 |
| `/api/v1/comments` | `GET` / `POST` | 分页查询文章/页面评论树或提交经 PoW 校验的新评论 |
| `/api/v1/reactions` | `POST` | 文章与评论轻量表情互动计数 |
