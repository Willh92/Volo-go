# 主题开发与旧版 Bolo-Solo 皮肤迁移指南

> English version: [Theme Authoring & Legacy Bolo-Solo Skin Migration Guide](../en-US/theme-migration.md)

`Volo-go` 采用纯 Go 标准库 `html/template` 构建零依赖、高性能的主题引擎，内置旗舰主题 `volo-studio`（另附官方外部纸刊主题 [`themes/volo-journal`](../../themes/volo-journal/)），同时全面支持外部目录 `./themes/<theme-id>` 与管理后台 `.zip` 主题包热安装、原位覆盖升级（`overwrite=true`）、纯净脚手架 `.zip` 导出、独立 JSON 多语言词典、模块化局部模板（Partials）、可选独立页版式（`page_templates`）、列表呈现模式（`Reading.ListStyle`）、自定义站点模板变量（`{{customVar}}`）以及声明式主题配置表单（`config_schema`）。

---

## 1. 主题包目录结构对比

| 旧版 Bolo-Solo 皮肤 (`skins/<name>/`) | Volo-go 主题 (`themes/<id>/`) | 说明 |
| --- | --- | --- |
| `skin.properties` 元信息配置文件 | `theme.json` | 主题 ID、多语言名称/简介、强调色、暗黑/PJAX 能力、`page_templates` 与 `config_schema` |
| `lang_*.properties` 语言键值配置 | `locales/zh-CN.json`、`locales/en-US.json` | UTF-8 标准 JSON 词典，通过 `{{tt .ThemeID .Locale "key"}}` 读取并自动回退系统词典 |
| `preview.png` / `preview.svg` | `preview.svg` / `preview.png` / `preview.webp` | 后台主题画廊卡片预览图（自动映射至 `/themes/<id>/preview`） |
| `header.ftl` / `footer.ftl` / `side.ftl` / `macro-*.ftl` | `templates/layout.html`、`templates/partials/*.html`、`templates/_*.html`、`templates/macro-*.html` | 共享骨架与局部模板，在所有页面模板间自动编译共享 |
| `index.ftl` / `article.ftl` / `page.ftl` / `dynamic.ftl` 等 | `templates/index.html`、`article.html`、`page.html`、`page-<tpl>.html`、`dynamic.html`、`error.html` 等 | 页面级模板；未覆写的页面自动回退至内置 `volo-studio` 同名模板 |
| `css/*.css`、`js/*.js`、`images/*` | `assets/**` | 自动支持多层子目录、SHA-256 指纹缓存（`{{assetURL .ThemeID "css/main.css"}}`）与预压缩 Gzip |

---

## 2. `theme.json` 清单、`page_templates` 与 `config_schema` 专属设置

外部主题无需自建数据库表，只需在 `theme.json` 中声明 `page_templates`（可选独立页面版式）与 `config_schema`（主题配置项），管理后台 **「外观与扩展 -> 主题皮肤 -> 主题设置」** 即会自动渲染可视化配置弹窗，并将配置持久化至 `Appearance.ThemeConfigs[<id>]`：

```json
{
  "id": "my-editorial-skin",
  "name": {
    "zh-CN": "墨韵手记",
    "en-US": "Ink Editorial"
  },
  "version": "1.0.0",
  "author": "Your Name",
  "license": "MIT",
  "preview": "preview.svg",
  "description": {
    "zh-CN": "从旧版 Bolo-Solo 迁移的双栏人文博客皮肤。",
    "en-US": "Two-column literary skin migrated from legacy Bolo-Solo."
  },
  "accents": ["forest", "ocean", "teal", "amber", "plum", "rose"],
  "supports_dark_mode": true,
  "supports_pjax": true,
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
  ],
  "config_schema": [
    {
      "key": "hero_notice",
      "type": "textarea",
      "label": { "zh-CN": "首页公告栏文案", "en-US": "Home Notice" },
      "default": ""
    },
    {
      "key": "sidebar_position",
      "type": "select",
      "label": { "zh-CN": "侧边栏位置", "en-US": "Sidebar Position" },
      "default": "right",
      "options": [
        { "value": "right", "label": { "zh-CN": "右侧", "en-US": "Right" } },
        { "value": "hidden", "label": { "zh-CN": "隐藏（单栏）", "en-US": "Hidden" } }
      ]
    },
    {
      "key": "show_site_stats",
      "type": "boolean",
      "label": { "zh-CN": "页脚显示站点统计", "en-US": "Show Footer Stats" },
      "default": true
    }
  ]
}
```

