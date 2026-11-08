import { signal } from "@preact/signals";

export type LocaleCode = string;

export interface LocaleOption {
  code: string;
  name: string;
  dir: string;
}

// Auto-discover all built-in per-language JSON catalogs in `./locales/*.json` at build time.
// Adding a new built-in language (e.g. `src/locales/zh-TW.json` or `src/locales/ja-JP.json`)
// requires zero changes to TypeScript code.
import { codes as bundledCodes, columns as bundledCols, keys as bundledKeys } from "virtual:volo-locales";

const bundledCatalogs: Record<string, Record<string, string>> = {};
const catalogs: Record<string, Record<string, string>> = {};
const initialLocales: LocaleOption[] = [];

for (let ci = 0; ci < bundledCodes.length; ci++) {
  const col = bundledCols[ci];
  const table: Record<string, string> = {};
  for (let ki = 0; ki < bundledKeys.length; ki++) {
    const val = col[ki];
    if (val) table[bundledKeys[ki]] = val;
  }
  const code = canonicalizeLocaleTag(table["meta.code"] || bundledCodes[ci]);
  if (!code) continue;
  bundledCatalogs[code] = table;
  catalogs[code] = { ...table };
  initialLocales.push({
    code,
    name: table["meta.name"] || code,
    dir: table["meta.dir"] || "ltr",
  });
}

function sortLocaleOptions(items: LocaleOption[]): LocaleOption[] {
  return [...items].sort((a, b) => {
    if (a.code === "zh-CN") return -1;
    if (b.code === "zh-CN") return 1;
    if (a.code === "en-US") return -1;
    if (b.code === "en-US") return 1;
    return a.code.localeCompare(b.code);
  });
}

export const availableLocalesSig = signal<LocaleOption[]>(sortLocaleOptions(initialLocales));
export const localeSig = signal<LocaleCode>("zh-CN");
const catalogVersionSig = signal<number>(0);
const loadedRuntimeOverlays = new Set<string>();

export function canonicalizeLocaleTag(raw?: string): string {
  const s = (raw || "").trim().replaceAll("_", "-");
  if (!s) return "";
  const parts = s.split("-");
  const lang = parts[0].toLowerCase();
  if (lang === "zh") {
    const sub = (parts[1] || "").toLowerCase();
    if (sub === "tw" || sub === "hant") return "zh-TW";
    if (sub === "hk" || sub === "mo") return `zh-${sub.toUpperCase()}`;
    return "zh-CN";
  }
  if (lang === "en") {
    const sub = (parts[1] || "").toLowerCase();
    if (sub && sub !== "us") return `en-${sub.toUpperCase()}`;
    return "en-US";
  }
  if (parts.length === 1) {
    return lang;
  }
  if (parts[1].length === 4) {
    const script = parts[1][0].toUpperCase() + parts[1].slice(1).toLowerCase();
    return parts[2] ? `${lang}-${script}-${parts[2].toUpperCase()}` : `${lang}-${script}`;
  }
  return `${lang}-${parts[1].toUpperCase()}`;
}

export function normalizeLocale(raw?: string): LocaleCode {
  const norm = canonicalizeLocaleTag(raw);
  if (!norm) return "zh-CN";
  const known = availableLocalesSig.value;
  if (known.some((l) => l.code === norm)) {
    return norm;
  }
  const prefix = norm.split("-")[0].toLowerCase();
  const byPrefix = known.find((l) => l.code.split("-")[0].toLowerCase() === prefix);
  if (byPrefix) {
    return byPrefix.code;
  }
  return catalogs[norm] ? norm : "zh-CN";
}

// registerExternalLocales merges server-discovered locales from `VOLO_LOCALES_DIR` (`./locales/*.json`)
// so community language packs appear automatically in all Admin/Setup language switchers without rebuilding.
export function registerExternalLocales(list?: LocaleOption[]) {
  if (!Array.isArray(list) || list.length === 0) return;
  const merged = new Map<string, LocaleOption>();
  for (const item of availableLocalesSig.value) {
    merged.set(item.code, item);
  }
  for (const raw of list) {
    const code = canonicalizeLocaleTag(raw.code);
    if (!code) continue;
    merged.set(code, {
      code,
      name: raw.name || merged.get(code)?.name || code,
      dir: raw.dir || merged.get(code)?.dir || "ltr",
    });
  }
  availableLocalesSig.value = sortLocaleOptions(Array.from(merged.values()));
}

