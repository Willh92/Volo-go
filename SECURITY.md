# Security Policy / 安全策略与漏洞响应

[简体中文](#简体中文) | [English](#english)

---

## 简体中文

### 支持版本

| 版本分支 | 安全更新支持 |
| :--- | :--- |
| `v1.0.x` (`main`) | 支持（主动维护） |
| `< v1.0.0` | 不支持 |

### 私密上报安全漏洞

请**切勿**通过公开的 GitHub Issue、Pull Request 或公开讨论区报告安全或隐私越权漏洞。

请直接通过 **[GitHub Private Security Advisories](https://github.com/Willh92/Volo-go/security/advisories/new)** 私密提交报告，并在报告中附上：
1. 受影响的版本号、操作系统架构与数据库方言（`sqlite` / `postgres` / `mysql`）；
2. 最小可复现步骤（HTTP 请求报文、配置项或 PoC 脚本）；
3. 潜在影响评估（例如：10 大出口隐私泄露、SSRF 绕过、RBAC 越权、XSS 等）。

维护团队将在 **48 小时内**确认收到报告，并在 **7 个工作日内**提供评估结论与修复时间表。修复发布后将致谢报告者（除非您希望保持匿名）。

### 核心安全防线概览

- **10 大出口零死角防泄密 (`ContentVisibilityGuard`)**：密码保护文章与未解锁 `:::lock` 内容块在卡片摘要、封面/OG 图、相邻文章、评论读取、二阶段搜索、RSS/Atom/Podcast、Sitemap、双向链接、`/llms.txt` & `.md` 端点以及非授信插件钩子中全部默认关闭（Fail-Closed）脱敏。
- **动态隐身后台路径 (`VOLO_ADMIN_PATH` & `security.admin_path` / `dynamic_login_path`)**：支持通过启动配置或管理台 `PATCH /api/admin/settings/security` **免重启热重载**自定义隐身后台前缀（如 `/secret-studio`）。一旦启用自定义前缀，所有默认扫描路径（`/console`、`/login`、`/admin`、`/start`）均返回不可区分的 `404` 响应并联动扫描器风控计数，同时自动从 `/robots.txt` 移除后台路径提示；保留路由段（如 `/api`、`/posts`、`/uploads`）在保存时严格拦截。
- **可配置三档阶梯自动风控、IP/CIDR 免封白名单与封禁/日志保留治理**：WAF 自动风控支持管理台实时调整统计窗口（`risk_window_seconds`）、触发阈值（`risk_strike_threshold`）、初犯/再犯/累犯三档封禁时长（`risk_tier{1,2,3}_minutes`，支持 `0` 永久封禁，并结合库内 `security_ip_bans.occurrences` 跨周期累犯加重）及规范化 IP/CIDR 免封白名单（`risk_whitelist_cidrs`，豁免自动风控计数与全局高频限流）；手动或批量「解封」采用提前到期机制保留历史轨迹，同时支持配置已过期封禁（`ip_ban_retention_days`）、审计日志（`audit_log_retention_days`）与插件日志（`plugin_log_retention_days`）的保留天数（`0` 永久保留）或一键清空。
- **离线多库共存 GeoIP 引擎与热重载**：零外部 API 调用，原生支持同目录共存挂载 `GeoLite2-City.mmdb`、`GeoLite2-Country.mmdb`、`GeoLite2-ASN.mmdb`（内置库级优先级排序，确保非地理型 `ASN`/`ISP` 库绝不遮蔽 `City`/`Country` 精度）以及 `ip2region` IPv4/IPv6 `.xdb` 级联查询；支持管理台免重启热切换 `security.geoip_dir`（严格阻断 `..` 穿越、符号链接、系统敏感目录及与公开 `uploads` 目录重叠）、可配置默认未命中策略 `geo_default_action` 与未知 IP 策略 `geo_unknown_action`（`allow | deny`），并内置当前管理员 IP 防自锁校验。
- **BREACH / CRIME 压缩旁路防护**：HTTP 动态 Gzip 中间件严格跳过所有携带会话 Cookie（`__volo_sid`）、解锁凭证（`__volo_unlock`）、`Authorization` 头、`?preview=` 限时令牌或设置 `Set-Cookie` / `Cache-Control: private | no-store` 的响应。
- **凭据落盘加密、Step-Up 二次密码验证、`X-Volo-Console` 反 CSRF 与插件机密隔离**：用户密码采用 `Argon2id`（兼容历史 `PBKDF2` 登录静默升级）；管理台 CSRF 引导令牌（`GET /api/admin/me`）要求显式同源 `X-Volo-Console: 1` 头防止跨站预取探测；全站备份下载与第三方 OIDC 账号绑定强制要求交互式 Step-Up 当前密码二次核验签发一次性短效票据；`VOLO_SECRET_KEY` 经 `HKDF-SHA256` 派生领域隔离子密钥，建站向导 `data/db.json`、SMTP、S3、WebDAV、Webhook、TOTP、文章密码以及插件 `type: "secret"` 敏感配置全程采用 `AES-256-GCM` 认证加密并绑定列级 AAD。
- **单趟 HTML5 AST 净化、评论刷屏高度轰炸防御与类 Discourse 账户封禁/禁言屏蔽**：文章/评论/插件 HTML 经基于 `golang.org/x/net/html` 的单趟白名单 AST 净化器消除 mXSS；评论管线内置按身份动态字数/行数阈值（默认游客 `512` 字/`16` 行、普通用户 `1024` 字/`24` 行）、Zalgo 组合符堆叠截断（每基字符 `<= 3`）、盲文空白 `U+2800`/谚文填充符/零宽字符空白行折叠、标题降级（`h1`–`h3` -> `h4`）、连续 `<br>` 封顶及前端超高评论自动折叠；账户治理支持永久/限时封禁与禁言，并支持**非破坏式历史评论一键文案屏蔽（`shield_comments`）**，解封或到期后自动无损恢复原评论；外置主题与插件静态资源统一通过内核级 `os.OpenInRoot` 句柄读取，物理阻断符号链接越界。
- **插件 ES6 沙箱资源硬边界与原型冻结隔离**：自研纯 Go ES6 VM 对流式 `JSON.parse` / `goToJSValueBounded` 强制深度（`<= 64`）、数组元素（`<= 65,536`）、对象键数（`<= 16,384`）与 AST 递归（`<= 128`）硬上限，冻结内置原型（`Object`/`Array`/`String`/`Function`）并在池化 VM 借还间执行 `snapshotPostInit`/`restorePostInit` 快照重置，彻底阻断跨请求原型污染与超时吞没。
- **反 SSRF 出站拨号器 (`OutboundSafeDialer`)**：Webhook、远程 OPML/书单抓取、S3/WebDAV 与沙箱插件网络请求（含默认远程词库同步 URL）统一校验 `allowed_hosts` 白名单并走 DNS 解析后连接时 IP 校验拨号器，物理拦截 `127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`169.254.169.254` 云元数据、`::1`、`fc00::/7` 等私网地址及跨重定向内网跳板。
- **服务安装器最小权限分离与 Unix Socket 防符号链接劫持**：系统服务默认实现可执行文件目录（`root:root 0755`）与数据目录（`volo:volo 0750`）权限物理分离；`OpenRC` 脚本采用纯文本键值解析器加载 `/etc/volo/volo.env`（彻底根除 Shell `.` 执行注入）；Unix Domain Socket 绑定前校验父目录真实路径并以 `Lstat` 拒绝符号链接覆盖，配合 `--socket-group` / `--socket-mode 0660` 安全授权反代进程。
- **真实客户端 IP 防伪造 (`VOLO_TRUSTED_PROXIES`)**：仅当直连对端匹配受信反代 CIDR 白名单（或激活 Unix Domain Socket 时的 `unix` 标识）时才解析转发头，彻底阻断客户端伪造 `X-Real-IP` / `X-Forwarded-For` 绕过 WAF 与限流。
- **紧急熔断与 CLI 救援开关**：若管理员因 IP 变更触发 GeoIP 区域拦截，可在宿主机设置 `VOLO_GEO_ACCESS_ENABLED=false`（或传 `--geo-access-enabled=false`）即时旁路地域规则而不丢失库内策略，或通过 SSH 执行 `volo admin reset-password --user <u|email>` / `volo admin disable-2fa --user <u|email>` 应急恢复。

---

## English

### Supported Versions

| Version | Supported |
| :--- | :--- |
| `v1.0.x` (`main`) | Yes (Active) |
| `< v1.0.0` | No |

### Reporting a Vulnerability Privately

Please **DO NOT** report security vulnerabilities or privacy bypasses through public GitHub Issues, Pull Requests, or public discussions.

Submit reports privately via **[GitHub Private Security Advisories](https://github.com/Willh92/Volo-go/security/advisories/new)** and include:
1. Affected version, OS/Arch, and SQL dialect (`sqlite`, `postgres`, or `mysql`);
2. Minimal reproduction steps (HTTP request, configuration flags, or PoC script);
3. Impact assessment (e.g., 10-surface content visibility leak, SSRF bypass, RBAC escalation, XSS).

We acknowledge receipt within **48 hours** and provide a triage assessment and remediation timeline within **7 business days**. Reporters are credited upon release unless anonymity is requested.

### Defense-in-Depth Architecture

- **10-Surface Privacy Perimeter (`ContentVisibilityGuard`)**: Password-protected posts and locked `:::lock` blocks fail closed across card summaries, cover/OG images, prev/next navigation, comments, two-stage search, RSS/Atom/Podcast feeds, Sitemap, backlinks, `/llms.txt` & `.md` endpoints, and untrusted plugin hooks.
- **Dynamic Stealth Admin Path (`VOLO_ADMIN_PATH` & `security.admin_path` / `dynamic_login_path`)**: Supports zero-downtime hot-reload via bootstrap config or Admin Console `PATCH /api/admin/settings/security` without restarting the server. When customized away from `/console` (e.g., `/secret-studio`), all default probe paths (`/console`, `/login`, `/admin`, `/start`) return indistinguishable `404` responses, feed the scanner risk-control tracker, and omit `Disallow: <admin_path>/` from `/robots.txt`, while reserved system prefixes (`/api`, `/posts`, `/uploads`, etc.) are strictly rejected.
- **Configurable 3-Tier Scanner Risk Control, IP/CIDR Whitelist & Retention Governance**: Live-tunable WAF scanner risk parameters (`risk_window_seconds`, `risk_strike_threshold`, 3-tier repeat-offender durations `risk_tier{1,2,3}_minutes` with `0` = permanent ban and cross-window database `security_ip_bans.occurrences` escalation, plus canonical IP/CIDR `risk_whitelist_cidrs` exempting trusted networks from automatic risk strikes and global WAF rate limits). Unbanning early-expires entries while preserving audit history, paired with configurable retention periods (`0` = keep permanently) and one-click clear actions across expired IP bans (`ip_ban_retention_days`), audit logs (`audit_log_retention_days`), and plugin runtime logs (`plugin_log_retention_days`).
- **Offline Multi-DB GeoIP Coexistence & Hot-Reload**: Zero external API calls, supporting side-by-side mounting of `GeoLite2-City.mmdb`, `GeoLite2-Country.mmdb`, `GeoLite2-ASN.mmdb` (tier-ranked so non-geographic `ASN`/`ISP` DBs never shadow `City`/`Country` accuracy), and `ip2region` IPv4/IPv6 `.xdb` cascade. Supports hot-reloadable `security.geoip_dir` (strictly blocking `..` traversal, symlinks, OS system roots, and overlap with public `uploads`), configurable `geo_default_action` & `geo_unknown_action` (`allow | deny`), and built-in admin IP anti-self-lockout verification.
- **BREACH / CRIME-Safe Dynamic HTTP Compression**: Dynamic Gzip middleware strictly bypasses any request carrying session cookies (`__volo_sid`), unlock cookies (`__volo_unlock`), `Authorization` headers, or `?preview=` timed tokens, as well as any response emitting `Set-Cookie` or `Cache-Control: private | no-store`.
- **Cryptography at Rest, Step-Up Re-Auth, `X-Volo-Console` Anti-CSRF & Isolated Plugin Secrets**: User passwords use `Argon2id` (with transparent `PBKDF2` upgrade on login). Admin CSRF token bootstrapping (`GET /api/admin/me`) requires `X-Volo-Console: 1` to block cross-site prefetch probing. High-sensitivity actions such as interactive backup export and OIDC identity linking require Step-Up current-password verification to mint short-lived one-time tickets. `VOLO_SECRET_KEY` derives domain-separated subkeys via `HKDF-SHA256`, sealing Setup Wizard `data/db.json`, SMTP, S3, WebDAV, Webhook, TOTP, article lock passwords, and plugin `type: "secret"` fields using `AES-256-GCM` + column-bound AAD.
- **Single-Pass HTML5 AST Sanitization, Comment Height-Bombing Defense & Discourse-Style Account Shielding**: Rendered articles, comments, and plugin outputs pass through a single-pass `golang.org/x/net/html` allowlist AST sanitizer eliminating mXSS; comments enforce configurable per-identity rune/line caps (defaults: guest `512` runes/`16` lines, user `1024` runes/`24` lines), Zalgo combining-mark capping (`<= 3` per base rune), invisible filler-line collapsing (`U+2800` Braille blank, Hangul fillers, ZWSP), heading demotion (`h1`–`h3` -> `h4`), consecutive `<br>` caps, and collapsible tall-comment folding; account moderation supports permanent/timed bans and mutes with **non-destructive historical comment shielding (`shield_comments`)** that restores original comments upon unban/unmute or expiry; external theme and plugin static assets are confined via kernel-enforced `os.OpenInRoot` handles.
- **Bounded ES6 Plugin VM & Frozen Prototype Isolation**: The pure-Go ES6 VM enforces streaming depth (`<= 64`), array element (`<= 65,536`), object key (`<= 16,384`), and AST recursion (`<= 128`) caps on `JSON.parse` / `goToJSValueBounded`, freezes builtin prototypes (`Object`/`Array`/`String`/`Function`), resets pooled VM globals via `snapshotPostInit`/`restorePostInit`, and prevents `try/catch` blocks from swallowing execution timeouts.
- **DNS-Pinning Anti-SSRF (`OutboundSafeDialer`)**: All outbound HTTP requests (webhooks, remote OPML/bookshelf fetches, S3/WebDAV, and sandboxed plugins — including default remote wordlist URLs) validate `allowed_hosts` allowlists and verify resolved IPs at dial time, physically blocking loopback (`127.0.0.0/8`, `::1`), RFC1918 (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), link-local cloud metadata (`169.254.169.254`), IPv6 ULA (`fc00::/7`), and cross-redirect private hops.
- **Installer Privilege Separation & Symlink-Safe Unix Sockets**: Service installers separate immutable binary directories (`root:root 0755`) from runtime data (`volo:volo 0750`), parse `/etc/volo/volo.env` in `OpenRC` without shell `.` execution, and validate Unix Domain Socket parent directories (`EvalSymlinks`) + leaf `Lstat` checks before applying `--socket-group` / `--socket-mode 0660`.
- **Anti-Spoofing Client IP Resolution (`VOLO_TRUSTED_PROXIES`)**: Forwarding headers are honored strictly when the direct peer matches the trusted reverse-proxy CIDR allowlist (or `unix` when listening on a Unix Domain Socket), preventing spoofed `X-Real-IP` / `X-Forwarded-For` headers from bypassing WAF and rate limits.
- **Emergency Circuit Breaker & CLI Rescue**: If an IP change triggers a GeoIP lockout, set `VOLO_GEO_ACCESS_ENABLED=false` (or pass `--geo-access-enabled=false`) on the host to immediately bypass region enforcement without deleting saved DB rules, or run `volo admin reset-password --user <u|email>` / `volo admin disable-2fa --user <u|email>` locally over SSH.