支持的 `config_schema` 字段类型：`string` / `text`、`textarea`、`url`、`image`、`color`、`boolean`、`number`（支持 `min`/`max`/`step`）、`select`、`sortable_list`（可勾选显隐并上下拖拽排序的多选列表，如 `header_nav_items`）。

在模板中读取主题配置、自定义站点变量与列表模式：
- 主题字符串配置：`{{themeCfg .ThemeConfig "hero_notice" ""}}`
- 主题布尔配置：`{{if themeCfgBool .ThemeConfig "show_site_stats" true}}...{{end}}`
- 主题整数配置：`{{themeCfgInt .ThemeConfig "recent_count" 6}}`
- 站点自定义模板变量（兼容旧版 `customVars`，在后台 **「站点设置 -> 自定义模板变量」** `site.custom_vars` 中维护）：`{{customVar .Site.CustomVars "icp_number" ""}}`
- 文章列表呈现模式（兼容旧版 `articleListStyle`，对应后台 `reading.list_style`：`summary` 摘要卡片 | `title_only` 纯标题极简目录 | `full` 全文时间流）：当 `.Reading.ListStyle` 为 `"full"` 时，公开时间流中的 `.ArticleCards` 会预先填充安全净化的 `.FullHTML`（密码保护文章与未解锁的 `:::lock` 内容块仍由 `VisibilityGuard` 严格脱敏零泄露）。

---

## 3. 模板组织模式与无边框独立 `error.html` 覆写

Volo-go 主题引擎同时支持现代母版继承、传统 `.ftl` 独立页面拼装以及全屏无框错误页覆写：

1. **母版继承模式（推荐）**：
   - 在 `templates/layout.html` 中定义 `{{define "layout"}}...{{template "content" .}}...{{end}}`。
   - 在具体页面（如 `templates/index.html`）中只需定义 `{{define "content"}}...{{end}}`。
2. **独立完整 HTML 模式（方便旧版 `.ftl` 直接迁移）**：
   - 若页面模板未定义 `"content"` 块，引擎将直接以该页面文件名作为入口执行完整 HTML。
   - 您可以在 `templates/partials/header.html` 与 `templates/partials/footer.html` 中定义 `{{define "header"}}` / `{{define "footer"}}`，然后在 `index.html` 中直接书写 `{{template "header" .}} ... {{template "footer" .}}`，与旧版 `<#include "header.ftl">` 完全一致。
3. **独立无边框 `error.html` 全屏错误页覆写（Standalone Frameless Override）**：
   - 即便主题采用 `templates/layout.html` 母版继承，`templates/error.html` 依然可以通过重定义 `{{define "layout"}}<!doctype html>...</html>{{end}}{{define "content"}}{{end}}`（或省略 `"content"` 定义直接输出完整文档），绕过常规顶栏与侧栏骨架，渲染沉浸式全屏 `404` / `403` / `429` / `500` 自定义错误页（内置 `volo-studio` 即采用此机制提供带即时站内搜索的独立错误画布）。

---

## 4. 页面模板路由与自动回退矩阵

外部主题可以按需定制任意细分页面；若未提供细分文件，引擎会自动按候选顺序安全回退：

