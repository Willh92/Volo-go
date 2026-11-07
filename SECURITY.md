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
1. 受影响的版本号、操作系统架构与数据库方言（SQLite / PostgreSQL / MySQL）；
2. 最小可复现步骤（HTTP 请求报文、配置项或 PoC 脚本）；
3. 潜在影响评估（例如：10 大出口隐私泄露、SSRF 绕过、RBAC 越权、XSS 等）。

维护团队将在 **48 小时内**确认收到报告，并在 **7 个工作日内**提供评估结论与修复时间表。修复发布后将致谢报告者（除非您希望保持匿名）。

### 核心安全防线概览

- **10 大出口零死角防泄密 (`ContentVisibilityGuard`)**：密码保护文章与未解锁 `:::lock` 内容块在卡片摘要、封面/OG 图、相邻文章、评论读取、二阶段搜索、RSS/Atom/Podcast、Sitemap、双向链接、`/llms.txt` & `.md` 端点以及非授信插件钩子中全部默认关闭（Fail-Closed）脱敏。
- **BREACH / CRIME 压缩旁路防护**：HTTP 动态 Gzip 中间件严格跳过所有携带会话 Cookie（`__volo_sid`）、解锁凭证（`__volo_unlock`）、`Authorization` 头、`?preview=` 限时令牌或设置 `Set-Cookie` / `Cache-Control: private | no-store` 的响应。
- **凭据加密与会话安全**：密码采用 `Argon2id`；`VOLO_SECRET_KEY` 经 `HKDF-SHA256` 派生独立用途密钥，数据库内 SMTP、S3、Webhook、TOTP、文章密码与插件密钥全程 `AES-256-GCM` 认证加密并绑定列级 AAD。
- **反 SSRF 出站拨号器 (`OutboundSafeDialer`)**：Webhook、远程字体/书单抓取、S3/WebDAV 与插件网络请求统一走 DNS 解析后 IP 校验拨号器，物理拦截 `127.0.0.0/8`、`10.0.0.0/8`、`169.254.169.254` 云元数据、`::1`、`fc00::/7` 等内网段及重定向跳板。
- **真实客户端 IP 防伪造 (`VOLO_TRUSTED_PROXIES`)**：仅当直连对端处于受信反代 CIDR 白名单时才从右向左剥离 `X-Forwarded-For`，彻底阻断伪造请求头绕过 WAF 与限流。
- **紧急熔断开关**：若管理员误配 GeoIP 区域拦截导致自身无法访问后台，可在宿主机 `.env` 或环境变量中设置 `VOLO_GEO_ACCESS_ENABLED=false` 重启即时旁路地域规则，或执行 `volo admin reset-password` / `volo admin disable-2fa` 应急恢复。

---

## English

### Supported Versions

| Version | Supported |
| :--- | :--- |
| `v1.0.x` (`main`) | Yes (Active) |
| `< v1.0.0` | No |

### Reporting a Vulnerability Privately

Please **DO NOT** report security vulnerabilities or privacy bypasses through public GitHub Issues or Pull Requests.

Submit reports privately via **[GitHub Security Advisories](https://github.com/Willh92/Volo-go/security/advisories/new)** with:
1. Affected version, OS/Arch, and SQL dialect (`sqlite`, `postgres`, or `mysql`);
2. Minimal reproduction steps (curl command, `.env` flags, or PoC);
3. Impact assessment (e.g., 10-surface content visibility leak, SSRF bypass, RBAC escalation, XSS).

We acknowledge receipt within **48 hours** and aim to deliver a triage assessment and remediation plan within **7 business days**.

### Defense-in-Depth Architecture

- **10-Surface Privacy Perimeter (`ContentVisibilityGuard`)**: Password-protected posts and locked `:::lock` blocks fail closed across cards, cover/OG images, prev/next links, comments, two-stage search, RSS/Atom/Podcast feeds, Sitemap, backlinks, `/llms.txt` & `.md`, and untrusted plugin hooks.
- **BREACH-Safe Dynamic HTTP Compression**: Dynamic Gzip strictly bypasses any request carrying `__volo_sid`, `__volo_unlock`, `Authorization`, or `?preview=`, as well as any response emitting `Set-Cookie` or `Cache-Control: private | no-store`.
- **Cryptography at Rest & in Transit**: User passwords use `Argon2id`. `VOLO_SECRET_KEY` derives domain-separated subkeys via `HKDF-SHA256`, sealing SMTP, S3, Webhook, TOTP, article passwords, and plugin secrets with `AES-256-GCM` + column-bound AAD.
- **DNS-Pinning Anti-SSRF (`OutboundSafeDialer`)**: All outbound HTTP connections verify resolved IPs at dial time, blocking loopback, RFC1918, link-local cloud metadata (`169.254.169.254`), IPv6 ULA, and cross-redirect private hops.
- **Emergency Recovery**: Set `VOLO_GEO_ACCESS_ENABLED=false` to immediately bypass misconfigured GeoIP lockouts, or use `volo admin reset-password` / `volo admin disable-2fa` locally over SSH.
