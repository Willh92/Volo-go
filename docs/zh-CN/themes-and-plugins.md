# 主题定制、三模态编辑器与插件开发指南

> English version: [Themes, Studio Editor & Plugin Development Guide](../en-US/themes-and-plugins.md)

`Volo-go` 内置零构建依赖的现代双栏编辑部默认主题 **`volo-studio`** 与三模态创作工作台编辑器，同时开放清晰、无耦合的 **外部主题目录协议（`./themes/*`）** 与 **安全沙箱插件体系（`./plugins/*`）**，第三方扩展无需重新编译 Go 宿主即可热加载生效。

---

## 1. 主题架构与自定义主题开发

### 1.1 设计令牌与零冲突样式体系

默认主题 `volo-studio` 与官方外部纸刊主题 [`themes/volo-journal`](../../themes/volo-journal/) 共享一套语义化 CSS 变量令牌（支持亮色/暗色/柔和暖色/跟随系统无闪烁切换，以及 `forest`、`ocean`、`teal`、`amber`、`plum`、`rose` 6 套无障碍 AA 对比度强调色）：

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

### 1.2 外部主题目录结构、脚手架 `.zip` 导出与原位覆盖升级

在 `VOLO_THEMES_DIR`（默认 `./themes`）下新建目录，或在管理工作台（默认 `/console`，支持通过 `security.admin_path` 热切换动态隐身路径）**「外观与扩展 -> 主题皮肤」** 上传 `.zip` 包，即可自动发现并即时切换：

```text
themes/my-editorial/
├── theme.json              # 主题元信息清单（必填）
├── preview.svg             # 后台画廊预览缩略图（可选，支持 .svg / .png / .webp）
├── locales/                # 主题专属多语言词典（可选，通过 {{tt .ThemeID .Locale "key"}} 读取）
│   ├── zh-CN.json
│   └── en-US.json
├── assets/
│   ├── theme.css           # 主题样式（通过 {{assetURL .ThemeID "theme.css"}} 带指纹与预压缩分发）
│   └── theme.js            # 可选增强脚本
└── templates/              # 可选覆盖模板（未提供的页面自动安全回退至宿主标准视图）
    ├── layout.html
    ├── index.html
    ├── article.html
    ├── page.html
    ├── page-wide.html
    └── error.html
```

- **一键纯净脚手架 `.zip` 导出**：管理员可在后台点击任一主题（含内置 `volo-studio`）的 **「导出主题包 (.zip)」**（`GET /api/admin/themes/{id}/export`），直接下载自动剥离临时文件与系统噪音的纯净 Starter `.zip` 包作为二次开发起点。
- **原位 `.zip` 覆盖升级**：上传主题时支持 `POST /api/admin/themes/install?overwrite=true` 原位热更新现有同 ID 主题文件，同时完整保留站长已保存的 `Appearance.ThemeConfigs[<id>]` 配置项。

### 1.3 PJAX 跨页常驻岛协议（`data-volo-persist`）

当前台开启无刷新平滑导航（PJAX）时，主内容容器局部替换，而挂载在 `public.footer` 或 `public.header` 且声明 `persist: true` 的节点会自动包裹在常驻岛容器中——**播客迷你播放器音频流不断流、互动挂件（如看板娘）动画与状态跨页不重载**。

### 1.4 桌面/手机双主题分流与顶部导航编排能力（`sortable_list` + `.NavMenu`）

- **桌面与手机独立主题设置**：管理台 **「外观与扩展 -> 主题皮肤」** 支持为桌面端（`active_theme`）与手机端（`mobile_theme`，留空则跟随桌面主题）分别启用不同主题，服务端按请求设备特征自动分流并输出 `Vary: User-Agent, Sec-CH-UA-Mobile`；管理员可通过 `/?skin=<id>` 与 `/?skin=<id>&device=mobile` 免切换实时预览。
- **主题级顶部菜单显隐、排序与溢出收纳**：主题可在 `theme.json` 的 `config_schema` 中声明 `"type": "sortable_list"`（如 `header_nav_items` + `header_nav_primary_count`），允许站长自由勾选并拖拽排序内置栏目（`home`、`archives`、`tags`、`categories`、`series`、`books`、`links`、`follows`、`dynamic`）及自定义导航项（`custom`）。模板直接读取 `PageContext.NavMenu`（`.Primary`、`.Overflow`、`.OverflowActive`）即可渲染自适应导航栏。

