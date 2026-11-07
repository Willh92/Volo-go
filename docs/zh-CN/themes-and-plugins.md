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
  --volo-text: #1c1e1b;
  --volo-muted: #5f665e;
  --volo-border: rgba(28, 30, 27, 0.10);
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
    ├── index.html
    └── post.html
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

---

## 2. 安全沙箱插件开发（`manifest.json` + `goja`）

`Volo-go` 彻底摒弃“在核心代码硬编码判断特定插件名”的做法。所有内置能力与外部目录插件（如 [`plugins/kanbanniang`](../../plugins/kanbanniang/)）统一遵守声明式权限沙箱协议。

### 2.1 插件目录结构与 `.zip` 热插拔

```text
plugins/hello-callout/
├── manifest.json           # 权限、插槽、钩子、定时任务与配置表单 Schema
├── main.js                 # 纯 Go 内嵌 goja (ES2020+) 沙箱入口脚本
└── assets/                 # 可选静态资源（通过 /plugins/hello-callout/assets/* 分发）
    ├── widget.css
    └── widget.js
```

管理员既可将目录直接放入 `./plugins/`，也可在管理台 **「插件扩展」** 页面直接上传 `.zip` 插件包一键校验安装、自动根据 `config_schema` 渲染多语言配置表单，并随时热启停或卸载。

### 2.2 `manifest.json` 完整字段说明

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
      "default": "感谢阅读，欢迎通过 RSS 订阅更新。",
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

> **密钥落盘加密保证**：`config_schema` 中标记 `"secret": true` 的字段在写入数据库时自动使用 `KeyRing` 进行 `AES-256-GCM` 列级加密，且在后台配置接口中自动脱敏为掩码，不会明文回显。

### 2.3 标准权限（Permissions）、钩子（Hooks）与插槽（UI Slots）

1. **权限白名单令牌**：
   - 存储：`kv`（读写）、`kv:read`（只读 `plugin_kv`，单插件默认配额上限保护）；
   - 领域钩子：`hooks:article.published`、`hooks:article.updated`、`hooks:comment.before_create`、`hooks:comment.created`、`hooks:render.post_html`；
   - 界面插槽：`slots:public.head`、`slots:public.header`、`slots:article.before_content`、`slots:article.after_content`、`slots:public.footer`、`slots:editor.quick_insert`、`slots:admin.dashboard_card`；
   - 网络与路由：`http:routes`（挂载 `/plugins/<id>/api/*` 自定义公开路由）、`http.outbound:<hostname>`（显式域名白名单 + `OutboundSafeDialer` 拦截内网/回环/云元数据 IP 防 SSRF）；
   - 高敏读取：`articles:read_protected`（未显式声明此权限并经管理员知情启用的插件，绝不会收到密码保护文章或未解锁块内容）。

2. **沙箱熔断与样式隔离红线**：
   - 每次 JS Hook / Slot 执行受严格指令中断超时与内存保护，连续异常或超时自动打开**熔断器（`CircuitOpen`）**旁路该插件，绝不拖垮主站响应；
   - `render.post_html` 返回的 HTML 二次经过 `bluemonday` 白名单净化，杜绝插件引入存储型 XSS；
   - 插件 CSS 变量必须使用 `--volo-plugin-<id>-*` 命名空间，包裹容器自动带 `.volo-plugin-scope[data-plugin="<id>"]` 以避免污染宿主排版。

### 2.4 `main.js` 编写示例

```javascript
// 订阅文章 HTML 后处理钩子
volo.on("render.post_html", function (ctx) {
  var hits = Number(volo.kv.get("rendered_count") || "0") + 1;
  volo.kv.set("rendered_count", String(hits));
  return ctx.html;
});

// 动态渲染 UI 插槽
function renderSlot(slotName, ctx) {
  if (slotName === "article.after_content") {
    var msg = volo.config.get("banner_text") || "Thanks for reading!";
    return '<aside class="callout-box">' + volo.escapeHTML(msg) + "</aside>";
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
