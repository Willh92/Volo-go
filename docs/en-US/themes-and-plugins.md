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

`Volo-go` never hardcodes plugin names in core host code. Built-in capabilities and external plugins (such as [`plugins/kanbanniang`](../../plugins/kanbanniang/)) share one declarative capability-gated sandbox architecture.

### 3.1 Plugin Directory Layout, `.zip` Export & In-Place Upgrade

```text
plugins/hello-callout/
├── manifest.json           # Permissions, UI slots, hooks, cron & config_schema
├── main.js                 # Pure-Go embedded ES6 JSVM entry script
└── assets/                 # Optional static assets (served at /plugins/hello-callout/assets/*)
    ├── widget.css
    └── widget.js
```

- **Hot Install & In-Place Upgrade (`overwrite=true`)**: Drop a folder into `./plugins/` and click **Rescan Plugins**, or upload a `.zip` via the Admin UI / `POST /api/admin/plugins/install?overwrite=true` for zero-downtime installation or in-place upgrades — **in-place upgrades automatically preserve existing `config_json` and encrypted `secret_ciphertext` secrets**.
- **Clean `.zip` Export**: Export any installed plugin via the Admin UI or `GET /api/admin/plugins/{id}/export` into a clean `.zip` archive (stripping runtime temp files and never exporting encrypted secret ciphertexts).

### 3.2 `manifest.json`, `type: "secret"` Encrypted Fields & Unified `config_schema`

`theme.json` and `manifest.json` share the unified `config_schema` field types:
- `string` / `text` (single-line text), `textarea` (multi-line text)
- `secret` (or any string field with `"secret": true`: masked password input sealed at rest with `AES-256-GCM`)
- `number` (supports optional `min`, `max`, `step` constraints)
- `boolean` (toggle switch)
- `select` (dropdown with `options: [{ "value": "...", "label": { ... } }]`)
- `url`, `image` (sanitized automatically against `javascript:` / `data:` URI injection)
- `color` (`#rgb` / `#rrggbb` color picker)
- `sortable_list` (toggleable, drag-sortable option list)