### 1.5 主题高阶扩展能力（`page_templates`、`{{customVar}}`、`Reading.ListStyle` 与无框 `error.html`）

1. **独立页面可选模板协议（`page_templates`）**：
   主题可在 `theme.json` 中通过 `"page_templates"` 声明供后台独立页面（Pages）选择的专属排版版式（渲染 `templates/page-<value>.html`，若切换至不含该文件的主题则自动安全回退至 `templates/page.html`）：
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
2. **自定义站点模板变量（`{{customVar}}` / `Site.CustomVars`）**：
   站长可在后台 **「站点设置 -> 自定义模板变量」**（`site.custom_vars`）中维护任意键值字典（如备案号、社交链接、横幅口号，亦支持从旧版 Bolo-Solo `customVars` 无损导入），模板中无需修改 `theme.json` 即可直接调用：
   ```gotmpl
   {{customVar .Site.CustomVars "icp_number" ""}}
   ```
3. **文章列表呈现风格（`Reading.ListStyle`）**：
   后台 **「阅读与排版」** 支持三种列表呈现模式 `reading.list_style`：`summary`（摘要卡片，默认）、`title_only`（纯标题极简索引）、`full`（时间流直接展开全文）。当设为 `"full"` 时，服务端会为公开 `.ArticleCards` 自动填充安全净化的 `.FullHTML`（密码保护文章与未解锁的 `:::lock ... :::` 内容块仍由 `VisibilityGuard` 严格拦截脱敏，绝不泄露）。
4. **独立无框全屏 `error.html` 覆写（Standalone Frameless Override）**：
   即便主题启用 `templates/layout.html` 母版继承，`templates/error.html` 依然可通过重定义 `{{define "layout"}}<!doctype html>...</html>{{end}}{{define "content"}}{{end}}`（或省略 `"content"` 定义直接编写完整 HTML 文档），完全脱离常规顶栏与侧栏框架，渲染沉浸式全屏 `404` / `403` / `429` / `500` 错误画布。

完整的旧版 FreeMarker (`.ftl`) 皮肤迁移对照表请参阅 [《主题开发与旧版 Bolo-Solo 皮肤迁移指南》](theme-migration.md)。

---

## 2. 三模态创作编辑器（3-Mode Studio Editor）

管理工作台内置基于 CodeMirror 6 深度定制的三模态创作编辑器，兼顾纯正 Markdown 掌控力与所见即所得流式体验：

- **IR 即时渲染模式（Instant Rendering）**：光标所在行展开源码微调，离开即原地渲染标题、引用、表格、数学公式与媒体组件；内置**长图加载滚动锚定防抖（Tall-Image Scroll Stabilization）**防止高分辨率图片异步撑开视口导致编辑位跳动，并完整支持反引号 ```` ``` ```` 与波浪线 `~~~` 围栏代码块。
- **WYSIWYG 富文本往返模式（Rich Round-Trip）**：可视化富文本编辑与无损 Markdown 双向序列化，专为 `<audio>` 与 `<video>` 媒体块注入前后光标着陆区（Cursor Landing Pads），彻底消除嵌入式音视频块首尾无法落光标或删除的痛点。
- **SV 分屏对照模式（Split View）**：左侧源码编辑与右侧实时排版预览同步滚动对照。
- **原生多媒体、LaTeX 公式保护与语音速记**：
  - 原生支持 `<video>` / `<audio>` 上传嵌入与播客外链，拖拽/粘贴图片自动上传；
  - **LaTeX 数学公式保护**：渲染管线自动保护行内 `$...$` 与块级 `$$...$$` 公式免受 Markdown 下划线 `_` / 星号 `*` 误解析破坏，配合前台按需懒加载 KaTeX 与 Mermaid；
  - 内置浏览器端**语音录制（Voice Recording）**直传媒体库并一键插入音频播放器标签，且开放 `slots:editor.quick_insert` 供插件注册自定义快捷指令按钮（如一键插入 `:::callout` 容器块）。

---

## 3. 安全沙箱插件开发（`manifest.json` + 纯 Go ES6 JSVM）

`Volo-go` 彻底摒弃“在核心代码硬编码判断特定插件名”的做法。所有内置能力与外部目录插件（如 [`plugins/kanbanniang`](../../plugins/kanbanniang/)）统一遵守声明式权限沙箱协议。

### 3.1 插件目录结构、`.zip` 导出与原位覆盖升级

```text
plugins/hello-callout/
├── manifest.json           # 权限、插槽、钩子、定时任务与配置表单 Schema
├── main.js                 # 纯 Go 内嵌 ES6 JSVM 沙箱入口脚本
└── assets/                 # 可选静态资源（通过 /plugins/hello-callout/assets/* 分发）
    ├── widget.css
    └── widget.js
