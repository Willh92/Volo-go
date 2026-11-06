import { useEffect, useState } from "preact/hooks";
import { apiFetch, showToast } from "../api";
import { CM6Editor } from "../components/CM6Editor";
import { formatDateTime, t } from "../i18n";

type SubTab = "pages" | "categories" | "tags" | "series" | "books" | "comments" | "media";

export function ContentWorkspace() {
  const [sub, setSub] = useState<SubTab>("pages");

  // Pages
  const [pages, setPages] = useState<any[]>([]);
  const [editPage, setEditPage] = useState<any | null>(null);

  // Categories & Tags & Series & Books
  const [categories, setCategories] = useState<any[]>([]);
  const [catForm, setCatForm] = useState({ id: "", name: "", slug: "", description: "", sort_order: 1 });
  const [tags, setTags] = useState<any[]>([]);
  const [tagName, setTagName] = useState("");
  const [series, setSeries] = useState<any[]>([]);
  const [serForm, setSerForm] = useState({ id: "", title: "", slug: "", description: "", sort_order: 1 });
  const [books, setBooks] = useState<any[]>([]);
  const [bookForm, setBookForm] = useState({
    id: "",
    title: "",
    slug: "",
    author: "",
    status: "reading",
    rating: 5,
    source_toc_url: "",
    notes_md: "",
  });

  // Comments & Media
  const [comments, setComments] = useState<any[]>([]);
  const [cmtStatus, setCmtStatus] = useState<string>("");
  const [replyText, setReplyText] = useState<Record<string, string>>({});
  const [media, setMedia] = useState<any[]>([]);
  const [orphans, setOrphans] = useState<any[] | null>(null);

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
        const r = await apiFetch<{ items: any[] }>("/api/admin/books");
        setBooks(r.items || []);
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
      setBookForm({ id: "", title: "", slug: "", author: "", status: "reading", rating: 5, source_toc_url: "", notes_md: "" });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  return (
    <div>
      <div class="volo-subtabs">
        {(["pages", "categories", "tags", "series", "books", "comments", "media"] as SubTab[]).map((k) => (
          <button key={k} type="button" class={`volo-tab ${sub === k ? "active" : ""}`} onClick={() => setSub(k)}>
            {t(`cnt.${k}`)}
          </button>
        ))}
      </div>

      {sub === "pages" && (
        <div class="volo-card">
          {editPage ? (
            <form onSubmit={savePage}>
              <div class="volo-card-head">
                <h3 class="volo-card-title">Standalone Markdown Page (`/{editPage.slug || "about"}`)</h3>
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setEditPage(null)}>
                  {t("common.cancel")}
                </button>
              </div>
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>Title</label>
                  <input class="volo-input" required value={editPage.title} onInput={(e) => setEditPage({ ...editPage, title: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>Slug (decoupled from Nav & Archive)</label>
                  <input class="volo-input" placeholder="about" value={editPage.slug} onInput={(e) => setEditPage({ ...editPage, slug: (e.currentTarget as HTMLInputElement).value })} />
                </div>
                <div class="volo-field">
                  <label>Locale / Translation Group</label>
                  <div style={{ display: "flex", gap: "6px" }}>
                    <select class="volo-select" value={editPage.lang || "zh-CN"} onChange={(e) => setEditPage({ ...editPage, lang: (e.currentTarget as HTMLSelectElement).value })}>
                      <option value="zh-CN">zh-CN</option>
                      <option value="en-US">en-US</option>
                    </select>
                    <input class="volo-input" placeholder="group-about" value={editPage.translation_group || ""} onInput={(e) => setEditPage({ ...editPage, translation_group: (e.currentTarget as HTMLInputElement).value })} />
                  </div>
                </div>
              </div>
              <CM6Editor value={editPage.content_md || ""} onChange={(md) => setEditPage({ ...editPage, content_md: md })} />
              <div style={{ display: "flex", gap: "18px", margin: "12px 0" }}>
                <label><input type="checkbox" checked={editPage.is_published !== false} onChange={(e) => setEditPage({ ...editPage, is_published: (e.currentTarget as HTMLInputElement).checked })} /> Published</label>
                <label><input type="checkbox" checked={editPage.allow_comment !== false} onChange={(e) => setEditPage({ ...editPage, allow_comment: (e.currentTarget as HTMLInputElement).checked })} /> Allow Comments</label>
                <label><input type="checkbox" checked={!!editPage.show_toc} onChange={(e) => setEditPage({ ...editPage, show_toc: (e.currentTarget as HTMLInputElement).checked })} /> Show TOC</label>
              </div>
              <button type="submit" class="volo-btn volo-btn-primary">{t("common.save")}</button>
            </form>
          ) : (
            <>
              <div class="volo-card-head">
                <h3 class="volo-card-title">{t("cnt.pages")}</h3>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary volo-btn-sm"
                  onClick={() => setEditPage({ title: "", slug: "", content_md: "", is_published: true, allow_comment: true, show_toc: false, lang: "zh-CN" })}
                >
                  + {t("common.create")}
                </button>
              </div>
              <div class="volo-table-wrap">
                <table class="volo-table">
                  <thead>
                    <tr>
                      <th class="col-fluid">Page</th>
                      <th class="col-status">Status</th>
                      <th class="col-num">Views / Comments</th>
                      <th class="col-date">Updated</th>
                      <th class="col-act">Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pages.map((p) => (
                      <tr key={p.id}>
                        <td class="col-fluid">
                          <strong>{p.title}</strong> <span class="volo-help">/{p.slug} ({p.lang})</span>
                        </td>
                        <td class="col-status">
                          <span class={`volo-badge ${p.is_published ? "volo-badge-ok" : "volo-badge-warn"}`}>
                            {p.is_published ? "published" : "hidden"}
                          </span>
                        </td>
                        <td class="col-num">{p.view_count ?? 0} / {p.comment_count ?? 0}</td>
                        <td class="col-date">{formatDateTime(p.updated_at)}</td>
                        <td class="col-act">
                          <a class="volo-btn volo-btn-sm" href={`/${encodeURIComponent(p.slug)}`} target="_blank" rel="noreferrer">View</a>{" "}
                          <button type="button" class="volo-btn volo-btn-sm" onClick={async () => {
                            const full = await apiFetch<{ page: any }>(`/api/admin/pages/${p.id}`);
                            setEditPage(full.page);
                          }}>{t("common.edit")}</button>{" "}
                          <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                            await apiFetch(`/api/admin/pages/${p.id}`, { method: "DELETE" });
                            void loadSub();
                          }}>×</button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
        </div>
      )}

      {sub === "categories" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("cnt.categories")} (Direct Article Binding)</h3>
          <form onSubmit={saveCategory} class="volo-grid-3" style={{ marginBottom: "16px" }}>
            <input class="volo-input" required placeholder="Category Name" value={catForm.name} onInput={(e) => setCatForm({ ...catForm, name: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" placeholder="slug (optional)" value={catForm.slug} onInput={(e) => setCatForm({ ...catForm, slug: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "8px" }}>
              <input class="volo-input" placeholder="Description" value={catForm.description} onInput={(e) => setCatForm({ ...catForm, description: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{catForm.id ? t("common.save") : t("common.create")}</button>
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead><tr><th class="col-fluid">Name</th><th class="col-cat">Slug</th><th class="col-num">Articles</th><th class="col-act">Actions</th></tr></thead>
              <tbody>
                {categories.map((c) => (
                  <tr key={c.id}>
                    <td class="col-fluid"><strong>{c.name}</strong> <span class="volo-help">{c.description}</span></td>
                    <td class="col-cat">{c.slug}</td>
                    <td class="col-num">{c.article_count ?? 0}</td>
                    <td class="col-act">
                      <button type="button" class="volo-btn volo-btn-sm" onClick={() => setCatForm(c)}>{t("common.edit")}</button>{" "}
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                        await apiFetch(`/api/admin/categories/${c.id}`, { method: "DELETE" });
                        void loadSub();
                      }}>×</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {sub === "tags" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("cnt.tags")}</h3>
          <form
            onSubmit={async (e) => {
              e.preventDefault();
              await apiFetch("/api/admin/tags", { method: "POST", body: JSON.stringify({ name: tagName }) });
              setTagName("");
              void loadSub();
            }}
            style={{ display: "flex", gap: "8px", marginBottom: "16px", maxWidth: "420px" }}
          >
            <input class="volo-input" required placeholder="New tag name" value={tagName} onInput={(e) => setTagName((e.currentTarget as HTMLInputElement).value)} />
            <button type="submit" class="volo-btn volo-btn-primary">{t("common.create")}</button>
          </form>
          <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
            {tags.map((tg) => (
              <span key={tg.id} class="volo-badge" style={{ padding: "6px 12px", gap: "8px" }}>
                #{tg.name} ({tg.article_count ?? 0})
                <button
                  type="button"
                  style={{ border: 0, background: "transparent", cursor: "pointer", color: "inherit", fontWeight: 700 }}
                  onClick={async () => {
                    await apiFetch(`/api/admin/tags/${tg.id}`, { method: "DELETE" });
                    void loadSub();
                  }}
                >
                  ×
                </button>
              </span>
            ))}
          </div>
        </div>
      )}

      {sub === "series" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("cnt.series")} (1st-Class Table)</h3>
          <form onSubmit={saveSeries} class="volo-grid-3" style={{ marginBottom: "16px" }}>
            <input class="volo-input" required placeholder="Series Title" value={serForm.title} onInput={(e) => setSerForm({ ...serForm, title: (e.currentTarget as HTMLInputElement).value })} />
            <input class="volo-input" placeholder="slug" value={serForm.slug} onInput={(e) => setSerForm({ ...serForm, slug: (e.currentTarget as HTMLInputElement).value })} />
            <div style={{ display: "flex", gap: "8px" }}>
              <input class="volo-input" placeholder="Description" value={serForm.description} onInput={(e) => setSerForm({ ...serForm, description: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{serForm.id ? t("common.save") : t("common.create")}</button>
            </div>
          </form>
          {series.map((s) => (
            <div key={s.id} style={{ display: "flex", justifyContent: "space-between", padding: "10px 0", borderBottom: "1px solid var(--volo-line)" }}>
              <div><strong>{s.title}</strong> <span class="volo-help">/series/{s.slug} — {s.description}</span></div>
              <div style={{ display: "flex", gap: "6px" }}>
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setSerForm(s)}>{t("common.edit")}</button>
                <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                  await apiFetch(`/api/admin/series/${s.id}`, { method: "DELETE" });
                  void loadSub();
                }}>×</button>
              </div>
            </div>
          ))}
        </div>
      )}

      {sub === "books" && (
        <div class="volo-card">
          <h3 class="volo-card-title" style={{ marginBottom: "12px" }}>{t("cnt.books")} (Bookshelf + SSRF-Safe TOC Crawler)</h3>
          <form onSubmit={saveBook} style={{ marginBottom: "16px" }}>
            <div class="volo-grid-3">
              <input class="volo-input" required placeholder="Book Title" value={bookForm.title} onInput={(e) => setBookForm({ ...bookForm, title: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder="Author" value={bookForm.author} onInput={(e) => setBookForm({ ...bookForm, author: (e.currentTarget as HTMLInputElement).value })} />
              <input class="volo-input" placeholder="External TOC URL (for 1-click crawl)" value={bookForm.source_toc_url} onInput={(e) => setBookForm({ ...bookForm, source_toc_url: (e.currentTarget as HTMLInputElement).value })} />
            </div>
            <div style={{ display: "flex", gap: "8px", marginTop: "8px" }}>
              <select class="volo-select" style={{ width: "140px" }} value={bookForm.status} onChange={(e) => setBookForm({ ...bookForm, status: (e.currentTarget as HTMLSelectElement).value })}>
                <option value="reading">Reading</option>
                <option value="finished">Finished</option>
                <option value="wishlist">Wishlist</option>
              </select>
              <input class="volo-input" placeholder="Reading notes..." value={bookForm.notes_md} onInput={(e) => setBookForm({ ...bookForm, notes_md: (e.currentTarget as HTMLInputElement).value })} />
              <button type="submit" class="volo-btn volo-btn-primary">{bookForm.id ? t("common.save") : t("common.create")}</button>
            </div>
          </form>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead><tr><th class="col-fluid">Book</th><th class="col-status">Status</th><th class="col-act">Actions</th></tr></thead>
              <tbody>
                {books.map((b) => (
                  <tr key={b.id}>
                    <td class="col-fluid"><strong>{b.title}</strong> <div class="volo-help">/books/{b.slug} {(b.source_url || b.series_id) ? `· Source: ${b.source_url || b.series_id}` : ""}</div></td>
                    <td class="col-status"><span class="volo-badge">{b.source_type || "auto"}</span></td>
                    <td class="col-act">
                      {(b.source_url || b.series_id) && (
                        <button type="button" class="volo-btn volo-btn-sm" onClick={async () => {
                          try {
                            const r = await apiFetch<{ chapter_count: number }>(`/api/admin/books/${b.id}/sync`, { method: "POST", body: "{}" });
                            showToast(`Synced ${r.chapter_count ?? 0} chapters`);
                            void loadSub();
                          } catch (err: any) {
                            showToast(err.message);
                          }
                        }}>Sync TOC</button>
                      )}{" "}
                      <button type="button" class="volo-btn volo-btn-sm" onClick={() => setBookForm(b)}>{t("common.edit")}</button>{" "}
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                        await apiFetch(`/api/admin/books/${b.id}`, { method: "DELETE" });
                        void loadSub();
                      }}>×</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {sub === "comments" && (
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("cnt.comments")} (Strict Author Isolation for Editors)</h3>
            <select class="volo-select" style={{ width: "160px" }} value={cmtStatus} onChange={(e) => setCmtStatus((e.currentTarget as HTMLSelectElement).value)}>
              <option value="">All Statuses</option>
              <option value="approved">Approved</option>
              <option value="pending">Pending</option>
              <option value="rejected">Rejected</option>
            </select>
          </div>
          {comments.length === 0 ? (
            <p class="volo-help">No comments matching current filter.</p>
          ) : (
            comments.map((row) => {
              const c = row.comment || row;
              return (
                <div key={c.id} class="volo-card" style={{ padding: "14px", marginBottom: "10px" }}>
                  <div style={{ display: "flex", justifyContent: "space-between", gap: "8px", marginBottom: "6px" }}>
                    <div>
                      <strong>{c.author_name}</strong>{" "}
                      <span class="volo-badge">{c.status}</span>{" "}
                      <span class="volo-help">
                        on {row.target_title || c.target_id} · {formatDateTime(c.created_at)}
                        {c.geo_region ? ` · ${c.geo_region}` : ""}
                        {c.client_ip ? ` · ${c.client_ip}` : ""}
                      </span>
                    </div>
                    <div style={{ display: "flex", gap: "6px" }}>
                      {c.status !== "approved" && (
                        <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={async () => {
                          await apiFetch(`/api/admin/comments/${c.id}`, { method: "PATCH", body: JSON.stringify({ status: "approved" }) });
                          void loadSub();
                        }}>Approve</button>
                      )}
                      {c.status !== "rejected" && (
                        <button type="button" class="volo-btn volo-btn-sm" onClick={async () => {
                          await apiFetch(`/api/admin/comments/${c.id}`, { method: "PATCH", body: JSON.stringify({ status: "rejected" }) });
                          void loadSub();
                        }}>Reject</button>
                      )}
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                        await apiFetch(`/api/admin/comments/${c.id}`, { method: "DELETE" });
                        void loadSub();
                      }}>Delete</button>
                    </div>
                  </div>
                  <div style={{ margin: "6px 0 10px", whiteSpace: "pre-wrap" }}>{c.content_md}</div>
                  <div style={{ display: "flex", gap: "8px" }}>
                    <input
                      class="volo-input"
                      placeholder="Reply as author..."
                      value={replyText[c.id] || ""}
                      onInput={(e) => setReplyText({ ...replyText, [c.id]: (e.currentTarget as HTMLInputElement).value })}
                    />
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      onClick={async () => {
                        const txt = (replyText[c.id] || "").trim();
                        if (!txt) return;
                        await apiFetch(`/api/admin/comments/${c.id}/reply`, { method: "POST", body: JSON.stringify({ content_md: txt }) });
                        setReplyText({ ...replyText, [c.id]: "" });
                        showToast("Reply posted");
                        void loadSub();
                      }}
                    >
                      Reply
                    </button>
                  </div>
                </div>
              );
            })
          )}
        </div>
      )}

      {sub === "media" && (
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("cnt.media")} (EXIF Privacy Strip + WebP + 9-Scope Orphan Scanner)</h3>
            <div style={{ display: "flex", gap: "8px" }}>
              <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
                Upload Asset
                <input
                  type="file"
                  style={{ display: "none" }}
                  onChange={async (e) => {
                    const f = (e.currentTarget as HTMLInputElement).files?.[0];
                    if (!f) return;
                    const fd = new FormData();
                    fd.append("file", f);
                    await apiFetch("/api/admin/media/upload", { method: "POST", body: fd });
                    showToast("Uploaded!");
                    void loadSub();
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
                  showToast(`Found ${list.length} unreferenced media file(s)`);
                }}
              >
                Scan Orphans
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
                    showToast(`Cleaned ${cleaned} orphan asset(s)`);
                    void loadSub();
                  }}
                >
                  Clean {orphans.length} Orphans
                </button>
              )}
            </div>
          </div>
          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">Asset</th>
                  <th class="col-cat">MIME / Dims</th>
                  <th class="col-num">Size</th>
                  <th class="col-date">Uploaded</th>
                  <th class="col-act">Actions</th>
                </tr>
              </thead>
              <tbody>
                {media.map((m) => (
                  <tr key={m.id}>
                    <td class="col-fluid">
                      <a href={m.public_url} target="_blank" rel="noreferrer" style={{ fontWeight: 600 }}>{m.orig_filename || m.storage_key}</a>
                      <div class="volo-help">{m.public_url}</div>
                    </td>
                    <td class="col-cat">{m.mime_type} {m.width > 0 ? `(${m.width}×${m.height})` : ""}</td>
                    <td class="col-num">{((m.size_bytes || 0) / 1024).toFixed(1)} KB</td>
                    <td class="col-date">{formatDateTime(m.created_at)}</td>
                    <td class="col-act">
                      <button type="button" class="volo-btn volo-btn-sm" onClick={() => {
                        void navigator.clipboard?.writeText(m.public_url);
                        showToast("Copied URL");
                      }}>Copy</button>{" "}
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={async () => {
                        await apiFetch(`/api/admin/media/${m.id}`, { method: "DELETE" });
                        void loadSub();
                      }}>×</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}
