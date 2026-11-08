import { useEffect, useState } from "preact/hooks";
import {
  apiFetch,
  currentUserSig,
  effectiveDarkSig,
  openConfirm,
  resetConsoleScroll,
  showToast,
  storageDriverSig,
} from "../api";
import { MediaPickButton } from "../components/MediaLibraryModal";
import {
  ActionMenu,
  AnimatedModal,
  BatchBar,
  ConsoleEmpty,
  ConsoleSelect,
  MobileSelectAllBar,
  NavIconPreview,
  paginateItems,
  Pagination,
  SwitchToggle,
  TableSkeleton,
  TokenListInput,
  UploadButton,
  useDebouncedValue,
} from "../components/UIControls";
import { availableLocalesSig, formatDateTime, t } from "../i18n";

export type DistTab = "nav_redirects" | "feed_seo" | "github" | "links" | "follows" | "webhooks";

const WH_EVENTS = [
  { key: "article.published", labelKey: "dist.wh_ev_post" },
  { key: "comment.created", labelKey: "dist.wh_ev_cmt" },
  { key: "security.alert", labelKey: "dist.wh_ev_sec" },
  { key: "backup.completed", labelKey: "dist.wh_ev_bak" },
] as const;

const NAV_ICON_PRESETS = ["github", "book", "rss", "mail", "link", "external", "admin"] as const;

const FEED_VERIFY_LINKS = ["/rss.xml", "/atom.xml", "/sitemap.xml", "/robots.txt", "/llms.txt"] as const;

const EMPTY_NAV = {
  id: "",
  parent_id: "",
  title: "",
  url: "/",
  i18n: {} as Record<string, string>,
  icon: "",
  sort_order: 10,
  open_in_new_tab: false,
  is_enabled: true,
};

const EMPTY_REDIR = { id: "", source_path: "", target_path: "", status_code: 301 };

const EMPTY_LINK = {
  id: "",
  title: "",
  url: "",
  icon_url: "",
  description: "",
  category_name: "",
  sort_order: 10,
};

const EMPTY_FOLLOW = {
  id: "",
  title: "",
  site_url: "",
  feed_url: "",
  icon_url: "",
  description: "",
  sort_order: 10,
};

const EMPTY_WH = {
  id: "",
  name: "",
  provider: "custom",
  url: "",
  chat_id: "",
  secret: "",
  secret_set: false,
  clear_secret: false,
  events: ["article.published", "comment.created"] as string[],
  is_enabled: true,
};

function distTabLabel(k: DistTab): string {
  switch (k) {
    case "nav_redirects":
      return t("dist.nav_redirects");
    case "feed_seo":
      return t("dist.feed_seo");
    case "github":
      return t("nav.github");
    case "links":
      return t("dist.links");
    case "follows":
      return t("dist.follows");
    case "webhooks":
      return t("dist.webhooks");
  }
}

function whProviderLabel(prov: string): string {
  switch (prov) {
    case "feishu":
      return t("dist.wh_feishu");
    case "dingtalk":
      return t("dist.wh_dingtalk");
    case "wecom":
      return t("dist.wh_wecom");
    case "bark":
      return t("dist.wh_bark");
    case "telegram":
      return t("dist.wh_telegram");
    default:
      return t("dist.wh_custom");
  }
}

function whEventText(evKey: string): string {
  const hit = WH_EVENTS.find((e) => e.key === evKey);
  if (!hit) return evKey;
  switch (hit.labelKey) {
    case "dist.wh_ev_post":
      return t("dist.wh_ev_post");
    case "dist.wh_ev_cmt":
      return t("dist.wh_ev_cmt");
    case "dist.wh_ev_sec":
      return t("dist.wh_ev_sec");
    case "dist.wh_ev_bak":
      return t("dist.wh_ev_bak");
  }
}

function parseJsonArr(raw: any): string[] {
  if (Array.isArray(raw)) return raw.map(String);
  try {
    const p = raw ? JSON.parse(raw) : [];
    return Array.isArray(p) ? p.map(String) : [];
  } catch {
    return [];
  }
}

function parseI18nMap(raw: any): Record<string, string> {
  if (raw && typeof raw === "object" && !Array.isArray(raw)) {
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(raw)) {
      if (typeof v === "string" && v) out[k] = v;
    }
    return out;
  }
  try {
    const parsed = raw ? JSON.parse(raw) : {};
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const out: Record<string, string> = {};
      for (const [k, v] of Object.entries(parsed)) {
        if (typeof v === "string" && v) out[k] = v;
      }
      return out;
    }
  } catch {
    // ignore
  }
  return {};
}