// syncExternalLocales reconciles availableLocalesSig with the authoritative server locale list
// (used after hot-installing or uninstalling external language packs).
export function syncExternalLocales(list?: LocaleOption[]) {
  const merged = new Map<string, LocaleOption>();
  for (const item of initialLocales) {
    merged.set(item.code, item);
  }
  if (Array.isArray(list)) {
    for (const raw of list) {
      const code = canonicalizeLocaleTag(raw.code);
      if (!code) continue;
      merged.set(code, {
        code,
        name: raw.name || merged.get(code)?.name || code,
        dir: raw.dir || merged.get(code)?.dir || "ltr",
      });
    }
  }
  // Clean cached overlays so updated/removed packs reflect immediately
  loadedRuntimeOverlays.clear();
  for (const code of Object.keys(catalogs)) {
    if (bundledCatalogs[code]) {
      catalogs[code] = { ...bundledCatalogs[code] };
    } else if (!merged.has(code)) {
      delete catalogs[code];
    }
  }
  availableLocalesSig.value = sortLocaleOptions(Array.from(merged.values()));
  if (!merged.has(localeSig.value)) {
    localeSig.value = "zh-CN";
  }
  catalogVersionSig.value++;
}

// ensureLocaleLoaded fetches runtime JSON overrides/packs from `/api/v1/i18n/{code}` when needed
// (e.g. external `./locales/zh-TW.json` or `ja-JP.json` installed in `VOLO_LOCALES_DIR`).
export async function ensureLocaleLoaded(rawCode: string, forceReload = false): Promise<LocaleCode> {
  const code = normalizeLocale(rawCode);
  localeSig.value = code;
  if (!forceReload && loadedRuntimeOverlays.has(code)) {
    return code;
  }
  loadedRuntimeOverlays.add(code);
  try {
    const res = await fetch(`/api/v1/i18n/${encodeURIComponent(code)}`, { credentials: "same-origin" });
    if (res.ok) {
      const data = (await res.json()) as {
        locales?: LocaleOption[];
        messages?: Record<string, string>;
        custom_messages?: Record<string, string>;
      };
      if (data.locales) {
        registerExternalLocales(data.locales);
      }
      const isBundled = Boolean(bundledCatalogs[code]);
      const source = isBundled ? data.custom_messages : data.custom_messages || data.messages;
      if (source && typeof source === "object") {
        const base = bundledCatalogs[code] ? { ...bundledCatalogs[code] } : catalogs[code] || {};
        const overlay: Record<string, string> = { ...base };
        for (const [k, v] of Object.entries(source)) {
          if (typeof v !== "string" || !v) continue;
          if (k.startsWith("admin.")) {
            overlay[k.slice("admin.".length)] = v;
          } else if (k.startsWith("nav.")) {
            // Public `nav.*` belongs to blog theme navigation; admin sidebar uses `admin.nav.*` in single-file packs.
            continue;
          } else if (
            base[k] !== undefined ||
            !bundledCatalogs["en-US"] ||
            bundledCatalogs["en-US"][k] !== undefined ||
            k.startsWith("meta.")
          ) {
            overlay[k] = v;
          }
        }
        catalogs[code] = overlay;
        catalogVersionSig.value++;
      }
    }
  } catch {
    // Offline or non-overlay environment: bundled JSON catalog remains authoritative.
  }
  return code;
}

