# Themes & Plugin Development Guide

> 中文版本：[主题定制与插件开发指南](../zh-CN/themes-and-plugins.md)

`Volo-go` ships with a built-in zero-framework editorial theme (**`volo-studio`**) while exposing a clean **external theme directory contract (`./themes/*`)** and a **permission-sandboxed plugin system (`./plugins/*`)** that hot-loads without recompiling the Go host binary.

---

## 1. Theme Architecture & Custom Themes

### 1.1 Semantic CSS Design Tokens

Both built-in `volo-studio` and the external reference theme [`themes/volo-journal`](../../themes/volo-journal/) share semantic CSS custom properties with flash-free tri-state Dark/Light/System switching and 6 WCAG AA accent palettes (`forest`, `ocean`, `teal`, `amber`, `plum`, `rose`):

```css
:root {
  --volo-bg: #faf9f5;
  --volo-surface: #ffffff;
  --volo-raised: #f3f1eb;
  --volo-text: #1c1e1b;
  --volo-muted: #5f665e;
  --volo-line: rgba(28, 30, 27, 0.10);
  --volo-accent: #1f5c48;        /* Dynamically driven by Console Appearance settings */
  --volo-accent-soft: rgba(31, 92, 72, 0.10);
  --volo-font-sans: system-ui, -apple-system, "PingFang SC", "Noto Sans SC", sans-serif;
  --volo-font-serif: "Songti SC", "Noto Serif SC", Georgia, serif;
  --volo-font-mono: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
}
```

### 1.2 External Theme Layout (`./themes/<theme-id>/`)

Drop a folder under `VOLO_THEMES_DIR` (default `./themes`) and select it live in **Console -> Appearance & Themes**:

```text
themes/my-editorial/
├── theme.json              # Required theme manifest
├── assets/
│   ├── theme.css           # Served zero-copy at /themes/my-editorial/assets/theme.css
│   └── theme.js            # Optional progressive enhancement script
└── templates/              # Optional template overrides (missing views safely fall back to host defaults)
    ├── layout.html
    ├── index.html
    ├── article.html
    └── page.html
```

Example `theme.json`:

```json
{
  "id": "my-editorial",
  "name": {
    "zh-CN": "极简专栏",
    "en-US": "Minimal Editorial"
  },
  "version": "1.0.0",
  "author": "Your Name",
  "description": {
    "zh-CN": "专注长文阅读的单栏排版主题。",
    "en-US": "Single-column long-form reading theme."
  },
  "accents": ["forest", "ocean", "teal", "amber", "plum", "rose"],
  "supports_dark_mode": true,
  "supports_pjax": true
}
```

### 1.3 Persistent PJAX Islands (`data-volo-persist`)

During smooth PJAX page transitions, the main article viewport swaps while widgets mounted in `public.footer` or `public.header` with `persist: true` remain alive inside persistent island containers — keeping **podcast audio playback uninterrupted** and **interactive companions (e.g., `kanbanniang`) stateful across navigations**.

### 1.4 Separate Desktop & Mobile Themes and Header Navigation Ordering (`sortable_list` + `.NavMenu`)

- **Independent Desktop & Mobile Theme Activation**: **Console -> Appearance & Extensions -> Themes** allows enabling one theme for Desktop (`active_theme`) and an optional dedicated theme for Mobile (`mobile_theme`, inheriting Desktop when unset) with cache-safe `Vary: User-Agent, Sec-CH-UA-Mobile` negotiation. Administrators can preview either mode live via `/?skin=<id>` and `/?skin=<id>&device=mobile`.
- **Theme-Level Top Menu Ordering & Overflow**: Themes can declare `"type": "sortable_list"` fields in `theme.json` `config_schema` (such as `header_nav_items` and `header_nav_primary_count` in `volo-studio` and `volo-journal`) so site owners can toggle visibility and reorder built-in routes (`home`, `archives`, `tags`, `categories`, `series`, `books`, `links`, `follows`, `dynamic`) alongside the `custom` slot (which expands custom links from **Distribute -> Navigation** and defaults to the last position). Templates consume `PageContext.NavMenu` (`.NavMenu.Primary`, `.NavMenu.Overflow`, `.NavMenu.OverflowActive`, and `.NavMenu.Items`) directly without manual slice math.

