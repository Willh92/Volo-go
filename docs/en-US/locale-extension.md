# Locale Pack Extension Guide

> 中文版本：[语言包扩展指南](../zh-CN/locale-extension.md)

`Volo-go` uses a pure-JSON, hot-reloadable dual-layer internationalization architecture. You can add any new language (e.g., `zh-TW`, `ja-JP`, `ko-KR`, `fr-FR`, `ar-SA`) or customize built-in Chinese/English strings at runtime without recompiling the Go binary or the Admin SPA.

---

## 1. Directory Convention, Hot Installation & Export

External locale packs live in `./locales` by default (configurable via CLI `--locales-dir` or environment variable `VOLO_LOCALES_DIR`). You can install and maintain locale packs in two ways:

1. **Admin Console UI Install & Export (Recommended)**: Open the Admin Console (default `/console`, or your custom dynamic stealth path configured via `security.admin_path`) -> **Appearance & Extensions -> Locale Packs**:
   - Click **Export Starter Template (.json)** (`GET /api/admin/locales/{code}/export`) to download a complete reference JSON containing both public theme and Admin Console keys;
   - Translate the file and click **Install Locale Pack (.json / .zip)** (`POST /api/admin/locales/install?overwrite=true`) to hot-activate or upgrade in place immediately.
2. **Directory Drop-in**: Place `<code>.json` (e.g., `locales/ja-JP.json`) inside `./locales/`, then click **Rescan Directory** (`POST /api/admin/locales/reload`) in the Admin Console or restart the server.

---

## 2. Single-File JSON Schema

Each locale pack is a flat key-value UTF-8 JSON object containing metadata (`meta.*`), public/API keys, and Admin Console keys:

```json
{
  "meta.code": "ja-JP",
  "meta.name": "日本語",
  "meta.dir": "ltr",
  "meta.author": "Community Contributor",
  "meta.version": "1.0.0",

  "nav.home": "ホーム",
  "nav.categories": "カテゴリー",
  "post.read_more": "続きを読む",

  "shell.console_title": "Volo 管理コンソール",
  "admin.nav.categories": "カテゴリー管理",
  "common.save": "変更を保存"
}
```

### Core Rules
- **Automatic Fallback Chain**: Any omitted or untranslated key automatically falls back to the site default locale and built-in `zh-CN` / `en-US` catalogs (`internal/i18n/builtin/{zh-CN,en-US}.json`), enabling incremental translation (even a 10-key override file runs safely).
- **RTL Layout Support**: Set `"meta.dir": "rtl"` (for Arabic `ar-SA`, Hebrew `he-IL`, etc.) to automatically switch both the public theme and Admin Console into right-to-left layout (`dir="rtl"`).
- **Collision-Free Namespacing**: Public navigation uses `"nav.*"` (e.g., `"nav.categories"`), while Admin sidebar navigation in single-file packs uses `"admin.nav.*"` (e.g., `"admin.nav.categories"`). Other Admin Console namespaces (`shell.*`, `ov.*`, `art.*`, `cnt.*`, `dist.*`, `app.*`, `sys.*`, `tbl.*`, `common.*`) work either bare or prefixed with `admin.`.
- **Theme-Scoped Catalogs**: Standalone themes can also ship `themes/<theme-id>/locales/<code>.json` and read theme-scoped strings via `{{tt .ThemeID .Locale "hero_tagline"}}` (falling back automatically to the system catalog `{{t .Locale "key"}}`).
