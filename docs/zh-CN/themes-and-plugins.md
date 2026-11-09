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

`Volo-go` 彻底摒弃“在核心代码硬编码判断特定插件名”的做法。所有内置能力与外部目录插件（如 [`plugins/kanbanniang`](../../plugins/kanbanniang/) 与 [`plugins/sensitive-words`](../../plugins/sensitive-words/)）统一遵守声明式权限沙箱与优先级链式协议。

### 3.1 插件目录结构、数据物理隔离与 5 阶段生命周期

```text
plugins/hello-callout/
├── manifest.json           # 权限、优先级(priority)、插槽、钩子、定时任务与配置 Schema
├── main.js                 # 纯 Go 内嵌 ES6 JSVM 沙箱入口脚本
├── dicts/                  # 可选静态词典文件（仅可读本插件根目录内相对路径）
└── assets/                 # 可选静态资源（通过 os.OpenInRoot 安全分发至 /plugins/<id>/assets/*）
    ├── widget.css
    └── widget.js
```

- **数据隔离与卸载彻底清理**：
  - 数据库存储按 `(plugin_id, key)` 在 `plugin_kv` 物理隔离（单键 `<= 256 KiB`、单插件默认 `<= 2,000` 条配额），加密配置密封于 `plugins.secret_ciphertext`；插件仅可只读访问自身根目录内文件，禁止写宿主文件系统。
  - **完整 5 阶段生命周期**：`onInstall()`（首次安装或原位升级后） -> `onEnable()`（启用加载） -> `onConfigChange(newCfg)`（后台保存配置即时热响应，无需重启 VM） -> `onDisable()`（停用并注销该插件全部动态定时任务与 Provider） -> `onUninstall()`（卸载前最佳努力执行收尾，随后宿主在事务内彻底清除该插件的 `plugin_kv`、`plugins` 记录及 `./plugins/<id>` 目录）。
- **热安装、原位升级（`overwrite=true`）与纯净 `.zip` 导出**：支持上传 `.zip` 包完成零停机安装或原位覆盖升级（自动保留已有 `config_json`、自定义优先级与 `secret_ciphertext` 加密密钥），并支持 `GET /api/admin/plugins/{id}/export` 导出不含任何机密密文的纯净 `.zip` 包。

### 3.2 `manifest.json` 完整字段、`priority` 链式优先级与 `type: "secret"` 加密配置

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

> **插件中心链式优先级（`priority`）与一键排序**：每个插件拥有执行优先级 `priority`（范围 `-1000..10000`，默认 `100`，官方 `sensitive-words` 默认为 `20`；**数值越小越先执行**，相同数值按插件 `id` 字典序确定性兜底）。管理员可在后台「插件中心」直接点击 **`↑` / `↓` / 置顶**（`POST /api/admin/plugins/reorder`）或点击 `#1 · P20` 徽章精确修改数值（`PATCH /api/admin/plugins/{id}/priority`）。该顺序同时决定**所有领域/前置拦截钩子、前台 UI 插槽拼接顺序、渲染缓存指纹以及同类型扩展 Provider 链（如多个 `provider:antispam`、`provider:notifier`、`provider:federation`、`provider:oidc`、`provider:storage`）的执行先后**。

> **`type: "secret"` 密钥落盘加密保证**：`config_schema` 中声明 `"type": "secret"`（或 `"secret": true`）的字段在写入数据库时自动使用宿主 `KeyRing` 进行 `AES-256-GCM` 独立列加密（`plugins.secret_ciphertext`，绑定 `AAD = plugins.secret_ciphertext:<plugin_id>`）。后台管理 API 仅返回布尔掩码映射 `secret_keys_set`，绝不回显明文；若需清空已保存密钥，可传入哨兵值 `"__VOLO_CLEAR_SECRET__"`。

### 3.3 统一前置链式拦截协议（`before_*` Hooks）与宿主屏蔽词并集关系

1. **统一前置拦截钩子清单（`IsInterceptableHook`）**：
   所有 `before_*` 前置钩子（含自定义 `*.before_*`）原生支持按插件优先级顺序**链式改写载荷与短路拦截**：
   - 评论治理：`comment.before_create`、`comment.before_update`（若钩子改写了 `content_md`，宿主会自动重跑反垃圾与敏感词二次复检，杜绝后置绕过）；
   - 用户与昵称治理：`user.before_register`（覆盖邮箱验证码预检、读者注册与 OIDC 首次自动建号）、`user.before_update_profile`（覆盖读者昵称修改与协作者个人资料保存）；
   - 内容与媒体治理：`article.before_save`、`page.before_save`、`media.before_upload`。

