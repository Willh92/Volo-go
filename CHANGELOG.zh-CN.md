# 版本更新日志 (Changelog)

[简体中文](./CHANGELOG.zh-CN.md) | [English](./CHANGELOG.md)

本文件记录 `Volo-go` 的所有重要版本变更，格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [语义化版本规范](https://semver.org/lang/zh-CN/)。

---

## [1.0.1] - 2026-11-10

### 新增特性
- **原生 HTTPS 与 HTTP/2**：
  - `volo serve` 可直接终止 TLS：`VOLO_TLS_CERT_FILE` / `--tls-cert`、`VOLO_TLS_KEY_FILE` / `--tls-key`，`VOLO_TLS_MIN_VERSION` 可选 `1.2`（默认）或 `1.3`；通过 ALPN `h2` 提供 HTTP/2（兼容 HTTP/1.1）。
  - 证书文件变更后零停机热加载（加载失败时保留上一份有效证书）；可选 `VOLO_TLS_HTTP_REDIRECT_ADDR`（如 `:80`）开启 HTTP → HTTPS `308` 跳转监听。
  - `volo healthcheck` 自动走 HTTPS 探活；`volo doctor` 与后台诊断新增 `tls_certificate` 检查（文件不可读、证书私钥不匹配、已过期或 14 天内到期）。
  - 继续完整支持 Nginx / Caddy 经 Unix Socket 反向代理（多站点主机仍推荐）；所有构建默认保留 HTTP/2（移除 `nethttpomithttp2` 构建标签）。
- **评论回收站、恢复与自动清理**：
  - 读者自删、管理员单条 / 批量删除的评论统一进入 **回收站** 并完整保留数据，恢复后可无损还原；只有「彻底删除」（手动、清空回收站或到期自动清理）时才会连同点赞物理删除整行，仍有回复的评论仅保留一条已抹除正文、昵称、链接、IP、脱敏 IP、地区、设备指纹与通知邮箱的匿名占位行。
  - 管理员可恢复、永久删除、批量恢复 / 永久删除或一键清空回收站（`POST /api/admin/comments/{id}/restore`、`DELETE /api/admin/comments/{id}?purge=1`、批量 `restore` / `purge`、`POST /api/admin/comments/trash/empty`）。
  - 新增「评论回收站保留天数」设置（默认 `30`，`0` 为仅手动清理），后台任务每 6 小时自动清理过期评论。
  - 已删除但仍有可见回复的评论显示多语言「该评论已删除」占位，楼层结构与分页保持完整。
- **评论游客信息编辑**：后台评论编辑弹窗支持修改昵称、头像 URL 与个人网站（自动校验规范化），便于修复迁移遗留评论；子回复中的 `@昵称` 自动同步。
- **可配置三档阶梯自动风控、IP/CIDR 免封白名单与封禁全生命周期治理**：
  - 开放完整自动风控策略参数：统计窗口（`risk_window_seconds`，默认 `600` 秒）、触发阈值（`risk_strike_threshold`，默认 `5` 次）、三档阶梯封禁时长（初犯 `15` 分钟 / 再犯 `60` 分钟 / 累犯 `1440` 分钟，支持设为 `0` 永久封禁）及规范化 IP/CIDR 免封白名单（`risk_whitelist_cidrs`，豁免自动风控计数与全局高频限流），并结合数据库 `security_ip_bans.occurrences` 历史累计次数实现跨周期累犯加重。
  - 重构 IP 封禁生命周期：「解封」（`POST /api/admin/security/ip-bans/{id}/unban`、批量 `unban`）改为提前到期并完整保留历史记录；新增单条编辑与已过期一键重新封禁（`PATCH /api/admin/security/ip-bans/{id}`，支持 `15分钟 / 1小时 / 24小时 / 3天 / 7天 / 30天 / 永久 / 自定义分钟数`）、批量修改时长（`set_duration`）与彻底删除（`delete`）、一键清理已过期记录（`POST /api/admin/security/ip-bans/purge-expired`）、`生效中 / 已过期 / 全部` 数量徽标、来源标签（手动封禁 / 自动风控）与累计触发次数（`×N`），以及评论管理单条 / 批量「封禁 IP」同款时长选择（`expires_minutes`）。
- **过期封禁与审计 / 插件日志保留策略及一键清空**：
  - 停止每小时误删刚到期的封禁记录；新增已过期封禁保留天数（`ip_ban_retention_days`，默认 `30` 天）、管理审计日志保留天数（`audit_log_retention_days`，默认 `90` 天）与插件运行日志保留天数（`plugin_log_retention_days`，默认 `30` 天，均支持 `0` 永久保留），并提供审计日志与单插件日志一键清空接口及按钮（`DELETE /api/admin/audit-logs`、`DELETE /api/admin/plugins/{id}/logs`）。

### 变更
- **发布包瘦身**：发布压缩包仅包含对应系统的安装脚本，文件顺序 / 属主 / 时间戳可复现，并使用 7-Zip 最高压缩（缺失时回退 `gzip -9n` / `zip -9`）。为容纳 HTTP/2 与原生 TLS，二进制体积预算由 20 MiB 调整为 21 MiB（linux/amd64 约 19.7 MiB）；评论组件 gzip 预算由 10 KiB 调整为 11 KiB。
- **版本号统一**：二进制、Docker 镜像、`docker-compose.yml` 与后台前端包统一为 `v1.0.1`。

### 修复与体验优化
- **移动端与 iOS 编辑体验优化**：
  - iOS Safari 聚焦输入框、Markdown / 即时渲染 / 所见即所得编辑器及评论输入框时不再自动放大页面（触屏设备统一 16px 字号，不禁用双指缩放）。
  - 评论弹窗改为仅用 `visualViewport` + `translateY` 跟随软键盘与输入法候选栏，切换全屏 / 非全屏或点击工具栏、标签按钮时保持输入框焦点与光标位置；键盘弹起时自动收起游客信息栏，iOS 下弹窗打开期间锁定页面滚动。
  - 后台编辑器在手机端改为单行可横向滚动工具栏，编辑区高度随键盘自适应，键盘弹起时隐藏底部状态栏，切换全屏后自动恢复焦点。
- **WAF 404 静态媒体豁免与管理员会话防误封**：
  - 将迁移与静态媒体路径（`/uploads/*`、`/upload/*`、`/image/*`、`/images/*`、`/posts/image/*`、`/posts/images/*`、`/posts/upload/*`、`/head.jpg` 及图片/音视频/字体子资源请求）豁免于高频 404 目录扫描自动封禁计数，同时保留敏感路径诱捕与目录穿越攻击拦截。
  - 已登录的有效 `admin` / `editor` 管理会话自动豁免于 IP 段封禁与 GeoIP 区域拦截，杜绝站长误锁在后台之外。
- **Solo 历史图片路由兼容与上传目录读写自检**：
  - 全面支持旧版图片路径（`/image/*`、`/upload/*`、`/posts/image/*`、`/head.jpg` 及单层 `/{name}.jpg`、`/posts/{name}.jpg`）搭配七牛云 `?imageView2/...` 参数实时无损缩放裁剪。
  - 调整上传目录与 GeoIP 目录生效优先级为：`命令行启动参数 (--upload-dir / --geoip-dir) > 后台设置 (storage.local_dir / security.geoip_dir) > .env / 环境变量 > 内置默认目录`。
  - 修复自定义目录在 `systemd` 沙箱下无写权限时导致 `volo serve` 启动崩溃的问题（自动告警并安全回退至可写默认目录），并在后台「本地磁盘存储配置」新增 **「检测目录读写」** 按钮与实时诊断横幅。
- **后台设置主开关与受控子项分组重构**：
  - 全面排查并重构管理后台 7 处主开关与受控输入项混排面板（站点身份版权声明与页脚 HTML、阅读过期提醒与内置垃圾评论过滤、SMTP 邮件通知与服务器鉴权、安全风控总开关 / 高频防护 / GeoIP 策略、定时本地快照与异地加密备份、手动备份导出与恢复独立分栏、播客 RSS 订阅源设置），统一采用 `.volo-toggle-group` 子卡片将总开关固定在卡片头部、受控配置项收纳于卡片主体内。
- **文章编辑器导航状态重置**：
  - 修复在「文章管理」点击「编辑」后再点击左侧导航「发布文章」时仍把旧文章带入编辑器的问题（若存在未保存修改则先弹窗确认，确认后自动重置 `editingID` 并打开空白新文章）。
- **风控多语言补全与封禁列表地理位置列**：
  - 移除手动添加 IP 封禁与地区规则弹窗中的硬编码英文默认值（`"manual"`、`"Geo Rule"`），补全 `path_traversal`、`geo_block`、`pow_fail`、`csrf_fail` 等风控拦截原因在简中、繁中、英文、日文四套语言包中的翻译。
  - 为 `GET /api/admin/security/ip-bans` 接口与后台「IP / CIDR 封禁列表」新增 **「地理位置」** 列，基于已挂载离线 MMDB 库自动解析国家与省市归属地（回环与私网地址段显示「本地 / 内网」）。
- **移动端评论与回复抽屉统一及软键盘防遮挡**：
  - 将评论区「回复」与「编辑」统一复用主评论弹窗/底部抽屉组件，消除楼中楼内嵌编辑在全屏与窄屏下的显示异常。
  - 重构 `.volo-comment-composer` 为弹性纵向布局：中部输入区独立滚动，底部字数/行数统计与「取消 / 发表评论」按钮栏 `flex-shrink: 0` 永久固定；精简移动端单行工具栏与紧凑型游客信息栏，并同时监听 `visualViewport` 的 `resize` 与 `scroll` 事件动态适配手机输入法弹起高度（普通抽屉与全屏模式均不再遮挡底部操作栏）。
- **`/dynamic` 分页 500 修复与后台运行日志 500 根因记录**：
  - 修复动态页（`/dynamic`）评论数超过 1 页时 `dynamic.html` 访问 `.Pagination.Page` 引发的 HTTP 500 模板错误（修正为 `.Pagination.CurrentPage` 并补充 `PaginationView.Page()` 兼容方法）。
  - 在全局错误处理中间件 `wrapCustomErrorPages` 中将所有 HTTP `5xx` 响应详情（含请求方法、完整路径及底层 SQL / 模板报错文本）与恢复的 `panic` 堆栈自动写入内存环形日志，确保后台「系统与安全 → 运行日志（ERROR）」可直接排查所有 500 错误根因。
- **在线书架（`/books`）双端紧凑排版与徽章本地化**：
  - 调小书架网格与卡片尺寸：桌面端优化为每行 4 本紧凑排布，手机端（`<= 640px`）优化为一行 2 本双列排布并收敛封面高度、内边距与字号。
  - 将书籍卡片右上角原始 `auto` / `manual` / `series` 字段值本地化为多语言可读徽章（「在线文档 / 精选读物 / 专栏合集」）。
- **中英文独立文档拆分、GitHub Actions 自动发布摘要与 CodeQL 安全扫描清零**：
  - 将 `README.md` / `README.zh-CN.md` 与 `CHANGELOG.md` / `CHANGELOG.zh-CN.md` 拆分为独立的中英文文件并顶部互链。
  - 新增 `.github/scripts/generate-release-notes.py` 并接入 `.github/workflows/release.yml`，自动从版本更新日志（或未写日志时的近期非机器人提交记录）提炼简短更新摘要写入 GitHub Release 页面。
  - 全面排查并修复 GitHub Code Scanning (`CodeQL`) 在 Go 与 JS/TS 扫描套件中的告警项（消除编辑器正则回溯 `js/polynomial-redos`、补全 URL 协议白名单校验 `js/incomplete-url-scheme-check` 与 HTML 属性转义 `js/incomplete-html-attribute-sanitization`、使用 `crypto.getRandomValues` 替代 `Math.random()`、迭代式剥离内联标签、通过 `Settings.Snapshot()` 隔离本地目录探测路径污点 `go/path-injection`、净化运行日志回车换行符 `go/log-injection` 并隔离 500 内部错误详情）。
- **旧版相对路径图片孤儿误判修复、即时渲染对齐块与媒体卡片样式优化**：
  - 媒体引用扫描现可将 Solo 旧版相对/绝对图片路径（`image/x.png`、`./image/x.png`、`/posts/image/x.png`、`/images/x.png`、`/upload/x.png`）解析为旧图片路由实际读取的对象键，正在被文章引用的迁移图片不再被误标为「未引用 · 孤儿」。
  - 即时渲染模式支持多行 `<div align=center|right|left>`、`<p align=…>`、`<center>` 与 `style="text-align:…"` 对齐容器（光标不在时自动隐藏包裹标签行，内部图片与文字按对齐方式渲染）。
  - 媒体库网格的选择框改为与引用徽章对齐的小型悬浮复选框（桌面端悬停或已有选中项时显示），操作栏收敛为单行并将「删除」改为图标按钮。
  - 带链接图片 `[![alt](src)](href)` 在即时渲染模式下显示为图片加紧凑的 `↗ 域名` 链接标签，不再残留 `[` 与 `](链接)` 源码；文章渲染器保留白名单内的 `align` 对齐属性，并在 `<div|p|center align=…>` 包裹块未留空行时也能正常解析其中的 Markdown。
  - 即时渲染模式下 `<div align=…>` 包裹标签行改为淡色源码常显（便于查看与修改），所见即所得模式保存时保留对齐包裹块。
  - 后台「站点图标 Favicon 地址」设置自定义图标后，`/favicon.ico` 与 `/favicon.svg` 同步重定向至自定义图标（支持 PNG/JPG/SVG/ICO），主题模板不再额外输出内置 `/favicon.ico` 备用图标链接，浏览器标签页不再显示默认「V」图标。
- **迁移评论头像 404 与编辑器媒体面板排版修复**：
  - 旧图片兼容路由内置 Volo-solo / Solo 前端包自带的默认头像（`/images/volo-mark.svg`、`/images/default-user-thumbnail.png`），从旧站迁移过来、引用这些路径的评论头像不再 404。
  - 文章编辑器「媒体与摘要」面板中，「播客音频 URL」「音频时长」输入框不再被封面预览图撑到行底，改为紧贴标签显示。
  - 文章卡片封面链接改用 `aria-label`，消除点击封面时浏览器报出的 `Blocked aria-hidden` 无障碍警告。
- **公告栏对齐块渲染**：后台公告栏 Markdown 与文章正文一致，支持 `<p|div|center align=…>` 包裹块内的 Markdown 渲染。
- **移动端卡片交互修复、`<meta name="color-scheme">` 同步与输入法回车防误触**：
  - 将表格行 `:hover` 高亮限定于精细指针设备（`@media (hover: hover) and (pointer: fine)`）并在触屏关闭弹窗时自动失焦，彻底消除手机端列表卡片残留异常悬停矩形框。
  - 修复移动端「顶部导航」卡片中「目标地址」字段孤立悬浮右侧的排版错位（支持 `.col-stack-mobile` 纵向堆叠长链接）。
  - 在前台主题与管理后台统一接入 `<meta name="color-scheme">` 动态同步（`light dark` / `light` / `dark`）与 `:root { color-scheme: light dark }`。
  - 为全部自定义 `Enter` 提交输入框补齐 `isComposing || keyCode === 229` 中文 / 日文输入法候选词回车保护。

---

## [1.0.0] - 2026-11-09

### 新增特性
- **实时无损图片处理管线（`/uploads/...?...`）**：
  - **七牛云路径语法 + 标准 KV 查询双语法兼容**：同时支持 `?imageView2/<mode>/w/960/h/540/format/webp/q/85/interlace/1/colors/128/strip/ignore-error/1` 与标准查询参数 `?mode=1&w=960&h=540&format=webp&q=85...`，覆盖 `0`–`5` 六种缩放裁剪模式、`jpg`/`png`/`webp`/`avif`（纯 Go 自动回退 `webp`）、质量 `1`–`100`、调色板量化 `2`–`256`、EXIF 方向自动摆正及 `ignore-error=1` 优雅降级。
  - **原图零修改直通、并发合并与孤儿扫描联动**：无参数请求零开销直通原图，绝不改动磁盘或 S3 原始文件；带参变换经 `singleflight` 并发合并与严格 `8 MiB` 字节级 LRU 缓存加速，同时自动预测变换后宽高以消除页面布局偏移（Zero-CLS），并在孤儿媒体清理时自动剥离查询参数防止误删。
- **数据库结构 `v8` — 账号封禁/解封、定时禁言与可逆历史评论屏蔽**：
  - 为 `users` 表幂等新增 `ban_until`、`muted_until`、`ban_reason`、`shield_comments`、`shield_text` 字段（兼容 SQLite、PostgreSQL、MySQL），支持后台用户管理与评论审核台一键联动处置。
  - 支持永久或定时封禁（立即吊销在线会话与个人 API 令牌，到期自动解封）及永久或定时禁言（允许登录阅读但拦截发评与互动并提示到期时间）。
  - **可逆历史评论屏蔽**：开启 `shield_comments` 后在渲染层将目标用户的历史评论无损替换为自定义占位文案，数据库原始 `content_md` 与回复楼层完整保留，解封或到期后即刻自动恢复原文。
- **分身份评论字数/行数边界、防刷屏高度炸弹与全屏回复编辑器**：
  - **三级身份独立配额**：默认最小 `2` 字，游客上限 `512` 字（`16` 行）、注册读者 `1024` 字（`24` 行）、管理员/编辑 `4096` 字（`64` 行），均可在后台「阅读与评论防护」实时热调。
  - **多层防刷屏高度炸弹**：归一化 Unicode 换行符，限制 Zalgo 组合字符叠加层数（`<= 3`），折叠盲文空白符/韩文填充符/零宽字符等隐形空行，合并连续空行，降级评论内大标题，限制图片与代码块最大高度，并对超长评论自动折叠展开。
  - **增强型安全 Markdown 评论框**：内置格式化工具栏、净化实时预览、全屏专注编辑与实时字数/行数统计。

### 性能瘦身、全配置闭环与安全加固
- **全 6 平台静态单二进制 `< 20.00 MiB` 深度瘦身**：自研零反射流式 XML 解析器（`internal/tinyxml`）替代 `encoding/xml`，自研纯 Go STARTTLS/SMTPS 邮件客户端与 RFC 5322 校验器（`internal/notify`）替代 `net/smtp` + `net/mail`，配合 `GOEXPERIMENT=nojsonv2` 与 `-funcalign=8`，使全部 6 大平台静态二进制严格 `< 20.00 MiB`、温热常驻内存 `VmRSS < 35.00 MiB`。
- **全链路配置闭环与安全加固**：完整打通全部 4 种文章固定链接模板（`/posts/{slug}`、`/archives/{slug}`、`/p/{slug}`、`/{year}/{month}/{slug}` 及其 `.md` 源码端点）、2FA 信任天数与窗口步长、待审回复通过后的邮件补发、上传体积上限拦截、WAF 参数实时热更新及 `/api/admin/me` 防跨站请求头校验。
- **插件沙箱与异步写回 KV 加固**：新增流式有界 `JSON.parse`、AST 递归深度上限（`128`）、内置原型链冻结与池化快照隔离、二进制安全 `btoa`/`atob` 与异步批量落盘 `volo.kv.incrAsync`，消除高并发下 SQLite 写锁争用。
- **Unix Domain Socket、服务安装脚本与发布流水线加固**：支持 `--unix-socket`、`--socket-group`、`--socket-mode` 及父目录符号链接防逃逸校验，提供只读 `volo healthcheck`、无 Shell 注入的 OpenRC 环境解析、`systemd` 读写路径最小权限隔离与升级失败自动回滚。

---

## [1.0.0-rc6] - 2026-11-09

### 新增特性
- **统一前置拦截链（`before_*`）与确定性插件优先级排序**：
  - 文章、页面、评论、用户注册/资料修改、媒体上传等全部 `before_*` 钩子共享统一链式管线，支持 `volo.reject(reason, code)`、无损字段改写与单调审核收紧（`status: "pending"`）。
  - 插件中心支持执行优先级（`-1000..10000`）数值编辑与一键上移/下移/置顶（附 FLIP 弹簧过渡动画）。
- **5 阶段插件生命周期与最小堆定时任务（`CronRegistry`）**：
  - 完整支持 `onInstall` → `onEnable` → `onConfigChange` → `onDisable` → `onUninstall` 生命周期回调，以及声明式与动态注册定时任务、异常恢复与后台一键手动触发。
- **零 CGO 双数组 Aho-Corasick 敏感词引擎（`pkg/wordfilter`）与官方 `sensitive-words` 插件**：
  - 原生 `O(N)` 零堆分配扫描，内置繁简转换、全半角折叠、Zalgo 剥离与同形异义字归一化的 4 遍抗混淆清洗，支持最长白名单优先放行、远程词库 ETag 同步、渲染层非破坏性 `*` 打码，以及多列搜索标签编辑器（`tag_list`）。
- **结构迁移 `v7`、敏感操作二次密码确认（Step-Up）与完整繁中 / 日语包**：
  - 备份导出与 OIDC 账号绑定接入二次密码核验，并随包附带完整 `./locales/zh-TW.json` 与 `./locales/ja-JP.json` 语言包。

### 优化与安全加固
- **自研纯 Go PostgreSQL v3 驱动（`pgwire`）与单遍 HTML5 AST 净化器**：支持 SCRAM-SHA-256、TLS 与简单/扩展查询协议，彻底移除臃肿第三方依赖；外部主题与插件静态资源采用 `os.OpenInRoot` 防符号链接穿越，并通过全部 CodeQL 安全扫描。

---

## [1.0.0-rc1] - 2026-11-08

### 初始架构特性
- **纯 Go 零 CGO 跨平台静态单文件引擎**：支持 `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64` 六大目标平台，多层配置优先级（`CLI 参数 > 环境变量 > .env > 后台数据库设置 > 默认值`），统一支持 SQLite WAL（读写物理分离连接池）、PostgreSQL 与 MySQL 8+ / MariaDB / TiDB。
- **33 张规范化数据表与流式迁移备份**：涵盖 9 大热重载配置域、超万条 URL 自动 `<sitemapindex>` 分片、二阶段内存全文检索、旧版 15 表备份流式 ETL 导入（自动生成 `.html` `301` 永久重定向）以及跨数据库 `NDJSON` + 媒体流式备份恢复与 WebDAV / S3 定时异地同步。
- **动态隐身后台路径、多库离线 GeoIP 与全套身份认证**：支持零停机切换隐身管理入口（默认 `/console` 自动伪装 404），支持 `GeoLite2-City` / `Country` / `ASN` 与 `ip2region` 多库并存分级查询，内置 `Argon2id`、WebAuthn 通行密钥、TOTP 两步验证、`volo_pat_*` 细粒度令牌、游客三层自愈凭证与算力验证码（PoW）。
- **三模式 Studio 编辑器、10 平面防泄密守卫与隐私媒体管线**：内置 `IR`（即时渲染）/ `WYSIWYG`（所见即所得）/ `SV`（分栏预览）三模式编辑器，`ContentVisibilityGuard` 严防加密文章与 `:::lock` 段落通过任何订阅源、搜索或缓存泄露，图片上传自动剥离 100% GPS/EXIF 隐私元数据并自适应转码 WebP。
- **可扩展主题 SDK、ES6 插件沙箱与全平台服务守护脚本**：内置 `volo-studio` 与外置 `volo-journal` 主题，纯 Go ES6 沙箱插件运行时，以及支持升级前自动备份、健康检查与失败自动回滚的 Linux / macOS / Windows 服务安装器与反向代理配置模板。
