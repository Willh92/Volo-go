import { signal } from "@preact/signals";
import {
  ensureLocaleLoaded,
  LocaleOption,
  localeSig,
  normalizeLocale,
  registerExternalLocales,
  t,
} from "./i18n";

export interface BootConfig {
  mode: "console" | "setup";
  admin_path: string;
  base_admin_path?: string;
  public_url: string;
  default_locale: string;
  accent: string;
  default_theme: string;
  site_title: string;
  locales?: LocaleOption[];
}

export interface StaffUser {
  id: string;
  username: string;
  email: string;
  display_name: string;
  avatar_url: string;
  bio: string;
  website_url: string;
  role: "admin" | "editor" | "reader";
  preferred_locale: string;
}

export function sanitizeAdminPath(raw?: string): string {
  const trimmed = String(raw || "").trim();
  if (!trimmed) return "/console";
  const withSlash = trimmed.startsWith("/") ? trimmed : `/${trimmed}`;
  const clean = withSlash.length > 1 ? withSlash.replace(/\/+$/, "") : withSlash;
  if (clean === "/" || !/^\/[A-Za-z0-9/_-]+$/.test(clean) || clean.includes("//") || clean.includes("..")) {
    return "/console";
  }
  return clean;
}

export function readBootConfig(): BootConfig {
  const el = document.getElementById("volo-boot-config");
  if (el && el.textContent) {
    try {
      const parsed = JSON.parse(el.textContent) as BootConfig;
      const adminPath = sanitizeAdminPath(parsed.admin_path);
      const baseAdminPath = parsed.base_admin_path ? sanitizeAdminPath(parsed.base_admin_path) : adminPath;
      return {
        ...parsed,
        admin_path: adminPath,
        base_admin_path: baseAdminPath,
      };
    } catch {
      // Fallback below
    }
  }
  return {
    mode: window.location.pathname.startsWith("/setup") ? "setup" : "console",
    admin_path: "/console",
    base_admin_path: "/console",
    public_url: window.location.origin,
    default_locale: "zh-CN",
    accent: "forest",
    default_theme: "auto",
    site_title: "Volo",
  };
}

export const bootConfig = readBootConfig();
if (bootConfig.locales) {
  registerExternalLocales(bootConfig.locales);
}

export type ToastKind = "ok" | "err" | "warn";
export interface ToastItem {
  id: number;
  text: string;
  kind: ToastKind;
  closing?: boolean;
}

export interface ConfirmOptions {
  title?: string;
  message: string;
  confirmText?: string;
  danger?: boolean;
  items?: string[];
}

export const csrfTokenSig = signal<string>("");
export const currentUserSig = signal<StaffUser | null>(null);
export const toastSig = signal<ToastItem | null>(null);
export const confirmSig = signal<(ConfirmOptions & { resolve: (ok: boolean) => void }) | null>(null);
export const dirtyEditorSig = signal<boolean>(false);
export const busyCountSig = signal<number>(0);
function readInitialThemeMode(): string {
  try {
    const shared = localStorage.getItem("volo_theme_mode");
    if (shared) return shared;
    const legacy = localStorage.getItem("volo_console_theme");
    if (legacy) {
      localStorage.setItem("volo_theme_mode", legacy);
      localStorage.removeItem("volo_console_theme");
      return legacy;
    }
  } catch {
    // Ignore storage errors
  }
  return bootConfig.default_theme || "auto";
}

export function resolveEffectiveTheme(mode: string): "light" | "dark" {
  const clean = mode || "auto";
  if (clean === "dark") return "dark";
  if (clean === "light") return "light";
  if (clean === "system") {
    return typeof window !== "undefined" && window.matchMedia?.("(prefers-color-scheme: dark)")?.matches
      ? "dark"
      : "light";
  }
  const h = new Date().getHours();
  return h < 7 || h >= 19 ? "dark" : "light";
}

export const themeModeSig = signal<string>(readInitialThemeMode());
export const effectiveDarkSig = signal<boolean>(resolveEffectiveTheme(themeModeSig.value) === "dark");
export const accentSig = signal<string>(bootConfig.accent || "forest");
export const storageDriverSig = signal<string>("local");

export function openConfirm(opts: ConfirmOptions | string): Promise<boolean> {
  const norm: ConfirmOptions = typeof opts === "string" ? { message: opts, danger: true } : opts;
  return new Promise<boolean>((resolve) => {
    if (confirmSig.value) {
      confirmSig.value.resolve(false);
    }
    confirmSig.value = { danger: true, ...norm, resolve };
  });
}

