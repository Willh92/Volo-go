import { signal } from "@preact/signals";

export type LocaleCode = string;

export interface LocaleOption {
  code: string;
  name: string;
  dir: string;
}

// Auto-discover built-in per-language metadata in `./locales/*.json` at build time.
// Full built-in JSON catalogs are emitted as standalone `./i18n/{code}.json` assets (with the active
// boot locale injected synchronously via `<script id="volo-boot-locale">` in `index.html`).
import { builtinLocales, keys as bundledKeys } from "virtual:volo-locales";

const bundledCatalogs: Record<string, Record<string, string>> = {};
const catalogs: Record<string, Record<string, string>> = {};
const builtinLocaleCodes = new Set<string>();
const pendingBuiltinLoads = new Map<string, Promise<Record<string, string> | undefined>>();
const initialLocales: LocaleOption[] = [];

function unpackLocalePayload(raw: unknown): Record<string, string> | null {
  if (Array.isArray(raw)) {
    const table: Record<string, string> = {};
    const len = Math.min(raw.length, bundledKeys.length);
    for (let i = 0; i < len; i++) {
      const v = raw[i];
      if (typeof v === "string" && v) {
        table[bundledKeys[i]] = v;
      }
    }
    return table;
  }
  if (raw && typeof raw === "object") {
    return raw as Record<string, string>;
  }
  return null;
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

for (const meta of builtinLocales) {
  const code = canonicalizeLocaleTag(meta.code);
  if (!code) continue;
  builtinLocaleCodes.add(code);
  initialLocales.push({
    code,
    name: meta.name || code,
    dir: meta.dir || "ltr",
  });
}

let bootLocaleCode: LocaleCode = "zh-CN";
if (typeof document !== "undefined") {
  try {
    const el = document.getElementById("volo-boot-locale");
    if (el && el.textContent) {
      const table = unpackLocalePayload(JSON.parse(el.textContent));
      if (table) {
        const rawCode = el.getAttribute("data-locale") || table["meta.code"] || "zh-CN";
        const code = canonicalizeLocaleTag(rawCode) || "zh-CN";
        bundledCatalogs[code] = table;
        catalogs[code] = { ...table };
        bootLocaleCode = code;
      }
    }
  } catch {
    // Ignore malformed boot locale payload; async fallback loader handles recovery.
  }
}

export const availableLocalesSig = signal<LocaleOption[]>(sortLocaleOptions(initialLocales));
export const localeSig = signal<LocaleCode>(bootLocaleCode);
const catalogVersionSig = signal<number>(0);
const loadedRuntimeOverlays = new Set<string>();

async function ensureBuiltinCatalog(rawCode: string): Promise<Record<string, string> | undefined> {
  const code = canonicalizeLocaleTag(rawCode);
  if (!code || !builtinLocaleCodes.has(code)) {
    return undefined;
  }
  if (bundledCatalogs[code]) {
    return bundledCatalogs[code];
  }
  const existing = pendingBuiltinLoads.get(code);
  if (existing) {
    return existing;
  }
  const loadPromise = (async () => {
    try {
      const baseURI = typeof document !== "undefined" ? document.baseURI : window.location.href;
      const assetURL = new URL(`i18n/${encodeURIComponent(code)}.json`, baseURI).toString();
      const res = await fetch(assetURL, { credentials: "same-origin" });
      if (res.ok) {
        const table = unpackLocalePayload(await res.json());
        if (table) {
          bundledCatalogs[code] = table;
          catalogs[code] = { ...table, ...(catalogs[code] || {}) };
          catalogVersionSig.value++;
          return table;
        }
      }
    } catch {
      // Offline or synthetic environment without static i18n asset
    } finally {
      pendingBuiltinLoads.delete(code);
    }
    return bundledCatalogs[code];
  })();
  pendingBuiltinLoads.set(code, loadPromise);
  return loadPromise;
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

// ensureLocaleLoaded fetches lazy built-in catalogs (`./i18n/{code}.json`) and runtime JSON overrides/packs
// from `/api/v1/i18n/{code}` when needed (e.g. external `./locales/zh-TW.json` or `ja-JP.json`).
export async function ensureLocaleLoaded(rawCode: string, forceReload = false): Promise<LocaleCode> {
  const code = normalizeLocale(rawCode);
  localeSig.value = code;
  const isZh = code.toLowerCase().startsWith("zh");
  const fallbackBuiltinCode = isZh ? "zh-CN" : "en-US";
  if (builtinLocaleCodes.has(code)) {
    await ensureBuiltinCatalog(code);
  } else {
    await ensureBuiltinCatalog(fallbackBuiltinCode);
  }
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
      const isBundled = builtinLocaleCodes.has(code);
      const source = isBundled ? data.custom_messages : data.custom_messages || data.messages;
      if (source && typeof source === "object") {
        const base = bundledCatalogs[code] ? { ...bundledCatalogs[code] } : {};
        const refBuiltin =
          bundledCatalogs[fallbackBuiltinCode] || bundledCatalogs["en-US"] || bundledCatalogs["zh-CN"];
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
            !refBuiltin ||
            refBuiltin[k] !== undefined ||
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
    // Offline or non-overlay environment: built-in JSON catalog remains authoritative.
  }
  return code;
}

// exportFullTemplateJSON merges public/backend keys from `/api/v1/i18n/{baseCode}` with all Admin SPA keys
// into a single unified starter JSON file for creating new community language packs.
export async function exportFullTemplateJSON(baseCode: "zh-CN" | "en-US"): Promise<string> {
  await ensureBuiltinCatalog(baseCode);
  if (!bundledCatalogs[baseCode]) {
    await ensureBuiltinCatalog("en-US");
  }
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
  const adminDict = bundledCatalogs[baseCode] || bundledCatalogs["en-US"] || bundledCatalogs["zh-CN"] || {};
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

// exportLocalePackJSON fetches `/api/admin/locales/{code}/export` and merges any missing built-in Admin SPA keys
// so row-level exported language packs always include the full public + admin keyset (~1,900+ keys).
export async function exportLocalePackJSON(rawCode: string): Promise<string> {
  const code = canonicalizeLocaleTag(rawCode) || rawCode;
  await Promise.all([ensureBuiltinCatalog("zh-CN"), ensureBuiltinCatalog("en-US")]);
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
    // Fallback to loaded/built-in catalog below
  }
  const isZh = code.toLowerCase().startsWith("zh");
  const fallbackAdmin =
    bundledCatalogs[code] ||
    catalogs[code] ||
    (isZh ? bundledCatalogs["zh-CN"] : bundledCatalogs["en-US"]) ||
    bundledCatalogs["en-US"] ||
    bundledCatalogs["zh-CN"] ||
    {};
  const enAdmin = bundledCatalogs["en-US"] || bundledCatalogs["zh-CN"] || {};
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

export function tOptFor(
  locale: string,
  keyOrIdx: string | number,
  vars?: Record<string, string | number>
): string | undefined {
  void catalogVersionSig.value;
  const key = typeof keyOrIdx === "number" ? bundledKeys[keyOrIdx] : keyOrIdx;
  if (!key) return undefined;
  const isZh = locale.toLowerCase().startsWith("zh");
  let tpl =
    catalogs[locale]?.[key] ??
    (isZh
      ? catalogs["zh-CN"]?.[key] ?? catalogs["en-US"]?.[key]
      : catalogs["en-US"]?.[key] ?? catalogs["zh-CN"]?.[key]);
  if (tpl === undefined) return undefined;
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      tpl = tpl.replaceAll(`{${k}}`, String(v));
    }
  }
  return tpl;
}

export function tOpt(keyOrIdx: string | number, vars?: Record<string, string | number>): string | undefined {
  return tOptFor(localeSig.value, keyOrIdx, vars);
}

export function tFor(locale: string, keyOrIdx: string | number, vars?: Record<string, string | number>): string {
  const found = tOptFor(locale, keyOrIdx, vars);
  if (found !== undefined) return found;
  return typeof keyOrIdx === "number" ? (bundledKeys[keyOrIdx] ?? String(keyOrIdx)) : keyOrIdx;
}

export function t(key: string | number, vars?: Record<string, string | number>): string {
  return tFor(localeSig.value, key, vars);
}

export function formatDateTime(val?: number | string): string {
  const ms = typeof val === "string" ? Date.parse(val) : val;
  if (!ms || Number.isNaN(ms) || ms <= 0) return "—";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function localizeBackupTable(name?: string): string {
  const raw = String(name || "").trim();
  if (!raw) return "";
  const low = raw.toLowerCase();
  return tOpt(`bak.tbl.${low === "article_revisions" ? "revisions" : low}`) ?? raw;
}

const AUDIT_RES_ALIAS: Record<string, string> = {
  media_asset: "media",
  user_session: "session",
  user_credential: "credential",
  nav_item: "nav",
};

export function localizeAuditResource(res?: string): string {
  const raw = String(res || "").trim();
  if (!raw) return "";
  const low = raw.toLowerCase();
  return tOpt(`audit.res.${AUDIT_RES_ALIAS[low] || low}`) ?? raw;
}

const AUDIT_ACTION_KEYS: Record<string, string | number> = {
  "auth.password_change": "audit.act.auth_pwd_change",
  "reader.register_oidc": "audit.act.reader_reg_oidc",
  "article.batch_import": "audit.act.article_import",
  "tag.batch_delete": "audit.act.tag_batch_del",
  "comment.moderate": "audit.act.comment_mod",
  "comment.delete": "audit.act.comment_del",
  "media.batch_delete": "audit.act.media_batch_del",
  "media.orphans_clean": "audit.act.media_clean",
  "backup.export_ticket": "audit.act.backup_ticket",
  "backup.restore_legacy": "audit.act.backup_restore_leg",
  "backup.import_legacy": "audit.act.backup_import_leg",
  "backup.local_snapshot": "audit.act.backup_snap",
  "backup.local_restore": "audit.act.backup_snap_res",
  "backup.local_delete": "audit.act.backup_snap_del",
  "plugin.config_patch": "audit.act.plugin_cfg",
  "plugin.priority": "audit.act.plugin_prio",
  "plugin.cron_trigger": "audit.act.plugin_cron",
  "locale.install_zip": "audit.act.locale_zip",
  "security.ban": "audit.act.sec_ban",
  "security.waf_auto_ban": "audit.act.sec_auto_ban",
  "security.waf_404_probe": "audit.act.sec_404_probe",
  "security.ip_ban_add": "audit.act.sec_ban",
  "security.unban": "audit.act.sec_unban",
  "security.ip_ban_remove": "audit.act.sec_unban",
  "security.ip_ban_batch_delete": "audit.act.sec_ban_batch",
  "security.comment_ip_ban": "audit.act.sec_cmt_ban",
  "security.geo_rule_add": "audit.act.sec_geo_add",
  "security.geo_rule_update": "audit.act.sec_geo_upd",
  "security.geoip_reload": "audit.act.sec_geo_reload",
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
  "follow.import_opml": "audit.act.follow_opml",
  "follow.sync_single": "audit.act.follow_sync",
  "redirect.import": "audit.act.redir_import",
  "redirect.batch_delete": "audit.act.redir_batch_del",
  "github.publish_showcase": "audit.act.gh_publish",
  "system.clear_runtime_logs": "audit.act.sys_clear_logs",
};

const WAF_REASON_KEYS: Record<string, string | number> = {
  scanner_trap: "sys.waf_reason_scanner_trap",
  wp_probe: "sys.waf_reason_scanner_trap",
  xmlrpc_probe: "sys.waf_reason_scanner_trap",
  admin_probe: "sys.waf_reason_scanner_trap",
  setup_probe: "sys.waf_reason_scanner_trap",
  honeypot_trap: "sys.waf_reason_scanner_trap",
  sensitive_probe: "sys.waf_reason_sensitive_probe",
  env_probe: "sys.waf_reason_sensitive_probe",
  git_probe: "sys.waf_reason_sensitive_probe",
  bot_probe: "sys.waf_reason_malicious_ua",
  malicious_ua: "sys.waf_reason_malicious_ua",
  ua_block: "sys.waf_reason_malicious_ua",
  bad_bot: "sys.waf_reason_malicious_ua",
  manual_ban: "sys.waf_reason_manual_ban",
  manual: "sys.waf_reason_manual_ban",
  admin_ban: "sys.waf_reason_manual_ban",
  admin_block: "sys.waf_reason_manual_ban",
  "manual administrator block": "sys.waf_reason_manual_ban",
  not_found_scan: "sys.waf_reason_not_found_scan",
  not_found_flood: "sys.waf_reason_not_found_scan",
  path_scan: "sys.waf_reason_not_found_scan",
  dir_scan: "sys.waf_reason_not_found_scan",
  "404_flood": "sys.waf_reason_not_found_scan",
  path_traversal: "sys.waf_reason_path_traversal",
  directory_traversal: "sys.waf_reason_path_traversal",
  lfi_probe: "sys.waf_reason_path_traversal",
  rfi_probe: "sys.waf_reason_exploit_probe",
  exploit_probe: "sys.waf_reason_exploit_probe",
  payload_attack: "sys.waf_reason_exploit_probe",
  sql_injection: "sys.waf_reason_exploit_probe",
  sqli: "sys.waf_reason_exploit_probe",
  xss_probe: "sys.waf_reason_exploit_probe",
  xss: "sys.waf_reason_exploit_probe",
  rate_abuse: "sys.waf_reason_rate_abuse",
  rate_limit: "sys.waf_reason_rate_abuse",
  burst_limit: "sys.waf_reason_rate_abuse",
  cc_attack: "sys.waf_reason_rate_abuse",
  flood: "sys.waf_reason_rate_abuse",
  login_fail: "sys.waf_reason_login_fail",
  auth_fail: "sys.waf_reason_login_fail",
  brute_force: "sys.waf_reason_login_fail",
  login_bruteforce: "sys.waf_reason_login_fail",
  comment_spam: "sys.waf_reason_comment_spam",
  comment_ip_ban: "sys.waf_reason_comment_spam",
  honeypot: "sys.waf_reason_comment_spam",
  geo_block: "sys.waf_reason_geo_block",
  geo_deny: "sys.waf_reason_geo_block",
};

export function localizeAuditAction(act?: string): string {
  const raw = String(act || "").trim();
  if (!raw) return "";
  const low = raw.toLowerCase();
  const key = AUDIT_ACTION_KEYS[low];
  if (key !== undefined) return t(key);
  return tOpt(`audit.act.${low.replace(/\./g, "_")}`) ?? raw;
}

export function localizeWafReason(code?: string): string {
  const orig = String(code || "").trim();
  if (!orig) return "";
  const autoMatch = orig.match(/^automated defense triggered\s*\(([^)]+)\)\s*$/i);
  const unwrapped = autoMatch ? autoMatch[1] : orig;
  const cleaned = unwrapped.replace(/^auto[-_ ]?ban\s*:\s*/i, "").trim().toLowerCase();
  const key = WAF_REASON_KEYS[cleaned];
  if (key !== undefined) return t(key);
  return tOpt(`sys.waf_reason_${cleaned}`) ?? orig;
}
