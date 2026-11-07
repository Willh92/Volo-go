# CLI 运维命令与 API 令牌接入指南

> English version: [CLI & API Token Reference](../en-US/cli-and-api.md)

`Volo-go` 在单个静态二进制中内置完整的运维 CLI 子命令集，并提供细粒度权限范围（Scope）的个人访问令牌（Personal Access Token，`volo_pat_*`），支持 Obsidian、PicGo、CI/CD 脚本与第三方客户端免 Cookie 直连。

---

## 1. CLI 子命令全景速查

所有子命令共享统一的四层配置解析优先级（`CLI 参数 > VOLO_* 环境变量 > .env 文件 > 安全默认值`），可通过 `--env-file /path/to/volo.env` 指定任意配置文件。

### 1.1 `volo serve`（默认命令）

启动 HTTP 或 Unix Domain Socket 服务，自动执行幂等数据库 Schema 迁移（`v2`）、预热全文检索索引与只读元数据快照，并监听 `SIGINT` / `SIGTERM` 执行优雅停机（排空在途请求 + 确定性刷盘异步阅读计数）。

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

### 1.2 `volo admin reset-password`（紧急重置密码并踢下线全部设备）

当管理员遗忘密码时，无需手工修改数据库，直接通过终端重置为高强度 `Argon2id` 哈希，自动清空历史遗留哈希、吊销该账户所有活跃 `user_sessions`，并写入 `audit_logs` 安全审计记录：

```bash
# 自动生成高强度临时密码并打印至终端
./volo admin reset-password --user admin --env-file /etc/volo/volo.env

# 从标准输入安全读取新密码（避免留在 shell history 中）
printf '%s\n' 'MyNewStrongPass#2026' | ./volo admin reset-password --user admin --password-stdin
```

### 1.3 `volo admin disable-2fa`（紧急熔断双因素认证）

当手机丢失且恢复码遗失导致无法通过二次验证时，一键清除指定用户的 `totp` 与 `recovery_codes` 凭据并记录本地 CLI 审计日志：

```bash
./volo admin disable-2fa --user admin@example.com --env-file /etc/volo/volo.env
```

### 1.4 `volo backup create` 与 `volo backup restore`（跨库流式备份与恢复）

详见 [迁移与跨库流式备份指南](./migration-and-backup.md)。

```bash
# 创建包含媒体文件的流式 ZIP 备份包（恒定低内存，不驻留整包）
./volo backup create --output ./volo-backup.zip --with-media=true

# 先执行 --dry-run 校验所有 NDJSON 分流 SHA-256 摘要与行数，不改动数据库
./volo backup restore ./volo-backup.zip --dry-run

# 正式流式恢复（支持 SQLite ⇄ PostgreSQL ⇄ MySQL 跨数据库无损搬迁）
./volo backup restore ./volo-backup.zip
```

### 1.5 `volo import-legacy`（历史旧版备份流式清洗导入）

```bash
# 预览旧版 15 表 ZIP/JSON 归档清洗报告（零写入）
./volo import-legacy ./legacy-solo-backup.zip --dry-run

# 正式导入并自动推导分类、还原专题/书架、保留 .html 历史固定链接
./volo import-legacy ./legacy-solo-backup.zip
```

### 1.6 `volo doctor` 与 `volo healthcheck`（自检诊断与容器探活）

```bash
# 人类可读诊断报告（检查密钥环、数据目录权限、SQLite WAL 模式、连接池、磁盘可写性）
./volo doctor

# 输出机器可读 JSON（适合监控采集）
./volo doctor --json

# 探测运行中实例的 /healthz（自动适配 TCP 或 Unix Domain Socket，专为 Distroless 容器设计）
./volo healthcheck
```

---

## 2. 细粒度 API Token（`volo_pat_*`）与外部工具直连

在管理台 **「系统与安全 -> 账户与安全 -> API 令牌」** 中可签发带过期时间与最小权限 Scope 的个人访问令牌。