export function resolveConfirm(ok: boolean) {
  const cur = confirmSig.value;
  confirmSig.value = null;
  if (cur) cur.resolve(ok);
}

export function resetConsoleScroll() {
  if (typeof window !== "undefined") {
    window.scrollTo(0, 0);
  }
  if (typeof document !== "undefined") {
    document.documentElement.scrollTop = 0;
    document.body.scrollTop = 0;
    const scrollers = document.querySelectorAll<HTMLElement>(".volo-main, .volo-content");
    scrollers.forEach((el) => {
      el.scrollTop = 0;
    });
  }
}

let toastTimer: number | undefined;
let toastCloseTimer: number | undefined;
let toastSeq = 0;

function inferToastKind(msg: string): ToastKind {
  if (
    /(HTTP \d{3}|fail|error|invalid|denied|forbid|unauthorized|too many|rate limit|too large|unsupported|mismatch|expired|aborted|timeout|cannot|unable|失败|错误|无效|拒绝|无法|未能|不可用|受限|异常|超时|必填|不能为空|未登录|未授权|权限不足|频繁|过大|不支持|不匹配|过期|中断|失敗|錯誤|無效|拒絕|無法|異常|逾時|過大|頻繁|不支援|未授權|權限不足)/i.test(
      msg
    )
  ) {
    return "err";
  }
  if (/(not detected|no .* device|未检测到|未偵測到)/i.test(msg)) {
    return "warn";
  }
  return "ok";
}

function scheduleToastDismiss(delayMs: number) {
  if (toastTimer) window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => {
    dismissToast();
  }, delayMs);
}

export function showToast(msg: string, kind?: ToastKind) {
  if (!msg) return;
  if (toastTimer) window.clearTimeout(toastTimer);
  if (toastCloseTimer) window.clearTimeout(toastCloseTimer);
  const resolvedKind = kind || inferToastKind(msg);
  toastSig.value = { id: ++toastSeq, text: msg, kind: resolvedKind, closing: false };
  scheduleToastDismiss(resolvedKind === "err" ? 4600 : resolvedKind === "warn" ? 4200 : 3400);
}

export function pauseToast() {
  if (toastTimer) {
    window.clearTimeout(toastTimer);
    toastTimer = undefined;
  }
}

export function resumeToast() {
  if (!toastSig.value || toastSig.value.closing) return;
  scheduleToastDismiss(1800);
}

export function dismissToast() {
  if (toastTimer) {
    window.clearTimeout(toastTimer);
    toastTimer = undefined;
  }
  const cur = toastSig.value;
  if (!cur || cur.closing) return;
  toastSig.value = { ...cur, closing: true };
  if (toastCloseTimer) window.clearTimeout(toastCloseTimer);
  toastCloseTimer = window.setTimeout(() => {
    toastSig.value = null;
  }, 160);
}

export function applyThemeAttributes(mode: string, accent: string, persistMode = false) {
  const cleanMode = mode || "auto";
  const cleanAccent = accent || "forest";
  const effective = resolveEffectiveTheme(cleanMode);
  themeModeSig.value = cleanMode;
  accentSig.value = cleanAccent;
  effectiveDarkSig.value = effective === "dark";
  try {
    if (persistMode) {
      localStorage.setItem("volo_theme_mode", cleanMode);
    }
    localStorage.removeItem("volo_console_theme");
    localStorage.removeItem("volo_console_accent");
  } catch {
    // Ignore storage errors
  }
  if (typeof document !== "undefined") {
    document.documentElement.setAttribute("data-mode", cleanMode);
    document.documentElement.setAttribute("data-accent", cleanAccent);
    document.documentElement.setAttribute("data-theme", effective);
  }
}

export const applyConsoleAppearance = applyThemeAttributes;

export function toggleEffectiveTheme(): "light" | "dark" {
  const next = resolveEffectiveTheme(themeModeSig.value) === "dark" ? "light" : "dark";
  applyThemeAttributes(next, accentSig.value, true);
  return next;
}

if (typeof window !== "undefined") {
  window.addEventListener("storage", (ev) => {
    if (!ev.key || ev.key === "volo_theme_mode") {
      let nextMode = bootConfig.default_theme || "auto";
      try {
        nextMode = localStorage.getItem("volo_theme_mode") || nextMode;
      } catch {
        // Ignore
      }
      applyThemeAttributes(nextMode, accentSig.value, false);
    }
  });
  const mql = window.matchMedia?.("(prefers-color-scheme: dark)");
  const onSchemeChange = () => {
    if (themeModeSig.value === "system") {
      applyThemeAttributes("system", accentSig.value, false);
    }
  };
  if (mql) {
    if (typeof mql.addEventListener === "function") {
      mql.addEventListener("change", onSchemeChange);
    } else if (typeof (mql as any).addListener === "function") {
      (mql as any).addListener(onSchemeChange);
    }
  }
  window.setInterval(() => {
    if (themeModeSig.value === "auto") {
      const next = resolveEffectiveTheme("auto");
      if ((next === "dark") !== effectiveDarkSig.value) {
        applyThemeAttributes("auto", accentSig.value, false);
      }
    }
  }, 60_000);
}

