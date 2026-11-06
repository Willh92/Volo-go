import { useEffect, useState } from "preact/hooks";
import { apiFetch, clearLocalDraft, loadLocalDraft, LocalDraft, saveLocalDraft, showToast } from "../api";
import { CM6Editor, QuickInsertItem } from "../components/CM6Editor";
import { formatDateTime, t } from "../i18n";

export function OverviewWorkspace() {
  const [overview, setOverview] = useState<any>(null);
  const [articles, setArticles] = useState<any[]>([]);
  const [categories, setCategories] = useState<any[]>([]);
  const [seriesList, setSeriesList] = useState<any[]>([]);
  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [query, setQuery] = useState<string>("");

  // Editor state (`null` = table mode, `""` = new post, `"id"` = editing post)
  const [editingID, setEditingID] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [slug, setSlug] = useState("");
  const [categoryID, setCategoryID] = useState("");
  const [tagsText, setTagsText] = useState("");
  const [seriesID, setSeriesID] = useState("");
  const [summaryMD, setSummaryMD] = useState("");
  const [contentMD, setContentMD] = useState("");
  const [coverURL, setCoverURL] = useState("");
  const [status, setStatus] = useState("published");
  const [isPinned, setIsPinned] = useState(false);
  const [allowComment, setAllowComment] = useState(true);
  const [password, setPassword] = useState("");
  const [clearPassword, setClearPassword] = useState(false);
  const [audioURL, setAudioURL] = useState("");
  const [audioDuration, setAudioDuration] = useState(0);
  const [audioMime, setAudioMime] = useState("audio/mpeg");
  const [lang, setLang] = useState("zh-CN");
  const [translationGroup, setTranslationGroup] = useState("");
  const [revisionNote, setRevisionNote] = useState("");

  const [pendingDraft, setPendingDraft] = useState<LocalDraft | null>(null);
  const [revisions, setRevisions] = useState<any[] | null>(null);
  const [shareURL, setShareURL] = useState<string>("");

  const loadAll = async () => {
    const [ov, arts, cats, sers] = await Promise.all([
      apiFetch<any>("/api/admin/overview").catch(() => null),
      apiFetch<{ items: any[] }>(
        `/api/admin/articles?status=${encodeURIComponent(statusFilter)}&q=${encodeURIComponent(query)}`
      ).catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/categories").catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/series").catch(() => ({ items: [] })),
    ]);
    if (ov) setOverview(ov);
    setArticles(arts.items || []);
    setCategories(cats.items || []);
    setSeriesList(sers.items || []);
  };

  useEffect(() => {
    void loadAll();
  }, [statusFilter]);

  const draftKey = editingID === "" ? "article:new" : editingID ? `article:${editingID}` : "";

  const startNewArticle = async () => {
    setEditingID("");
    setTitle("");
    setSlug("");
    setCategoryID(categories[0]?.id || "");
    setTagsText("");
    setSeriesID("");
    setSummaryMD("");
    setContentMD("");
    setCoverURL("");
    setStatus("published");
    setIsPinned(false);
    setAllowComment(true);
    setPassword("");
    setClearPassword(false);
    setAudioURL("");
    setAudioDuration(0);
    setLang("zh-CN");
    setTranslationGroup("");
    setRevisionNote("");
    setShareURL("");
    const saved = await loadLocalDraft("article:new");
    if (saved && (saved.title || saved.content_md)) {
      setPendingDraft(saved);
    } else {
      setPendingDraft(null);
    }
  };

  const openEditArticle = async (id: string) => {
    try {
      const data = await apiFetch<any>(`/api/admin/articles/${id}`);
      const a = data.article;
      setEditingID(a.id);
      setTitle(a.title || "");
      setSlug(a.slug || "");
      setCategoryID(a.category_id || "");
      setTagsText((data.tags || []).join(", "));
      setSeriesID(data.series_id || "");
      setSummaryMD(a.summary_md || "");
      setContentMD(a.content_md || "");
      setCoverURL(a.cover_url || "");
      setStatus(a.status || "published");
      setIsPinned(!!a.is_pinned);
      setAllowComment(a.allow_comment !== false);
      setPassword(data.password || "");
      setClearPassword(false);
      setAudioURL(a.audio_url || "");
      setAudioDuration(a.audio_duration_sec || 0);
      setAudioMime(a.audio_mime || "audio/mpeg");
      setLang(a.lang || "zh-CN");
      setTranslationGroup(a.translation_group || "");
      setRevisionNote("");
      setShareURL("");
      const saved = await loadLocalDraft(`article:${id}`);
      if (saved && saved.content_md && saved.content_md !== a.content_md) {
        setPendingDraft(saved);
      } else {
        setPendingDraft(null);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  // Auto-persist local draft to IndexedDB every 1.2s while editing
  useEffect(() => {
    if (editingID === null || !draftKey) return;
    const timer = window.setTimeout(() => {
      if (title || contentMD) {
        void saveLocalDraft(draftKey, {
          title,
          slug,
          summary_md: summaryMD,
          content_md: contentMD,
          saved_at: Date.now(),
        });
      }
    }, 1200);
    return () => window.clearTimeout(timer);
  }, [editingID, title, slug, summaryMD, contentMD]);

  const saveArticle = async (e: Event) => {
    e.preventDefault();
    try {
      const payload: any = {
        category_id: categoryID,
        title,
        slug,
        summary_md: summaryMD,
        content_md: contentMD,
        cover_url: coverURL,
        status,
        is_pinned: isPinned,
        allow_comment: allowComment,
        clear_password: clearPassword,
        audio_url: audioURL,
        audio_duration_sec: Number(audioDuration) || 0,
        audio_mime: audioMime,
        lang,
        translation_group: translationGroup,
        tags: tagsText
          .split(",")
          .map((s) => s.trim())
          .filter(Boolean),
        series_id: seriesID,
        revision_note: revisionNote,
      };
      if (!clearPassword) {
        payload.password = password;
      }
      if (editingID === "") {
        await apiFetch("/api/admin/articles", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/articles/${editingID}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      if (draftKey) await clearLocalDraft(draftKey);
      showToast(t("common.saved"));
      setEditingID(null);
      void loadAll();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const deleteArticle = async (id: string) => {
    if (!window.confirm("Delete this article permanently?")) return;
    try {
      await apiFetch(`/api/admin/articles/${id}`, { method: "DELETE" });
      showToast("Deleted");
      void loadAll();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const handleBatchImport = async (file: File) => {
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{ imported_count: number }>("/api/admin/articles/import", {
        method: "POST",
        body: fd,
      });
      showToast(`Imported ${res.imported_count} Markdown article(s)`);
      void loadAll();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const openRevisions = async () => {
    if (!editingID) return;
    try {
      const res = await apiFetch<{ items: any[] }>(`/api/admin/articles/${editingID}/revisions`);
      setRevisions(res.items || []);
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const restoreRevision = async (revID: string) => {
    if (!editingID) return;
    try {
      const res = await apiFetch<{ article: any }>(`/api/admin/articles/${editingID}/revisions/${revID}/restore`, {
        method: "POST",
        body: "{}",
      });
      setTitle(res.article.title || "");
      setSummaryMD(res.article.summary_md || "");
      setContentMD(res.article.content_md || "");
      setRevisions(null);
      showToast("Revision restored!");
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const issueShareLink = async () => {
    if (!editingID) return;
    try {
      const res = await apiFetch<{ share_url: string }>(`/api/admin/articles/${editingID}/share-token`, {
        method: "POST",
        body: JSON.stringify({ ttl_hours: 72 }),
      });
      setShareURL(res.share_url);
      showToast("72h Share URL generated!");
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const catNameMap: Record<string, string> = {};
  categories.forEach((c) => {
    catNameMap[c.id] = c.name;
  });

  if (editingID !== null) {
    const quickInserts: QuickInsertItem[] = overview?.quick_inserts || [];
    return (
      <div>
        <div class="volo-card">
          <div class="volo-card-head">
            <h2 class="volo-card-title">{editingID === "" ? t("art.new") : `${t("common.edit")}: ${title}`}</h2>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              {editingID !== "" && (
                <>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={openRevisions}>
                    {t("art.revisions")}
                  </button>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={issueShareLink}>
                    {t("art.share_token")}
                  </button>
                </>
              )}
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => setEditingID(null)}>
                {t("common.cancel")}
              </button>
            </div>
          </div>

          {pendingDraft && (
            <div class="volo-card" style={{ background: "var(--volo-accent-soft)", padding: "12px 16px" }}>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "12px" }}>
                <span>{t("art.draft_recovered")} ({formatDateTime(pendingDraft.saved_at)})</span>
                <div style={{ display: "flex", gap: "8px" }}>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm volo-btn-primary"
                    onClick={() => {
                      setTitle(pendingDraft.title);
                      setSlug(pendingDraft.slug);
                      setSummaryMD(pendingDraft.summary_md);
                      setContentMD(pendingDraft.content_md);
                      setPendingDraft(null);
                    }}
                  >
                    Restore
                  </button>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => {
                      if (draftKey) void clearLocalDraft(draftKey);
                      setPendingDraft(null);
                    }}
                  >
                    Discard
                  </button>
                </div>
              </div>
            </div>
          )}

          {shareURL && (
            <div class="volo-card" style={{ padding: "10px 14px", fontSize: "12.5px" }}>
              <strong>Timed Share Link (72h):</strong>{" "}
              <a href={shareURL} target="_blank" rel="noreferrer">{shareURL}</a>
            </div>
          )}

          <form onSubmit={saveArticle}>
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.title")}</label>
                <input class="volo-input" required value={title} onInput={(e) => setTitle((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.slug")}</label>
                <input class="volo-input" placeholder="auto-from-title" value={slug} onInput={(e) => setSlug((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.category")}</label>
                <select class="volo-select" value={categoryID} onChange={(e) => setCategoryID((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="">— Uncategorized —</option>
                  {categories.map((c) => (
                    <option key={c.id} value={c.id}>{c.name}</option>
                  ))}
                </select>
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>Free Tags (comma-separated)</label>
                <input class="volo-input" placeholder="Go, Architecture, Notes" value={tagsText} onInput={(e) => setTagsText((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Series</label>
                <select class="volo-select" value={seriesID} onChange={(e) => setSeriesID((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="">— None —</option>
                  {seriesList.map((s) => (
                    <option key={s.id} value={s.id}>{s.title}</option>
                  ))}
                </select>
              </div>
              <div class="volo-field">
                <label>{t("art.status")}</label>
                <select class="volo-select" value={status} onChange={(e) => setStatus((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="published">{t("art.published")}</option>
                  <option value="draft">{t("art.draft")}</option>
                  <option value="scheduled">{t("art.scheduled")}</option>
                </select>
              </div>
            </div>

            <div class="volo-field">
              <label>Markdown Body (CodeMirror 6)</label>
              <CM6Editor value={contentMD} onChange={setContentMD} quickInserts={quickInserts} />
            </div>

            <div class="volo-field" style={{ marginTop: "12px" }}>
              <label>Custom Excerpt / Summary (optional)</label>
              <textarea class="volo-textarea" rows={2} value={summaryMD} onInput={(e) => setSummaryMD((e.currentTarget as HTMLTextAreaElement).value)} />
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>Article Password (AES-256-GCM)</label>
                <input
                  type="text"
                  class="volo-input"
                  placeholder="Leave empty for public"
                  value={password}
                  onInput={(e) => {
                    setPassword((e.currentTarget as HTMLInputElement).value);
                    setClearPassword(false);
                  }}
                />
              </div>
              <div class="volo-field">
                <label>Language (`lang`)</label>
                <select class="volo-select" value={lang} onChange={(e) => setLang((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="zh-CN">zh-CN</option>
                  <option value="en-US">en-US</option>
                </select>
              </div>
              <div class="volo-field">
                <label>Translation Group (`hreflang` ID)</label>
                <input class="volo-input" placeholder="e.g. post-volo-arch" value={translationGroup} onInput={(e) => setTranslationGroup((e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>Cover Image URL</label>
                <input class="volo-input" value={coverURL} onInput={(e) => setCoverURL((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Podcast Audio URL (.mp3 / .m4a)</label>
                <input class="volo-input" value={audioURL} onInput={(e) => setAudioURL((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>Podcast Duration (sec) / Revision Note</label>
                <div style={{ display: "flex", gap: "6px" }}>
                  <input
                    type="number"
                    class="volo-input"
                    style={{ width: "95px" }}
                    value={audioDuration}
                    onInput={(e) => setAudioDuration(Number((e.currentTarget as HTMLInputElement).value) || 0)}
                  />
                  <input
                    class="volo-input"
                    placeholder="Revision note..."
                    value={revisionNote}
                    onInput={(e) => setRevisionNote((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
            </div>

            <div style={{ display: "flex", alignItems: "center", gap: "18px", margin: "8px 0 16px" }}>
              <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                <input type="checkbox" checked={isPinned} onChange={(e) => setIsPinned((e.currentTarget as HTMLInputElement).checked)} />
                Pin to Top
              </label>
              <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                <input type="checkbox" checked={allowComment} onChange={(e) => setAllowComment((e.currentTarget as HTMLInputElement).checked)} />
                Allow Comments
              </label>
              {editingID !== "" && (
                <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                  <input
                    type="checkbox"
                    checked={clearPassword}
                    onChange={(e) => {
                      setClearPassword((e.currentTarget as HTMLInputElement).checked);
                      if ((e.currentTarget as HTMLInputElement).checked) setPassword("");
                    }}
                  />
                  Clear Article Password
                </label>
              )}
            </div>

            <button type="submit" class="volo-btn volo-btn-primary">
              {t("art.save")}
            </button>
          </form>
        </div>

        {revisions && (
          <div class="volo-modal-backdrop" onClick={() => setRevisions(null)}>
            <div class="volo-modal" onClick={(e) => e.stopPropagation()}>
              <div class="volo-card-head">
                <h3 class="volo-card-title">{t("art.revisions")} (Max 20)</h3>
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setRevisions(null)}>Close</button>
              </div>
              {revisions.length === 0 ? (
                <p class="volo-help">No saved revisions yet.</p>
              ) : (
                revisions.map((r) => (
                  <div key={r.id} class="volo-card" style={{ padding: "12px", marginBottom: "10px" }}>
                    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "6px" }}>
                      <div>
                        <strong>{r.title}</strong>{" "}
                        <span class="volo-help">· {formatDateTime(r.created_at)} · {r.note || "Snapshot"}</span>
                      </div>
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={() => restoreRevision(r.id)}>
                        Rollback
                      </button>
                    </div>
                    <pre style={{ margin: 0, maxHeight: "110px", overflow: "auto", fontSize: "11.5px", background: "var(--volo-raised)", padding: "8px", borderRadius: "6px" }}>
                      {(r.content_md || "").slice(0, 600)}
                    </pre>
                  </div>
                ))
              )}
            </div>
          </div>
        )}
      </div>
    );
  }

  return (
    <div>
      {overview && (
        <>
          <div class="volo-kpi-grid">
            <div class="volo-kpi">
              <div class="volo-kpi-label">{t("ov.published")}</div>
              <div class="volo-kpi-val">{overview.counts?.published ?? 0}</div>
            </div>
            <div class="volo-kpi">
              <div class="volo-kpi-label">{t("ov.drafts")}</div>
              <div class="volo-kpi-val">{overview.counts?.drafts ?? 0}</div>
            </div>
            <div class="volo-kpi">
              <div class="volo-kpi-label">{t("ov.pages")}</div>
              <div class="volo-kpi-val">{overview.counts?.pages ?? 0}</div>
            </div>
            <div class="volo-kpi">
              <div class="volo-kpi-label">{t("ov.pending_comments")}</div>
              <div class="volo-kpi-val">{overview.counts?.pending_comments ?? 0}</div>
            </div>
            <div class="volo-kpi">
              <div class="volo-kpi-label">Runtime ({overview.runtime_dialect})</div>
              <div class="volo-kpi-val" style={{ fontSize: "18px" }}>
                {(overview.heap_alloc_mb ?? 0).toFixed(1)} MB Heap · {(overview.admin_bundle_kb ?? 0).toFixed(0)} KB UI
              </div>
            </div>
          </div>

          {overview.analytics_daily?.length > 0 && (
            <div class="volo-card">
              <h3 class="volo-card-title" style={{ marginBottom: "10px" }}>Zero-PII Daily PV / UV (14d)</h3>
              <div style={{ display: "flex", gap: "8px", alignItems: "flex-end", height: "74px", overflowX: "auto" }}>
                {overview.analytics_daily.slice(-14).map((d: any) => {
                  const h = Math.max(8, Math.min(64, Number(d.pv || 0) * 3));
                  return (
                    <div key={d.stat_date} style={{ flex: "1 0 42px", textAlign: "center" }}>
                      <div
                        title={`${d.stat_date}: ${d.pv} PV / ${d.uv} UV`}
                        style={{ height: `${h}px`, background: "var(--volo-accent)", borderRadius: "5px 5px 2px 2px", marginBottom: "4px" }}
                      />
                      <div style={{ fontSize: "10px", color: "var(--volo-muted)" }}>{String(d.stat_date).slice(5)}</div>
                    </div>
                  );
                })}
              </div>
            </div>
          )}

          {overview.plugin_cards?.length > 0 && (
            <div class="volo-grid-2" style={{ marginBottom: "18px" }}>
              {overview.plugin_cards.map((c: any, idx: number) => (
                <div key={idx} class="volo-card" dangerouslySetInnerHTML={{ __html: c.html || "" }} />
              ))}
            </div>
          )}
        </>
      )}

      <div class="volo-card">
        <div class="volo-card-head">
          <div style={{ display: "flex", alignItems: "center", gap: "8px", flexWrap: "wrap" }}>
            <h2 class="volo-card-title" style={{ marginRight: "8px" }}>{t("art.list")}</h2>
            {(["all", "published", "draft", "scheduled"] as const).map((st) => (
              <button
                key={st}
                type="button"
                class={`volo-tab ${statusFilter === st ? "active" : ""}`}
                onClick={() => setStatusFilter(st)}
              >
                {t(`art.${st}`)}
              </button>
            ))}
          </div>
          <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
            <input
              class="volo-input"
              style={{ width: "180px" }}
              placeholder="Search title / slug..."
              value={query}
              onInput={(e) => setQuery((e.currentTarget as HTMLInputElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void loadAll();
              }}
            />
            <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer" }}>
              {t("art.import_md")}
              <input
                type="file"
                accept=".md,.markdown,.zip"
                style={{ display: "none" }}
                onChange={(e) => {
                  const f = (e.currentTarget as HTMLInputElement).files?.[0];
                  if (f) void handleBatchImport(f);
                  (e.currentTarget as HTMLInputElement).value = "";
                }}
              />
            </label>
            <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={startNewArticle}>
              + {t("art.new")}
            </button>
          </div>
        </div>

        <div class="volo-table-wrap">
          <table class="volo-table">
            <thead>
              <tr>
                <th class="col-fluid">{t("art.title")}</th>
                <th class="col-status">{t("art.status")}</th>
                <th class="col-cat">{t("art.category")}</th>
                <th class="col-num">{t("art.metrics")}</th>
                <th class="col-date">{t("art.updated")}</th>
                <th class="col-act">{t("art.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {articles.length === 0 ? (
                <tr>
                  <td colSpan={6} style={{ textAlign: "center", color: "var(--volo-muted)", padding: "24px" }}>
                    No articles yet. Click "{t("art.new")}" to start writing!
                  </td>
                </tr>
              ) : (
                articles.map((row) => {
                  const a = row.article;
                  return (
                    <tr key={a.id}>
                      <td class="col-fluid">
                        <div style={{ fontWeight: 600 }}>
                          {a.is_pinned ? "📌 " : ""}
                          {row.is_password_protected ? "🔒 " : a.has_content_lock ? "🔐 " : ""}
                          {a.title}
                        </div>
                        <div class="volo-help">/posts/{a.slug} · {a.lang}</div>
                      </td>
                      <td class="col-status">
                        <span class={`volo-badge ${a.status === "published" ? "volo-badge-ok" : "volo-badge-warn"}`}>
                          {a.status}
                        </span>
                      </td>
                      <td class="col-cat">{catNameMap[a.category_id] || "—"}</td>
                      <td class="col-num">{a.view_count ?? 0} / {a.comment_count ?? 0}</td>
                      <td class="col-date">{formatDateTime(a.updated_at)}</td>
                      <td class="col-act">
                        <div style={{ display: "inline-flex", gap: "6px" }}>
                          <a class="volo-btn volo-btn-sm" href={`/posts/${encodeURIComponent(a.slug)}`} target="_blank" rel="noreferrer">
                            View
                          </a>
                          <button type="button" class="volo-btn volo-btn-sm" onClick={() => openEditArticle(a.id)}>
                            {t("common.edit")}
                          </button>
                          <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={() => deleteArticle(a.id)}>
                            ×
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
      </div>
    </div>
  );
}