### 1.5 Custom Standalone Page Templates (`page_templates`)

Themes can optionally declare selectable standalone page layouts in `theme.json` via `"page_templates"` (rendered from `templates/page-<id>.html`, automatically falling back to `templates/page.html` if a theme is switched):

```json
{
  "page_templates": [
    {
      "id": "wide",
      "name": { "zh-CN": "宽幅画廊排版", "en-US": "Wide Showcase" },
      "description": { "zh-CN": "无侧栏全宽画布页面", "en-US": "Full-width layout without sidebar" }
    }
  ]
}
```

---

## 2. Sandboxed Plugin Development (`manifest.json` + `goja`)

`Volo-go` has zero hardcoded plugin names in host code. Every plugin — including [`plugins/kanbanniang`](../../plugins/kanbanniang/) and [`plugins/sensitive-words`](../../plugins/sensitive-words/) — runs through the declarative `pkg/plugin` permission sandbox.

### 2.1 Directory Layout & `.zip` Hot-Installation

```text
plugins/hello-callout/
├── manifest.json           # Permissions, UI slots, hooks, cron jobs & localized config_schema
├── main.js                 # Embedded pure-Go JSVM sandbox entrypoint
└── assets/                 # Optional static assets served under /plugins/hello-callout/assets/*
    ├── widget.css
    └── widget.js
```

Administrators can either mount a directory under `./plugins/`, upload a `.zip` package directly in **Console -> Plugins**, or trigger **Rescan Plugins** for zero-downtime hot-reload, automatic localized settings form generation, live enable/disable, or clean uninstallation (with optional `plugin_kv` data purge).

### 2.2 `manifest.json` Reference & Unified `config_schema`

Both `theme.json` and `manifest.json` share a unified `config_schema` type system:
- `string` / `text` (single-line text), `textarea` (multi-line text)
- `number` (supports optional `min`, `max`, `step` constraints)
- `boolean` (toggle switch)
- `select` (dropdown with `options: [{ "value": "...", "label": { ... } }]`)
- `url`, `image` (automatically sanitized to reject `javascript:` / `data:` URLs)
- `color` (`#rgb` / `#rrggbb` color picker)
- `sortable_list` (reorderable + toggleable item array)

```json
{
  "id": "hello-callout",
  "version": "1.0.0",
  "min_host_version": "0.1.0",
  "runtime": "goja",
  "entry": "main.js",
  "author": "Volo Developer",
  "homepage": "https://example.com",
  "name": {
    "zh-CN": "文末寄语卡片",
    "en-US": "Post Callout Card"
  },
  "description": {
    "zh-CN": "在文章正文末尾自动注入可配置的定制署名提示。",
    "en-US": "Appends a customizable callout card after article content."
  },
  "permissions": [
    "kv",
    "site:read",
    "crypto",
    "ratelimit",
    "hooks:render.pre_markdown",
    "hooks:render.post_html",
    "slots:article.after_content",
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
      "default": "Thanks for reading — subscribe via RSS!",
      "label": { "zh-CN": "提示文案", "en-US": "Banner Text" }
    },
    {
      "key": "accent_color",
      "type": "color",
      "default": "#1f5c48",
      "label": { "zh-CN": "卡片强调色", "en-US": "Accent Color" }
    },
    {
      "key": "max_items",
      "type": "number",
      "default": 5,
      "min": 1,
      "max": 20,
      "step": 1,
      "label": { "zh-CN": "最大条数", "en-US": "Max Items" }
    },
    {
      "key": "api_secret",
      "type": "string",
      "secret": true,
      "label": { "zh-CN": "第三方密钥", "en-US": "API Secret" }
    }
  ]
}
```

