# 架构设计与性能内幕

> English version: [Architecture & Performance Internals](../en-US/architecture.md)

`Volo-go` 是从零构建的现代纯 Go（`CGO_ENABLED=0`，Go `1.27.3`，`GOEXPERIMENT=nojsonv2`）独立主权博客引擎，核心设计目标是在**全部 6 大发布目标平台均严格 `< 21 MiB` 的静态单二进制**（`linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`）与 **`< 35 MiB` 常驻内存 (`VmRSS`)** 的严格边界内，提供三方言存储支持、实时无损图片变换、10 大出口零死角隐私防护、纯 Go ES6 插件沙箱与微秒级高并发吞吐。

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
│  • Auth & RBAC (Argon2id / Passkey / TOTP / 封禁禁言与可逆屏蔽 / PAT)   │
│  • GuestPassport & PoW (三端自愈签名通行证 + env_fp + SHA-256 PoW)      │
│  • Media & ImageView (<=64MP / EXIF 剥离 / ?imageView2 实时变换 LRU)    │
│  • Plugin Manager (纯 Go ES6 冻结原型沙箱 + 优先级链式拦截 + 写合并 KV) │
└───────────────────────────────┬─────────────────────────────────────────┘
                                ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                  多方言 SQL 与版本感知存储引擎 (`sqlx`)                  │
│  • Dialect: SQLite (modernc WAL 读写分离) | PostgreSQL (pgwire) | MySQL │
│  • 零闭包列映射计划 + Postgres Rebind 缓存 + 原子 DataRevision 失效总线  │
│  • Write-Behind View & KV Buffers (内存实时叠加 + 定时/停机确定性刷盘)   │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 正规化数据模型（`v8` Schema：33 张业务数据表 + 9 大设置域）

`Volo-go` 彻底摒弃了传统博客“利用键值表分片硬塞业务大 JSON”与“文章必须经由标签间接绑定分类”的反范式包袱，建立清晰的正规化关系模型（位于 `internal/db/migrate/schema.go`，当前 Schema 版本 **`v8`**，由 `schema_migrations` 跟踪 `v1`–`v8` 幂等演进，`CanonicalTables` 共包含 **33 张业务数据表**）：

| 领域 | 数据表（共 33 张业务表 + `schema_migrations`） | 设计要点 |
| :--- | :--- | :--- |
| **系统设置（1 张）** | `settings` | 承载全部 **9 大热重载设置域**（`site`、`reading`、`feed`、`seo`、`storage`、`appearance`、`notifications`、`security`、`backup`），支持乐观锁局部 `PATCH`，敏感凭据独立密封于 `secret_ciphertext`（`AES-256-GCM` + 列级 AAD） |
| **账户与鉴权（4 张）** | `users`, `user_credentials`, `user_sessions`, `api_tokens` | 物理隔离 `admin` / `editor` / `reader` 三级角色；`v8` 内置永久/限时封禁（`ban_until`）、永久/限时禁言（`muted_until`）、封禁原因与可逆历史评论屏蔽（`shield_comments` / `shield_text`）；Passkey、TOTP 与恢复码存于 `user_credentials`；支持细粒度 `volo_pat_*` 令牌 |
| **内容与结构（8 张）** | `categories`, `tags`, `articles`, `article_tags`, `article_revisions`, `article_backlinks`, `pages`, `nav_items` | `articles.category_id` 直接外键关联分类；`pages`（支持自定义 `template` 的独立 Markdown 页面）与 `nav_items`（多级双语导航树）彻底解耦，绝不污染文章归档流 |
| **专题与书架（4 张）** | `series`, `series_articles`, `books`, `book_chapters` | 原生一等公民关系表，支持有序专题系列、阅读状态（`reading_status`）、批量拖拽排序及多源电子书目录章节树（`book_chapters`）缓存 |
| **互动与订阅（6 张）** | `comments`, `reactions`, `links`, `follows`, `follow_items`, `redirects` | 评论绑定 `guest_id_hash` 与 `env_fp_hash` 支持后续注册一键认领，按身份约束字数/行数并内置刷屏高度防御；`reactions` 承载表情互动；`follows` / `follow_items` 聚合订阅动态；`redirects` 与 `legacy_permalink` 守护历史链接零死链 |
| **媒体、钩子、风控、统计与插件（10 张）** | `media_assets`, `webhooks`, `webhook_deliveries`, `analytics_daily`, `analytics_top_items`, `security_ip_bans`, `security_geo_rules`, `audit_logs`, `plugins`, `plugin_kv` | `media_assets` 记录宽高、`alt_text` 与 `exif_stripped`（配合孤儿扫描忽略 `?imageView2` 查询串防误删）；`webhook_deliveries` 记录有界投递审计；`security_ip_bans` / `security_geo_rules` 驱动 WAF 与多库 GeoIP；`plugins` / `plugin_kv` 隔离插件机密与键值存储 |

### 9 大热重载设置域一览（`internal/domain/settings.go`）

