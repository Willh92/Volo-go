# Language Pack Extension Guide

Volo-go uses a hot-reloadable JSON localization architecture. You can add new languages (such as `zh-TW`, `ja-JP`, `ko-KR`, `fr-FR`, `de-DE`, or RTL `ar-SA`) or customize built-in `zh-CN` / `en-US` wording without recompiling the Go binary or rebuilding the Admin SPA.

## 1. Installing & Exporting Language Packs

External language packs live in `./locales` by default (configurable via `VOLO_LOCALES_DIR`):

1. **Admin Console One-Click Install (Recommended)**: Open **Appearance & Extensions → Language Packs**, click **Export Full Template (.json)** to download a complete starter dictionary covering both public theme and Admin Console keys, translate your desired entries, and click **Install Pack (.json / .zip)** for instant hot-reload.
2. **Filesystem Drop-in**: Place `<code.json>` (e.g. `locales/ja-JP.json`) inside `./locales/` and click **Rescan Directory** in the Admin Console.

## 2. Single-File JSON Schema

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

### Key Rules
- **Graceful Fallback**: Any omitted key automatically falls back to the site default locale and built-in `zh-CN` / `en-US`, so partial overrides and incremental translations work seamlessly.
- **RTL Support**: Setting `"meta.dir": "rtl"` automatically switches `<html dir="rtl">` across public pages and the Admin Console.
- **Namespace Isolation**: Public navigation uses `"nav.*"`, while Admin Console sidebar items use `"admin.nav.*"` in unified single-file packs so both coexist without key collisions.
- **Per-Theme Locales**: Individual themes can also bundle `themes/<theme-id>/locales/<code.json>` accessed via `{{ tt "key" }}`.