> **Encrypted Secret Fields**: Any `config_schema` field marked `"secret": true` is sealed at rest via `AES-256-GCM` (`KeyRing`) and masked in admin API responses.

### 2.3 Permissions, Domain Hooks, UI Slots & Provider Adapters

1. **Permission Tokens**:
   - **Scoped Storage**: `kv` / `kv:rw` (read/write), `kv:read` (read-only `plugin_kv` with per-plugin quota enforcement and background non-cache-invalidating writes);
   - **Host Capability Gates**:
     - `crypto`: Unlocks `volo.crypto` (`sha256`, `hmacSha256`, `randomUUID`);
     - `site:read`: Unlocks `volo.site.info()` for read-only site metadata;
     - `ratelimit`: Unlocks `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)` sliding-window rate limiting (also enabled automatically when `http:routes` is declared);
     - `cron`: Connects the plugin to the host's **unified min-heap scheduled task registry (`CronRegistry`)** via static `manifest.json` `cron` entries or dynamic `volo.cron` registration (zero idle timer wakeups when no jobs are registered, non-reentrant coalesced execution);
   - **Domain Hooks** (`hooks:*` or fine-grained tokens):
     - Articles: `hooks:article.published`, `hooks:article.updated`, `hooks:article.deleted`;
     - Pages: `hooks:page.published`, `hooks:page.updated`, `hooks:page.deleted`;
     - Comments: `hooks:comment.before_create`, `hooks:comment.created`, `hooks:comment.moderated`;
     - Rendering Pipeline: `hooks:render.pre_markdown` (transforms `content_md` on unlocked articles without `:::lock` blocks prior to Markdown parsing) and `hooks:render.post_html` (transforms rendered HTML prior to XSS sanitization);
   - **UI Slots** (`ui:slots` or per-slot tokens): `slots:public.head`, `slots:public.header`, `slots:public.sidebar`, `slots:article.before_content`, `slots:article.after_content`, `slots:comment.before`, `slots:comment.after`, `slots:public.footer`, `slots:editor.quick_insert`, `slots:admin.dashboard_card`;
   - **HTTP & Networking**: `http:routes` (custom `/plugins/<id>/api/*` endpoints), `http:raw_headers` (exposes safe webhook verification headers such as `x-hub-signature-256`, `x-signature`, `stripe-signature`, `authorization`), `http:outbound` + `http.outbound:<hostname>` / `allowed_hosts` (explicit hostname allowlist or validated `type: "url"` config target hostnames enforced with `OutboundSafeDialer` SSRF protection);
   - **Extension Providers**:
     - `provider:storage`: Registers an external media/backup `StorageProvider` when `main.js` defines `storagePut(objectKey, bodyBase64, contentType)` (returning a public URL string or `{ url }`), plus optional `storageDelete(objectKey)` and `storageHealthCheck()`. Uploads gracefully fall back to local disk if an external provider is unavailable;
     - `provider:oidc`: Registers a third-party `OIDCProvider` / `OIDCAdvancedProvider` for federated sign-in when `main.js` defines `oidcAuthURL(state, redirectURI, opts)` (`opts`: `{ nonce, code_challenge, code_challenge_method }`; missing `nonce` or `code_challenge` query parameters are automatically appended to valid `http`/`https` URLs, and 2-argument JS functions remain 100% compatible) and `oidcExchange(code, redirectURI, opts)` (`opts`: `{ nonce, code_verifier }`, returning `{ subject, email, email_verified, nonce, display_name, avatar_url }` with `email_verified` strictly defaulting to `false` when absent), plus optional `oidcProviderInfo()` returning `{ id, display_name }`;
     - `provider:notifier`: Registers a `NotifierProvider` invoking `sendNotification(event)` or `notifySend(event)` alongside SMTP and outbound webhooks;
     - `provider:antispam`: Registers a comment & nickname `AntiSpamProvider` / `NicknameAntiSpamProvider` when `main.js` defines `checkSpam(input)` (returning `{ action: "pass" | "allow" | "moderate" | "reject", reason, score }`) and optional `checkNickname(input)`, uniformly screening **comment bodies, guest/reader comment nicknames, reader email registration (including send-code preflight without consuming verification codes on rejection), and OIDC auto-provisioned handles**; the official [`plugins/sensitive-words`](../../plugins/sensitive-words/) plugin bundles a sanitized 64k+ categorized `houbb/sensitive-word` dictionary compiled into a 16-byte/state zero-GC-pointer BFS-CSR Aho-Corasick automaton with Traditional-to-Simplified folding, anti-obfuscation piercing, span-safe allowlists, and online feed conversion;
   - **Protected Content Opt-In**: `articles:read_protected` (without this explicit permission, password-protected article payloads and CommonMark-nested `:::lock ... :::` blocks are automatically stripped by both `DispatchHook` and `DispatchArticleHook` before reaching any JS plugin).