- 数据库仅存储令牌的 `SHA-256` 摘要（`token_hash`）与前 12 位展示前缀（`token_prefix`），明文仅在创建时显示一次；
- 使用 `Authorization: Bearer volo_pat_<secret>` 请求 `/api/admin/*` 接口时自动走无状态鉴权，无需携带浏览器 Session Cookie 或 `X-CSRF-Token`。

### 2.1 权限范围（Scopes）对照表

| Scope 标识 | 允许访问的操作范围 | 典型集成场景 |
| :--- | :--- | :--- |
| `articles:read` | 读取个人信息（`/api/admin/me`）、概览指标、文章/独立页面/媒体列表 | 只读统计面板、文章本地备份脚本 |
| `articles:write` | 创建、更新、删除文章，管理分类、标签、专题系列与电子书架 | Obsidian 发布插件、CI 自动同步 Markdown 仓库 |
| `pages:write` | 创建、更新、删除独立 Markdown 页面（如 `/about`、`/now`） | 外部自动化更新个人状态页 |
| `media:write` | 上传媒体资源（自动剥离 GPS EXIF、摆正方向并自适应编码 WebP） | PicGo / ShareX / uPic 图床直传 |
| `backup:read` | 流式下载全站备份包（`GET /api/admin/backups/export`） | 异地定时拉取加密冷备脚本 |

> **安全边界**：系统设置修改、用户权限变更、WAF 封禁与插件安装等高危管理操作**强制要求交互式管理员会话（含 2FA 策略校验）**，任何 API Token 均无法越权调用。

### 2.2 `curl` / Obsidian 发布文章示例

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
    "summary_markdown": "探讨单二进制静态编译、SQLite WAL 读写隔离与版本感知缓存。",
    "content_markdown": "## 核心目标\n\n在 35 MiB 内存预算内跑满高并发读吞吐。",
    "status": "published",
    "allow_comment": true,
    "pinned": false
  }'
```

### 2.3 PicGo / CLI 图床上传示例（`media:write`）

```bash
curl -X POST "${VOLO_URL}/api/admin/media/upload" \
  -H "Authorization: Bearer ${VOLO_TOKEN}" \
  -F "file=@./screenshot.png"
```

成功响应示例（自动脱敏 EXIF 并返回宽高与公开访问地址）：

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

## 3. 前台公开接口与机器可读订阅端点

| 路径 | 方法 | 说明 |
| :--- | :--- | :--- |
| `/healthz` | `GET` | 存活与数据库只读探针（返回 `{"status":"ok","db":"ok"}`） |
| `/rss.xml` / `/atom.xml` | `GET` | 标准 RSS 2.0 与 Atom 1.0 全文订阅源（支持强 `ETag` / `304`，自动脱敏加密内容） |
| `/podcast.xml` | `GET` | Apple Podcasts / Spotify 兼容 iTunes 播客订阅源 |
| `/sitemap.xml` / `/robots.txt` | `GET` | 搜索引擎站点地图（含文章与已发布独立页面）与爬虫协议 |
| `/llms.txt` / `/llms-full.txt` | `GET` | AI 大语言模型结构化索引与公开语料汇总（可于 SEO 设置中一键关闭） |
| `/posts/{slug}.md` | `GET` | 单篇公开文章的纯净原始 Markdown 视图（密码锁文章未授权返回 `403`） |
| `/api/v1/search?q=<keyword>` | `GET` | 二阶段毫秒级全文检索（返回高亮摘要片段，零泄露锁定内容） |
| `/api/v1/pow/challenge` | `GET` | 获取无第三方依赖的 `HMAC + SHA-256` 工作量证明挑战令牌 |
| `/api/v1/guest/passport` | `POST` | 签发或自愈刷新双轨持久游客签名通行证（`__volo_gp`） |
| `/api/v1/comments` | `GET` / `POST` | 查询文章/页面评论树或提交经 PoW 校验的新评论 |
| `/api/v1/reactions` | `POST` | 文章与评论轻量表情互动计数 |
