# 主题定制与插件开发指南

> English version: [Themes & Plugin Development Guide](../en-US/themes-and-plugins.md)

`Volo-go` 内置零构建依赖的现代双栏编辑部默认主题 **`volo-studio`**，同时开放清晰、无耦合的 **外部主题目录协议（`./themes/*`）** 与 **安全沙箱插件体系（`./plugins/*`）**，第三方扩展无需重新编译 Go 宿主即可热加载生效。

---

## 1. 主题架构与自定义主题开发

### 1.1 设计令牌与零冲突样式体系

默认主题 `volo-studio` 与官方外部纸刊主题 [`themes/volo-journal`](../../themes/volo-journal/) 共享一套语义化 CSS 变量令牌（支持亮色/暗色/跟随系统三态无闪烁切换，以及 `forest`、`ocean`、`teal`、`amber`、`plum`、`rose` 6 套无障碍 AA 对比度强调色）：

```css
:root {
  --volo-bg: #faf9f5;
  --volo-surface: #ffffff;
  --volo-raised: #f3f1eb;
  --volo-text: #1c1e1b;
  --volo-muted: #5f665e;
  --volo-line: rgba(28, 30, 27, 0.10);
  --volo-accent: #1f5c48;        /* 随后台外观设置动态切换 */
  --volo-accent-soft: rgba(31, 92, 72, 0.10);
  --volo-font-sans: system-ui, -apple-system, "PingFang SC", "Noto Sans SC", sans-serif;
  --volo-font-serif: "Songti SC", "Noto Serif SC", Georgia, serif;
  --volo-font-mono: ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace;
}
```

### 1.2 外部主题目录结构（`./themes/<theme-id>/`）

在 `VOLO_THEMES_DIR`（默认 `./themes`）下新建目录即可被管理台 **「外观与主题」** 自动发现并即时切换：

```text
themes/my-editorial/
├── theme.json              # 主题元信息清单（必填）
├── assets/
│   ├── theme.css           # 主题样式（通过 /themes/my-editorial/assets/theme.css 零拷贝预压缩分发）
│   └── theme.js            # 可选增强脚本
└── templates/              # 可选覆盖模板（未提供的页面自动安全回退至宿主标准视图）
    ├── layout.html
    ├── index.html
    ├── article.html
    └── page.html
```

`theme.json` 规范示例：

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

### 1.3 PJAX 跨页常驻岛协议（`data-volo-persist`）

当前台开启无刷新平滑导航（PJAX）时，主内容容器局部替换，而挂载在 `public.footer` 或 `public.header` 且声明 `persist: true` 的节点会自动包裹在常驻岛容器中——**播客迷你播放器音频流不断流、互动挂件（如看板娘）动画与状态跨页不重载**。

### 1.4 桌面/手机双主题分流与顶部导航编排能力（`sortable_list` + `.NavMenu`）

- **桌面与手机独立主题设置**：管理台 **「外观与扩展 -> 主题皮肤」** 支持为桌面端（`active_theme`）与手机端（`mobile_theme`，留空则跟随桌面主题）分别启用不同主题，服务端按请求设备特征自动分流并输出 `Vary: User-Agent, Sec-CH-UA-Mobile`；管理员可通过 `/?skin=<id>` 与 `/?skin=<id>&device=mobile` 免切换实时预览。
- **主题级顶部菜单显隐、排序与溢出收纳**：主题可在 `theme.json` 的 `config_schema` 中声明 `"type": "sortable_list"`（如内置 `volo-studio` 与外部 `volo-journal` 的 `header_nav_items` + `header_nav_primary_count`），允许站长自由勾选并拖拽/调整内置栏目（`home`、`archives`、`tags`、`categories`、`series`、`books`、`links`、`follows`、`dynamic`）及自定义导航插槽（`custom`，自动展开后台自定义的导航链接且默认排在末尾）。模板直接读取 `PageContext.NavMenu`（含 `.NavMenu.Primary` 顶栏直出项、`.NavMenu.Overflow` “更多”收纳项与 `.NavMenu.OverflowActive` 高亮态）即可渲染自适应导航栏。

### 1.5 独立页面可选模板协议（`page_templates`）

主题可在 `theme.json` 中通过 `"page_templates"` 声明供后台独立页面（Pages）选择的专属排版模板（对应渲染 `templates/page-<id>.html`，切换至不含该模板的主题时自动安全回退至 `templates/page.html`）：

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

## 2. 安全沙箱插件开发（`manifest.json` + `goja`）

`Volo-go` 彻底摒弃“在核心代码硬编码判断特定插件名”的做法。所有内置能力与外部目录插件（如 [`plugins/kanbanniang`](../../plugins/kanbanniang/)、[`plugins/sensitive-words`](../../plugins/sensitive-words/)）统一遵守声明式权限沙箱协议。