2. **Circuit Breakers, Resource Caps, XSS Sanitization & CSS Scoping**:
   - Every sandbox invocation runs under strict step budgets, wall-clock timeouts, cycle/depth caps (`Export` / `JSON.stringify`), and a `4 MiB` string allocation ceiling; repeated failures trip the plugin's **Circuit Breaker (`CircuitOpen`)** so a misbehaving plugin never stalls reader traffic;
   - `volo.on(hookName, handlerFn)` is frozen once top-level module evaluation completes, rejecting request-time registrations so listeners can never leak across requests; exporting optional `onConfigChange(newCfg)` allows zero-restart hot-reloading of cached matchers and scheduled tasks when configuration is updated;
   - Both `render.post_html` and `admin.dashboard_card` outputs are sanitized against `<script>`, `<iframe>`, `on*` event attributes, and `javascript:`/`data:` URLs;
   - Plugin CSS custom properties must use `--volo-plugin-<id>-*` inside `.volo-plugin-scope[data-plugin="<id>"]`.

### 2.4 Sandbox SDK (`volo.*`), ES6 Stdlib & `main.js` Example

The zero-dependency embedded JS VM natively supports modern **ES6+ syntax and standard library built-ins**:
- **Language Semantics & ES6 Syntax**: Function/module-root `var` hoisting, block-scoped `let`/`const`, per-iteration `for (let ...)` closure bindings, template literals `${...}`, optional chaining (`obj?.prop`, `obj?.[expr]`, `fn?.(args)`), nullish coalescing `??`, arrow functions (lexical `this`), default & rest parameters `(...args)`, array/call/object spread `...x`, object property & method shorthand `{ a, method() {} }`, and array/object destructuring (`const { a, b: renamed = 1 } = obj`, `const [x, y = 2] = arr`);
- **Built-in Standard Library**: RFC 3986 `encodeURIComponent` / `decodeURIComponent`, `Number` (`isNaN`, `isFinite`, `isInteger`, `parseInt`, `parseFloat`, `toFixed`, `toString(radix)`), `Array` (`from`, `isArray`, `concat`, `map`, `filter`, `reduce`, `find`, `findIndex`, `some`, `every`, `sort`, `reverse`, `shift`, `unshift`, `splice`, `flat`, `flatMap`), `Object` (`keys`, `values`, `entries`, `fromEntries`, `assign`, `hasOwn`, `Object.prototype.hasOwnProperty`), `String` (`fromCharCode`, `padStart`, `padEnd`, `trimStart`, `trimEnd`, `lastIndexOf`, and `replace` / `replaceAll` with both `RegExp` and callback replacer functions), `Function.prototype.{call, apply, bind}`, `RegExp`, `Date`, `JSON`, `Math`, and `btoa` / `atob`.

