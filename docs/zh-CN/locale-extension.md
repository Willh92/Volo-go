# 语言包扩展指南 (Locale Pack Extensions)

> English version: [Locale Pack Extension Guide](../en-US/locale-extension.md)

`Volo-go` 采用纯 JSON 驱动的双层热加载多语言架构，无需重新编译二进制文件或前端工程，即可自由扩展任意新语种（如 `zh-TW`、`ja-JP`、`ko-KR`、`fr-FR`、`ar-SA`）或微调内置中英文词条。

---

## 1. 目录约定、热安装与导出方式

默认外部语言包目录为 `./locales`（可通过 CLI `--locales-dir` 或环境变量 `VOLO_LOCALES_DIR` 指定）。您可以通过以下任一方式接入与维护语言包：

1. **后台可视化安装与导出（推荐）**：进入管理工作台（默认 `/console`，或通过 `security.admin_path` 自定义的动态隐身路径）**「外观与扩展 → 语言包扩展」**：
   - 点击 **「导出完整模板 (.json)」**（`GET /api/admin/locales/{code}/export`）下载包含前台主题与后台工作台全部词条的基准 JSON；
   - 翻译后点击 **「安装语言包 (.json / .zip)」**（`POST /api/admin/locales/install?overwrite=true`）即可即时热生效并支持原位覆盖升级。
2. **目录放置扫描**：将 `<语言代码>.json`（例如 `locales/ja-JP.json`）放入 `./locales/` 目录，在后台点击 **「重新扫描目录」**（`POST /api/admin/locales/reload`）或重启服务即可自动识别。

---

## 2. 单文件语言包 JSON 结构

每个语言包是一个扁平键值 UTF-8 JSON 文件，包含元数据键（`meta.*`）、前台与接口词条，以及后台工作台词条：

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

### 关键规则
- **自动回退链**：未翻译或缺省的键会自动回退至站点默认语言及内置 `zh-CN` / `en-US`（`internal/i18n/builtin/{zh-CN,en-US}.json`），支持渐进式增量翻译（哪怕只写 10 个键也能安全运行）。
- **RTL 从右向左排版**：将 `"meta.dir"` 设为 `"rtl"`（如阿拉伯语 `ar-SA`、希伯来语 `he-IL`），前台主题与后台控制台会自动切换文档流方向（`dir="rtl"`）。
- **前后缀无冲突共存**：前台导航使用 `"nav.*"`（如 `"nav.categories": "分类"`），后台侧栏导航在单文件扩展包中使用 `"admin.nav.*"`（如 `"admin.nav.categories": "分类管理"`），其余后台命名空间（`shell.*`、`ov.*`、`art.*`、`cnt.*`、`dist.*`、`app.*`、`sys.*`、`tbl.*`、`common.*`）可直接书写或加 `admin.` 前缀。
- **主题专属语言包**：独立主题皮肤也可在 `themes/<theme-id>/locales/<code>.json` 内自带专属词条，模板中通过 `{{tt .ThemeID .Locale "hero_tagline"}}` 调用（缺省时自动回退系统词典 `{{t .Locale "key"}}`）。