```

- **热安装与原位升级（`overwrite=true`）**：管理员既可将目录放入 `./plugins/` 点击 **「重新扫描插件」**，也可通过后台或 `POST /api/admin/plugins/install?overwrite=true` 上传 `.zip` 包完成零停机安装或原位覆盖升级——**原位升级时自动保留已有 `config_json` 与加密的 `secret_ciphertext` 密钥**，无需重复录入 API Token。
- **纯净 `.zip` 导出**：通过后台按钮或 `GET /api/admin/plugins/{id}/export` 可随时将已安装插件导出为干净的 `.zip` 归档包（自动剔除运行态临时文件，且绝不导出数据库中的加密密钥密文）。

### 3.2 `manifest.json` 完整字段、`type: "secret"` 加密配置与统一 `config_schema`

`theme.json` 与 `manifest.json` 共享一致的 `config_schema` 控件类型体系：
- `string` / `text`（单行文本）、`textarea`（多行文本）
- `secret`（或任意字符串字段附加 `"secret": true`：密码掩码输入框，落盘 `AES-256-GCM` 强加密）
- `number`（支持可选 `min`、`max`、`step` 数值范围与步进约束）
- `boolean`（布尔开关）
- `select`（下拉选择，配 `options: [{ "value": "...", "label": { ... } }]`）
- `url`、`image`（自动安全净化，拦截 `javascript:` / `data:` 危险协议）
- `color`（`#rgb` / `#rrggbb` 颜色选择器）
- `sortable_list`（可勾选显隐并拖拽排序的列表项）

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
      "key": "api_secret",
      "type": "secret",
      "label": { "zh-CN": "第三方密钥", "en-US": "API Secret" }
    }
  ]
}
```

> **`type: "secret"` 密钥落盘加密保证**：`config_schema` 中声明 `"type": "secret"`（或 `"secret": true`）的字段在写入数据库时自动使用宿主 `KeyRing` 进行 `AES-256-GCM` 独立列加密（`plugins.secret_ciphertext`，绑定 `AAD = plugins.secret_ciphertext:<plugin_id>`）。后台管理 API 仅返回布尔掩码映射 `secret_keys_set`，绝不回显明文；若需清空已保存密钥，可传入哨兵值 `"__VOLO_CLEAR_SECRET__"`。

### 3.3 标准权限（Permissions）、自定义指令、自动联邦分发与扩展 Provider

1. **权限白名单令牌**：
   - **存储**：`kv` / `kv:rw`（读写）、`kv:read`（只读 `plugin_kv`，单插件默认配额上限保护，且后台 KV 写入采用独立通道，不抖动主站文章/RSS 页面缓存）；
   - **宿主能力门控**：
     - `crypto`：解锁 `volo.crypto`（`sha256`、`hmacSha256`、`randomUUID`）；
     - `site:read`：解锁 `volo.site.info()` 读取站点只读元信息；
     - `ratelimit`：解锁 `volo.rateLimit(key, max, windowSec?)` / `volo.rateLimit.allow(key, max, windowSec?)` 滑动窗口限流器（声明 `http:routes` 时亦自动开放）；
     - `cron`：允许声明 `cron` 周期后台任务；
   - **自定义 Markdown 指令扩展（`render` + `volo.registerDirective`）**：
     - 声明 `"render"`（或 `"hooks:render.*"` / `"hooks:render.directive"`）权限后，插件可在模块顶层初始化阶段调用 `volo.registerDirective(name, handlerFn)` 注册自定义 `:::name {key="val"} ... :::` 块级指令；
     - 宿主传入 `{ name, attrs, body }` 上下文并对返回的 HTML 自动执行 `bluemonday` 白名单净化；
     - **系统保留指令红线**：内置局部加密指令 `:::lock` 由宿主物理保留，任何插件均无法劫持或读取未解锁的 `:::lock` 块内容；连续超时或抛错会自动触发插件熔断器（`CIRCUIT_OPEN`）；
   - **自动联邦网络异步分发（`federation` / `provider:federation`）**：
     - 声明 `"federation"`（或 `"federation:*"` / `"provider:federation"`）权限并调用 `volo.registerFederation({ publish(ev), deliver(ev) })` 后，每当公开文章**发布或更新**时，宿主会在后台异步触发联邦投递（`DispatchFederationArticle`，停机时优雅等待在途任务排空），方便构建 ActivityPub、Webmention、AT Protocol 或跨站同步桥接插件；
     - **第十道隐私防线保证**：密码保护文章正文自动脱敏并标记 `ev.omitted_protected = true`，正文内嵌的所有 `:::lock ... :::` 局部加密块在进入联邦插件前已被物理剥离，确保零私密内容外泄；
   - **领域钩子**（支持 `hooks:*` 或细粒度令牌）：
     - 文章生命周期：`hooks:article.published`、`hooks:article.updated`、`hooks:article.deleted`；
     - 独立页面生命周期：`hooks:page.published`、`hooks:page.updated`、`hooks:page.deleted`；
     - 评论治理：`hooks:comment.before_create`、`hooks:comment.created`、`hooks:comment.moderated`；
     - 渲染管线：`hooks:render.pre_markdown`（在非加密且不含 `:::lock` 的文章进入 Markdown 解析器前改写 `content_md`）与 `hooks:render.post_html`（对渲染后的 HTML 做后处理并二次净化）；
   - **界面插槽**（支持 `ui:slots` 或按插槽授权）：`slots:public.head`、`slots:public.header`、`slots:public.sidebar`、`slots:article.before_content`、`slots:article.after_content`、`slots:comment.before`、`slots:comment.after`、`slots:public.footer`、`slots:editor.quick_insert`、`slots:admin.dashboard_card`；
   - **网络与路由**：`http:routes`（挂载 `/plugins/<id>/api/*` 自定义公开路由）、`http:raw_headers`（按需透传 `x-hub-signature-256`、`x-signature`、`stripe-signature`、`authorization` 等安全验签请求头）、`http.outbound:<hostname>` / `allowed_hosts`（显式域名或 `*.domain.tld` 白名单 + `OutboundSafeDialer` 防 SSRF）；
   - **核心扩展 Provider**：
     - `provider:storage`：实现 `storagePut(objectKey, bodyBase64, contentType)` 与可选 `storageDelete(objectKey)`、`storageHealthCheck()` 注册外部对象存储驱动（熔断时自动降级本地磁盘）；
     - `provider:oidc`：实现 `oidcAuthURL(state, redirectURI, opts)` 与 `oidcExchange(code, redirectURI, opts)`（含 PKCE + `nonce` 防重放校验，`email_verified` 默认 `false`）及可选 `oidcProviderInfo()` 注册第三方联合登录驱动；
     - `provider:notifier`：实现 `sendNotification(event)` 或 `notifySend(event)` 注册自定义通知渠道；
     - `provider:antispam`：实现 `checkSpam(input)`（返回 `{ action: "allow" | "moderate" | "reject", reason, score }`）注册评论反垃圾审查驱动；
   - **高敏读取**：`articles:read_protected`（未显式声明此权限的插件，宿主自动清空密码保护文章正文并按嵌套规则剥离所有 `:::lock ... :::` 内容块）。

2. **沙箱熔断、资源护栏与宿主安全联动**：
   - 每次 JS Hook / Directive / Federation / Slot 执行均受指令步数预算、墙钟超时、循环引用深度上限及 `4 MiB` 字符串体积上限保护；连续异常或超时自动打开**熔断器（`CIRCUIT_OPEN`）**；
   - `volo.on(...)`、`volo.registerDirective(...)` 与 `volo.registerFederation(...)` 仅允许在模块顶层初始化阶段调用，初始化完成后自动冻结；
   - 插件 CSS 变量建议使用 `--volo-plugin-<id>-*` 命名空间，包裹容器自动带 `.volo-plugin-scope[data-plugin="<id>"]`；
   - 关于宿主级 **动态隐身后台路径热更新（`security.admin_path`）** 与 **多库共存 GeoIP 访问控制引擎（`GeoLite2-City/Country/ASN.mmdb` + `ip2region` XDB）** 的详细安全机制，请参阅 [`SECURITY.md`](../../SECURITY.md)。

### 3.4 沙箱 SDK（`volo.*`）、ES6 标准库与 `main.js` 完整示例

内置零外部依赖 JS 虚拟机原生支持现代 **ES6+ 语法与标准库**（`let`/`const`、模板字符串、可选链 `?.`、空值合并 `??`、箭头函数、展开与解构赋值，以及标准 `Array`/`Object`/`String`/`Number`/`RegExp`/`Date`/`JSON`/`Math`、`encodeURIComponent`、`btoa`/`atob`）。

宿主桥接对象 `volo`（别名 `ctx`）：
- `volo.on(hookName, handlerFn)`：顶层初始化期订阅领域/渲染钩子；
- `volo.registerDirective(name, handlerFn)`：顶层初始化期注册自定义 `:::name ... :::` Markdown 指令（需 `render` 权限）；
- `volo.registerFederation({ publish(ev), deliver(ev) })`：顶层初始化期注册文章发布/更新联邦分发处理器（需 `federation` 权限）；
- `volo.escapeHTML(str)`：HTML 实体转义；
- `volo.config.get(key)` / `volo.config.all()`：读取校验后的插件配置（含沙箱内自动透明解密的 `type: "secret"` 字段）；
- `volo.kv.get(key)` / `volo.kv.getInt(key)` / `volo.kv.set(key, value, ttlSeconds?)` / `volo.kv.del(key)` / `volo.kv.incr(key, delta?, ttlSeconds?)` / `volo.kv.list(prefix, limit?, afterKey?)`：插件命名空间 KV 存储；
- `volo.crypto.sha256(str)` / `volo.crypto.hmacSha256(secret, msg)` / `volo.crypto.randomUUID()`：加密哈希与随机 UUID；
- `volo.site.info()`、`volo.rateLimit(key, max, windowSec?)`、`volo.http.fetch(url, options?)`、`volo.i18n.t(locale, key, vars?)` 与 `volo.log.{info,warn,error}(msg)`。

```javascript
// 1. 注册 :::callout-box {type="tip"} ... ::: 自定义 Markdown 容器指令
volo.registerDirective("callout-box", (ctx) => {
  const kind = ctx.attrs?.type || "note";
  return `<div class="callout callout-${volo.escapeHTML(kind)}">${volo.escapeHTML(ctx.body)}</div>`;
});

// 2. 注册自动联邦发布/更新处理器（自动剥离 :::lock 与加密正文）
volo.registerFederation({
  publish(ev) {
    if (ev.omitted_protected) return;
    volo.kv.set(`fed:${ev.id}`, JSON.stringify({ action: ev.action, title: ev.title }), 86400);
  },
  deliver(ev) {
    volo.log.info(`federation deliver: ${ev.id}`);
  }
});

// 3. 订阅渲染钩子与安全渲染 UI 插槽
volo.on("render.post_html", ({ html }) => {
  volo.kv.incr("rendered_count", 1, 0);
  return { html };
});

function renderSlot(slotName, ctx) {
  if (slotName === "article.after_content") {
    const msg = volo.config.get("banner_text") || "感谢阅读！";
    return `<aside class="callout-box">${volo.escapeHTML(String(msg))}</aside>`;
  }
  return "";
}
```

---

## 4. 外部 Webhook 通知与 HMAC 验签协议

除进程内插件外，管理台 **「通知中心 -> Webhooks」** 原生支持向飞书、Telegram、Discord、Slack、Bark、企业微信及任意自定义 HTTP 服务推送 `article.published`、`comment.created` 等事件。

当为自定义 Webhook 配置了签名密钥（`Secret`）时，`Volo-go` 会在请求头附带标准 `HMAC-SHA256` 签名供接收端验签防伪造：

- `X-Volo-Event`: 事件名称（如 `article.published`、`comment.created`、`webhook.test`）
- `X-Volo-Timestamp`: Unix 秒级时间戳
- `X-Volo-Signature`: `sha256=<hex(HMAC_SHA256(secret, timestamp + "." + raw_json_body))>`

Node.js / Python / Go 接收端只需使用恒定时间比较（`crypto.timingSafeEqual` / `hmac.Equal`）校验 `X-Volo-Signature`，并拒绝时间戳偏差超过 5 分钟的重放请求即可。