| 访问路径 | 优先查找的细分模板 | 默认回退模板 |
| --- | --- | --- |
| `/` | `index.html`（兼容外部主题 `home.html`） | `index.html` |
| `/posts/{slug}` | `article.html` | `article.html` |
| `/{page-slug}` | `page-<template>.html`（由页面所选 `page_templates` 决定） | `page.html` |
| `/categories` | `categories.html` | `taxonomies.html` |
| `/categories/{slug}` | `category_posts.html` | `index.html` |
| `/tags` | `tags.html` | `taxonomies.html` |
| `/tags/{slug}` | `tag_posts.html` | `index.html` |
| `/archives` | `archives.html` | `taxonomies.html` |
| `/archives/{yyyy-mm}` | `archive_posts.html` | `index.html` |
| `/series` | `series.html` | `taxonomies.html` |
| `/series/{slug}` | `series_detail.html` | `index.html` |
| `/dynamic` | `dynamic.html` | `taxonomies.html` |
| `/books` / `/books/{slug}` | `bookshelf.html` / `book_reader.html` | 内置同名模板 |
| `/links` / `/follows` | `links.html` / `follows.html` | 内置同名模板 |
| `/reader/login` / 错误页 | `reader_auth.html` / `error.html` | 内置同名模板 |

此外，所有旧版静态后缀链接（如 `/archives.html`、`/tags.html`、`/categories.html`、`/links.html`、`/dynamic.html`、`/category/{slug}`）均由服务端自动 `301` 规范化重定向至无后缀现代路由，确保旧主题硬编码导航链接与历史外链零死链。

---

## 5. FreeMarker (`.ftl`) 语法与数据模型映射速查表

### 5.1 语法对照

| FreeMarker (`.ftl`) | Volo-go (`html/template`) |
| --- | --- |
| `${site.title}` | `{{.Site.Title}}` |
| `${customVars["icp"]!""}` | `{{customVar .Site.CustomVars "icp" ""}}` |
| `${i18nLabel}` | `{{tt .ThemeID .Locale "skin.label"}}` 或 `{{t .Locale "nav.home"}}` |
| `<#if cond> ... <#elseif c2> ... <#else> ... </#if>` | `{{if cond}} ... {{else if c2}} ... {{else}} ... {{end}}` |
| `<#list articles as article> ... </#list>` | `{{range .ArticleCards}} ... {{end}}` |
| `<#include "side.ftl">` | `{{template "side" .}}`（在 `partials/side.html` 中 `{{define "side"}}`） |
| `${article.createDate?string("yyyy.MM.dd")}` | `{{formatDotDate .PublishedAt}}` 或 `{{formatTime .PublishedAt "2006.01.02"}}` |
| `${article.content}`（已脱敏 HTML） | `{{safeHTML .Detail.HTML}}`（或时间流全文模式 `{{safeHTML .FullHTML}}`） |

### 5.2 核心数据模型 (`PageContext`) 对照

