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
  --volo-text: #1c1e1b;
  --volo-muted: #5f665e;
  --volo-border: rgba(28, 30, 27, 0.10);
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
    ├── index.html
    └── post.html
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

---

## 2. Sandboxed Plugin Development (`manifest.json` + `goja`)

`Volo-go` has zero hardcoded plugin names in host code. Every plugin — including [`plugins/kanbanniang`](../../plugins/kanbanniang/) — runs through the declarative `pkg/plugin` permission sandbox.

### 2.1 Directory Layout & `.zip` Hot-Installation

```text
plugins/hello-callout/
├── manifest.json           # Permissions, UI slots, hooks, cron jobs & localized config_schema
├── main.js                 # Embedded pure-Go goja (ES2020+) sandbox entrypoint
└── assets/                 # Optional static assets served under /plugins/hello-callout/assets/*
    ├── widget.css
    └── widget.js
```

Administrators can either mount a directory under `./plugins/` or upload a `.zip` package directly in **Console -> Plugins** for validated hot-installation, automatic localized settings form generation, live enable/disable, or clean uninstallation.

### 2.2 `manifest.json` Reference

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
    "hooks:render.post_html",
    "slots:article.after_content",
    "http.outbound:api.github.com"
  ],
  "hooks": ["render.post_html"],
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
      "type": "string",
      "default": "Thanks for reading — subscribe via RSS!",
      "label": { "zh-CN": "提示文案", "en-US": "Banner Text" }
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

### 2.3 Permissions, Domain Hooks & UI Slots

1. **Permission Tokens**:
   - Scoped Storage: `kv` (read/write), `kv:read` (read-only `plugin_kv` with per-plugin quota enforcement);
   - Domain Hooks: `hooks:article.published`, `hooks:article.updated`, `hooks:comment.before_create`, `hooks:comment.created`, `hooks:render.post_html`;
   - UI Slots: `slots:public.head`, `slots:public.header`, `slots:article.before_content`, `slots:article.after_content`, `slots:public.footer`, `slots:editor.quick_insert`, `slots:admin.dashboard_card`;
   - HTTP & Networking: `http:routes` (custom `/plugins/<id>/api/*` endpoints), `http.outbound:<hostname>` (explicit hostname allowlist enforced with `OutboundSafeDialer` SSRF protection blocking loopback/RFC1918/cloud metadata IPs);
   - Protected Content Opt-In: `articles:read_protected` (without this explicit permission, locked posts and `:::lock` blocks are redacted before reaching the plugin).

2. **Circuit Breakers, XSS Sanitization & CSS Scoping**:
   - Every `goja` invocation runs under strict execution timeouts; repeated timeouts or panics trip the plugin's **Circuit Breaker (`CircuitOpen`)** so a misbehaving plugin can never stall reader requests;
   - `render.post_html` output is re-sanitized through `bluemonday` to prevent stored XSS;
   - Plugin CSS custom properties must use `--volo-plugin-<id>-*` inside `.volo-plugin-scope[data-plugin="<id>"]`.

### 2.4 Minimal `main.js` Example

```javascript
// Subscribe to post HTML post-processing hook
volo.on("render.post_html", function (ctx) {
  var hits = Number(volo.kv.get("rendered_count") || "0") + 1;
  volo.kv.set("rendered_count", String(hits));
  return ctx.html;
});

// Render a dynamic UI slot
function renderSlot(slotName, ctx) {
  if (slotName === "article.after_content") {
    var msg = volo.config.get("banner_text") || "Thanks for reading!";
    return '<aside class="callout-box">' + volo.escapeHTML(msg) + "</aside>";
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
