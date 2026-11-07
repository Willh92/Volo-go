import { useEffect, useState } from "preact/hooks";
import { apiFetch, showToast } from "../api";
import { CM6Editor } from "../components/CM6Editor";
import { availableLocalesSig, formatDateTime, localeSig, t } from "../i18n";

export type ContentSubTab = "pages" | "categories" | "tags" | "series" | "books" | "comments" | "media";

const EMPTY_BOOK = {
  id: "",
  title: "",
  slug: "",
  description: "",
  cover_url: "",
  source_type: "auto",
  source_url: "",
  series_id: "",
  embed_mode: "iframe",
  sort_order: 1,
};

export function ContentWorkspace({
  activeTab = "categories",
  onNavigate,
}: {
  activeTab?: ContentSubTab;
  onNavigate?: (sec: string) => void;
}) {
  const [sub, setSub] = useState<ContentSubTab>(activeTab);
  const locales = availableLocalesSig.value;

  // Pages
  const [pages, setPages] = useState<any[]>([]);
  const [editPage, setEditPage] = useState<any | null>(null);

  // Categories, Tags, Series, Books
  const [categories, setCategories] = useState<any[]>([]);
  const [catForm, setCatForm] = useState({ id: "", name: "", slug: "", description: "", sort_order: 1 });
  const [tags, setTags] = useState<any[]>([]);
  const [tagName, setTagName] = useState("");
  const [tagFilter, setTagFilter] = useState("");
  const [series, setSeries] = useState<any[]>([]);
  const [serForm, setSerForm] = useState({ id: "", title: "", slug: "", description: "", sort_order: 1 });
  const [books, setBooks] = useState<any[]>([]);
  const [bookForm, setBookForm] = useState({ ...EMPTY_BOOK });

  // Comments & Media
  const [comments, setComments] = useState<any[]>([]);
  const [cmtStatus, setCmtStatus] = useState<string>("");
  const [replyText, setReplyText] = useState<Record<string, string>>({});
  const [media, setMedia] = useState<any[]>([]);
  const [mediaQuery, setMediaQuery] = useState<string>("");
  const [orphans, setOrphans] = useState<any[] | null>(null);

  useEffect(() => {
    setSub(activeTab);
    setEditPage(null);
  }, [activeTab]);

  const loadSub = async () => {
    try {
      if (sub === "pages") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/pages");
        setPages(r.items || []);
      } else if (sub === "categories") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/categories");
        setCategories(r.items || []);
      } else if (sub === "tags") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/tags");
        setTags(r.items || []);
      } else if (sub === "series") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/series");
        setSeries(r.items || []);
      } else if (sub === "books") {
        const [bkRes, serRes] = await Promise.all([
          apiFetch<{ items: any[] }>("/api/admin/books"),
          apiFetch<{ items: any[] }>("/api/admin/series"),
        ]);
        setBooks(bkRes.items || []);
        setSeries(serRes.items || []);
      } else if (sub === "comments") {
        const r = await apiFetch<{ items: any[] }>(`/api/admin/comments?status=${encodeURIComponent(cmtStatus)}`);
        setComments(r.items || []);
      } else if (sub === "media") {
        const r = await apiFetch<{ items: any[] }>("/api/admin/media");
        setMedia(r.items || []);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [sub, cmtStatus]);

  const savePage = async (e: Event) => {
    e.preventDefault();
    if (!editPage) return;
    try {
      if (!editPage.id) {
        await apiFetch("/api/admin/pages", { method: "POST", body: JSON.stringify(editPage) });
      } else {
        await apiFetch(`/api/admin/pages/${editPage.id}`, { method: "PUT", body: JSON.stringify(editPage) });
      }
      setEditPage(null);
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveCategory = async (e: Event) => {
    e.preventDefault();
    try {
      if (!catForm.id) {
        await apiFetch("/api/admin/categories", { method: "POST", body: JSON.stringify(catForm) });
      } else {
        await apiFetch(`/api/admin/categories/${catForm.id}`, { method: "PUT", body: JSON.stringify(catForm) });
      }
      setCatForm({ id: "", name: "", slug: "", description: "", sort_order: 1 });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveSeries = async (e: Event) => {
    e.preventDefault();
    try {
      if (!serForm.id) {
        await apiFetch("/api/admin/series", { method: "POST", body: JSON.stringify(serForm) });
      } else {
        await apiFetch(`/api/admin/series/${serForm.id}`, { method: "PUT", body: JSON.stringify(serForm) });
      }
      setSerForm({ id: "", title: "", slug: "", description: "", sort_order: 1 });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveBook = async (e: Event) => {
    e.preventDefault();
    try {
      if (!bookForm.id) {
        await apiFetch("/api/admin/books", { method: "POST", body: JSON.stringify(bookForm) });
      } else {
        await apiFetch(`/api/admin/books/${bookForm.id}`, { method: "PUT", body: JSON.stringify(bookForm) });
      }
      setBookForm({ ...EMPTY_BOOK });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const cmtStatusLabel = (st: string) => {
    if (st === "approved") return t("cnt.cmt_approved");
    if (st === "pending") return t("cnt.cmt_pending");
    if (st === "rejected") return t("cnt.cmt_rejected");
    return st;
  };

  const filteredTags = tagFilter.trim()
    ? tags.filter((tg) => String(tg.name || "").toLowerCase().includes(tagFilter.trim().toLowerCase()))
    : tags;

  const filteredMedia = mediaQuery.trim()
    ? media.filter((m) => {
        const q = mediaQuery.trim().toLowerCase();
        const name = String(m.original_name || m.object_key || "").toLowerCase();
        const url = String(m.public_url || "").toLowerCase();
        return name.includes(q) || url.includes(q);
      })
    : media;

  return (
    <div>
      {sub !== "comments" && (
        <div class="volo-subtabs">
          {(["categories", "tags", "series", "books", "pages", "media"] as ContentSubTab[]).map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => {
                setSub(k);
                onNavigate?.(k);
              }}
            >
              {t(`cnt.${k}`)}
            </button>
          ))}
        </div>
      )}

      {sub === "pages" && (
        <div class="volo-card">
          {editPage ? (
            <form onSubmit={savePage}>
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("cnt.page_editor_title", { slug: editPage.slug || "about" })}</h3>
                  <p class="volo-sec-desc">{t("cnt.page_desc")}</p>
                </div>
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setEditPage(null)}>
                  {t("common.cancel")}
                </button>
              </div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("art.title")}</label>
                  <input class="volo-input" required value={editPage.title} onInput={(e) => setEditPage({ ...editPage, title: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.page_slug")}</label>
                  <input class="volo-input" placeholder="about" value={editPage.slug} onInput={(e) => setEditPage({ ...editPage, slug: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.page_locale_group")}</label>
                  <div style={{ display: "flex", gap: "6px" }}>
                    <select class="volo-select" value={editPage.lang || "zh-CN"} onChange={(e) => setEditPage({ ...editPage, lang: (e.currentTarget as HTMLSelectElement).value })}>
                      {locales.map((loc) => (
                        <option key={loc.code} value={loc.code}>{loc.name} ({loc.code})</option>
                      ))}
                    </select>
                    <input class="volo-input" placeholder="group-about" value={editPage.translation_group || ""} onInput={(e) => setEditPage({ ...editPage, translation_group: (e.currentTarget as HTMLInputElement).value })} />
                  </div>
                </div>
              </div>
              <CM6Editor value={editPage.content_md || ""} onChange={(md) => setEditPage({ ...editPage, content_md: md })} />
              <div class="volo-check-grid">
                <label class="volo-check-item">
                  <input type="checkbox" checked={editPage.is_published !== false} onChange={(e) => setEditPage({ ...editPage, is_published: (e.currentTarget as HTMLInputElement).checked })} />
                  <span>{t("cnt.page_published")}</span>
                </label>
                <label class="volo-check-item">
                  <input type="checkbox" checked={editPage.allow_comment !== false} onChange={(e) => setEditPage({ ...editPage, allow_comment: (e.currentTarget as HTMLInputElement).checked })} />
                  <span>{t("art.allow_comment")}</span>
                </label>
                <label class="volo-check-item">
                  <input type="checkbox" checked={!!editPage.show_toc} onChange={(e) => setEditPage({ ...editPage, show_toc: (e.currentTarget as HTMLInputElement).checked })} />
                  <span>{t("cnt.page_show_toc")}</span>
                </label>
              </div>
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.save")}</button>
            </form>
          ) : (
            <>
              <div class="volo-sec-head">
                <div class="volo-sec-copy">
                  <h3 class="volo-card-title">{t("cnt.pages")}</h3>
                  <p class="volo-sec-desc">{t("cnt.page_desc")}</p>
                </div>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  onClick={() => setEditPage({ title: "", slug: "", content_md: "", is_published: true, allow_comment: true, show_toc: false, lang: localeSig.value })}
                >
                  + {t("common.create")}
                </button>
              </div>
              <div class="volo-table-wrap">
                <table class="volo-table">
                  <thead>
                    <tr>
                      <th class="col-fluid">{t("cnt.page_col")}</th>
                      <th class="col-status">{t("art.status")}</th>
                      <th class="col-num">{t("art.metrics")}</th>
                      <th class="col-date">{t("art.updated")}</th>
                      <th class="col-act col-act-th">{t("art.actions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pages.length === 0 ? (
                      <tr>
                        <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                      </tr>
                    ) : (
                      pages.map((p) => (
                        <tr key={p.id}>
                          <td class="col-fluid">
                            <div class="volo-cell-main">
                              <div class="volo-cell-title">{p.title}</div>
                              <div class="volo-cell-meta">
                                <span class="volo-code-chip">/{p.slug}</span>
                                <span>{p.lang}</span>
                              </div>
                            </div>
                          </td>
                          <td class="col-status">
                            <span class={`volo-status-pill ${p.is_published ? "ok" : "warn"}`}>
                              <span class="volo-status-dot" />
                              {p.is_published ? t("cnt.page_published") : t("cnt.page_hidden")}
                            </span>
                          </td>
                          <td class="col-num">
                            <div class="volo-metric-pair">
                              <span class="volo-metric-item" title="PV">
                                <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
                                {p.view_count ?? 0}
                              </span>
                              <span class="volo-metric-item" title={t("nav.comments")}>
                                <svg width="12.5" height="12.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9"><path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
                                {p.comment_count ?? 0}
                              </span>
                            </div>
                          </td>
                          <td class="col-date">{formatDateTime(p.updated_at)}</td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <a class="volo-act-btn" href={`/${encodeURIComponent(p.slug)}`} target="_blank" rel="noreferrer">{t("common.view")}</a>
                              <button type="button" class="volo-act-btn" onClick={async () => {
                                const full = await apiFetch<{ page: any }>(`/api/admin/pages/${p.id}`);
                                setEditPage(full.page);
                              }}>{t("common.edit")}</button>
                              <button type="button" class="volo-act-btn danger" onClick={async () => {
                                await apiFetch(`/api/admin/pages/${p.id}`, { method: "DELETE" });
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
            </>
          )}
        </div>
      )}

      {sub === "categories" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.cat_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.cat_desc")}</p>
            </div>
          </div>
          <form onSubmit={saveCategory} class="volo-form-bar">
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("cnt.cat_name_ph")} value={catForm.name} onInput={(e) => setCatForm({ ...catForm, name: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("cnt.cat_slug_ph")} value={catForm.slug} onInput={(e) => setCatForm({ ...catForm, slug: (e.currentTarget as HTMLInputElement).value })} />
              <div style={{ display: "flex", gap: "8px" }}>
                <input class="volo-input" placeholder={t("cnt.desc_ph")} value={catForm.description} onInput={(e) => setCatForm({ ...catForm, description: (e.currentTarget as HTMLInputElement).value })} />
                <button type="submit" class="volo-btn volo-btn-primary">{catForm.id ? t("common.save") : t("common.create")}</button>
                {catForm.id && (
                  <button
                    type="button"
                    class="volo-btn"
                    onClick={() => setCatForm({ id: "", name: "", slug: "", description: "", sort_order: 1 })}
                  >
                    {t("common.cancel")}
                  </button>
                )}
              </div>
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("cnt.col_slug")}</th>
                  <th class="col-num">{t("cnt.col_articles")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {categories.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  categories.map((c) => (
                    <tr key={c.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">{c.name}</div>
                          {c.description && <div class="volo-cell-meta"><span>{c.description}</span></div>}
                        </div>
                      </td>
                      <td class="col-cat">
                        <span class="volo-code-chip">/categories/{c.slug}</span>
                      </td>
                      <td class="col-num">
                        <span class="volo-cat-chip">{c.article_count ?? 0}</span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/categories/${encodeURIComponent(c.slug)}`} target="_blank" rel="noreferrer">{t("common.view")}</a>
                          <button type="button" class="volo-act-btn" onClick={() => setCatForm(c)}>{t("common.edit")}</button>
                          <button type="button" class="volo-act-btn danger" onClick={async () => {
                            await apiFetch(`/api/admin/categories/${c.id}`, { method: "DELETE" });
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

      {sub === "tags" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.tags")}</h3>
              <p class="volo-sec-desc">{t("cnt.tag_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <input
                class="volo-input"
                style={{ width: "200px" }}
                placeholder={t("cnt.tag_filter_ph")}
                value={tagFilter}
                onInput={(e) => setTagFilter((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
          </div>

          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/tags", { method: "POST", body: JSON.stringify({ name: tagName }) });
              setTagName("");
              void loadSub();
            }}
            class="volo-form-bar"
            style={{ display: "flex", gap: "8px", alignItems: "center" }}
          >
            <input class="volo-input" style={{ maxWidth: "320px" }} required placeholder={t("cnt.tag_new_ph")} value={tagName} onInput={(e) => setTagName((e.currentTarget as HTMLInputElement).value)} />
            <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
          </form>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("cnt.col_slug")}</th>
                  <th class="col-num">{t("cnt.col_articles")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {filteredTags.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  filteredTags.map((tg) => (
                    <tr key={tg.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-title">#{tg.name}</div>
                      </td>
                      <td class="col-cat">
                        <span class="volo-code-chip">/tags/{tg.slug || tg.name}</span>
                      </td>
                      <td class="col-num">
                        <span class="volo-cat-chip">{tg.article_count ?? 0}</span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/tags/${encodeURIComponent(tg.slug || tg.name)}`} target="_blank" rel="noreferrer">{t("common.view")}</a>
                          <button type="button" class="volo-act-btn danger" onClick={async () => {
                            await apiFetch(`/api/admin/tags/${tg.id}`, { method: "DELETE" });
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

      {sub === "series" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.ser_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.ser_desc")}</p>
            </div>
          </div>
          <form onSubmit={saveSeries} class="volo-form-bar">
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("cnt.ser_title_ph")} value={serForm.title} onInput={(e) => setSerForm({ ...serForm, title: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("cnt.cat_slug_ph")} value={serForm.slug} onInput={(e) => setSerForm({ ...serForm, slug: (e.currentTarget as HTMLInputElement).value })} />
              <div style={{ display: "flex", gap: "8px" }}>
                <input class="volo-input" placeholder={t("cnt.desc_ph")} value={serForm.description} onInput={(e) => setSerForm({ ...serForm, description: (e.currentTarget as HTMLInputElement).value })} />
                <button type="submit" class="volo-btn volo-btn-primary">{serForm.id ? t("common.save") : t("common.create")}</button>
                {serForm.id && (
                  <button
                    type="button"
                    class="volo-btn"
                    onClick={() => setSerForm({ id: "", title: "", slug: "", description: "", sort_order: 1 })}
                  >
                    {t("common.cancel")}
                  </button>
                )}
              </div>
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("cnt.col_slug")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {series.length === 0 ? (
                  <tr>
                    <td colSpan={3} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  series.map((s) => (
                    <tr key={s.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">{s.title}</div>
                          {s.description && <div class="volo-cell-meta"><span>{s.description}</span></div>}
                        </div>
                      </td>
                      <td class="col-cat">
                        <span class="volo-code-chip">/series/{s.slug}</span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/series/${encodeURIComponent(s.slug)}`} target="_blank" rel="noreferrer">{t("common.view")}</a>
                          <button type="button" class="volo-act-btn" onClick={() => setSerForm(s)}>{t("common.edit")}</button>
                          <button type="button" class="volo-act-btn danger" onClick={async () => {
                            await apiFetch(`/api/admin/series/${s.id}`, { method: "DELETE" });
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

      {sub === "books" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.book_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.book_desc")}</p>
            </div>
          </div>
          <form onSubmit={saveBook} class="volo-form-bar">
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder={t("cnt.book_title_ph")} value={bookForm.title} onInput={(e) => setBookForm({ ...bookForm, title: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("cnt.cat_slug_ph")} value={bookForm.slug} onInput={(e) => setBookForm({ ...bookForm, slug: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder={t("cnt.book_cover_ph")} value={bookForm.cover_url} onInput={(e) => setBookForm({ ...bookForm, cover_url: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div class="volo-grid-3" style={{ marginTop: "8px" }}>
              <select
                class="volo-select"
                value={bookForm.source_type}
                onChange={(e) => setBookForm({ ...bookForm, source_type: (e.currentTarget as HTMLSelectElement).value })}
              >
                <option value="auto">{t("cnt.book_src_auto")}</option>
                <option value="gitbook">{t("cnt.book_src_gitbook")}</option>
                <option value="mdbook">{t("cnt.book_src_mdbook")}</option>
                <option value="vuepress">{t("cnt.book_src_vuepress")}</option>
                <option value="series">{t("cnt.book_src_series")}</option>
                <option value="manual">{t("cnt.book_src_manual")}</option>
              </select>
              {bookForm.source_type === "series" ? (
                <select
                  class="volo-select"
                  value={bookForm.series_id}
                  onChange={(e) => setBookForm({ ...bookForm, series_id: (e.currentTarget as HTMLSelectElement).value })}
                >
                  <option value="">{t("art.series_none")}</option>
                  {series.map((s) => (
                    <option key={s.id} value={s.id}>{s.title}</option>
                  ))}
                </select>
              ) : (
                <input
                  class="volo-input"
                  placeholder={t("cnt.book_toc_ph")}
                  value={bookForm.source_url}
                  onInput={(e) => setBookForm({ ...bookForm, source_url: (e.currentTarget as HTMLInputElement).value })}
                />
              )}
              <select
                class="volo-select"
                value={bookForm.embed_mode}
                onChange={(e) => setBookForm({ ...bookForm, embed_mode: (e.currentTarget as HTMLSelectElement).value })}
              >
                <option value="iframe">{t("cnt.book_embed_iframe")}</option>
                <option value="link">{t("cnt.book_embed_link")}</option>
              </select>
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <input class="volo-input" placeholder={t("cnt.book_notes_ph")} value={bookForm.description} onInput={(e) => setBookForm({ ...bookForm, description: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{bookForm.id ? t("common.save") : t("common.create")}</button>
              {bookForm.id && (
                <button type="button" class="volo-btn" onClick={() => setBookForm({ ...EMPTY_BOOK })}>
                  {t("common.cancel")}
                </button>
              )}
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.book_col")}</th>
                  <th class="col-cat">{t("art.status")}</th>
                  <th class="col-num">{t("cnt.col_articles")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {books.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  books.map((b) => (
                    <tr key={b.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">{b.title}</div>
                          <div class="volo-cell-meta">
                            <span class="volo-code-chip">/books/{b.slug}</span>
                            {b.description && <span>{b.description}</span>}
                            {(b.source_url || b.series_id) && (
                              <span class="volo-help">{t("cnt.book_source", { src: b.source_url || b.series_id })}</span>
                            )}
                          </div>
                        </div>
                      </td>
                      <td class="col-cat">
                        <span class="volo-cat-chip">{t(`cnt.book_src_${b.source_type || "auto"}`)}</span>
                      </td>
                      <td class="col-num">
                        <span class="volo-status-pill ok">
                          <span class="volo-status-dot" />
                          {t("cnt.book_chapters", { count: b.chapter_count ?? 0 })}
                        </span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/books/${encodeURIComponent(b.slug)}`} target="_blank" rel="noreferrer">{t("common.view")}</a>
                          {(b.source_url || b.series_id) && (
                            <button type="button" class="volo-act-btn" onClick={async () => {
                              try {
                                const r = await apiFetch<{ chapter_count: number }>(`/api/admin/books/${b.id}/sync`, { method: "POST", body: "{}" });
                                showToast(t("cnt.book_synced", { count: r.chapter_count ?? 0 }));
                                void loadSub();
                              } catch (err: any) {
                                showToast(err.message);
                              }
                            }}>{t("cnt.book_sync_toc")}</button>
                          )}
                          <button type="button" class="volo-act-btn" onClick={() => setBookForm({ ...EMPTY_BOOK, ...b })}>{t("common.edit")}</button>
                          <button type="button" class="volo-act-btn danger" onClick={async () => {
                            await apiFetch(`/api/admin/books/${b.id}`, { method: "DELETE" });
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

      {sub === "comments" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.cmt_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.cmt_desc")}</p>
            </div>
            <div class="volo-filter-pills">
              {[
                { val: "", label: t("cnt.cmt_all") },
                { val: "pending", label: t("cnt.cmt_pending") },
                { val: "approved", label: t("cnt.cmt_approved") },
                { val: "rejected", label: t("cnt.cmt_rejected") },
              ].map((it) => (
                <button
                  key={it.val}
                  type="button"
                  class={`volo-filter-pill ${cmtStatus === it.val ? "active" : ""}`}
                  onClick={() => setCmtStatus(it.val)}
                >
                  {it.label}
                </button>
              ))}
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.cmt_col_content")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-date">{t("art.updated")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {comments.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("cnt.cmt_empty")}</td>
                  </tr>
                ) : (
                  comments.map((row) => {
                    const c = row.comment || row;
                    return (
                      <tr key={c.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{c.author_name}</span>
                              {c.geo_region && <span class="volo-mini-tag">{c.geo_region}</span>}
                              {(c.client_ip || c.ip_masked) && (
                                <span class="volo-code-chip">{c.client_ip || c.ip_masked}</span>
                              )}
                              <span class="volo-help">{t("cnt.cmt_on", { target: row.target_title || c.target_id })}</span>
                            </div>
                            <div style={{ whiteSpace: "pre-wrap", fontSize: "13.5px", margin: "3px 0 6px" }}>{c.content_md}</div>
                            <div style={{ display: "flex", gap: "6px", maxWidth: "520px" }}>
                              <input
                                class="volo-input"
                                style={{ height: "30px", fontSize: "12.5px", padding: "0 10px" }}
                                placeholder={t("cnt.cmt_reply_ph")}
                                value={replyText[c.id] || ""}
                                onInput={(e) => setReplyText({ ...replyText, [c.id]: (e.currentTarget as HTMLInputElement).value })}
                              />
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  const txt = (replyText[c.id] || "").trim();
                                  if (!txt) return;
                                  await apiFetch(`/api/admin/comments/${c.id}/reply`, { method: "POST", body: JSON.stringify({ content_md: txt }) });
                                  setReplyText({ ...replyText, [c.id]: "" });
                                  showToast(t("cnt.cmt_replied"));
                                  void loadSub();
                                }}
                              >
                                {t("cnt.cmt_reply_btn")}
                              </button>
                            </div>
                          </div>
                        </td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${c.status === "approved" ? "ok" : c.status === "rejected" ? "danger" : "warn"}`}>
                            <span class="volo-status-dot" />
                            {cmtStatusLabel(c.status)}
                          </span>
                        </td>
                        <td class="col-date">{formatDateTime(c.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            {c.status !== "approved" && (
                              <button type="button" class="volo-act-btn" onClick={async () => {
                                await apiFetch(`/api/admin/comments/${c.id}`, { method: "PATCH", body: JSON.stringify({ status: "approved" }) });
                                void loadSub();
                              }}>{t("cnt.cmt_approve")}</button>
                            )}
                            {c.status !== "rejected" && (
                              <button type="button" class="volo-act-btn" onClick={async () => {
                                await apiFetch(`/api/admin/comments/${c.id}`, { method: "PATCH", body: JSON.stringify({ status: "rejected" }) });
                                void loadSub();
                              }}>{t("cnt.cmt_reject")}</button>
                            )}
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/comments/${c.id}`, { method: "DELETE" });
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

      {sub === "media" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.media_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.media_desc")}</p>
            </div>
            <div class="volo-filter-controls">
              <input
                class="volo-input"
                style={{ width: "200px" }}
                placeholder={t("cnt.media_search_ph")}
                value={mediaQuery}
                onInput={(e) => setMediaQuery((e.currentTarget as HTMLInputElement).value)}
              />
              <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
                {t("cnt.media_upload")}
                <input
                  type="file"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f) return;
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch("/api/admin/media/upload", { method: "POST", body: fd });
                    showToast(t("cnt.media_uploaded"));
                    void loadSub();
                    (e.currentTarget as HTMLInputElement).value = "";
                  }}
                />
              </label>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={async () => {
                  const res = await apiFetch<{ orphans: any[] }>("/api/admin/media/orphans", {
                    method: "POST",
                    body: "{}",
                  });
                  const list = res.orphans || [];
                  setOrphans(list);
                  showToast(t("cnt.media_found_orphans", { count: list.length }));
                }}
              >
                {t("cnt.media_scan_orphans")}
              </button>
              {orphans && orphans.length > 0 && (
                <button
                  type="button"
                  class="volo-btn volo-btn-sm volo-btn-danger"
                  onClick={async () => {
                    const res = await apiFetch<{ orphans: any[] }>("/api/admin/media/orphans?delete=1", {
                      method: "POST",
                      body: "{}",
                    });
                    const cleaned = res.orphans?.length || orphans.length;
                    setOrphans(null);
                    showToast(t("cnt.media_cleaned_orphans", { count: cleaned }));
                    void loadSub();
                  }}
                >
                  {t("cnt.media_clean_orphans", { count: orphans.length })}
                </button>
              )}
            </div>
          </div>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("cnt.media_col_asset")}</th>
                  <th class="col-cat">{t("cnt.media_col_mime")}</th>
                  <th class="col-num">{t("cnt.media_col_size")}</th>
                  <th class="col-date">{t("cnt.media_col_uploaded")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {filteredMedia.length === 0 ? (
                  <tr>
                    <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  filteredMedia.map((m) => {
                    const displayName = m.original_name || m.orig_filename || m.object_key || m.storage_key || m.id;
                    const isImage = String(m.mime_type || "").startsWith("image/");
                    return (
                      <tr key={m.id}>
                        <td class="col-fluid">
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <a href={m.public_url} target="_blank" rel="noreferrer">{displayName}</a>
                              {m.storage_driver && <span class="volo-mini-tag">{m.storage_driver}</span>}
                            </div>
                            <div class="volo-cell-meta">
                              <span class="volo-code-chip">{m.public_url}</span>
                            </div>
                          </div>
                        </td>
                        <td class="col-cat">
                          <div class="volo-cell-main">
                            <span class="volo-cat-chip">{m.mime_type}</span>
                            {m.width > 0 && <span class="volo-help">{m.width}×{m.height}</span>}
                          </div>
                        </td>
                        <td class="col-num">{((m.size_bytes || 0) / 1024).toFixed(1)} KB</td>
                        <td class="col-date">{formatDateTime(m.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button type="button" class="volo-act-btn" onClick={() => {
                              void navigator.clipboard?.writeText(m.public_url);
                              showToast(t("cnt.media_copied"));
                            }}>{t("cnt.media_copy")}</button>
                            <button type="button" class="volo-act-btn" onClick={() => {
                              const md = isImage ? `![${displayName}](${m.public_url})` : `[${displayName}](${m.public_url})`;
                              void navigator.clipboard?.writeText(md);
                              showToast(t("cnt.media_copied"));
                            }}>{t("cnt.media_copy_md")}</button>
                            <button type="button" class="volo-act-btn danger" onClick={async () => {
                              await apiFetch(`/api/admin/media/${m.id}`, { method: "DELETE" });
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
    </div>
  );
}