const EXACT_ADMIN_ERROR_KEYS: Record<string, string> = {
  "invalid json": "api.err_invalid_json",
  "invalid json body": "api.err_invalid_json",
  "malformed json": "api.err_invalid_json",
  "unauthorized": "api.err_unauthorized",
  "authentication required": "api.err_unauthorized",
  "invalid or expired api token": "api.err_unauthorized",
  "session expired": "api.err_unauthorized",
  "forbidden": "api.err_forbidden",
  "admin role required": "api.err_forbidden",
  "insufficient permissions": "api.err_forbidden",
  "this endpoint requires an interactive admin session": "api.err_forbidden",
  "not found": "api.err_not_found",
  "method not allowed": "api.err_method_not_allowed",
  "csrf token mismatch": "api.err_csrf_mismatch",
  "invalid csrf token": "api.err_csrf_mismatch",
  "missing csrf token": "api.err_csrf_mismatch",
  "invalid or missing x-csrf-token": "api.err_csrf_mismatch",
  "cross-origin request blocked": "api.err_csrf_mismatch",
  "rate limit exceeded": "api.err_rate_limit",
  "rate limited": "api.err_rate_limit",
  "too many requests": "api.err_rate_limit",
  "invalid credentials": "api.err_invalid_credentials",
  "invalid username or password": "api.err_invalid_credentials",
  "invalid username/email or password": "api.err_invalid_credentials",
  "current password is incorrect": "api.err_invalid_current_pwd",
  "invalid current password": "api.err_invalid_current_pwd",
  "current password required": "api.err_current_pwd_required",
  "totp code required": "api.err_totp_required",
  "2fa code required": "api.err_totp_required",
  "invalid totp code": "api.err_invalid_totp",
  "invalid 2fa code": "api.err_invalid_totp",
  "invalid or expired totp verification code": "api.err_invalid_totp",
  "totp code already used in current time window": "api.err_totp_replay",
  "invalid or expired challenge token": "api.err_challenge_expired",
  "two-factor login challenge expired or invalid": "api.err_challenge_expired",
  "challenge expired": "api.err_challenge_expired",
  "passkey ceremony verification failed": "api.err_passkey_failed",
  "passkey signature counter regression (possible cloned authenticator)": "api.err_passkey_failed",
  "invalid volo_public_url for webauthn rp id": "api.err_passkey_failed",
  "password must be 6-128 characters": "api.err_weak_password",
  "password must not be empty": "api.err_weak_password",
  "invalid email address": "api.err_invalid_email",
  "username must be 2-48 characters": "api.err_username_len",
  "at least one api token scope is required": "api.err_pat_scope_required",
  "invalid dynamic_login_path": "api.err_invalid_dynamic_login_path",
  "re-authentication required": "api.err_reauth_required",
  "step-up authentication required": "api.err_reauth_required",
  "account disabled": "api.err_account_disabled",
  "user is disabled": "api.err_account_disabled",
  "file too large": "api.err_file_too_large",
  "request body too large": "api.err_file_too_large",
  "http: request body too large": "api.err_file_too_large",
  "unsupported media type": "api.err_unsupported_media",
  "unsupported file type": "api.err_unsupported_media",
  "missing file": "api.err_missing_file",
  "no file uploaded": "api.err_missing_file",
  "slug already exists": "api.err_duplicate_slug",
  "duplicate slug": "api.err_duplicate_slug",
  "title is required": "api.err_title_required",
  "title required": "api.err_title_required",
  "internal server error": "api.err_internal",
  "network error": "api.err_network",
  "upload aborted": "api.err_upload_aborted",
};

