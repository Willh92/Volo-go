import { useEffect, useState } from "preact/hooks";
import { apiFetch, currentUserSig, resetConsoleScroll, showToast, storageDriverSig } from "../api";
import { MediaPickButton } from "../components/MediaLibraryModal";
import { AnimatedModal, ConsoleSelect, NavIconPreview, paginateItems, Pagination } from "../components/UIControls";
import { formatDateTime, t } from "../i18n";

export type DistTab = "nav_redirects" | "feed_seo" | "github" | "links" | "follows" | "webhooks";

const WH_EVENTS = [
  { key: "article.published", labelKey: "dist.wh_ev_post" },
  { key: "comment.created", labelKey: "dist.wh_ev_cmt" },
  { key: "security.alert", labelKey: "dist.wh_ev_sec" },
  { key: "backup.completed", labelKey: "dist.wh_ev_bak" },
] as const;

const NAV_ICON_PRESETS = [
  { label: "github", value: "github" },
  { label: "book", value: "book" },
  { label: "rss", value: "rss" },
  { label: "mail", value: "mail" },
  { label: "link", value: "link" },
  { label: "external", value: "external" },
  { label: "admin", value: "admin" },
] as const;

const EMPTY_NAV = {
  id: "",
  title: "",
  url: "/",
  zh: "",
  en: "",
  icon: "",
  sort_order: 10,
  open_in_new_tab: false,
  is_enabled: true,
};

const EMPTY_REDIR = {
  id: "",
  source_path: "",
  target_path: "",
  status_code: 301,
};

const EMPTY_LINK = {
  id: "",
  title: "",
  url: "",
  icon_url: "",
  description: "",
  category_name: "",
  sort_order: 1,
};

const EMPTY_FOLLOW = {
  id: "",
  title: "",
  site_url: "",
  feed_url: "",
  icon_url: "",
  description: "",
  sort_order: 1,
};

