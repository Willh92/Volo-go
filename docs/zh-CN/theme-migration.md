# 主题开发与旧版 Bolo-Solo 皮肤迁移指南

Volo-go 采用纯 Go 标准库 `html/template` 构建零依赖、高性能的主题引擎，内置旗舰主题 `volo-studio`，同时全面支持外部目录 `./themes/<theme-id>` 与管理后台 `.zip` 主题包热安装、热重载、独立 JSON 多语言词典、模块化局部模板（Partials）以及声明式主题配置表单（`config_schema`）。

---

## 1. 主题包目录结构对比

| 旧版 Bolo-Solo 皮肤 (`skins/<name>/`) | Volo-go 主题 (`themes/<id>/`) | 说明 |
| --- | --- | --- |
| `skin` 元信息配置文件 | `theme.json` | 主题 ID、多语言名称/简介、强调色、暗黑/PJAX 能力与 `config_schema` |
| `lang_*.<ext>` 语言键值配置 | `locales/zh-CN.json`、`locales/en-US.json` | UTF-8 标准 JSON 词典，通过 `{{tt .ThemeID .Locale "key"}}` 读取并自动回退系统词典 |
| `preview.png` / `preview.svg` | `preview.svg` / `preview.png` / `preview.webp` | 后台主题画廊卡片预览图（自动映射至 `/themes/<id>/preview`） |
| `header.ftl` / `footer.ftl` / `side.ftl` / `macro-*.ftl` | `templates/layout.html`、`templates/partials/*.html`、`templates/_*.html`、`templates/macro-*.html` | 共享骨架与局部模板，在所有页面模板间自动编译共享 |
| `index.ftl` / `article.ftl` / `page.ftl` / `dynamic.ftl` 等 | `templates/index.html`、`article.html`、`page.html`、`dynamic.html` 等 | 页面级模板；未覆写的页面自动回退至内置 `volo-studio` 同名模板 |
| `css/*.css`、`js/*.js`、`images/*` | `assets/**` | 自动支持多层子目录、SHA-256 指纹缓存（`{{assetURL .ThemeID "css/main.css"}}`）与预压缩 Gzip |

---

## 2. `theme.json` 清单与 `config_schema` 专属设置

外部主题无需自建数据库表，只需在 `theme.json` 中声明 `config_schema`，管理后台 **「外观与扩展 -> 主题皮肤 -> 主题设置」** 即会自动渲染可视化配置弹窗，并将配置持久化至 `Appearance.ThemeConfigs[<id>]`：

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

支持的字段类型：`text`、`textarea`、`url`、`color`、`boolean`、`number`、`select`。
在模板中读取配置：
- 字符串：`{{themeCfg .ThemeConfig "hero_notice" ""}}`
- 布尔值：`{{if themeCfgBool .ThemeConfig "show_site_stats" true}}...{{end}}`
- 整数值：`{{themeCfgInt .ThemeConfig "recent_count" 6}}`

---

## 3. 两种模板组织模式（兼容 Layout 继承与旧版独立 HTML 宏拼装）

Volo-go 主题引擎同时支持现代母版继承与传统 `.ftl` 独立页面拼装两种风格：

1. **母版继承模式（推荐）**：
   - 在 `templates/layout.html` 中定义 `{{define "layout"}}...{{template "content" .}}...{{end}}`。
   - 在具体页面（如 `templates/index.html`）中只需定义 `{{define "content"}}...{{end}}`。
2. **独立完整 HTML 模式（方便旧版 `.ftl` 直接迁移）**：
   - 若页面模板未定义 `"content"` 块，引擎将直接执行页面文件本身。
   - 您可以在 `templates/partials/header.html` 与 `templates/partials/footer.html` 中定义 `{{define "header"}}` / `{{define "footer"}}`，然后在 `index.html` 中直接书写 `{{template "header" .}} ... {{template "footer" .}}`，与旧版 `<#include "header.ftl">` 完全一致。

---

## 4. 页面模板路由与自动回退矩阵

外部主题可以按需定制任意细分页面；若未提供细分文件，引擎会自动按候选顺序回退：