### 2.1 插件目录结构与 `.zip` 热插拔

```text
plugins/hello-callout/
├── manifest.json           # 权限、插槽、钩子、定时任务与配置表单 Schema
├── main.js                 # 纯 Go 内嵌 JSVM 沙箱入口脚本
└── assets/                 # 可选静态资源（通过 /plugins/hello-callout/assets/* 分发）
    ├── widget.css
    └── widget.js
```

管理员既可将目录直接放入 `./plugins/`，也可在管理台 **「插件扩展」** 页面直接上传 `.zip` 插件包或点击 **「重新扫描插件」** 零停机热重载、自动根据 `config_schema` 渲染多语言配置表单，并随时热启停或卸载（支持按需一并清理 `plugin_kv` 数据）。

### 2.2 `manifest.json` 完整字段与统一 `config_schema` 规范

`theme.json` 与 `manifest.json` 共享完全一致的 `config_schema` 控件类型体系：
- `string` / `text`（单行文本）、`textarea`（多行文本）
- `number`（支持可选 `min`、`max`、`step` 数值范围与步进约束）
- `boolean`（布尔开关）
- `select`（下拉选择，配 `options: [{ "value": "...", "label": { ... } }]`）
- `url`、`image`（自动安全净化，拦截 `javascript:` / `data:` 危险协议）
- `color`（`#rgb` / `#rrggbb` 颜色选择器）
- `sortable_list`（可勾选显隐并拖拽排序的列表项）

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
      "default": "感谢阅读，欢迎通过 RSS 订阅更新。",
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

> **密钥落盘加密保证**：`config_schema` 中标记 `"secret": true` 的字段在写入数据库时自动使用 `KeyRing` 进行 `AES-256-GCM` 列级加密，且在后台配置接口中自动脱敏为掩码，不会明文回显。

### 2.3 标准权限（Permissions）、钩子（Hooks）、插槽（UI Slots）与扩展 Provider

1. **权限白名单令牌**：
   - **存储**：`kv` / `kv:rw`（读写）、`kv:read`（只读 `plugin_kv`，单插件默认配额上限保护，且后台 KV 写入采用非缓存失效通道，不抖动主站文章/RSS 缓存）；
   - **宿主能力门控**：
     - `crypto`：解锁 `volo.crypto`（`sha256`、`hmacSha256`、`randomUUID`）；
     - `site:read`：解锁 `volo.site.info()` 读取站点只读元信息；
     - `ratelimit`：解锁 `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)` 滑动窗口限流器（声明 `http:routes` 时亦自动开放）；
     - `cron`：接入宿主**统一最小堆定时调度中心（`CronRegistry`）**，支持在 `manifest.json` 静态声明或通过 `volo.cron` 动态注册/注销周期任务（零任务时零后台空转唤醒，同任务非重入合并执行）；
   - **领域钩子**（支持 `hooks:*` 或细粒度令牌）：
     - 文章生命周期：`hooks:article.published`、`hooks:article.updated`、`hooks:article.deleted`；
     - 独立页面生命周期：`hooks:page.published`、`hooks:page.updated`、`hooks:page.deleted`；
     - 评论治理：`hooks:comment.before_create`、`hooks:comment.created`、`hooks:comment.moderated`；
     - 渲染管线：`hooks:render.pre_markdown`（在非加密且不含 `:::lock` 的文章进入 Markdown 解析器前改写 `content_md`）与 `hooks:render.post_html`（对渲染后的 HTML 做后处理并二次净化）；
   - **界面插槽**（支持 `ui:slots` 或按插槽授权）：`slots:public.head`、`slots:public.header`、`slots:public.sidebar`、`slots:article.before_content`、`slots:article.after_content`、`slots:comment.before`、`slots:comment.after`、`slots:public.footer`、`slots:editor.quick_insert`、`slots:admin.dashboard_card`；
   - **网络与路由**：`http:routes`（挂载 `/plugins/<id>/api/*` 自定义公开路由）、`http:raw_headers`（按需透传 `x-hub-signature-256`、`x-signature`、`stripe-signature`、`authorization` 等安全验签请求头）、`http:outbound` + `http.outbound:<hostname>` / `allowed_hosts`（显式域名白名单或插件 `config_schema` 中校验通过的 `type: "url"` 目标域名 + `OutboundSafeDialer` 防 SSRF）；
   - **核心扩展 Provider**：
     - `provider:storage`：当 `main.js` 实现 `storagePut(objectKey, bodyBase64, contentType)`（返回公开 URL 字符串或 `{ url }`）与可选 `storageDelete(objectKey)`、`storageHealthCheck()` 时自动注册外部媒体/备份对象存储驱动（`StorageProvider`）；若插件熔断或不可用则自动降级回本地磁盘不中断上传；
     - `provider:oidc`：当 `main.js` 实现 `oidcAuthURL(state, redirectURI, opts)`（`opts` 含 `{ nonce, code_challenge, code_challenge_method }`，若返回的有效 `http`/`https` URL 尚未附带 `nonce` 或 `code_challenge`，宿主会自动补齐查询参数；2 参数旧签名保持 100% 兼容）与 `oidcExchange(code, redirectURI, opts)`（`opts` 含 `{ nonce, code_verifier }`，返回 `{ subject, email, email_verified, nonce, display_name, avatar_url }`，其中 `email_verified` 缺省严格默认为 `false`），以及可选 `oidcProviderInfo()`（返回 `{ id, display_name }`）时自动注册第三方联合登录驱动（`OIDCProvider` / `OIDCAdvancedProvider`）；
     - `provider:notifier`：注册自定义通知渠道，支持导出 `sendNotification(event)` 或 `notifySend(event)`，与邮件和 Webhook 并行分发；
     - `provider:antispam`：当 `main.js` 实现 `checkSpam(input)`（返回 `{ action: "pass" | "allow" | "moderate" | "reject", reason, score }`）与可选 `checkNickname(input)` 时自动注册评论与昵称审查驱动（`AntiSpamProvider` / `NicknameAntiSpamProvider`），统一覆盖**评论正文、访客/读者评论昵称、读者邮箱注册（含发码预检且拦截不消耗验证码）及 OIDC 首次自动建号昵称**；官方插件 [`plugins/sensitive-words`](../../plugins/sensitive-words/) 基于 `houbb/sensitive-word` 6.4 万+ 分类词库与 16 字节零 GC 指针 BFS-CSR Aho-Corasick 自动机实现简繁折叠、分隔符/叠字反混淆、区间安全白名单与在线转换热更新；
   - **高敏读取**：`articles:read_protected`（未显式声明此权限的 JS 插件，即使调用方直接触发 `DispatchHook` 或 `DispatchArticleHook`，宿主也会自动清空密码保护文章正文并按 CommonMark 嵌套规则物理剥离所有 `:::lock ... :::` 内容块）。

