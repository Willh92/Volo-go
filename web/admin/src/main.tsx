import { render } from "preact";
import type { CSSProperties } from "preact";
import { createPortal } from "preact/compat";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import "./styles/console.css";
import {
  accentSig,
  apiFetch,
  applyThemeAttributes,
  bootConfig,
  busyCountSig,
  csrfTokenSig,
  currentUserSig,
  dirtyEditorSig,
  dismissToast,
  effectiveDarkSig,
  openConfirm,
  pauseToast,
  refreshMe,
  resetConsoleScroll,
  resumeToast,
  saveGlobalAccent,
  showToast,
  themeModeSig,
  toastSig,
  toggleEffectiveTheme,
} from "./api";
import {
  availableLocalesSig,
  ensureLocaleLoaded,
  getLocaleDir,
  localeSig,
  normalizeLocale,
  t,
} from "./i18n";
import { BrandLogo } from "./components/BrandLogo";
import { AnimatedModal, GlobalConfirmModal, LocalePopover, computePortalMenuStyle, useDebouncedValue } from "./components/UIControls";
import { SetupWizard } from "./views/SetupWizard";
import { LoginView, formatOidcError } from "./views/LoginView";
import { OverviewSection, OverviewWorkspace } from "./views/OverviewWorkspace";
import { ContentSubTab, ContentWorkspace } from "./views/ContentWorkspace";
import { DistTab, DistributeWorkspace } from "./views/DistributeWorkspace";
import { AppearanceWorkspace } from "./views/AppearanceWorkspace";
import { SysTab, SystemWorkspace } from "./views/SystemWorkspace";
import { AboutHelpTab, AboutHelpWorkspace } from "./views/AboutHelpWorkspace";

type NavSection =
  | OverviewSection
  | ContentSubTab
  | DistTab
  | "themes"
  | "locales"
  | "plugins"
  | SysTab
  | AboutHelpTab;

const ACCENT_OPTIONS: Array<{ id: string; swatch: string }> = [
  { id: "forest", swatch: "#246b51" },
  { id: "ocean", swatch: "#2d6583" },
  { id: "teal", swatch: "#286c70" },
  { id: "amber", swatch: "#855c27" },
  { id: "plum", swatch: "#70517d" },
  { id: "rose", swatch: "#87525f" },
];

const MODE_OPTIONS = ["auto", "system", "light", "dark"] as const;

const SECTION_LABEL_KEYS: Record<NavSection, string> = {
  dashboard: "nav.home",
  editor: "nav.post_new",
  articles: "nav.post_list",
  drafts: "nav.post_drafts",
  comments: "nav.comments",
  categories: "nav.categories",
  tags: "nav.tags",
  series: "nav.series",
  books: "nav.books",
  pages: "nav.pages",
  nav_redirects: "nav.nav_redirects",
  media: "nav.media",
  feed_seo: "nav.feed_seo",
  github: "nav.github",
  links: "nav.links",
  follows: "nav.follows",
  webhooks: "nav.webhooks",
  themes: "nav.themes",
  locales: "nav.locales",
  plugins: "nav.plugins",
  settings_site: "nav.settings_site",
  settings_io: "nav.settings_io",
  users: "nav.users",
  backup: "nav.backup",
  account: "nav.account",
  waf: "nav.waf",
  logs: "nav.logs",
  audit: "nav.audit",
  about: "nav.about",
  help: "nav.help",
};

interface NavEntry {
  kind: "item" | "group";
  id: string;
  labelKey: string;
  icon: string;
  adminOnly?: boolean;
  section?: NavSection;
  children?: Array<{ section: NavSection; labelKey: string; adminOnly?: boolean }>;
}