```json
{
  "id": "hello-callout",
  "version": "1.1.0",
  "min_host_version": "0.1.0",
  "runtime": "goja",
  "entry": "main.js",
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
    "render",
    "site:read",
    "crypto",
    "ratelimit",
    "federation",
    "hooks:render.pre_markdown",
    "hooks:render.post_html",
    "slots:article.after_content",
    "slots:editor.quick_insert",
    "http.outbound:api.github.com"
  ],
  "allowed_hosts": ["api.github.com"],
  "hooks": ["render.pre_markdown", "render.post_html"],
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

> **Encrypted-at-Rest `type: "secret"` Guarantee**: Fields declared with `"type": "secret"` (or `"secret": true`) are encrypted at rest via the host `KeyRing` using `AES-256-GCM` (`plugins.secret_ciphertext` bound to `AAD = plugins.secret_ciphertext:<plugin_id>`). Admin APIs return only boolean masks (`secret_keys_set`) and never echo plaintext secrets; pass `"__VOLO_CLEAR_SECRET__"` to explicitly clear a stored secret.

### 3.3 Permissions, Custom Directives, Automatic Federation & Extension Providers

1. **Permission Allowlist Tokens**:
   - **Scoped Storage**: `kv` / `kv:rw` (read/write), `kv:read` (read-only `plugin_kv` with per-plugin quota enforcement and non-invalidating background writes that never thrash public page/RSS caches);
   - **Host Capability Gates**:
     - `crypto`: unlocks `volo.crypto` (`sha256`, `hmacSha256`, `randomUUID`);
     - `site:read`: unlocks `volo.site.info()` read-only site metadata;
     - `ratelimit`: unlocks `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)` sliding-window limiters (also granted automatically with `http:routes`);
     - `cron`: allows background `cron` schedules;
   - **Custom Markdown Directives (`render` + `volo.registerDirective`)**:
     - With `"render"` (or `"hooks:render.*"` / `"hooks:render.directive"`) permission, call `volo.registerDirective(name, handlerFn)` during top-level module initialization to transform custom `:::name {key="val"} ... :::` fenced directives;
     - Receives `{ name, attrs, body }` and sanitizes returned HTML through host `bluemonday` allowlists;
     - **Reserved `:::lock` Protection**: Built-in `:::lock` blocks are physically reserved by the host and can never be intercepted by plugins; repeated directive timeouts or errors trip the circuit breaker (`CIRCUIT_OPEN`);
   - **Automatic Federation Dispatch (`federation` / `provider:federation`)**:
     - Declare `"federation"` (or `"federation:*"` / `"provider:federation"`) and call `volo.registerFederation({ publish(ev), deliver(ev) })` to receive automatic asynchronous dispatches whenever public articles are **published or updated** (`DispatchFederationArticle`, drained gracefully on shutdown) — ideal for ActivityPub, Webmention, AT Protocol, or cross-site syndication plugins;
     - **10th Privacy Surface Guarantee**: Password-protected bodies are redacted (`ev.omitted_protected = true`) and partial `:::lock ... :::` blocks are physically stripped before reaching federation handlers;
   - **Domain Hooks** (`hooks:*` or granular tokens):
     - Articles: `hooks:article.published`, `hooks:article.updated`, `hooks:article.deleted`;
     - Pages: `hooks:page.published`, `hooks:page.updated`, `hooks:page.deleted`;
     - Comments: `hooks:comment.before_create`, `hooks:comment.created`, `hooks:comment.moderated`;
     - Render pipeline: `hooks:render.pre_markdown` (transforms `content_md` on unlocked posts without `:::lock`) and `hooks:render.post_html` (post-processes rendered HTML with secondary sanitization);
   - **UI Slots** (`ui:slots` or per-slot tokens): `slots:public.head`, `slots:public.header`, `slots:public.sidebar`, `slots:article.before_content`, `slots:article.after_content`, `slots:comment.before`, `slots:comment.after`, `slots:public.footer`, `slots:editor.quick_insert`, `slots:admin.dashboard_card`;
   - **Network & Custom Routes**: `http:routes` (mounts `/plugins/<id>/api/*`), `http:raw_headers` (opt-in signature headers such as `x-hub-signature-256`, `x-signature`, `stripe-signature`, `authorization`), `http.outbound:<hostname>` / `allowed_hosts` (explicit domain or `*.domain.tld` allowlist + SSRF-safe `OutboundSafeDialer`);
   - **Core Extension Providers**:
     - `provider:storage`: export `storagePut(objectKey, bodyBase64, contentType)` plus optional `storageDelete(objectKey)` and `storageHealthCheck()` to register external media/backup object storage with automatic local-disk fallback;
     - `provider:oidc`: export `oidcAuthURL(state, redirectURI, opts)` and `oidcExchange(code, redirectURI, opts)` (with PKCE + `nonce` replay verification and `email_verified` defaulting strictly to `false`) plus optional `oidcProviderInfo()` for federated SSO;
     - `provider:notifier`: export `sendNotification(event)` or `notifySend(event)` for custom notification channels;
     - `provider:antispam`: export `checkSpam(input)` returning `{ action: "allow" | "moderate" | "reject", reason, score }` for comment moderation;
   - **Sensitive Content Gate**: `articles:read_protected` (without this explicit permission, password-protected bodies are blanked and `:::lock ... :::` blocks are stripped across every hook surface).

2. **Circuit Breakers, Resource Guardrails & Host Security Integration**:
   - Every JS Hook, Directive, Federation task, and UI Slot runs under strict instruction step budgets, wall-clock timeouts, cycle-depth limits, and `4 MiB` string caps; consecutive failures automatically trip the **circuit breaker (`CIRCUIT_OPEN`)**;
   - `volo.on(...)`, `volo.registerDirective(...)`, and `volo.registerFederation(...)` are permitted only during top-level module initialization and frozen afterward;
   - Scope plugin CSS custom properties under `--volo-plugin-<id>-*` inside `.volo-plugin-scope[data-plugin="<id>"]`;
   - For host-level **Dynamic Stealth Admin Path hot-reload (`security.admin_path`)** and **Multi-Database GeoIP Access Control (`GeoLite2-City/Country/ASN.mmdb` + `ip2region` XDB)** details, see [`SECURITY.md`](../../SECURITY.md).

### 3.4 Sandbox SDK (`volo.*`), ES6 Standard Library & Complete `main.js` Example

The zero-CGO pure-Go JS VM natively supports modern **ES6+ syntax and builtins** (`let`/`const`, template literals, optional chaining `?.`, nullish coalescing `??`, arrow functions, spread/rest and destructuring, plus standard `Array`/`Object`/`String`/`Number`/`RegExp`/`Date`/`JSON`/`Math`, `encodeURIComponent`, and `btoa`/`atob`).

Host bridge object `volo` (alias `ctx`):
- `volo.on(hookName, handlerFn)`: subscribe to domain/render hooks at module init;
- `volo.registerDirective(name, handlerFn)`: register custom `:::name ... :::` Markdown directives at module init (requires `render`);
- `volo.registerFederation({ publish(ev), deliver(ev) })`: register asynchronous federation handlers for article publish/update events at module init (requires `federation`);
- `volo.escapeHTML(str)`: HTML-entity escaping helper;
- `volo.config.get(key)` / `volo.config.all()`: validated plugin settings (with `type: "secret"` fields transparently decrypted inside the sandbox);
- `volo.kv.get(key)` / `volo.kv.getInt(key)` / `volo.kv.set(key, value, ttlSeconds?)` / `volo.kv.del(key)` / `volo.kv.incr(key, delta?, ttlSeconds?)` / `volo.kv.list(prefix, limit?, afterKey?)`: plugin-scoped KV store;
- `volo.crypto.sha256(str)` / `volo.crypto.hmacSha256(secret, msg)` / `volo.crypto.randomUUID()`: cryptographic helpers;
- `volo.site.info()`, `volo.rateLimit(key, max, windowSec?)`, `volo.http.fetch(url, options?)`, `volo.i18n.t(locale, key, vars?)`, and `volo.log.{info,warn,error}(msg)`.

```javascript
// 1. Register :::callout-box {type="tip"} ... ::: custom Markdown directive
volo.registerDirective("callout-box", (ctx) => {
  const kind = ctx.attrs?.type || "note";
  return `<div class="callout callout-${volo.escapeHTML(kind)}">${volo.escapeHTML(ctx.body)}</div>`;
});

// 2. Register automatic federation publish/update handlers (:::lock & protected posts redacted)
volo.registerFederation({
  publish(ev) {
    if (ev.omitted_protected) return;
    volo.kv.set(`fed:${ev.id}`, JSON.stringify({ action: ev.action, title: ev.title }), 86400);
  },
  deliver(ev) {
    volo.log.info(`federation deliver: ${ev.id}`);
  }
});

// 3. Subscribe to render hooks and render dynamic UI slots safely
volo.on("render.post_html", ({ html }) => {
  volo.kv.incr("rendered_count", 1, 0);
  return { html };
});

function renderSlot(slotName, ctx) {
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