2. **沙箱熔断、资源护栏与 XSS 隔离红线**：
   - 每次 JS Hook / Slot 执行受严格指令步数预算、墙钟超时、循环引用深度上限（`Export` / `JSON.stringify`）及 `4 MiB` 字符串体积上限保护；连续异常或超时自动打开**熔断器（`CircuitOpen`）**旁路该插件；
   - `volo.on(hookName, handlerFn)` 仅允许在模块顶层初始化阶段调用，初始化完成后自动冻结，防止请求期误调导致监听器泄漏；导出可选 `onConfigChange(newCfg)` 可在后台修改配置时零重启热刷新内存缓存与定时任务；
   - `render.post_html` 与后台仪表盘卡片 `admin.dashboard_card` 输出均经过严格白名单 HTML 净化（剥离 `<script>`、`<iframe>`、`on*` 事件属性与 `javascript:`/`data:` 协议）；
   - 插件 CSS 变量必须使用 `--volo-plugin-<id>-*` 命名空间，包裹容器自动带 `.volo-plugin-scope[data-plugin="<id>"]` 以避免污染宿主排版。

### 2.4 沙箱 SDK（`volo.*`）、ES6 标准库与 `main.js` 编写示例

内置零外部依赖 JS 虚拟机原生支持现代 **ES6+ 语法与标准库**：
- **语法特性**：函数级 `var` 提升、块级 `let`/`const`、`for (let ...)` 每轮独立闭包绑定、模板字符串 `${...}`、可选链（`obj?.prop`、`obj?.[expr]`、`fn?.(args)`）、空值合并 `??`、箭头函数（词法 `this`）、默认参数与剩余参数 `(...args)`、数组/调用/对象展开 `...x`、对象属性简写与方法简写 `{ a, method() {} }`、以及数组与对象解构赋值（`const { a, b: renamed = 1 } = obj`、`const [x, y = 2] = arr`）；
- **标准内置对象**：符合 RFC 3986 的 `encodeURIComponent` / `decodeURIComponent`、`Number`（`isNaN`、`isFinite`、`isInteger`、`parseInt`、`parseFloat`、`toFixed`、`toString(radix)`）、`Array`（`from`、`isArray`、`concat`、`map`、`filter`、`reduce`、`find`、`findIndex`、`some`、`every`、`sort`、`reverse`、`shift`、`unshift`、`splice`、`flat`、`flatMap`）、`Object`（`keys`、`values`、`entries`、`fromEntries`、`assign`、`hasOwn`、`Object.prototype.hasOwnProperty`）、`String`（`fromCharCode`、`padStart`、`padEnd`、`trimStart`、`trimEnd`、`lastIndexOf`、支持 `RegExp` 与回调函数的 `replace` / `replaceAll`）、`Function.prototype.{call, apply, bind}`、`RegExp`、`Date`、`JSON`、`Math` 及 `btoa` / `atob`。