const SIDEBAR_ENTRIES: NavEntry[] = [
  { kind: "item", id: "home", labelKey: "nav.home", icon: "home", section: "dashboard" },
  {
    kind: "group",
    id: "posts",
    labelKey: "nav.group_posts",
    icon: "posts",
    children: [
      { section: "editor", labelKey: "nav.post_new" },
      { section: "articles", labelKey: "nav.post_list" },
      { section: "drafts", labelKey: "nav.post_drafts" },
    ],
  },
  { kind: "item", id: "comments", labelKey: "nav.comments", icon: "comments", section: "comments" },
  {
    kind: "group",
    id: "content",
    labelKey: "nav.group_content",
    icon: "content",
    children: [
      { section: "categories", labelKey: "nav.categories" },
      { section: "tags", labelKey: "nav.tags" },
      { section: "series", labelKey: "nav.series" },
      { section: "books", labelKey: "nav.books" },
      { section: "pages", labelKey: "nav.pages" },
      { section: "nav_redirects", labelKey: "nav.nav_redirects" },
      { section: "media", labelKey: "nav.media" },
    ],
  },
  {
    kind: "group",
    id: "distribute",
    labelKey: "nav.group_distribute",
    icon: "distribute",
    children: [
      { section: "feed_seo", labelKey: "nav.feed_seo", adminOnly: true },
      { section: "github", labelKey: "nav.github", adminOnly: true },
      { section: "links", labelKey: "nav.links" },
      { section: "follows", labelKey: "nav.follows" },
      { section: "webhooks", labelKey: "nav.webhooks", adminOnly: true },
    ],
  },
  {
    kind: "group",
    id: "appearance",
    labelKey: "nav.group_appearance",
    icon: "appearance",
    adminOnly: true,
    children: [
      { section: "themes", labelKey: "nav.themes", adminOnly: true },
      { section: "locales", labelKey: "nav.locales", adminOnly: true },
      { section: "plugins", labelKey: "nav.plugins", adminOnly: true },
    ],
  },
  {
    kind: "group",
    id: "settings",
    labelKey: "nav.group_settings",
    icon: "settings",
    adminOnly: true,
    children: [
      { section: "settings_site", labelKey: "nav.settings_site", adminOnly: true },
      { section: "settings_io", labelKey: "nav.settings_io", adminOnly: true },
      { section: "users", labelKey: "nav.users", adminOnly: true },
      { section: "backup", labelKey: "nav.backup", adminOnly: true },
    ],
  },
  {
    kind: "group",
    id: "security",
    labelKey: "nav.group_security",
    icon: "security",
    children: [
      { section: "account", labelKey: "nav.account" },
      { section: "waf", labelKey: "nav.waf", adminOnly: true },
      { section: "logs", labelKey: "nav.logs", adminOnly: true },
      { section: "audit", labelKey: "nav.audit", adminOnly: true },
    ],
  },
  { kind: "item", id: "about", labelKey: "nav.about", icon: "info", section: "about" },
  { kind: "item", id: "help", labelKey: "nav.help", icon: "help", section: "help" },
];

function NavIcon({ name }: { name: string }) {
  switch (name) {
    case "home":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 10.5L12 3l9 7.5" />
          <path d="M5.5 9.5V20h13V9.5" />
        </svg>
      );
    case "posts":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M12 20h9" />
          <path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z" />
        </svg>
      );
    case "comments":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
        </svg>
      );
    case "content":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
        </svg>
      );
    case "distribute":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 11a9 9 0 0 1 9 9" />
          <path d="M4 4a16 16 0 0 1 16 16" />
          <circle cx="5" cy="19" r="1.5" fill="currentColor" />
        </svg>
      );
    case "appearance":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="13.5" cy="6.5" r=".8" fill="currentColor" />
          <circle cx="17.5" cy="10.5" r=".8" fill="currentColor" />
          <circle cx="8.5" cy="7.5" r=".8" fill="currentColor" />
          <circle cx="6.5" cy="12.5" r=".8" fill="currentColor" />
          <path d="M12 2a10 10 0 1 0 0 20 2.5 2.5 0 0 0 2.5-2.5c0-.6-.2-1.1-.6-1.5-.4-.4-.6-.9-.6-1.5A2.5 2.5 0 0 1 15.8 14H17a5 5 0 0 0 5-5c0-3.9-4.5-7-10-7z" />
        </svg>
      );
    case "settings":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <line x1="4" y1="6" x2="20" y2="6" />
          <line x1="4" y1="12" x2="20" y2="12" />
          <line x1="4" y1="18" x2="20" y2="18" />
          <circle cx="9" cy="6" r="2" fill="var(--volo-nav)" />
          <circle cx="15" cy="12" r="2" fill="var(--volo-nav)" />
          <circle cx="10" cy="18" r="2" fill="var(--volo-nav)" />
        </svg>
      );
    case "info":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="9.5" />
          <line x1="12" y1="16" x2="12" y2="11.5" />
          <circle cx="12" cy="8" r="0.8" fill="currentColor" />
        </svg>
      );
    case "help":
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="9.5" />
          <path d="M9.4 9.2a2.7 2.7 0 0 1 5.1 1c0 1.7-2.5 2.2-2.5 3.6" />
          <circle cx="12" cy="17.2" r="0.8" fill="currentColor" />
        </svg>
      );
    default:
      return (
        <svg class="volo-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
        </svg>
      );
  }
}