| 设置域 (`domain`) | 核心职责与代表配置项 |
| :--- | :--- |
| `site` | 站点标题/副标题、默认语言、时区、公告栏、社交链接、版权模板、自定义全局变量 `custom_vars` |
| `reading` | 列表样式 `list_style`（`summary \| title_only \| full`）、分页条数、PJAX、即时搜索、评论准入与智能审核、按身份动态评论字数/最大行数（`comment_min_length`、`comment_max_length_{guest,user,staff}`、`comment_max_lines_{guest,user,staff}`）、动态 PoW 难度、CDN 前缀 |
| `feed` | RSS/Atom 输出模式（摘要/全文）、条目上限 `item_count`（同步覆盖 RSS/Atom/Podcast）、`/llms.txt` 开关、`.md` 原始视图开关、播客（`podcast`）频道元数据 |
| `seo` | 默认关键词/描述、默认 OG 图、4 种文章固定链接模板 `post_permalink_style`（`/posts/{slug}`、`/archives/{slug}`、`/p/{slug}`、`/{year}/{month}/{slug}`，修改即时刷新缓存与检索索引）、IndexNow 密钥、GitHub 展示缓存 |
| `storage` | 活跃存储驱动（`local \| s3`）、本地上传目录 `local_dir`、最大上传体积 `max_upload_mb`、EXIF 剥离、自适应 WebP 质量、S3/R2/MinIO 连接与加密 `s3_secret_key` |
| `appearance` | 桌面端主题 `active_theme`、移动端独立主题 `mobile_theme`、主题色 `accent`、明暗模式 `default_mode`、全站 `custom_css`、按主题隔离的 `theme_configs` |
| `notifications` | 纯 Go SMTP 邮件服务与加密 `smtp_password`、新评论站长通知与读者回复邮件提醒（含待审回复审核通过补发 `NotifyCommentReplyApproved`） |
| `security` | 扫描风控与 WAF 窗口实时联动、离线 GeoIP 热重载目录 `geoip_dir`、强制 TOTP 范围、`totp_remember_days`（支持 `0` 禁用记住设备）与 `totp_window_steps`、免重启隐身后台路径 `dynamic_login_path`（`admin_path`）及加密 `share_token_secret` |
| `backup` | 本地定时自动备份周期/保留份数/是否含媒体、远程异地备份（`s3 \| webdav`）及加密 `webdav_password` / `s3_secret_key` |

---

## 3. 多数据库方言与 SQLite WAL 读写隔离

1. **纯 Go 三方言抽象 (`internal/db/dialect`)**：
   - 统一重绑定 `?` 占位符至 PostgreSQL `$1..$N`（内置有界 `sync.Map` 缓存，热路径 **`0 allocs/op`，`21.8 ns/op`**），并支持 `--db-simple-protocol`（`VOLO_DB_SIMPLE_PROTOCOL`）无缝对接 PgBouncer / Supabase 事务连接池；
   - 统一封装跨库幂等 `UpsertSQL`（`ON CONFLICT DO UPDATE` vs `ON DUPLICATE KEY UPDATE`）、`PaginateSQL` 与物理列类型映射（`TypeMap`）。
2. **SQLite 读写连接池物理隔离 (`internal/db/sqlx`)**：
   - `WriteDB` 固定 `MaxOpenConns=1` + `_txlock=immediate` + `busy_timeout=5000`，彻底消除并发写锁死锁；
   - `ReadDB` 独立连接池强制注入 `PRAGMA query_only = ON` 与 `cache_size = -4096`（有界 `4 MiB` 页缓存），从数据库驱动层物理杜绝任何只读请求产生写库副作用。
3. **非泛型零闭包结构体扫描 (`GetInto` / `SelectInto`)**：结构体列计划按 `(reflect.Type, Columns)` 预编译缓存，消除逐列闭包分配与泛型单态化代码膨胀。

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
10. **插件钩子与联邦分发隔离**：未声明高敏权限的插件与自动 `federation` 分发永远收不到加密文章载荷或嵌套 `:::lock` 块，且 `render.post_html` 与自定义指令输出二次经过单遍 HTML5 AST 安全清洗器严格清洗防 XSS。

---

## 5. 零脏读版本感知缓存、实时无损图片变换与内存预算控制

为了在不引入 Redis 等外部组件的条件下兼顾极致吞吐与 `< 35 MiB` 内存红线，`Volo-go` 采用 **原子数据版本号 (`Store.DataRevision()`) + 设置版本号 (`SettingsManager.Revision()`)** 驱动的多级自校验有界缓存：

- **任何业务写入 1 纳秒全局失效**：所有 `ExecContext` 与 `WithTx` 提交都会原子递增 `dataRev`；重定向哈希表、导航/分类/标签/最新评论快照（`publicChromeSnapshot`）以及预压缩 RSS/Atom/Podcast/Sitemap/LLMs 文档（`CachedDocument`，上限 `2 MiB`）在读取时对比 `(dataRev, settingsRev)`，一旦发生任何增删改立即重建并刷新强 `ETag`。
- **实时无损图片变换管线 (`ImageViewService`)**：`/uploads/...` 无参数时直接零拷贝直出原图；携带七牛云 `?imageView2/...` 或 KV 查询参数时，通过 `singleflight` 合并同参并发解码，并将变换结果存入严格上限 `8 MiB`、绑定原图 `(mtime, size)` 自动失效的内存 LRU 缓存与强 `ETag` / `304` 协商，全程绝不修改磁盘/S3 原图。
- **异步写合并计数缓冲 (`viewAnalyticsBuffer` & `kvBufferHub`)**：前台阅读计数与高频插件 KV 自增（`volo.kv.incrAsync`）均在内存中合并并在后台周期性及停机时确定性批量落盘，彻底消除 SQLite 热点写锁争用。
- **常驻内存分项上限（实测温热 `VmRSS = 24.23 MiB < 35.00 MiB`）**：
  - Go 运行时、HTTP 栈、零反射 `tinyxml`、内嵌资源与纯 Go ES6 插件 VM 池：`~11.5 MiB`
  - SQLite WAL 页缓存（`cache_size = -4096`）：`<= 4 MiB`
  - 二阶段全文检索内存索引（`SearchIndex`）：`<= 8 MiB`
  - Markdown 渲染 LRU（`<= 4 MiB`）、实时图片变换按需 LRU（上限 `8 MiB`）与预压缩订阅源缓存（`<= 2 MiB`）：严格有界回收。