| 访问路径 | 优先查找的细分模板 | 默认回退模板 |
| --- | --- | --- |
| `/` | `index.html` | `index.html` |
| `/posts/{slug}` | `article.html` | `article.html` |
| `/{page-slug}` | `page.html` | `page.html` |
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
| `${i18nLabel}` | `{{tt .ThemeID .Locale "skin.label"}}` 或 `{{t .Locale "nav.home"}}` |
| `<#if cond> ... <#elseif c2> ... <#else> ... </#if>` | `{{if cond}} ... {{else if c2}} ... {{else}} ... {{end}}` |
| `<#list articles as article> ... </#list>` | `{{range .ArticleCards}} ... {{end}}` |
| `<#include "side.ftl">` | `{{template "side" .}}`（在 `partials/side.html` 中 `{{define "side"}}`） |
| `${article.createDate?string("yyyy.MM.dd")}` | `{{formatDotDate .PublishedAt}}` 或 `{{formatTime .PublishedAt "2006.01.02"}}` |
| `${article.content}`（已脱敏 HTML） | `{{safeHTML .Detail.HTML}}` |

### 5.2 核心数据模型 (`PageContext`) 对照

| 旧版皮肤常见模型字段 | Volo-go `PageContext` 对应字段 | 说明 |
| --- | --- | --- |
| `blogTitle` / `blogSubtitle` | `.Site.Title` / `.Site.Subtitle` | 站点主副标题 |
| `noticeBoard` / `footerContent` | `.Site.Description`（或主题配置）/ `.Site.FooterText` | 公告文案与页脚信息 |
| `adminUser` / `users` | `.Owner` / `.Authors` | 博主与团队公开资料（含头像、简介、文章数） |
| `statistic.*` | `.Stats` (`ArticleCount`, `CommentCount`, `ViewCount`, `TagCount`, `UptimeDays`) | 全站公开统计指标（内存快照直出，零 SQL 开销） |
| `pageNavigations` | `.NavItems`（支持 `.Children` 二级菜单） | 自定义顶部导航栏 |
| `articles` + `pagination*` | `.ArticleCards` + `.Pagination` (`PageNumbers`, `PrevPage`, `NextPage`, `ShowFirst`, `ShowLast`) | 文章卡片列表与滑动窗口分页器 |
| `article` + `previousArticle` / `nextArticle` | `.Detail` + `.PrevPost` / `.NextPost` | 文章详情（已通过 `VisibilityGuard` 脱敏保护）与上下篇 |
| `relevantArticles` / `randomArticles` | `.RelatedPosts` / `.RandomPosts` | 相关文章与随机推荐（加密文章物理隔离零泄露） |
| `mostUsedCategories` / `mostUsedTags` / `archiveDates` | `.SidebarCategories` / `.SidebarTags` / `.SidebarArchives` | 侧边栏分类、热门标签、按月归档 |
| `recentArticles` / `mostViewCountArticles` / `mostCommentArticles` | `.RecentPosts` / `.MostViewedPosts` / `.MostCommentedPosts` | 侧边栏最新、热门阅读、热议文章榜单 |
| `recentComments` | `.RecentComments` | 最新公开评论动态 |

### 5.3 内置模板函数库 (`FuncMap`)

- **多语言与资源**：`t`、`tt`、`assetURL`、`coverSVG`
- **主题配置**：`themeCfg`、`themeCfgBool`、`themeCfgInt`
- **日期时间**：`formatDate` (`YYYY-MM-DD`)、`formatDotDate` (`YYYY.MM.DD`)、`formatDateTime`、`formatMonthDay`、`formatTime`、`nowYear`
- **集合与字典**：`dict`（向局部模板传参，如 `{{template "card" (dict "Post" . "Root" $)}}`）、`slice`、`seq`、`first`、`default`、`coalesce`
- **字符串与数值**：`hasPrefix`、`hasSuffix`、`contains`、`replace`、`split`、`join`、`lower`、`upper`、`trim`、`truncate`、`urlquery`、`add`、`sub`、`mul`、`div`、`mod`
- **安全类型标记**：`safeHTML`、`safeCSS`、`safeJS`、`safeURL`、`json`

---

## 6. 打包与后台一键热安装

1. 将主题目录打包为 `.zip`（支持根目录直接包含 `theme.json` 或单层顶层文件夹 `<theme-id>/theme.json`）：
   ```bash
   zip -r my-editorial-skin.zip theme.json preview.svg locales/ templates/ assets/
   ```
2. 登录管理后台 `/console` -> **「外观与扩展 -> 主题皮肤」**：
   - 点击右上角 **「上传安装主题 (.zip)」** 即可校验并热加载；
   - 点击卡片上的 **「预览」** 可使用管理员身份免切换实时预览（`/?skin=my-editorial-skin`）；
   - 点击 **「主题设置」** 配置 `config_schema` 表单项，确认无误后点击 **「启用主题」** 立即生效。
