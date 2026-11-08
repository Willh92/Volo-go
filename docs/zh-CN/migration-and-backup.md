# 历史迁移与跨库流式备份指南

> English version: [Legacy Migration & Cross-DB Streaming Backup](../en-US/migration-and-backup.md)

`Volo-go` 提供两条零内存暴涨的流式数据通道：
1. **历史旧版归档流式清洗导入器（`internal/etl`）**：一键导入早期 `bolo-solo` 导出的 15 表 `.zip` 或 `JSON` 备份包，自动完成反范式结构正规化并 **100% 守护历史 `.html` 固定链接零 404**；
2. **原生跨数据库流式备份引擎（`internal/backup`）**：采用方言无关的 `volo-backup-v1`（`NDJSON` 行流 + `SHA-256` 清单 + 媒体文件流），支持 **SQLite ⇄ PostgreSQL ⇄ MySQL** 任意方向无损搬家。

---

## 1. 从旧版 `bolo-solo` 无损迁移

### 1.1 自动执行的 6 项结构清洗与正规化

旧版备份包中存在大量历史包袱（如 10KB 字符串分片表、标签间接推导分类、裸口令哈希与 `.html` 固定链接）。`Volo-go` 的流式解析器（`json.Decoder` 逐条消费，不将大包读入内存）在单次导入事务中自动完成以下转换：

| 旧版历史结构 | `Volo-go` 正规化落库目标 | 迁移行为说明 |
| :--- | :--- | :--- |
| 分片键值文档（专题、书架、每日统计、Webhook） | `series`、`series_articles`、`books`、`analytics_daily`、`webhooks` | 自动拼合还原多片 JSON 并拆入具备外键约束的正规关系表 |
| `category` + `category_tag` + `tag_article` 间接归类 | `articles.category_id` + `article_tags` | 自动通过文章关联标签反向推导主分类并直接绑定至文章，同时保留全部自由标签 |
| 扁平混杂 `option` 配置项 | 6 大强类型分域 `settings`（`site`、`appearance`、`reading`、`feed`、`seo`、`notifications`） | 自动映射站点标题、副标题、ICP 备案、社交链接与页脚信息；旧版废弃皮肤自动平滑回退至 `volo-studio` |
| 自定义导航栏与独立链接 | `nav_items` 与 `pages` | 完整保留顶部导航顺序、图标与新窗口属性 |
| 老文章路径（如 `/articles/2021/05/12/1620812.html`） | `articles.legacy_permalink` + `redirects` | 新系统生成干净 Slug 的同时将原路径写入 `legacy_permalink`；访问旧 `.html` URL 自动触发 `O(1)` 内存路由返回 **`301 Moved Permanently`** 跳转至规范地址，SEO 权重零流失 |
| 旧版 `PBKDF2` / 历史密码哈希 | `users.legacy_hash` -> `Argon2id` | 导入后老用户无需重置密码，首次成功登录时自动在后台静默升级为 `Argon2id` 并擦除 `legacy_hash` |

### 1.2 迁移操作步骤（CLI 或 Web 向导二选一）

#### 方式 A：通过 CLI 预检并导入（推荐大体积归档）

```bash
# 第一步：Dry-Run 预检，输出识别到的文章、分类、评论、专题、书架数量，不写数据库
./volo import-legacy /path/to/legacy-backup.zip --dry-run

# 第二步：正式执行流式清洗入库
./volo import-legacy /path/to/legacy-backup.zip
```

#### 方式 B：通过 Web 初始化向导直连旧数据库一键迁移（无需手工导出归档）

