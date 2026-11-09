# Themes, Studio Editor & Plugin Development Guide

> 中文版本：[主题定制、三模态编辑器与插件开发指南](../zh-CN/themes-and-plugins.md)

`Volo-go` ships with the zero-build modern two-column editorial theme **`volo-studio`** and a **3-Mode Studio Editor**, while exposing clean, decoupled **external theme packages (`./themes/*`)** and a **capability-sandboxed plugin runtime (`./plugins/*`)** that hot-load without recompiling the Go host binary.

---

## 1. Theme Architecture & Custom Theme Authoring

### 1.1 Semantic Design Tokens & Collision-Free Styling

The built-in `volo-studio` theme and the official external magazine theme [`themes/volo-journal`](../../themes/volo-journal/) share semantic CSS custom properties supporting flicker-free appearance modes (light / dark / warm paper / system) and 6 WCAG AA-compliant accent palettes (`forest`, `ocean`, `teal`, `amber`, `plum`, `rose`):

```css
:root {
  --volo-bg: #faf9f5;
  --volo-surface: #ffffff;
  --volo-raised: #f3f1eb;
  --volo-text: #1c1e1b;
  --volo-muted: #5f665e;
  --volo-line: rgba(28, 30, 27, 0.10);
  --volo-accent: #1f5c48;        /* Dynamically switched via Admin Appearance settings */
  --volo-accent-soft: rgba(31, 92, 72, 0.10);
  --volo-font-sans: system-ui, -apple-system, "PingFang SC", "Noto Sans SC", sans-serif;
  --volo-font-serif: "Songti SC", "Noto Serif SC", Georgia, serif;
  --volo-font-mono: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
}
```

### 1.2 External Theme Layout, Clean Starter `.zip` Export & In-Place Upgrade

Create a folder under `VOLO_THEMES_DIR` (default `./themes`), or upload a `.zip` package in the Admin Console (default `/console`, or hot-switched via `security.admin_path`) -> **Appearance & Extensions -> Themes**:

```text
themes/my-editorial/
├── theme.json              # Required theme manifest
├── preview.svg             # Optional gallery preview (.svg / .png / .webp)
├── locales/                # Optional theme-scoped locale dictionaries ({{tt .ThemeID .Locale "key"}})
│   ├── zh-CN.json
│   └── en-US.json
├── assets/
│   ├── theme.css           # Served fingerprinted & pre-compressed via {{assetURL .ThemeID "theme.css"}}
│   └── theme.js            # Optional progressive enhancement script
└── templates/              # Optional template overrides (missing pages fall back safely to host views)
    ├── layout.html
    ├── index.html
    ├── article.html
    ├── page.html
    ├── page-wide.html
    └── error.html
```

- **Clean Starter `.zip` Export**: Click **Export (.zip)** (`GET /api/admin/themes/{id}/export`) on any theme card (including built-in `volo-studio`) to download a clean starter `.zip` stripped of temporary or OS noise files.
- **In-Place `.zip` Upgrade**: Upload via `POST /api/admin/themes/install?overwrite=true` to hot-upgrade an existing theme in place while preserving all saved `Appearance.ThemeConfigs[<id>]` values.

### 1.3 Persistent PJAX Islands (`data-volo-persist`)

When seamless PJAX navigation is enabled, the main content container swaps cleanly while widgets mounted in `public.footer` or `public.header` with `persist: true` remain alive inside persistent islands — **keeping podcast mini-player audio uninterrupted and interactive companions (such as `kanbanniang`) stateful across page transitions**.

### 1.4 Dual Desktop/Mobile Theme Routing & Header Menu Orchestration (`sortable_list` + `.NavMenu`)

- **Independent Desktop & Mobile Themes**: Assign distinct themes for desktop (`active_theme`) and mobile (`mobile_theme`, empty inherits desktop) in **Appearance & Extensions -> Themes**. The server routes automatically with `Vary: User-Agent, Sec-CH-UA-Mobile`, and admins can live-preview via `/?skin=<id>` and `/?skin=<id>&device=mobile`.
- **Sortable Header Menus & Overflow**: Declare `"type": "sortable_list"` in `theme.json` `config_schema` (such as `header_nav_items` + `header_nav_primary_count`) so site owners can toggle and drag-reorder built-in sections (`home`, `archives`, `tags`, `categories`, `series`, `books`, `links`, `follows`, `dynamic`) and custom links (`custom`). Templates read `PageContext.NavMenu` (`.Primary`, `.Overflow`, `.OverflowActive`) directly.

