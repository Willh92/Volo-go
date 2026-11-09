# 架构设计与性能内幕

> English version: [Architecture & Performance Internals](../en-US/architecture.md)

`Volo-go` 是从零构建的现代纯 Go（`CGO_ENABLED=0`，Go `1.27.3`）独立主权博客引擎，核心设计目标是在 **`< 20 MiB` 静态单二进制**（`linux/amd64` 及全部 `arm64` 目标；全 6 大发布目标平台 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64` 均严格 `< 21 MiB`）与 **`< 35 MiB` 常驻内存 (`VmRSS`)** 的严格边界内，提供三方言存储支持、10 大出口零死角隐私防护、纯 Go ES6 插件沙箱与微秒级高并发吞吐。

---

## 1. 整体分层拓扑

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    HTTP / Unix Domain Socket 接入层                     │
│  WAFEngine (CIDR 封禁 / 扫描器诱捕 / 子网限流 / 多库共存离线 GeoIP)     │
│  Dynamic Stealth Admin Gate (免重启热重载隐身后台前缀 + 默认路径伪装404)│
│  RedirectGuardian (O(1) 内存重定向快照：自定义规则 + 历史 .html 301)     │
│  BREACH-Safe Dynamic Gzip (仅压缩匿名公开响应，自动旁路敏感凭据流)      │
└───────────────────────────────┬─────────────────────────────────────────┘
                                │
        ┌───────────────────────┼───────────────────────┐
        ▼                       ▼                       ▼
┌───────────────┐     ┌───────────────────┐     ┌──────────────────┐
│ 前台主题 SSR  │     │ Studio 管理台 SPA │     │  机器与 AI 分发  │
│ volo-studio & │     │ Preact + Signals  │     │ RSS/Atom/Podcast │
│ ./themes/*    │     │ IR/WYSIWYG/SV CM6 │     │ 分片Sitemap/llms │
└───────┬───────┘     └─────────┬─────────┘     └────────┬─────────┘
        └───────────────────────┼────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                     领域服务与安全收口核心层                             │
│  • ContentVisibilityGuard (10 大出口统一脱敏 + 4 MiB 渲染摘要 LRU)      │
│  • SearchIndex (8 MiB 常驻二阶段分页检索 + UTF-8 零全量切片窗口摘要)    │
│  • Auth & RBAC (Argon2id / Passkey / TOTP / 会话管理 / API Token)       │
│  • GuestPassport & PoW (三端自愈签名通行证 + env_fp + SHA-256 PoW)      │
│  • Media Pipeline (<=64MP 防炸弹 / EXIF 摆正与剥离 / WebP / SigV4 S3)   │
│  • Plugin Manager (自研纯 Go ES6 池化沙箱 + 指令扩展 + 联邦分发/熔断)   │
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

## 2. 正规化数据模型（`v6` Schema：33 张业务数据表 + 9 大设置域）

`Volo-go` 彻底摒弃了传统博客“利用键值表分片硬塞业务大 JSON”与“文章必须经由标签间接绑定分类”的反范式包袱，建立清晰的正规化关系模型（位于 `internal/db/migrate/schema.go`，当前 Schema 版本 **`v6`**，由 `schema_migrations` 跟踪 `v1`–`v6` 幂等演进，`CanonicalTables` 共包含 **33 张业务数据表**）：

| 领域 | 数据表（共 33 张业务表 + `schema_migrations`） | 设计要点 |
| :--- | :--- | :--- |
| **系统设置（1 张）** | `settings` | 承载全部 **9 大热重载设置域**（`site`、`reading`、`feed`、`seo`、`storage`、`appearance`、`notifications`、`security`、`backup`），支持乐观锁局部 `PATCH`，敏感凭据独立密封于 `secret_ciphertext`（`AES-256-GCM` + 列级 AAD） |
| **账户与鉴权（4 张）** | `users`, `user_credentials`, `user_sessions`, `api_tokens` | 物理隔离 `admin` / `editor` / `reader` 三级角色；多设备命名 WebAuthn Passkey、TOTP 与恢复码独立存于 `user_credentials`；支持细粒度 Scope 的 `volo_pat_*` 令牌 |
| **内容与结构（8 张）** | `categories`, `tags`, `articles`, `article_tags`, `article_revisions`, `article_backlinks`, `pages`, `nav_items` | `articles.category_id` 直接外键关联分类；`pages`（支持自定义 `template` 的独立 Markdown 页面）与 `nav_items`（多级双语导航树）彻底解耦，绝不污染文章归档流 |
| **专题与书架（4 张）** | `series`, `series_articles`, `books`, `book_chapters` | 原生一等公民关系表，支持有序专题系列、阅读状态（`reading_status`）、批量拖拽排序及多源电子书目录章节树（`book_chapters`）缓存 |
| **互动与订阅（6 张）** | `comments`, `reactions`, `links`, `follows`, `follow_items`, `redirects` | 评论绑定 `guest_id_hash` 与 `env_fp_hash` 支持后续注册一键认领；`reactions` 承载表情互动；`follows` / `follow_items` 聚合订阅动态；`redirects` 与 `legacy_permalink` 守护历史链接零死链 |
| **媒体、钩子、风控、统计与插件（10 张）** | `media_assets`, `webhooks`, `webhook_deliveries`, `analytics_daily`, `analytics_top_items`, `security_ip_bans`, `security_geo_rules`, `audit_logs`, `plugins`, `plugin_kv` | `media_assets` 记录宽高、`alt_text` 与 `exif_stripped`；`webhook_deliveries` 记录有界投递审计与重放载荷；`security_ip_bans` / `security_geo_rules` 驱动 WAF 与离线多库 GeoIP；`plugins` / `plugin_kv` 隔离插件机密配置与带 TTL 的键值存储 |

### 9 大热重载设置域一览（`internal/domain/settings.go`）

| 设置域 (`domain`) | 核心职责与代表配置项 |
| :--- | :--- |
| `site` | 站点标题/副标题、默认语言、时区、公告栏、社交链接、版权模板、自定义全局变量 `custom_vars` |
| `reading` | 文章列表样式 `list_style`（`summary \| title_only \| full`）、分页条数、PJAX、即时搜索、评论准入与智能审核、动态 PoW 难度、静态/渲染库 CDN 前缀 |
| `feed` | RSS/Atom 输出模式（摘要/全文）、`/llms.txt` 开关、`.md` 原始视图开关、Apple/Spotify 播客（`podcast`）完整频道元数据 |
| `seo` | 默认关键词/描述、默认 OG 图、文章固定链接风格、IndexNow 密钥、GitHub 开源仓库展示缓存 |
| `storage` | 活跃存储驱动（`local \| s3`）、本地上传目录 `local_dir`、最大上传体积、EXIF 剥离、自适应 WebP 质量、S3/R2/MinIO 连接与加密 `s3_secret_key` |
| `appearance` | 桌面端主题 `active_theme`、移动端独立主题 `mobile_theme`、主题色 `accent`、明暗模式 `default_mode`、全站 `custom_css`、按主题隔离的 `theme_configs` |
| `notifications` | SMTP 邮件服务与加密 `smtp_password`、新评论站长通知与读者回复邮件提醒 |
| `security` | 扫描风控与 WAF 窗口阈值、离线 GeoIP 开关与热重载目录 `geoip_dir`、`geo_default_action` / `geo_unknown_action`、强制 TOTP 范围、免重启隐身后台路径 `dynamic_login_path`（`admin_path`）及加密 `share_token_secret` |
| `backup` | 本地定时自动备份周期/保留份数/是否含媒体、远程异地备份（`s3 \| webdav`）及加密 `webdav_password` / `s3_secret_key` |

---

## 3. 多数据库方言与 SQLite WAL 读写隔离

1. **纯 Go 三方言抽象 (`internal/db/dialect`)**：
   - 统一重绑定 `?` 占位符至 PostgreSQL `$1..$N`（内置有界 `sync.Map` 缓存，热路径 **`0 allocs/op`，`21.8 ns/op`**），并支持 `--db-simple-protocol`（`VOLO_DB_SIMPLE_PROTOCOL`）无缝对接 PgBouncer / Supabase 事务连接池；
   - 统一封装跨库幂等 `UpsertSQL`（`ON CONFLICT DO UPDATE` vs `ON DUPLICATE KEY UPDATE`）、`PaginateSQL` 与物理列类型映射（`TypeMap`，`v6` 将长索引键统一拓宽至 `VARCHAR(512)`）。
2. **SQLite 读写连接池物理隔离 (`internal/db/sqlx`)**：
   - `WriteDB` 固定 `MaxOpenConns=1` + `_txlock=immediate` + `busy_timeout=5000`，彻底消除并发写锁死锁；
   - `ReadDB` 独立连接池强制注入 `PRAGMA query_only = ON` 与 `cache_size = -4096`（有界 `4 MiB` 页缓存），从数据库驱动层物理杜绝任何只读请求产生写库副作用。
3. **零闭包反射扫描**：结构体列计划按 `(reflect.Type, Columns)` 预编译缓存，扫描行时直接复用字段偏移数组，消除逐列闭包分配。

---

## 4. 10 大出口零死角隐私防线 (`ContentVisibilityGuard`)

密码保护文章（`password_ciphertext`）与局部加密块（`:::lock`）在以下 **10 个数据出口**由 `internal/visibility/guard.go` 统一执行默认关闭（Fail-Closed）脱敏：

1. **列表卡片摘要与全文时间流**：锁定文章严禁截取正文摘要（即使 `reading.list_style = "full"` 亦严格脱敏）；
2. **首图与 OpenGraph 图**：锁定文章严禁暴露正文首图或封面 URL，统一回退本地确定性几何封面 SVG；
3. **上下相邻文章**：仅暴露最小元数据（标题与链接），绝不预取正文；
4. **未解锁评论接口**：未解锁状态请求 `/api/v1/comments` 直接返回 `403 Forbidden`，最新评论侧栏物理过滤加密文章评论；
5. **二阶段分页全文检索**：8 MiB 内存索引与超限 SQL 回退检索均在入库/查询前剥离密码文章与 `:::lock` 块；
6. **RSS / Atom / Podcast 订阅源**：整篇加密文章物理排除（避免阅读器缓存泄密），局部锁定块自动替换为多语言提示框；
7. **Sitemap（含自动分片）与 IndexNow**：密码文章绝不写入 `/sitemap.xml` 或任何 `/sitemap-N.xml` 分片，绝不触发 IndexNow 主动推送，且详情页自动注入 `noindex, nofollow, noarchive` 与 `Cache-Control: private, no-store`；
8. **双向链接 (`[[slug]]`)**：加密文章不建立出站引用记录，也不在目标文章底部反向链接中展示；
9. **`/llms.txt` 与 `.md` 原始端点**：语料索引完全排除密码文章，未携有效解锁凭证请求 `.md` 返回 `403`；
10. **插件钩子与联邦分发隔离**：未声明高敏权限的插件与自动 `federation` 分发永远收不到加密文章载荷或嵌套 `:::lock` 块，且 `render.post_html` 与自定义指令输出二次经过 `bluemonday` 严格清洗防 XSS。

---

## 5. 零脏读版本感知缓存、编辑器与内存预算控制

为了在不引入 Redis 等外部组件的条件下兼顾极致吞吐与 `< 35 MiB` 内存红线，`Volo-go` 采用 **原子数据版本号 (`Store.DataRevision()`) + 设置版本号 (`SettingsManager.Revision()`)** 驱动的多级自校验有界缓存：

- **任何业务写入 1 纳秒全局失效**：所有 `ExecContext` 与 `WithTx` 提交都会原子递增 `dataRev`；重定向哈希表、导航/分类/标签/最新评论快照（`publicChromeSnapshot`）以及预压缩 RSS/Atom/Podcast/Sitemap（含 `> 10,000` URL 自动分片索引）/LLMs 文档（`CachedDocument`，上限 `2 MiB`）在读取时对比 `(dataRev, settingsRev)`，一旦发生任何增删改立即重建并刷新强 `ETag`，实现 **零脏读、零漏失效**。
- **异步写合并计数缓冲 (`viewAnalyticsBuffer`)**：前台高频阅读计数（`articles.view_count`、`pages.view_count`、`redirects.hit_count`、`analytics_daily`）在内存中无锁合并，读取时实时叠加 `PendingArticleViews`，后台每 `2s` 经由不触发内容缓存失效的 `WithBgTx` 批量落盘，并在打开管理台概览及 `SIGTERM` 优雅停机时确定性刷盘（零丢数）。
- **按需懒加载三模式编辑器**：管理台首屏仅加载 `84.82 KB`（Gzip）轻量 Preact + Signals 外壳（全部内嵌管理台资源总计 `< 300 KB` Gzip）；认证后工作区与 CodeMirror 6 三模式核心（`IR` 即时渲染、`WYSIWYG` 富文本往返、`SV` 分屏预览）均按需异步加载，且前台页面的 KaTeX 公式与 Mermaid 图表渲染库亦仅在正文实际包含对应语法块时按需懒加载。
- **常驻内存分项上限（实测温热 `VmRSS = 25.17 MiB`）**：
  - Go 运行时、HTTP 栈、内嵌静态资源与自研纯 Go ES6 插件 VM 池：`~12 MiB`
  - SQLite WAL 页缓存（`cache_size = -4096`）：`<= 4 MiB`
  - 二阶段全文检索内存索引（`SearchIndex`）：`<= 8 MiB`
  - Markdown 卡片与公开正文渲染 LRU（`boundedRenderLRU`）：`<= 4 MiB`
  - 预压缩订阅源缓存（`<= 2 MiB`）与池化 Gzip / 图像缓冲：严格有界回收。