export function DistributeWorkspace({
  activeTab = "links",
  onNavigate,
}: {
  activeTab?: DistTab;
  onNavigate?: (sec: string) => void;
}) {
  const isAdmin = currentUserSig.value?.role === "admin";
  const effectiveDark = effectiveDarkSig.value;
  const localeOptions = availableLocalesSig.value;
  const [sub, setSub] = useState<DistTab>(activeTab);
  const [navSub, setNavSub] = useState<"nav" | "redirects">("nav");
  const [pageIdx, setPageIdx] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [loading, setLoading] = useState(false);

  // Nav
  const [navItems, setNavItems] = useState<any[]>([]);
  const [navSearch, setNavSearch] = useState("");
  const [navModalOpen, setNavModalOpen] = useState(false);
  const [navForm, setNavForm] = useState({ ...EMPTY_NAV });

  // Redirects (server-paginated + Import/Export)
  const [redirects, setRedirects] = useState<any[]>([]);
  const [redirTotal, setRedirTotal] = useState(0);
  const [redirSearch, setRedirSearch] = useState("");
  const debouncedRedirQ = useDebouncedValue(redirSearch, 250);
  const [redirCodeFilter, setRedirCodeFilter] = useState("");
  const [redirSourceFilter, setRedirSourceFilter] = useState("");
  const [redirSort, setRedirSort] = useState("newest");
  const [selRedirIds, setSelRedirIds] = useState<string[]>([]);
  const [redirModalOpen, setRedirModalOpen] = useState(false);
  const [redirForm, setRedirForm] = useState({ ...EMPTY_REDIR });
  const [redirImportOpen, setRedirImportOpen] = useState(false);
  const [redirImportFormat, setRedirImportFormat] = useState<"csv" | "json">("csv");
  const [redirImportData, setRedirImportData] = useState("");
  const [importingRedir, setImportingRedir] = useState(false);

  // Links (server-paginated + category + single check + batch move category)
  const [links, setLinks] = useState<any[]>([]);
  const [linkTotal, setLinkTotal] = useState(0);
  const [knownLinkCategories, setKnownLinkCategories] = useState<string[]>([]);
  const [linkCatFilter, setLinkCatFilter] = useState("");
  const [linkHealthFilter, setLinkHealthFilter] = useState("");
  const [linkSearch, setLinkSearch] = useState("");
  const debouncedLinkQ = useDebouncedValue(linkSearch, 250);
  const [patrolling, setPatrolling] = useState(false);
  const [checkingLinkId, setCheckingLinkId] = useState("");
  const [selLinkIds, setSelLinkIds] = useState<string[]>([]);
  const [linkModalOpen, setLinkModalOpen] = useState(false);
  const [linkForm, setLinkForm] = useState({ ...EMPTY_LINK });
  const [batchCatOpen, setBatchCatOpen] = useState(false);
  const [batchCatName, setBatchCatName] = useState("");
  const [movingLinksCat, setMovingLinksCat] = useState(false);

  // Follows + Synced Items Preview + OPML Import/Export (server-paginated)
  const [follows, setFollows] = useState<any[]>([]);
  const [followTotal, setFollowTotal] = useState(0);
  const [followSearch, setFollowSearch] = useState("");
  const debouncedFollowQ = useDebouncedValue(followSearch, 250);
  const [followStatusFilter, setFollowStatusFilter] = useState("");
  const [syncingFollows, setSyncingFollows] = useState(false);
  const [syncingFollowId, setSyncingFollowId] = useState("");
  const [followModalOpen, setFollowModalOpen] = useState(false);
  const [followForm, setFollowForm] = useState({ ...EMPTY_FOLLOW });
  const [followPreviewOpen, setFollowPreviewOpen] = useState(false);
  const [followGroups, setFollowGroups] = useState<any[]>([]);
  const [followPreviewLoading, setFollowPreviewLoading] = useState(false);
  const [opmlImportOpen, setOpmlImportOpen] = useState(false);
  const [opmlData, setOpmlData] = useState("");
  const [importingOpml, setImportingOpml] = useState(false);

  // Webhooks + Deliveries Log Modal
  const [webhooks, setWebhooks] = useState<any[]>([]);
  const [whSearch, setWhSearch] = useState("");
  const [testingWhId, setTestingWhId] = useState("");
  const [whModalOpen, setWhModalOpen] = useState(false);
  const [whForm, setWhForm] = useState({ ...EMPTY_WH });
  const [whDelivModal, setWhDelivModal] = useState<{
    webhook: any;
    items: any[];
    loading: boolean;
  } | null>(null);
  const [expandedDelivId, setExpandedDelivId] = useState<string | null>(null);
  const [replayingDelivId, setReplayingDelivId] = useState("");

  // Feed / SEO / GitHub Showcase
  const [feedCfg, setFeedCfg] = useState<any>(null);
  const [feedVer, setFeedVer] = useState(0);
  const [savingFeed, setSavingFeed] = useState(false);
  const [seoCfg, setSeoCfg] = useState<any>(null);
  const [seoVer, setSeoVer] = useState(0);
  const [savingSeo, setSavingSeo] = useState(false);
  const [syncingGh, setSyncingGh] = useState(false);
  const [publishingGh, setPublishingGh] = useState(false);
  const [pushingIndexNow, setPushingIndexNow] = useState(false);
  const [ghSearch, setGhSearch] = useState("");

  useEffect(() => {
    setSub(activeTab);
    setPageIdx(1);
    setSelRedirIds([]);
    setSelLinkIds([]);
    resetConsoleScroll();
  }, [activeTab]);

  const switchDistSub = (next: DistTab) => {
    setSub(next);
    setPageIdx(1);
    setSelRedirIds([]);
    setSelLinkIds([]);
    resetConsoleScroll();
    onNavigate?.(next);
  };

  const switchNavSub = (next: "nav" | "redirects") => {
    setNavSub(next);
    setPageIdx(1);
    setSelRedirIds([]);
    resetConsoleScroll();
  };

  const loadNav = async () => {
    setLoading(true);
    try {
      const nv = await apiFetch<{ items: any[] }>("/api/admin/nav?per_page=500");
      setNavItems(nv.items || []);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadRedirects = async () => {
    setLoading(true);
    try {
      const sp = new URLSearchParams();
      sp.set("page", String(pageIdx));
      sp.set("per_page", String(pageSize));
      if (debouncedRedirQ.trim()) sp.set("q", debouncedRedirQ.trim());
      if (redirCodeFilter) sp.set("status_code", redirCodeFilter);
      if (redirSourceFilter) sp.set("source", redirSourceFilter);
      if (redirSort) sp.set("sort", redirSort);
      const rd = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/redirects?${sp.toString()}`);
      const list = rd.items || [];
      setRedirects(list);
      setRedirTotal(rd.total ?? list.length);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadLinks = async () => {
    setLoading(true);
    try {
      const sp = new URLSearchParams();
      sp.set("page", String(pageIdx));
      sp.set("per_page", String(pageSize));
      if (debouncedLinkQ.trim()) sp.set("q", debouncedLinkQ.trim());
      if (linkHealthFilter) sp.set("health", linkHealthFilter);
      if (linkCatFilter) sp.set("category", linkCatFilter);
      const r = await apiFetch<{ items: any[]; total?: number; categories?: string[] }>(
        `/api/admin/links?${sp.toString()}`
      );
      const list = r.items || [];
      setLinks(list);
      setLinkTotal(r.total ?? list.length);
      const itemCats = list.map((l) => String(l.category_name || "").trim()).filter(Boolean);
      const serverCats = Array.isArray(r.categories)
        ? r.categories.map((c) => String(c || "").trim()).filter(Boolean)
        : [];
      if (itemCats.length > 0 || serverCats.length > 0) {
        setKnownLinkCategories((prev) => Array.from(new Set([...prev, ...serverCats, ...itemCats])).sort());
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadFollows = async () => {
    setLoading(true);
    try {
      const sp = new URLSearchParams();
      sp.set("page", String(pageIdx));
      sp.set("per_page", String(pageSize));
      if (debouncedFollowQ.trim()) sp.set("q", debouncedFollowQ.trim());
      if (followStatusFilter) sp.set("status", followStatusFilter);
      const r = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/follows?${sp.toString()}`);
      const list = r.items || [];
      setFollows(list);
      setFollowTotal(r.total ?? list.length);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  const loadSub = async () => {
    if (sub === "nav_redirects") {
      if (navSub === "nav") {
        await loadNav();
      } else {
        await loadRedirects();
      }
      return;
    }
    if (sub === "links") {
      await loadLinks();
      return;
    }
    if (sub === "follows") {
      await loadFollows();
      return;
    }
    setLoading(true);
    try {
      if (sub === "webhooks" && isAdmin) {
        const r = await apiFetch<{ items: any[] }>("/api/admin/webhooks?per_page=200");
        setWebhooks(r.items || []);
      } else if ((sub === "feed_seo" || sub === "github") && isAdmin) {
        const s = await apiFetch<any>("/api/admin/settings");
        setFeedCfg(s.domains?.feed || {});
        setFeedVer(s.versions?.feed || 0);
        setSeoCfg(s.domains?.seo || {});
        setSeoVer(s.versions?.seo || 0);
        if (s.domains?.storage?.driver) {
          storageDriverSig.value = s.domains.storage.driver;
        }
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  // Non-server-paginated tabs & Top Nav load
  useEffect(() => {
    if (sub === "nav_redirects" && navSub === "nav") {
      void loadNav();
    } else if (sub === "webhooks" || sub === "feed_seo" || sub === "github") {
      void loadSub();
    }
  }, [sub, navSub]);

  // Redirects server-paginated fetch
  useEffect(() => {
    if (sub !== "nav_redirects" || navSub !== "redirects") return;
    if (redirSearch !== debouncedRedirQ) return;
    void loadRedirects();
  }, [sub, navSub, pageIdx, pageSize, debouncedRedirQ, redirCodeFilter, redirSourceFilter, redirSort]);

  // Links server-paginated fetch
  useEffect(() => {
    if (sub !== "links") return;
    if (linkSearch !== debouncedLinkQ) return;
    void loadLinks();
  }, [sub, pageIdx, pageSize, debouncedLinkQ, linkHealthFilter, linkCatFilter]);

  // Follows server-paginated fetch
  useEffect(() => {
    if (sub !== "follows") return;
    if (followSearch !== debouncedFollowQ) return;
    void loadFollows();
  }, [sub, pageIdx, pageSize, debouncedFollowQ, followStatusFilter]);

  const distTabs: DistTab[] = isAdmin
    ? ["feed_seo", "github", "links", "follows", "webhooks"]
    : ["links", "follows"];

  const genRandomHexKey = () => {
    const arr = new Uint8Array(16);
    crypto.getRandomValues(arr);
    return Array.from(arr, (b) => b.toString(16).padStart(2, "0")).join("");
  };

  const saveNav = async (e?: Event) => {
    e?.preventDefault();
    if (!navForm.title.trim() || !navForm.url.trim()) return;
    try {
      const i18nTitles: Record<string, string> = {};
      for (const [code, val] of Object.entries(navForm.i18n || {})) {
        const trimmed = String(val || "").trim();
        if (trimmed) i18nTitles[code] = trimmed;
      }
      const payload = {
        parent_id: navForm.parent_id || "",
        title: navForm.title.trim(),
        url: navForm.url.trim(),
        icon: navForm.icon.trim(),
        i18n_titles_json: JSON.stringify(i18nTitles),
        sort_order: Number(navForm.sort_order) || 10,
        open_in_new_tab: navForm.open_in_new_tab,
        is_enabled: navForm.is_enabled !== false,
      };
      if (!navForm.id) {
        await apiFetch("/api/admin/nav", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/nav/${navForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setNavModalOpen(false);
      setNavForm({ ...EMPTY_NAV });
      showToast(t("common.saved"));
      void loadNav();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const sortedRawNav = [...navItems].sort((a, b) => Number(a.sort_order ?? 0) - Number(b.sort_order ?? 0));
  const topLevelNav = sortedRawNav.filter((x) => !x.parent_id);
  const topNavIdSet = new Set(topLevelNav.map((x) => x.id));
  const orderedNavRows: Array<{ item: any; isChild: boolean; sibIdx: number; sibCount: number }> = [];
  topLevelNav.forEach((top, ti) => {
    orderedNavRows.push({ item: top, isChild: false, sibIdx: ti, sibCount: topLevelNav.length });
    const children = sortedRawNav.filter((c) => c.parent_id === top.id);
    children.forEach((ch, ci) => {
      orderedNavRows.push({ item: ch, isChild: true, sibIdx: ci, sibCount: children.length });
    });
  });
  const orphanNav = sortedRawNav.filter((x) => x.parent_id && !topNavIdSet.has(x.parent_id));
  orphanNav.forEach((orph, oi) => {
    orderedNavRows.push({ item: orph, isChild: true, sibIdx: oi, sibCount: orphanNav.length });
  });

  const swapNavOrder = async (itemId: string, dir: -1 | 1) => {
    const target = sortedRawNav.find((x) => x.id === itemId);
    if (!target) return;
    const parentKey = target.parent_id || "";
    const siblings = sortedRawNav.filter((x) => (x.parent_id || "") === parentKey);
    const idx = siblings.findIndex((x) => x.id === itemId);
    const targetIdx = idx + dir;
    if (idx < 0 || targetIdx < 0 || targetIdx >= siblings.length) return;
    const hasTies = new Set(siblings.map((s) => Number(s.sort_order ?? 0))).size < siblings.length;
    try {
      if (hasTies) {
        const reordered = [...siblings];
        const [moved] = reordered.splice(idx, 1);
        reordered.splice(targetIdx, 0, moved);
        await Promise.all(
          reordered.map((s, i) => {
            const desired = (i + 1) * 10;
            if (Number(s.sort_order) === desired) return Promise.resolve();
            return apiFetch(`/api/admin/nav/${s.id}`, {
              method: "PUT",
              body: JSON.stringify({ ...s, sort_order: desired, is_enabled: s.is_enabled !== false }),
            });
          })
        );
      } else {
        const a = siblings[idx];
        const b = siblings[targetIdx];
        const orderA = Number(a.sort_order ?? (idx + 1) * 10);
        const orderB = Number(b.sort_order ?? (targetIdx + 1) * 10);
        await Promise.all([
          apiFetch(`/api/admin/nav/${a.id}`, {
            method: "PUT",
            body: JSON.stringify({ ...a, sort_order: orderB, is_enabled: a.is_enabled !== false }),
          }),
          apiFetch(`/api/admin/nav/${b.id}`, {
            method: "PUT",
            body: JSON.stringify({ ...b, sort_order: orderA, is_enabled: b.is_enabled !== false }),
          }),
        ]);
      }
      void loadNav();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveRedirect = async (e?: Event) => {
    e?.preventDefault();
    const src = redirForm.source_path.trim();
    const dst = redirForm.target_path.trim();
    if (!src || !dst) return;
    const normSrc = src.startsWith("/") ? src.replace(/\/+$/, "") || "/" : src;
    const normDst = dst.startsWith("/") ? dst.replace(/\/+$/, "") || "/" : dst;
    if (src === dst || normSrc === normDst) {
      showToast(t("dist.redir_self_loop"), "err");
      return;
    }
    try {
      const payload = {
        source_path: src,
        target_path: dst,
        status_code: Number(redirForm.status_code) || 301,
      };
      if (!redirForm.id) {
        await apiFetch("/api/admin/redirects", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/redirects/${redirForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setRedirModalOpen(false);
      setRedirForm({ ...EMPTY_REDIR });
      showToast(t("common.saved"));
      void loadRedirects();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const submitImportRedirects = async (e?: Event) => {
    e?.preventDefault();
    if (!redirImportData.trim() || importingRedir) return;
    setImportingRedir(true);
    try {
      const res = await apiFetch<{ imported?: number; count?: number }>("/api/admin/redirects/import", {
        method: "POST",
        body: JSON.stringify({ format: redirImportFormat, data: redirImportData }),
      });
      setRedirImportOpen(false);
      setRedirImportData("");
      showToast(t("dist.redir_imported", { count: res.imported ?? res.count ?? 0 }), "ok");
      void loadRedirects();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setImportingRedir(false);
    }
  };

  const batchDeleteRedirects = async () => {
    if (!selRedirIds.length) return;
    if (!(await openConfirm({ message: t("dist.confirm_batch_del_redir", { n: selRedirIds.length }), danger: true }))) return;
    try {
      await apiFetch("/api/admin/redirects/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selRedirIds, action: "delete" }),
      });
      setSelRedirIds([]);
      showToast(t("common.deleted"));
      void loadRedirects();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveLink = async (e?: Event) => {
    e?.preventDefault();
    if (!linkForm.title.trim() || !linkForm.url.trim()) return;
    try {
      const payload = {
        title: linkForm.title.trim(),
        url: linkForm.url.trim(),
        icon_url: linkForm.icon_url.trim(),
        description: linkForm.description,
        category_name: linkForm.category_name.trim(),
        sort_order: Number(linkForm.sort_order) || 0,
      };
      if (!linkForm.id) {
        await apiFetch("/api/admin/links", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/links/${linkForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      if (payload.category_name) {
        setKnownLinkCategories((prev) => Array.from(new Set([...prev, payload.category_name])).sort());
      }
      setLinkModalOpen(false);
      setLinkForm({ ...EMPTY_LINK });
      showToast(t("common.saved"));
      void loadLinks();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const checkSingleLink = async (linkId: string) => {
    if (checkingLinkId) return;
    setCheckingLinkId(linkId);
    try {
      await apiFetch(`/api/admin/links/${encodeURIComponent(linkId)}/check`, {
        method: "POST",
        body: "{}",
      });
      showToast(t("common.saved"), "ok");
      await loadLinks();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setCheckingLinkId("");
    }
  };

  const submitBatchMoveLinkCat = async (e?: Event) => {
    e?.preventDefault();
    if (!selLinkIds.length || movingLinksCat) return;
    setMovingLinksCat(true);
    try {
      const cat = batchCatName.trim();
      await apiFetch("/api/admin/links/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selLinkIds, action: "category", category_name: cat }),
      });
      if (cat) {
        setKnownLinkCategories((prev) => Array.from(new Set([...prev, cat])).sort());
      }
      setBatchCatOpen(false);
      setBatchCatName("");
      setSelLinkIds([]);
      showToast(t("common.saved"), "ok");
      void loadLinks();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setMovingLinksCat(false);
    }
  };

  const batchDeleteLinks = async () => {
    if (!selLinkIds.length) return;
    if (!(await openConfirm({ message: t("dist.confirm_batch_del_links", { n: selLinkIds.length }), danger: true }))) return;
    try {
      await apiFetch("/api/admin/links/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selLinkIds, action: "delete" }),
      });
      setSelLinkIds([]);
      showToast(t("common.deleted"));
      void loadLinks();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveFollow = async (e?: Event) => {
    e?.preventDefault();
    if (!followForm.title.trim() || !followForm.feed_url.trim()) return;
    try {
      const payload = {
        title: followForm.title.trim(),
        site_url: followForm.site_url.trim(),
        feed_url: followForm.feed_url.trim(),
        icon_url: followForm.icon_url.trim(),
        description: followForm.description,
        sort_order: Number(followForm.sort_order) || 0,
      };
      if (!followForm.id) {
        await apiFetch("/api/admin/follows", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/follows/${followForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setFollowModalOpen(false);
      setFollowForm({ ...EMPTY_FOLLOW });
      showToast(t("common.saved"));
      void loadFollows();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const syncSingleFollow = async (followId: string) => {
    if (syncingFollowId) return;
    setSyncingFollowId(followId);
    try {
      const r = await apiFetch<{ synced_items?: number }>(
        `/api/admin/follows/${encodeURIComponent(followId)}/sync`,
        { method: "POST", body: "{}" }
      );
      showToast(t("dist.follows_synced", { count: r.synced_items ?? 0 }), "ok");
      await loadFollows();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setSyncingFollowId("");
    }
  };

  const submitImportOpml = async (e?: Event) => {
    e?.preventDefault();
    if (!opmlData.trim() || importingOpml) return;
    setImportingOpml(true);
    try {
      const r = await apiFetch<{ imported?: number; count?: number }>("/api/admin/follows/import-opml", {
        method: "POST",
        body: JSON.stringify({ opml: opmlData }),
      });
      setOpmlImportOpen(false);
      setOpmlData("");
      showToast(t("dist.follows_opml_imported", { count: r.imported ?? r.count ?? 0 }), "ok");
      void loadFollows();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setImportingOpml(false);
    }
  };

  const openFollowPreview = async () => {
    setFollowPreviewOpen(true);
    setFollowPreviewLoading(true);
    try {
      const r = await apiFetch<{ groups?: any[]; items?: any[] }>("/api/admin/follows/items");
      setFollowGroups(r.groups || r.items || []);
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setFollowPreviewLoading(false);
    }
  };

  const saveWebhook = async (e?: Event) => {
    e?.preventDefault();
    if (!whForm.name.trim() || !whForm.url.trim()) return;
    try {
      const payload: Record<string, any> = {
        name: whForm.name.trim(),
        provider: whForm.provider,
        url: whForm.url.trim(),
        chat_id: whForm.chat_id.trim(),
        secret: whForm.secret,
        clear_secret: Boolean(whForm.clear_secret && !whForm.secret),
        events: whForm.events,
        is_enabled: whForm.is_enabled,
      };
      if (!whForm.id) {
        await apiFetch("/api/admin/webhooks", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/webhooks/${whForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setWhModalOpen(false);
      setWhForm({ ...EMPTY_WH });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const openWebhookDeliveries = async (w: any) => {
    setExpandedDelivId(null);
    setWhDelivModal({ webhook: w, items: [], loading: true });
    try {
      const r = await apiFetch<{ items?: any[] }>(`/api/admin/webhooks/${encodeURIComponent(w.id)}/deliveries`);
      setWhDelivModal({ webhook: w, items: r.items || [], loading: false });
    } catch (err: any) {
      showToast(err.message, "err");
      setWhDelivModal((prev) => (prev ? { ...prev, loading: false } : null));
    }
  };

  const replayWebhookDelivery = async (wId: string, deliveryId: string) => {
    if (replayingDelivId) return;
    setReplayingDelivId(deliveryId);
    try {
      await apiFetch(
        `/api/admin/webhooks/${encodeURIComponent(wId)}/deliveries/${encodeURIComponent(deliveryId)}/replay`,
        { method: "POST", body: "{}" }
      );
      showToast(t("dist.wh_replayed"), "ok");
      const r = await apiFetch<{ items?: any[] }>(`/api/admin/webhooks/${encodeURIComponent(wId)}/deliveries`);
      setWhDelivModal((prev) => (prev ? { ...prev, items: r.items || [] } : null));
      void loadSub();
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setReplayingDelivId("");
    }
  };

  const toggleWebhookEnabled = async (w: any, evList: string[], next: boolean) => {
    try {
      await apiFetch(`/api/admin/webhooks/${w.id}`, {
        method: "PUT",
        body: JSON.stringify({
          name: w.name,
          provider: w.provider || w.platform || "custom",
          url: w.url || w.target_url,
          chat_id: w.chat_id || "",
          events: evList,
          is_enabled: next,
        }),
      });
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveSeoSettings = async (silent = false) => {
    if (!seoCfg || savingSeo) return false;
    setSavingSeo(true);
    try {
      const res = await apiFetch<any>("/api/admin/settings/seo", {
        method: "PATCH",
        body: JSON.stringify({ expected_version: seoVer, ...seoCfg }),
      });
      setSeoCfg(res.value || seoCfg);
      setSeoVer(res.version);
      if (!silent) showToast(t("common.saved"));
      return true;
    } catch (err: any) {
      showToast(err.message);
      return false;
    } finally {
      setSavingSeo(false);
    }
  };

  const pullGitHubRepos = async () => {
    if (!seoCfg || syncingGh) return;
    setSyncingGh(true);
    try {
      const r = await apiFetch<{ repos: any[]; github_synced_at?: number }>("/api/admin/seo/github-sync", {
        method: "POST",
        body: JSON.stringify({
          username: seoCfg.github_username || "",
          include_forks: !!seoCfg.github_include_forks,
        }),
      });
      const fresh = await apiFetch<any>("/api/admin/settings/seo");
      setSeoCfg(fresh.value || { ...seoCfg, github_repos: r.repos || [] });
      setSeoVer(fresh.version ?? 0);
      showToast(t("dist.synced_gh", { count: r.repos?.length || 0 }));
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setSyncingGh(false);
    }
  };

  const publishGitHubShowcase = async () => {
    if (!seoCfg || publishingGh) return;
    setPublishingGh(true);
    try {
      const ok = await saveSeoSettings(true);
      if (!ok) return;
      await apiFetch("/api/admin/seo/github-publish", { method: "POST", body: "{}" });
      const repos: any[] = Array.isArray(seoCfg.github_repos) ? seoCfg.github_repos : [];
      const shownCount = repos.filter((rp) => !rp.hidden && (!rp.is_fork || seoCfg.github_include_forks)).length;
      showToast(t("dist.gh_published_toast", { count: shownCount }));
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setPublishingGh(false);
    }
  };

  const filteredNavRows = navSearch.trim()
    ? orderedNavRows.filter(({ item }) => {
        const q = navSearch.trim().toLowerCase();
        const i18nStr = Object.values(parseI18nMap(item.i18n_titles_json)).join(" ").toLowerCase();
        return (
          String(item.title || "").toLowerCase().includes(q) ||
          String(item.url || "").toLowerCase().includes(q) ||
          String(item.icon || "").toLowerCase().includes(q) ||
          i18nStr.includes(q)
        );
      })
    : orderedNavRows;

  const redirPageSlice =
    redirects.length > pageSize ? paginateItems(redirects, pageIdx, pageSize).slice : redirects;

  const effectiveLinkTotal = linkTotal;
  const linkPageSlice =
    links.length > pageSize ? paginateItems(links, pageIdx, pageSize).slice : links;

  const followPageSlice =
    follows.length > pageSize ? paginateItems(follows, pageIdx, pageSize).slice : follows;

  const filteredWebhooks = whSearch.trim()
    ? webhooks.filter((item) => {
        const w = item.webhook || item;
        const q = whSearch.trim().toLowerCase();
        return (
          String(w.name || "").toLowerCase().includes(q) ||
          String(w.url || w.target_url || "").toLowerCase().includes(q) ||
          String(w.provider || w.platform || "").toLowerCase().includes(q)
        );
      })
    : webhooks;

  const allGhRepos: any[] = Array.isArray(seoCfg?.github_repos) ? seoCfg.github_repos : [];
  const filteredGhRepos = ghSearch.trim()
    ? allGhRepos.filter((rp) => {
        const q = ghSearch.trim().toLowerCase();
        return (
          String(rp.name || "").toLowerCase().includes(q) ||
          String(rp.language || "").toLowerCase().includes(q) ||
          String(rp.description || "").toLowerCase().includes(q)
        );
      })
    : allGhRepos;

  const visibleGhRepos = allGhRepos.filter((rp) => !rp.hidden && (!rp.is_fork || seoCfg?.github_include_forks));
  const totalGhStars = visibleGhRepos.reduce((acc, rp) => acc + Number(rp.stars || 0), 0);
  const totalGhForks = visibleGhRepos.reduce((acc, rp) => acc + Number(rp.forks || 0), 0);

  const pagedNav = paginateItems(filteredNavRows, pageIdx, pageSize);
  const pagedGh = paginateItems(filteredGhRepos, pageIdx, pageSize);
  const pagedWebhooks = paginateItems(filteredWebhooks, pageIdx, pageSize);

  const toggleSel = (list: string[], setList: (v: string[]) => void, id: string) => {
    setList(list.includes(id) ? list.filter((x) => x !== id) : [...list, id]);
  };

  const onChangePageSize = (sz: number) => {
    setPageSize(sz);
    setPageIdx(1);
  };

  return (
    <div>
      {sub === "nav_redirects" ? (
        <div class="volo-subtabs">
          <button
            type="button"
            class={`volo-tab ${navSub === "nav" ? "active" : ""}`}
            onClick={() => switchNavSub("nav")}
          >
            {t("dist.nav")}
          </button>
          <button
            type="button"
            class={`volo-tab ${navSub === "redirects" ? "active" : ""}`}
            onClick={() => switchNavSub("redirects")}
          >
            {t("dist.redirects")}
          </button>
        </div>
      ) : (
        <div class="volo-subtabs">
          {distTabs.map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => switchDistSub(k)}
            >
              {distTabLabel(k)}
            </button>
          ))}
        </div>
      )}

      {/* 1A. Top Navigation */}
      {sub === "nav_redirects" && navSub === "nav" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.nav_head")}</h3>
              <p class="volo-sec-desc">{t("dist.nav_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.nav_search_ph")}
                value={navSearch}
                onInput={(e) => {
                  setNavSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              {isAdmin && (
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  onClick={() => onNavigate?.("themes")}
                >
                  {t("dist.nav_theme_order")}
                </button>
              )}
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = navItems.reduce((m, x) => Math.max(m, Number(x.sort_order || 0)), 0);
                  setNavForm({ ...EMPTY_NAV, i18n: {}, sort_order: maxSort + 10 });
                  setNavModalOpen(true);
                }}
              >
                + {t("dist.new_nav")}
              </button>
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("tbl.col_url")}</th>
                  <th class="col-fluid">{t("tbl.col_i18n")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && navItems.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : pagedNav.slice.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  pagedNav.slice.map(({ item: it, isChild, sibIdx, sibCount }) => {
                    const parsed = parseI18nMap(it.i18n_titles_json);
                    const i18nSummary = Object.entries(parsed)
                      .filter(([, v]) => Boolean(v))
                      .map(([k, v]) => `${k}: ${v}`)
                      .join(" · ");
                    const searchActive = Boolean(navSearch.trim());
                    return (
                      <tr key={it.id}>
                        <td class="col-fluid" data-label={t("cnt.col_name")}>
                          <div class="volo-cell-title" style={isChild ? { paddingLeft: "20px" } : undefined}>
                            {isChild && <span class="volo-help">↳</span>}
                            <NavIconPreview icon={it.icon} fallbackLabel={it.title} />
                            <span>{it.title}</span>
                            {it.icon && <span class="volo-code-chip">{it.icon}</span>}
                            {it.open_in_new_tab && <span class="volo-mini-tag">↗</span>}
                            {it.is_enabled === false && <span class="volo-mini-tag warn">{t("tbl.status_disabled")}</span>}
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("tbl.col_url")}>
                          <span class="volo-code-chip">{it.url}</span>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_i18n")}>
                          <span class="volo-help">{i18nSummary || "—"}</span>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={searchActive || sibIdx <= 0}
                              title={searchActive ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
                              onClick={() => void swapNavOrder(it.id, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={searchActive || sibIdx < 0 || sibIdx >= sibCount - 1}
                              title={searchActive ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
                              onClick={() => void swapNavOrder(it.id, 1)}
                            >
                              ↓
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setNavForm({
                                  id: it.id,
                                  parent_id: it.parent_id || "",
                                  title: it.title || "",
                                  url: it.url || "/",
                                  i18n: parsed,
                                  icon: it.icon || "",
                                  sort_order: it.sort_order ?? 10,
                                  open_in_new_tab: !!it.open_in_new_tab,
                                  is_enabled: it.is_enabled !== false,
                                });
                                setNavModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    const childCount = navItems.filter((c) => c.parent_id === it.id).length;
                                    const msg =
                                      childCount > 0
                                        ? t("dist.confirm_del_nav_parent", { n: childCount })
                                        : t("dist.confirm_del_nav");
                                    if (!(await openConfirm({ message: msg, danger: true }))) return;
                                    await apiFetch(`/api/admin/nav/${it.id}`, { method: "DELETE" });
                                    showToast(t("common.deleted"));
                                    void loadNav();
                                  },
                                },
                              ]}
                            />
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={filteredNavRows.length}
            page={pagedNav.safePage}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <AnimatedModal
            open={navModalOpen}
            onClose={() => setNavModalOpen(false)}
            title={navForm.id ? t("dist.edit_nav") : t("dist.new_nav")}
            subtitle={t("dist.nav_desc")}
            maxWidth="640px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setNavModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveNav()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveNav(e)} autoComplete="off">
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_nav_label")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.nav_label_ph")}
                    value={navForm.title}
                    onInput={(e) => setNavForm({ ...navForm, title: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.lbl_nav_url")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.nav_url_ph")}
                    value={navForm.url}
                    onInput={(e) => setNavForm({ ...navForm, url: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_nav_parent")}</label>
                <ConsoleSelect
                  value={navForm.parent_id || ""}
                  onChange={(val) => setNavForm({ ...navForm, parent_id: val })}
                  options={[
                    { value: "", label: t("dist.nav_parent_none") },
                    ...topLevelNav
                      .filter((x) => !x.parent_id && x.id !== navForm.id)
                      .map((x) => ({ value: x.id, label: x.title })),
                  ]}
                />
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_nav_i18n")}</label>
                <div class="volo-grid-2">
                  {localeOptions.map((loc) => (
                    <div key={loc.code} class="volo-field" style={{ marginBottom: 0 }}>
                      <label style={{ fontSize: "12px", color: "var(--volo-muted)" }}>
                        {loc.name} ({loc.code})
                      </label>
                      <input
                        class="volo-input"
                        placeholder={navForm.title || loc.name}
                        value={navForm.i18n[loc.code] || ""}
                        onInput={(e) =>
                          setNavForm({
                            ...navForm,
                            i18n: {
                              ...navForm.i18n,
                              [loc.code]: (e.currentTarget as HTMLInputElement).value,
                            },
                          })
                        }
                      />
                    </div>
                  ))}
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_nav_icon")}</label>
                <div class="volo-input-row">
                  <NavIconPreview icon={navForm.icon} fallbackLabel={navForm.title} size={18} />
                  <input
                    class="volo-input"
                    placeholder="github / book / /uploads/..."
                    value={navForm.icon}
                    onInput={(e) => setNavForm({ ...navForm, icon: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton filterKind="image" onPick={(u) => setNavForm({ ...navForm, icon: u })} />
                </div>
                <div class="volo-tag-pool">
                  <span class="volo-help">{t("dist.nav_icon_presets")}</span>
                  {NAV_ICON_PRESETS.map((pr) => (
                    <button
                      key={pr}
                      type="button"
                      class={`volo-chip-btn ${navForm.icon === pr ? "active" : ""}`}
                      onClick={() => setNavForm({ ...navForm, icon: navForm.icon === pr ? "" : pr })}
                    >
                      {pr}
                    </button>
                  ))}
                </div>
              </div>

              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={navForm.sort_order}
                    onInput={(e) => setNavForm({ ...navForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
                <div class="volo-check-grid">
                  <SwitchToggle
                    checked={navForm.open_in_new_tab}
                    onChange={(v) => setNavForm({ ...navForm, open_in_new_tab: v })}
                    label={t("dist.nav_new_tab")}
                  />
                  <SwitchToggle
                    checked={navForm.is_enabled !== false}
                    onChange={(v) => setNavForm({ ...navForm, is_enabled: v })}
                    label={t("tbl.status_active")}
                  />
                </div>
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 1B. Redirects */}
      {sub === "nav_redirects" && navSub === "redirects" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.redir_head")}</h3>
              <p class="volo-sec-desc">{t("dist.redir_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <a class="volo-btn volo-btn-sm" href="/api/admin/redirects/export?format=csv" download>
                {t("dist.redir_export_csv")}
              </a>
              <a class="volo-btn volo-btn-sm" href="/api/admin/redirects/export?format=json" download>
                {t("dist.redir_export_json")}
              </a>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  setRedirImportFormat("csv");
                  setRedirImportData("");
                  setRedirImportOpen(true);
                }}
              >
                {t("dist.redir_import")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  setRedirForm({ ...EMPTY_REDIR });
                  setRedirModalOpen(true);
                }}
              >
                + {t("dist.new_redir")}
              </button>
            </div>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-controls">
              <ConsoleSelect
                searchable={false}
                value={redirCodeFilter}
                onChange={(val) => {
                  setRedirCodeFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("dist.redir_all_codes") },
                  { value: "301", label: t("dist.redir_301") },
                  { value: "302", label: t("dist.redir_302") },
                ]}
              />
              <ConsoleSelect
                searchable={false}
                value={redirSourceFilter}
                onChange={(val) => {
                  setRedirSourceFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("dist.redir_all_sources") },
                  { value: "manual", label: t("dist.redir_manual") },
                  { value: "legacy", label: t("dist.redir_legacy") },
                ]}
              />
              <ConsoleSelect
                searchable={false}
                value={redirSort}
                onChange={(val) => {
                  setRedirSort(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "newest", label: t("dist.redir_sort_newest") },
                  { value: "hits", label: t("dist.redir_sort_hits") },
                ]}
              />
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.redir_search_ph")}
                value={redirSearch}
                onInput={(e) => {
                  setRedirSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
            </div>
          </div>

          <MobileSelectAllBar
            checked={redirPageSlice.length > 0 && redirPageSlice.every((r) => selRedirIds.includes(r.id))}
            count={selRedirIds.length}
            total={redirPageSlice.length}
            onToggle={(on) => {
              const pageIds = redirPageSlice.map((r) => r.id);
              setSelRedirIds(
                on
                  ? Array.from(new Set([...selRedirIds, ...pageIds]))
                  : selRedirIds.filter((id) => !pageIds.includes(id))
              );
            }}
          />

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-check">
                    <input
                      type="checkbox"
                      aria-label={t("ov.select_all")}
                      checked={redirPageSlice.length > 0 && redirPageSlice.every((r) => selRedirIds.includes(r.id))}
                      onChange={(e) => {
                        const on = (e.currentTarget as HTMLInputElement).checked;
                        const pageIds = redirPageSlice.map((r) => r.id);
                        setSelRedirIds(
                          on
                            ? Array.from(new Set([...selRedirIds, ...pageIds]))
                            : selRedirIds.filter((id) => !pageIds.includes(id))
                        );
                      }}
                    />
                  </th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-fluid">{t("tbl.col_source")}</th>
                  <th class="col-fluid">{t("tbl.col_target")}</th>
                  <th class="col-num">{t("tbl.col_hits")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && redirects.length === 0 ? (
                  <TableSkeleton cols={6} />
                ) : redirPageSlice.length === 0 ? (
                  <ConsoleEmpty colSpan={6} />
                ) : (
                  redirPageSlice.map((r) => {
                    const testHref = String(r.source_path || "").startsWith("/")
                      ? r.source_path
                      : `/${r.source_path || ""}`;
                    return (
                      <tr key={r.id}>
                        <td class="col-check">
                          <input
                            type="checkbox"
                            checked={selRedirIds.includes(r.id)}
                            onChange={() => toggleSel(selRedirIds, setSelRedirIds, r.id)}
                          />
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <div class="volo-cell-meta">
                            <span class={`volo-status-pill ${r.status_code === 301 ? "ok" : "warn"}`}>
                              <span class="volo-status-dot" />
                              {r.status_code}
                            </span>
                            {r.is_system_legacy && <span class="volo-mini-tag">{t("dist.redir_legacy")}</span>}
                          </div>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_source")} style={{ minWidth: 0 }}>
                          <a
                            href={testHref}
                            target="_blank"
                            rel="noreferrer"
                            class="volo-code-chip is-wrap"
                          >
                            {r.source_path} ↗
                          </a>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_target")} style={{ minWidth: 0 }}>
                          <span class="volo-code-chip is-wrap">{r.target_path}</span>
                        </td>
                        <td class="col-num" data-label={t("tbl.col_hits")}>
                          <span class="volo-cat-chip">{t("dist.redir_hits", { count: r.hit_count ?? 0 })}</span>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setRedirForm({
                                  id: r.id,
                                  source_path: r.source_path || "",
                                  target_path: r.target_path || "",
                                  status_code: r.status_code || 301,
                                });
                                setRedirModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                if (!(await openConfirm({ message: t("dist.confirm_del_redir"), danger: true }))) return;
                                await apiFetch(`/api/admin/redirects/${r.id}`, { method: "DELETE" });
                                showToast(t("common.deleted"));
                                void loadRedirects();
                              }}
                            >
                              {t("common.delete")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>

          <BatchBar count={selRedirIds.length} onClear={() => setSelRedirIds([])}>
            <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteRedirects()}>
              {t("common.delete")}
            </button>
          </BatchBar>

          <Pagination
            total={redirTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <AnimatedModal
            open={redirModalOpen}
            onClose={() => setRedirModalOpen(false)}
            title={redirForm.id ? t("dist.edit_redir") : t("dist.new_redir")}
            subtitle={t("dist.redir_desc")}
            maxWidth="560px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setRedirModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveRedirect()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveRedirect(e)} autoComplete="off">
              <div class="volo-field">
                <label>{t("dist.lbl_redir_src")}</label>
                <input
                  class="volo-input"
                  required
                  placeholder={t("dist.redir_src_ph")}
                  value={redirForm.source_path}
                  onInput={(e) => setRedirForm({ ...redirForm, source_path: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-field">
                <label>{t("dist.lbl_redir_dst")}</label>
                <input
                  class="volo-input"
                  required
                  placeholder={t("dist.redir_dst_ph")}
                  value={redirForm.target_path}
                  onInput={(e) => setRedirForm({ ...redirForm, target_path: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-field">
                <label>{t("dist.lbl_redir_code")}</label>
                <ConsoleSelect
                  searchable={false}
                  value={String(redirForm.status_code)}
                  onChange={(val) => setRedirForm({ ...redirForm, status_code: Number(val) })}
                  options={[
                    { value: "301", label: t("dist.redir_301") },
                    { value: "302", label: t("dist.redir_302") },
                  ]}
                />
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={redirImportOpen}
            onClose={() => setRedirImportOpen(false)}
            title={t("dist.redir_import")}
            subtitle={t("dist.redir_desc")}
            maxWidth="580px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setRedirImportOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={importingRedir || !redirImportData.trim()}
                  onClick={() => void submitImportRedirects()}
                >
                  {t("dist.redir_import")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void submitImportRedirects(e)} autoComplete="off">
              <div class="volo-input-row" style={{ marginBottom: "12px", flexWrap: "wrap" }}>
                <div style={{ flex: "1 1 160px" }}>
                  <ConsoleSelect
                    searchable={false}
                    value={redirImportFormat}
                    onChange={(v) => setRedirImportFormat((v as "csv" | "json") || "csv")}
                    options={[
                      { value: "csv", label: "CSV" },
                      { value: "json", label: "JSON" },
                    ]}
                  />
                </div>
                <UploadButton
                  accept=".csv,.json,.txt"
                  label={t("dist.redir_upload_file")}
                  onFile={async (f) => {
                    const txt = await f.text();
                    if (/\.json$/i.test(f.name) || txt.trim().startsWith("[")) {
                      setRedirImportFormat("json");
                    } else {
                      setRedirImportFormat("csv");
                    }
                    setRedirImportData(txt);
                  }}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <textarea
                  class="volo-textarea"
                  rows={7}
                  style={{ fontFamily: "var(--volo-mono)", fontSize: "12.5px" }}
                  placeholder={
                    redirImportFormat === "json"
                      ? '[{"source_path":"/old","target_path":"/posts/new","status_code":301}]'
                      : "/old-path,/posts/new-slug,301"
                  }
                  value={redirImportData}
                  onInput={(e) => setRedirImportData((e.currentTarget as HTMLTextAreaElement).value)}
                />
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 2. GitHub Showcase */}
      {sub === "github" && isAdmin && seoCfg && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.gh_head")}</h3>
              <p class="volo-sec-desc">{t("dist.gh_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                disabled={savingSeo || publishingGh}
                onClick={() => void saveSeoSettings(false)}
              >
                {t("dist.gh_save_sel")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-sm volo-btn-primary"
                disabled={publishingGh || syncingGh}
                onClick={() => void publishGitHubShowcase()}
              >
                {t("dist.gh_publish_btn")}
              </button>
            </div>
          </div>

          <form
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              void pullGitHubRepos();
            }}
            class="volo-form-bar"
          >
            <div style={{ display: "flex", flexWrap: "wrap", alignItems: "flex-end", gap: "12px" }}>
              <div class="volo-field" style={{ flex: "1 1 220px", marginBottom: 0 }}>
                <label>{t("dist.seo_gh_user")}</label>
                <input
                  class="volo-input"
                  placeholder="octocat"
                  value={seoCfg.github_username || ""}
                  onInput={(e) => setSeoCfg({ ...seoCfg, github_username: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div style={{ paddingBottom: "4px" }}>
                <SwitchToggle
                  checked={!!seoCfg.github_include_forks}
                  onChange={(v) => setSeoCfg({ ...seoCfg, github_include_forks: v })}
                  label={t("dist.seo_gh_forks")}
                />
              </div>
              <div class="volo-filter-controls" style={{ marginLeft: "auto" }}>
                {seoCfg.github_username && (
                  <a class="volo-btn" href="/posts/github" target="_blank" rel="noreferrer">
                    {t("common.preview")}
                  </a>
                )}
                <button type="submit" class="volo-btn volo-btn-primary" disabled={syncingGh}>
                  {t("dist.sync_gh")}
                </button>
              </div>
            </div>
          </form>

          {allGhRepos.length > 0 && (
            <div class="volo-subcard">
              <div class="volo-sec-head">
                <div>
                  <div class="volo-cell-title">
                    {t("dist.gh_stats_bar", {
                      shown: visibleGhRepos.length,
                      total: allGhRepos.length,
                      stars: totalGhStars,
                      forks: totalGhForks,
                    })}
                  </div>
                  {seoCfg.github_synced_at > 0 && (
                    <div class="volo-help">
                      {t("dist.follows_last_sync", { time: formatDateTime(seoCfg.github_synced_at) })}
                    </div>
                  )}
                </div>
                {seoCfg.github_username && (
                  <img
                    src={`/github-stats/${encodeURIComponent(seoCfg.github_username)}.svg?t=${seoVer}&theme=${effectiveDark ? "dark" : "light"}`}
                    alt={seoCfg.github_username}
                    style={{
                      maxHeight: "74px",
                      maxWidth: "100%",
                      height: "auto",
                      borderRadius: "8px",
                      border: "1px solid var(--volo-line)",
                    }}
                  />
                )}
              </div>
            </div>
          )}

          <div class="volo-filter-bar">
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.gh_search_ph")}
                value={ghSearch}
                onInput={(e) => {
                  setGhSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
            </div>
            <div class="volo-filter-controls">
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  const filteredNames = new Set(filteredGhRepos.map((r) => r.full_name || r.name));
                  const next = allGhRepos.map((r) =>
                    filteredNames.has(r.full_name || r.name) ? { ...r, hidden: false } : r
                  );
                  setSeoCfg({ ...seoCfg, github_repos: next });
                }}
              >
                {t("dist.gh_select_all")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  const filteredNames = new Set(filteredGhRepos.map((r) => r.full_name || r.name));
                  const next = allGhRepos.map((r) =>
                    filteredNames.has(r.full_name || r.name) ? { ...r, hidden: true } : r
                  );
                  setSeoCfg({ ...seoCfg, github_repos: next });
                }}
              >
                {t("dist.gh_clear_all")}
              </button>
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-status">{t("dist.gh_col_show")}</th>
                  <th class="col-fluid">{t("dist.gh_col_repo")}</th>
                  <th class="col-cat">{t("dist.gh_col_lang")}</th>
                  <th class="col-num">{t("dist.gh_col_metrics")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && allGhRepos.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : pagedGh.slice.length === 0 ? (
                  <ConsoleEmpty text={t("dist.gh_empty")} colSpan={4} />
                ) : (
                  pagedGh.slice.map((rp) => {
                    const key = rp.full_name || rp.name;
                    const shown = !rp.hidden;
                    return (
                      <tr key={key}>
                        <td class="col-status" data-label={t("dist.gh_col_show")}>
                          <SwitchToggle
                            checked={shown}
                            onChange={(on) => {
                              const next = allGhRepos.map((item) =>
                                (item.full_name || item.name) === key ? { ...item, hidden: !on } : item
                              );
                              setSeoCfg({ ...seoCfg, github_repos: next });
                            }}
                          />
                        </td>
                        <td class="col-fluid" data-label={t("dist.gh_col_repo")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <a href={rp.html_url || `https://github.com/${rp.full_name}`} target="_blank" rel="noreferrer">
                                {rp.name}
                              </a>
                              {rp.is_fork && <span class="volo-mini-tag">{t("dist.gh_fork_badge")}</span>}
                            </div>
                            {rp.description && (
                              <div class="volo-cell-meta">
                                <span>{rp.description}</span>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("dist.gh_col_lang")}>
                          <span class={`volo-cat-chip ${rp.language ? "" : "muted"}`}>{rp.language || "—"}</span>
                        </td>
                        <td class="col-num" data-label={t("dist.gh_col_metrics")}>
                          <div class="volo-metric-pair">
                            <span class="volo-metric-item">★ {rp.stars ?? 0}</span>
                            <span class="volo-metric-item">⑂ {rp.forks ?? 0}</span>
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={filteredGhRepos.length}
            page={pagedGh.safePage}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <div class="volo-form-actions">
            <button
              type="button"
              class="volo-btn"
              disabled={savingSeo || publishingGh}
              onClick={() => void saveSeoSettings(false)}
            >
              {t("dist.gh_save_sel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              disabled={publishingGh || syncingGh}
              onClick={() => void publishGitHubShowcase()}
            >
              {t("dist.gh_publish_btn")}
            </button>
          </div>
        </div>
      )}

      {/* 3. Links */}
      {sub === "links" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.links")}</h3>
              <p class="volo-sec-desc">{t("dist.links_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                disabled={patrolling}
                onClick={async () => {
                  if (patrolling) return;
                  setPatrolling(true);
                  try {
                    const r = await apiFetch<{ checked: number; items?: any[] }>("/api/admin/links/patrol", {
                      method: "POST",
                      body: "{}",
                    });
                    const list = r.items || [];
                    const healthy = list.filter((x) => x.health_status === "ok" || x.health_status === "redirect").length;
                    showToast(t("dist.links_patrolled", { checked: r.checked ?? list.length, healthy }));
                    await loadLinks();
                  } catch (err: any) {
                    showToast(err.message);
                  } finally {
                    setPatrolling(false);
                  }
                }}
              >
                {t("dist.links_patrol")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = links.reduce((m, x) => Math.max(m, Number(x.sort_order || 0)), 0);
                  setLinkForm({ ...EMPTY_LINK, sort_order: maxSort + 10 });
                  setLinkModalOpen(true);
                }}
              >
                + {t("dist.new_link")}
              </button>
            </div>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-controls">
              <ConsoleSelect
                searchable={false}
                value={linkHealthFilter}
                onChange={(val) => {
                  setLinkHealthFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("dist.links_health_all") },
                  { value: "ok", label: t("dist.links_ok") },
                  { value: "redirect", label: t("dist.links_redirect") },
                  { value: "broken", label: t("dist.links_unreachable") },
                  { value: "unchecked", label: t("dist.links_unknown") },
                ]}
              />
              {knownLinkCategories.length > 0 && (
                <ConsoleSelect
                  value={linkCatFilter}
                  onChange={(val) => {
                    setLinkCatFilter(val);
                    setPageIdx(1);
                  }}
                  options={[
                    { value: "", label: t("art.all_categories") },
                    ...knownLinkCategories.map((cat) => ({ value: cat, label: cat })),
                  ]}
                />
              )}
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.links_search_ph")}
                value={linkSearch}
                onInput={(e) => {
                  setLinkSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
            </div>
          </div>

          <MobileSelectAllBar
            checked={linkPageSlice.length > 0 && linkPageSlice.every((l) => selLinkIds.includes(l.id))}
            count={selLinkIds.length}
            total={linkPageSlice.length}
            onToggle={(on) => {
              const pageIds = linkPageSlice.map((l) => l.id);
              setSelLinkIds(
                on
                  ? Array.from(new Set([...selLinkIds, ...pageIds]))
                  : selLinkIds.filter((id) => !pageIds.includes(id))
              );
            }}
          />

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-check">
                    <input
                      type="checkbox"
                      aria-label={t("ov.select_all")}
                      checked={linkPageSlice.length > 0 && linkPageSlice.every((l) => selLinkIds.includes(l.id))}
                      onChange={(e) => {
                        const on = (e.currentTarget as HTMLInputElement).checked;
                        const pageIds = linkPageSlice.map((l) => l.id);
                        setSelLinkIds(
                          on
                            ? Array.from(new Set([...selLinkIds, ...pageIds]))
                            : selLinkIds.filter((id) => !pageIds.includes(id))
                        );
                      }}
                    />
                  </th>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-fluid">{t("tbl.col_url")}</th>
                  <th class="col-status">{t("tbl.col_health")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && links.length === 0 ? (
                  <TableSkeleton cols={5} />
                ) : linkPageSlice.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
                ) : (
                  linkPageSlice.map((l) => {
                    const hs = l.health_status || "unknown";
                    const pillCls = hs === "ok" ? "ok" : hs === "redirect" ? "warn" : hs === "unknown" ? "" : "danger";
                    const statusText =
                      hs === "ok"
                        ? t("dist.links_ok")
                        : hs === "redirect"
                        ? t("dist.links_redirect")
                        : hs === "unknown"
                        ? t("dist.links_unknown")
                        : t("dist.links_unreachable");
                    const label = l.title || l.name || "";
                    const errText = String(l.last_error || l.health_error || "").trim();
                    return (
                      <tr key={l.id}>
                        <td class="col-check">
                          <input
                            type="checkbox"
                            checked={selLinkIds.includes(l.id)}
                            onChange={() => toggleSel(selLinkIds, setSelLinkIds, l.id)}
                          />
                        </td>
                        <td class="col-fluid" data-label={t("cnt.col_name")} style={{ minWidth: 0 }}>
                          <div class="volo-cell-main" style={{ minWidth: 0 }}>
                            <div class="volo-cell-title" style={{ minWidth: 0 }}>
                              <NavIconPreview icon={l.icon_url} fallbackLabel={label} />
                              <span class="volo-wrap-anywhere">{label}</span>
                              {l.category_name && <span class="volo-mini-tag">{l.category_name}</span>}
                            </div>
                            {l.description && (
                              <div class="volo-cell-meta" style={{ minWidth: 0 }}>
                                <span class="volo-wrap-anywhere">{l.description}</span>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_url")} style={{ minWidth: 0 }}>
                          <a href={l.url} target="_blank" rel="noreferrer" class="volo-code-chip is-wrap">
                            {l.url}
                          </a>
                        </td>
                        <td class="col-status" data-label={t("tbl.col_health")} style={{ minWidth: 0 }}>
                          <div class="volo-cell-main" style={{ minWidth: 0 }}>
                            <div class="volo-cell-meta">
                              <span class={`volo-status-pill ${pillCls}`}>
                                <span class="volo-status-dot" />
                                {statusText}
                              </span>
                              {l.http_code > 0 && <span class="volo-mini-tag">HTTP {l.http_code}</span>}
                              {l.latency_ms > 0 && <span class="volo-help">{l.latency_ms}ms</span>}
                            </div>
                            {errText && (
                              <span
                                class="volo-help volo-wrap-anywhere"
                                title={errText}
                                style={{ color: "var(--volo-danger)", fontSize: "11.5px", minWidth: 0 }}
                              >
                                {errText.length > 42 ? `${errText.slice(0, 42)}…` : errText}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={checkingLinkId === l.id}
                              onClick={() => void checkSingleLink(l.id)}
                            >
                              {t("dist.links_check_one")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setLinkForm({
                                  id: l.id,
                                  title: l.title || l.name || "",
                                  url: l.url || "",
                                  icon_url: l.icon_url || "",
                                  description: l.description || "",
                                  category_name: l.category_name || "",
                                  sort_order: l.sort_order ?? 10,
                                });
                                setLinkModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("common.view"),
                                  href: l.url,
                                  target: "_blank",
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("dist.confirm_del_link"), danger: true }))) return;
                                    await apiFetch(`/api/admin/links/${l.id}`, { method: "DELETE" });
                                    showToast(t("common.deleted"));
                                    void loadLinks();
                                  },
                                },
                              ]}
                            />
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>

          <BatchBar count={selLinkIds.length} onClear={() => setSelLinkIds([])}>
            <button
              type="button"
              class="volo-act-btn"
              onClick={() => {
                setBatchCatName("");
                setBatchCatOpen(true);
              }}
            >
              {t("dist.links_batch_cat")}
            </button>
            <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteLinks()}>
              {t("common.delete")}
            </button>
          </BatchBar>

          <Pagination
            total={effectiveLinkTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <AnimatedModal
            open={batchCatOpen}
            onClose={() => setBatchCatOpen(false)}
            title={t("dist.links_batch_cat")}
            maxWidth="440px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setBatchCatOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={movingLinksCat}
                  onClick={() => void submitBatchMoveLinkCat()}
                >
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void submitBatchMoveLinkCat(e)} autoComplete="off">
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("dist.lbl_link_cat")}</label>
                <input
                  class="volo-input"
                  placeholder={t("dist.links_cat_ph")}
                  value={batchCatName}
                  onInput={(e) => setBatchCatName((e.currentTarget as HTMLInputElement).value)}
                />
                {knownLinkCategories.length > 0 && (
                  <div class="volo-tag-pool" style={{ marginTop: "8px" }}>
                    {knownLinkCategories.map((c) => (
                      <button
                        key={c}
                        type="button"
                        class={`volo-chip-btn ${batchCatName === c ? "active" : ""}`}
                        onClick={() => setBatchCatName(c)}
                      >
                        {c}
                      </button>
                    ))}
                  </div>
                )}
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={linkModalOpen}
            onClose={() => setLinkModalOpen(false)}
            title={linkForm.id ? t("dist.edit_link") : t("dist.new_link")}
            subtitle={t("dist.links_desc")}
            maxWidth="620px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setLinkModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveLink()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveLink(e)} autoComplete="off">
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_link_name")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.links_name_ph")}
                    value={linkForm.title}
                    onInput={(e) => setLinkForm({ ...linkForm, title: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.lbl_link_url")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder="https://example.com"
                    value={linkForm.url}
                    onInput={(e) => setLinkForm({ ...linkForm, url: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              </div>

              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_link_cat")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("dist.links_cat_ph")}
                    value={linkForm.category_name}
                    onInput={(e) => setLinkForm({ ...linkForm, category_name: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={linkForm.sort_order ?? 10}
                    onInput={(e) => setLinkForm({ ...linkForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_link_icon")}</label>
                <div class="volo-input-row">
                  <NavIconPreview icon={linkForm.icon_url} fallbackLabel={linkForm.title} size={18} />
                  <input
                    class="volo-input"
                    placeholder={t("dist.links_icon_ph")}
                    value={linkForm.icon_url}
                    onInput={(e) => setLinkForm({ ...linkForm, icon_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton filterKind="image" onPick={(u) => setLinkForm({ ...linkForm, icon_url: u })} />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("cnt.lbl_desc")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  placeholder={t("cnt.desc_ph")}
                  value={linkForm.description}
                  onInput={(e) => setLinkForm({ ...linkForm, description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 4. Follows */}
      {sub === "follows" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.follows")}</h3>
              <p class="volo-sec-desc">{t("dist.follows_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <a class="volo-btn volo-btn-sm" href="/api/admin/follows/export.opml" download>
                {t("dist.follows_export_opml")}
              </a>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  setOpmlData("");
                  setOpmlImportOpen(true);
                }}
              >
                {t("dist.follows_import_opml")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void openFollowPreview()}>
                {t("dist.follows_preview")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                disabled={syncingFollows}
                onClick={async () => {
                  if (syncingFollows) return;
                  setSyncingFollows(true);
                  try {
                    const r = await apiFetch<{ synced_items: number }>("/api/admin/follows/sync", { method: "POST", body: "{}" });
                    showToast(t("dist.follows_synced", { count: r.synced_items }));
                    await loadFollows();
                  } catch (err: any) {
                    showToast(err.message);
                  } finally {
                    setSyncingFollows(false);
                  }
                }}
              >
                {t("dist.follows_sync")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = follows.reduce((m, x) => Math.max(m, Number(x.sort_order || 0)), 0);
                  setFollowForm({ ...EMPTY_FOLLOW, sort_order: maxSort + 10 });
                  setFollowModalOpen(true);
                }}
              >
                + {t("dist.new_follow")}
              </button>
            </div>
          </div>

          <div class="volo-filter-bar">
            <div class="volo-filter-controls">
              <ConsoleSelect
                searchable={false}
                value={followStatusFilter}
                onChange={(val) => {
                  setFollowStatusFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("dist.follows_status_all") },
                  { value: "ok", label: t("dist.follows_status_ok") },
                  { value: "error", label: t("dist.follows_status_err") },
                ]}
              />
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.follows_search_ph")}
                value={followSearch}
                onInput={(e) => {
                  setFollowSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-fluid">{t("tbl.col_feed")}</th>
                  <th class="col-date">{t("tbl.col_sync")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && follows.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : followPageSlice.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  followPageSlice.map((f) => {
                    const label = f.title || f.name || "";
                    return (
                      <tr key={f.id}>
                        <td class="col-fluid" data-label={t("cnt.col_name")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <NavIconPreview icon={f.icon_url} fallbackLabel={label} />
                              <span>{label}</span>
                            </div>
                            <div class="volo-cell-meta">
                              {f.site_url && <span style={{ overflowWrap: "anywhere" }}>{f.site_url}</span>}
                              {f.description && <span>{f.description}</span>}
                            </div>
                          </div>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_feed")}>
                          <span class="volo-code-chip is-wrap" style={{ overflowWrap: "anywhere" }}>{f.feed_url}</span>
                        </td>
                        <td class="col-date" data-label={t("tbl.col_sync")}>
                          <div class="volo-cell-main">
                            <span>{formatDateTime(f.last_synced_at)}</span>
                            {f.sync_error && (
                              <span
                                class="volo-status-pill danger"
                                title={f.sync_error}
                                style={{
                                  maxWidth: "min(240px, 100%)",
                                  overflow: "hidden",
                                  textOverflow: "ellipsis",
                                  whiteSpace: "nowrap",
                                }}
                              >
                                <span class="volo-status-dot" />
                                {t("dist.follows_err", { err: f.sync_error })}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={syncingFollowId === f.id}
                              onClick={() => void syncSingleFollow(f.id)}
                            >
                              {t("dist.follows_sync_one")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setFollowForm({
                                  id: f.id,
                                  title: f.title || f.name || "",
                                  site_url: f.site_url || "",
                                  feed_url: f.feed_url || "",
                                  icon_url: f.icon_url || "",
                                  description: f.description || "",
                                  sort_order: f.sort_order ?? 10,
                                });
                                setFollowModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                if (!(await openConfirm({ message: t("dist.confirm_del_follow"), danger: true }))) return;
                                await apiFetch(`/api/admin/follows/${f.id}`, { method: "DELETE" });
                                showToast(t("common.deleted"));
                                void loadFollows();
                              }}
                            >
                              {t("common.delete")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={followTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <AnimatedModal
            open={opmlImportOpen}
            onClose={() => setOpmlImportOpen(false)}
            title={t("dist.follows_import_opml")}
            subtitle={t("dist.follows_desc")}
            maxWidth="580px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setOpmlImportOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={importingOpml || !opmlData.trim()}
                  onClick={() => void submitImportOpml()}
                >
                  {t("dist.follows_import_opml")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void submitImportOpml(e)} autoComplete="off">
              <div class="volo-input-row" style={{ marginBottom: "12px" }}>
                <UploadButton
                  accept=".opml,.xml,.txt"
                  label={t("dist.redir_upload_file")}
                  onFile={async (f) => {
                    setOpmlData(await f.text());
                  }}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <textarea
                  class="volo-textarea"
                  rows={7}
                  style={{ fontFamily: "var(--volo-mono)", fontSize: "12.5px" }}
                  placeholder='<?xml version="1.0" encoding="UTF-8"?><opml version="2.0">...'
                  value={opmlData}
                  onInput={(e) => setOpmlData((e.currentTarget as HTMLTextAreaElement).value)}
                />
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={followModalOpen}
            onClose={() => setFollowModalOpen(false)}
            title={followForm.id ? t("dist.edit_follow") : t("dist.new_follow")}
            subtitle={t("dist.follows_desc")}
            maxWidth="620px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setFollowModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveFollow()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveFollow(e)} autoComplete="off">
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_follow_name")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.follows_name_ph")}
                    value={followForm.title}
                    onInput={(e) => setFollowForm({ ...followForm, title: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.lbl_follow_feed")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.follows_feed_ph")}
                    value={followForm.feed_url}
                    onInput={(e) => setFollowForm({ ...followForm, feed_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
              </div>

              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_follow_site")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("dist.follows_site_ph")}
                    value={followForm.site_url}
                    onInput={(e) => setFollowForm({ ...followForm, site_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={followForm.sort_order ?? 10}
                    onInput={(e) => setFollowForm({ ...followForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_follow_icon")}</label>
                <div class="volo-input-row">
                  <NavIconPreview icon={followForm.icon_url} fallbackLabel={followForm.title} size={18} />
                  <input
                    class="volo-input"
                    placeholder="https://example.com/avatar.png"
                    value={followForm.icon_url}
                    onInput={(e) => setFollowForm({ ...followForm, icon_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton filterKind="image" onPick={(u) => setFollowForm({ ...followForm, icon_url: u })} />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_follow_desc")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  placeholder={t("cnt.desc_ph")}
                  value={followForm.description}
                  onInput={(e) => setFollowForm({ ...followForm, description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={followPreviewOpen}
            onClose={() => setFollowPreviewOpen(false)}
            title={t("dist.follows_preview")}
            subtitle={t("dist.follows_desc")}
            maxWidth="720px"
            footer={
              <button type="button" class="volo-btn volo-btn-primary" onClick={() => setFollowPreviewOpen(false)}>
                {t("common.close")}
              </button>
            }
          >
            {followPreviewLoading ? (
              <div class="volo-help">{t("shell.loading")}</div>
            ) : followGroups.length === 0 || followGroups.every((g) => !(g.items || []).length && !g.url) ? (
              <ConsoleEmpty />
            ) : (
              <div>
                {followGroups.map((g, idx) => {
                  const fl = g.follow || g;
                  const entries: any[] = Array.isArray(g.items) ? g.items : g.url ? [g] : [];
                  if (!entries.length) return null;
                  return (
                    <div key={fl.id || idx} class="volo-subcard">
                      <div class="volo-subcard-title">
                        <span>{fl.title || fl.feed_url}</span>
                        <span class="volo-mini-tag">{entries.length}</span>
                      </div>
                      {entries.slice(0, 8).map((it: any) => (
                        <div key={it.id || it.url} class="volo-cell-main" style={{ padding: "6px 0", borderTop: "1px dashed var(--volo-line)" }}>
                          <div class="volo-cell-title">
                            <a href={it.url} target="_blank" rel="noreferrer">
                              {it.title || it.url}
                            </a>
                            {it.published_at > 0 && <span class="volo-help">{formatDateTime(it.published_at)}</span>}
                          </div>
                          {it.summary && <div class="volo-help">{it.summary}</div>}
                        </div>
                      ))}
                    </div>
                  );
                })}
              </div>
            )}
          </AnimatedModal>
        </div>
      )}

      {/* 5. Webhooks */}
      {sub === "webhooks" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.wh_head")}</h3>
              <p class="volo-sec-desc">{t("dist.wh_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                autoComplete="off"
                placeholder={t("dist.wh_search_ph")}
                value={whSearch}
                onInput={(e) => {
                  setWhSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  setWhForm({ ...EMPTY_WH, events: ["article.published", "comment.created"] });
                  setWhModalOpen(true);
                }}
              >
                + {t("dist.new_wh")}
              </button>
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("tbl.col_platform")}</th>
                  <th class="col-fluid">{t("tbl.col_url")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && webhooks.length === 0 ? (
                  <TableSkeleton cols={5} />
                ) : pagedWebhooks.slice.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
                ) : (
                  pagedWebhooks.slice.map((item) => {
                    const w = item.webhook || item;
                    const prov = w.provider || w.platform || "custom";
                    const evList = parseJsonArr(w.events_json || w.events);
                    return (
                      <tr key={w.id}>
                        <td class="col-fluid" data-label={t("cnt.col_name")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">{w.name}</div>
                            {evList.length > 0 && (
                              <div class="volo-cell-meta">
                                {evList.map((ev) => (
                                  <span key={ev} class="volo-mini-tag">
                                    {whEventText(ev)}
                                  </span>
                                ))}
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("tbl.col_platform")}>
                          <span class="volo-cat-chip">{whProviderLabel(prov)}</span>
                        </td>
                        <td class="col-fluid" data-label={t("tbl.col_url")}>
                          <div class="volo-cell-main">
                            <span class="volo-code-chip is-wrap" style={{ overflowWrap: "anywhere" }}>{w.url || w.target_url}</span>
                            {w.last_triggered_at > 0 && (
                              <span class="volo-help">{formatDateTime(w.last_triggered_at)}</span>
                            )}
                          </div>
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <div class="volo-cell-meta">
                            <SwitchToggle
                              checked={w.is_enabled !== false}
                              onChange={(next) => void toggleWebhookEnabled(w, evList, next)}
                              label={w.is_enabled !== false ? t("tbl.status_active") : t("tbl.status_disabled")}
                            />
                            {item.secret_set && <span class="volo-mini-tag">{t("dist.wh_sealed")}</span>}
                            {w.last_status_code > 0 && (
                              <span class={`volo-mini-tag ${w.last_status_code < 400 ? "" : "warn"}`}>
                                HTTP {w.last_status_code}
                              </span>
                            )}
                            {w.last_error && (
                              <span class="volo-mini-tag warn" title={w.last_error}>
                                {t("dist.wh_last_err")}: {String(w.last_error).slice(0, 32)}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => void openWebhookDeliveries(w)}
                            >
                              {t("dist.wh_deliveries")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={testingWhId === w.id}
                              onClick={async () => {
                                if (testingWhId) return;
                                setTestingWhId(w.id);
                                try {
                                  const r = await apiFetch<{ status_code: number }>(`/api/admin/webhooks/${w.id}/test`, {
                                    method: "POST",
                                    body: "{}",
                                  });
                                  showToast(t("dist.wh_tested", { code: r.status_code }));
                                } catch (err: any) {
                                  showToast(err.message);
                                } finally {
                                  setTestingWhId("");
                                  void loadSub();
                                }
                              }}
                            >
                              {t("dist.wh_test")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setWhForm({
                                  id: w.id,
                                  name: w.name || "",
                                  provider: prov,
                                  url: w.url || "",
                                  chat_id: w.chat_id || "",
                                  secret: "",
                                  secret_set: !!item.secret_set,
                                  clear_secret: false,
                                  events: evList.length > 0 ? evList : ["article.published", "comment.created"],
                                  is_enabled: w.is_enabled !== false,
                                });
                                setWhModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("dist.confirm_del_wh"), danger: true }))) return;
                                    await apiFetch(`/api/admin/webhooks/${w.id}`, { method: "DELETE" });
                                    showToast(t("common.deleted"));
                                    void loadSub();
                                  },
                                },
                              ]}
                            />
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
          <Pagination
            total={filteredWebhooks.length}
            page={pagedWebhooks.safePage}
            pageSize={pageSize}
            onPageSizeChange={onChangePageSize}
            onChange={setPageIdx}
          />

          <AnimatedModal
            open={whModalOpen}
            onClose={() => setWhModalOpen(false)}
            title={whForm.id ? t("dist.edit_wh") : t("dist.new_wh")}
            subtitle={t("dist.wh_desc")}
            maxWidth="640px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setWhModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveWebhook()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveWebhook(e)} autoComplete="off">
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_wh_name")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("dist.wh_name_ph")}
                    value={whForm.name}
                    onInput={(e) => setWhForm({ ...whForm, name: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.lbl_wh_provider")}</label>
                  <ConsoleSelect
                    searchable={false}
                    value={whForm.provider}
                    onChange={(val) => setWhForm({ ...whForm, provider: val })}
                    options={[
                      { value: "custom", label: t("dist.wh_custom") },
                      { value: "feishu", label: t("dist.wh_feishu") },
                      { value: "dingtalk", label: t("dist.wh_dingtalk") },
                      { value: "wecom", label: t("dist.wh_wecom") },
                      { value: "bark", label: t("dist.wh_bark") },
                      { value: "telegram", label: t("dist.wh_telegram") },
                    ]}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_wh_url")}</label>
                <input
                  class="volo-input"
                  required
                  placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/..."
                  value={whForm.url}
                  onInput={(e) => setWhForm({ ...whForm, url: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>

              <div class={whForm.provider === "telegram" ? "volo-grid-2" : "volo-field"}>
                <div class="volo-field">
                  <label>{t("dist.lbl_wh_secret")}</label>
                  <input
                    type="password"
                    name="webhook_hmac_token"
                    autoComplete="new-password"
                    class="volo-input"
                    disabled={whForm.clear_secret}
                    placeholder={
                      whForm.clear_secret
                        ? t("dist.wh_clear_secret")
                        : whForm.secret_set
                        ? t("sys.keep_blank_ph")
                        : t("dist.wh_secret_ph")
                    }
                    value={whForm.secret}
                    onInput={(e) =>
                      setWhForm({
                        ...whForm,
                        secret: (e.currentTarget as HTMLInputElement).value,
                        clear_secret: false,
                      })
                    }
                  />
                  {whForm.id && whForm.secret_set && !whForm.secret && (
                    <div style={{ marginTop: "6px" }}>
                      <SwitchToggle
                        checked={whForm.clear_secret}
                        onChange={(v) => setWhForm({ ...whForm, clear_secret: v })}
                        label={t("dist.wh_clear_secret")}
                      />
                    </div>
                  )}
                </div>
                {whForm.provider === "telegram" && (
                  <div class="volo-field">
                    <label>{t("dist.lbl_wh_chat")}</label>
                    <input
                      class="volo-input"
                      placeholder={t("dist.wh_chat_id_ph")}
                      value={whForm.chat_id}
                      onInput={(e) => setWhForm({ ...whForm, chat_id: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                )}
              </div>

              <div class="volo-field">
                <label>{t("dist.lbl_wh_events")}</label>
                <div class="volo-check-grid">
                  {WH_EVENTS.map((ev) => (
                    <SwitchToggle
                      key={ev.key}
                      checked={whForm.events.includes(ev.key)}
                      onChange={(on) =>
                        setWhForm({
                          ...whForm,
                          events: on ? [...whForm.events, ev.key] : whForm.events.filter((x) => x !== ev.key),
                        })
                      }
                      label={whEventText(ev.key)}
                    />
                  ))}
                </div>
              </div>

              <SwitchToggle
                checked={whForm.is_enabled}
                onChange={(v) => setWhForm({ ...whForm, is_enabled: v })}
                label={t("tbl.status_active")}
              />
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={!!whDelivModal}
            onClose={() => setWhDelivModal(null)}
            title={t("dist.wh_deliveries")}
            subtitle={whDelivModal?.webhook?.name || ""}
            maxWidth="780px"
            footer={
              <button type="button" class="volo-btn volo-btn-primary" onClick={() => setWhDelivModal(null)}>
                {t("common.close")}
              </button>
            }
          >
            {whDelivModal?.loading ? (
              <div class="volo-help">{t("shell.loading")}</div>
            ) : !whDelivModal?.items?.length ? (
              <ConsoleEmpty text={t("dist.wh_deliveries_empty")} />
            ) : (
              <div style={{ display: "flex", flexDirection: "column", gap: "8px" }}>
                {whDelivModal.items.map((d: any) => {
                  const isOk = Boolean(d.success) || (d.status_code >= 200 && d.status_code < 400);
                  const isExp = expandedDelivId === d.id;
                  return (
                    <div key={d.id} class="volo-subcard" style={{ margin: 0 }}>
                      <div style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: "8px", justifyContent: "space-between" }}>
                        <div class="volo-cell-meta">
                          <span class={`volo-status-pill ${isOk ? "ok" : "danger"}`}>
                            <span class="volo-status-dot" />
                            {d.status_code > 0 ? `HTTP ${d.status_code}` : isOk ? "OK" : "ERR"}
                          </span>
                          <span class="volo-code-chip">{d.event || "event"}</span>
                          {d.duration_ms >= 0 && <span class="volo-help">{d.duration_ms}ms</span>}
                          <span class="volo-help">{formatDateTime(d.created_at)}</span>
                        </div>
                        <div class="volo-row-actions">
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => setExpandedDelivId(isExp ? null : d.id)}
                          >
                            {isExp ? t("common.close") : t("common.view")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn"
                            disabled={replayingDelivId === d.id}
                            onClick={() => void replayWebhookDelivery(whDelivModal.webhook.id, d.id)}
                          >
                            {t("dist.wh_replay")}
                          </button>
                        </div>
                      </div>
                      {d.error_text && (
                        <div class="volo-help" style={{ color: "var(--volo-danger)", marginTop: "4px", overflowWrap: "anywhere" }}>
                          {d.error_text}
                        </div>
                      )}
                      {isExp && (
                        <pre
                          style={{
                            marginTop: "8px",
                            marginBottom: 0,
                            padding: "10px",
                            borderRadius: "8px",
                            background: "var(--volo-raised)",
                            fontSize: "12px",
                            maxHeight: "240px",
                            overflow: "auto",
                            whiteSpace: "pre-wrap",
                            overflowWrap: "anywhere",
                          }}
                        >
                          {[
                            d.payload_json ? `Payload:\n${d.payload_json}` : "",
                            d.response_body ? `Response:\n${d.response_body}` : "",
                          ]
                            .filter(Boolean)
                            .join("\n\n") || "—"}
                        </pre>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </AnimatedModal>
        </div>
      )}

      {/* 6. Feed & SEO */}
      {sub === "feed_seo" && isAdmin && feedCfg && seoCfg && (
        <div>
          <div class="volo-card">
            <div class="volo-sec-head">
              <span class="volo-subcard-title">{t("dist.feed_verify_bar")}</span>
              <div class="volo-filter-controls">
                {FEED_VERIFY_LINKS.map((href) => (
                  <a key={href} class="volo-act-btn" href={href} target="_blank" rel="noreferrer">
                    {href} ↗
                  </a>
                ))}
                {feedCfg.podcast?.enabled && (
                  <a class="volo-act-btn" href="/podcast.xml" target="_blank" rel="noreferrer">
                    /podcast.xml ↗
                  </a>
                )}
              </div>
            </div>
          </div>

          <div class="volo-grid-2">
            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("dist.feed_head")}</h3>
                  <p class="volo-sec-desc">{t("dist.feed_desc")}</p>
                </div>
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.feed_mode")}</label>
                  <ConsoleSelect
                    searchable={false}
                    value={feedCfg.output_mode || "summary"}
                    onChange={(val) => setFeedCfg({ ...feedCfg, output_mode: val })}
                    options={[
                      { value: "summary", label: t("dist.feed_mode_summary") },
                      { value: "full", label: t("dist.feed_mode_full") },
                    ]}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.feed_count")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={feedCfg.item_count ?? 20}
                    onInput={(e) => setFeedCfg({ ...feedCfg, item_count: Number((e.currentTarget as HTMLInputElement).value) })}
                  />
                </div>
              </div>

              <div class="volo-check-grid">
                <SwitchToggle
                  checked={!!feedCfg.enable_llms_txt}
                  onChange={(v) => setFeedCfg({ ...feedCfg, enable_llms_txt: v })}
                  label={t("dist.feed_llms")}
                />
                <SwitchToggle
                  checked={!!feedCfg.enable_raw_markdown}
                  onChange={(v) => setFeedCfg({ ...feedCfg, enable_raw_markdown: v })}
                  label={t("dist.feed_raw_md")}
                />
                <SwitchToggle
                  checked={!!feedCfg.podcast?.enabled}
                  onChange={(v) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), enabled: v } })}
                  label={t("dist.feed_podcast")}
                />
                {!!feedCfg.podcast?.enabled && (
                  <SwitchToggle
                    checked={!!feedCfg.podcast?.explicit}
                    onChange={(v) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), explicit: v } })}
                    label={t("dist.podcast_explicit")}
                  />
                )}
              </div>

              {!!feedCfg.podcast?.enabled && (
                <div class="volo-subcard">
                  <div class="volo-subcard-title">{t("dist.podcast_meta")}</div>
                  <div class="volo-grid-2">
                    <div class="volo-field">
                      <label>{t("dist.lbl_pod_title")}</label>
                      <input
                        class="volo-input"
                        placeholder={t("dist.podcast_title_ph")}
                        value={feedCfg.podcast?.title || ""}
                        onInput={(e) =>
                          setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), title: (e.currentTarget as HTMLInputElement).value } })
                        }
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("dist.lbl_pod_author")}</label>
                      <input
                        class="volo-input"
                        placeholder={t("dist.podcast_author_ph")}
                        value={feedCfg.podcast?.author || ""}
                        onInput={(e) =>
                          setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), author: (e.currentTarget as HTMLInputElement).value } })
                        }
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("dist.lbl_pod_email")}</label>
                      <input
                        class="volo-input"
                        placeholder={t("dist.podcast_email_ph")}
                        value={feedCfg.podcast?.owner_email || ""}
                        onInput={(e) =>
                          setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), owner_email: (e.currentTarget as HTMLInputElement).value } })
                        }
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("dist.lbl_pod_cat")}</label>
                      <input
                        class="volo-input"
                        placeholder={t("dist.podcast_cat_ph")}
                        value={feedCfg.podcast?.category || ""}
                        onInput={(e) =>
                          setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), category: (e.currentTarget as HTMLInputElement).value } })
                        }
                      />
                    </div>
                  </div>
                  <div class="volo-field">
                    <label>{t("dist.lbl_pod_cover")}</label>
                    <div class="volo-input-row">
                      <input
                        class="volo-input"
                        placeholder={t("dist.podcast_cover_ph")}
                        value={feedCfg.podcast?.cover_image_url || ""}
                        onInput={(e) =>
                          setFeedCfg({
                            ...feedCfg,
                            podcast: { ...(feedCfg.podcast || {}), cover_image_url: (e.currentTarget as HTMLInputElement).value },
                          })
                        }
                      />
                      <MediaPickButton
                        filterKind="image"
                        onPick={(u) =>
                          setFeedCfg({
                            ...feedCfg,
                            podcast: { ...(feedCfg.podcast || {}), cover_image_url: u },
                          })
                        }
                      />
                    </div>
                  </div>
                  <div class="volo-field">
                    <label>{t("dist.lbl_pod_sum")}</label>
                    <textarea
                      class="volo-textarea"
                      rows={2}
                      placeholder={t("dist.podcast_sum_ph")}
                      value={feedCfg.podcast?.summary || ""}
                      onInput={(e) =>
                        setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), summary: (e.currentTarget as HTMLTextAreaElement).value } })
                      }
                    />
                  </div>
                </div>
              )}

              <div class="volo-form-actions">
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={savingFeed}
                  onClick={async () => {
                    if (savingFeed) return;
                    setSavingFeed(true);
                    try {
                      const res = await apiFetch<any>("/api/admin/settings/feed", {
                        method: "PATCH",
                        body: JSON.stringify({ expected_version: feedVer, ...feedCfg }),
                      });
                      setFeedCfg(res.value || feedCfg);
                      setFeedVer(res.version);
                      showToast(t("common.saved"));
                    } catch (err: any) {
                      showToast(err.message);
                    } finally {
                      setSavingFeed(false);
                    }
                  }}
                >
                  {t("dist.save_feed")}
                </button>
              </div>
            </div>

            <div class="volo-card">
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("dist.seo_head")}</h3>
                  <p class="volo-sec-desc">{t("dist.seo_sub")}</p>
                </div>
              </div>
              <div class="volo-field">
                <label>{t("dist.seo_kw")}</label>
                <TokenListInput
                  values={String(seoCfg.meta_keywords || "")
                    .split(/[,，]/)
                    .map((s) => s.trim())
                    .filter(Boolean)}
                  onChange={(next) => setSeoCfg({ ...seoCfg, meta_keywords: next.join(", ") })}
                />
              </div>
              <div class="volo-field">
                <label>{t("dist.seo_desc")}</label>
                <textarea
                  class="volo-textarea"
                  rows={3}
                  value={seoCfg.meta_description || ""}
                  onInput={(e) => setSeoCfg({ ...seoCfg, meta_description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>
              <div class="volo-field">
                <label>{t("dist.seo_og")}</label>
                <div class="volo-input-row">
                  <input
                    class="volo-input"
                    value={seoCfg.default_og_image_url || ""}
                    onInput={(e) => setSeoCfg({ ...seoCfg, default_og_image_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton filterKind="image" onPick={(u) => setSeoCfg({ ...seoCfg, default_og_image_url: u })} />
                </div>
              </div>

              <div class="volo-subcard">
                <div class="volo-subcard-title">
                  <span>{t("dist.seo_indexnow_title")}</span>
                  {!!seoCfg.indexnow_enabled && (
                    <button
                      type="button"
                      class="volo-act-btn"
                      disabled={pushingIndexNow || savingSeo}
                      onClick={async () => {
                        if (pushingIndexNow || savingSeo) return;
                        setPushingIndexNow(true);
                        try {
                          const ok = await saveSeoSettings(true);
                          if (!ok) return;
                          const r = await apiFetch<{ pushed_urls: number }>("/api/admin/seo/indexnow-push", {
                            method: "POST",
                            body: "{}",
                          });
                          showToast(t("dist.seo_pushed", { count: r.pushed_urls ?? 0 }));
                        } catch (err: any) {
                          showToast(err.message);
                        } finally {
                          setPushingIndexNow(false);
                        }
                      }}
                    >
                      {t("dist.seo_push_now")}
                    </button>
                  )}
                </div>
                <SwitchToggle
                  checked={!!seoCfg.indexnow_enabled}
                  onChange={(v) => setSeoCfg({ ...seoCfg, indexnow_enabled: v })}
                  label={t("dist.seo_indexnow_on")}
                />
                {!!seoCfg.indexnow_enabled && (
                  <div class="volo-field" style={{ marginTop: "12px" }}>
                    <label>{t("dist.seo_indexnow_key")}</label>
                    <div class="volo-input-row">
                      <input
                        class="volo-input"
                        value={seoCfg.indexnow_key || ""}
                        onInput={(e) => setSeoCfg({ ...seoCfg, indexnow_key: (e.currentTarget as HTMLInputElement).value })}
                      />
                      <button type="button" class="volo-btn volo-btn-sm" onClick={() => setSeoCfg({ ...seoCfg, indexnow_key: genRandomHexKey() })}>
                        {t("dist.seo_gen_key")}
                      </button>
                    </div>
                  </div>
                )}
              </div>

              <div class="volo-subcard">
                <div class="volo-subcard-title">
                  <span>{t("dist.seo_gh_title")}</span>
                  <button type="button" class="volo-act-btn" onClick={() => switchDistSub("github")}>
                    {t("nav.github")} →
                  </button>
                </div>
                <div class="volo-help">{t("dist.seo_gh_hint")}</div>
              </div>

              <div class="volo-form-actions">
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={savingSeo}
                  onClick={() => void saveSeoSettings(false)}
                >
                  {t("dist.save_seo")}
                </button>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

