# Theme Authoring & Legacy Bolo-Solo Skin Migration Guide

> 中文版本：[主题开发与旧版 Bolo-Solo 皮肤迁移指南](../zh-CN/theme-migration.md)

`Volo-go` builds a zero-dependency, high-performance theme engine atop Go's standard `html/template`. Alongside the built-in flagship `volo-studio` theme (and the official external magazine theme [`themes/volo-journal`](../../themes/volo-journal/)), it supports external themes in `./themes/<theme-id>`, hot `.zip` installation, in-place `.zip` upgrades (`overwrite=true`), clean starter `.zip` export, standalone JSON locale dictionaries, modular partials, selectable standalone page layouts (`page_templates`), timeline list styles (`Reading.ListStyle`), custom site template variables (`{{customVar}}`), and declarative theme configuration forms (`config_schema`).

---

## 1. Theme Package Directory Comparison

| Legacy Bolo-Solo Skin (`skins/<name>/`) | Volo-go Theme (`themes/<id>/`) | Notes |
| --- | --- | --- |
| `skin.properties` metadata | `theme.json` | Theme ID, localized name/description, accents, dark/PJAX capabilities, `page_templates`, and `config_schema` |
| `lang_*.properties` i18n files | `locales/zh-CN.json`, `locales/en-US.json` | UTF-8 JSON catalogs accessed via `{{tt .ThemeID .Locale "key"}}` with automatic system fallback |
| `preview.png` / `preview.svg` | `preview.svg` / `preview.png` / `preview.webp` | Admin theme gallery thumbnail (served automatically at `/themes/<id>/preview`) |
| `header.ftl` / `footer.ftl` / `side.ftl` / `macro-*.ftl` | `templates/layout.html`, `templates/partials/*.html`, `templates/_*.html`, `templates/macro-*.html` | Shared skeleton and partials compiled across all page templates |
| `index.ftl` / `article.ftl` / `page.ftl` / `dynamic.ftl`, etc. | `templates/index.html`, `article.html`, `page.html`, `page-<tpl>.html`, `dynamic.html`, `error.html`, etc. | Page templates; unoverridden pages fall back automatically to built-in `volo-studio` templates |
| `css/*.css`, `js/*.js`, `images/*` | `assets/**` | Nested subdirectories, SHA-256 cache-busted URLs (`{{assetURL .ThemeID "css/main.css"}}`), and pre-compressed Gzip |

---

## 2. `theme.json` Manifest, `page_templates` & Declarative `config_schema`

External themes never need custom database tables. Declare `page_templates` (selectable layouts for standalone Pages) and `config_schema` (theme settings) inside `theme.json`, and the Admin Console (**Appearance & Extensions -> Themes -> Theme Settings**) automatically renders a localized visual configuration modal persisted under `Appearance.ThemeConfigs[<id>]`:

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

Supported `config_schema` field types: `string` / `text`, `textarea`, `url`, `image`, `color`, `boolean`, `number` (with optional `min`/`max`/`step`), `select`, and `sortable_list` (toggleable, reorderable multi-item lists such as `header_nav_items`).

Reading theme settings, custom site variables, and list styles in templates:
- Theme string setting: `{{themeCfg .ThemeConfig "hero_notice" ""}}`
- Theme boolean setting: `{{if themeCfgBool .ThemeConfig "show_site_stats" true}}...{{end}}`
- Theme integer setting: `{{themeCfgInt .ThemeConfig "recent_count" 6}}`
- Site custom template variables (compatible with legacy `customVars`, managed in **Admin Console -> Site Settings -> Custom Template Variables** `site.custom_vars`): `{{customVar .Site.CustomVars "icp_number" ""}}`
- Article list presentation style (compatible with legacy `articleListStyle`, backed by `reading.list_style`: `summary` | `title_only` | `full`): when `.Reading.ListStyle` is `"full"`, public `.ArticleCards` automatically carry sanitized `.FullHTML` while password-protected posts and locked `:::lock` blocks remain strictly redacted by `VisibilityGuard`.

---

## 3. Template Organization & Standalone Frameless `error.html` Override

The Volo-go theme engine supports modern layout inheritance, legacy `.ftl`-style standalone partial composition, and frameless full-page error overrides:

1. **Master Layout Inheritance (Recommended)**:
   - Define `{{define "layout"}}...{{template "content" .}}...{{end}}` inside `templates/layout.html`.
   - Define `{{define "content"}}...{{end}}` inside each page template (e.g., `templates/index.html`).
