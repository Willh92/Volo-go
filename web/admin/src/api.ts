import { signal } from "@preact/signals";
import {
  ensureLocaleLoaded,
  LocaleOption,
  localeSig,
  normalizeLocale,
  registerExternalLocales,
} from "./i18n";

export interface BootConfig {
  mode: "console" | "setup";
  admin_path: string;
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
  if (!trimmed || !/^\/[A-Za-z0-9/_-]+$/.test(trimmed) || trimmed.startsWith("//")) {
    return "/console";
  }
  return trimmed;
}

export function readBootConfig(): BootConfig {
  const el = document.getElementById("volo-boot-config");
  if (el && el.textContent) {
    try {
      const parsed = JSON.parse(el.textContent) as BootConfig;
      return {
        ...parsed,
        admin_path: sanitizeAdminPath(parsed.admin_path),
      };
    } catch {
      // Fallback below
    }
  }
  return {
    mode: window.location.pathname.startsWith("/setup") ? "setup" : "console",
    admin_path: "/console",
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
  if (/(HTTP \d{3}|fail|error|invalid|denied|forbid|timeout|cannot|unable|失败|错误|无效|拒绝|异常|超时|必填|不能为空)/i.test(msg)) {
    return "err";
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
  scheduleToastDismiss(resolvedKind === "err" ? 4600 : 3400);
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
      showToast(err?.message || "Failed to save accent");
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
      const msg = (payload && typeof payload === "object" && payload.error) || `HTTP ${res.status}`;
      throw new ApiError(res.status, msg, payload);
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
        const msg = (payload && typeof payload === "object" && payload.error) || `HTTP ${xhr.status || 0}`;
        reject(new ApiError(xhr.status || 0, msg, payload));
      }
    };
    xhr.onerror = () => {
      busyCountSig.value = Math.max(0, busyCountSig.value - 1);
      reject(new ApiError(0, "Network error"));
    };
    xhr.onabort = () => {
      busyCountSig.value = Math.max(0, busyCountSig.value - 1);
      reject(new ApiError(0, "Upload aborted"));
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