2. **4 种标准拦截返回语义（业务拦截绝不误触熔断器）**：
   在前置钩子中，任一插件返回或抛出以下结构均会立即短路后续插件并向客户端返回本地化业务提示（返回 `HookRejectedError`，**计为正常业务拦截，绝不累加 `failure_count` 或触发 `CIRCUIT_OPEN` 熔断**）：
   - 推荐写法：`return volo.reject("包含违禁词汇", "SENSITIVE_WORD")` 或直接 `throw volo.reject("包含违禁词汇", "SENSITIVE_WORD")`；
   - 对象写法：`return { allow: false, reason: "禁止发布", code: "POLICY_BLOCK" }` 或 `return { action: "reject", reason: "..." }`（或 `{ reject: true, reason: "..." }`）；
   - 极简布尔：`return false`；
   - **非破坏式局部载荷合并与单向待审升级**：返回普通对象（如 `return { content_md: cleaned }`）时，宿主将其与当前载荷做局部键合并并继续传递给下一优先级插件；评论钩子若设 `{ status: "pending" }`（或 `{ action: "moderate" }`），待审状态在链路上单向保留，不会被后续低优先级插件意外冲掉。

3. **官方 `sensitive-words` 插件与宿主后台配置的合并（并集叠加）关系**：
   - **插件内部词库合并**：`houbb_base.txt.gz`（`281 KiB` 前缀/标签差分压缩，支持政治/涉枪涉暴/涉黄/毒品赌博/广告导流/脏话辱骂 6 大分类独立开关） + 内置技术词汇白名单 `allowlist.txt` + 可选远程词库自动同步 + 后台自定义拦截词 `custom_deny` + 自定义白名单 `custom_allow`（白名单最长区间覆盖优先） + 保留昵称 `reserved_nicknames` 统一编译为原生双数组 Aho-Corasick 自动机。
   - **与宿主后台设置（`reading.blocked_keywords` / `allowed_keywords`）并集叠加**：
     - **昵称与账号链路**：对宿主 `reading.blocked_keywords`、全部已启用 `provider:antispam` 插件（`checkNickname`）及 `user.before_*` 钩子严格取**并集**，任一侧命中即拦截；
     - **评论链路**：智能审核先执行宿主 `reading.blocked_keywords`（命中置为 `pending` 待审），随后按优先级依次执行 `provider:antispam` 链——插件未命中时返回 `"pass"` 完整保留宿主的 `pending` 判定，命中时则按插件策略升级为直接拦截（`reject`）、打码放行或转人工审核（`moderate`）。

### 3.4 标准权限（Permissions）、定时任务调度器与扩展 Provider

