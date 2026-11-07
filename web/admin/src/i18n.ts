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
const bundledModules = import.meta.glob<Record<string, string>>("./locales/*.json", {
  eager: true,
  import: "default",
});

const bundledCatalogs: Record<string, Record<string, string>> = {};
const catalogs: Record<string, Record<string, string>> = {};
const initialLocales: LocaleOption[] = [];

for (const [filePath, table] of Object.entries(bundledModules)) {
  const fileBase = filePath.split("/").pop()?.replace(/\.json$/i, "") || "";
  const code = canonicalizeLocaleTag(table["meta.code"] || fileBase);
  if (!code) continue;
  bundledCatalogs[code] = { ...table };
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

export function formatDateTime(ms?: number): string {
  if (!ms || ms <= 0) return "—";
  const d = new Date(ms);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