function ToastBanner() {
  const cur = toastSig.value;
  if (!cur) return null;
  return (
    <div
      key={cur.id}
      class={`volo-toast is-${cur.kind} ${cur.closing ? "is-closing" : ""}`}
      role="status"
      onMouseEnter={pauseToast}
      onMouseLeave={resumeToast}
    >
      <span class="volo-toast-icon" aria-hidden="true">
        {cur.kind === "err" ? "!" : cur.kind === "warn" ? "i" : "✓"}
      </span>
      <span class="volo-toast-text">{cur.text}</span>
      <button
        type="button"
        class="volo-toast-close"
        aria-label={t("common.close")}
        onClick={dismissToast}
      >
        ×
      </button>
    </div>
  );
}

interface CmdItem {
  id: string;
  title: string;
  sub: string;
  run: () => void;
}

function CommandPalette({
  open,
  onClose,
  isAdmin,
  onNavigate,
}: {
  open: boolean;
  onClose: () => void;
  isAdmin: boolean;
  onNavigate: (sec: string) => void;
}) {
  const [q, setQ] = useState("");
  const debouncedQ = useDebouncedValue(q, 250);
  const [idx, setIdx] = useState(0);
  const [posts, setPosts] = useState<Array<{ id: string; title: string; slug: string; status: string }>>([]);
  const [searchedQ, setSearchedQ] = useState("");
  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  const isDark = effectiveDarkSig.value;

  useEffect(() => {
    if (!open) return;
    setQ("");
    setIdx(0);
    window.setTimeout(() => inputRef.current?.focus(), 30);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const activeQ = debouncedQ.trim();
    const params = new URLSearchParams({ per_page: "50" });
    if (activeQ) params.set("q", activeQ);
    apiFetch<{ items?: any[]; articles?: any[] }>(`/api/admin/articles?${params.toString()}`)
      .then((res) => {
        const raw = res.items || res.articles || [];
        setPosts(
          raw.slice(0, 50).map((it: any) => {
            const a = it.article || it;
            return { id: a.id, title: a.title || a.slug, slug: a.slug || "", status: a.status || "published" };
          })
        );
        setSearchedQ(activeQ.toLowerCase());
      })
      .catch(() => {});
  }, [open, debouncedQ]);

  const items = useMemo<CmdItem[]>(() => {
    const out: CmdItem[] = [];
    for (const entry of SIDEBAR_ENTRIES) {
      if (entry.adminOnly && !isAdmin) continue;
      if (entry.kind === "item" && entry.section) {
        out.push({
          id: `sec:${entry.section}`,
          title: t(entry.labelKey),
          sub: t("nav.caption"),
          run: () => onNavigate(entry.section!),
        });
      }
      for (const ch of entry.children || []) {
        if (ch.adminOnly && !isAdmin) continue;
        out.push({
          id: `sec:${ch.section}`,
          title: t(ch.labelKey),
          sub: t(entry.labelKey),
          run: () => onNavigate(ch.section),
        });
      }
    }
    out.push({
      id: "act:theme",
      title: `${t("shell.mode_title")}: ${isDark ? t("mode.light") : t("mode.dark")}`,
      sub: t("shell.appearance_title"),
      run: () => toggleEffectiveTheme(),
    });
    out.push({
      id: "act:blog",
      title: t("shell.view_blog"),
      sub: "/",
      run: () => window.open("/", "_blank", "noopener"),
    });
    for (const p of posts) {
      out.push({
        id: `post:${p.id}`,
        title: p.title,
        sub: `/posts/${p.slug}`,
        run: () => {
          void (async () => {
            if (dirtyEditorSig.value) {
              const ok = await openConfirm({
                title: t("ui.unsaved_title"),
                message: t("ui.unsaved_msg"),
                confirmText: t("ui.unsaved_leave"),
                danger: true,
              });
              if (!ok) return;
              dirtyEditorSig.value = false;
            }
            sessionStorage.setItem("volo_jump_article_id", p.id);
            onNavigate("editor");
            window.dispatchEvent(new CustomEvent("volo:edit-article", { detail: p.id }));
          })();
        },
      });
    }
    const kw = q.trim().toLowerCase();
    if (!kw) return out.slice(0, 18);
    return out
      .filter((it) => {
        if (it.id.startsWith("post:") && searchedQ === kw) return true;
        return it.title.toLowerCase().includes(kw) || it.sub.toLowerCase().includes(kw);
      })
      .slice(0, 24);
  }, [q, isAdmin, posts, searchedQ, isDark, localeSig.value]);

  useEffect(() => {
    setIdx(0);
  }, [q]);

  useEffect(() => {
    if (!open) return;
    const activeEl = listRef.current?.querySelector(".volo-cmdk-item.active") as HTMLElement | null;
    activeEl?.scrollIntoView({ block: "nearest" });
  }, [idx, open]);

  return (
    <AnimatedModal
      open={open}
      onClose={onClose}
      title={t("ui.cmdk_title")}
      maxWidth="560px"
    >
      <div class="volo-cmdk-search">
        <input
          ref={inputRef}
          class="volo-input"
          placeholder={t("ui.cmdk_ph")}
          value={q}
          onInput={(e) => setQ((e.currentTarget as HTMLInputElement).value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setIdx((i) => (i + 1) % Math.max(1, items.length));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setIdx((i) => (i - 1 + items.length) % Math.max(1, items.length));
            } else if (e.key === "Enter" && items[idx]) {
              e.preventDefault();
              const target = items[idx];
              onClose();
              target.run();
            }
          }}
        />
      </div>
      <div ref={listRef} class="volo-cmdk-list">
        {items.length === 0 ? (
          <div class="volo-empty-state">{t("tbl.empty")}</div>
        ) : (
          items.map((it, i) => (
            <button
              key={it.id}
              type="button"
              class={`volo-cmdk-item ${i === idx ? "active" : ""}`}
              onMouseEnter={() => setIdx(i)}
              onClick={() => {
                onClose();
                it.run();
              }}
            >
              <span class="volo-cmdk-item-title">{it.title}</span>
              <span class="volo-code-chip">{it.sub}</span>
            </button>
          ))
        )}
      </div>
    </AnimatedModal>
  );
}