// exportFullTemplateJSON merges public/backend keys from `/api/v1/i18n/{baseCode}` with all Admin SPA keys
// into a single unified starter JSON file for creating new community language packs.
export async function exportFullTemplateJSON(baseCode: "zh-CN" | "en-US"): Promise<string> {
  const merged: Record<string, string> = {
    "meta.code": baseCode,
    "meta.name": baseCode === "zh-CN" ? "简体中文" : "English",
    "meta.dir": "ltr",
    "meta.author": "Volo Community",
    "meta.version": "1.0.0",
  };
  try {
    const res = await fetch(`/api/v1/i18n/${encodeURIComponent(baseCode)}`, { credentials: "same-origin" });
    if (res.ok) {
      const data = await res.json();
      if (data.messages && typeof data.messages === "object") {
        for (const [k, v] of Object.entries(data.messages as Record<string, string>)) {
          merged[k] = v;
        }
      }
    }
  } catch {
    // ignore fetch error; export admin keys
  }
  const adminDict = bundledCatalogs[baseCode] || bundledCatalogs["en-US"] || {};
  for (const [k, v] of Object.entries(adminDict)) {
    if (k.startsWith("meta.")) continue;
    if (k.startsWith("nav.")) {
      merged[`admin.${k}`] = v;
    } else {
      merged[k] = v;
    }
  }
  const sorted: Record<string, string> = {};
  for (const k of Object.keys(merged).sort((a, b) => {
    const am = a.startsWith("meta.");
    const bm = b.startsWith("meta.");
    if (am !== bm) return am ? -1 : 1;
    return a.localeCompare(b);
  })) {
    sorted[k] = merged[k];
  }
  return JSON.stringify(sorted, null, 2) + "\n";
}

// exportLocalePackJSON fetches `/api/admin/locales/{code}/export` and merges any missing bundled Admin SPA keys
// so row-level exported language packs always include the full public + admin keyset (~1,380+ keys).
export async function exportLocalePackJSON(rawCode: string): Promise<string> {
  const code = canonicalizeLocaleTag(rawCode) || rawCode;
  const merged: Record<string, string> = {
    "meta.code": code,
    "meta.name": availableLocalesSig.value.find((l) => l.code === code)?.name || code,
    "meta.dir": getLocaleDir(code),
  };
  try {
    const res = await fetch(`/api/admin/locales/${encodeURIComponent(code)}/export`, {
      credentials: "same-origin",
    });
    if (res.ok) {
      const data = await res.json();
      if (data && typeof data === "object") {
        for (const [k, v] of Object.entries(data as Record<string, unknown>)) {
          if (typeof v === "string") merged[k] = v;
        }
      }
    }
  } catch {
    // Fallback to loaded/bundled catalog below
  }
  const fallbackAdmin =
    bundledCatalogs[code] ||
    catalogs[code] ||
    (code.startsWith("zh") ? bundledCatalogs["zh-CN"] : bundledCatalogs["en-US"]) ||
    bundledCatalogs["en-US"] ||
    {};
  const enAdmin = bundledCatalogs["en-US"] || {};
  const allAdminKeys = new Set([...Object.keys(enAdmin), ...Object.keys(fallbackAdmin)]);
  for (const k of allAdminKeys) {
    if (k.startsWith("meta.")) continue;
    const targetKey = k.startsWith("nav.") ? `admin.${k}` : k;
    if (!merged[targetKey]) {
      const val = catalogs[code]?.[k] || fallbackAdmin[k] || enAdmin[k];
      if (val) merged[targetKey] = val;
    }
  }
  const sorted: Record<string, string> = {};
  for (const k of Object.keys(merged).sort((a, b) => {
    const am = a.startsWith("meta.");
    const bm = b.startsWith("meta.");
    if (am !== bm) return am ? -1 : 1;
    return a.localeCompare(b);
  })) {
    sorted[k] = merged[k];
  }
  return JSON.stringify(sorted, null, 2) + "\n";
}

export function getLocaleDir(code?: string): string {
  const target = code || localeSig.value;
  return (
    availableLocalesSig.value.find((l) => l.code === target)?.dir ||
    catalogs[target]?.["meta.dir"] ||
    "ltr"
  );
}

export function tFor(locale: string, key: string, vars?: Record<string, string | number>): string {
  void catalogVersionSig.value;
  let tpl =
    catalogs[locale]?.[key] ??
    catalogs["zh-CN"]?.[key] ??
    catalogs["en-US"]?.[key] ??
    key;
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      tpl = tpl.replaceAll(`{${k}}`, String(v));
    }
  }
  return tpl;
}

export function t(key: string, vars?: Record<string, string | number>): string {
  return tFor(localeSig.value, key, vars);
}

