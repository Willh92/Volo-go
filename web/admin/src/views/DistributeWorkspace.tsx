import { useEffect, useState } from "preact/hooks";
import { apiFetch, currentUserSig, showToast } from "../api";
import { formatDateTime, t } from "../i18n";

export type DistTab = "nav_redirects" | "feed_seo" | "links" | "follows" | "webhooks";

const WH_EVENTS = [
  { key: "article.published", labelKey: "dist.wh_ev_post" },
  { key: "comment.created", labelKey: "dist.wh_ev_cmt" },
  { key: "security.alert", labelKey: "dist.wh_ev_sec" },
  { key: "backup.completed", labelKey: "dist.wh_ev_bak" },
] as const;

export function DistributeWorkspace({
  activeTab = "links",
  onNavigate,
}: {
  activeTab?: DistTab;
  onNavigate?: (sec: string) => void;
}) {
  const isAdmin = currentUserSig.value?.role === "admin";
  const [sub, setSub] = useState<DistTab>(activeTab);

  const [navItems, setNavItems] = useState<any[]>([]);
  const [navForm, setNavForm] = useState({
    id: "",
    title: "",
    url: "/",
    zh: "",
    en: "",
    sort_order: 10,
    open_in_new_tab: false,
  });

  const [links, setLinks] = useState<any[]>([]);
  const [linkForm, setLinkForm] = useState({
    id: "",
    title: "",
    url: "",
    icon_url: "",
    description: "",
    category_name: "",
    sort_order: 1,
  });

  const [follows, setFollows] = useState<any[]>([]);
  const [followForm, setFollowForm] = useState({ id: "", title: "", site_url: "", feed_url: "", description: "" });

  const [redirects, setRedirects] = useState<any[]>([]);
  const [redirForm, setRedirForm] = useState({ source_path: "", target_path: "", status_code: 301 });

  const [webhooks, setWebhooks] = useState<any[]>([]);
  const [whForm, setWhForm] = useState({
    id: "",
    name: "",
    provider: "custom",
    url: "",
    chat_id: "",
    secret: "",
    events: ["article.published", "comment.created"] as string[],
    is_enabled: true,
  });

  const [feedCfg, setFeedCfg] = useState<any>(null);
  const [feedVer, setFeedVer] = useState<number>(0);
  const [seoCfg, setSeoCfg] = useState<any>(null);
  const [seoVer, setSeoVer] = useState<number>(0);

  useEffect(() => {
    setSub(activeTab);
  }, [activeTab]);

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
      } else if (sub === "feed_seo" && isAdmin) {
        const s = await apiFetch<any>("/api/admin/settings");
        setFeedCfg(s.domains?.feed || {});
        setFeedVer(s.versions?.feed || 0);
        setSeoCfg(s.domains?.seo || {});
        setSeoVer(s.versions?.seo || 0);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [sub]);

  const distTabs: DistTab[] = isAdmin
    ? ["feed_seo", "links", "follows", "webhooks"]
    : ["links", "follows"];

  const genRandomHexKey = () => {
    const arr = new Uint8Array(16);
    crypto.getRandomValues(arr);
    return Array.from(arr, (b) => b.toString(16).padStart(2, "0")).join("");
  };

  return (
    <div>
      {sub !== "nav_redirects" && (
        <div class="volo-subtabs">
          {distTabs.map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => {
                setSub(k);
                onNavigate?.(k);
              }}
            >
              {t(`dist.${k}`)}
            </button>
          ))}
        </div>
      )}

      {sub === "nav_redirects" && (
        <div>
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("dist.nav_head")}</h3>
                <p class="volo-sec-desc">{t("dist.nav_desc")}</p>
              </div>
            </div>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                const i18nTitles: Record<string, string> = {};
                if (navForm.zh) i18nTitles["zh-CN"] = navForm.zh;
                if (navForm.en) i18nTitles["en-US"] = navForm.en;
                const payload = {
                  title: navForm.title,
                  url: navForm.url,
                  i18n_titles_json: JSON.stringify(i18nTitles),
                  sort_order: Number(navForm.sort_order) || 10,
                  open_in_new_tab: navForm.open_in_new_tab,
                  is_enabled: true,
                };
                if (!navForm.id) {
                  await apiFetch("/api/admin/nav", { method: "POST", body: JSON.stringify(payload) });
                } else {
                  await apiFetch(`/api/admin/nav/${navForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
                }
                setNavForm({ id: "", title: "", url: "/", zh: "", en: "", sort_order: 10, open_in_new_tab: false });
                showToast(t("common.saved"));
                void loadSub();
              }}
              class="volo-form-bar"
            >
              <div class="volo-grid-3">
                <input class="volo-input" required placeholder={t("dist.nav_label_ph")} value={navForm.title} onInput={(e) => setNavForm({ ...navForm, title: (e.currentTarget as HTMLInputElement).value })} />
                <input class="volo-input" required placeholder={t("dist.nav_url_ph")} value={navForm.url} onInput={(e) => setNavForm({ ...navForm, url: (e.currentTarget as HTMLInputElement).value })} />
                <div style={{ display: "flex", gap: "6px" }}>
                  <input class="volo-input" placeholder={t("dist.nav_zh_ph")} value={navForm.zh} onInput={(e) => setNavForm({ ...navForm, zh: (e.currentTarget as HTMLInputElement).value })} />
                  <input class="volo-input" placeholder={t("dist.nav_en_ph")} value={navForm.en} onInput={(e) => setNavForm({ ...navForm, en: (e.currentTarget as HTMLInputElement).value })} />
                </div>
              </div>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "12px", marginTop: "8px", flexWrap: "wrap" }}>
                <div style={{ display: "flex", alignItems: "center", gap: "14px" }}>
                  <input
                    type="number"
                    class="volo-input"
                    style={{ width: "92px" }}
                    placeholder={t("cnt.sort_ph")}
                    value={navForm.sort_order}
                    onInput={(e) => setNavForm({ ...navForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) })}
                  />
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={navForm.open_in_new_tab}
                      onChange={(e) => setNavForm({ ...navForm, open_in_new_tab: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{t("dist.nav_new_tab")}</span>
                  </label>
                </div>
                <div style={{ display: "flex", gap: "8px" }}>
                  {navForm.id && (
                    <button
                      type="button"
                      class="volo-btn"
                      onClick={() => setNavForm({ id: "", title: "", url: "/", zh: "", en: "", sort_order: 10, open_in_new_tab: false })}
                    >
                      {t("common.cancel")}
                    </button>
                  )}
                  <button type="submit" class="volo-btn volo-btn-primary">
                    {navForm.id ? t("common.save") : t("common.create")}
                  </button>
                </div>
              </div>
            </form>
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
                  {navItems.length === 0 ? (
                    <tr>
                      <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                    </tr>
                  ) : (
                    navItems.map((it) => {
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
                              <span>{it.title}</span>
                              {it.open_in_new_tab && <span class="volo-mini-tag">↗</span>}
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
                                onClick={() =>
                                  setNavForm({
                                    id: it.id,
                                    title: it.title || "",
                                    url: it.url || "/",
                                    zh: parsed["zh-CN"] || "",
                                    en: parsed["en-US"] || "",
                                    sort_order: it.sort_order ?? 10,
                                    open_in_new_tab: !!it.open_in_new_tab,
                                  })
                                }
                              >
                                {t("common.edit")}
                              </button>
                              <button type="button" class="volo-act-btn danger" onClick={async () => {
                                await apiFetch(`/api/admin/nav/${it.id}`, { method: "DELETE" });
                                void loadSub();
                              }}>{t("common.delete")}</button>
                            </div>
                          </td>
                        </tr>
                      );
                    })
                  )}
                </tbody>
              </table>
            </div>
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("dist.redir_head")}</h3>
                <p class="volo-sec-desc">{t("dist.redir_desc")}</p>
              </div>
            </div>
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                await apiFetch("/api/admin/redirects", { method: "POST", body: JSON.stringify(redirForm) });
                setRedirForm({ source_path: "", target_path: "", status_code: 301 });
                showToast(t("common.saved"));
                void loadSub();
              }}
              class="volo-form-bar"
            >
              <div class="volo-grid-3">
                <input class="volo-input" required placeholder={t("dist.redir_src_ph")} value={redirForm.source_path} onInput={(e) => setRedirForm({ ...redirForm, source_path: (e.currentTarget as HTMLInputElement).value })} />
                <input class="volo-input" required placeholder={t("dist.redir_dst_ph")} value={redirForm.target_path} onInput={(e) => setRedirForm({ ...redirForm, target_path: (e.currentTarget as HTMLInputElement).value })} />
                <div style={{ display: "flex", gap: "8px" }}>
                  <select class="volo-select" value={redirForm.status_code} onChange={(e) => setRedirForm({ ...redirForm, status_code: Number((e.currentTarget as HTMLSelectElement).value) })}>
                    <option value={301}>{t("dist.redir_301")}</option>
                    <option value={302}>{t("dist.redir_302")}</option>
                  </select>
                  <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
                </div>
              </div>
            </form>
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
                  {redirects.length === 0 ? (
                    <tr>
                      <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                    </tr>
                  ) : (
                    redirects.map((r) => (
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
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/redirects/${r.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("common.delete")}</button>
                          </div>
                        </td>
                      </tr>
                    ))
                  )}
                </tbody>
              </table>
            </div>
          </div>
        </div>
      )}

      {sub === "links" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.links")}</h3>
              <p class="volo-sec-desc">{t("dist.links_desc")}</p>
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-sm"
              onClick={async () => {
                const r = await apiFetch<{ checked: number; items?: any[] }>("/api/admin/links/patrol", { method: "POST", body: "{}" });
                const list = r.items || [];
                const healthy = list.filter((x) => x.health_status === "ok" || x.health_status === "redirect").length;
                setLinks(list);
                showToast(t("dist.links_patrolled", { checked: r.checked ?? list.length, healthy }));
              }}
            >
              {t("dist.links_patrol")}
            </button>
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              if (!linkForm.id) {
                await apiFetch("/api/admin/links", { method: "POST", body: JSON.stringify(linkForm) });
              } else {
                await apiFetch(`/api/admin/links/${linkForm.id}`, { method: "PUT", body: JSON.stringify(linkForm) });
              }
              setLinkForm({ id: "", title: "", url: "", icon_url: "", description: "", category_name: "", sort_order: 1 });
              showToast(t("common.saved"));
              void loadSub();
            }}
            class="volo-form-bar"
          >
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("dist.links_name_ph")} value={linkForm.title} onInput={(e) => setLinkForm({ ...linkForm, title: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" required placeholder="https://example.com" value={linkForm.url} onInput={(e) => setLinkForm({ ...linkForm, url: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("dist.links_cat_ph")} value={linkForm.category_name} onInput={(e) => setLinkForm({ ...linkForm, category_name: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <input class="volo-input" style={{ maxWidth: "260px" }} placeholder={t("dist.links_icon_ph")} value={linkForm.icon_url} onInput={(e) => setLinkForm({ ...linkForm, icon_url: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("cnt.desc_ph")} value={linkForm.description} onInput={(e) => setLinkForm({ ...linkForm, description: (e.currentTarget as HTMLInputElement).value })} />
              {linkForm.id && (
                <button
                  type="button"
                  class="volo-btn"
                  onClick={() => setLinkForm({ id: "", title: "", url: "", icon_url: "", description: "", category_name: "", sort_order: 1 })}
                >
                  {t("common.cancel")}
                </button>
              )}
              <button type="submit" class="volo-btn volo-btn-primary">
                {linkForm.id ? t("common.save") : t("common.create")}
              </button>
            </div>
          </form>
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
                {links.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  links.map((l) => {
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
                    return (
                      <tr key={l.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{l.title || l.name}</span>
                              {l.category_name && <span class="volo-mini-tag">{l.category_name}</span>}
                            </div>
                            {l.description && <div class="volo-cell-meta"><span>{l.description}</span></div>}
                          </div>
                        </td>
                        <td class="col-fluid">
                          <a href={l.url} target="_blank" rel="noreferrer" class="volo-code-chip" style={{ textDecoration: "none" }}>{l.url}</a>
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
                            <a class="volo-act-btn" href={l.url} target="_blank" rel="noreferrer">{t("common.view")}</a>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() =>
                                setLinkForm({
                                  id: l.id,
                                  title: l.title || "",
                                  url: l.url || "",
                                  icon_url: l.icon_url || "",
                                  description: l.description || "",
                                  category_name: l.category_name || "",
                                  sort_order: l.sort_order ?? 1,
                                })
                              }
                            >
                              {t("common.edit")}
                            </button>
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/links/${l.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("common.delete")}</button>
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {sub === "follows" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.follows")}</h3>
              <p class="volo-sec-desc">{t("dist.follows_desc")}</p>
            </div>
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
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              if (!followForm.id) {
                await apiFetch("/api/admin/follows", { method: "POST", body: JSON.stringify(followForm) });
              } else {
                await apiFetch(`/api/admin/follows/${followForm.id}`, { method: "PUT", body: JSON.stringify(followForm) });
              }
              setFollowForm({ id: "", title: "", site_url: "", feed_url: "", description: "" });
              showToast(t("common.saved"));
              void loadSub();
            }}
            class="volo-form-bar"
          >
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("dist.follows_name_ph")} value={followForm.title} onInput={(e) => setFollowForm({ ...followForm, title: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" required placeholder={t("dist.follows_feed_ph")} value={followForm.feed_url} onInput={(e) => setFollowForm({ ...followForm, feed_url: (e.currentTarget as HTMLInputElement).value })} />
              <div style={{ display: "flex", gap: "8px" }}>
                <input class="volo-input" placeholder={t("dist.follows_site_ph")} value={followForm.site_url} onInput={(e) => setFollowForm({ ...followForm, site_url: (e.currentTarget as HTMLInputElement).value })} />
                {followForm.id && (
                  <button type="button" class="volo-btn" onClick={() => setFollowForm({ id: "", title: "", site_url: "", feed_url: "", description: "" })}>
                    {t("common.cancel")}
                  </button>
                )}
                <button type="submit" class="volo-btn volo-btn-primary">
                  {followForm.id ? t("common.save") : t("common.create")}
                </button>
              </div>
            </div>
          </form>
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
                {follows.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  follows.map((f) => (
                    <tr key={f.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">{f.title}</div>
                          {f.site_url && <div class="volo-cell-meta"><span>{f.site_url}</span></div>}
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
                          <button type="button" class="volo-act-btn" onClick={() => setFollowForm({ ...f })}>{t("common.edit")}</button>
                          <button type="button" class="volo-act-btn danger" onClick={async () => {
                            await apiFetch(`/api/admin/follows/${f.id}`, { method: "DELETE" });
                            void loadSub();
                          }}>{t("common.delete")}</button>
                        </div>
                      </td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {sub === "webhooks" && isAdmin && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("dist.wh_head")}</h3>
              <p class="volo-sec-desc">{t("dist.wh_desc")}</p>
            </div>
          </div>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              if (!whForm.id) {
                await apiFetch("/api/admin/webhooks", {
                  method: "POST",
                  body: JSON.stringify(whForm),
                });
              } else {
                await apiFetch(`/api/admin/webhooks/${whForm.id}`, {
                  method: "PUT",
                  body: JSON.stringify(whForm),
                });
              }
              setWhForm({
                id: "",
                name: "",
                provider: "custom",
                url: "",
                chat_id: "",
                secret: "",
                events: ["article.published", "comment.created"],
                is_enabled: true,
              });
              showToast(t("common.saved"));
              void loadSub();
            }}
            class="volo-form-bar"
          >
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("dist.wh_name_ph")} value={whForm.name} onInput={(e) => setWhForm({ ...whForm, name: (e.currentTarget as HTMLInputElement).value })} />
              <select class="volo-select" value={whForm.provider} onChange={(e) => setWhForm({ ...whForm, provider: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="custom">{t("dist.wh_custom")}</option>
                <option value="feishu">{t("dist.wh_feishu")}</option>
                <option value="dingtalk">{t("dist.wh_dingtalk")}</option>
                <option value="wecom">{t("dist.wh_wecom")}</option>
                <option value="bark">{t("dist.wh_bark")}</option>
                <option value="telegram">{t("dist.wh_telegram")}</option>
              </select>
              <input class="volo-input" placeholder={t("dist.wh_secret_ph")} value={whForm.secret} onInput={(e) => setWhForm({ ...whForm, secret: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <input class="volo-input" required placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/..." value={whForm.url} onInput={(e) => setWhForm({ ...whForm, url: (e.currentTarget as HTMLInputElement).value })} />
              {whForm.provider === "telegram" && (
                <input
                  class="volo-input"
                  style={{ maxWidth: "200px" }}
                  placeholder={t("dist.wh_chat_id_ph")}
                  value={whForm.chat_id}
                  onInput={(e) => setWhForm({ ...whForm, chat_id: (e.currentTarget as HTMLInputElement).value })}
                />
              )}
              {whForm.id && (
                <button
                  type="button"
                  class="volo-btn"
                  onClick={() =>
                    setWhForm({
                      id: "",
                      name: "",
                      provider: "custom",
                      url: "",
                      chat_id: "",
                      secret: "",
                      events: ["article.published", "comment.created"],
                      is_enabled: true,
                    })
                  }
                >
                  {t("common.cancel")}
                </button>
              )}
              <button type="submit" class="volo-btn volo-btn-primary">
                {whForm.id ? t("common.save") : t("common.create")}
              </button>
            </div>
            <div class="volo-check-grid" style={{ marginBottom: 0 }}>
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
                  <span>{t(ev.labelKey)}</span>
                </label>
              ))}
            </div>
          </form>
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
                {webhooks.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  webhooks.map((item) => {
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
                          <span class="volo-cat-chip">{t(`dist.wh_${prov}`)}</span>
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
                            <button type="button" class="volo-act-btn" onClick={async () => {
                              try {
                                const r = await apiFetch<{ status_code: number }>(`/api/admin/webhooks/${w.id}/test`, { method: "POST", body: "{}" });
                                showToast(t("dist.wh_tested", { code: r.status_code }));
                                void loadSub();
                              } catch (err: any) {
                                showToast(err.message);
                              }
                            }}>{t("dist.wh_test")}</button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() =>
                                setWhForm({
                                  id: w.id,
                                  name: w.name || "",
                                  provider: prov,
                                  url: w.url || "",
                                  chat_id: w.chat_id || "",
                                  secret: "",
                                  events: evList.length > 0 ? evList : ["article.published", "comment.created"],
                                  is_enabled: w.is_enabled !== false,
                                })
                              }
                            >
                              {t("common.edit")}
                            </button>
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/webhooks/${w.id}`, { method: "DELETE" });
                              void loadSub();
                            }}>{t("common.delete")}</button>
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>
        </div>
      )}

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
                <select
                  class="volo-select"
                  value={feedCfg.output_mode || "summary"}
                  onChange={(e) => setFeedCfg({ ...feedCfg, output_mode: (e.currentTarget as HTMLSelectElement).value })}
                >
                  <option value="summary">{t("dist.feed_mode_summary")}</option>
                  <option value="full">{t("dist.feed_mode_full")}</option>
                </select>
              </div>
              <div class="volo-field">
                <label>{t("dist.feed_count")}</label>
                <input type="number" class="volo-input" value={feedCfg.item_count ?? 20} onInput={(e) => setFeedCfg({ ...feedCfg, item_count: Number((e.currentTarget as HTMLInputElement).value) })} />
              </div>
            </div>

            <div class="volo-check-grid">
              <label class="volo-check-item">
                <input type="checkbox" checked={!!feedCfg.enable_llms_txt} onChange={(e) => setFeedCfg({ ...feedCfg, enable_llms_txt: (e.currentTarget as HTMLInputElement).checked })} />
                <span>{t("dist.feed_llms")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!feedCfg.enable_raw_markdown} onChange={(e) => setFeedCfg({ ...feedCfg, enable_raw_markdown: (e.currentTarget as HTMLInputElement).checked })} />
                <span>{t("dist.feed_raw_md")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!feedCfg.podcast?.enabled} onChange={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), enabled: (e.currentTarget as HTMLInputElement).checked } })} />
                <span>{t("dist.feed_podcast")}</span>
              </label>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!feedCfg.podcast?.explicit} onChange={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), explicit: (e.currentTarget as HTMLInputElement).checked } })} />
                <span>{t("dist.podcast_explicit")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">{t("dist.podcast_meta")}</div>
              <div class="volo-grid-2">
                <input class="volo-input" placeholder={t("dist.podcast_title_ph")} value={feedCfg.podcast?.title || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), title: (e.currentTarget as HTMLInputElement).value } })} />
                <input class="volo-input" placeholder={t("dist.podcast_author_ph")} value={feedCfg.podcast?.author || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), author: (e.currentTarget as HTMLInputElement).value } })} />
                <input class="volo-input" placeholder={t("dist.podcast_email_ph")} value={feedCfg.podcast?.owner_email || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), owner_email: (e.currentTarget as HTMLInputElement).value } })} />
                <input class="volo-input" placeholder={t("dist.podcast_cat_ph")} value={feedCfg.podcast?.category || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), category: (e.currentTarget as HTMLInputElement).value } })} />
              </div>
              <input class="volo-input" style={{ marginTop: "8px" }} placeholder={t("dist.podcast_cover_ph")} value={feedCfg.podcast?.cover_image_url || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), cover_image_url: (e.currentTarget as HTMLInputElement).value } })} />
              <textarea class="volo-textarea" style={{ marginTop: "8px" }} rows={2} placeholder={t("dist.podcast_sum_ph")} value={feedCfg.podcast?.summary || ""} onInput={(e) => setFeedCfg({ ...feedCfg, podcast: { ...(feedCfg.podcast || {}), summary: (e.currentTarget as HTMLTextAreaElement).value } })} />
            </div>

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

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("dist.seo_head")}</h3>
                <p class="volo-sec-desc">{t("dist.seo_sub")}</p>
              </div>
            </div>
            <div class="volo-field">
              <label>{t("dist.seo_kw")}</label>
              <input class="volo-input" value={seoCfg.meta_keywords || ""} onInput={(e) => setSeoCfg({ ...seoCfg, meta_keywords: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div class="volo-field">
              <label>{t("dist.seo_desc")}</label>
              <textarea class="volo-textarea" rows={2} value={seoCfg.meta_description || ""} onInput={(e) => setSeoCfg({ ...seoCfg, meta_description: (e.currentTarget as HTMLTextAreaElement).value })} />
            </div>
            <div class="volo-field">
              <label>{t("dist.seo_og")}</label>
              <input class="volo-input" value={seoCfg.default_og_image_url || ""} onInput={(e) => setSeoCfg({ ...seoCfg, default_og_image_url: (e.currentTarget as HTMLInputElement).value })} />
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
              <div style={{ display: "flex", gap: "8px", marginBottom: "8px" }}>
                <input class="volo-input" placeholder={t("dist.seo_indexnow_key")} value={seoCfg.indexnow_key || ""} onInput={(e) => setSeoCfg({ ...seoCfg, indexnow_key: (e.currentTarget as HTMLInputElement).value })} />
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setSeoCfg({ ...seoCfg, indexnow_key: genRandomHexKey() })}>
                  {t("dist.seo_gen_key")}
                </button>
              </div>
              <label class="volo-check-item">
                <input type="checkbox" checked={!!seoCfg.indexnow_enabled} onChange={(e) => setSeoCfg({ ...seoCfg, indexnow_enabled: (e.currentTarget as HTMLInputElement).checked })} />
                <span>{t("dist.seo_indexnow_on")}</span>
              </label>
            </div>

            <div class="volo-subcard">
              <div class="volo-subcard-title">
                <span>{t("dist.seo_gh_title")}</span>
                <button
                  type="button"
                  class="volo-act-btn"
                  onClick={async () => {
                    try {
                      const r = await apiFetch<{ repos: any[] }>("/api/admin/seo/github-sync", {
                        method: "POST",
                        body: JSON.stringify({ username: seoCfg.github_username || "" }),
                      });
                      showToast(t("dist.synced_gh", { count: r.repos?.length || 0 }));
                    } catch (err: any) {
                      showToast(err.message);
                    }
                  }}
                >
                  {t("dist.sync_gh")}
                </button>
              </div>
              <input class="volo-input" placeholder={t("dist.seo_gh_user")} value={seoCfg.github_username || ""} onInput={(e) => setSeoCfg({ ...seoCfg, github_username: (e.currentTarget as HTMLInputElement).value })} />
              <label class="volo-check-item" style={{ marginTop: "8px" }}>
                <input type="checkbox" checked={!!seoCfg.github_include_forks} onChange={(e) => setSeoCfg({ ...seoCfg, github_include_forks: (e.currentTarget as HTMLInputElement).checked })} />
                <span>{t("dist.seo_gh_forks")}</span>
              </label>
            </div>

            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                const res = await apiFetch<any>("/api/admin/settings/seo", {
                  method: "PATCH",
                  body: JSON.stringify({ expected_version: seoVer, ...seoCfg }),
                });
                setSeoCfg(res.value || seoCfg);
                setSeoVer(res.version);
                showToast(t("common.saved"));
              }}
            >
              {t("dist.save_seo")}
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