宿主桥接对象 `volo`（别名 `ctx`）：
- `volo.on(hookName, handlerFn)`（顶层初始化期注册，或导出全局 `function onHook(hookName, payload)`）：订阅已声明的领域/渲染钩子；
- `volo.escapeHTML(str)`：对不可信文本做 HTML 实体转义，防止动态插槽拼接引入 XSS；
- `volo.config.get(key)` / `volo.config.all()`：读取校验后的插件配置（含沙箱内自动解密的 `secret` 字段）；
- `volo.kv.get(key)` / `volo.kv.getInt(key)` / `volo.kv.set(key, value, ttlSeconds?)` / `volo.kv.del(key)` / `volo.kv.incr(key, delta?, ttlSeconds?)` / `volo.kv.list(prefix, limit?, afterKey?)`：需声明 `kv` / `kv:read` 权限，支持 `afterKey` 游标翻页；
- `volo.assets.readText(relPath)`：只读读取受限于插件目录内的随包文本或 `.gz` 压缩文件（防符号链接越界，上限 `4 MiB`）；
- `volo.wordFilter.{compile, match, contains, has, stats, clear, revision, syncRemote}`：跨 VM 池共享的 Go 原生 16B/状态零 GC 指针 BFS-CSR Aho-Corasick 词库引擎，支持直接流式编译随包 `.gz` 词表与在线同步转换上游 `houbb` 词库而无需在 JS 堆分配大字符串；
- `volo.cron.{register, unregister, trigger, list}`：需声明 `cron` 权限，在统一 `CronRegistry` 中按需动态注册、注销、手动触发或查询周期任务；
- `volo.crypto.sha256(str)` / `volo.crypto.hmacSha256(secret, msg)` / `volo.crypto.randomUUID()`：需声明 `crypto` 权限；
- `volo.site.info()`：需声明 `site:read` 权限，返回站点名称、URL、默认语言等只读摘要；
- `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)`：需声明 `ratelimit` 或 `http:routes` 权限；
- `volo.http.fetch(url, options?)`：受 `allowed_hosts` / `http.outbound:<host>` / `type: "url"` 白名单与 `OutboundSafeDialer` 保护的出站 HTTP 请求；
- `volo.i18n.t(locale, key, vars?)` 与 `volo.log.info(msg)` / `volo.log.warn(msg)` / `volo.log.error(msg)`：多语言翻译与结构化插件日志。

```javascript
// 通过 volo.on(...) 在顶层初始化阶段订阅 Markdown 预处理与 HTML 后处理钩子
volo.on("render.pre_markdown", (article) => ({
  content_md: article.content_md.replace(/\[toc\]/gi, () => "## 目录\n")
}));

volo.on("render.post_html", ({ html }) => {
  volo.kv.incr("rendered_count", 1, 0);
  return { html };
});

// 使用 afterKey 游标翻页遍历插件 KV
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

// 使用 volo.escapeHTML 安全渲染动态 UI 插槽
function renderSlot(slotName, ctx) {
  if (slotName === "article.after_content") {
    const msg = volo.config.get("banner_text") || "感谢阅读！";
    return `<aside class="callout-box">${volo.escapeHTML(String(msg))}</aside>`;
  }
  return "";
}
```

---

## 3. 外部 Webhook 通知与 HMAC 验签协议

除进程内 `goja` 插件外，管理台 **「通知中心 -> Webhooks」** 原生支持向飞书、Telegram、Discord、Slack、Bark、企业微信及任意自定义 HTTP 服务推送 `article.published`、`comment.created` 等事件。

当为自定义 Webhook 配置了签名密钥（`Secret`）时，`Volo-go` 会在请求头附带标准 `HMAC-SHA256` 签名供接收端验签防伪造：

- `X-Volo-Event`: 事件名称（如 `article.published`、`comment.created`、`webhook.test`）
- `X-Volo-Timestamp`: Unix 秒级时间戳
- `X-Volo-Signature`: `sha256=<hex(HMAC_SHA256(secret, timestamp + "." + raw_json_body))>`

Node.js / Python / Go 接收端只需使用恒定时间比较（`crypto.timingSafeEqual` / `hmac.Equal`）校验 `X-Volo-Signature`，并拒绝时间戳偏差超过 5 分钟的重放请求即可。