1. **权限白名单令牌**：
   - **存储与基础能力**：`kv` / `kv:rw`、`kv:read`、`crypto`（`sha256` / `hmacSha256` / `randomUUID`）、`site:read`（`volo.site.info()`）、`ratelimit`（`volo.rateLimit`）、`cron`（解锁 `manifest.json` `"cron"` 与运行时动态 `volo.cron.register/unregister/list`，单插件上限 16 个任务、最小间隔 5 秒，支持后台一键「立即执行定时任务」）；
   - **自定义 Markdown 指令扩展（`render` + `volo.registerDirective`）**：声明 `"render"` 权限后可注册 `:::name {key="val"} ... :::` 块级指令，输出自动经宿主单趟 HTML5 AST 白名单净化器清洗；内置 `:::lock` 由宿主物理保留，禁止插件劫持；
   - **自动联邦网络异步分发（`federation` / `provider:federation`）**：声明 `"federation"` 并调用 `volo.registerFederation({ publish(ev), deliver(ev) })`，公开文章发布/更新时异步按插件优先级投递，密码文章与 `:::lock` 块 100% 物理剥离；
   - **领域钩子**（支持 `hooks:*`、`hooks:<domain>.*` 通配符或细粒度令牌）：
     - 前置拦截钩子：`hooks:comment.before_create`、`hooks:comment.before_update`、`hooks:user.before_register`、`hooks:user.before_update_profile`、`hooks:article.before_save`、`hooks:page.before_save`、`hooks:media.before_upload`；
     - 后置通知与渲染钩子：`hooks:article.{published,updated,deleted}`、`hooks:page.{published,updated,deleted}`、`hooks:comment.{created,moderated}`、`hooks:render.{pre_markdown,post_html}`；
   - **界面插槽**（`ui:slots` 或 `slots:*`）：`public.head`、`public.header`、`public.sidebar`、`article.before_content`、`article.after_content`、`comment.before`、`comment.after`、`public.footer`、`editor.quick_insert`、`admin.dashboard_card`；
   - **网络与路由**：`http:routes`（挂载 `/plugins/<id>/api/*`）、`http:raw_headers`、`http.outbound:<hostname>` / `allowed_hosts`（配合 `OutboundSafeDialer` 防 SSRF）；
   - **核心扩展 Provider**（多个同类型插件严格按 `priority` 升序链式执行）：
     - `provider:storage`：`storagePut` / `storageDelete` / `storageHealthCheck`（自动降级本地磁盘）；
     - `provider:oidc`：`oidcAuthURL` / `oidcExchange` / `oidcProviderInfo`（PKCE + `nonce` 校验）；
     - `provider:notifier`：`sendNotification(event)` / `notifySend(event)`；
     - `provider:antispam`：`checkSpam(input)`（返回 `{ action: "pass" | "allow" | "moderate" | "reject", reason, score }`）与可选 `checkNickname(input)`（返回 `{ allow: boolean, reason }`）；
   - **高敏读取**：`articles:read_protected`。

2. **沙箱熔断、资源护栏与宿主安全联动**：
   - 每次 JS Hook / Directive / Cron / Federation / Slot 执行均受指令步数预算、墙钟超时、循环引用深度上限及 `4 MiB` 字符串体积上限保护；非业务抛错或超时连续达到阈值自动打开**熔断器（`CIRCUIT_OPEN`）**；
   - 关于宿主级 **动态隐身后台路径热更新（`security.admin_path`）** 与 **多库共存 GeoIP 访问控制引擎** 的详细安全机制，请参阅 [`SECURITY.md`](../../SECURITY.md)。

### 3.5 沙箱 SDK（`volo.*`）、原生 `wordFilter` 引擎与 `main.js` 示例

宿主桥接对象 `volo`（别名 `ctx`）：
- `volo.on(hookName, handlerFn)` / `volo.reject(reason, code?)`：订阅领域/前置拦截钩子与返回结构化业务拦截；
- `volo.wordFilter.compile({ deny, allow, normalize, maxRepeat })` / `volo.wordFilter.loadHoubbDict(relGzPath, opts)`：编译原生零 CGO 双数组 Aho-Corasick 词库匹配器，返回实例支持 `.match(text)`、`.contains(text)`、`.censor(text, maskRune?)` 与 `.stats()`；
- `volo.cron.register(name, intervalSec, handlerName, immediate?)` / `volo.cron.unregister(name)` / `volo.cron.list()`：动态定时任务管理（需 `cron` 权限）；
- `volo.registerDirective(name, handlerFn)` / `volo.registerFederation({ publish(ev), deliver(ev) })`；
- `volo.escapeHTML(str)`、`volo.config.{get,all}()`、`volo.kv.{get,getInt,set,del,incr,list}(...)`、`volo.crypto.{sha256,hmacSha256,randomUUID}()`、`volo.site.info()`、`volo.rateLimit(...)`、`volo.http.fetch(...)`、`volo.i18n.t(...)` 与 `volo.log.{info,warn,error}(msg)`。

```javascript
let matcher = null;

function rebuildMatcher() {
  const customDeny = String(volo.config.get("blocked_words") || "").split("\n");
  matcher = volo.wordFilter.compile({ deny: customDeny, allow: ["Go语言"], normalize: true });
}

function onEnable() { rebuildMatcher(); }
function onConfigChange() { rebuildMatcher(); }
function onUninstall() { matcher = null; }

// 前置链式拦截钩子：命中时通过 volo.reject 短路拦截（不触发熔断器），否则局部改写载荷继续传递
volo.on("comment.before_create", (payload) => {
  if (matcher && matcher.contains(payload.content_md || "")) {
    return volo.reject("评论包含受限词汇，请修改后重试", "WORD_BLOCKED");
  }
  return { content_md: String(payload.content_md || "").trim() };
});

function renderSlot(slotName) {
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
