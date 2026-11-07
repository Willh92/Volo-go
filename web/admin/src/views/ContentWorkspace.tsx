import { useEffect, useState } from "preact/hooks";
import { apiFetch, currentUserSig, resetConsoleScroll, showToast, storageDriverSig } from "../api";
import { CM6Editor } from "../components/CM6Editor";
import { MediaAssetRecord, MediaPickButton } from "../components/MediaLibraryModal";
import { AnimatedModal, ConsoleSelect, paginateItems, Pagination } from "../components/UIControls";
import { availableLocalesSig, formatDateTime, localeSig, t } from "../i18n";

export type ContentSubTab = "pages" | "categories" | "tags" | "series" | "books" | "comments" | "media";

interface BookChapterDraft {
  id?: string;
  title: string;
  url: string;
  depth: number;
  sort_order: number;
}

const EMPTY_CAT = {
  id: "",
  name: "",
  slug: "",
  description: "",
  sort_order: 1,
};

const EMPTY_TAG = {
  id: "",
  name: "",
  slug: "",
};

const EMPTY_SER = {
  id: "",
  title: "",
  slug: "",
  description: "",
  cover_url: "",
  sort_order: 1,
  article_ids: [] as string[],
};

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

function subTabLabel(k: ContentSubTab): string {
  switch (k) {
    case "categories":
      return t("cnt.categories");
    case "tags":
      return t("cnt.tags");
    case "series":
      return t("cnt.series");
    case "books":
      return t("cnt.books");
    case "pages":
      return t("cnt.pages");
    case "media":
      return t("cnt.media");
    case "comments":
      return t("cnt.comments");
  }
}

function bookSourceLabel(src: string): string {
  switch (src) {
    case "gitbook":
      return t("cnt.book_src_gitbook");
    case "mdbook":
      return t("cnt.book_src_mdbook");
    case "vuepress":
      return t("cnt.book_src_vuepress");
    case "series":
      return t("cnt.book_src_series");
    case "manual":
      return t("cnt.book_src_manual");
    default:
      return t("cnt.book_src_auto");
  }
}

function formatAssetSize(bytes: number): string {
  const n = Number(bytes || 0);
  if (n >= 1024 * 1024) {
    return `${(n / (1024 * 1024)).toFixed(2)} MB`;
  }
  return `${Math.max(1, Math.round(n / 1024))} KB`;
}

