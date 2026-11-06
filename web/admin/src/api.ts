import { signal } from "@preact/signals";
import { localeSig, normalizeLocale } from "./i18n";

export interface BootConfig {
  mode: "console" | "setup";
  admin_path: string;
  public_url: string;
  default_locale: string;
  accent: string;
  default_theme: string;
  site_title: string;
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

export function readBootConfig(): BootConfig {
  const el = document.getElementById("volo-boot-config");
  if (el && el.textContent) {
    try {
      return JSON.parse(el.textContent) as BootConfig;
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
    site_title: "Volo Blog",
  };
}

export const bootConfig = readBootConfig();
export const csrfTokenSig = signal<string>("");
export const currentUserSig = signal<StaffUser | null>(null);
export const toastSig = signal<string>("");
export const themeModeSig = signal<string>(localStorage.getItem("volo_console_theme") || bootConfig.default_theme || "auto");
export const accentSig = signal<string>(bootConfig.accent || "forest");

let toastTimer: number | undefined;
export function showToast(msg: string) {
  toastSig.value = msg;
  if (toastTimer) window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => {
    toastSig.value = "";
  }, 3400);
}

export function applyThemeAttributes(mode: string, accent: string) {
  themeModeSig.value = mode;
  accentSig.value = accent || "forest";
  localStorage.setItem("volo_console_theme", mode);
  document.documentElement.setAttribute("data-accent", accentSig.value);

  let dark = false;
  if (mode === "dark") {
    dark = true;
  } else if (mode === "system") {
    dark = window.matchMedia?.("(prefers-color-scheme: dark)")?.matches ?? false;
  } else if (mode === "auto") {
    const h = new Date().getHours();
    dark = h < 6 || h >= 19 || (window.matchMedia?.("(prefers-color-scheme: dark)")?.matches ?? false);
  }
  document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
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
}

export async function refreshMe(): Promise<StaffUser | null> {
  try {
    const data = await apiFetch<{
      user: StaffUser;
      csrf_token: string;
      appearance?: { accent?: string; default_mode?: string };
    }>("/api/admin/me");
    currentUserSig.value = data.user;
    csrfTokenSig.value = data.csrf_token || "";
    if (data.user?.preferred_locale) {
      localeSig.value = normalizeLocale(data.user.preferred_locale);
    }
    if (data.appearance?.accent) {
      applyThemeAttributes(themeModeSig.value, data.appearance.accent);
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

export interface LocalDraft {
  title: string;
  slug: string;
  summary_md: string;
  content_md: string;
  saved_at: number;
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