export function localizeAdminError(rawMsg: string): string {
  const trimmed = String(rawMsg || "").trim();
  if (!trimmed) return t("api.err_internal");

  const norm = trimmed.replace(/[.!]+$/, "").trim().toLowerCase();
  const exactKey = EXACT_ADMIN_ERROR_KEYS[norm];
  if (exactKey) {
    const val = t(exactKey);
    if (val !== exactKey) return val;
  }

  const authMatch = /^auth:\s*(.+)$/i.exec(trimmed);
  if (authMatch && authMatch[1]) {
    const inner = localizeAdminError(authMatch[1]);
    if (inner !== authMatch[1].trim()) return inner;
  }

  const httpMatch = /^http\s+(\d{3})$/i.exec(trimmed);
  if (httpMatch) {
    const code = httpMatch[1];
    if (code === "401") return t("api.err_unauthorized");
    if (code === "403") return t("api.err_forbidden");
    if (code === "404") return t("api.err_not_found");
    if (code === "413") return t("api.err_file_too_large");
    if (code === "415") return t("api.err_unsupported_media");
    if (code === "429") return t("api.err_rate_limit");
    return t("api.err_http_status", { status: code });
  }

  const prefixRules: Array<[RegExp, string]> = [
    [/^settings:\s*(.+)$/i, "api.err_settings_prefix"],
    [/^validation(?:\s+failed)?:\s*(.+)$/i, "api.err_validation_prefix"],
    [/^database:\s*(.+)$/i, "api.err_db_prefix"],
    [/^backup:\s*(.+)$/i, "api.err_backup_prefix"],
    [/^plugin:\s*(.+)$/i, "api.err_plugin_prefix"],
    [/^theme:\s*(.+)$/i, "api.err_theme_prefix"],
  ];
  for (const [re, key] of prefixRules) {
    const m = re.exec(trimmed);
    if (m && m[1]) {
      const inner = localizeAdminError(m[1]);
      const val = t(key, { detail: inner });
      if (val !== key) return val;
    }
  }

  return trimmed;
}

export async function saveGlobalAccent(nextAccent: string): Promise<void> {
  const cleanAccent = nextAccent || "forest";
  applyThemeAttributes(themeModeSig.value, cleanAccent, false);
  if (currentUserSig.value?.role === "admin") {
    try {
      await apiFetch("/api/admin/settings/appearance", {
        method: "PATCH",
        body: JSON.stringify({ accent: cleanAccent }),
      });
    } catch (err: any) {
      showToast(err?.message || t("api.err_internal"), "err");
    }
  }
}

export class ApiError extends Error {
  status: number;
  data: any;
  constructor(status: number, message: string, data?: any) {
    super(message);
    this.status = status;
    this.data = data;
  }
}

export async function apiFetch<T = any>(
  url: string,
  opts: RequestInit = {}
): Promise<T> {
  const method = (opts.method || "GET").toUpperCase();
  const headers = new Headers(opts.headers || {});
  if (!headers.has("Accept-Language") && localeSig.value) {
    headers.set("Accept-Language", localeSig.value);
  }
  if (!(opts.body instanceof FormData) && !headers.has("Content-Type") && method !== "GET" && method !== "HEAD") {
    headers.set("Content-Type", "application/json");
  }
  if (csrfTokenSig.value && method !== "GET" && method !== "HEAD") {
    headers.set("X-CSRF-Token", csrfTokenSig.value);
  }
  const trackBusy = !url.endsWith("/articles/preview");
  if (trackBusy) busyCountSig.value = busyCountSig.value + 1;
  try {
    const res = await fetch(url, {
      ...opts,
      method,
      headers,
      credentials: "same-origin",
    });
    const ct = res.headers.get("Content-Type") || "";
    let payload: any = null;
    if (ct.includes("application/json")) {
      payload = await res.json().catch(() => null);
    } else {
      payload = await res.text().catch(() => "");
    }
    if (!res.ok) {
      const rawMsg = (payload && typeof payload === "object" && payload.error) || `HTTP ${res.status}`;
      throw new ApiError(res.status, localizeAdminError(String(rawMsg)), payload);
    }
    return payload as T;
  } finally {
    if (trackBusy) busyCountSig.value = Math.max(0, busyCountSig.value - 1);
  }
}

export async function refreshMe(): Promise<StaffUser | null> {
  try {
    const data = await apiFetch<{
      user: StaffUser;
      csrf_token: string;
      storage_driver?: string;
      appearance?: { accent?: string; default_mode?: string };
    }>("/api/admin/me");
    currentUserSig.value = data.user;
    csrfTokenSig.value = data.csrf_token || "";
    if (data.storage_driver) {
      storageDriverSig.value = data.storage_driver;
    }
    if (data.user?.preferred_locale) {
      const nextLoc = normalizeLocale(data.user.preferred_locale);
      localeSig.value = nextLoc;
      void ensureLocaleLoaded(nextLoc);
    }
    if (data.appearance) {
      let hasExplicitLocal = false;
      try {
        hasExplicitLocal = Boolean(localStorage.getItem("volo_theme_mode"));
      } catch {
        // Ignore
      }
      const nextMode = !hasExplicitLocal && data.appearance.default_mode
        ? data.appearance.default_mode
        : themeModeSig.value;
      const nextAccent = data.appearance.accent || accentSig.value;
      applyThemeAttributes(nextMode, nextAccent, false);
    }
    return data.user;
  } catch {
    currentUserSig.value = null;
    return null;
  }
}