const EMPTY_WH = {
  id: "",
  name: "",
  provider: "custom",
  url: "",
  chat_id: "",
  secret: "",
  secret_set: false,
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

function whEventLabel(labelKey: (typeof WH_EVENTS)[number]["labelKey"]): string {
  switch (labelKey) {
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

export function DistributeWorkspace({
  activeTab = "links",
  onNavigate,
}: {
  activeTab?: DistTab;
  onNavigate?: (sec: string) => void;
}) {
  const isAdmin = currentUserSig.value?.role === "admin";
  const [sub, setSub] = useState<DistTab>(activeTab);
  const [navSub, setNavSub] = useState<"nav" | "redirects">("nav");
  const [pageIdx, setPageIdx] = useState<number>(1);

  // Nav
  const [navItems, setNavItems] = useState<any[]>([]);
  const [navModalOpen, setNavModalOpen] = useState<boolean>(false);
  const [navForm, setNavForm] = useState({ ...EMPTY_NAV });

  // Redirects
  const [redirects, setRedirects] = useState<any[]>([]);
  const [redirSearch, setRedirSearch] = useState<string>("");
  const [redirModalOpen, setRedirModalOpen] = useState<boolean>(false);
  const [redirForm, setRedirForm] = useState({ ...EMPTY_REDIR });

  // Links
  const [links, setLinks] = useState<any[]>([]);
  const [linkCatFilter, setLinkCatFilter] = useState<string>("");
  const [linkSearch, setLinkSearch] = useState<string>("");
  const [linkModalOpen, setLinkModalOpen] = useState<boolean>(false);
  const [linkForm, setLinkForm] = useState({ ...EMPTY_LINK });

  // Follows
  const [follows, setFollows] = useState<any[]>([]);
  const [followModalOpen, setFollowModalOpen] = useState<boolean>(false);
  const [followForm, setFollowForm] = useState({ ...EMPTY_FOLLOW });

  // Webhooks
  const [webhooks, setWebhooks] = useState<any[]>([]);
  const [whModalOpen, setWhModalOpen] = useState<boolean>(false);
  const [whForm, setWhForm] = useState({ ...EMPTY_WH });

  // Feed / SEO / GitHub Showcase
  const [feedCfg, setFeedCfg] = useState<any>(null);
  const [feedVer, setFeedVer] = useState<number>(0);
  const [seoCfg, setSeoCfg] = useState<any>(null);
  const [seoVer, setSeoVer] = useState<number>(0);
  const [ghSearch, setGhSearch] = useState<string>("");

  useEffect(() => {
    setSub(activeTab);
    setPageIdx(1);
    resetConsoleScroll();
  }, [activeTab]);

  const switchDistSub = (next: DistTab) => {
    setSub(next);
    setPageIdx(1);
    resetConsoleScroll();
    onNavigate?.(next);
  };

  const switchNavSub = (next: "nav" | "redirects") => {
    setNavSub(next);
    setPageIdx(1);
    resetConsoleScroll();
  };

  const loadSub = async () => {
    try {
      if (sub === "nav_redirects") {
        const [nv, rd] = await Promise.all([
          apiFetch<{ items: any[] }>("/api/admin/nav"),
          apiFetch<{ items: any[] }>("/api/admin/redirects"),
        ]);
        setNavItems(nv.items || []);
        setRedirects(rd.items || []);
      } else if (sub === "links") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/links");
        setLinks(r.items || []);
      } else if (sub === "follows") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/follows");
        setFollows(r.items || []);
      } else if (sub === "webhooks" && isAdmin) {
        const r = await apiFetch<{ items: any[] }>("/api/admin/webhooks");
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
    }
  };

  useEffect(() => {
    setPageIdx(1);
    void loadSub();
  }, [sub]);

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
      if (navForm.zh.trim()) i18nTitles["zh-CN"] = navForm.zh.trim();
      if (navForm.en.trim()) i18nTitles["en-US"] = navForm.en.trim();
      const payload = {
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
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveRedirect = async (e?: Event) => {
    e?.preventDefault();
    if (!redirForm.source_path.trim() || !redirForm.target_path.trim()) return;
    try {
      const payload = {
        source_path: redirForm.source_path.trim(),
        target_path: redirForm.target_path.trim(),
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
      void loadSub();
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
      setLinkModalOpen(false);
      setLinkForm({ ...EMPTY_LINK });
      showToast(t("common.saved"));
      void loadSub();
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
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveWebhook = async (e?: Event) => {
    e?.preventDefault();
    if (!whForm.name.trim() || !whForm.url.trim()) return;
    try {
      const payload = {
        name: whForm.name.trim(),
        provider: whForm.provider,
        url: whForm.url.trim(),
        chat_id: whForm.chat_id.trim(),
        secret: whForm.secret,
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

  const saveSeoSettings = async (silent = false) => {
    if (!seoCfg) return false;
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
    }
  };

  const pullGitHubRepos = async () => {
    if (!seoCfg) return;
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
    }
  };

  const publishGitHubShowcase = async () => {
    if (!seoCfg) return;
    const ok = await saveSeoSettings(true);
    if (!ok) return;
    try {
      await apiFetch("/api/admin/seo/github-publish", {
        method: "POST",
        body: "{}",
      });
      const repos: any[] = Array.isArray(seoCfg.github_repos) ? seoCfg.github_repos : [];
      const shownCount = repos.filter((rp) => !rp.hidden && (!rp.is_fork || seoCfg.github_include_forks)).length;
      showToast(t("dist.gh_published_toast", { count: shownCount }));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const filteredRedirects = redirSearch.trim()
    ? redirects.filter((r) => {
        const q = redirSearch.trim().toLowerCase();
        return (
          String(r.source_path || "").toLowerCase().includes(q) ||
          String(r.target_path || "").toLowerCase().includes(q)
        );
      })
    : redirects;

  const linkCategories = Array.from(
    new Set(links.map((l) => String(l.category_name || "").trim()).filter(Boolean))
  );

  const filteredLinks = links.filter((l) => {
    if (linkCatFilter && String(l.category_name || "").trim() !== linkCatFilter) {
      return false;
    }
    if (linkSearch.trim()) {
      const q = linkSearch.trim().toLowerCase();
      return (
        String(l.title || l.name || "").toLowerCase().includes(q) ||
        String(l.url || "").toLowerCase().includes(q) ||
        String(l.description || "").toLowerCase().includes(q)
      );
    }
    return true;
  });

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

  const pagedNav = paginateItems(navItems, pageIdx, 15);
  const pagedRedir = paginateItems(filteredRedirects, pageIdx, 15);
  const pagedGh = paginateItems(filteredGhRepos, pageIdx, 15);
  const pagedLinks = paginateItems(filteredLinks, pageIdx, 15);
  const pagedFollows = paginateItems(follows, pageIdx, 15);
  const pagedWebhooks = paginateItems(webhooks, pageIdx, 15);

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
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => {
                setNavForm({ ...EMPTY_NAV });
                setNavModalOpen(true);
              }}
            >
              + {t("dist.new_nav")}
            </button>
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
                {pagedNav.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedNav.slice.map((it) => {
                    let parsed: Record<string, string> = {};
                    try {
                      parsed = it.i18n_titles_json ? JSON.parse(it.i18n_titles_json) : {};
                    } catch {
                      parsed = {};
                    }
                    const i18nSummary = Object.entries(parsed)
                      .filter(([, v]) => Boolean(v))
                      .map(([k, v]) => `${k}: ${v}`)
                      .join(" · ");
                    return (
                      <tr key={it.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-title">
                            <NavIconPreview icon={it.icon} fallbackLabel={it.title} />
                            <span>{it.title}</span>
                            {it.icon && <span class="volo-code-chip">{it.icon}</span>}
                            {it.open_in_new_tab && <span class="volo-mini-tag">↗</span>}
                            {it.is_enabled === false && <span class="volo-mini-tag warn">{t("tbl.status_disabled")}</span>}
                          </div>
                        </td>
                        <td class="col-cat">
                          <span class="volo-code-chip">{it.url}</span>
                        </td>
                        <td class="col-fluid">
                          <span class="volo-help">{i18nSummary || "—"}</span>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setNavForm({
                                  id: it.id,
                                  title: it.title || "",
                                  url: it.url || "/",
                                  zh: parsed["zh-CN"] || "",
                                  en: parsed["en-US"] || "",
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
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                await apiFetch(`/api/admin/nav/${it.id}`, { method: "DELETE" });
                                void loadSub();
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
          <Pagination total={navItems.length} page={pagedNav.safePage} pageSize={15} onChange={setPageIdx} />

          <AnimatedModal
            open={navModalOpen}
            onClose={() => setNavModalOpen(false)}
            title={navForm.id ? t("dist.edit_nav") : t("dist.new_nav")}
            subtitle={t("dist.nav_desc")}
            maxWidth="620px"
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

              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("dist.lbl_nav_zh")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("dist.nav_zh_ph")}
                    value={navForm.zh}
                    onInput={(e) => setNavForm({ ...navForm, zh: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("dist.lbl_nav_en")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("dist.nav_en_ph")}
                    value={navForm.en}
                    onInput={(e) => setNavForm({ ...navForm, en: (e.currentTarget as HTMLInputElement).value })}
                  />
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
                  <MediaPickButton
                    filterKind="image"
                    onPick={(u) => setNavForm({ ...navForm, icon: u })}
                  />
                </div>
                <div class="volo-tag-pool">
                  <span class="volo-help" style={{ fontSize: "11.5px" }}>{t("dist.nav_icon_presets")}</span>
                  {NAV_ICON_PRESETS.map((pr) => (
                    <button
                      key={pr.value}
                      type="button"
                      class={`volo-chip-btn ${navForm.icon === pr.value ? "active" : ""}`}
                      onClick={() => setNavForm({ ...navForm, icon: navForm.icon === pr.value ? "" : pr.value })}
                    >
                      {pr.label}
                    </button>
                  ))}
                </div>
              </div>

              <div class="volo-grid-2">
                <div class="volo-field" style={{ marginBottom: 0 }}>
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={navForm.sort_order}
                    onInput={(e) => setNavForm({ ...navForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
                <div class="volo-field" style={{ marginBottom: 0, display: "flex", flexDirection: "column", justifyContent: "flex-end" }}>
                  <div class="volo-check-grid" style={{ margin: 0 }}>
                    <label class="volo-check-item">
                      <input
                        type="checkbox"
                        checked={navForm.open_in_new_tab}
                        onChange={(e) => setNavForm({ ...navForm, open_in_new_tab: (e.currentTarget as HTMLInputElement).checked })}
                      />
                      <span>{t("dist.nav_new_tab")}</span>
                    </label>
                    <label class="volo-check-item">
                      <input
                        type="checkbox"
                        checked={navForm.is_enabled !== false}
                        onChange={(e) => setNavForm({ ...navForm, is_enabled: (e.currentTarget as HTMLInputElement).checked })}
                      />
                      <span>{t("tbl.status_active")}</span>
                    </label>
                  </div>
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
              <input
                class="volo-input"
                style={{ width: "220px" }}
                placeholder={t("dist.redir_search_ph")}
                value={redirSearch}
                onInput={(e) => {
                  setRedirSearch((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
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

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-fluid">{t("tbl.col_source")}</th>
                  <th class="col-fluid">{t("tbl.col_target")}</th>
                  <th class="col-num">{t("tbl.col_hits")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {pagedRedir.slice.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedRedir.slice.map((r) => (
                    <tr key={r.id}>
                      <td class="col-status">
                        <div class="volo-cell-meta">
                          <span class={`volo-status-pill ${r.status_code === 301 ? "ok" : "warn"}`}>
                            <span class="volo-status-dot" />
                            {r.status_code}
                          </span>
                          {r.is_system_legacy && <span class="volo-mini-tag">{t("dist.redir_legacy")}</span>}
                        </div>
                      </td>
                      <td class="col-fluid">
                        <span class="volo-code-chip">{r.source_path}</span>
                      </td>
                      <td class="col-fluid">
                        <span class="volo-code-chip">{r.target_path}</span>
                      </td>
                      <td class="col-num">
                        <span class="volo-cat-chip">{t("dist.redir_hits", { count: r.hit_count ?? 0 })}</span>
                      </td>
                      <td class="col-act">
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
                              await apiFetch(`/api/admin/redirects/${r.id}`, { method: "DELETE" });
                              void loadSub();
                            }}
                          >
                            {t("common.delete")}
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
          <Pagination total={filteredRedirects.length} page={pagedRedir.safePage} pageSize={15} onChange={setPageIdx} />

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
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("dist.lbl_redir_code")}</label>
                <ConsoleSelect
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
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void saveSeoSettings(false)}>
                {t("dist.gh_save_sel")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={() => void publishGitHubShowcase()}>
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
            <div class="volo-grid-3">
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("dist.seo_gh_user")}</label>
                <input
                  class="volo-input"
                  placeholder="Willh92"
                  value={seoCfg.github_username || ""}
                  onInput={(e) => setSeoCfg({ ...seoCfg, github_username: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0, display: "flex", flexDirection: "column", justifyContent: "flex-end" }}>
                <label class="volo-check-item" style={{ height: "38px" }}>
                  <input
                    type="checkbox"
                    checked={!!seoCfg.github_include_forks}
                    onChange={(e) => setSeoCfg({ ...seoCfg, github_include_forks: (e.currentTarget as HTMLInputElement).checked })}
                  />
                  <span>{t("dist.seo_gh_forks")}</span>
                </label>
              </div>
              <div class="volo-field" style={{ marginBottom: 0, display: "flex", flexDirection: "column", justifyContent: "flex-end" }}>
                <div style={{ display: "flex", gap: "8px", justifyContent: "flex-end" }}>
                  {seoCfg.github_username && (
                    <a class="volo-btn" href="/posts/github" target="_blank" rel="noreferrer">
                      {t("common.preview")}
                    </a>
                  )}
                  <button type="submit" class="volo-btn volo-btn-primary">
                    {t("dist.sync_gh")}
                  </button>
                </div>
              </div>
            </div>
          </form>

          {allGhRepos.length > 0 && (
            <div
              class="volo-subcard"
              style={{
                display: "flex",
                alignItems: "center",
                justifyContent: "space-between",
                gap: "14px",
                flexWrap: "wrap",
                marginTop: 0,
              }}
            >
              <div>
                <div style={{ fontWeight: 650, fontSize: "13.5px" }}>
                  {t("dist.gh_stats_bar", {
                    shown: visibleGhRepos.length,
                    total: allGhRepos.length,
                    stars: totalGhStars,
                    forks: totalGhForks,
                  })}
                </div>
                {seoCfg.github_synced_at > 0 && (
                  <div class="volo-help" style={{ marginTop: "3px" }}>
                    {t("dist.follows_last_sync", { time: formatDateTime(seoCfg.github_synced_at) })}
                  </div>
                )}
              </div>
              {seoCfg.github_username && (
                <img
                  src={`/github-stats/${encodeURIComponent(seoCfg.github_username)}.svg?t=${seoVer}`}
                  alt={seoCfg.github_username}
                  style={{ maxHeight: "74px", borderRadius: "8px", border: "1px solid var(--volo-line)" }}
                />
              )}
            </div>
          )}

          <div class="volo-filter-bar" style={{ marginTop: "6px" }}>
            <div class="volo-filter-controls">
              <input
                class="volo-input"
                autoComplete="off"
                style={{ width: "260px" }}
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
                  <th style={{ width: "92px" }}>{t("dist.gh_col_show")}</th>
                  <th class="col-fluid">{t("dist.gh_col_repo")}</th>
                  <th class="col-cat">{t("dist.gh_col_lang")}</th>
                  <th class="col-num">{t("dist.gh_col_metrics")}</th>
                </tr>
              </thead>
              <tbody>
                {pagedGh.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("dist.gh_empty")}</td>
                  </tr>
                ) : (
                  pagedGh.slice.map((rp) => {
                    const key = rp.full_name || rp.name;
                    const shown = !rp.hidden;
                    return (
                      <tr key={key}>
                        <td>
                          <label class="volo-check-item" style={{ justifyContent: "flex-start" }}>
                            <input
                              type="checkbox"
                              checked={shown}
                              onChange={(e) => {
                                const on = (e.currentTarget as HTMLInputElement).checked;
                                const next = allGhRepos.map((item) =>
                                  (item.full_name || item.name) === key ? { ...item, hidden: !on } : item
                                );
                                setSeoCfg({ ...seoCfg, github_repos: next });
                              }}
                            />
                          </label>
                        </td>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <a href={rp.html_url || `https://github.com/${rp.full_name}`} target="_blank" rel="noreferrer">
                                {rp.name}
                              </a>
                              {rp.is_fork && <span class="volo-mini-tag">Fork</span>}
                            </div>
                            {rp.description && (
                              <div class="volo-cell-meta">
                                <span>{rp.description}</span>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-cat">
                          <span class={`volo-cat-chip ${rp.language ? "" : "muted"}`}>{rp.language || "—"}</span>
                        </td>
                        <td class="col-num">
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
          <Pagination total={filteredGhRepos.length} page={pagedGh.safePage} pageSize={15} onChange={setPageIdx} />

          <div class="volo-form-actions">
            <button type="button" class="volo-btn" onClick={() => void saveSeoSettings(false)}>
              {t("dist.gh_save_sel")}
            </button>
            <button type="button" class="volo-btn volo-btn-primary" onClick={() => void publishGitHubShowcase()}>
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
                onClick={async () => {
                  const r = await apiFetch<{ checked: number; items?: any[] }>("/api/admin/links/patrol", {
                    method: "POST",
                    body: "{}",
                  });
                  const list = r.items || [];
                  const healthy = list.filter((x) => x.health_status === "ok" || x.health_status === "redirect").length;
                  setLinks(list);
                  showToast(t("dist.links_patrolled", { checked: r.checked ?? list.length, healthy }));
                }}
              >
                {t("dist.links_patrol")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  setLinkForm({ ...EMPTY_LINK });
                  setLinkModalOpen(true);
                }}
              >
                + {t("dist.new_link")}
              </button>
            </div>
          </div>

          <div class="volo-filter-bar" style={{ marginBottom: "14px" }}>
            <div class="volo-filter-controls">
              <ConsoleSelect
                style={{ width: "176px" }}
                value={linkCatFilter}
                onChange={(val) => {
                  setLinkCatFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("art.all_categories") },
                  ...linkCategories.map((cat) => ({ value: cat, label: cat })),
                ]}
              />
              <input
                class="volo-input"
                autoComplete="off"
                style={{ width: "220px" }}
                placeholder={t("dist.links_search_ph")}
                value={linkSearch}
                onInput={(e) => {
                  setLinkSearch((e.currentTarget as HTMLInputElement).value);
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
                  <th class="col-fluid">{t("tbl.col_url")}</th>
                  <th class="col-status">{t("tbl.col_health")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {pagedLinks.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedLinks.slice.map((l) => {
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
                    return (
                      <tr key={l.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <NavIconPreview icon={l.icon_url} fallbackLabel={label} />
                              <span>{label}</span>
                              {l.category_name && <span class="volo-mini-tag">{l.category_name}</span>}
                            </div>
                            {l.description && <div class="volo-cell-meta"><span>{l.description}</span></div>}
                          </div>
                        </td>
                        <td class="col-fluid">
                          <a href={l.url} target="_blank" rel="noreferrer" class="volo-code-chip" style={{ textDecoration: "none" }}>
                            {l.url}
                          </a>
                        </td>
                        <td class="col-status">
                          <div class="volo-cell-meta">
                            <span class={`volo-status-pill ${pillCls}`}>
                              <span class="volo-status-dot" />
                              {statusText}
                            </span>
                            {l.latency_ms > 0 && <span class="volo-help">{l.latency_ms}ms</span>}
                          </div>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <a class="volo-act-btn" href={l.url} target="_blank" rel="noreferrer">
                              {t("common.view")}
                            </a>
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
                                  sort_order: l.sort_order ?? 1,
                                });
                                setLinkModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                await apiFetch(`/api/admin/links/${l.id}`, { method: "DELETE" });
                                void loadSub();
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
          <Pagination total={filteredLinks.length} page={pagedLinks.safePage} pageSize={15} onChange={setPageIdx} />

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
                    value={linkForm.sort_order ?? 1}
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
                  <MediaPickButton
                    filterKind="image"
                    onPick={(u) => setLinkForm({ ...linkForm, icon_url: u })}
                  />
                </div>
              </div>

              <div class="volo-field" style={{ marginBottom: 0 }}>
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
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={async () => {
                  const r = await apiFetch<{ synced_items: number }>("/api/admin/follows/sync", { method: "POST", body: "{}" });
                  showToast(t("dist.follows_synced", { count: r.synced_items }));
                  void loadSub();
                }}
              >
                {t("dist.follows_sync")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  setFollowForm({ ...EMPTY_FOLLOW });
                  setFollowModalOpen(true);
                }}
              >
                + {t("dist.new_follow")}
              </button>
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
                {pagedFollows.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedFollows.slice.map((f) => {
                    const label = f.title || f.name || "";
                    return (
                      <tr key={f.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <NavIconPreview icon={f.icon_url} fallbackLabel={label} />
                              <span>{label}</span>
                            </div>
                            <div class="volo-cell-meta">
                              {f.site_url && <span>{f.site_url}</span>}
                              {f.description && <span>{f.description}</span>}
                            </div>
                          </div>
                        </td>
                        <td class="col-fluid">
                          <span class="volo-code-chip">{f.feed_url}</span>
                        </td>
                        <td class="col-date">
                          <div class="volo-cell-main">
                            <span>{formatDateTime(f.last_synced_at)}</span>
                            {f.sync_error && (
                              <span class="volo-status-pill danger">
                                <span class="volo-status-dot" />
                                {t("dist.follows_err", { err: f.sync_error })}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
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
                                  sort_order: f.sort_order ?? 1,
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
                                await apiFetch(`/api/admin/follows/${f.id}`, { method: "DELETE" });
                                void loadSub();
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
          <Pagination total={follows.length} page={pagedFollows.safePage} pageSize={15} onChange={setPageIdx} />

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
                    value={followForm.sort_order ?? 1}
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
                  <MediaPickButton
                    filterKind="image"
                    onPick={(u) => setFollowForm({ ...followForm, icon_url: u })}
                  />
                </div>
              </div>

              <div class="volo-field" style={{ marginBottom: 0 }}>
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
                {pagedWebhooks.slice.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedWebhooks.slice.map((item) => {
                    const w = item.webhook || item;
                    const prov = w.provider || w.platform || "custom";
                    let evList: string[] = [];
                    try {
                      evList = w.events_json ? JSON.parse(w.events_json) : [];
                    } catch {
                      evList = [];
                    }
                    return (
                      <tr key={w.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">{w.name}</div>
                            {evList.length > 0 && <div class="volo-cell-meta"><span>{evList.join(" · ")}</span></div>}
                          </div>
                        </td>
                        <td class="col-cat">
                          <span class="volo-cat-chip">{whProviderLabel(prov)}</span>
                        </td>
                        <td class="col-fluid">
                          <span class="volo-code-chip">{w.url || w.target_url}</span>
                        </td>
                        <td class="col-status">
                          <div class="volo-cell-meta">
                            <span class={`volo-status-pill ${w.is_enabled !== false ? "ok" : ""}`}>
                              <span class="volo-status-dot" />
                              {w.is_enabled !== false ? t("tbl.status_active") : t("tbl.status_disabled")}
                            </span>
                            {item.secret_set && <span class="volo-mini-tag">{t("dist.wh_sealed")}</span>}
                            {w.last_status_code > 0 && (
                              <span class={`volo-mini-tag ${w.last_status_code < 400 ? "" : "warn"}`}>
                                HTTP {w.last_status_code}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                try {
                                  const r = await apiFetch<{ status_code: number }>(`/api/admin/webhooks/${w.id}/test`, {
                                    method: "POST",
                                    body: "{}",
                                  });
                                  showToast(t("dist.wh_tested", { code: r.status_code }));
                                  void loadSub();
                                } catch (err: any) {
                                  showToast(err.message);
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
                                  events: evList.length > 0 ? evList : ["article.published", "comment.created"],
                                  is_enabled: w.is_enabled !== false,
                                });
                                setWhModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                await apiFetch(`/api/admin/webhooks/${w.id}`, { method: "DELETE" });
                                void loadSub();
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
          <Pagination total={webhooks.length} page={pagedWebhooks.safePage} pageSize={15} onChange={setPageIdx} />

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
                <div class="volo-field" style={{ marginBottom: whForm.provider === "telegram" ? undefined : 0 }}>
                  <label>{t("dist.lbl_wh_secret")}</label>
                  <input
                    type="password"
                    name="webhook_hmac_token"
                    autoComplete="new-password"
                    class="volo-input"
                    placeholder={whForm.secret_set ? t("sys.keep_blank_ph") : t("dist.wh_secret_ph")}
                    value={whForm.secret}
                    onInput={(e) => setWhForm({ ...whForm, secret: (e.currentTarget as HTMLInputElement).value })}
                  />
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
                <div class="volo-check-grid" style={{ margin: "4px 0 0" }}>
                  {WH_EVENTS.map((ev) => (
                    <label key={ev.key} class="volo-check-item">
                      <input
                        type="checkbox"
                        checked={whForm.events.includes(ev.key)}
                        onChange={(e) => {
                          const on = (e.currentTarget as HTMLInputElement).checked;
                          setWhForm({
                            ...whForm,
                            events: on ? [...whForm.events, ev.key] : whForm.events.filter((x) => x !== ev.key),
                          });
                        }}
                      />
                      <span>{whEventLabel(ev.labelKey)}</span>
                    </label>
                  ))}
                </div>
              </div>

              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={whForm.is_enabled}
                  onChange={(e) => setWhForm({ ...whForm, is_enabled: (e.currentTarget as HTMLInputElement).checked })}
                />
                <span>{t("tbl.status_active")}</span>
              </label>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 6. Feed & SEO */}
      {sub === "feed_seo" && isAdmin && feedCfg && seoCfg && (
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
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!feedCfg.enable_llms_txt}
                  onChange={(e) => setFeedCfg({ ...feedCfg, enable_llms_txt: (e.currentTarget as HTMLInputElement).checked })}
                />
                <span>{t("dist.feed_llms")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!feedCfg.enable_raw_markdown}
                  onChange={(e) => setFeedCfg({ ...feedCfg, enable_raw_markdown: (e.currentTarget as HTMLInputElement).checked })}
                />
                <span>{t("dist.feed_raw_md")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!feedCfg.podcast?.enabled}
                  onChange={(e) =>
                    setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), enabled: (e.currentTarget as HTMLInputElement).checked } })
                  }
                />
                <span>{t("dist.feed_podcast")}</span>
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!feedCfg.podcast?.explicit}
                  onChange={(e) =>
                    setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), explicit: (e.currentTarget as HTMLInputElement).checked } })
                  }
                />
                <span>{t("dist.podcast_explicit")}</span>
              </label>
            </div>

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
              <div class="volo-field" style={{ marginBottom: 0 }}>
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

            <div class="volo-form-actions">
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  const res = await apiFetch<any>("/api/admin/settings/feed", {
                    method: "PATCH",
                    body: JSON.stringify({ expected_version: feedVer, ...feedCfg }),
                  });
                  setFeedCfg(res.value || feedCfg);
                  setFeedVer(res.version);
                  showToast(t("common.saved"));
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
              <input
                class="volo-input"
                value={seoCfg.meta_keywords || ""}
                onInput={(e) => setSeoCfg({ ...seoCfg, meta_keywords: (e.currentTarget as HTMLInputElement).value })}
              />
            </div>
            <div class="volo-field">
              <label>{t("dist.seo_desc")}</label>
              <textarea
                class="volo-textarea"
                rows={2}
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
                <MediaPickButton
                  filterKind="image"
                  onPick={(u) => setSeoCfg({ ...seoCfg, default_og_image_url: u })}
                />
              </div>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">
                <span>{t("dist.seo_indexnow_title")}</span>
                <button
                  type="button"
                  class="volo-act-btn"
                  onClick={async () => {
                    try {
                      const r = await apiFetch<{ pushed_urls: number }>("/api/admin/seo/indexnow-push", {
                        method: "POST",
                        body: "{}",
                      });
                      showToast(t("dist.seo_pushed", { count: r.pushed_urls ?? 0 }));
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  {t("dist.seo_push_now")}
                </button>
              </div>
              <div class="volo-field">
                <label>{t("dist.seo_indexnow_key")}</label>
                <div style={{ display: "flex", gap: "8px" }}>
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
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!seoCfg.indexnow_enabled}
                  onChange={(e) => setSeoCfg({ ...seoCfg, indexnow_enabled: (e.currentTarget as HTMLInputElement).checked })}
                />
                <span>{t("dist.seo_indexnow_on")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">
                <span>{t("dist.seo_gh_title")}</span>
                <div style={{ display: "flex", gap: "6px" }}>
                  <button
                    type="button"
                    class="volo-act-btn"
                    onClick={() => switchDistSub("github")}
                  >
                    {t("nav.github")}
                  </button>
                  <button type="button" class="volo-act-btn" onClick={() => void pullGitHubRepos()}>
                    {t("dist.sync_gh")}
                  </button>
                </div>
              </div>
              <div class="volo-field" style={{ marginBottom: "8px" }}>
                <label>{t("dist.seo_gh_user")}</label>
                <input
                  class="volo-input"
                  value={seoCfg.github_username || ""}
                  onInput={(e) => setSeoCfg({ ...seoCfg, github_username: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={!!seoCfg.github_include_forks}
                  onChange={(e) => setSeoCfg({ ...seoCfg, github_include_forks: (e.currentTarget as HTMLInputElement).checked })}
                />
                <span>{t("dist.seo_gh_forks")}</span>
              </label>
            </div>

            <div class="volo-form-actions">
              <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveSeoSettings(false)}>
                {t("dist.save_seo")}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