| 旧版皮肤常见模型字段 | Volo-go `PageContext` 对应字段 | 说明 |
| --- | --- | --- |
| `blogTitle` / `blogSubtitle` | `.Site.Title` / `.Site.Subtitle` | 站点主副标题 |
| `customVars` | `.Site.CustomVars`（配合 `{{customVar .Site.CustomVars "key" "def"}}`） | 后台可视化维护的任意自定义键值模板变量 |
| `noticeBoard` / `footerContent` | `.Site.Description`（或主题配置）/ `.Site.FooterText` | 公告文案与页脚信息 |
| `articleListStyle` | `.Reading.ListStyle`（`summary` \| `title_only` \| `full`） | 列表摘要、纯标题或全文展开呈现模式 |
| `adminUser` / `users` | `.Owner` / `.Authors` | 博主与团队公开资料（含头像、简介、文章数） |
| `statistic.*` | `.Stats` (`ArticleCount`, `CommentCount`, `ViewCount`, `TagCount`, `UptimeDays`) | 全站公开统计指标（内存快照直出，零 SQL 开销） |
| `pageNavigations` | `.NavItems` / `.NavMenu`（`.Primary`、`.Overflow`、`.OverflowActive`） | 自定义顶部导航栏与自适应溢出菜单 |
| `articles` + `pagination*` | `.ArticleCards` + `.Pagination` (`PageNumbers`, `PrevPage`, `NextPage`, `ShowFirst`, `ShowLast`) | 文章卡片列表与滑动窗口分页器 |
| `article` + `previousArticle` / `nextArticle` | `.Detail` + `.PrevPost` / `.NextPost` | 文章详情（已通过 `VisibilityGuard` 脱敏保护）与上下篇 |
| `relevantArticles` / `randomArticles` | `.RelatedPosts` / `.RandomPosts` | 相关文章与随机推荐（加密文章物理隔离零泄露） |
| `mostUsedCategories` / `mostUsedTags` / `archiveDates` | `.SidebarCategories` / `.SidebarTags` / `.SidebarArchives` | 侧边栏分类、热门标签、按月归档 |
| `recentArticles` / `mostViewCountArticles` / `mostCommentArticles` | `.RecentPosts` / `.MostViewedPosts` / `.MostCommentedPosts` | 侧边栏最新、热门阅读、热议文章榜单 |
| `recentComments` | `.RecentComments` | 最新公开评论动态 |

### 5.3 内置模板函数库 (`FuncMap`)

- **多语言、站点变量与资源**：`t`、`tt`、`customVar`、`assetURL`、`coverSVG`、`pageURL`
- **主题配置**：`themeCfg`、`themeCfgBool`（别名 `themeBool`）、`themeCfgInt`（别名 `themeInt`）
- **日期时间**：`formatDate` (`YYYY-MM-DD`)、`formatDotDate` (`YYYY.MM.DD`)、`formatDateTime`、`formatMonthDay`、`formatTime`、`nowYear`
- **集合与字典**：`dict`（向局部模板传参，如 `{{template "card" (dict "Post" . "Root" $)}}`）、`slice`、`seq`、`first`、`default`、`coalesce`
- **字符串与数值**：`hasPrefix`、`hasSuffix`、`contains`、`replace`、`split`、`join`、`lower`、`upper`、`trim`、`truncate`、`urlquery`、`add`、`sub`、`mul`、`div`、`mod`
- **安全类型标记**：`safeHTML`、`proseHTML`、`renderNotice`、`safeCSS`、`safeJS`、`safeURL`、`json`

---

## 6. 脚手架导出、打包与后台一键热安装/升级

1. **一键导出纯净 Starter `.zip` 脚手架（推荐起步方式）**：
   - 登录管理后台（默认 `/console`，或通过 `security.admin_path` 自定义的动态隐身路径）-> **「外观与扩展 -> 主题皮肤」**；
   - 在内置 `volo-studio` 或任一现有主题卡片上点击 **「导出主题包 (.zip)」**（`GET /api/admin/themes/{id}/export`），即可下载剔除临时文件与运行态噪音的纯净 `.zip` 脚手架直接二次创作。
2. **手工打包主题目录为 `.zip`**（支持根目录直接包含 `theme.json` 或单层顶层文件夹 `<theme-id>/theme.json`）：
   ```bash
   zip -r my-editorial-skin.zip theme.json preview.svg locales/ templates/ assets/
   ```
3. **热安装、原位升级与免切换预览**：
   - 点击右上角 **「上传安装主题 (.zip)」**（`POST /api/admin/themes/install?overwrite=true`）即可完成安全校验、热加载或原位覆盖升级（已保存的 `Appearance.ThemeConfigs[<id>]` 配置自动保留）；
   - 点击卡片上的 **「预览」** 可使用管理员身份免切换实时预览桌面端（`/?skin=my-editorial-skin`）或移动端（`/?skin=my-editorial-skin&device=mobile`）；
   - 点击 **「主题设置」** 配置 `config_schema` 表单项，确认无误后点击 **「启用主题」** 立即生效。
