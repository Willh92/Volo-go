import { useEffect, useState } from "preact/hooks";
import {
  apiFetch,
  clearLocalDraft,
  currentUserSig,
  loadLocalDraft,
  LocalDraft,
  saveLocalDraft,
  showToast,
} from "../api";
import { CM6Editor, QuickInsertItem } from "../components/CM6Editor";
import { availableLocalesSig, formatDateTime, localeSig, t } from "../i18n";

export type OverviewSection = "dashboard" | "editor" | "articles" | "drafts";

interface ChartPoint {
  x: number;
  y: number;
}

function buildSmoothCurve(pts: ChartPoint[]): string {
  if (pts.length === 0) return "";
  if (pts.length === 1) return `M ${pts[0].x.toFixed(1)} ${pts[0].y.toFixed(1)}`;
  let d = `M ${pts[0].x.toFixed(1)} ${pts[0].y.toFixed(1)}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i];
    const p1 = pts[i + 1];
    const dx = (p1.x - p0.x) * 0.42;
    d += ` C ${(p0.x + dx).toFixed(1)} ${p0.y.toFixed(1)}, ${(p1.x - dx).toFixed(1)} ${p1.y.toFixed(1)}, ${p1.x.toFixed(1)} ${p1.y.toFixed(1)}`;
  }
  return d;
}

function buildFallbackDays(days: number): Array<{ stat_date: string; pv: number; uv: number }> {
  const out: Array<{ stat_date: string; pv: number; uv: number }> = [];
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  for (let i = days - 1; i >= 0; i--) {
    const dt = new Date(now.getTime() - i * 86400000);
    out.push({
      stat_date: `${dt.getFullYear()}-${pad(dt.getMonth() + 1)}-${pad(dt.getDate())}`,
      pv: 0,
      uv: 0,
    });
  }
  return out;
}

export function OverviewWorkspace({
  section = "dashboard",
  onNavigate,
}: {
  section?: OverviewSection;
  onNavigate?: (sec: string) => void;
}) {
  const curLocale = localeSig.value;
  const isAdmin = currentUserSig.value?.role === "admin";
  const locales = availableLocalesSig.value;

  const [overview, setOverview] = useState<any>(null);
  const [trendDays, setTrendDays] = useState<7 | 30>(7);
  const [articles, setArticles] = useState<any[]>([]);
  const [categories, setCategories] = useState<any[]>([]);
  const [seriesList, setSeriesList] = useState<any[]>([]);
  const [statusFilter, setStatusFilter] = useState<string>(section === "drafts" ? "draft" : "all");
  const [catFilter, setCatFilter] = useState<string>("");
  const [query, setQuery] = useState<string>("");

  // Editor state (`null` = not editing, `""` = new post, `"id"` = editing post)
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

  const draftKey = editingID === "" ? "article:new" : editingID ? `article:${editingID}` : "";

  const startNewArticle = async (catsOverride?: any[]) => {
    const catList = catsOverride || categories;
    setEditingID("");
    setTitle("");
    setSlug("");
    setCategoryID(catList[0]?.id || "");
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
    setLang(localeSig.value);
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

  const loadOverview = async (days: number) => {
    const ov = await apiFetch<any>(
      `/api/admin/overview?locale=${encodeURIComponent(localeSig.value)}&days=${days}`
    ).catch(() => null);
    if (ov) setOverview(ov);
  };

  const loadArticlesAndTaxonomies = async (effStatus = statusFilter, effCat = catFilter, effQuery = query) => {
    const [arts, cats, sers] = await Promise.all([
      apiFetch<{ items: any[] }>(
        `/api/admin/articles?status=${encodeURIComponent(effStatus)}&category_id=${encodeURIComponent(effCat)}&q=${encodeURIComponent(effQuery)}`
      ).catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/categories").catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/series").catch(() => ({ items: [] })),
    ]);
    setArticles(arts.items || []);
    setCategories(cats.items || []);
    setSeriesList(sers.items || []);
    return cats.items || [];
  };

  // Synchronize sidebar section selection
  useEffect(() => {
    if (section === "editor") {
      void loadArticlesAndTaxonomies().then((cats) => {
        if (editingID === null) {
          void startNewArticle(cats);
        }
      });
      void loadOverview(trendDays);
      return;
    }
    setEditingID(null);
    if (section === "drafts") {
      setStatusFilter("draft");
      void loadArticlesAndTaxonomies("draft", catFilter, query);
    } else if (section === "articles") {
      const nextSt = statusFilter === "draft" ? "all" : statusFilter;
      if (nextSt !== statusFilter) setStatusFilter(nextSt);
      void loadArticlesAndTaxonomies(nextSt, catFilter, query);
    } else {
      void loadOverview(trendDays);
      void loadArticlesAndTaxonomies(statusFilter, catFilter, query);
    }
  }, [section, curLocale]);

  useEffect(() => {
    void loadOverview(trendDays);
  }, [trendDays]);

  useEffect(() => {
    if (section === "articles" || section === "drafts") {
      void loadArticlesAndTaxonomies(statusFilter, catFilter, query);
    }
  }, [statusFilter, catFilter]);

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

  const closeEditor = () => {
    setEditingID(null);
    if (section === "editor") {
      onNavigate?.("articles");
    }
  };

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
      if (section === "editor") {
        onNavigate?.("articles");
      } else {
        void loadArticlesAndTaxonomies();
        void loadOverview(trendDays);
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const deleteArticle = async (id: string) => {
    if (!window.confirm(t("art.confirm_delete"))) return;
    try {
      await apiFetch(`/api/admin/articles/${id}`, { method: "DELETE" });
      showToast(t("common.deleted"));
      void loadArticlesAndTaxonomies();
      void loadOverview(trendDays);
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
      showToast(t("art.imported_toast", { count: res.imported_count }));
      void loadArticlesAndTaxonomies();
      void loadOverview(trendDays);
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
      showToast(t("art.rev_restored"));
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
      showToast(t("art.share_generated"));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const catNameMap: Record<string, string> = {};
  categories.forEach((c) => {
    catNameMap[c.id] = c.name;
  });

  // 1. Editor View (`section === "editor"` or editing an existing post)
  if (editingID !== null || section === "editor") {
    const quickInserts: QuickInsertItem[] = overview?.quick_inserts || [];
    return (
      <div>
        <div class="volo-card">
          <div class="volo-card-head">
            <h2 class="volo-card-title">{!editingID ? t("art.new") : `${t("common.edit")}: ${title}`}</h2>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              {editingID && (
                <>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={openRevisions}>
                    {t("art.revisions")}
                  </button>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={issueShareLink}>
                    {t("art.share_token")}
                  </button>
                </>
              )}
              <button type="button" class="volo-btn volo-btn-sm" onClick={closeEditor}>
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
                    {t("art.restore_draft")}
                  </button>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => {
                      if (draftKey) void clearLocalDraft(draftKey);
                      setPendingDraft(null);
                    }}
                  >
                    {t("art.discard_draft")}
                  </button>
                </div>
              </div>
            </div>
          )}

          {shareURL && (
            <div class="volo-card" style={{ padding: "10px 14px", fontSize: "12.5px" }}>
              <strong>{t("art.share_label")}</strong>{" "}
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
                <input class="volo-input" placeholder={t("art.slug_placeholder")} value={slug} onInput={(e) => setSlug((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.category")}</label>
                <select class="volo-select" value={categoryID} onChange={(e) => setCategoryID((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="">{t("art.uncategorized")}</option>
                  {categories.map((c) => (
                    <option key={c.id} value={c.id}>{c.name}</option>
                  ))}
                </select>
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.free_tags")}</label>
                <input class="volo-input" placeholder={t("art.tags_placeholder")} value={tagsText} onInput={(e) => setTagsText((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.series")}</label>
                <select class="volo-select" value={seriesID} onChange={(e) => setSeriesID((e.currentTarget as HTMLSelectElement).value)}>
                  <option value="">{t("art.series_none")}</option>
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
              <label>{t("art.md_body")}</label>
              <CM6Editor value={contentMD} onChange={setContentMD} quickInserts={quickInserts} />
            </div>

            <div class="volo-field" style={{ marginTop: "12px" }}>
              <label>{t("art.summary")}</label>
              <textarea class="volo-textarea" rows={2} value={summaryMD} onInput={(e) => setSummaryMD((e.currentTarget as HTMLTextAreaElement).value)} />
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.password")}</label>
                <input
                  type="text"
                  class="volo-input"
                  placeholder={t("art.password_placeholder")}
                  value={password}
                  onInput={(e) => {
                    setPassword((e.currentTarget as HTMLInputElement).value);
                    setClearPassword(false);
                  }}
                />
              </div>
              <div class="volo-field">
                <label>{t("art.lang")}</label>
                <select class="volo-select" value={lang} onChange={(e) => setLang((e.currentTarget as HTMLSelectElement).value)}>
                  {locales.map((loc) => (
                    <option key={loc.code} value={loc.code}>{loc.name} ({loc.code})</option>
                  ))}
                </select>
              </div>
              <div class="volo-field">
                <label>{t("art.trans_group")}</label>
                <input class="volo-input" placeholder={t("art.trans_placeholder")} value={translationGroup} onInput={(e) => setTranslationGroup((e.currentTarget as HTMLInputElement).value)} />
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.cover_url")}</label>
                <input class="volo-input" value={coverURL} onInput={(e) => setCoverURL((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.audio_url")}</label>
                <input class="volo-input" value={audioURL} onInput={(e) => setAudioURL((e.currentTarget as HTMLInputElement).value)} />
              </div>
              <div class="volo-field">
                <label>{t("art.audio_dur_rev")}</label>
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
                    placeholder={t("art.rev_note_placeholder")}
                    value={revisionNote}
                    onInput={(e) => setRevisionNote((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              </div>
            </div>

            <div style={{ display: "flex", alignItems: "center", gap: "18px", margin: "8px 0 16px" }}>
              <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                <input type="checkbox" checked={isPinned} onChange={(e) => setIsPinned((e.currentTarget as HTMLInputElement).checked)} />
                {t("art.pin_top")}
              </label>
              <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                <input type="checkbox" checked={allowComment} onChange={(e) => setAllowComment((e.currentTarget as HTMLInputElement).checked)} />
                {t("art.allow_comment")}
              </label>
              {editingID && (
                <label style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                  <input
                    type="checkbox"
                    checked={clearPassword}
                    onChange={(e) => {
                      setClearPassword((e.currentTarget as HTMLInputElement).checked);
                      if ((e.currentTarget as HTMLInputElement).checked) setPassword("");
                    }}
                  />
                  {t("art.clear_password")}
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
                <h3 class="volo-card-title">{t("art.revisions_max")}</h3>
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => setRevisions(null)}>{t("common.close")}</button>
              </div>
              {revisions.length === 0 ? (
                <p class="volo-help">{t("art.revisions_empty")}</p>
              ) : (
                revisions.map((r) => (
                  <div key={r.id} class="volo-card" style={{ padding: "12px", marginBottom: "10px" }}>
                    <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "6px" }}>
                      <div>
                        <strong>{r.title}</strong>{" "}
                        <span class="volo-help">· {formatDateTime(r.created_at)} · {r.note || t("art.snapshot")}</span>
                      </div>
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={() => restoreRevision(r.id)}>
                        {t("art.rollback")}
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

  // 2. Article Management / Drafts View (`section === "articles" | "drafts"`)
  if (section === "articles" || section === "drafts") {
    return (
      <div class="volo-card">
        <div class="volo-filter-bar">
          <div class="volo-filter-pills">
            {(["all", "published", "draft", "scheduled", "protected"] as const).map((st) => (
              <button
                key={st}
                type="button"
                class={`volo-filter-pill ${statusFilter === st ? "active" : ""}`}
                onClick={() => setStatusFilter(st)}
              >
                {t(`art.${st}`)}
              </button>
            ))}
          </div>
          <div class="volo-filter-controls">
            <select
              class="volo-select"
              style={{ width: "152px" }}
              value={catFilter}
              onChange={(e) => setCatFilter((e.currentTarget as HTMLSelectElement).value)}
            >
              <option value="">{t("art.all_categories")}</option>
              {categories.map((c) => (
                <option key={c.id} value={c.id}>{c.name}</option>
              ))}
            </select>
            <input
              class="volo-input"
              style={{ width: "204px" }}
              placeholder={t("art.search_placeholder")}
              value={query}
              onInput={(e) => setQuery((e.currentTarget as HTMLInputElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") void loadArticlesAndTaxonomies(statusFilter, catFilter, query);
              }}
            />
            <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer", height: "34px" }}>
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
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              style={{ height: "34px" }}
              onClick={() => {
                void startNewArticle();
                onNavigate?.("editor");
              }}
            >
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
                <th class="col-act col-act-th">{t("art.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {articles.length === 0 ? (
                <tr>
                  <td colSpan={6} class="volo-empty-state">
                    {t("art.empty", { btn: t("art.new") })}
                  </td>
                </tr>
              ) : (
                articles.map((row) => {
                  const a = row.article;
                  const catLabel = catNameMap[a.category_id];
                  return (
                    <tr key={a.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">
                            <a href="javascript:void(0)" onClick={(e) => { e.preventDefault(); void openEditArticle(a.id); }}>
                              {a.title}
                            </a>
                            {a.is_pinned && <span class="volo-mini-tag">{t("tbl.pinned")}</span>}
                            {row.is_password_protected ? (
                              <span class="volo-mini-tag warn">{t("tbl.protected")}</span>
                            ) : a.has_content_lock ? (
                              <span class="volo-mini-tag warn">{t("tbl.locked_block")}</span>
                            ) : null}
                          </div>
                          <div class="volo-cell-meta">
                            <span class="volo-code-chip">/posts/{a.slug}</span>
                            <span>{a.lang}</span>
                          </div>
                        </div>
                      </td>
                      <td class="col-status">
                        <span class={`volo-status-pill ${a.status === "published" ? "ok" : "warn"}`}>
                          <span class="volo-status-dot" />
                          {t(`art.${a.status}`)}
                        </span>
                      </td>
                      <td class="col-cat">
                        <span class={`volo-cat-chip ${catLabel ? "" : "muted"}`}>
                          {catLabel || t("art.uncategorized")}
                        </span>
                      </td>
                      <td class="col-num">
                        <div class="volo-metric-pair">
                          <span class="volo-metric-item" title="PV">
                            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9"><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>
                            {a.view_count ?? 0}
                          </span>
                          <span class="volo-metric-item" title={t("nav.comments")}>
                            <svg width="12.5" height="12.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.9"><path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>
                            {a.comment_count ?? 0}
                          </span>
                        </div>
                      </td>
                      <td class="col-date">{formatDateTime(a.updated_at)}</td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/posts/${encodeURIComponent(a.slug)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          <button type="button" class="volo-act-btn" onClick={() => openEditArticle(a.id)}>
                            {t("common.edit")}
                          </button>
                          <button type="button" class="volo-act-btn danger" onClick={() => deleteArticle(a.id)}>
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
      </div>
    );
  }

  // 3. Dashboard Overview (`section === "dashboard"`)
  const rawSeries: Array<{ stat_date: string; pv: number; uv: number }> =
    overview?.analytics_series?.length > 0
      ? overview.analytics_series
      : overview?.analytics_daily?.length > 0
      ? overview.analytics_daily.slice(-trendDays)
      : buildFallbackDays(trendDays);

  const rangePV = rawSeries.reduce((acc, item) => acc + Number(item.pv || 0), 0);
  const rangeUV = rawSeries.reduce((acc, item) => acc + Number(item.uv || 0), 0);

  // SVG Chart Geometry
  const W = 860;
  const H = 220;
  const padL = 44;
  const padR = 18;
  const padT = 18;
  const padB = 32;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const maxRaw = Math.max(4, ...rawSeries.map((d) => Math.max(Number(d.pv || 0), Number(d.uv || 0))));
  const yMax = Math.ceil(maxRaw / 4) * 4;
  const n = rawSeries.length;

  const pvPts: ChartPoint[] = rawSeries.map((d, idx) => ({
    x: padL + (n <= 1 ? plotW / 2 : (idx / (n - 1)) * plotW),
    y: padT + plotH - (Number(d.pv || 0) / yMax) * plotH,
  }));
  const uvPts: ChartPoint[] = rawSeries.map((d, idx) => ({
    x: padL + (n <= 1 ? plotW / 2 : (idx / (n - 1)) * plotW),
    y: padT + plotH - (Number(d.uv || 0) / yMax) * plotH,
  }));

  const pvLine = buildSmoothCurve(pvPts);
  const uvLine = buildSmoothCurve(uvPts);
  const pvArea =
    pvPts.length > 0
      ? `${pvLine} L ${pvPts[pvPts.length - 1].x.toFixed(1)} ${(padT + plotH).toFixed(1)} L ${pvPts[0].x.toFixed(1)} ${(padT + plotH).toFixed(1)} Z`
      : "";
  const xLabelStep = Math.max(1, Math.ceil(n / 8));
  const topArticles: any[] = overview?.top_articles || [];
  const topReferers: any[] = overview?.top_referers || [];

  return (
    <div>
      {/* Welcome Banner */}
      <div class="volo-welcome">
        <div>
          <div class="volo-eyebrow">{t("ov.eyebrow")}</div>
          <h2 class="volo-welcome-title">{t("ov.welcome_title")}</h2>
          <div class="volo-help">{t("ov.welcome_desc")}</div>
        </div>
        <div class="volo-welcome-actions">
          <button
            type="button"
            class="volo-btn volo-btn-primary"
            onClick={() => {
              void startNewArticle();
              onNavigate?.("editor");
            }}
          >
            + {t("ov.write_post")}
          </button>
          <button type="button" class="volo-btn" onClick={() => onNavigate?.("articles")}>
            {t("ov.manage_posts")}
          </button>
          <button
            type="button"
            class="volo-btn"
            onClick={() => onNavigate?.(isAdmin ? "settings_site" : "account")}
          >
            {isAdmin ? t("ov.site_settings") : t("nav.account")}
          </button>
        </div>
      </div>

      {/* Content & Runtime Summary KPIs */}
      {overview && (
        <div class="volo-kpi-grid">
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("articles")}>
            <div class="volo-kpi-label">{t("ov.published")}</div>
            <div class="volo-kpi-val">{overview.counts?.published ?? 0}</div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("drafts")}>
            <div class="volo-kpi-label">{t("ov.drafts")}</div>
            <div class="volo-kpi-val">{overview.counts?.drafts ?? 0}</div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("pages")}>
            <div class="volo-kpi-label">{t("ov.pages")}</div>
            <div class="volo-kpi-val">{overview.counts?.pages ?? 0}</div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("comments")}>
            <div class="volo-kpi-label">{t("ov.pending_comments")}</div>
            <div class="volo-kpi-val">{overview.counts?.pending_comments ?? 0}</div>
          </div>
          <div class="volo-kpi">
            <div class="volo-kpi-label">{t("ov.runtime", { dialect: overview.runtime_dialect || "sqlite" })}</div>
            <div class="volo-kpi-val" style={{ fontSize: "16.5px" }}>
              {t("ov.heap_ui", {
                heap: (overview.heap_alloc_mb ?? 0).toFixed(1),
                ui: (overview.admin_bundle_kb ?? 0).toFixed(0),
              })}
            </div>
          </div>
        </div>
      )}

      {/* Traffic & Reading Trends Panel */}
      <div class="volo-card">
        <div class="volo-card-head">
          <div>
            <h3 class="volo-card-title">{t("ov.trend_title")}</h3>
            <div class="volo-help" style={{ marginTop: "2px" }}>{t("ov.trend_sub")}</div>
          </div>
          <div class="volo-seg">
            <button
              type="button"
              class={`volo-seg-btn ${trendDays === 7 ? "active" : ""}`}
              onClick={() => setTrendDays(7)}
            >
              {t("ov.days_7")}
            </button>
            <button
              type="button"
              class={`volo-seg-btn ${trendDays === 30 ? "active" : ""}`}
              onClick={() => setTrendDays(30)}
            >
              {t("ov.days_30")}
            </button>
          </div>
        </div>

        <div class="volo-trend-kpis">
          <div class="volo-trend-kpi">
            <div class="volo-kpi-label">{t("ov.kpi_pv")}</div>
            <div class="volo-kpi-val">{rangePV}</div>
          </div>
          <div class="volo-trend-kpi">
            <div class="volo-kpi-label">{t("ov.kpi_uv")}</div>
            <div class="volo-kpi-val">{rangeUV}</div>
          </div>
          <div class="volo-trend-kpi">
            <div class="volo-kpi-label">{t("ov.kpi_privacy")}</div>
            <div style={{ display: "flex", alignItems: "baseline", gap: "8px" }}>
              <span class="volo-kpi-val" style={{ fontSize: "19px", color: "var(--volo-accent)" }}>
                {t("ov.kpi_privacy_val")}
              </span>
              <span class="volo-help">{t("ov.kpi_privacy_sub")}</span>
            </div>
          </div>
        </div>

        <div class="volo-chart-box">
          <div class="volo-chart-legend">
            <span style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
              <span style={{ width: "10px", height: "10px", borderRadius: "999px", background: "var(--volo-accent)" }} />
              {t("ov.legend_pv")}
            </span>
            <span style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
              <span style={{ width: "12px", height: "0", borderTop: "2px dashed #c9822b" }} />
              {t("ov.legend_uv")}
            </span>
          </div>
          <svg viewBox={`0 0 ${W} ${H}`} style={{ width: "100%", height: "auto", display: "block" }}>
            <defs>
              <linearGradient id="voloPvGrad" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor="var(--volo-accent)" stopOpacity="0.22" />
                <stop offset="100%" stopColor="var(--volo-accent)" stopOpacity="0.01" />
              </linearGradient>
            </defs>

            {/* Horizontal Y-axis gridlines */}
            {[0, 0.25, 0.5, 0.75, 1].map((ratio, idx) => {
              const y = padT + plotH - ratio * plotH;
              const val = Math.round(ratio * yMax);
              return (
                <g key={idx}>
                  <line
                    x1={padL}
                    y1={y}
                    x2={W - padR}
                    y2={y}
                    stroke="var(--volo-line)"
                    strokeDasharray={ratio === 0 ? undefined : "4 4"}
                  />
                  <text
                    x={padL - 8}
                    y={y + 4}
                    textAnchor="end"
                    fontSize="11"
                    fill="var(--volo-muted)"
                  >
                    {val}
                  </text>
                </g>
              );
            })}

            {pvArea && <path d={pvArea} fill="url(#voloPvGrad)" />}
            {pvLine && <path d={pvLine} fill="none" stroke="var(--volo-accent)" strokeWidth="2.5" strokeLinecap="round" />}
            {uvLine && <path d={uvLine} fill="none" stroke="#c9822b" strokeWidth="2" strokeDasharray="5 4" strokeLinecap="round" />}

            {rawSeries.map((d, idx) => {
              const p = pvPts[idx];
              const showLabel = idx % xLabelStep === 0 || idx === n - 1;
              return (
                <g key={d.stat_date}>
                  <circle cx={p.x} cy={p.y} r="3.4" fill="var(--volo-surface)" stroke="var(--volo-accent)" strokeWidth="2">
                    <title>{`${d.stat_date}: ${d.pv} PV / ${d.uv} UV`}</title>
                  </circle>
                  {showLabel && (
                    <text
                      x={p.x}
                      y={H - 8}
                      textAnchor="middle"
                      fontSize="11"
                      fill="var(--volo-muted)"
                    >
                      {String(d.stat_date).slice(5)}
                    </text>
                  )}
                </g>
              );
            })}
          </svg>
        </div>

        {/* 2-Column Top 10 Rank Cards */}
        <div class="volo-grid-2">
          <div class="volo-rank-card">
            <h4 class="volo-card-title" style={{ fontSize: "14px", marginBottom: "10px" }}>{t("ov.top_articles")}</h4>
            {topArticles.length === 0 ? (
              <div class="volo-help" style={{ padding: "12px 0" }}>{t("ov.top_articles_empty")}</div>
            ) : (
              topArticles.slice(0, 10).map((it, idx) => (
                <div key={it.item_key || idx} class="volo-rank-row">
                  <div style={{ display: "flex", alignItems: "center", minWidth: 0, gap: "6px" }}>
                    <span class="volo-rank-idx">{String(idx + 1).padStart(2, "0")}</span>
                    {it.slug ? (
                      <a
                        href={`/posts/${encodeURIComponent(it.slug)}`}
                        target="_blank"
                        rel="noreferrer"
                        style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontWeight: 550 }}
                      >
                        {it.title || it.slug}
                      </a>
                    ) : (
                      <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                        {it.title || it.item_key}
                      </span>
                    )}
                  </div>
                  <span class="volo-badge">{t("ov.hits_unit", { hits: it.hits ?? 0 })}</span>
                </div>
              ))
            )}
          </div>

          <div class="volo-rank-card">
            <h4 class="volo-card-title" style={{ fontSize: "14px", marginBottom: "10px" }}>{t("ov.top_referers")}</h4>
            {topReferers.length === 0 ? (
              <div class="volo-help" style={{ padding: "12px 0" }}>{t("ov.top_referers_empty")}</div>
            ) : (
              topReferers.slice(0, 10).map((it, idx) => (
                <div key={it.item_key || idx} class="volo-rank-row">
                  <div style={{ display: "flex", alignItems: "center", minWidth: 0, gap: "6px" }}>
                    <span class="volo-rank-idx">{String(idx + 1).padStart(2, "0")}</span>
                    <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontFamily: "var(--volo-mono)", fontSize: "12.5px" }}>
                      {it.item_key}
                    </span>
                  </div>
                  <span class="volo-badge">{t("ov.hits_unit", { hits: it.hits ?? 0 })}</span>
                </div>
              ))
            )}
          </div>
        </div>
      </div>

      {/* Plugin Dashboard Cards */}
      {overview?.plugin_cards?.length > 0 && (
        <div class="volo-grid-2">
          {overview.plugin_cards.map((c: any, idx: number) => (
            <div key={idx} class="volo-card" dangerouslySetInnerHTML={{ __html: c.html || "" }} />
          ))}
        </div>
      )}
    </div>
  );
}