export function formatDateTime(val?: number | string): string {
  const ms = typeof val === "string" ? Date.parse(val) : val;
  if (!ms || Number.isNaN(ms) || ms <= 0) return "—";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const BACKUP_TABLE_KEYS: Record<string, string> = {
  settings: "bak.tbl.settings",
  users: "bak.tbl.users",
  user_credentials: "bak.tbl.user_credentials",
  user_sessions: "bak.tbl.user_sessions",
  api_tokens: "bak.tbl.api_tokens",
  categories: "bak.tbl.categories",
  tags: "bak.tbl.tags",
  articles: "bak.tbl.articles",
  article_tags: "bak.tbl.article_tags",
  article_revisions: "bak.tbl.revisions",
  revisions: "bak.tbl.revisions",
  article_backlinks: "bak.tbl.article_backlinks",
  pages: "bak.tbl.pages",
  nav_items: "bak.tbl.nav_items",
  series: "bak.tbl.series",
  series_articles: "bak.tbl.series_articles",
  books: "bak.tbl.books",
  book_chapters: "bak.tbl.book_chapters",
  comments: "bak.tbl.comments",
  reactions: "bak.tbl.reactions",
  links: "bak.tbl.links",
  follows: "bak.tbl.follows",
  follow_items: "bak.tbl.follow_items",
  media_assets: "bak.tbl.media_assets",
  redirects: "bak.tbl.redirects",
  webhooks: "bak.tbl.webhooks",
  webhook_deliveries: "bak.tbl.webhook_deliveries",
  analytics_daily: "bak.tbl.analytics_daily",
  analytics_top_items: "bak.tbl.analytics_top_items",
  security_ip_bans: "bak.tbl.security_ip_bans",
  security_geo_rules: "bak.tbl.security_geo_rules",
  audit_logs: "bak.tbl.audit_logs",
  plugins: "bak.tbl.plugins",
  plugin_kv: "bak.tbl.plugin_kv",
};

export function localizeBackupTable(name?: string): string {
  const raw = String(name || "").trim();
  if (!raw) return "";
  const key = BACKUP_TABLE_KEYS[raw.toLowerCase()];
  return key ? t(key) : raw;
}

const AUDIT_RESOURCE_KEYS: Record<string, string> = {
  article: "audit.res.article",
  page: "audit.res.page",
  category: "audit.res.category",
  tag: "audit.res.tag",
  series: "audit.res.series",
  book: "audit.res.book",
  comment: "audit.res.comment",
  media_asset: "audit.res.media",
  media: "audit.res.media",
  user: "audit.res.user",
  user_session: "audit.res.session",
  user_credential: "audit.res.credential",
  api_token: "audit.res.api_token",
  settings: "audit.res.settings",
  system: "audit.res.system",
  theme: "audit.res.theme",
  plugin: "audit.res.plugin",
  locale: "audit.res.locale",
  ip_ban: "audit.res.ip_ban",
  geo_rule: "audit.res.geo_rule",
  link: "audit.res.link",
  follow: "audit.res.follow",
  nav_item: "audit.res.nav",
  nav: "audit.res.nav",
  redirect: "audit.res.redirect",
  webhook: "audit.res.webhook",
};

export function localizeAuditResource(res?: string): string {
  const raw = String(res || "").trim();
  if (!raw) return "";
  const key = AUDIT_RESOURCE_KEYS[raw.toLowerCase()];
  return key ? t(key) : raw;
}

const AUDIT_ACTION_KEYS: Record<string, string> = {
  "auth.login": "audit.act.auth_login",
  "auth.login_2fa": "audit.act.auth_login_2fa",
  "auth.login_passkey": "audit.act.auth_login_passkey",
  "auth.login_oidc": "audit.act.auth_login_oidc",
  "auth.logout": "audit.act.auth_logout",
  "auth.password_change": "audit.act.auth_pwd_change",
  "reader.register_oidc": "audit.act.reader_reg_oidc",
  "settings.patch": "audit.act.settings_patch",
  "article.create": "audit.act.article_create",
  "article.update": "audit.act.article_update",
  "article.save": "audit.act.article_save",
  "article.delete": "audit.act.article_delete",
  "article.pin": "audit.act.article_pin",
  "article.batch": "audit.act.article_batch",
  "article.batch_import": "audit.act.article_import",
  "page.create": "audit.act.page_create",
  "page.update": "audit.act.page_update",
  "page.delete": "audit.act.page_delete",
  "tags.cleanup": "audit.act.tags_cleanup",
  "tags.merge": "audit.act.tags_merge",
  "tag.batch_delete": "audit.act.tag_batch_del",
  "comment.moderate": "audit.act.comment_mod",
  "comment.delete": "audit.act.comment_del",
  "comment.batch": "audit.act.comment_batch",
  "media.upload": "audit.act.media_upload",
  "media.delete": "audit.act.media_delete",
  "media.batch_delete": "audit.act.media_batch_del",
  "media.reconcile": "audit.act.media_reconcile",
  "media.orphans_clean": "audit.act.media_clean",
  "backup.export": "audit.act.backup_export",
  "backup.export_ticket": "audit.act.backup_ticket",
  "backup.restore": "audit.act.backup_restore",
  "backup.restore_legacy": "audit.act.backup_restore_leg",
  "backup.import_legacy": "audit.act.backup_import_leg",
  "backup.local_snapshot": "audit.act.backup_snap",
  "backup.local_restore": "audit.act.backup_snap_res",
  "backup.local_delete": "audit.act.backup_snap_del",
  "user.create": "audit.act.user_create",
  "user.update": "audit.act.user_update",
  "user.delete": "audit.act.user_delete",
  "plugin.install": "audit.act.plugin_install",
  "plugin.update": "audit.act.plugin_update",
  "plugin.enable": "audit.act.plugin_enable",
  "plugin.disable": "audit.act.plugin_disable",
  "plugin.reload": "audit.act.plugin_reload",
  "plugin.clear_kv": "audit.act.plugin_clear_kv",
  "plugin.config_patch": "audit.act.plugin_cfg",
  "plugin.uninstall": "audit.act.plugin_uninstall",
  "theme.activate": "audit.act.theme_activate",
  "theme.install": "audit.act.theme_install",
  "theme.export": "audit.act.theme_export",
  "theme.config": "audit.act.theme_config",
  "theme.uninstall": "audit.act.theme_uninstall",
  "locale.install": "audit.act.locale_install",
  "locale.install_zip": "audit.act.locale_zip",
  "locale.delete": "audit.act.locale_delete",
  "security.ban": "audit.act.sec_ban",
  "security.ip_ban_add": "audit.act.sec_ban",
  "security.unban": "audit.act.sec_unban",
  "security.ip_ban_remove": "audit.act.sec_unban",
  "security.ip_ban_batch_delete": "audit.act.sec_ban_batch",
  "security.comment_ip_ban": "audit.act.sec_cmt_ban",
  "security.geo_rule_add": "audit.act.sec_geo_add",
  "security.geo_rule_update": "audit.act.sec_geo_upd",
  "security.totp_enroll": "audit.act.sec_totp_on",
  "security.totp_disable": "audit.act.sec_totp_off",
  "security.passkey_register": "audit.act.sec_pk_add",
  "security.passkey_delete": "audit.act.sec_pk_del",
  "security.session_revoke": "audit.act.sec_sess_rev",
  "security.sessions_revoke_others": "audit.act.sec_sess_all",
  "security.api_token_create": "audit.act.sec_tok_add",
  "security.api_token_revoke": "audit.act.sec_tok_rev",
  "security.oidc_link": "audit.act.sec_oidc_link",
  "security.oidc_unlink": "audit.act.sec_oidc_unlink",
  "security.rotate_share_secret": "audit.act.sec_rot_share",
  "link.batch": "audit.act.link_batch",
  "link.check": "audit.act.link_check",
  "follow.import_opml": "audit.act.follow_opml",
  "follow.sync_single": "audit.act.follow_sync",
  "redirect.import": "audit.act.redir_import",
  "redirect.batch_delete": "audit.act.redir_batch_del",
  "webhook.replay": "audit.act.webhook_replay",
  "github.publish_showcase": "audit.act.gh_publish",
  "system.clear_runtime_logs": "audit.act.sys_clear_logs",
};

export function localizeAuditAction(act?: string): string {
  const raw = String(act || "").trim();
  if (!raw) return "";
  const key = AUDIT_ACTION_KEYS[raw.toLowerCase()];
  return key ? t(key) : raw;
}