### 1.5 Advanced Theme Extensibility (`page_templates`, `{{customVar}}`, `Reading.ListStyle` & Frameless `error.html`)

1. **Selectable Standalone Page Layouts (`page_templates`)**:
   Declare `"page_templates"` in `theme.json` so authors can pick custom layouts per standalone Page (rendering `templates/page-<value>.html`, with automatic fallback to `templates/page.html` if another theme lacks that template):
   ```json
   {
     "page_templates": [
       {
         "id": "default",
         "value": "default",
         "label": { "zh-CN": "标准版式", "en-US": "Default Layout" }
       },
       {
         "id": "wide",
         "value": "wide",
         "label": { "zh-CN": "宽幅展示", "en-US": "Wide Showcase" }
       }
     ]
   }
   ```
2. **Custom Site Template Variables (`{{customVar}}` / `Site.CustomVars`)**:
   Site owners can manage arbitrary key-value variables in **Admin Console -> Site Settings -> Custom Template Variables** (`site.custom_vars`, also imported automatically from legacy Bolo-Solo `customVars`). Read them in any template without editing `theme.json`:
   ```gotmpl
   {{customVar .Site.CustomVars "icp_number" ""}}
   ```
3. **Timeline List Styles (`Reading.ListStyle`)**:
   **Settings -> Reading** exposes `reading.list_style` with three modes: `summary` (excerpt cards, default), `title_only` (minimalist title index), and `full` (full-article stream). When set to `"full"`, public `.ArticleCards` automatically receive sanitized `.FullHTML` while password-protected posts and locked `:::lock ... :::` blocks remain strictly redacted by `VisibilityGuard`.
4. **Standalone Frameless `error.html` Override**:
   Even when a theme uses `templates/layout.html` master layout inheritance, `templates/error.html` can redefine `{{define "layout"}}<!doctype html>...</html>{{end}}{{define "content"}}{{end}}` (or omit `"content"` and emit a complete HTML document) to bypass standard header/sidebar chrome and render a bespoke full-screen `404` / `403` / `429` / `500` canvas.

For a complete FreeMarker (`.ftl`) migration reference, see the [Theme Authoring & Legacy Bolo-Solo Skin Migration Guide](theme-migration.md).

---

## 2. 3-Mode Studio Editor

The Admin Console features a custom CodeMirror 6 **3-Mode Studio Editor** combining precision Markdown control with fluid visual editing:

- **IR (Instant Rendering)**: Expands raw Markdown syntax on the active cursor line while rendering headings, blockquotes, tables, math, and media widgets inline; includes **tall-image scroll stabilization** so asynchronous high-resolution image loads never jump the viewport, plus full support for both backtick (```` ``` ````) and tilde (`~~~`) code fences.
- **WYSIWYG (Rich Round-Trip)**: Visual rich-text editing with lossless Markdown round-tripping and dedicated **cursor landing pads** around `<audio>` and `<video>` blocks so authors can always place the caret cleanly before, between, or after embedded media.
- **SV (Split View)**: Synchronized side-by-side Markdown source and rendered HTML preview.
- **Native Media, LaTeX Math Protection & Voice Recording**:
  - Native `<video>` and `<audio>` embeds, podcast enclosures, and drag-and-drop/paste uploads;
  - **LaTeX Math Protection**: Inline `$...$` and display `$$...$$` expressions are shielded during Markdown parsing so underscores `_` and asterisks `*` are never corrupted, paired with lazy on-demand KaTeX and Mermaid loading on the public site;
  - Built-in browser **voice recording** that uploads directly to the Media Library and inserts ready-to-play `<audio>` tags, plus `slots:editor.quick_insert` toolbar integration for custom plugin directives.

---

## 3. Capability-Sandboxed Plugin Development (`manifest.json` + Pure-Go ES6 JSVM)

`Volo-go` never hardcodes plugin names in core host code. Built-in capabilities and external plugins (such as [`plugins/kanbanniang`](../../plugins/kanbanniang/) and [`plugins/sensitive-words`](../../plugins/sensitive-words/)) share one declarative capability-gated sandbox and priority-ordered chain protocol.

### 3.1 Plugin Directory Layout, Data Isolation & 5-Stage Lifecycle

```text
plugins/hello-callout/
├── manifest.json           # Permissions, priority, UI slots, hooks, cron & config_schema
├── main.js                 # Pure-Go embedded ES6 JSVM entry script
├── dicts/                  # Optional read-only dictionary files confined inside pluginDir
└── assets/                 # Optional static assets (served via os.OpenInRoot at /plugins/<id>/assets/*)
    ├── widget.css
    └── widget.js
```

- **Physical Data Isolation & Clean Uninstall**:
  - Persistent state is isolated per `(plugin_id, key)` in `plugin_kv` (`<= 256 KiB` per value, `<= 2,000` keys default quota), with secrets sealed in `plugins.secret_ciphertext`; plugins can read relative files inside their own folder only and can never write to the host filesystem.
  - **5-Stage Lifecycle**: `onInstall()` (fresh install or in-place ZIP upgrade) -> `onEnable()` -> `onConfigChange(newCfg)` (hot-applied immediately without restarting the VM) -> `onDisable()` (unregisters all dynamic cron jobs and providers) -> `onUninstall()` (invoked best-effort before transactionally wiping `plugin_kv`, `plugins`, and `./plugins/<id>` on disk).
- **Hot Install, In-Place Upgrade (`overwrite=true`) & Clean `.zip` Export**: Upload via `POST /api/admin/plugins/install?overwrite=true` to hot-upgrade while preserving `config_json`, custom `priority`, and encrypted `secret_ciphertext` secrets, or export via `GET /api/admin/plugins/{id}/export` without leaking secret ciphertexts.

### 3.2 `manifest.json`, `priority` Chain Ordering & `type: "secret"` Encrypted Fields

`theme.json` and `manifest.json` share the unified `config_schema` field types:
- `string` / `text` (single-line text), `textarea` (multi-line text)
- `secret` (or any string field with `"secret": true`: masked password input sealed at rest with `AES-256-GCM`)
- `number` (supports optional `min`, `max`, `step` constraints)
- `boolean` (toggle switch)
- `select` (dropdown with `options: [{ "value": "...", "label": { ... } }]`)
- `url`, `image` (sanitized automatically against `javascript:` / `data:` URI injection)
- `color` (`#rgb` / `#rrggbb` color picker)
- `sortable_list` (toggleable, drag-sortable option list)
- `tag_list` (alias `token_list`: capsule-tag list stored as `string[]` and backward-compatible with newline/comma-delimited strings; includes progressive DOM reveal for multi-thousand-word lists, live search filter, and one-click Bulk Edit / Tag View toggle)

```json
{
  "id": "hello-callout",
  "version": "1.1.0",
  "min_host_version": "0.1.0",
  "runtime": "goja",
  "entry": "main.js",
  "priority": 50,
  "author": "Volo Developer",
  "homepage": "https://example.com",
  "name": {
    "zh-CN": "自定义容器与文末寄语",
    "en-US": "Custom Directive & Callout"
  },
  "description": {
    "zh-CN": "注册 :::callout-box 自定义指令并在文章末尾注入定制卡片。",
    "en-US": "Registers :::callout-box directives and appends a customizable card after articles."
  },
  "permissions": [
    "kv",
    "cron",
    "render",
    "site:read",
    "crypto",
    "ratelimit",
    "federation",
    "hooks:comment.before_create",
    "hooks:render.pre_markdown",
    "hooks:render.post_html",
    "slots:article.after_content",
    "slots:editor.quick_insert",
    "http.outbound:api.github.com"
  ],
  "allowed_hosts": ["api.github.com"],
  "hooks": ["comment.before_create", "render.pre_markdown", "render.post_html"],
  "cron": [
    { "name": "sync_status", "interval_seconds": 3600, "handler": "onSyncCron" }
  ],
  "ui_slots": [
    {
      "slot": "article.after_content",
      "dynamic": true,
      "style_asset": "widget.css"
    }
  ],
  "config_schema": [
    {
      "key": "banner_text",
      "type": "textarea",
      "default": "Thanks for reading! Subscribe via RSS for updates.",
      "label": { "zh-CN": "提示文案", "en-US": "Banner Text" }
    },
    {
      "key": "accent_color",
      "type": "color",
      "default": "#1f5c48",
      "label": { "zh-CN": "卡片强调色", "en-US": "Accent Color" }
    },
    {
      "key": "api_secret",
      "type": "secret",
      "label": { "zh-CN": "第三方密钥", "en-US": "API Secret" }
    }
  ]
}
```

> **Plugin Priority (`priority`) & Deterministic Chain Ordering**: Every plugin has an execution `priority` (`-1000..10000`, default `100`, official `sensitive-words` defaults to `20`; **smaller numbers execute earlier**, breaking ties deterministically by `id`). Admins can reorder plugins with one click (`↑` / `↓` / Move-Top via `POST /api/admin/plugins/reorder`, animated with the unified FLIP row transition) or click `#1 · P20` to set an exact integer (`PATCH /api/admin/plugins/{id}/priority`). This order governs **all domain and `before_*` interception hooks, UI slot concatenation, render cache digests, and same-type `ProviderRegistry` chains** (`antispam`, `notifier`, `federation`, `oidc`, `storage`).

> **Encrypted-at-Rest `type: "secret"` Guarantee**: Fields declared with `"type": "secret"` (or `"secret": true`) are encrypted at rest via the host `KeyRing` using `AES-256-GCM` (`plugins.secret_ciphertext` bound to `AAD = plugins.secret_ciphertext:<plugin_id>`). Admin APIs return only boolean masks (`secret_keys_set`) and never echo plaintext secrets; pass `"__VOLO_CLEAR_SECRET__"` to explicitly clear a stored secret.

### 3.3 Unified Pre-Action Chained Interception (`before_*` Hooks) & Host Keyword Merge Semantics

1. **Unified Interceptable Hooks (`IsInterceptableHook`)**:
   All `before_*` hooks (including custom `*.before_*` hooks) execute in priority order with **chained payload transformation and short-circuit rejection**:
   - Comments: `comment.before_create`, `comment.before_update` (if a hook mutates `content_md`, the host automatically re-runs spam/word-filter checks so hook rewrites cannot bypass moderation);
   - Users & Nicknames: `user.before_register` (covers email OTP preflight, reader registration, and first-time OIDC auto-provisioning), `user.before_update_profile` (covers reader nickname edits and staff profile saves);
   - Content & Media: `article.before_save`, `page.before_save`, `media.before_upload`.

2. **Four Standard Interception Forms (Zero False Circuit-Breaker Trips)**:
   Returning or throwing any of the following inside a `before_*` hook immediately short-circuits downstream plugins and returns a localized rejection (`HookRejectedError` — **counted as an intentional policy block, never incrementing `failure_count` or tripping `CIRCUIT_OPEN`**):
   - Recommended: `return volo.reject("Blocked keyword detected", "SENSITIVE_WORD")` or `throw volo.reject("Blocked keyword detected", "SENSITIVE_WORD")`;
   - Structured object: `return { allow: false, reason: "Policy violation", code: "POLICY_BLOCK" }` or `return { action: "reject", reason: "..." }` (or `{ reject: true, reason: "..." }`);
   - Boolean shorthand: `return false`;
   - **Non-Destructive Partial Payload Merge & Monotonic Moderation**: Returning a plain object (e.g. `return { content_md: cleaned }`) shallow-merges keys into the payload for the next plugin in the chain; setting `{ status: "pending" }` (or `{ action: "moderate" }`) on comments ratchets monotonically so lower-priority plugins cannot accidentally downgrade moderation back to `approved`.

3. **Official `sensitive-words` Plugin, Host Settings Union & Non-Destructive `*` Masking for Existing Content**:
   - **Intra-Plugin Lexicon Merge**: Compiles `houbb_base.txt.gz` (`281 KiB` prefix/tag delta-compressed dictionary with 6 category toggles), built-in technical `allowlist.txt`, optional SSRF-safe remote wordlist sync, capsule-tag `custom_deny` / `custom_allow` (longest-span allowlist override priority), and `reserved_nicknames` into a single native Double-Array Aho-Corasick automaton.
   - **Host `reading.blocked_keywords` / `allowed_keywords` Union**:
     - **Nicknames & Profiles**: Enforces a strict **union** across host `reading.blocked_keywords`, all enabled `provider:antispam` plugins (`checkNickname`), and `user.before_*` hooks — any match rejects the nickname;
     - **Comments**: Smart moderation evaluates host `reading.blocked_keywords` first (marking matches `pending`), then runs `provider:antispam` plugins in priority order — non-matching plugins return `"pass"` so host `pending` decisions are preserved, while matches escalate to `reject`, mask-and-allow, or `moderate`.
   - **Non-Destructive `*` Display Masking for Historical Content (`mask_existing` + `volo.wordFilter.setSanitizer`)**:
     - When word filtering is active (`mask_existing: true` by default in `sensitive-words`, and/or host `builtin_spam_filter` + `blocked_keywords`), pre-existing historical comments, guest impersonator handles, and recent-comment feeds/excerpts are automatically masked with `*` in pure Go with zero per-comment JS VM overhead, instant state-hash cache invalidation, and zero destructive database mutations;
     - HTML-aware masking preserves all HTML tags, attributes (`<a href>`, `<img src>`), and entities while piercing inline split-tag evasions (`傻<strong>逼</strong>` -> `*<strong>*</strong>`); genuine staff/author display names are exempted from `reserved_nicknames` masking, and pre-existing registered readers whose legacy display names contain newly-reserved substrings have their public display names masked without being locked out from posting clean comments.

### 3.4 Permissions, Scheduled `CronRegistry` & Extension Providers

1. **Permission Allowlist Tokens**:
   - **Storage & Host Capabilities**: `kv` / `kv:rw`, `kv:read`, `crypto` (`sha256`, `hmacSha256`, `randomUUID`), `site:read` (`volo.site.info()`), `ratelimit` (`volo.rateLimit`), and `cron` (unlocks `manifest.json` `"cron"` schedules plus dynamic `volo.cron.register/unregister/list`, up to 16 jobs per plugin, `>= 5s` interval, with one-click **Run Cron Now** in the Admin UI);
   - **Custom Markdown Directives (`render` + `volo.registerDirective`)**: Transforms custom `:::name {key="val"} ... :::` blocks and sanitizes output via the host single-pass HTML5 AST sanitizer; built-in `:::lock` remains physically reserved by the host;
   - **Automatic Federation Dispatch (`federation` / `provider:federation`)**: Asynchronous priority-ordered `publish(ev)` / `deliver(ev)` dispatch on public article publish/update with 100% redaction of locked posts and `:::lock` blocks;
   - **Domain Hooks** (`hooks:*`, `hooks:<domain>.*` wildcards, or granular tokens):
     - Pre-action interception hooks: `hooks:comment.before_create`, `hooks:comment.before_update`, `hooks:user.before_register`, `hooks:user.before_update_profile`, `hooks:article.before_save`, `hooks:page.before_save`, `hooks:media.before_upload`;
     - Post-action & render hooks: `hooks:article.{published,updated,deleted}`, `hooks:page.{published,updated,deleted}`, `hooks:comment.{created,moderated}`, `hooks:render.{pre_markdown,post_html}`;
   - **UI Slots** (`ui:slots` or `slots:*`): `public.head`, `public.header`, `public.sidebar`, `article.before_content`, `article.after_content`, `comment.before`, `comment.after`, `public.footer`, `editor.quick_insert`, `admin.dashboard_card`;
   - **Network & Custom Routes**: `http:routes` (`/plugins/<id>/api/*`), `http:raw_headers`, `http.outbound:<hostname>` / `allowed_hosts` (enforced with `OutboundSafeDialer`);
   - **Core Extension Providers** (chained deterministically in ascending `priority` order):
     - `provider:storage`: `storagePut` / `storageDelete` / `storageHealthCheck` (with automatic local-disk fallback);
     - `provider:oidc`: `oidcAuthURL` / `oidcExchange` / `oidcProviderInfo` (PKCE + `nonce` replay protection);
     - `provider:notifier`: `sendNotification(event)` / `notifySend(event)`;
     - `provider:antispam`: `checkSpam(input)` (`{ action: "pass" | "allow" | "moderate" | "reject", reason, score }`) and optional `checkNickname(input)` (`{ allow: boolean, reason }`);
   - **Sensitive Content Gate**: `articles:read_protected`.

2. **Circuit Breakers, Resource Guardrails & Host Security Integration**:
   - Every JS Hook, Directive, Cron job, Federation task, and UI Slot runs under instruction step budgets, wall-clock timeouts, cycle-depth limits, and `4 MiB` string caps; consecutive runtime errors or timeouts automatically trip `CIRCUIT_OPEN`;
   - See [`SECURITY.md`](../../SECURITY.md) for host-level **Dynamic Stealth Admin Path (`security.admin_path`)** and **Multi-DB GeoIP** details.

### 3.5 Sandbox SDK (`volo.*`), Native `wordFilter` Engine & `main.js` Example

Host bridge object `volo` (alias `ctx`):
- `volo.on(hookName, handlerFn)` / `volo.reject(reason, code?)`: subscribe to domain/`before_*` hooks and emit structured policy rejections;
- `volo.wordFilter.compile({ deny, allow, normalize, maxRepeat })` / `volo.wordFilter.loadHoubbDict(relGzPath, opts)`: compile zero-CGO CSR/Double-Array Aho-Corasick matchers exposing `.match(text)`, `.contains(text)`, `.censor(text, maskRune?)`, and `.stats()`;
- `volo.cron.register(name, intervalSec, handlerName, immediate?)` / `volo.cron.unregister(name)` / `volo.cron.list()`: dynamic scheduled jobs (requires `cron`);
- `volo.registerDirective(name, handlerFn)` / `volo.registerFederation({ publish(ev), deliver(ev) })`;
- `volo.escapeHTML(str)`, `volo.config.{get,all}()`, `volo.kv.{get,getInt,set,del,incr,list}(...)`, `volo.crypto.{sha256,hmacSha256,randomUUID}()`, `volo.site.info()`, `volo.rateLimit(...)`, `volo.http.fetch(...)`, `volo.i18n.t(...)`, and `volo.log.{info,warn,error}(msg)`.

```javascript
let matcher = null;

function rebuildMatcher() {
  const customDeny = String(volo.config.get("blocked_words") || "").split("\n");
  matcher = volo.wordFilter.compile({ deny: customDeny, allow: ["GoLang"], normalize: true });
}

function onEnable() { rebuildMatcher(); }
function onConfigChange() { rebuildMatcher(); }
function onUninstall() { matcher = null; }

// Chained pre-action interception: short-circuit via volo.reject (no circuit-breaker trip) or mutate payload
volo.on("comment.before_create", (payload) => {
  if (matcher && matcher.contains(payload.content_md || "")) {
    return volo.reject("Comment contains restricted terms", "WORD_BLOCKED");
  }
  return { content_md: String(payload.content_md || "").trim() };
});

function renderSlot(slotName) {
  if (slotName === "article.after_content") {
    const msg = volo.config.get("banner_text") || "Thanks for reading!";
    return `<aside class="callout-box">${volo.escapeHTML(String(msg))}</aside>`;
  }
  return "";
}
```

---

## 4. Outbound Webhooks & HMAC Signature Verification

Alongside in-process plugins, **Notifications -> Webhooks** natively dispatches `article.published`, `comment.created`, and test events to Feishu/Lark, Telegram, Discord, Slack, Bark, WeCom, and custom HTTP endpoints.

When a signing `Secret` is configured on a custom Webhook, `Volo-go` attaches `HMAC-SHA256` verification headers:

- `X-Volo-Event`: Event name (e.g., `article.published`, `comment.created`, `webhook.test`)
- `X-Volo-Timestamp`: Unix timestamp in seconds
- `X-Volo-Signature`: `sha256=<hex(HMAC_SHA256(secret, timestamp + "." + raw_json_body))>`

Receivers in Node.js, Python, or Go should verify `X-Volo-Signature` using constant-time comparison (`crypto.timingSafeEqual` / `hmac.Equal`) and reject timestamps skewed by more than 5 minutes.