Host capability bridge `volo` (alias `ctx`):
- `volo.on(hookName, handlerFn)` (registered during top-level initialization, or via global `function onHook(hookName, payload)`): Subscribe to declared hooks;
- `volo.escapeHTML(str)`: Safely escape untrusted strings before interpolating into slot HTML;
- `volo.config.get(key)` / `volo.config.all()`: Read validated plugin configuration (including decrypted secrets inside the sandbox);
- `volo.kv.get(key)` / `volo.kv.getInt(key)` / `volo.kv.set(key, value, ttlSeconds?)` / `volo.kv.del(key)` / `volo.kv.incr(key, delta?, ttlSeconds?)` / `volo.kv.list(prefix, limit?, afterKey?)`: Scoped KV storage (requires `kv` / `kv:read`) with cursor pagination via `afterKey`;
- `volo.assets.readText(relPath)`: Read-only access to bundled text or `.gz` files confined strictly inside the plugin directory (symlink-escape safe, `4 MiB` cap);
- `volo.wordFilter.{compile, match, contains, has, stats, clear, revision, syncRemote}`: Cross-VM shared Go-native 16B/state zero-GC-pointer BFS-CSR Aho-Corasick word filter supporting direct streaming compilation of bundled `.gz` dictionaries and online conversion of upstream `houbb` feeds without allocating large JS strings;
- `volo.cron.{register, unregister, trigger, list}`: Dynamic periodic task management backed by the unified `CronRegistry` (requires `cron`);
- `volo.crypto.sha256(str)` / `volo.crypto.hmacSha256(secret, msg)` / `volo.crypto.randomUUID()`: Cryptographic helpers (requires `crypto`);
- `volo.site.info()`: Read-only site metadata snapshot (requires `site:read`);
- `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)`: Sliding-window rate limiter (requires `ratelimit` or `http:routes`);
- `volo.http.fetch(url, options?)`: SSRF-guarded outbound HTTP client restricted to declared `http.outbound:<host>` / `allowed_hosts` / `type: "url"` targets;
- `volo.i18n.t(locale, key, vars?)` and `volo.log.info(msg)` / `volo.log.warn(msg)` / `volo.log.error(msg)`: Localization lookup and structured plugin diagnostics.

```javascript
// Subscribe to pre-Markdown and post-HTML hooks at top-level module init
volo.on("render.pre_markdown", (article) => ({
  content_md: article.content_md.replace(/\[toc\]/gi, () => "## Table of Contents\n")
}));

volo.on("render.post_html", ({ html }) => {
  volo.kv.incr("rendered_count", 1, 0);
  return { html };
});

// Cursor pagination over plugin KV keys
function listAllBadges() {
  const out = [];
  let cursor = "";
  while (true) {
    const batch = volo.kv.list("badge:", 100, cursor);
    if (!batch?.length) break;
    out.push(...batch);
    cursor = batch[batch.length - 1].key;
    if (batch.length < 100) break;
  }
  return out;
}

// Render a dynamic UI slot safely with volo.escapeHTML
function renderSlot(slotName, ctx) {
  if (slotName === "article.after_content") {
    const msg = volo.config.get("banner_text") || "Thanks for reading!";
    return `<aside class="callout-box">${volo.escapeHTML(String(msg))}</aside>`;
  }
  return "";
}
```

---

## 3. Outbound Webhooks & `HMAC-SHA256` Signature Verification

In addition to in-process `goja` plugins, **Console -> Notifications -> Webhooks** delivers `article.published`, `comment.created`, and test events to Feishu/Lark, Telegram, Discord, Slack, Bark, WeCom, or custom HTTP endpoints.

When a signing `Secret` is configured on a webhook, `Volo-go` attaches cryptographic headers for verification:

- `X-Volo-Event`: Event type (e.g. `article.published`, `comment.created`, `webhook.test`)
- `X-Volo-Timestamp`: Unix timestamp (seconds)
- `X-Volo-Signature`: `sha256=<hex(HMAC_SHA256(secret, timestamp + "." + raw_json_body))>`

Receivers should compare `X-Volo-Signature` in constant time (`crypto.timingSafeEqual` / `hmac.Equal`) and reject timestamps older than 5 minutes to prevent replay attacks.