export function ContentWorkspace({
  activeTab = "categories",
  onNavigate,
}: {
  activeTab?: ContentSubTab;
  onNavigate?: (sec: string) => void;
}) {
  const [sub, setSub] = useState<ContentSubTab>(activeTab);
  const locales = availableLocalesSig.value;
  const isAdmin = currentUserSig.value?.role === "admin";
  const [pageIdx, setPageIdx] = useState<number>(1);

  // Pages (list on top + dedicated full-width Page Editor view)
  const [pages, setPages] = useState<any[]>([]);
  const [pageEditorOpen, setPageEditorOpen] = useState<boolean>(false);
  const [editPage, setEditPage] = useState<any | null>(null);

  // Categories
  const [categories, setCategories] = useState<any[]>([]);
  const [catModalOpen, setCatModalOpen] = useState<boolean>(false);
  const [catForm, setCatForm] = useState({ ...EMPTY_CAT });

  // Tags
  const [tags, setTags] = useState<any[]>([]);
  const [tagFilter, setTagFilter] = useState<string>("");
  const [tagModalOpen, setTagModalOpen] = useState<boolean>(false);
  const [tagForm, setTagForm] = useState({ ...EMPTY_TAG });

  // Series + Articles checklist
  const [series, setSeries] = useState<any[]>([]);
  const [allArticles, setAllArticles] = useState<any[]>([]);
  const [serModalOpen, setSerModalOpen] = useState<boolean>(false);
  const [serForm, setSerForm] = useState({ ...EMPTY_SER });
  const [serArtFilter, setSerArtFilter] = useState<string>("");

  // Books + Manual Chapters modal
  const [books, setBooks] = useState<any[]>([]);
  const [bookModalOpen, setBookModalOpen] = useState<boolean>(false);
  const [bookForm, setBookForm] = useState({ ...EMPTY_BOOK });
  const [chapModalOpen, setChapModalOpen] = useState<boolean>(false);
  const [chapBook, setChapBook] = useState<any | null>(null);
  const [chapItems, setChapItems] = useState<BookChapterDraft[]>([]);

  // Comments
  const [comments, setComments] = useState<any[]>([]);
  const [cmtStatus, setCmtStatus] = useState<string>("");
  const [replyText, setReplyText] = useState<Record<string, string>>({});

  // Media Library
  const [media, setMedia] = useState<MediaAssetRecord[]>([]);
  const [orphanCount, setOrphanCount] = useState<number>(0);
  const [mediaView, setMediaView] = useState<"grid" | "list">("grid");
  const [mediaRefFilter, setMediaRefFilter] = useState<"all" | "orphan" | "used">("all");
  const [mediaKindFilter, setMediaKindFilter] = useState<"all" | "image" | "audio" | "other">("all");
  const [mediaQuery, setMediaQuery] = useState<string>("");
  const [isDraggingMedia, setIsDraggingMedia] = useState<boolean>(false);
  const [uploadingMedia, setUploadingMedia] = useState<boolean>(false);
  const [cleanOrphansModalOpen, setCleanOrphansModalOpen] = useState<boolean>(false);
  const [previewAsset, setPreviewAsset] = useState<MediaAssetRecord | null>(null);

  useEffect(() => {
    setSub(activeTab);
    setPageEditorOpen(false);
    setEditPage(null);
    setPageIdx(1);
    resetConsoleScroll();
  }, [activeTab]);

  const switchSub = (k: ContentSubTab) => {
    setSub(k);
    setPageEditorOpen(false);
    setEditPage(null);
    setPageIdx(1);
    resetConsoleScroll();
    onNavigate?.(k);
  };

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
        const [serRes, artRes] = await Promise.all([
          apiFetch<{ items: any[] }>("/api/admin/series"),
          apiFetch<{ items: any[] }>("/api/admin/articles"),
        ]);
        setSeries(serRes.items || []);
        const normalizedArts = (artRes.items || []).map((row: any) => row.article || row);
        setAllArticles(normalizedArts);
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
        const r = await apiFetch<{
          items: MediaAssetRecord[];
          orphan_count?: number;
          storage_driver?: string;
        }>("/api/admin/media");
        const list = r.items || [];
        setMedia(list);
        setOrphanCount(r.orphan_count ?? list.filter((x) => x.is_orphan).length);
        storageDriverSig.value = r.storage_driver || "local";
      }
    } catch (err: any) {
      showToast(err.message);
    }
  };

  useEffect(() => {
    setPageIdx(1);
    void loadSub();
  }, [sub, cmtStatus]);

  const openNewPageEditor = () => {
    setEditPage({
      title: "",
      slug: "",
      content_md: "",
      is_published: true,
      allow_comment: true,
      show_toc: false,
      lang: localeSig.value,
      translation_group: "",
    });
    setPageEditorOpen(true);
    resetConsoleScroll();
  };

  const openExistingPageEditor = async (pageId: string) => {
    try {
      const full = await apiFetch<{ page: any }>(`/api/admin/pages/${pageId}`);
      setEditPage(full.page);
      setPageEditorOpen(true);
      resetConsoleScroll();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const closePageEditor = () => {
    setPageEditorOpen(false);
    setEditPage(null);
    resetConsoleScroll();
  };

  const savePage = async (e: Event) => {
    e.preventDefault();
    if (!editPage) return;
    try {
      const payload = {
        ...editPage,
        translation_group: editPage.translation_group || "",
      };
      if (!editPage.id) {
        await apiFetch("/api/admin/pages", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/pages/${editPage.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setPageEditorOpen(false);
      setEditPage(null);
      resetConsoleScroll();
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveCategory = async (e?: Event) => {
    e?.preventDefault();
    if (!catForm.name.trim()) return;
    try {
      const payload = {
        name: catForm.name.trim(),
        slug: catForm.slug.trim(),
        sort_order: Number(catForm.sort_order) || 0,
        description: catForm.description,
      };
      if (!catForm.id) {
        await apiFetch("/api/admin/categories", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/categories/${catForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setCatModalOpen(false);
      setCatForm({ ...EMPTY_CAT });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveTag = async (e?: Event) => {
    e?.preventDefault();
    if (!tagForm.name.trim()) return;
    try {
      const payload = {
        name: tagForm.name.trim(),
        slug: tagForm.slug.trim(),
      };
      if (!tagForm.id) {
        await apiFetch("/api/admin/tags", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/tags/${tagForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setTagModalOpen(false);
      setTagForm({ ...EMPTY_TAG });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const cleanupUnusedTags = async () => {
    try {
      const res = await apiFetch<{ deleted: number; items: any[] }>("/api/admin/tags/cleanup", {
        method: "POST",
        body: "{}",
      });
      setTags(res.items || []);
      showToast(t("cnt.tag_cleaned", { count: res.deleted ?? 0 }));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveSeries = async (e?: Event) => {
    e?.preventDefault();
    if (!serForm.title.trim()) return;
    try {
      const payload = {
        title: serForm.title.trim(),
        slug: serForm.slug.trim(),
        sort_order: Number(serForm.sort_order) || 0,
        cover_url: serForm.cover_url.trim(),
        description: serForm.description,
        article_ids: serForm.article_ids,
      };
      if (!serForm.id) {
        await apiFetch("/api/admin/series", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/series/${serForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setSerModalOpen(false);
      setSerForm({ ...EMPTY_SER });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveBook = async (e?: Event) => {
    e?.preventDefault();
    if (!bookForm.title.trim()) return;
    try {
      const payload = {
        title: bookForm.title.trim(),
        slug: bookForm.slug.trim(),
        description: bookForm.description,
        cover_url: bookForm.cover_url.trim(),
        source_type: bookForm.source_type,
        source_url: bookForm.source_url.trim(),
        series_id: bookForm.series_id,
        embed_mode: bookForm.embed_mode,
        sort_order: Number(bookForm.sort_order) || 0,
      };
      if (!bookForm.id) {
        await apiFetch("/api/admin/books", { method: "POST", body: JSON.stringify(payload) });
      } else {
        await apiFetch(`/api/admin/books/${bookForm.id}`, { method: "PUT", body: JSON.stringify(payload) });
      }
      setBookModalOpen(false);
      setBookForm({ ...EMPTY_BOOK });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const openManageChapters = async (book: any) => {
    try {
      const res = await apiFetch<{ book: any; chapters: any[] }>(`/api/admin/books/${book.id}`);
      setChapBook(res.book || book);
      const list = (res.chapters || []).map((ch: any, idx: number) => ({
        id: ch.id,
        title: ch.title || "",
        url: ch.url || "",
        depth: Number(ch.depth) || 0,
        sort_order: idx + 1,
      }));
      setChapItems(list);
      setChapModalOpen(true);
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveChapters = async () => {
    if (!chapBook) return;
    try {
      const normalized = chapItems
        .filter((ch) => ch.title.trim() || ch.url.trim())
        .map((ch, idx) => ({
          title: ch.title.trim() || `${idx + 1}`,
          url: ch.url.trim(),
          depth: Math.max(0, Math.min(3, Number(ch.depth) || 0)),
          sort_order: idx + 1,
        }));
      await apiFetch(`/api/admin/books/${chapBook.id}`, {
        method: "PUT",
        body: JSON.stringify({
          ...chapBook,
          chapters: normalized,
        }),
      });
      setChapModalOpen(false);
      setChapBook(null);
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const uploadMediaFiles = async (fileList: FileList | File[]) => {
    const files = Array.from(fileList);
    if (files.length === 0) return;
    setUploadingMedia(true);
    let okCount = 0;
    try {
      for (const f of files) {
        const fd = new FormData();
        fd.append("file", f);
        await apiFetch("/api/admin/media/upload", { method: "POST", body: fd });
        okCount++;
      }
      if (okCount > 0) {
        showToast(t("cnt.media_uploaded"));
        await loadSub();
      }
    } catch (err: any) {
      showToast(err.message);
      if (okCount > 0) {
        await loadSub();
      }
    } finally {
      setUploadingMedia(false);
    }
  };

  const handleScanOrphans = async () => {
    try {
      const res = await apiFetch<{ orphans?: any[]; count?: number }>("/api/admin/media/orphans", {
        method: "POST",
        body: "{}",
      });
      await loadSub();
      const found = res.count ?? res.orphans?.length ?? 0;
      showToast(t("cnt.media_found_orphans", { count: found }));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const handleCleanOrphans = async () => {
    try {
      const res = await apiFetch<{ orphans?: any[]; deleted_count?: number }>("/api/admin/media/orphans?delete=1", {
        method: "POST",
        body: "{}",
      });
      const cleaned = res.deleted_count ?? res.orphans?.length ?? orphanCount;
      setCleanOrphansModalOpen(false);
      await loadSub();
      showToast(t("cnt.media_cleaned_orphans", { count: cleaned }));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const handleDeleteMediaItem = async (asset: MediaAssetRecord) => {
    if (!window.confirm(t("cnt.media_confirm_del"))) return;
    try {
      await apiFetch(`/api/admin/media/${asset.id}`, { method: "DELETE" });
      if (previewAsset?.id === asset.id) {
        setPreviewAsset(null);
      }
      showToast(t("common.deleted"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const copyMediaMarkdown = (m: MediaAssetRecord) => {
    const name = m.original_name || m.id;
    const isImg = String(m.mime_type || "").startsWith("image/");
    const md = isImg ? `![${name}](${m.public_url})` : `[${name}](${m.public_url})`;
    void navigator.clipboard?.writeText(md);
    showToast(t("cnt.media_copied"));
  };

  const copyMediaUrl = (m: MediaAssetRecord) => {
    void navigator.clipboard?.writeText(m.public_url);
    showToast(t("cnt.media_copied"));
  };

  const cmtStatusLabel = (st: string) => {
    if (st === "approved") return t("cnt.cmt_approved");
    if (st === "pending") return t("cnt.cmt_pending");
    if (st === "rejected") return t("cnt.cmt_rejected");
    return st;
  };

  const filteredTags = tagFilter.trim()
    ? tags.filter((tg) => {
        const q = tagFilter.trim().toLowerCase();
        return (
          String(tg.name || "").toLowerCase().includes(q) ||
          String(tg.slug || "").toLowerCase().includes(q)
        );
      })
    : tags;

  const filteredSeriesArticles = serArtFilter.trim()
    ? allArticles.filter((a) => {
        const q = serArtFilter.trim().toLowerCase();
        return (
          String(a.title || "").toLowerCase().includes(q) ||
          String(a.slug || "").toLowerCase().includes(q)
        );
      })
    : allArticles;

  const orphanItems = media.filter((m) => !!m.is_orphan);
  const orphanTotalBytes = orphanItems.reduce((acc, m) => acc + Number(m.size_bytes || 0), 0);
  const totalMediaBytes = media.reduce((acc, m) => acc + Number(m.size_bytes || 0), 0);
  const totalMB = (totalMediaBytes / (1024 * 1024)).toFixed(2);

  const filteredMedia = media.filter((m) => {
    if (mediaRefFilter === "orphan" && !m.is_orphan) return false;
    if (mediaRefFilter === "used" && m.is_orphan) return false;
    const mime = String(m.mime_type || "").toLowerCase();
    if (mediaKindFilter === "image" && !mime.startsWith("image/")) return false;
    if (mediaKindFilter === "audio" && !mime.startsWith("audio/")) return false;
    if (mediaKindFilter === "other" && (mime.startsWith("image/") || mime.startsWith("audio/"))) return false;
    if (mediaQuery.trim()) {
      const q = mediaQuery.trim().toLowerCase();
      const name = String(m.original_name || "").toLowerCase();
      const url = String(m.public_url || "").toLowerCase();
      if (!name.includes(q) && !url.includes(q)) return false;
    }
    return true;
  });

  const pagedPages = paginateItems(pages, pageIdx, 15);
  const pagedCats = paginateItems(categories, pageIdx, 15);
  const pagedTags = paginateItems(filteredTags, pageIdx, 15);
  const pagedSeries = paginateItems(series, pageIdx, 15);
  const pagedBooks = paginateItems(books, pageIdx, 15);
  const pagedComments = paginateItems(comments, pageIdx, 15);
  const pagedMedia = paginateItems(filteredMedia, pageIdx, 24);

  return (
    <div>
      {sub !== "comments" && (
        <div class="volo-subtabs">
          {(["categories", "tags", "series", "books", "pages", "media"] as ContentSubTab[]).map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => switchSub(k)}
            >
              {subTabLabel(k)}
            </button>
          ))}
        </div>
      )}

      {/* 1. Categories */}
      {sub === "categories" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.cat_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.cat_desc")}</p>
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => {
                setCatForm({ ...EMPTY_CAT });
                setCatModalOpen(true);
              }}
            >
              + {t("cnt.new_cat")}
            </button>
          </div>

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
                {pagedCats.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedCats.slice.map((c) => (
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
                          <a class="volo-act-btn" href={`/categories/${encodeURIComponent(c.slug)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              setCatForm({
                                id: c.id,
                                name: c.name || "",
                                slug: c.slug || "",
                                description: c.description || "",
                                sort_order: c.sort_order ?? 1,
                              });
                              setCatModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              await apiFetch(`/api/admin/categories/${c.id}`, { method: "DELETE" });
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
          <Pagination total={categories.length} page={pagedCats.safePage} pageSize={15} onChange={setPageIdx} />

          <AnimatedModal
            open={catModalOpen}
            onClose={() => setCatModalOpen(false)}
            title={catForm.id ? t("cnt.edit_cat") : t("cnt.new_cat")}
            subtitle={t("cnt.cat_desc")}
            maxWidth="540px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setCatModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveCategory()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveCategory(e)} autoComplete="off">
              <div class="volo-field">
                <label>{t("cnt.lbl_cat_name")}</label>
                <input
                  class="volo-input"
                  required
                  placeholder={t("cnt.cat_name_ph")}
                  value={catForm.name}
                  onInput={(e) => setCatForm({ ...catForm, name: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-grid-2">
                <div class="volo-field">
                  <label>{t("cnt.lbl_slug")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("cnt.cat_slug_ph")}
                    value={catForm.slug}
                    onInput={(e) => setCatForm({ ...catForm, slug: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={catForm.sort_order ?? 1}
                    onInput={(e) => setCatForm({ ...catForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("cnt.lbl_desc")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  placeholder={t("cnt.desc_ph")}
                  value={catForm.description}
                  onInput={(e) => setCatForm({ ...catForm, description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 2. Tags */}
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
                onInput={(e) => {
                  setTagFilter((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              {isAdmin && (
                <button type="button" class="volo-btn volo-btn-sm" onClick={cleanupUnusedTags}>
                  {t("cnt.tag_cleanup")}
                </button>
              )}
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  setTagForm({ ...EMPTY_TAG });
                  setTagModalOpen(true);
                }}
              >
                + {t("cnt.new_tag")}
              </button>
            </div>
          </div>

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
                {pagedTags.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedTags.slice.map((tg) => (
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
                          <a class="volo-act-btn" href={`/tags/${encodeURIComponent(tg.slug || tg.name)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              setTagForm({
                                id: tg.id,
                                name: tg.name || "",
                                slug: tg.slug || "",
                              });
                              setTagModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              await apiFetch(`/api/admin/tags/${tg.id}`, { method: "DELETE" });
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
          <Pagination total={filteredTags.length} page={pagedTags.safePage} pageSize={15} onChange={setPageIdx} />

          <AnimatedModal
            open={tagModalOpen}
            onClose={() => setTagModalOpen(false)}
            title={tagForm.id ? t("cnt.edit_tag") : t("cnt.new_tag")}
            subtitle={t("cnt.tag_desc")}
            maxWidth="480px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setTagModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveTag()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveTag(e)} autoComplete="off">
              <div class="volo-field">
                <label>{t("cnt.lbl_tag_name")}</label>
                <input
                  class="volo-input"
                  required
                  placeholder={t("cnt.tag_new_ph")}
                  value={tagForm.name}
                  onInput={(e) => setTagForm({ ...tagForm, name: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("cnt.lbl_slug")}</label>
                <input
                  class="volo-input"
                  placeholder={t("cnt.cat_slug_ph")}
                  value={tagForm.slug}
                  onInput={(e) => setTagForm({ ...tagForm, slug: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 3. Series */}
      {sub === "series" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.ser_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.ser_desc")}</p>
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => {
                setSerForm({ ...EMPTY_SER });
                setSerArtFilter("");
                setSerModalOpen(true);
              }}
            >
              + {t("cnt.new_series")}
            </button>
          </div>

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
                {pagedSeries.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedSeries.slice.map((s) => (
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
                      <td class="col-num">
                        <span class="volo-cat-chip">{Array.isArray(s.article_ids) ? s.article_ids.length : (s.article_count ?? 0)}</span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/series/${encodeURIComponent(s.slug)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              setSerForm({
                                id: s.id,
                                title: s.title || "",
                                slug: s.slug || "",
                                description: s.description || "",
                                cover_url: s.cover_url || "",
                                sort_order: s.sort_order ?? 1,
                                article_ids: Array.isArray(s.article_ids) ? [...s.article_ids] : [],
                              });
                              setSerArtFilter("");
                              setSerModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              await apiFetch(`/api/admin/series/${s.id}`, { method: "DELETE" });
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
          <Pagination total={series.length} page={pagedSeries.safePage} pageSize={15} onChange={setPageIdx} />

          <AnimatedModal
            open={serModalOpen}
            onClose={() => setSerModalOpen(false)}
            title={serForm.id ? t("cnt.edit_series") : t("cnt.new_series")}
            subtitle={t("cnt.ser_desc")}
            maxWidth="680px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setSerModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveSeries()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveSeries(e)} autoComplete="off">
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("cnt.lbl_ser_title")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("cnt.ser_title_ph")}
                    value={serForm.title}
                    onInput={(e) => setSerForm({ ...serForm, title: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_slug")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("cnt.cat_slug_ph")}
                    value={serForm.slug}
                    onInput={(e) => setSerForm({ ...serForm, slug: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={serForm.sort_order ?? 1}
                    onInput={(e) => setSerForm({ ...serForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("art.cover_url")}</label>
                <div class="volo-input-row">
                  <input
                    class="volo-input"
                    placeholder={t("cnt.book_cover_ph")}
                    value={serForm.cover_url}
                    onInput={(e) => setSerForm({ ...serForm, cover_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton
                    filterKind="image"
                    onPick={(u) => setSerForm({ ...serForm, cover_url: u })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("cnt.lbl_desc")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  placeholder={t("cnt.desc_ph")}
                  value={serForm.description}
                  onInput={(e) => setSerForm({ ...serForm, description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>

              <div class="volo-field" style={{ marginBottom: 0 }}>
                <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", marginBottom: "6px", flexWrap: "wrap" }}>
                  <label style={{ marginBottom: 0 }}>
                    {t("cnt.ser_articles_lbl")} ({serForm.article_ids.length})
                  </label>
                  <input
                    class="volo-input"
                    style={{ width: "220px", height: "30px", fontSize: "12.5px" }}
                    placeholder={t("ed.qi_search_art")}
                    value={serArtFilter}
                    onInput={(e) => setSerArtFilter((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
                <div
                  style={{
                    maxHeight: "210px",
                    overflowY: "auto",
                    border: "1px solid var(--volo-line)",
                    borderRadius: "9px",
                    padding: "8px 10px",
                    background: "var(--volo-raised)",
                    display: "flex",
                    flexDirection: "column",
                    gap: "6px",
                  }}
                >
                  {filteredSeriesArticles.length === 0 ? (
                    <div class="volo-help" style={{ padding: "8px 4px" }}>{t("tbl.empty")}</div>
                  ) : (
                    filteredSeriesArticles.map((art) => {
                      const checked = serForm.article_ids.includes(art.id);
                      return (
                        <label key={art.id} class="volo-check-item" style={{ justifyContent: "space-between", gap: "10px" }}>
                          <span style={{ display: "inline-flex", alignItems: "center", gap: "8px", minWidth: 0 }}>
                            <input
                              type="checkbox"
                              checked={checked}
                              onChange={(e) => {
                                const on = (e.currentTarget as HTMLInputElement).checked;
                                setSerForm({
                                  ...serForm,
                                  article_ids: on
                                    ? [...serForm.article_ids, art.id]
                                    : serForm.article_ids.filter((id) => id !== art.id),
                                });
                              }}
                            />
                            <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                              {art.title}
                            </span>
                          </span>
                          <span class="volo-code-chip" style={{ flexShrink: 0 }}>/{art.slug}</span>
                        </label>
                      );
                    })
                  )}
                </div>
              </div>
            </form>
          </AnimatedModal>
        </div>
      )}

      {/* 4. Books */}
      {sub === "books" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("cnt.book_head")}</h3>
              <p class="volo-sec-desc">{t("cnt.book_desc")}</p>
            </div>
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => {
                setBookForm({ ...EMPTY_BOOK });
                setBookModalOpen(true);
              }}
            >
              + {t("cnt.new_book")}
            </button>
          </div>

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
                {pagedBooks.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("tbl.empty")}</td>
                  </tr>
                ) : (
                  pagedBooks.slice.map((b) => (
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
                        <span class="volo-cat-chip">{bookSourceLabel(b.source_type || "auto")}</span>
                      </td>
                      <td class="col-num">
                        <span class="volo-status-pill ok">
                          <span class="volo-status-dot" />
                          {t("cnt.book_chapters", { count: b.chapter_count ?? 0 })}
                        </span>
                      </td>
                      <td class="col-act">
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/books/${encodeURIComponent(b.slug)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          {(b.source_url || b.series_id) && (
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                try {
                                  const r = await apiFetch<{ chapter_count: number }>(`/api/admin/books/${b.id}/sync`, {
                                    method: "POST",
                                    body: "{}",
                                  });
                                  showToast(t("cnt.book_synced", { count: r.chapter_count ?? 0 }));
                                  void loadSub();
                                } catch (err: any) {
                                  showToast(err.message);
                                }
                              }}
                            >
                              {t("cnt.book_sync_toc")}
                            </button>
                          )}
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => void openManageChapters(b)}
                          >
                            {t("cnt.manage_chapters")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              setBookForm({ ...EMPTY_BOOK, ...b });
                              setBookModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              await apiFetch(`/api/admin/books/${b.id}`, { method: "DELETE" });
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
          <Pagination total={books.length} page={pagedBooks.safePage} pageSize={15} onChange={setPageIdx} />

          <AnimatedModal
            open={bookModalOpen}
            onClose={() => setBookModalOpen(false)}
            title={bookForm.id ? t("cnt.edit_book") : t("cnt.new_book")}
            subtitle={t("cnt.book_desc")}
            maxWidth="680px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setBookModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveBook()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void saveBook(e)} autoComplete="off">
              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("cnt.lbl_book_title")}</label>
                  <input
                    class="volo-input"
                    required
                    placeholder={t("cnt.book_title_ph")}
                    value={bookForm.title}
                    onInput={(e) => setBookForm({ ...bookForm, title: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_slug")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("cnt.cat_slug_ph")}
                    value={bookForm.slug}
                    onInput={(e) => setBookForm({ ...bookForm, slug: (e.currentTarget as HTMLInputElement).value })}
                  />
                </div>
                <div class="volo-field">
                  <label>{t("cnt.lbl_sort")}</label>
                  <input
                    type="number"
                    class="volo-input"
                    value={bookForm.sort_order ?? 1}
                    onInput={(e) => setBookForm({ ...bookForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>

              <div class="volo-field">
                <label>{t("cnt.lbl_book_cover")}</label>
                <div class="volo-input-row">
                  <input
                    class="volo-input"
                    placeholder={t("cnt.book_cover_ph")}
                    value={bookForm.cover_url}
                    onInput={(e) => setBookForm({ ...bookForm, cover_url: (e.currentTarget as HTMLInputElement).value })}
                  />
                  <MediaPickButton
                    filterKind="image"
                    onPick={(u) => setBookForm({ ...bookForm, cover_url: u })}
                  />
                </div>
              </div>

              <div class="volo-grid-3">
                <div class="volo-field">
                  <label>{t("cnt.lbl_book_source")}</label>
                  <ConsoleSelect
                    value={bookForm.source_type}
                    onChange={(val) => setBookForm({ ...bookForm, source_type: val })}
                    options={[
                      { value: "auto", label: t("cnt.book_src_auto") },
                      { value: "gitbook", label: t("cnt.book_src_gitbook") },
                      { value: "mdbook", label: t("cnt.book_src_mdbook") },
                      { value: "vuepress", label: t("cnt.book_src_vuepress") },
                      { value: "series", label: t("cnt.book_src_series") },
                      { value: "manual", label: t("cnt.book_src_manual") },
                    ]}
                  />
                </div>
                {bookForm.source_type === "series" ? (
                  <div class="volo-field">
                    <label>{t("art.series")}</label>
                    <ConsoleSelect
                      value={bookForm.series_id}
                      onChange={(val) => setBookForm({ ...bookForm, series_id: val })}
                      options={[
                        { value: "", label: t("art.series_none") },
                        ...series.map((s) => ({ value: s.id, label: s.title })),
                      ]}
                    />
                  </div>
                ) : (
                  <div class="volo-field">
                    <label>{t("cnt.lbl_book_toc")}</label>
                    <input
                      class="volo-input"
                      placeholder={t("cnt.book_toc_ph")}
                      value={bookForm.source_url}
                      onInput={(e) => setBookForm({ ...bookForm, source_url: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                )}
                <div class="volo-field">
                  <label>{t("cnt.lbl_book_embed")}</label>
                  <ConsoleSelect
                    value={bookForm.embed_mode}
                    onChange={(val) => setBookForm({ ...bookForm, embed_mode: val })}
                    options={[
                      { value: "iframe", label: t("cnt.book_embed_iframe") },
                      { value: "link", label: t("cnt.book_embed_link") },
                    ]}
                  />
                </div>
              </div>

              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("cnt.lbl_book_notes")}</label>
                <textarea
                  class="volo-textarea"
                  rows={2}
                  placeholder={t("cnt.book_notes_ph")}
                  value={bookForm.description}
                  onInput={(e) => setBookForm({ ...bookForm, description: (e.currentTarget as HTMLTextAreaElement).value })}
                />
              </div>
            </form>
          </AnimatedModal>

          <AnimatedModal
            open={chapModalOpen}
            onClose={() => setChapModalOpen(false)}
            title={t("cnt.chap_modal_title", { title: chapBook?.title || "" })}
            subtitle={t("cnt.chap_modal_desc")}
            maxWidth="680px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setChapModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-primary" onClick={() => void saveChapters()}>
                  {t("common.save")}
                </button>
              </>
            }
          >
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", marginBottom: "10px" }}>
              <span class="volo-help">{t("cnt.book_chapters", { count: chapItems.length })}</span>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() =>
                  setChapItems([
                    ...chapItems,
                    { title: "", url: "", depth: 0, sort_order: chapItems.length + 1 },
                  ])
                }
              >
                + {t("cnt.chap_add")}
              </button>
            </div>
            {chapItems.length === 0 ? (
              <div class="volo-empty-state">{t("tbl.empty")}</div>
            ) : (
              <div style={{ display: "flex", flexDirection: "column", gap: "8px", maxHeight: "360px", overflowY: "auto" }}>
                {chapItems.map((ch, idx) => (
                  <div
                    key={idx}
                    style={{
                      display: "grid",
                      gridTemplateColumns: "1fr 1.2fr 76px auto",
                      gap: "8px",
                      alignItems: "center",
                    }}
                  >
                    <input
                      class="volo-input"
                      placeholder={t("cnt.chap_title_ph")}
                      value={ch.title}
                      onInput={(e) => {
                        const next = [...chapItems];
                        next[idx] = { ...ch, title: (e.currentTarget as HTMLInputElement).value };
                        setChapItems(next);
                      }}
                    />
                    <input
                      class="volo-input"
                      placeholder={t("cnt.chap_url_ph")}
                      value={ch.url}
                      onInput={(e) => {
                        const next = [...chapItems];
                        next[idx] = { ...ch, url: (e.currentTarget as HTMLInputElement).value };
                        setChapItems(next);
                      }}
                    />
                    <input
                      type="number"
                      min={0}
                      max={3}
                      class="volo-input"
                      title={t("cnt.chap_depth_lbl")}
                      value={ch.depth}
                      onInput={(e) => {
                        const next = [...chapItems];
                        next[idx] = { ...ch, depth: Number((e.currentTarget as HTMLInputElement).value) || 0 };
                        setChapItems(next);
                      }}
                    />
                    <div class="volo-row-actions">
                      <button
                        type="button"
                        class="volo-act-btn"
                        disabled={idx === 0}
                        onClick={() => {
                          if (idx === 0) return;
                          const next = [...chapItems];
                          [next[idx - 1], next[idx]] = [next[idx], next[idx - 1]];
                          setChapItems(next);
                        }}
                      >
                        ↑
                      </button>
                      <button
                        type="button"
                        class="volo-act-btn"
                        disabled={idx === chapItems.length - 1}
                        onClick={() => {
                          if (idx >= chapItems.length - 1) return;
                          const next = [...chapItems];
                          [next[idx + 1], next[idx]] = [next[idx], next[idx + 1]];
                          setChapItems(next);
                        }}
                      >
                        ↓
                      </button>
                      <button
                        type="button"
                        class="volo-act-btn danger"
                        onClick={() => setChapItems(chapItems.filter((_, i) => i !== idx))}
                      >
                        {t("common.delete")}
                      </button>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </AnimatedModal>
        </div>
      )}

      {/* 5. Pages */}
      {sub === "pages" && (
        <div class="volo-card">
          {pageEditorOpen && editPage ? (
            <form onSubmit={savePage} autoComplete="off">
              <div class="volo-sec-head">
                <button type="button" class="volo-btn volo-btn-sm" onClick={closePageEditor}>
                  ← {t("cnt.back_pages")}
                </button>
                <span class="volo-help">
                  {editPage.id
                    ? t("cnt.page_editor_title", { slug: editPage.slug || "about" })
                    : t("cnt.new_page")}
                </span>
              </div>

              <div class="volo-field">
                <input
                  class="volo-editor-title-input"
                  required
                  placeholder={t("cnt.page_title_ph")}
                  value={editPage.title || ""}
                  onInput={(e) => setEditPage({ ...editPage, title: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>

              <div class="volo-field">
                <CM6Editor
                  value={editPage.content_md || ""}
                  onChange={(md) => setEditPage({ ...editPage, content_md: md })}
                />
              </div>

              <div class="volo-subcard">
                <div class="volo-grid-3">
                  <div class="volo-field">
                    <label>{t("cnt.page_slug")}</label>
                    <input
                      class="volo-input"
                      placeholder="about"
                      value={editPage.slug || ""}
                      onInput={(e) => setEditPage({ ...editPage, slug: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("cnt.lbl_page_locale")}</label>
                    <ConsoleSelect
                      value={editPage.lang || "zh-CN"}
                      onChange={(val) => setEditPage({ ...editPage, lang: val })}
                      options={locales.map((loc) => ({
                        value: loc.code,
                        label: loc.name,
                        badge: loc.code,
                      }))}
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("cnt.lbl_page_group")}</label>
                    <input
                      class="volo-input"
                      placeholder="group-about"
                      value={editPage.translation_group || ""}
                      onInput={(e) => setEditPage({ ...editPage, translation_group: (e.currentTarget as HTMLInputElement).value })}
                    />
                  </div>
                </div>

                <div class="volo-check-grid" style={{ marginBottom: 0 }}>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={editPage.is_published !== false}
                      onChange={(e) => setEditPage({ ...editPage, is_published: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{t("cnt.page_published")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={!!editPage.show_toc}
                      onChange={(e) => setEditPage({ ...editPage, show_toc: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{t("cnt.page_show_toc")}</span>
                  </label>
                  <label class="volo-check-item">
                    <input
                      type="checkbox"
                      checked={editPage.allow_comment !== false}
                      onChange={(e) => setEditPage({ ...editPage, allow_comment: (e.currentTarget as HTMLInputElement).checked })}
                    />
                    <span>{t("art.allow_comment")}</span>
                  </label>
                </div>
              </div>

              <div class="volo-form-actions">
                <button type="button" class="volo-btn" onClick={closePageEditor}>
                  {t("common.cancel")}
                </button>
                <button type="submit" class="volo-btn volo-btn-primary">
                  {t("common.save")}
                </button>
              </div>
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
                  onClick={openNewPageEditor}
                >
                  + {t("cnt.new_page")}
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
                    {pagedPages.slice.length === 0 ? (
                      <tr>
                        <td colSpan={5} class="volo-empty-state">{t("tbl.empty")}</td>
                      </tr>
                    ) : (
                      pagedPages.slice.map((p) => (
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
                                <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                                  <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" />
                                  <circle cx="12" cy="12" r="3" />
                                </svg>
                                {p.view_count ?? 0}
                              </span>
                              <span class="volo-metric-item" title={t("nav.comments")}>
                                <svg width="12.5" height="12.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                                  <path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
                                </svg>
                                {p.comment_count ?? 0}
                              </span>
                            </div>
                          </td>
                          <td class="col-date">{formatDateTime(p.updated_at)}</td>
                          <td class="col-act">
                            <div class="volo-row-actions">
                              <a class="volo-act-btn" href={`/${encodeURIComponent(p.slug)}`} target="_blank" rel="noreferrer">
                                {t("common.view")}
                              </a>
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={() => void openExistingPageEditor(p.id)}
                              >
                                {t("common.edit")}
                              </button>
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  await apiFetch(`/api/admin/pages/${p.id}`, { method: "DELETE" });
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
              <Pagination total={pages.length} page={pagedPages.safePage} pageSize={15} onChange={setPageIdx} />
            </>
          )}
        </div>
      )}

      {/* 6. Comments */}
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
                {pagedComments.slice.length === 0 ? (
                  <tr>
                    <td colSpan={4} class="volo-empty-state">{t("cnt.cmt_empty")}</td>
                  </tr>
                ) : (
                  pagedComments.slice.map((row) => {
                    const c = row.comment || row;
                    const canBanIp = isAdmin && Boolean(c.ip_masked || c.client_ip || c.author_name);
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
                            <div class="volo-input-row" style={{ maxWidth: "560px" }}>
                              <input
                                class="volo-input"
                                autoComplete="off"
                                style={{ height: "30px", fontSize: "12.5px", padding: "0 10px" }}
                                placeholder={t("cnt.cmt_reply_ph")}
                                value={replyText[c.id] || ""}
                                onInput={(e) => setReplyText({ ...replyText, [c.id]: (e.currentTarget as HTMLInputElement).value })}
                              />
                              <button
                                type="button"
                                class="volo-act-btn"
                                style={{ marginLeft: "auto", flexShrink: 0 }}
                                onClick={async () => {
                                  const txt = (replyText[c.id] || "").trim();
                                  if (!txt) return;
                                  await apiFetch(`/api/admin/comments/${c.id}/reply`, {
                                    method: "POST",
                                    body: JSON.stringify({ content_md: txt }),
                                  });
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
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  await apiFetch(`/api/admin/comments/${c.id}`, {
                                    method: "PATCH",
                                    body: JSON.stringify({ status: "approved" }),
                                  });
                                  void loadSub();
                                }}
                              >
                                {t("cnt.cmt_approve")}
                              </button>
                            )}
                            {c.status !== "rejected" && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  await apiFetch(`/api/admin/comments/${c.id}`, {
                                    method: "PATCH",
                                    body: JSON.stringify({ status: "rejected" }),
                                  });
                                  void loadSub();
                                }}
                              >
                                {t("cnt.cmt_reject")}
                              </button>
                            )}
                            {canBanIp && (
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  if (!window.confirm(t("cnt.cmt_confirm_ban_ip"))) return;
                                  try {
                                    await apiFetch(`/api/admin/comments/${c.id}/ban-ip`, {
                                      method: "POST",
                                      body: JSON.stringify({
                                        duration_hours: 720,
                                        expires_hours: 720,
                                        reason: "comment spam",
                                        note: "comment spam",
                                      }),
                                    });
                                    showToast(t("cnt.cmt_ip_banned"));
                                    void loadSub();
                                  } catch (err: any) {
                                    showToast(err.message);
                                  }
                                }}
                              >
                                {t("cnt.cmt_ban_ip")}
                              </button>
                            )}
                            <button
                              type="button"
                              class="volo-act-btn danger"
                              onClick={async () => {
                                await apiFetch(`/api/admin/comments/${c.id}`, { method: "DELETE" });
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
          <Pagination total={comments.length} page={pagedComments.safePage} pageSize={15} onChange={setPageIdx} />
        </div>
      )}

      {/* 7. Media Library (Visual Overhaul) */}
      {sub === "media" && (
        <div
          class={`volo-card volo-media-dropzone ${isDraggingMedia ? "is-dragging" : ""}`}
          style={{ padding: "22px 24px" }}
          onDragOver={(e) => {
            e.preventDefault();
            setIsDraggingMedia(true);
          }}
          onDragLeave={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node)) {
              setIsDraggingMedia(false);
            }
          }}
          onDrop={(e) => {
            e.preventDefault();
            setIsDraggingMedia(false);
            if (e.dataTransfer?.files?.length) {
              void uploadMediaFiles(e.dataTransfer.files);
            }
          }}
        >
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <div style={{ display: "flex", alignItems: "center", gap: "10px", flexWrap: "wrap" }}>
                <h3 class="volo-card-title">{t("cnt.media_head")}</h3>
                <span class="volo-badge">
                  {t("cnt.media_total_stat", { count: media.length, mb: totalMB })}
                </span>
                {orphanCount > 0 && (
                  <span class="volo-status-pill warn">
                    <span class="volo-status-dot" />
                    {t("cnt.media_orphan_stat", { count: orphanCount })}
                  </span>
                )}
              </div>
              <p class="volo-sec-desc">{t("cnt.media_desc")}</p>
            </div>

            <div class="volo-filter-controls">
              <div class="volo-filter-pills">
                <button
                  type="button"
                  class={`volo-filter-pill ${mediaView === "grid" ? "active" : ""}`}
                  onClick={() => {
                    setMediaView("grid");
                    setPageIdx(1);
                  }}
                >
                  {t("cnt.media_view_grid")}
                </button>
                <button
                  type="button"
                  class={`volo-filter-pill ${mediaView === "list" ? "active" : ""}`}
                  onClick={() => {
                    setMediaView("list");
                    setPageIdx(1);
                  }}
                >
                  {t("cnt.media_view_list")}
                </button>
              </div>

              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void handleScanOrphans()}>
                {t("cnt.media_scan_orphans")}
              </button>

              {isAdmin && orphanCount > 0 && (
                <button
                  type="button"
                  class="volo-btn volo-btn-sm volo-btn-danger"
                  onClick={() => setCleanOrphansModalOpen(true)}
                >
                  {t("cnt.media_clean_orphans", { count: orphanCount })}
                </button>
              )}

              <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
                {uploadingMedia ? t("ed.uploading") : t("cnt.media_upload")}
                <input
                  type="file"
                  multiple
                  accept="image/*,audio/*,video/*,.pdf,.zip"
                  style={{ display: "none" }}
                  disabled={uploadingMedia}
                  onChange={(e) => {
                    const files = (e.currentTarget as HTMLInputElement).files;
                    if (files && files.length > 0) {
                      void uploadMediaFiles(files);
                    }
                    (e.currentTarget as HTMLInputElement).value = "";
                  }}
                />
              </label>
            </div>
          </div>

          <div class="volo-filter-bar" style={{ marginBottom: "14px" }}>
            <div class="volo-filter-pills">
              <button
                type="button"
                class={`volo-filter-pill ${mediaRefFilter === "all" ? "active" : ""}`}
                onClick={() => {
                  setMediaRefFilter("all");
                  setPageIdx(1);
                }}
              >
                {t("cnt.media_filter_all")} ({media.length})
              </button>
              <button
                type="button"
                class={`volo-filter-pill ${mediaRefFilter === "orphan" ? "active" : ""}`}
                onClick={() => {
                  setMediaRefFilter("orphan");
                  setPageIdx(1);
                }}
              >
                {t("cnt.media_filter_orphan")} ({orphanCount})
              </button>
              <button
                type="button"
                class={`volo-filter-pill ${mediaRefFilter === "used" ? "active" : ""}`}
                onClick={() => {
                  setMediaRefFilter("used");
                  setPageIdx(1);
                }}
              >
                {t("cnt.media_filter_used")} ({Math.max(0, media.length - orphanCount)})
              </button>
            </div>

            <div class="volo-filter-controls">
              <ConsoleSelect
                style={{ width: "152px" }}
                value={mediaKindFilter}
                onChange={(val) => {
                  setMediaKindFilter(val as "all" | "image" | "audio" | "other");
                  setPageIdx(1);
                }}
                options={[
                  { value: "all", label: t("cnt.media_type_all") },
                  { value: "image", label: t("cnt.media_type_image") },
                  { value: "audio", label: t("cnt.media_type_audio") },
                  { value: "other", label: t("cnt.media_type_other") },
                ]}
              />
              <input
                class="volo-input"
                autoComplete="off"
                style={{ width: "220px" }}
                placeholder={t("cnt.media_search_ph")}
                value={mediaQuery}
                onInput={(e) => {
                  setMediaQuery((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
            </div>
          </div>

          {pagedMedia.slice.length === 0 ? (
            <div class="volo-empty-state">{t("tbl.empty")}</div>
          ) : mediaView === "grid" ? (
            <div class="volo-media-grid">
              {pagedMedia.slice.map((m) => {
                const displayName = m.original_name || m.id;
                const mime = String(m.mime_type || "");
                const isImage = mime.startsWith("image/");
                const isAudio = mime.startsWith("audio/");
                const extBadge = (displayName.split(".").pop() || mime.split("/").pop() || "FILE").toUpperCase();
                return (
                  <div key={m.id} class={`volo-media-card ${m.is_orphan ? "is-orphan" : ""}`}>
                    <div class="volo-media-thumb" onClick={() => setPreviewAsset(m)}>
                      {isImage ? (
                        <img src={m.public_url} alt={displayName} loading="lazy" />
                      ) : isAudio ? (
                        <div class="volo-media-type-placeholder">
                          <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
                            <path d="M9 18V5l12-2v13" />
                            <circle cx="6" cy="18" r="3" />
                            <circle cx="18" cy="16" r="3" />
                          </svg>
                          <audio
                            controls
                            preload="none"
                            src={m.public_url}
                            style={{ width: "90%", height: "32px" }}
                            onClick={(e) => e.stopPropagation()}
                          />
                        </div>
                      ) : (
                        <div class="volo-media-type-placeholder">
                          <span class="volo-code-chip">{extBadge}</span>
                        </div>
                      )}
                      <span class={`volo-media-ref-pill ${m.is_orphan ? "orphan" : "used"}`}>
                        {m.is_orphan
                          ? t("cnt.media_badge_orphan")
                          : t("cnt.media_badge_used", { count: m.ref_count || 1 })}
                      </span>
                    </div>

                    <div class="volo-media-body">
                      <div class="volo-media-name" title={displayName}>
                        {displayName}
                      </div>
                      <div class="volo-media-meta">
                        <span>{m.width > 0 ? `${m.width}×${m.height}` : mime || "file"}</span>
                        <span>{formatAssetSize(m.size_bytes)}</span>
                      </div>
                      <div class="volo-media-actions">
                        <button type="button" class="volo-act-btn" onClick={() => copyMediaMarkdown(m)}>
                          {t("cnt.media_copy_md")}
                        </button>
                        <button type="button" class="volo-act-btn" onClick={() => copyMediaUrl(m)}>
                          {t("cnt.media_copy")}
                        </button>
                        <button type="button" class="volo-act-btn danger" onClick={() => void handleDeleteMediaItem(m)}>
                          {t("common.delete")}
                        </button>
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
          ) : (
            <div class="volo-table-wrap">
              <table class="volo-table">
                <thead>
                  <tr>
                    <th class="col-fluid">{t("cnt.media_col_asset")}</th>
                    <th class="col-status">{t("art.status")}</th>
                    <th class="col-cat">{t("cnt.media_col_mime")}</th>
                    <th class="col-num">{t("cnt.media_col_size")}</th>
                    <th class="col-date">{t("cnt.media_col_uploaded")}</th>
                    <th class="col-act col-act-th">{t("art.actions")}</th>
                  </tr>
                </thead>
                <tbody>
                  {pagedMedia.slice.map((m) => {
                    const displayName = m.original_name || m.id;
                    const mime = String(m.mime_type || "");
                    const isImage = mime.startsWith("image/");
                    const extBadge = (displayName.split(".").pop() || mime.split("/").pop() || "FILE").toUpperCase();
                    return (
                      <tr key={m.id}>
                        <td class="col-fluid">
                          <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
                            <button
                              type="button"
                              onClick={() => setPreviewAsset(m)}
                              style={{
                                width: "40px",
                                height: "40px",
                                borderRadius: "7px",
                                border: "1px solid var(--volo-line)",
                                background: "var(--volo-raised)",
                                padding: 0,
                                overflow: "hidden",
                                display: "inline-flex",
                                alignItems: "center",
                                justifyContent: "center",
                                cursor: "pointer",
                                flexShrink: 0,
                              }}
                            >
                              {isImage ? (
                                <img
                                  src={m.public_url}
                                  alt={displayName}
                                  loading="lazy"
                                  style={{ width: "100%", height: "100%", objectFit: "cover" }}
                                />
                              ) : (
                                <span style={{ fontSize: "10px", fontWeight: 700, color: "var(--volo-muted)" }}>
                                  {extBadge.slice(0, 4)}
                                </span>
                              )}
                            </button>
                            <div class="volo-cell-main">
                              <div class="volo-cell-title">
                                <button
                                  type="button"
                                  onClick={() => setPreviewAsset(m)}
                                  style={{
                                    appearance: "none",
                                    border: 0,
                                    background: "transparent",
                                    padding: 0,
                                    color: "inherit",
                                    font: "inherit",
                                    cursor: "pointer",
                                    textAlign: "left",
                                  }}
                                >
                                  {displayName}
                                </button>
                                {m.storage_driver && <span class="volo-mini-tag">{m.storage_driver}</span>}
                              </div>
                              <div class="volo-cell-meta">
                                <span class="volo-code-chip">{m.public_url}</span>
                              </div>
                            </div>
                          </div>
                        </td>
                        <td class="col-status">
                          <span class={`volo-status-pill ${m.is_orphan ? "warn" : "ok"}`}>
                            <span class="volo-status-dot" />
                            {m.is_orphan
                              ? t("cnt.media_badge_orphan")
                              : t("cnt.media_badge_used", { count: m.ref_count || 1 })}
                          </span>
                        </td>
                        <td class="col-cat">
                          <div class="volo-cell-main">
                            <span class="volo-cat-chip">{mime || "file"}</span>
                            {m.width > 0 && <span class="volo-help">{m.width}×{m.height}</span>}
                          </div>
                        </td>
                        <td class="col-num">{formatAssetSize(m.size_bytes)}</td>
                        <td class="col-date">{formatDateTime(m.created_at)}</td>
                        <td class="col-act">
                          <div class="volo-row-actions">
                            <button type="button" class="volo-act-btn" onClick={() => copyMediaMarkdown(m)}>
                              {t("cnt.media_copy_md")}
                            </button>
                            <button type="button" class="volo-act-btn" onClick={() => copyMediaUrl(m)}>
                              {t("cnt.media_copy")}
                            </button>
                            <button type="button" class="volo-act-btn danger" onClick={() => void handleDeleteMediaItem(m)}>
                              {t("common.delete")}
                            </button>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}

          <Pagination total={filteredMedia.length} page={pagedMedia.safePage} pageSize={24} onChange={setPageIdx} />

          {/* Clean Orphans Confirmation Modal */}
          <AnimatedModal
            open={cleanOrphansModalOpen}
            onClose={() => setCleanOrphansModalOpen(false)}
            title={t("cnt.media_clean_orphans", { count: orphanCount })}
            subtitle={t("cnt.media_clean_confirm_desc", {
              count: orphanCount,
              size: formatAssetSize(orphanTotalBytes),
            })}
            maxWidth="540px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setCleanOrphansModalOpen(false)}>
                  {t("common.cancel")}
                </button>
                <button type="button" class="volo-btn volo-btn-danger" onClick={() => void handleCleanOrphans()}>
                  {t("cnt.media_clean_orphans", { count: orphanCount })}
                </button>
              </>
            }
          >
            <div
              style={{
                maxHeight: "220px",
                overflowY: "auto",
                border: "1px solid var(--volo-line)",
                borderRadius: "9px",
                padding: "8px 12px",
                background: "var(--volo-raised)",
                display: "flex",
                flexDirection: "column",
                gap: "6px",
              }}
            >
              {orphanItems.slice(0, 12).map((o) => (
                <div key={o.id} style={{ display: "flex", justifyContent: "space-between", gap: "8px", fontSize: "12.5px" }}>
                  <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                    {o.original_name || o.public_url}
                  </span>
                  <span class="volo-help" style={{ flexShrink: 0 }}>{formatAssetSize(o.size_bytes)}</span>
                </div>
              ))}
              {orphanItems.length > 12 && (
                <div class="volo-help">… +{orphanItems.length - 12}</div>
              )}
            </div>
          </AnimatedModal>

          {/* Lightbox Asset Preview Modal */}
          <AnimatedModal
            open={!!previewAsset}
            onClose={() => setPreviewAsset(null)}
            title={previewAsset?.original_name || t("cnt.media_head")}
            subtitle={previewAsset?.public_url || ""}
            maxWidth="780px"
            footer={
              previewAsset ? (
                <>
                  <button type="button" class="volo-btn" onClick={() => copyMediaMarkdown(previewAsset)}>
                    {t("cnt.media_copy_md")}
                  </button>
                  <button type="button" class="volo-btn" onClick={() => copyMediaUrl(previewAsset)}>
                    {t("cnt.media_copy")}
                  </button>
                  <a
                    class="volo-btn"
                    href={previewAsset.public_url}
                    target="_blank"
                    rel="noreferrer"
                    style={{ textDecoration: "none" }}
                  >
                    {t("cnt.media_open_new")} ↗
                  </a>
                  <button type="button" class="volo-btn volo-btn-primary" onClick={() => setPreviewAsset(null)}>
                    {t("common.close")}
                  </button>
                </>
              ) : null
            }
          >
            {previewAsset && (
              <>
                <div class="volo-lightbox-img-wrap">
                  {String(previewAsset.mime_type || "").startsWith("image/") ? (
                    <img src={previewAsset.public_url} alt={previewAsset.original_name} />
                  ) : String(previewAsset.mime_type || "").startsWith("audio/") ? (
                    <audio controls src={previewAsset.public_url} style={{ width: "100%", maxWidth: "520px" }} />
                  ) : (
                    <div class="volo-media-type-placeholder" style={{ padding: "36px 12px" }}>
                      <span class="volo-code-chip">{previewAsset.mime_type || "FILE"}</span>
                      <span>{previewAsset.original_name}</span>
                    </div>
                  )}
                </div>
                <div class="volo-about-kv-grid">
                  <div class="volo-about-kv">
                    <small>{t("cnt.media_col_asset")}</small>
                    <span>{previewAsset.public_url}</span>
                  </div>
                  <div class="volo-about-kv">
                    <small>{t("cnt.media_col_mime")}</small>
                    <span>
                      {previewAsset.mime_type || "file"}
                      {previewAsset.width > 0 ? ` · ${previewAsset.width}×${previewAsset.height}` : ""}
                    </span>
                  </div>
                  <div class="volo-about-kv">
                    <small>{t("cnt.media_col_size")}</small>
                    <span>{formatAssetSize(previewAsset.size_bytes)} · {previewAsset.storage_driver || "local"}</span>
                  </div>
                  <div class="volo-about-kv">
                    <small>{t("art.status")}</small>
                    <span>
                      {previewAsset.is_orphan
                        ? t("cnt.media_badge_orphan")
                        : t("cnt.media_badge_used", { count: previewAsset.ref_count || 1 })}
                    </span>
                  </div>
                </div>
              </>
            )}
          </AnimatedModal>
        </div>
      )}
    </div>
  );
}