// WebAuthn Base64URL helpers
export function b64urlToBytes(b64url: string): ArrayBuffer {
  const pad = "=".repeat((4 - (b64url.length % 4)) % 4);
  const b64 = (b64url + pad).replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out.buffer as ArrayBuffer;
}

export function bytesToB64url(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

// IndexedDB Crash-Safe Draft Vault (`volo_drafts_v1`)
const DRAFT_DB = "volo_drafts_v1";
const DRAFT_STORE = "drafts";

function openDraftDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DRAFT_DB, 1);
    req.onupgradeneeded = () => {
      if (!req.result.objectStoreNames.contains(DRAFT_STORE)) {
        req.result.createObjectStore(DRAFT_STORE);
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

export function uploadWithProgress<T = any>(
  url: string,
  fd: FormData,
  onProgress?: (pct: number) => void,
  method = "POST"
): Promise<T> {
  busyCountSig.value = busyCountSig.value + 1;
  if (onProgress) onProgress(0);
  return new Promise<T>((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open(method.toUpperCase(), url, true);
    xhr.withCredentials = true;
    if (localeSig.value) {
      xhr.setRequestHeader("Accept-Language", localeSig.value);
    }
    if (csrfTokenSig.value) {
      xhr.setRequestHeader("X-CSRF-Token", csrfTokenSig.value);
    }
    if (xhr.upload && onProgress) {
      xhr.upload.onprogress = (ev) => {
        if (ev.lengthComputable && ev.total > 0) {
          onProgress(Math.min(100, Math.round((ev.loaded / ev.total) * 100)));
        }
      };
    }
    xhr.onload = () => {
      busyCountSig.value = Math.max(0, busyCountSig.value - 1);
      const ct = xhr.getResponseHeader("Content-Type") || "";
      let payload: any = null;
      if (ct.includes("application/json")) {
        try {
          payload = JSON.parse(xhr.responseText || "null");
        } catch {
          payload = null;
        }
      } else {
        payload = xhr.responseText || "";
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        if (onProgress) onProgress(100);
        resolve(payload as T);
      } else {
        const rawMsg = (payload && typeof payload === "object" && payload.error) || `HTTP ${xhr.status || 0}`;
        reject(new ApiError(xhr.status || 0, localizeAdminError(String(rawMsg)), payload));
      }
    };
    xhr.onerror = () => {
      busyCountSig.value = Math.max(0, busyCountSig.value - 1);
      reject(new ApiError(0, localizeAdminError("network error")));
    };
    xhr.onabort = () => {
      busyCountSig.value = Math.max(0, busyCountSig.value - 1);
      reject(new ApiError(0, localizeAdminError("upload aborted")));
    };
    xhr.send(fd);
  });
}

export interface LocalDraft {
  title: string;
  slug: string;
  summary_md: string;
  content_md: string;
  saved_at: number;
  category_id?: string;
  tags_text?: string;
  tag_ids?: string[];
  series_id?: string;
  cover_url?: string;
  visibility?: string;
  status?: string;
  template?: string;
  is_published?: boolean;
  show_toc?: boolean;
  allow_comment?: boolean;
  lang?: string;
  translation_group?: string;
}

export async function saveLocalDraft(key: string, draft: LocalDraft): Promise<void> {
  try {
    const db = await openDraftDB();
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(DRAFT_STORE, "readwrite");
      tx.objectStore(DRAFT_STORE).put(draft, key);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error);
    });
  } catch {
    // Ignore private-browsing IndexedDB quota restrictions
  }
}

export async function loadLocalDraft(key: string): Promise<LocalDraft | null> {
  try {
    const db = await openDraftDB();
    return await new Promise<LocalDraft | null>((resolve) => {
      const tx = db.transaction(DRAFT_STORE, "readonly");
      const req = tx.objectStore(DRAFT_STORE).get(key);
      req.onsuccess = () => resolve((req.result as LocalDraft) || null);
      req.onerror = () => resolve(null);
    });
  } catch {
    return null;
  }
}

export async function clearLocalDraft(key: string): Promise<void> {
  try {
    const db = await openDraftDB();
    const tx = db.transaction(DRAFT_STORE, "readwrite");
    tx.objectStore(DRAFT_STORE).delete(key);
  } catch {
    // No-op
  }
}
