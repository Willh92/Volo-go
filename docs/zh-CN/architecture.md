# 架构设计与性能内幕

> English version: [Architecture & Performance Internals](../en-US/architecture.md)

`Volo-go` 是从零构建的现代纯 Go（`CGO_ENABLED=0`）独立主权博客引擎，核心设计目标是在 **`< 20 MiB` 静态单二进制** 与 **`< 35 MiB` 常驻内存 (`VmRSS`)** 的严格边界内，提供多数据库方言支持、10 大出口零死角隐私防护、双模插件沙箱与毫秒级高并发吞吐。

---

## 1. 整体分层拓扑

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    HTTP / Unix Domain Socket 接入层                     │
│  WAFEngine (CIDR 封禁 / 扫描器诱捕 / 子网限流 / 离线 GeoIP / 隐身 404)  │
│  RedirectGuardian (O(1) 内存重定向快照：自定义规则 + 历史 .html 301)     │
│  BREACH-Safe Dynamic Gzip (仅压缩匿名公开响应，自动旁路敏感凭据流)      │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        ▼                       ▼                       ▼
┌───────────────┐     ┌───────────────────┐     ┌──────────────────┐
│ 前台主题 SSR  │     │ Studio 管理台 SPA │     │  机器与 AI 分发  │
│ volo-studio & │     │ Preact + Signals  │     │ RSS/Atom/Podcast │
│ ./themes/*    │     │ 按需懒加载 CM6    │     │ Sitemap / llms   │
└───────┬───────┘     └─────────┬─────────┘     └────────┬─────────┘
        └───────────────────────┼────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                     领域服务与安全收口核心层                             │
│  • ContentVisibilityGuard (10 大出口统一脱敏 + 4 MiB 渲染摘要 LRU)      │
│  • SearchIndex (8 MiB 常驻二阶段检索 + UTF-8 零全量切片窗口摘要)        │
│  • Auth & RBAC (Argon2id / Passkey / TOTP / 会话管理 / API Token)       │
│  • GuestPassport & PoW (三端自愈签名通行证 + env_fp + SHA-256 PoW)      │
│  • Media Pipeline (<=64MP 防炸弹 / EXIF 摆正与剥离 / WebP / SigV4 S3)   │
│  • Plugin Manager (goja 池化 VM 沙箱 + HMAC 外部 Webhook + 熔断器)      │
└───────────────────────────────┬─────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                  多方言 SQL 与版本感知存储引擎 (`sqlx`)                  │
│  • Dialect: SQLite (modernc WAL 读写分离) | PostgreSQL (pgx/v5) | MySQL │
│  • 零闭包列映射计划 + Postgres Rebind 缓存 + 原子 DataRevision 失效总线  │
│  • Write-Behind View Buffer (内存增量实时叠加 + 2s/停机确定性批量刷盘)   │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 正规化数据模型（23 张核心关系表）

`Volo-go` 彻底摒弃了传统博客“利用键值表分片硬塞业务大 JSON”与“文章必须经由标签间接绑定分类”的反范式包袱，建立清晰的正规化关系表（位于 `internal/db/migrate/schema.go`，当前 Schema 版本 `v2`）：

| 领域 | 数据表 | 设计要点 |
| :--- | :--- | :--- |
| **账户与鉴权** | `users`, `user_credentials`, `user_sessions`, `api_tokens`, `email_verifications` | 支持 `admin` / `editor` / `reader` 角色；多设备命名 Passkey 与 TOTP 独立建表；细粒度 Scope PAT |
| **内容与结构** | `categories`, `tags`, `articles`, `article_tags`, `article_revisions`, `article_backlinks`, `pages`, `nav_items` | `articles.category_id` 直接绑定分类；`pages`（独立 Markdown 页面）与 `nav_items`（多级导航树）彻底解耦，绝不污染文章归档时间流 |
| **专题与书架** | `series`, `series_articles`, `books` | 原生一等公民关系表，支持有序专题系列与多源电子书目录树缓存 |
| **互动与链接** | `comments`, `links`, `follows`, `follow_items`, `redirects` | 评论绑定 `guest_id_hash` 与 `env_fp_hash` 支持后续注册一键认领；`redirects` 与 `legacy_permalink` 守护历史链接零死链 |
| **系统与扩展** | `media_assets`, `settings`, `webhooks`, `analytics_daily`, `analytics_top_items`, `plugin_kv`, `security_ip_bans`, `audit_logs` | 6 大设置域乐观锁局部 `PATCH` + 敏感字段列级 `AES-256-GCM` 密文隔离；零 PII 每日 PV/UV 聚合 |

---

## 3. 多数据库方言与 SQLite WAL 读写隔离

1. **纯 Go 三方言抽象 (`internal/db/dialect`)**：
   - 统一重绑定 `?` 占位符至 PostgreSQL `$1..$N`（内置有界 `sync.Map` 缓存，热路径 **`0 allocs/op`，`21.8 ns/op`**）；
   - 统一封装跨库幂等 `UpsertSQL`（`ON CONFLICT DO UPDATE` vs `ON DUPLICATE KEY UPDATE`）、`PaginateSQL` 与物理列类型映射（`TypeMap`）。
2. **SQLite 读写连接池物理隔离 (`internal/db/sqlx`)**：
   - `WriteDB` 固定 `MaxOpenConns=1` + `_txlock=immediate` + `busy_timeout=5000`，彻底消除并发写锁死锁；
   - `ReadDB` 独立连接池强制注入 `PRAGMA query_only = ON` 与 `cache_size = -4096`（有界 `4 MiB` 页缓存），从数据库驱动层物理杜绝任何只读请求产生写库副作用。
3. **零闭包反射扫描**：结构体列计划按 `(reflect.Type, Columns)` 预编译缓存，扫描行时直接复用字段偏移数组，消除逐列闭包分配。

---

## 4. 10 大出口零死角隐私防线 (`ContentVisibilityGuard`)

密码保护文章（`password_ciphertext`）与局部加密块（`:::lock`）在以下 **10 个数据出口**由 `internal/visibility/guard.go` 统一执行默认关闭（Fail-Closed）脱敏：

1. **列表卡片摘要**：锁定文章严禁自动截取正文摘要；
2. **首图与 OpenGraph 图**：锁定文章严禁暴露正文首图或封面 URL，统一回退本地确定性几何封面 SVG；
3. **上下相邻文章**：仅暴露最小元数据（标题与链接），绝不预取正文；
4. **未解锁评论接口**：未解锁状态请求 `/api/v1/comments` 直接返回 `403 Forbidden`，最新评论侧栏物理过滤加密文章评论；
5. **二阶段全文检索**：8 MiB 内存索引与超限 SQL 回退检索均在入库/查询前剥离密码文章与 `:::lock` 块；
6. **RSS / Atom / Podcast 订阅源**：整篇加密文章物理排除（避免阅读器缓存泄密），局部锁定块自动替换为多语言提示框；
7. **Sitemap 与 IndexNow**：密码文章绝不写入 `sitemap.xml`，绝不触发搜索引擎主动推送，且详情页自动注入 `noindex, nofollow, noarchive` 与 `Cache-Control: private, no-store`；
8. **双向链接 (`[[slug]]`)**：加密文章不建立出站引用记录，也不在目标文章底部反向链接中展示；
9. **`/llms.txt` 与 `.md` 原始端点**：语料索引完全排除密码文章，未携有效解锁凭证请求 `.md` 返回 `403`；
10. **插件钩子隔离**：未声明高敏权限的插件永远收不到加密文章载荷，且 `render.post_html` 输出二次经过 `bluemonday` 严格清洗防 XSS。

---

## 5. 零脏读版本感知缓存与内存预算控制

为了在不引入 Redis 等外部组件的条件下兼顾极致吞吐与 `< 35 MiB` 内存红线，`Volo-go` 采用 **原子数据版本号 (`Store.DataRevision()`) + 设置版本号 (`SettingsManager.Revision()`)** 驱动的多级自校验有界缓存：

- **任何业务写入 1 纳秒全局失效**：所有 `ExecContext` 与 `WithTx` 提交都会原子递增 `dataRev`；重定向哈希表、导航/分类/标签/最新评论快照（`publicChromeSnapshot`）以及预压缩 RSS/Atom/Podcast/Sitemap/LLMs 文档（`CachedDocument`，上限 `2 MiB`）在读取时对比 `(dataRev, settingsRev)`，一旦发生任何增删改立即重建并刷新强 `ETag`，实现 **零脏读、零漏失效**。
- **异步写合并计数缓冲 (`viewAnalyticsBuffer`)**：前台高频阅读计数（`articles.view_count`、`pages.view_count`、`redirects.hit_count`、`analytics_daily`）在内存中无锁合并，读取时实时叠加 `PendingArticleViews`（读者刷新立即可见阅读量增长），后台每 `2s` 经由不触发内容缓存失效的 `WithBgTx` 批量落盘，并在打开管理台概览及 `SIGTERM` 优雅停机时执行确定性刷盘（零丢数）。
- **常驻内存分项上限（实测温热 `VmRSS = 28.60 MiB`）**：
  - Go 运行时、HTTP 栈、内嵌静态资源与 `goja` 池：`~12 MiB`
  - SQLite WAL 页缓存（`cache_size = -4096`）：`<= 4 MiB`
  - 二阶段全文检索内存索引（`SearchIndex`）：`<= 8 MiB`
  - Markdown 卡片与公开正文渲染 LRU（`boundedRenderLRU`）：`<= 4 MiB`
  - 预压缩订阅源缓存（`<= 2 MiB`）与池化 Gzip / 图像缓冲：严格有界回收。
