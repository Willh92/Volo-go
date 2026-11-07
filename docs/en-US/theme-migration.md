# Theme Authoring & Legacy Bolo-Solo Skin Migration Guide

Volo-go features a zero-dependency, pure-Go `html/template` theme engine. Alongside the embedded `volo-studio` flagship theme, Volo-go supports external themes under `./themes/<theme-id>` and hot `.zip` installation from the Admin Console — complete with per-theme JSON locale catalogs, modular partials, custom preview artwork, and declarative per-theme settings (`config_schema`).

---

## 1. Package Layout Mapping

| Legacy Bolo-Solo Skin (`skins/<name>/`) | Volo-go Theme (`themes/<id>/`) | Purpose |
| --- | --- | --- |
| Skin metadata configuration | `theme.json` | Manifest (`id`, localized `name` & `description`, `accents`, `supports_dark_mode`, `supports_pjax`, `config_schema`) |
| Legacy locale key-value bundles | `locales/zh-CN.json`, `locales/en-US.json` | UTF-8 JSON dictionaries resolved via `{{tt .ThemeID .Locale "key"}}` with automatic system fallback |
| `preview.png` / `preview.svg` | `preview.svg` / `preview.png` / `preview.webp` | Visual thumbnail served at `/themes/<id>/preview` in the Admin Theme Gallery |
| `header.ftl` / `footer.ftl` / `side.ftl` / `macro-*.ftl` | `templates/layout.html`, `templates/partials/*.html`, `templates/_*.html`, `templates/macro-*.html` | Shared layout and modular partial definitions compiled across all pages |
| `index.ftl` / `article.ftl` / `page.ftl` / `dynamic.ftl` | `templates/index.html`, `article.html`, `page.html`, `dynamic.html`, etc. | Page templates; unoverridden pages automatically fall back to built-in `volo-studio` |
| `css/*`, `js/*`, `images/*` | `assets/**` | Recursive static assets with SHA-256 cache busting (`{{assetURL .ThemeID "css/main.css"}}`) & pre-compressed Gzip |

---

## 2. `theme.json` & Declarative `config_schema`

Themes can declare custom settings in `theme.json` without any database schema changes. The Admin Console (**Appearance & Extensions -> Themes -> Theme Settings**) automatically renders a localized configuration modal and persists values in `Appearance.ThemeConfigs[<id>]`:

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
    "zh-CN": "双栏人文博客主题。",
    "en-US": "Two-column literary theme migrated from Bolo-Solo."
  },
  "accents": ["forest", "ocean", "teal", "amber", "plum", "rose"],
  "supports_dark_mode": true,
  "supports_pjax": true,
  "config_schema": [
    {
      "key": "hero_notice",
      "type": "textarea",
      "label": { "zh-CN": "首页公告文案", "en-US": "Hero Notice" },
      "default": ""
    },
    {
      "key": "show_site_stats",
      "type": "boolean",
      "label": { "zh-CN": "显示页脚统计", "en-US": "Show Footer Stats" },
      "default": true
    }
  ]
}
```

Supported field types: `text`, `textarea`, `url`, `color`, `boolean`, `number`, `select`, `sortable_list` (checkable and reorderable option list, e.g. `header_nav_items` driving `.NavMenu.Primary` / `.NavMenu.Overflow`).
Read theme config values inside templates with `{{themeCfg .ThemeConfig "hero_notice" ""}}`, `{{themeCfgBool .ThemeConfig "show_site_stats" true}}`, and `{{themeCfgInt .ThemeConfig "recent_count" 6}}`.

---

## 3. Two Template Composition Styles

1. **Master Layout Inheritance (Recommended)**:
   - Define `{{define "layout"}}...{{template "content" .}}...{{end}}` in `templates/layout.html`.
   - Define `{{define "content"}}...{{end}}` in each page template (`index.html`, `article.html`, etc.).
2. **Standalone Full-HTML Composition (1:1 `.ftl` Migration)**:
   - Omit `"content"` and write full HTML pages directly using `{{template "header" .}} ... {{template "footer" .}}` defined inside `templates/partials/*.html`.

---

## 4. Route-to-Template Fallback Matrix & Legacy URL Redirects

| Public Route | Theme-Specific Override Candidate | Default Fallback |
| --- | --- | --- |
| `/` | `index.html` | `index.html` |
| `/posts/{slug}` | `article.html` | `article.html` |
| `/{page-slug}` | `page.html` | `page.html` |
| `/categories` & `/categories/{slug}` | `categories.html` / `category_posts.html` | `taxonomies.html` / `index.html` |
| `/tags` & `/tags/{slug}` | `tags.html` / `tag_posts.html` | `taxonomies.html` / `index.html` |
| `/archives` & `/archives/{yyyy-mm}` | `archives.html` / `archive_posts.html` | `taxonomies.html` / `index.html` |
| `/series` & `/series/{slug}` | `series.html` / `series_detail.html` | `taxonomies.html` / `index.html` |
| `/dynamic` | `dynamic.html` | `taxonomies.html` |
| `/books` & `/books/{slug}` | `bookshelf.html` / `book_reader.html` | Built-in template |
| `/links` & `/follows` | `links.html` / `follows.html` | Built-in template |

Legacy `.html` endpoints (`/archives.html`, `/tags.html`, `/categories.html`, `/links.html`, `/follows.html`, `/dynamic.html`, and `/category/{slug}`) automatically issue `301 Moved Permanently` redirects to canonical clean URLs.

---

## 5. FreeMarker (`.ftl`) to `PageContext` Cheat Sheet

- **Site & Owner**: `.Site.Title`, `.Site.Subtitle`, `.Site.FooterText`, `.Owner`, `.Authors`, `.Stats` (`ArticleCount`, `CommentCount`, `ViewCount`, `TagCount`, `UptimeDays`).
- **Navigation**: `.NavItems` (with nested `.Children`), `.Locales`, `.Locale`.
- **Lists & Windowed Pagination**: `.ArticleCards`, `.Pagination` (`CurrentPage`, `TotalPages`, `PrevPage`, `NextPage`, `PageNumbers`, `ShowFirst`, `ShowLast`).
- **Article Detail & Recommendations**: `.Detail` (`VisibilityGuard`-redacted HTML & TOC), `.PrevPost`, `.NextPost`, `.RelatedPosts`, `.RandomPosts`, `.Backlinks`, `.SeriesContext`.
- **Sidebar Modules**: `.SidebarCategories`, `.SidebarTags`, `.SidebarArchives`, `.RecentPosts`, `.MostViewedPosts`, `.MostCommentedPosts`, `.RecentComments`.
- **Template Helpers**: `t`, `tt`, `assetURL`, `coverSVG`, `themeCfg`, `themeCfgBool`, `themeCfgInt`, `formatDate`, `formatDotDate`, `formatDateTime`, `formatMonthDay`, `formatTime`, `dict`, `slice`, `seq`, `first`, `default`, `coalesce`, `truncate`, `add`, `sub`, `mul`, `div`, `mod`, `safeHTML`, `safeCSS`, `safeJS`, `safeURL`, `json`.

---

## 6. Packaging & Hot Installation

```bash
zip -r my-editorial-skin.zip theme.json preview.svg locales/ templates/ assets/
```

Upload the `.zip` archive in **Admin Console -> Appearance & Extensions -> Themes**, preview live via **Preview (`/?skin=my-editorial-skin`)**, configure `config_schema` options, and click **Activate**.