- **全新安装时**：浏览器首次进入 `/setup` 建站向导，切换至 **「从旧版数据一键迁移 -> 直连旧数据库迁移」**：
  1. 在 `1A` 配置新目标数据库（`SQLite` / `PostgreSQL` / `MySQL`），在 `1B` 填写旧源数据库连接串（支持 `Solo` / `Bolo` / `Volo-solo` 历史库以及既有 `Volo-go` 实例库）；
  2. 点击 **「检测旧库与数据量」**，后端以严格只读模式连接旧库，自动探测表前缀（如 `b3_solo_`、`solo_` 或原生表）并展示识别到的文章、评论与用户数；
  3. 可开启 **Dry-Run 预演校验** 先行检查清洗结果，确认无误后一键流式迁入新库并保留全部 `301` 历史固定链接，全程不修改旧库任何数据。

#### 方式 C：通过 Web 初始化向导或管理台上传归档导入

- **全新安装时**：在 `/setup` 向导选择 **「上传备份归档 (.zip / .json)」**，直接拖入旧版 `.zip` / `.json` 备份包一键建站；
- **运行中站点**：进入管理台 **「系统与安全 -> 备份与迁移 -> 历史旧版备份导入」** 上传归档并查看实时转换统计报告。

---

## 2. 原生跨数据库流式备份与搬家（`volo-backup-v1`）

### 2.1 归档包内部结构与防炸弹保护

每个原生备份 `.zip` 均由流式写入器生成并附带完整密码学完整性校验：

```text
volo-backup-20261107-010000.zip
├── manifest.json                 # 记录 format、schema_version、source_dialect 及每个流的字节数、行数与 SHA-256
├── tables/
│   ├── 01_users.ndjson           # 按外键拓扑顺序排列的数据库无关 NDJSON 行流
│   ├── 02_categories.ndjson
│   ├── ...
│   └── 23_audit_logs.ndjson
└── media/                        # 可选包含（--with-media=true）的本地上传图片与播客音频流
    └── 2026/11/...
```

恢复时的安全防线：
- **先验哈希校验**：正式写入数据库前，先流式验证所有条目的字节数与 `SHA-256` 摘要，任何损坏或篡改立即中止，确保目标库不被半截脏数据污染；
- **防 ZIP 炸弹配额**：单流上限 `512 MiB`、总解压上限 `2 GiB`、严格净化媒体相对路径拦截 `../` 路径穿越；
- **分批事务回滚**：每 `200` 行流式批量写入并在外键拓扑事务内提交，任意表失败自动整体回滚。

### 2.2 跨数据库引擎无损搬家实战（例如 SQLite -> PostgreSQL）

当站点规模增长需要从单机 SQLite 迁移至托管 PostgreSQL（如 Supabase / Neon / 自建 PG）或 MySQL 8+ / TiDB 时，只需 3 步：

```bash
# 1. 在原 SQLite 实例上导出完整流式备份包
./volo backup create --env-file /etc/volo/volo.env --output /tmp/site-full.zip

# 2. 修改目标环境的数据库连接配置（例如切换至 PostgreSQL）
#    VOLO_DB_DRIVER=postgres
#    VOLO_DB_DSN=postgres://volo:secret@db.internal:5432/volo?sslmode=verify-full
#    注意：务必保持两端的 VOLO_SECRET_KEY（或 data/.secret.key）一致，以确保已加密的 S3/SMTP/Webhook 密钥可正常解密！

# 3. 先 Dry-Run 验证归档完整性，再一键流式导入 PostgreSQL
./volo backup restore /tmp/site-full.zip --env-file /etc/volo/volo-pg.env --dry-run
./volo backup restore /tmp/site-full.zip --env-file /etc/volo/volo-pg.env
```

---

## 3. 自动定时备份与异地冷备（S3 / R2 / WebDAV）

在管理台 **「系统与安全 -> 备份与迁移」** 或系统存储配置中：
1. 开启定时滚动备份策略并设置本地保留份数（超出保留份数的旧归档将自动轮转清理，防止撑爆磁盘）；
2. 若已配置 **S3 兼容对象存储（Cloudflare R2 / AWS S3 / MinIO）** 或 **WebDAV 远端备份目录**，每次生成的 `.zip` 快照可自动异步流式同步至异地存储，实现真正的 `3-2-1` 容灾保护。