function findParentGroup(sec: NavSection): string | null {
  for (const entry of SIDEBAR_ENTRIES) {
    if (entry.kind === "group" && entry.children?.some((c) => c.section === sec)) {
      return entry.id;
    }
  }
  return null;
}

function resolveHashSection(): NavSection {
  if (typeof window === "undefined") return "dashboard";
  const raw = window.location.hash.replace(/^#/, "").trim();
  return (raw in SECTION_LABEL_KEYS ? raw : "dashboard") as NavSection;
}

function App() {
  const [ready, setReady] = useState(bootConfig.mode === "setup");
  const [section, setSection] = useState<NavSection>(resolveHashSection);
  const sectionRef = useRef<NavSection>(section);
  sectionRef.current = section;
  const ignoreHashRef = useRef<boolean>(false);
  const [openGroups, setOpenGroups] = useState<Record<string, boolean>>(() => {
    const initial = resolveHashSection();
    const p = findParentGroup(initial);
    return {
      posts: true,
      content: true,
      ...(p ? { [p]: true } : {}),
    };
  });
  const [bootAuthParams] = useState<{ challengeToken: string; oidcError: string }>(() => {
    if (typeof window === "undefined") return { challengeToken: "", oidcError: "" };
    const params = new URLSearchParams(window.location.search);
    const ct = (params.get("challenge_token") || "").trim();
    const oe = (params.get("oidc_error") || "").trim();
    if (ct || oe) {
      params.delete("challenge_token");
      params.delete("oidc_error");
      const qs = params.toString();
      const cleanUrl = `${window.location.pathname}${qs ? `?${qs}` : ""}${window.location.hash}`;
      window.history.replaceState({}, "", cleanUrl);
    }
    return { challengeToken: ct, oidcError: oe };
  });
  const oidcBindToastShownRef = useRef(false);
  const [appearanceOpen, setAppearanceOpen] = useState<boolean>(false);
  const [appearanceClosing, setAppearanceClosing] = useState<boolean>(false);
  const [appearanceStyle, setAppearanceStyle] = useState<CSSProperties>({});
  const [mobileMenuOpen, setMobileMenuOpen] = useState<boolean>(false);
  const [cmdkOpen, setCmdkOpen] = useState<boolean>(false);
  const [avatarErr, setAvatarErr] = useState<boolean>(false);
  const appCloseTimer = useRef<number | undefined>(undefined);
  const appBtnRef = useRef<HTMLButtonElement | null>(null);
  const appMenuRef = useRef<HTMLDivElement | null>(null);

  const curLocale = localeSig.value;
  const locales = availableLocalesSig.value;
  const user = currentUserSig.value;
  const curMode = themeModeSig.value;
  const isDark = effectiveDarkSig.value;
  const curAccent = accentSig.value;
  const isBusy = busyCountSig.value > 0;

  useEffect(() => {
    setAvatarErr(false);
  }, [user?.avatar_url]);

  const updateAppearancePos = () => {
    if (!appBtnRef.current) return;
    const rect = appBtnRef.current.getBoundingClientRect();
    const estH = appMenuRef.current?.offsetHeight || 236;
    setAppearanceStyle(computePortalMenuStyle(rect, { minW: 256, maxW: 300, maxH: 380, estH, align: "right" }).style);
  };

  const closeAppearanceAnimated = () => {
    if (!appearanceOpen || appearanceClosing) return;
    setAppearanceClosing(true);
    if (appCloseTimer.current) window.clearTimeout(appCloseTimer.current);
    appCloseTimer.current = window.setTimeout(() => {
      setAppearanceOpen(false);
      setAppearanceClosing(false);
    }, 140);
  };

  const toggleAppearance = () => {
    if (appearanceOpen && !appearanceClosing) {
      closeAppearanceAnimated();
    } else {
      if (appCloseTimer.current) window.clearTimeout(appCloseTimer.current);
      setAppearanceClosing(false);
      updateAppearancePos();
      setAppearanceOpen(true);
    }
  };

  const navigateTo = async (rawSec: string) => {
    const next = (rawSec in SECTION_LABEL_KEYS ? rawSec : "dashboard") as NavSection;
    if (next !== section && dirtyEditorSig.value) {
      const ok = await openConfirm({
        title: t("ui.unsaved_title"),
        message: t("ui.unsaved_msg"),
        confirmText: t("ui.unsaved_leave"),
        danger: true,
      });
      if (!ok) return;
      dirtyEditorSig.value = false;
    }
    setSection(next);
    setMobileMenuOpen(false);
    const parent = findParentGroup(next);
    if (parent) {
      setOpenGroups((prev) => ({ ...prev, [parent]: true }));
    }
    if (typeof window !== "undefined") {
      const targetHash = `#${next}`;
      if (window.location.hash !== targetHash) {
        window.history.replaceState(null, "", targetHash);
      }
    }
    resetConsoleScroll();
  };

  useEffect(() => {
    const initLoc = normalizeLocale(bootConfig.default_locale);
    void ensureLocaleLoaded(initLoc);
    applyThemeAttributes(themeModeSig.value, accentSig.value, false);
    if (bootConfig.mode !== "setup") {
      refreshMe().finally(() => setReady(true));
    }
  }, []);

  useEffect(() => {
    if (ready && user && bootAuthParams.oidcError && !oidcBindToastShownRef.current) {
      oidcBindToastShownRef.current = true;
      showToast(formatOidcError(bootAuthParams.oidcError), "err");
    }
  }, [ready, user, bootAuthParams.oidcError]);

  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (dirtyEditorSig.value) {
        e.preventDefault();
        e.returnValue = "";
      }
    };
    const onGlobalKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        const inCm = (e.target as HTMLElement | null)?.closest?.(".cm-editor");
        if (!inCm && currentUserSig.value && bootConfig.mode !== "setup") {
          e.preventDefault();
          setCmdkOpen((v) => !v);
        }
      } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "s") {
        if (currentUserSig.value && bootConfig.mode !== "setup") {
          e.preventDefault();
          window.dispatchEvent(new CustomEvent("volo:save"));
        }
      }
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    window.addEventListener("keydown", onGlobalKey);
    return () => {
      window.removeEventListener("beforeunload", onBeforeUnload);
      window.removeEventListener("keydown", onGlobalKey);
    };
  }, []);

  useEffect(() => {
    const onHashChange = () => {
      if (ignoreHashRef.current) {
        ignoreHashRef.current = false;
        return;
      }
      const next = resolveHashSection();
      if (next === sectionRef.current) return;
      if (dirtyEditorSig.value) {
        const prevSec = sectionRef.current;
        ignoreHashRef.current = true;
        window.history.replaceState(null, "", `#${prevSec}`);
        void openConfirm({
          title: t("ui.unsaved_title"),
          message: t("ui.unsaved_msg"),
          confirmText: t("ui.unsaved_leave"),
          danger: true,
        }).then((ok) => {
          if (!ok) return;
          dirtyEditorSig.value = false;
          setSection(next);
          const parent = findParentGroup(next);
          if (parent) {
            setOpenGroups((prev) => ({ ...prev, [parent]: true }));
          }
          window.history.replaceState(null, "", `#${next}`);
          resetConsoleScroll();
        });
        return;
      }
      setSection(next);
      const parent = findParentGroup(next);
      if (parent) {
        setOpenGroups((prev) => ({ ...prev, [parent]: true }));
      }
      resetConsoleScroll();
    };
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    if (!appearanceOpen) return;
    const onDocDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (appBtnRef.current?.contains(target) || appMenuRef.current?.contains(target)) return;
      closeAppearanceAnimated();
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") closeAppearanceAnimated();
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", updateAppearancePos);
    window.addEventListener("scroll", updateAppearancePos, true);
    vv?.addEventListener("resize", updateAppearancePos);
    vv?.addEventListener("scroll", updateAppearancePos);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", updateAppearancePos);
      window.removeEventListener("scroll", updateAppearancePos, true);
      vv?.removeEventListener("resize", updateAppearancePos);
      vv?.removeEventListener("scroll", updateAppearancePos);
    };
  }, [appearanceOpen, appearanceClosing]);

  useEffect(() => {
    document.documentElement.lang = curLocale;
    document.documentElement.dir = getLocaleDir(curLocale);
    if (bootConfig.mode === "setup") {
      document.title = t("setup.title");
    } else if (!user) {
      document.title = `${t("auth.sign_in")} · ${t("shell.console_title")}`;
    } else {
      const site = bootConfig.site_title || "Volo";
      document.title = `${t(SECTION_LABEL_KEYS[section])} · ${site} · ${t("shell.console_title")}`;
    }
  }, [curLocale, section, user]);

  if (bootConfig.mode === "setup") {
    return (
      <>
        <div class={`volo-top-progress ${isBusy ? "is-busy" : ""}`} />
        <SetupWizard />
        <GlobalConfirmModal />
        <ToastBanner />
      </>
    );
  }

  if (!ready) {
    return (
      <div class="volo-center-stage">
        <div class="volo-top-progress is-busy" />
        <span class="volo-help">{t("shell.loading")}</span>
      </div>
    );
  }

  if (!user) {
    return (
      <>
        <div class={`volo-top-progress ${isBusy ? "is-busy" : ""}`} />
        <LoginView
          initialChallengeToken={bootAuthParams.challengeToken}
          initialOidcError={bootAuthParams.oidcError}
        />
        <GlobalConfirmModal />
        <ToastBanner />
      </>
    );
  }

  const isAdmin = user.role === "admin";
  const activeSwatch = ACCENT_OPTIONS.find((a) => a.id === curAccent)?.swatch || "#246b51";
  const initialChar = (user.display_name || user.username || "V").trim().charAt(0).toUpperCase();

  return (
    <div class="volo-shell">
      <div class={`volo-top-progress ${isBusy ? "is-busy" : ""}`} />

      {mobileMenuOpen && (
        <div class="volo-mobile-backdrop" onClick={() => setMobileMenuOpen(false)} />
      )}

      <aside class={`volo-rail ${mobileMenuOpen ? "mobile-open" : ""}`}>
        <a class="volo-brand" href="/" title={t("shell.view_blog")}>
          <BrandLogo size={34} variant="rail" />
          <div style={{ minWidth: 0 }}>
            <div class="volo-brand-title">Volo</div>
            <div class="volo-brand-sub">{t("shell.brand_sub")}</div>
          </div>
        </a>

        <nav class="volo-nav-scroll">
          <div class="volo-nav-caption">{t("nav.caption")}</div>

          {SIDEBAR_ENTRIES.map((entry) => {
            if (entry.adminOnly && !isAdmin) return null;

            if (entry.kind === "item" && entry.section) {
              const isAct = section === entry.section;
              return (
                <button
                  key={entry.id}
                  type="button"
                  class={`volo-nav-btn ${isAct ? "active" : ""}`}
                  onClick={() => void navigateTo(entry.section!)}
                >
                  <span class="volo-nav-label">
                    <NavIcon name={entry.icon} />
                    <span>{t(entry.labelKey)}</span>
                  </span>
                </button>
              );
            }

            const visibleChildren = (entry.children || []).filter((c) => !c.adminOnly || isAdmin);
            if (visibleChildren.length === 0) return null;
            const hasActiveChild = visibleChildren.some((c) => c.section === section);
            const isOpen = Boolean(openGroups[entry.id]);

            return (
              <div
                key={entry.id}
                class={`volo-nav-group ${isOpen ? "open" : ""} ${hasActiveChild ? "has-active" : ""}`}
              >
                <button
                  type="button"
                  class="volo-nav-group-btn"
                  aria-expanded={isOpen}
                  onClick={() =>
                    setOpenGroups((prev) => ({ ...prev, [entry.id]: !prev[entry.id] }))
                  }
                >
                  <span class="volo-nav-label">
                    <NavIcon name={entry.icon} />
                    <span>{t(entry.labelKey)}</span>
                  </span>
                  <svg class="volo-nav-chev" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                    <polyline points="7 5 13 10 7 15" />
                  </svg>
                </button>

                <div class="volo-nav-sub-wrap" aria-hidden={!isOpen}>
                  <div class="volo-nav-sub">
                    {visibleChildren.map((child) => (
                      <button
                        key={child.section}
                        type="button"
                        tabIndex={isOpen ? 0 : -1}
                        class={`volo-nav-btn ${section === child.section ? "active" : ""}`}
                        onClick={() => void navigateTo(child.section)}
                      >
                        <span>{t(child.labelKey)}</span>
                      </button>
                    ))}
                  </div>
                </div>
              </div>
            );
          })}
        </nav>

        <div class="volo-rail-footer">
          <span>
            <span class="volo-rail-dot" />
            VOLO v1.0.0
          </span>
          <span>{t(`role.${user.role}`)}</span>
        </div>
      </aside>

      <div class="volo-main">
        <header class="volo-topbar">
          <div class="volo-topbar-left">
            <button
              type="button"
              class="volo-icon-btn volo-mobile-toggle"
              onClick={() => setMobileMenuOpen(!mobileMenuOpen)}
              aria-label={t("shell.console_title")}
            >
              <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
                <line x1="4" y1="7" x2="20" y2="7" />
                <line x1="4" y1="12" x2="20" y2="12" />
                <line x1="4" y1="17" x2="20" y2="17" />
              </svg>
            </button>
            <div style={{ minWidth: 0 }}>
              <div class="volo-eyebrow">{t("shell.eyebrow")}</div>
              <h1 class="volo-topbar-title">{t(SECTION_LABEL_KEYS[section])}</h1>
            </div>
          </div>

          <div class="volo-topbar-actions">
            {/* Command Palette Quick Jump (Ctrl/Cmd + K) */}
            <button
              type="button"
              class="volo-cmdk-trigger"
              title={`${t("ui.cmdk_title")} (${typeof navigator !== "undefined" && /Mac|iPhone|iPad|iPod/i.test(navigator.platform || navigator.userAgent) ? "⌘K" : "Ctrl+K"})`}
              onClick={() => setCmdkOpen(true)}
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                <circle cx="11" cy="11" r="7.5" />
                <line x1="20.5" y1="20.5" x2="16.4" y2="16.4" />
              </svg>
              <span class="volo-pill-label">{t("ui.cmdk_btn")}</span>
              <kbd class="volo-pill-label volo-cmdk-kbd">
                {typeof navigator !== "undefined" && /Mac|iPhone|iPad|iPod/i.test(navigator.platform || navigator.userAgent) ? "⌘K" : "Ctrl K"}
              </kbd>
            </button>

            {/* Unified Appearance Popover (4 Modes + Admin-Persisted Accent Swatches) */}
            <div class="volo-pop-wrap">
              <button
                ref={appBtnRef}
                type="button"
                class="volo-appearance-trigger"
                title={`${t("shell.appearance_title")}: ${t(`mode.${curMode}`)} · ${t(`accent.${curAccent}`)}`}
                aria-expanded={appearanceOpen && !appearanceClosing}
                onClick={toggleAppearance}
              >
                {isDark ? (
                  <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z" />
                  </svg>
                ) : (
                  <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                    <circle cx="12" cy="12" r="4.5" />
                    <path d="M12 2v2.2M12 19.8V22M4.9 4.9l1.6 1.6M17.5 17.5l1.6 1.6M2 12h2.2M19.8 12H22M4.9 19.1l1.6-1.6M17.5 6.5l1.6-1.6" />
                  </svg>
                )}
                <span class="volo-swatch-dot" style={{ background: activeSwatch, width: "12px", height: "12px" }} />
              </button>

              {appearanceOpen &&
                typeof document !== "undefined" &&
                createPortal(
                  <div
                    ref={appMenuRef}
                    class={`volo-cselect-menu is-portal volo-appearance-pop ${appearanceClosing ? "is-closing" : ""}`}
                    style={appearanceStyle}
                  >
                    <div class="volo-pop-caption">{t("shell.mode_title")}</div>
                    <div class="volo-mode-grid">
                      {MODE_OPTIONS.map((m) => (
                        <button
                          key={m}
                          type="button"
                          class={`volo-pop-item volo-mode-item ${curMode === m ? "active" : ""}`}
                          onClick={() => applyThemeAttributes(m, curAccent, true)}
                        >
                          <span class="volo-mode-icon" aria-hidden="true">
                            {m === "auto" ? (
                              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>
                            ) : m === "system" ? (
                              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>
                            ) : m === "light" ? (
                              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4.5"/><path d="M12 2v2.2M12 19.8V22M4.9 4.9l1.6 1.6M17.5 17.5l1.6 1.6M2 12h2.2M19.8 12H22M4.9 19.1l1.6-1.6M17.5 6.5l1.6-1.6"/></svg>
                            ) : (
                              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z"/></svg>
                            )}
                          </span>
                          <span class="volo-mode-copy">
                            <span>{t(`mode.${m}`)}</span>
                            <span class="volo-mode-sub">{t(`mode.${m}_desc`)}</span>
                          </span>
                          {curMode === m && <span>✓</span>}
                        </button>
                      ))}
                    </div>

                    {isAdmin && (
                      <>
                        <div class="volo-pop-divider" />
                        <div class="volo-pop-caption">{t("shell.accent_title")}</div>
                        <div class="volo-accent-row">
                          {ACCENT_OPTIONS.map((ac) => {
                            const isAct = curAccent === ac.id;
                            return (
                              <button
                                key={ac.id}
                                type="button"
                                class={`volo-accent-dot-btn ${isAct ? "active" : ""}`}
                                style={{ background: ac.swatch }}
                                title={t(`accent.${ac.id}`)}
                                aria-label={t(`accent.${ac.id}`)}
                                onClick={() => void saveGlobalAccent(ac.id)}
                              >
                                {isAct && <span>✓</span>}
                              </button>
                            );
                          })}
                        </div>
                      </>
                    )}
                  </div>,
                  document.body
                )}
            </div>

            {/* Scalable Language Popover */}
            <LocalePopover
              value={localeSig.value}
              locales={locales}
              onChange={async (nextRaw) => {
                const next = normalizeLocale(nextRaw);
                await ensureLocaleLoaded(next);
                await apiFetch("/api/admin/me", {
                  method: "PATCH",
                  body: JSON.stringify({ preferred_locale: next }),
                }).catch(() => {});
              }}
            />

            {/* Open Public Blog */}
            <a class="volo-site-pill" href="/" target="_blank" rel="noreferrer" title={t("shell.view_blog")}>
              <span class="volo-site-arrow">↗</span>
              <span class="volo-pill-label">{t("shell.view_blog")}</span>
            </a>

            {/* User Avatar Pill */}
            <button
              type="button"
              class="volo-user-pill"
              style={{ cursor: "pointer" }}
              onClick={() => void navigateTo("account")}
              title={t("nav.account")}
            >
              <span class="volo-user-avatar">
                {user.avatar_url && !avatarErr ? (
                  <img src={user.avatar_url} alt="" onError={() => setAvatarErr(true)} />
                ) : (
                  initialChar
                )}
              </span>
              <span class="volo-pill-label">{user.display_name || user.username}</span>
            </button>

            {/* Logout Icon Button */}
            <button
              type="button"
              class="volo-icon-btn"
              title={t("nav.logout")}
              onClick={async () => {
                if (dirtyEditorSig.value) {
                  const ok = await openConfirm({
                    title: t("ui.unsaved_title"),
                    message: t("ui.unsaved_msg"),
                    confirmText: t("ui.unsaved_leave"),
                    danger: true,
                  });
                  if (!ok) return;
                  dirtyEditorSig.value = false;
                }
                await apiFetch("/api/admin/auth/logout", { method: "POST", body: "{}" }).catch(() => {});
                currentUserSig.value = null;
                csrfTokenSig.value = "";
              }}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
                <polyline points="16 17 21 12 16 7" />
                <line x1="21" y1="12" x2="9" y2="12" />
              </svg>
            </button>
          </div>
        </header>

        <main class="volo-content">
          <div key={section} class="volo-view-enter">
            {(section === "dashboard" || section === "editor" || section === "articles" || section === "drafts") && (
              <OverviewWorkspace section={section} onNavigate={(s) => void navigateTo(s)} />
            )}

            {(section === "comments" ||
              section === "categories" ||
              section === "tags" ||
              section === "series" ||
              section === "books" ||
              section === "pages" ||
              section === "media") && (
              <ContentWorkspace activeTab={section} onNavigate={(s) => void navigateTo(s)} />
            )}

            {(section === "nav_redirects" ||
              section === "feed_seo" ||
              section === "github" ||
              section === "links" ||
              section === "follows" ||
              section === "webhooks") && (
              <DistributeWorkspace activeTab={section} onNavigate={(s) => void navigateTo(s)} />
            )}

            {(section === "themes" || section === "locales" || section === "plugins") && isAdmin && (
              <AppearanceWorkspace activeTab={section} onNavigate={(s) => void navigateTo(s)} />
            )}

            {(section === "settings_site" ||
              section === "settings_io" ||
              section === "users" ||
              section === "backup" ||
              section === "account" ||
              section === "waf" ||
              section === "logs" ||
              section === "audit") && (
              <SystemWorkspace activeTab={section} onNavigate={(s) => void navigateTo(s)} />
            )}

            {(section === "about" || section === "help") && (
              <AboutHelpWorkspace activeTab={section} onNavigate={(s) => void navigateTo(s)} />
            )}
          </div>
        </main>
      </div>

      <CommandPalette
        open={cmdkOpen}
        onClose={() => setCmdkOpen(false)}
        isAdmin={isAdmin}
        onNavigate={(s) => void navigateTo(s)}
      />
      <GlobalConfirmModal />
      <ToastBanner />
    </div>
  );
}

const root = document.getElementById("app");
if (root) {
  render(<App />, root);
}