2. **Standalone Full-HTML Composition (Ideal for Direct `.ftl` Porting)**:
   - If a page template does not define `"content"`, the engine executes the page template file directly as the root entry point.
   - Define `{{define "header"}}` / `{{define "footer"}}` in `templates/partials/header.html` and `footer.html`, then invoke `{{template "header" .}} ... {{template "footer" .}}` directly inside `index.html` — matching legacy `<#include "header.ftl">`.
3. **Standalone Frameless `error.html` Override**:
   - Even when a theme uses `templates/layout.html` inheritance, `templates/error.html` can redefine `{{define "layout"}}<!doctype html>...</html>{{end}}{{define "content"}}{{end}}` (or omit `"content"` entirely and emit a full HTML document) to bypass standard header/sidebar chrome and render a custom frameless `404` / `403` / `429` / `500` canvas (used by built-in `volo-studio` for its interactive search-enabled error screen).

---

## 4. Page Template Routing & Automatic Fallback Matrix

External themes can override any granular view; unoverridden templates fall back safely in candidate order:

| Route | Preferred Granular Template | Fallback Template |
| --- | --- | --- |
| `/` | `index.html` (also supports external `home.html`) | `index.html` |
| `/posts/{slug}` | `article.html` | `article.html` |
| `/{page-slug}` | `page-<template>.html` (from the page's selected `page_templates` value) | `page.html` |
| `/categories` | `categories.html` | `taxonomies.html` |
| `/categories/{slug}` | `category_posts.html` | `index.html` |
| `/tags` | `tags.html` | `taxonomies.html` |
| `/tags/{slug}` | `tag_posts.html` | `index.html` |
| `/archives` | `archives.html` | `taxonomies.html` |
| `/archives/{yyyy-mm}` | `archive_posts.html` | `index.html` |
| `/series` | `series.html` | `taxonomies.html` |
| `/series/{slug}` | `series_detail.html` | `index.html` |
| `/dynamic` | `dynamic.html` | `taxonomies.html` |
| `/books` / `/books/{slug}` | `bookshelf.html` / `book_reader.html` | Built-in template of same name |
| `/links` / `/follows` | `links.html` / `follows.html` | Built-in template of same name |
| `/reader/login` / Error pages | `reader_auth.html` / `error.html` | Built-in template of same name |

In addition, legacy static-suffix routes (`/archives.html`, `/tags.html`, `/categories.html`, `/links.html`, `/dynamic.html`, `/category/{slug}`) are automatically `301` canonicalized to clean modern URLs, guaranteeing zero broken links from migrated skins or historical backlinks.

---

## 5. FreeMarker (`.ftl`) Syntax & `PageContext` Mapping Cheat Sheet

### 5.1 Syntax Mapping

| FreeMarker (`.ftl`) | Volo-go (`html/template`) |
| --- | --- |
| `${site.title}` | `{{.Site.Title}}` |
| `${customVars["icp"]!""}` | `{{customVar .Site.CustomVars "icp" ""}}` |
| `${i18nLabel}` | `{{tt .ThemeID .Locale "skin.label"}}` or `{{t .Locale "nav.home"}}` |
| `<#if cond> ... <#elseif c2> ... <#else> ... </#if>` | `{{if cond}} ... {{else if c2}} ... {{else}} ... {{end}}` |
| `<#list articles as article> ... </#list>` | `{{range .ArticleCards}} ... {{end}}` |
| `<#include "side.ftl">` | `{{template "side" .}}` (with `{{define "side"}}` in `partials/side.html`) |
| `${article.createDate?string("yyyy.MM.dd")}` | `{{formatDotDate .PublishedAt}}` or `{{formatTime .PublishedAt "2006.01.02"}}` |
| `${article.content}` (redacted HTML) | `{{safeHTML .Detail.HTML}}` (or `{{safeHTML .FullHTML}}` in `full` list mode) |

### 5.2 Core Data Model (`PageContext`) Mapping

| Legacy Skin Model Field | Volo-go `PageContext` Field | Notes |
| --- | --- | --- |
| `blogTitle` / `blogSubtitle` | `.Site.Title` / `.Site.Subtitle` | Site primary title and subtitle |
| `customVars` | `.Site.CustomVars` (via `{{customVar .Site.CustomVars "key" "def"}}`) | Arbitrary Admin-managed custom key-value template variables |
| `noticeBoard` / `footerContent` | `.Site.Description` (or theme config) / `.Site.FooterText` | Site notice copy and footer HTML/text |
| `articleListStyle` | `.Reading.ListStyle` (`summary` \| `title_only` \| `full`) | Excerpt cards, title-only directory, or full-article stream |
| `adminUser` / `users` | `.Owner` / `.Authors` | Site owner and public author profiles (avatar, bio, article counts) |
| `statistic.*` | `.Stats` (`ArticleCount`, `CommentCount`, `ViewCount`, `TagCount`, `UptimeDays`) | Public site statistics served from zero-SQL in-memory snapshot |
| `pageNavigations` | `.NavItems` / `.NavMenu` (`.Primary`, `.Overflow`, `.OverflowActive`) | Custom navigation items and responsive overflow menu |
| `articles` + `pagination*` | `.ArticleCards` + `.Pagination` (`PageNumbers`, `PrevPage`, `NextPage`, `ShowFirst`, `ShowLast`) | Article cards and sliding-window paginator |
| `article` + `previousArticle` / `nextArticle` | `.Detail` + `.PrevPost` / `.NextPost` | Article detail (sanitized & redacted by `VisibilityGuard`) and adjacent posts |
| `relevantArticles` / `randomArticles` | `.RelatedPosts` / `.RandomPosts` | Related and random posts (password-protected posts physically excluded) |
| `mostUsedCategories` / `mostUsedTags` / `archiveDates` | `.SidebarCategories` / `.SidebarTags` / `.SidebarArchives` | Sidebar categories, popular tags, and monthly archives |
| `recentArticles` / `mostViewCountArticles` / `mostCommentArticles` | `.RecentPosts` / `.MostViewedPosts` / `.MostCommentedPosts` | Sidebar recent, most-viewed, and most-commented rankings |
| `recentComments` | `.RecentComments` | Recent approved public comments |

### 5.3 Built-in Template Function Library (`FuncMap`)

- **i18n, Site Vars & Assets**: `t`, `tt`, `customVar`, `assetURL`, `coverSVG`, `pageURL`
- **Theme Config**: `themeCfg`, `themeCfgBool` (alias `themeBool`), `themeCfgInt` (alias `themeInt`)
- **Date & Time**: `formatDate` (`YYYY-MM-DD`), `formatDotDate` (`YYYY.MM.DD`), `formatDateTime`, `formatMonthDay`, `formatTime`, `nowYear`
- **Collections & Dictionaries**: `dict` (pass named arguments to partials, e.g., `{{template "card" (dict "Post" . "Root" $)}}`), `slice`, `seq`, `first`, `default`, `coalesce`
- **Strings & Math**: `hasPrefix`, `hasSuffix`, `contains`, `replace`, `split`, `join`, `lower`, `upper`, `trim`, `truncate`, `urlquery`, `add`, `sub`, `mul`, `div`, `mod`
- **Safe Output Helpers**: `safeHTML`, `proseHTML`, `renderNotice`, `safeCSS`, `safeJS`, `safeURL`, `json`

---

## 6. Starter Export, Packaging & Hot `.zip` Installation / Upgrade

1. **Export a Clean Starter `.zip` (Recommended Starting Point)**:
   - Open the Admin Console (default `/console`, or your custom dynamic stealth path configured via `security.admin_path`) -> **Appearance & Extensions -> Themes**;
   - Click **Export (.zip)** (`GET /api/admin/themes/{id}/export`) on built-in `volo-studio` or any installed theme to download a clean starter `.zip` archive free of temporary or runtime artifacts.
2. **Package a Theme Directory Manually** (`theme.json` at archive root or inside a single top-level `<theme-id>/theme.json` folder):
   ```bash
   zip -r my-editorial-skin.zip theme.json preview.svg locales/ templates/ assets/
   ```
3. **Hot Install, In-Place Upgrade & Live Preview**:
   - Click **Install Theme (.zip)** (`POST /api/admin/themes/install?overwrite=true`) to validate, hot-load, or upgrade an existing theme in place while preserving saved `Appearance.ThemeConfigs[<id>]` values;
   - Click **Preview** on any theme card to live-preview desktop (`/?skin=my-editorial-skin`) or mobile (`/?skin=my-editorial-skin&device=mobile`) as an administrator without switching the live site;
   - Click **Theme Settings** to customize `config_schema` fields, then click **Activate** to go live immediately.
