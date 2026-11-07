import { useEffect, useRef, useState } from "preact/hooks";
import {
  apiFetch,
  currentUserSig,
  dirtyEditorSig,
  openConfirm,
  resetConsoleScroll,
  showToast,
  storageDriverSig,
} from "../api";
import { CM6Editor } from "../components/CM6Editor";
import { MediaAssetRecord, MediaPickButton } from "../components/MediaLibraryModal";
import {
  ActionMenu,
  AnimatedModal,
  BatchBar,
  ConsoleEmpty,
  ConsoleSelect,
  MobileSelectAllBar,
  paginateItems,
  Pagination,
  SwitchToggle,
  TableSkeleton,
  UploadButton,
  useDebouncedValue,
} from "../components/UIControls";
import { availableLocalesSig, formatDateTime, localeSig, t } from "../i18n";

export type ContentSubTab = "pages" | "categories" | "tags" | "series" | "books" | "comments" | "media";

interface BookChapterDraft {
  _uid: string;
  id?: string;
  title: string;
  url: string;
  depth: number;
  sort_order: number;
}

let chapUidSeq = 0;
function nextChapUid(): string {
  chapUidSeq += 1;
  return `ch_${Date.now()}_${chapUidSeq}`;
}

const EMPTY_CAT = { id: "", name: "", slug: "", description: "", sort_order: 10 };
const EMPTY_TAG = { id: "", name: "", slug: "" };
const EMPTY_SER = {
  id: "",
  title: "",
  slug: "",
  description: "",
  cover_url: "",
  sort_order: 10,
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
  sort_order: 10,
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
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(2)} MB`;
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
  const [pageIdx, setPageIdx] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [mediaPageSize, setMediaPageSize] = useState(24);
  const [subTotal, setSubTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const reqSeqRef = useRef(0);

  // Pages (list + full-width Studio Page Editor)
  const [pages, setPages] = useState<any[]>([]);
  const [pageQuery, setPageQuery] = useState("");
  const [pageStatusFilter, setPageStatusFilter] = useState("");
  const debouncedPageQuery = useDebouncedValue(pageQuery, 250);
  const [pageEditorOpen, setPageEditorOpen] = useState(false);
  const [editPage, setEditPage] = useState<any | null>(null);
  const [savingPage, setSavingPage] = useState(false);
  const savePageRef = useRef<() => void>(() => {});

  // Categories
  const [categories, setCategories] = useState<any[]>([]);
  const [catQuery, setCatQuery] = useState("");
  const debouncedCatQuery = useDebouncedValue(catQuery, 250);
  const [catModalOpen, setCatModalOpen] = useState(false);
  const [catForm, setCatForm] = useState({ ...EMPTY_CAT });

  // Tags
  const [tags, setTags] = useState<any[]>([]);
  const [tagFilter, setTagFilter] = useState("");
  const [tagSort, setTagSort] = useState<"count" | "name">("count");
  const debouncedTagFilter = useDebouncedValue(tagFilter, 250);
  const [selTagIds, setSelTagIds] = useState<string[]>([]);
  const [tagModalOpen, setTagModalOpen] = useState(false);
  const [tagForm, setTagForm] = useState({ ...EMPTY_TAG });

  // Series + Paginated Articles checklist
  const [series, setSeries] = useState<any[]>([]);
  const [serQuery, setSerQuery] = useState("");
  const debouncedSerQuery = useDebouncedValue(serQuery, 250);
  const [knownArticlesMap, setKnownArticlesMap] = useState<Record<string, any>>({});
  const [serPickerArticles, setSerPickerArticles] = useState<any[]>([]);
  const [serArtPage, setSerArtPage] = useState(1);
  const [serArtTotal, setSerArtTotal] = useState(0);
  const [serModalOpen, setSerModalOpen] = useState(false);
  const [serForm, setSerForm] = useState({ ...EMPTY_SER });
  const [serArtFilter, setSerArtFilter] = useState("");
  const debouncedSerArtFilter = useDebouncedValue(serArtFilter, 250);

  // Books + Manual Chapters tree modal
  const [books, setBooks] = useState<any[]>([]);
  const [bookQuery, setBookQuery] = useState("");
  const [bookFilter, setBookFilter] = useState("");
  const debouncedBookQuery = useDebouncedValue(bookQuery, 250);
  const [bookModalOpen, setBookModalOpen] = useState(false);
  const [bookForm, setBookForm] = useState({ ...EMPTY_BOOK });
  const [chapModalOpen, setChapModalOpen] = useState(false);
  const [chapBook, setChapBook] = useState<any | null>(null);
  const [chapItems, setChapItems] = useState<BookChapterDraft[]>([]);
  const [chapQuery, setChapQuery] = useState("");
  const [chapPage, setChapPage] = useState(1);
  const [collapsedChapUids, setCollapsedChapUids] = useState<string[]>([]);

  // Comments
  const [comments, setComments] = useState<any[]>([]);
  const [cmtStatus, setCmtStatus] = useState("");
  const [cmtStatusCounts, setCmtStatusCounts] = useState<{
    all?: number;
    pending?: number;
    approved?: number;
    rejected?: number;
  }>({});
  const [cmtQuery, setCmtQuery] = useState("");
  const debouncedCmtQuery = useDebouncedValue(cmtQuery, 250);
  const [selCmtIds, setSelCmtIds] = useState<string[]>([]);
  const [cmtBox, setCmtBox] = useState<{ id: string; mode: "reply" | "edit"; text: string } | null>(null);

  // Media Library
  const [media, setMedia] = useState<MediaAssetRecord[]>([]);
  const [mediaAllTotal, setMediaAllTotal] = useState(0);
  const [orphanCount, setOrphanCount] = useState(0);
  const [mediaView, setMediaView] = useState<"grid" | "list">("grid");
  const [mediaRefFilter, setMediaRefFilter] = useState<"all" | "orphan" | "used">("all");
  const [mediaKindFilter, setMediaKindFilter] = useState<"all" | "image" | "audio" | "other">("all");
  const [mediaSort, setMediaSort] = useState<"newest" | "size" | "name">("newest");
  const [mediaQuery, setMediaQuery] = useState("");
  const debouncedMediaQuery = useDebouncedValue(mediaQuery, 250);
  const [selMediaIds, setSelMediaIds] = useState<string[]>([]);
  const [isDraggingMedia, setIsDraggingMedia] = useState(false);
  const [uploadingMedia, setUploadingMedia] = useState(false);
  const [reconcilingMedia, setReconcilingMedia] = useState(false);
  const [previewAsset, setPreviewAsset] = useState<MediaAssetRecord | null>(null);
  const [assetMetaForm, setAssetMetaForm] = useState({ original_name: "", alt_text: "" });
  const [savingAssetMeta, setSavingAssetMeta] = useState(false);

  useEffect(() => {
    setSub(activeTab);
    setPageEditorOpen(false);
    setEditPage(null);
    dirtyEditorSig.value = false;
    setPageIdx(1);
    setSelTagIds([]);
    setSelCmtIds([]);
    setSelMediaIds([]);
    resetConsoleScroll();
  }, [activeTab]);

  useEffect(() => {
    return () => {
      dirtyEditorSig.value = false;
    };
  }, []);

  const confirmLeaveDirtyPage = async (): Promise<boolean> => {
    if (!pageEditorOpen || !dirtyEditorSig.value) return true;
    return openConfirm({
      title: t("ui.unsaved_title"),
      message: t("ui.unsaved_msg"),
      confirmText: t("ui.unsaved_leave"),
      danger: true,
    });
  };

  const switchSub = async (k: ContentSubTab) => {
    if (!(await confirmLeaveDirtyPage())) return;
    dirtyEditorSig.value = false;
    setSub(k);
    setPageEditorOpen(false);
    setEditPage(null);
    setPageIdx(1);
    setSelTagIds([]);
    setSelCmtIds([]);
    setSelMediaIds([]);
    resetConsoleScroll();
    onNavigate?.(k);
  };

  const loadSub = async () => {
    const reqId = ++reqSeqRef.current;
    setLoading(true);
    try {
      if (sub === "pages") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (pageStatusFilter) qs.set("status", pageStatusFilter);
        if (debouncedPageQuery.trim()) qs.set("q", debouncedPageQuery.trim());
        const r = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/pages?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        setPages(list);
        setSubTotal(typeof r.total === "number" ? r.total : list.length);
      } else if (sub === "categories") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (debouncedCatQuery.trim()) qs.set("q", debouncedCatQuery.trim());
        const r = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/categories?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        setCategories(list);
        setSubTotal(typeof r.total === "number" ? r.total : list.length);
      } else if (sub === "tags") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        qs.set("sort", tagSort);
        if (debouncedTagFilter.trim()) qs.set("q", debouncedTagFilter.trim());
        const r = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/tags?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        setTags(list);
        setSubTotal(typeof r.total === "number" ? r.total : list.length);
      } else if (sub === "series") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (debouncedSerQuery.trim()) qs.set("q", debouncedSerQuery.trim());
        const serRes = await apiFetch<{ items: any[]; total?: number }>(`/api/admin/series?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = serRes.items || [];
        setSeries(list);
        setSubTotal(typeof serRes.total === "number" ? serRes.total : list.length);
      } else if (sub === "books") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (debouncedBookQuery.trim()) qs.set("q", debouncedBookQuery.trim());
        const [bkRes, serRes] = await Promise.all([
          apiFetch<{ items: any[]; total?: number }>(`/api/admin/books?${qs.toString()}`),
          apiFetch<{ items: any[] }>("/api/admin/series?per_page=200"),
        ]);
        if (reqId !== reqSeqRef.current) return;
        const list = bkRes.items || [];
        setBooks(list);
        setSubTotal(typeof bkRes.total === "number" ? bkRes.total : list.length);
        setSeries(serRes.items || []);
      } else if (sub === "comments") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (cmtStatus) qs.set("status", cmtStatus);
        if (debouncedCmtQuery.trim()) qs.set("q", debouncedCmtQuery.trim());
        const r = await apiFetch<{
          items: any[];
          total?: number;
          status_counts?: { all?: number; pending?: number; approved?: number; rejected?: number };
        }>(`/api/admin/comments?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        setComments(list);
        setSubTotal(typeof r.total === "number" ? r.total : list.length);
        if (r.status_counts) setCmtStatusCounts(r.status_counts);
      } else if (sub === "media") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(mediaPageSize));
        qs.set("sort", mediaSort);
        if (mediaKindFilter !== "all") qs.set("mime", mediaKindFilter);
        if (mediaRefFilter === "orphan") qs.set("orphan_only", "1");
        if (debouncedMediaQuery.trim()) qs.set("q", debouncedMediaQuery.trim());
        const r = await apiFetch<{
          items: MediaAssetRecord[];
          total?: number;
          orphan_count?: number;
          storage_driver?: string;
        }>(`/api/admin/media?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        const tot = typeof r.total === "number" ? r.total : list.length;
        setMedia(list);
        setSubTotal(tot);
        if (mediaRefFilter === "all" && mediaKindFilter === "all" && !debouncedMediaQuery.trim()) {
          setMediaAllTotal(tot);
        } else if (mediaAllTotal === 0) {
          setMediaAllTotal(tot);
        }
        setOrphanCount(r.orphan_count ?? list.filter((x) => x.is_orphan).length);
        storageDriverSig.value = r.storage_driver || "local";
      }
    } catch (err: any) {
      if (reqId === reqSeqRef.current) showToast(err.message);
    } finally {
      if (reqId === reqSeqRef.current) setLoading(false);
    }
  };

  useEffect(() => {
    void loadSub();
  }, [
    sub,
    pageIdx,
    pageSize,
    mediaPageSize,
    debouncedPageQuery,
    pageStatusFilter,
    debouncedCatQuery,
    debouncedTagFilter,
    tagSort,
    debouncedSerQuery,
    debouncedBookQuery,
    cmtStatus,
    debouncedCmtQuery,
    mediaRefFilter,
    mediaKindFilter,
    mediaSort,
    debouncedMediaQuery,
  ]);

  // Paginated Series Article Picker loader inside Series Modal
  useEffect(() => {
    if (!serModalOpen) return;
    let cancelled = false;
    const qs = new URLSearchParams();
    qs.set("page", String(serArtPage));
    qs.set("per_page", "20");
    if (debouncedSerArtFilter.trim()) qs.set("q", debouncedSerArtFilter.trim());
    apiFetch<{ items: any[]; total?: number }>(`/api/admin/articles?${qs.toString()}`)
      .then((res) => {
        if (cancelled) return;
        const rows = (res.items || []).map((row: any) => row.article || row);
        setSerPickerArticles(rows);
        setSerArtTotal(typeof res.total === "number" ? res.total : rows.length);
        setKnownArticlesMap((prev) => {
          const next = { ...prev };
          rows.forEach((a: any) => {
            if (a?.id) next[a.id] = a;
          });
          return next;
        });
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [serModalOpen, serArtPage, debouncedSerArtFilter]);

  // Hydrate titles for any already-selected Series article IDs not yet in knownArticlesMap
  useEffect(() => {
    if (!serModalOpen || serForm.article_ids.length === 0) return;
    const missing = serForm.article_ids.filter((id) => !knownArticlesMap[id]);
    if (missing.length === 0) return;
    let cancelled = false;
    Promise.all(
      missing.slice(0, 30).map((id) =>
        apiFetch<any>(`/api/admin/articles/${encodeURIComponent(id)}`)
          .then((r) => r?.article || r)
          .catch(() => null)
      )
    ).then((loaded) => {
      if (cancelled) return;
      setKnownArticlesMap((prev) => {
        const next = { ...prev };
        loaded.forEach((a) => {
          if (a?.id) next[a.id] = a;
        });
        return next;
      });
    });
    return () => {
      cancelled = true;
    };
  }, [serModalOpen, serForm.article_ids.length]);

  const updatePageDraft = (patch: Record<string, any>) => {
    setEditPage((prev: any) => (prev ? { ...prev, ...patch } : prev));
    dirtyEditorSig.value = true;
  };

  const openNewPageEditor = () => {
    dirtyEditorSig.value = false;
    setEditPage({
      title: "",
      slug: "",
      content_md: "",
      template: "default",
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
      dirtyEditorSig.value = false;
      setEditPage({ template: "default", ...full.page });
      setPageEditorOpen(true);
      resetConsoleScroll();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const closePageEditor = async () => {
    if (!(await confirmLeaveDirtyPage())) return;
    dirtyEditorSig.value = false;
    setPageEditorOpen(false);
    setEditPage(null);
    resetConsoleScroll();
  };

  const savePage = async (e?: Event) => {
    e?.preventDefault();
    if (!editPage || savingPage) return;
    if (!String(editPage.title || "").trim()) return;
    setSavingPage(true);
    try {
      const payload = {
        ...editPage,
        title: String(editPage.title || "").trim(),
        translation_group: editPage.translation_group || "",
      };
      if (!editPage.id) {
        const res = await apiFetch<{ page?: any }>("/api/admin/pages", {
          method: "POST",
          body: JSON.stringify(payload),
        });
        if (res?.page) setEditPage({ template: editPage.template || "default", ...res.page });
      } else {
        const res = await apiFetch<{ page?: any }>(`/api/admin/pages/${editPage.id}`, {
          method: "PUT",
          body: JSON.stringify(payload),
        });
        if (res?.page) setEditPage({ template: editPage.template || "default", ...res.page });
      }
      dirtyEditorSig.value = false;
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setSavingPage(false);
    }
  };

  savePageRef.current = () => {
    void savePage();
  };

  useEffect(() => {
    if (!pageEditorOpen) return;
    const onSaveShortcut = () => savePageRef.current();
    window.addEventListener("volo:save", onSaveShortcut);
    return () => window.removeEventListener("volo:save", onSaveShortcut);
  }, [pageEditorOpen]);

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

  const swapCatOrder = async (idx: number, dir: -1 | 1) => {
    const targetIdx = idx + dir;
    if (targetIdx < 0 || targetIdx >= categories.length) return;
    const orders = categories.map((c) => Number(c.sort_order ?? 0));
    const hasTies = new Set(orders).size < categories.length;
    try {
      if (hasTies) {
        const reordered = [...categories];
        [reordered[idx], reordered[targetIdx]] = [reordered[targetIdx], reordered[idx]];
        await Promise.all(
          reordered.map((item, i) => {
            const desired = (i + 1) * 10;
            if (Number(item.sort_order) === desired) return Promise.resolve();
            return apiFetch(`/api/admin/categories/${item.id}`, {
              method: "PUT",
              body: JSON.stringify({ ...item, sort_order: desired }),
            });
          })
        );
      } else {
        const a = categories[idx];
        const b = categories[targetIdx];
        const orderA = Number(a.sort_order ?? (idx + 1) * 10);
        const orderB = Number(b.sort_order ?? (targetIdx + 1) * 10);
        await Promise.all([
          apiFetch(`/api/admin/categories/${a.id}`, { method: "PUT", body: JSON.stringify({ ...a, sort_order: orderB }) }),
          apiFetch(`/api/admin/categories/${b.id}`, { method: "PUT", body: JSON.stringify({ ...b, sort_order: orderA }) }),
        ]);
      }
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const saveTag = async (e?: Event) => {
    e?.preventDefault();
    if (!tagForm.name.trim()) return;
    try {
      const payload = { name: tagForm.name.trim(), slug: tagForm.slug.trim() };
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
    if (!(await openConfirm({ message: t("cnt.confirm_cleanup_tags"), danger: true }))) return;
    try {
      const res = await apiFetch<{ deleted: number; items?: any[] }>("/api/admin/tags/cleanup", {
        method: "POST",
        body: "{}",
      });
      showToast(t("cnt.tag_cleaned", { count: res.deleted ?? 0 }));
      setPageIdx(1);
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const batchDeleteTags = async () => {
    if (!selTagIds.length) return;
    if (!(await openConfirm({ message: t("cnt.confirm_batch_del_tags", { n: selTagIds.length }), danger: true }))) return;
    try {
      await apiFetch("/api/admin/tags/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selTagIds, action: "delete" }),
      });
      setSelTagIds([]);
      showToast(t("common.deleted"));
      void loadSub();
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

  const moveSeriesArticle = (idx: number, dir: -1 | 1) => {
    const targetIdx = idx + dir;
    if (targetIdx < 0 || targetIdx >= serForm.article_ids.length) return;
    const next = [...serForm.article_ids];
    [next[idx], next[targetIdx]] = [next[targetIdx], next[idx]];
    setSerForm({ ...serForm, article_ids: next });
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

  const swapBookOrder = async (idx: number, dir: -1 | 1) => {
    const targetIdx = idx + dir;
    if (targetIdx < 0 || targetIdx >= books.length) return;
    const orders = books.map((b) => Number(b.sort_order ?? 0));
    const hasTies = new Set(orders).size < books.length;
    try {
      if (hasTies) {
        const reordered = [...books];
        [reordered[idx], reordered[targetIdx]] = [reordered[targetIdx], reordered[idx]];
        await Promise.all(
          reordered.map((item, i) => {
            const desired = (i + 1) * 10;
            if (Number(item.sort_order) === desired) return Promise.resolve();
            return apiFetch(`/api/admin/books/${item.id}`, {
              method: "PUT",
              body: JSON.stringify({ ...item, sort_order: desired }),
            });
          })
        );
      } else {
        const a = books[idx];
        const b = books[targetIdx];
        const orderA = Number(a.sort_order ?? (idx + 1) * 10);
        const orderB = Number(b.sort_order ?? (targetIdx + 1) * 10);
        await Promise.all([
          apiFetch(`/api/admin/books/${a.id}`, { method: "PUT", body: JSON.stringify({ ...a, sort_order: orderB }) }),
          apiFetch(`/api/admin/books/${b.id}`, { method: "PUT", body: JSON.stringify({ ...b, sort_order: orderA }) }),
        ]);
      }
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const openManageChapters = async (book: any) => {
    try {
      const res = await apiFetch<{ book: any; chapters: any[] }>(`/api/admin/books/${book.id}`);
      setChapBook(res.book || book);
      setChapQuery("");
      setChapPage(1);
      setCollapsedChapUids([]);
      setChapItems(
        (res.chapters || []).map((ch: any, idx: number) => ({
          _uid: nextChapUid(),
          id: ch.id,
          title: ch.title || "",
          url: ch.url || "",
          depth: Number(ch.depth) || 0,
          sort_order: idx + 1,
        }))
      );
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
        body: JSON.stringify({ ...chapBook, chapters: normalized }),
      });
      setChapModalOpen(false);
      setChapBook(null);
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const runBatchComments = async (action: "approve" | "reject" | "delete" | "ban_ip") => {
    if (!selCmtIds.length) return;
    if (action === "delete" || action === "ban_ip") {
      const msg =
        action === "ban_ip"
          ? t("cnt.cmt_confirm_ban_ip")
          : t("cnt.confirm_batch_del_cmts", { n: selCmtIds.length });
      if (!(await openConfirm({ message: msg, danger: true }))) return;
    }
    try {
      await apiFetch("/api/admin/comments/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selCmtIds, action }),
      });
      setSelCmtIds([]);
      showToast(action === "delete" ? t("common.deleted") : t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const submitCommentBox = async () => {
    if (!cmtBox) return;
    const txt = cmtBox.text.trim();
    if (!txt) return;
    try {
      if (cmtBox.mode === "reply") {
        await apiFetch(`/api/admin/comments/${cmtBox.id}/reply`, {
          method: "POST",
          body: JSON.stringify({ content_md: txt }),
        });
        showToast(t("cnt.cmt_replied"));
      } else {
        await apiFetch(`/api/admin/comments/${cmtBox.id}`, {
          method: "PATCH",
          body: JSON.stringify({ content_md: txt }),
        });
        showToast(t("common.saved"));
      }
      setCmtBox(null);
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
        showToast(okCount > 1 ? t("cnt.media_uploaded_n", { count: okCount }) : t("cnt.media_uploaded"));
        await loadSub();
      }
    } catch (err: any) {
      showToast(err.message);
      if (okCount > 0) await loadSub();
    } finally {
      setUploadingMedia(false);
    }
  };

  const handleReconcileDisk = async () => {
    setReconcilingMedia(true);
    try {
      await apiFetch("/api/admin/media/reconcile", { method: "POST", body: "{}" });
      await loadSub();
      showToast(t("cnt.media_reconciled"));
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setReconcilingMedia(false);
    }
  };

  const handleCleanOrphans = async () => {
    try {
      const res = await apiFetch<{ orphans?: any[] }>("/api/admin/media/orphans", {
        method: "POST",
        body: "{}",
      });
      const list = (res.orphans || []).map((o: any) =>
        typeof o === "string" ? o : String(o.public_url || o.original_name || o.object_key || o.id || "")
      );
      if (list.length === 0) {
        await loadSub();
        showToast(t("cnt.no_orphans"), "ok");
        return;
      }
      const ok = await openConfirm({
        title: t("cnt.clean_orphans"),
        message: t("cnt.confirm_clean_orphans", { n: list.length }),
        items: list,
        danger: true,
      });
      if (!ok) {
        await loadSub();
        return;
      }
      const delRes = await apiFetch<{ deleted_count?: number }>("/api/admin/media/orphans?delete=1", {
        method: "POST",
        body: "{}",
      });
      await loadSub();
      showToast(t("cnt.media_cleaned_orphans", { count: delRes.deleted_count ?? list.length }));
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const handleDeleteMediaItem = async (asset: MediaAssetRecord) => {
    if (!(await openConfirm({ message: t("cnt.media_confirm_del"), danger: true }))) return;
    try {
      await apiFetch(`/api/admin/media/${asset.id}`, { method: "DELETE" });
      if (previewAsset?.id === asset.id) setPreviewAsset(null);
      setSelMediaIds((prev) => prev.filter((id) => id !== asset.id));
      showToast(t("common.deleted"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const batchDeleteMedia = async () => {
    if (!selMediaIds.length) return;
    if (!(await openConfirm({ message: t("cnt.confirm_batch_del_media", { n: selMediaIds.length }), danger: true }))) return;
    try {
      await apiFetch("/api/admin/media/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selMediaIds, action: "delete" }),
      });
      setSelMediaIds([]);
      showToast(t("common.deleted"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const openAssetLightbox = (m: MediaAssetRecord) => {
    setPreviewAsset(m);
    setAssetMetaForm({
      original_name: m.original_name || "",
      alt_text: m.alt_text || "",
    });
  };

  const saveAssetMetadata = async () => {
    if (!previewAsset || savingAssetMeta) return;
    setSavingAssetMeta(true);
    try {
      await apiFetch(`/api/admin/media/${previewAsset.id}`, {
        method: "PATCH",
        body: JSON.stringify({
          original_name: assetMetaForm.original_name.trim(),
          alt_text: assetMetaForm.alt_text.trim(),
        }),
      });
      setPreviewAsset({
        ...previewAsset,
        original_name: assetMetaForm.original_name.trim() || previewAsset.original_name,
        alt_text: assetMetaForm.alt_text.trim(),
      });
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setSavingAssetMeta(false);
    }
  };

  const copyMediaMarkdown = (m: MediaAssetRecord) => {
    const label = m.alt_text || m.original_name || m.id;
    const isImg = String(m.mime_type || "").startsWith("image/");
    const md = isImg ? `![${label}](${m.public_url})` : `[${label}](${m.public_url})`;
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

  const toggleSel = (list: string[], setList: (v: string[]) => void, id: string) => {
    setList(list.includes(id) ? list.filter((x) => x !== id) : [...list, id]);
  };

  const totalMediaBytes = media.reduce((acc, m) => acc + Number(m.size_bytes || 0), 0);
  const totalMB = (totalMediaBytes / (1024 * 1024)).toFixed(2);

  // Client-side fallback filtering ONLY for attributes not sent as server params (e.g. mediaRefFilter === "used", bookFilter)
  const filteredBooks = bookFilter
    ? books.filter((b) => {
        if (bookFilter.startsWith("rs:")) return (b.reading_status || "") === bookFilter.slice(3);
        return (b.source_type || "auto") === bookFilter;
      })
    : books;

  const filteredPages = pageStatusFilter
    ? pages.filter((p) => (pageStatusFilter === "published" ? p.is_published !== false : p.is_published === false))
    : pages;

  const filteredMedia =
    mediaRefFilter === "used" ? media.filter((m) => !m.is_orphan) : media;

  const displayPages = filteredPages.length > pageSize ? paginateItems(filteredPages, pageIdx, pageSize).slice : filteredPages;
  const displayCats = categories.length > pageSize ? paginateItems(categories, pageIdx, pageSize).slice : categories;
  const displayTags = tags.length > pageSize ? paginateItems(tags, pageIdx, pageSize).slice : tags;
  const displaySeries = series.length > pageSize ? paginateItems(series, pageIdx, pageSize).slice : series;
  const displayBooks = filteredBooks.length > pageSize ? paginateItems(filteredBooks, pageIdx, pageSize).slice : filteredBooks;
  const displayComments = comments.length > pageSize ? paginateItems(comments, pageIdx, pageSize).slice : comments;
  const displayMedia =
    filteredMedia.length > mediaPageSize ? paginateItems(filteredMedia, pageIdx, mediaPageSize).slice : filteredMedia;

  return (
    <div>
      {sub !== "comments" && (
        <div class="volo-subtabs">
          {(["categories", "tags", "series", "books", "pages", "media"] as ContentSubTab[]).map((k) => (
            <button
              key={k}
              type="button"
              class={`volo-tab ${sub === k ? "active" : ""}`}
              onClick={() => void switchSub(k)}
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
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                placeholder={t("art.search_ph")}
                value={catQuery}
                onInput={(e) => {
                  setCatQuery((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = categories.reduce((mx, c) => Math.max(mx, Number(c.sort_order || 0)), 0);
                  setCatForm({ ...EMPTY_CAT, sort_order: maxSort + 10 });
                  setCatModalOpen(true);
                }}
              >
                + {t("cnt.new_cat")}
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
                {loading && categories.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : displayCats.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  displayCats.map((c) => {
                    const absIdx = categories.findIndex((x) => x.id === c.id);
                    return (
                      <tr key={c.id}>
                        <td class="col-fluid" data-label={t("cnt.col_name")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">{c.name}</div>
                            {c.description && (
                              <div class="volo-cell-meta">
                                <span>{c.description}</span>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("cnt.col_slug")}>
                          <span class="volo-code-chip">/categories/{c.slug}</span>
                        </td>
                        <td class="col-num" data-label={t("cnt.col_articles")}>
                          <span class="volo-cat-chip">{c.article_count ?? 0}</span>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={absIdx <= 0}
                              title={t("cnt.lbl_sort")}
                              onClick={() => void swapCatOrder(absIdx, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={absIdx < 0 || absIdx >= categories.length - 1}
                              title={t("cnt.lbl_sort")}
                              onClick={() => void swapCatOrder(absIdx, 1)}
                            >
                              ↓
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() => {
                                setCatForm({
                                  id: c.id,
                                  name: c.name || "",
                                  slug: c.slug || "",
                                  description: c.description || "",
                                  sort_order: c.sort_order ?? 10,
                                });
                                setCatModalOpen(true);
                              }}
                            >
                              {t("common.edit")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("common.view"),
                                  href: `/categories/${encodeURIComponent(c.slug)}`,
                                  target: "_blank",
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("cnt.confirm_del_cat"), danger: true }))) return;
                                    await apiFetch(`/api/admin/categories/${c.id}`, { method: "DELETE" });
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
            total={subTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={(sz) => {
              setPageSize(sz);
              setPageIdx(1);
            }}
            onChange={setPageIdx}
          />

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
                    value={catForm.sort_order ?? 10}
                    onInput={(e) => setCatForm({ ...catForm, sort_order: Number((e.currentTarget as HTMLInputElement).value) || 0 })}
                  />
                </div>
              </div>
              <div class="volo-field">
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
              <ConsoleSelect
                searchable={false}
                value={tagSort}
                onChange={(val) => {
                  setTagSort((val as "count" | "name") || "count");
                  setPageIdx(1);
                }}
                options={[
                  { value: "count", label: t("cnt.tag_sort_count") },
                  { value: "name", label: t("cnt.tag_sort_name") },
                ]}
              />
              <input
                class="volo-input volo-search-input"
                placeholder={t("cnt.tag_filter_ph")}
                value={tagFilter}
                onInput={(e) => {
                  setTagFilter((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              {isAdmin && (
                <button type="button" class="volo-btn volo-btn-sm" onClick={() => void cleanupUnusedTags()}>
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

          <BatchBar count={selTagIds.length} onClear={() => setSelTagIds([])}>
            <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteTags()}>
              {t("common.delete")}
            </button>
          </BatchBar>

          <MobileSelectAllBar
            checked={displayTags.length > 0 && displayTags.every((tg) => selTagIds.includes(tg.id))}
            count={selTagIds.length}
            total={displayTags.length}
            onToggle={(on) => {
              const pageIds = displayTags.map((tg) => tg.id);
              setSelTagIds(
                on ? Array.from(new Set([...selTagIds, ...pageIds])) : selTagIds.filter((id) => !pageIds.includes(id))
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
                      checked={displayTags.length > 0 && displayTags.every((tg) => selTagIds.includes(tg.id))}
                      onChange={(e) => {
                        const on = (e.currentTarget as HTMLInputElement).checked;
                        const pageIds = displayTags.map((tg) => tg.id);
                        setSelTagIds(on ? Array.from(new Set([...selTagIds, ...pageIds])) : selTagIds.filter((id) => !pageIds.includes(id)));
                      }}
                    />
                  </th>
                  <th class="col-fluid">{t("cnt.col_name")}</th>
                  <th class="col-cat">{t("cnt.col_slug")}</th>
                  <th class="col-num">{t("cnt.col_articles")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && tags.length === 0 ? (
                  <TableSkeleton cols={5} />
                ) : displayTags.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
                ) : (
                  displayTags.map((tg) => (
                    <tr key={tg.id}>
                      <td class="col-check">
                        <input
                          type="checkbox"
                          checked={selTagIds.includes(tg.id)}
                          onChange={() => toggleSel(selTagIds, setSelTagIds, tg.id)}
                        />
                      </td>
                      <td class="col-fluid" data-label={t("cnt.col_name")}>
                        <div class="volo-cell-title">#{tg.name}</div>
                      </td>
                      <td class="col-cat" data-label={t("cnt.col_slug")}>
                        <span class="volo-code-chip">/tags/{tg.slug || tg.name}</span>
                      </td>
                      <td class="col-num" data-label={t("cnt.col_articles")}>
                        <span class="volo-cat-chip">{tg.article_count ?? 0}</span>
                      </td>
                      <td class="col-act" data-label={t("art.actions")}>
                        <div class="volo-row-actions">
                          <a class="volo-act-btn" href={`/tags/${encodeURIComponent(tg.slug || tg.name)}`} target="_blank" rel="noreferrer">
                            {t("common.view")}
                          </a>
                          <button
                            type="button"
                            class="volo-act-btn"
                            onClick={() => {
                              setTagForm({ id: tg.id, name: tg.name || "", slug: tg.slug || "" });
                              setTagModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              if (!(await openConfirm({ message: t("cnt.confirm_del_tag"), danger: true }))) return;
                              await apiFetch(`/api/admin/tags/${tg.id}`, { method: "DELETE" });
                              showToast(t("common.deleted"));
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
          <Pagination
            total={subTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={(sz) => {
              setPageSize(sz);
              setPageIdx(1);
            }}
            onChange={setPageIdx}
          />

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
              <div class="volo-field">
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
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                placeholder={t("art.search_ph")}
                value={serQuery}
                onInput={(e) => {
                  setSerQuery((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = series.reduce((mx, s) => Math.max(mx, Number(s.sort_order || 0)), 0);
                  setSerForm({ ...EMPTY_SER, sort_order: maxSort + 10 });
                  setSerArtFilter("");
                  setSerArtPage(1);
                  setSerModalOpen(true);
                }}
              >
                + {t("cnt.new_series")}
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
                {loading && series.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : displaySeries.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  displaySeries.map((s) => (
                    <tr key={s.id}>
                      <td class="col-fluid" data-label={t("cnt.col_name")}>
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">{s.title}</div>
                          {s.description && (
                            <div class="volo-cell-meta">
                              <span>{s.description}</span>
                            </div>
                          )}
                        </div>
                      </td>
                      <td class="col-cat" data-label={t("cnt.col_slug")}>
                        <span class="volo-code-chip">/series/{s.slug}</span>
                      </td>
                      <td class="col-num" data-label={t("cnt.col_articles")}>
                        <span class="volo-cat-chip">
                          {Array.isArray(s.article_ids) ? s.article_ids.length : (s.article_count ?? 0)}
                        </span>
                      </td>
                      <td class="col-act" data-label={t("art.actions")}>
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
                                sort_order: s.sort_order ?? 10,
                                article_ids: Array.isArray(s.article_ids) ? [...s.article_ids] : [],
                              });
                              setSerArtFilter("");
                              setSerArtPage(1);
                              setSerModalOpen(true);
                            }}
                          >
                            {t("common.edit")}
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={async () => {
                              if (!(await openConfirm({ message: t("cnt.confirm_del_series"), danger: true }))) return;
                              await apiFetch(`/api/admin/series/${s.id}`, { method: "DELETE" });
                              showToast(t("common.deleted"));
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
          <Pagination
            total={subTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={(sz) => {
              setPageSize(sz);
              setPageIdx(1);
            }}
            onChange={setPageIdx}
          />

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
              <div class="volo-auto-grid">
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
                    value={serForm.sort_order ?? 10}
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
                  <MediaPickButton filterKind="image" onPick={(u) => setSerForm({ ...serForm, cover_url: u })} />
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

              {serForm.article_ids.length > 0 && (
                <div class="volo-subcard" style={{ maxHeight: "196px", overflowY: "auto" }}>
                  <div class="volo-subcard-title">
                    <span>
                      {t("cnt.ser_articles_lbl")} ({serForm.article_ids.length})
                    </span>
                  </div>
                  {serForm.article_ids.map((aid, idx) => {
                    const art = knownArticlesMap[aid];
                    const titleText = art?.title || aid;
                    return (
                      <div key={aid} class="volo-input-row" style={{ marginBottom: "6px", flexWrap: "nowrap" }}>
                        <span class="volo-code-chip" style={{ flexShrink: 0 }}>
                          #{idx + 1}
                        </span>
                        <span
                          style={{
                            flex: 1,
                            minWidth: 0,
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                            fontSize: "13px",
                          }}
                          title={titleText}
                        >
                          {titleText}
                        </span>
                        <div class="volo-row-actions" style={{ flexShrink: 0 }}>
                          <button
                            type="button"
                            class="volo-act-btn"
                            disabled={idx === 0}
                            onClick={() => moveSeriesArticle(idx, -1)}
                          >
                            ↑
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn"
                            disabled={idx === serForm.article_ids.length - 1}
                            onClick={() => moveSeriesArticle(idx, 1)}
                          >
                            ↓
                          </button>
                          <button
                            type="button"
                            class="volo-act-btn danger"
                            onClick={() =>
                              setSerForm({
                                ...serForm,
                                article_ids: serForm.article_ids.filter((id) => id !== aid),
                              })
                            }
                          >
                            ×
                          </button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}

              <div class="volo-field">
                <div class="volo-sec-head">
                  <label>
                    {t("cnt.ser_articles_lbl")} ({serForm.article_ids.length})
                  </label>
                  <input
                    class="volo-input volo-search-input"
                    placeholder={t("ed.qi_search_art")}
                    value={serArtFilter}
                    onInput={(e) => {
                      setSerArtFilter((e.currentTarget as HTMLInputElement).value);
                      setSerArtPage(1);
                    }}
                  />
                </div>
                <div class="volo-subcard" style={{ maxHeight: "220px", overflowY: "auto" }}>
                  {serPickerArticles.length === 0 ? (
                    <div class="volo-help">{t("tbl.empty")}</div>
                  ) : (
                    serPickerArticles.map((art) => {
                      const checked = serForm.article_ids.includes(art.id);
                      return (
                        <label
                          key={art.id}
                          class="volo-check-item"
                          style={{ justifyContent: "space-between", gap: "8px" }}
                        >
                          <span class="volo-cell-title" style={{ minWidth: 0, flex: 1 }}>
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
                            <span
                              style={{
                                minWidth: 0,
                                overflow: "hidden",
                                textOverflow: "ellipsis",
                                whiteSpace: "nowrap",
                              }}
                            >
                              {art.title || art.slug}
                            </span>
                          </span>
                          <span class="volo-code-chip" style={{ flexShrink: 0 }}>
                            /{art.slug}
                          </span>
                        </label>
                      );
                    })
                  )}
                </div>
                <Pagination total={serArtTotal} page={serArtPage} pageSize={20} onChange={setSerArtPage} />
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
            <div class="volo-filter-controls">
              <ConsoleSelect
                searchable={false}
                value={bookFilter}
                onChange={(val) => {
                  setBookFilter(val);
                  setPageIdx(1);
                }}
                options={[
                  { value: "", label: t("art.all") },
                  ...(books.some((b) => b.reading_status)
                    ? [
                        { value: "rs:reading", label: t("cnt.book_rs_reading") },
                        { value: "rs:finished", label: t("cnt.book_rs_finished") },
                        { value: "rs:wishlist", label: t("cnt.book_rs_wishlist") },
                      ]
                    : []),
                  { value: "auto", label: t("cnt.book_src_auto") },
                  { value: "gitbook", label: t("cnt.book_src_gitbook") },
                  { value: "mdbook", label: t("cnt.book_src_mdbook") },
                  { value: "vuepress", label: t("cnt.book_src_vuepress") },
                  { value: "series", label: t("cnt.book_src_series") },
                  { value: "manual", label: t("cnt.book_src_manual") },
                ]}
              />
              <input
                class="volo-input volo-search-input"
                placeholder={t("ed.qi_search_books")}
                value={bookQuery}
                onInput={(e) => {
                  setBookQuery((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-primary volo-btn-sm"
                onClick={() => {
                  const maxSort = books.reduce((mx, b) => Math.max(mx, Number(b.sort_order || 0)), 0);
                  setBookForm({ ...EMPTY_BOOK, sort_order: maxSort + 10 });
                  setBookModalOpen(true);
                }}
              >
                + {t("cnt.new_book")}
              </button>
            </div>
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
                {loading && books.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : displayBooks.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  displayBooks.map((b) => {
                    const absIdx = books.findIndex((x) => x.id === b.id);
                    return (
                      <tr key={b.id}>
                        <td class="col-fluid" data-label={t("cnt.book_col")}>
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
                        <td class="col-cat" data-label={t("art.status")}>
                          <span class="volo-cat-chip">{bookSourceLabel(b.source_type || "auto")}</span>
                        </td>
                        <td class="col-num" data-label={t("cnt.col_articles")}>
                          <span class="volo-status-pill ok">
                            <span class="volo-status-dot" />
                            {t("cnt.book_chapters", { count: b.chapter_count ?? 0 })}
                          </span>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={absIdx <= 0}
                              title={t("cnt.lbl_sort")}
                              onClick={() => void swapBookOrder(absIdx, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={absIdx < 0 || absIdx >= books.length - 1}
                              title={t("cnt.lbl_sort")}
                              onClick={() => void swapBookOrder(absIdx, 1)}
                            >
                              ↓
                            </button>
                            <button type="button" class="volo-act-btn" onClick={() => void openManageChapters(b)}>
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
                            <ActionMenu
                              items={[
                                {
                                  label: t("common.view"),
                                  href: `/books/${encodeURIComponent(b.slug)}`,
                                  target: "_blank",
                                },
                                {
                                  label: t("cnt.book_sync_toc"),
                                  hidden: !(b.source_url || b.series_id),
                                  onClick: async () => {
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
                                  },
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("cnt.confirm_del_book"), danger: true }))) return;
                                    await apiFetch(`/api/admin/books/${b.id}`, { method: "DELETE" });
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
            total={bookFilter ? filteredBooks.length : subTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={(sz) => {
              setPageSize(sz);
              setPageIdx(1);
            }}
            onChange={setPageIdx}
          />

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
              <div class="volo-auto-grid">
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
                    value={bookForm.sort_order ?? 10}
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
                  <MediaPickButton filterKind="image" onPick={(u) => setBookForm({ ...bookForm, cover_url: u })} />
                </div>
              </div>

              <div class="volo-auto-grid">
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

              <div class="volo-field">
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
            maxWidth="760px"
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
            <div class="volo-sec-head" style={{ marginBottom: "10px" }}>
              <span class="volo-help">{t("cnt.book_chapters", { count: chapItems.length })}</span>
              <div class="volo-filter-controls">
                {chapItems.length > 6 && (
                  <input
                    class="volo-input volo-search-input"
                    placeholder={t("cnt.chap_search_ph")}
                    value={chapQuery}
                    onInput={(e) => {
                      setChapQuery((e.currentTarget as HTMLInputElement).value);
                      setChapPage(1);
                    }}
                  />
                )}
                <button
                  type="button"
                  class="volo-btn volo-btn-sm volo-btn-primary"
                  onClick={() => {
                    const next = [
                      ...chapItems,
                      { _uid: nextChapUid(), title: "", url: "", depth: 0, sort_order: chapItems.length + 1 },
                    ];
                    setChapItems(next);
                    setChapQuery("");
                    setChapPage(Math.max(1, Math.ceil(next.length / 80)));
                  }}
                >
                  + {t("cnt.chap_add")}
                </button>
              </div>
            </div>
            {chapItems.length === 0 ? (
              <ConsoleEmpty />
            ) : (
              (() => {
                const q = chapQuery.trim().toLowerCase();
                const collapsedSet = new Set(collapsedChapUids);
                const visibleRows: Array<{ ch: BookChapterDraft; idx: number; hasChildren: boolean; isCollapsed: boolean }> = [];
                let activeTopParentCollapsed = false;

                for (let i = 0; i < chapItems.length; i++) {
                  const ch = chapItems[i];
                  const d = Math.max(0, Math.min(3, Number(ch.depth) || 0));
                  const hasChildren = d === 0 && i + 1 < chapItems.length && (Number(chapItems[i + 1].depth) || 0) > 0;
                  const isCollapsed = d === 0 && collapsedSet.has(ch._uid);
                  if (d === 0) {
                    activeTopParentCollapsed = isCollapsed;
                  } else if (!q && activeTopParentCollapsed) {
                    continue;
                  }
                  if (q && !ch.title.toLowerCase().includes(q) && !ch.url.toLowerCase().includes(q)) {
                    continue;
                  }
                  visibleRows.push({ ch, idx: i, hasChildren, isCollapsed });
                }

                const pagedRows = paginateItems(visibleRows, chapPage, 80);

                return visibleRows.length === 0 ? (
                  <ConsoleEmpty />
                ) : (
                  <>
                    <div class="volo-chap-tree">
                      {pagedRows.slice.map(({ ch, idx, hasChildren, isCollapsed }) => {
                        const depth = Math.max(0, Math.min(3, Number(ch.depth) || 0));
                        return (
                          <div
                            key={ch._uid}
                            class="volo-chap-row"
                            style={{ paddingLeft: `${8 + depth * 20}px` }}
                          >
                            {hasChildren ? (
                              <button
                                type="button"
                                class="volo-chap-btn"
                                title={isCollapsed ? t("common.open") : t("common.close")}
                                onClick={() =>
                                  setCollapsedChapUids((prev) =>
                                    prev.includes(ch._uid) ? prev.filter((u) => u !== ch._uid) : [...prev, ch._uid]
                                  )
                                }
                              >
                                {isCollapsed ? "▸" : "▾"}
                              </button>
                            ) : (
                              <span
                                style={{
                                  width: "26px",
                                  textAlign: "center",
                                  color: "var(--volo-muted)",
                                  fontSize: "11px",
                                  flexShrink: 0,
                                }}
                              >
                                {depth === 0 ? "•" : "↳"}
                              </span>
                            )}

                            <div class="volo-chap-inputs">
                              <input
                                class="volo-input"
                                placeholder={t("cnt.chap_title_ph")}
                                value={ch.title}
                                onInput={(e) => {
                                  const val = (e.currentTarget as HTMLInputElement).value;
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    next[idx] = { ...next[idx], title: val };
                                    return next;
                                  });
                                }}
                              />
                              <input
                                class="volo-input"
                                placeholder={t("cnt.chap_url_ph")}
                                value={ch.url}
                                onInput={(e) => {
                                  const val = (e.currentTarget as HTMLInputElement).value;
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    next[idx] = { ...next[idx], url: val };
                                    return next;
                                  });
                                }}
                              />
                            </div>

                            <div class="volo-chap-acts">
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={depth <= 0}
                                title={t("cnt.chap_depth_lbl")}
                                onClick={() => {
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    next[idx] = { ...next[idx], depth: Math.max(0, depth - 1) };
                                    return next;
                                  });
                                }}
                              >
                                ←
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={depth >= 3}
                                title={t("cnt.chap_depth_lbl")}
                                onClick={() => {
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    next[idx] = { ...next[idx], depth: Math.min(3, depth + 1) };
                                    return next;
                                  });
                                }}
                              >
                                →
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                title={t("cnt.chap_add")}
                                onClick={() => {
                                  setCollapsedChapUids((prev) => prev.filter((u) => u !== ch._uid));
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    next.splice(idx + 1, 0, {
                                      _uid: nextChapUid(),
                                      title: "",
                                      url: "",
                                      depth,
                                      sort_order: idx + 2,
                                    });
                                    return next;
                                  });
                                }}
                              >
                                +
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={idx === 0}
                                onClick={() => {
                                  if (idx === 0) return;
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    [next[idx - 1], next[idx]] = [next[idx], next[idx - 1]];
                                    return next;
                                  });
                                }}
                              >
                                ↑
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={idx >= chapItems.length - 1}
                                onClick={() => {
                                  if (idx >= chapItems.length - 1) return;
                                  setChapItems((prev) => {
                                    const next = [...prev];
                                    [next[idx + 1], next[idx]] = [next[idx], next[idx + 1]];
                                    return next;
                                  });
                                }}
                              >
                                ↓
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn danger"
                                title={t("common.delete")}
                                onClick={async () => {
                                  const isEmpty = !ch.title.trim() && !ch.url.trim();
                                  if (!isEmpty && !(await openConfirm({ message: t("cnt.confirm_del_chap"), danger: true }))) {
                                    return;
                                  }
                                  setChapItems((prev) => prev.filter((_, i) => i !== idx));
                                }}
                              >
                                ×
                              </button>
                            </div>
                          </div>
                        );
                      })}
                    </div>
                    <Pagination
                      total={visibleRows.length}
                      page={pagedRows.safePage}
                      pageSize={80}
                      onChange={setChapPage}
                    />
                  </>
                );
              })()
            )}
          </AnimatedModal>
        </div>
      )}

      {/* 5. Standalone Pages */}
      {sub === "pages" && (
        <div class="volo-card">
          {pageEditorOpen && editPage ? (
            <form onSubmit={(e) => void savePage(e)} autoComplete="off">
              <div class="volo-sec-head">
                <div class="volo-filter-controls">
                  <button type="button" class="volo-btn volo-btn-sm" onClick={() => void closePageEditor()}>
                    ← {t("cnt.back_pages")}
                  </button>
                  <span class="volo-help">
                    {editPage.id
                      ? t("cnt.page_editor_title", { slug: editPage.slug || "about" })
                      : t("cnt.new_page")}
                  </span>
                </div>
                <div class="volo-filter-controls">
                  {editPage.slug && (
                    <a
                      class="volo-btn volo-btn-sm"
                      href={`/${encodeURIComponent(editPage.slug)}`}
                      target="_blank"
                      rel="noreferrer"
                    >
                      {t("common.preview")} ↗
                    </a>
                  )}
                  <button type="submit" class="volo-btn volo-btn-primary volo-btn-sm" disabled={savingPage}>
                    {t("common.save")}
                  </button>
                </div>
              </div>

              <div class="volo-field">
                <input
                  class="volo-editor-title-input"
                  required
                  placeholder={t("cnt.page_title_ph")}
                  value={editPage.title || ""}
                  onInput={(e) => updatePageDraft({ title: (e.currentTarget as HTMLInputElement).value })}
                />
              </div>
              <div class="volo-field">
                <CM6Editor
                  value={editPage.content_md || ""}
                  onChange={(md) => updatePageDraft({ content_md: md })}
                  onSave={() => void savePage()}
                />
              </div>

              <div class="volo-editor-panels">
                <div class="volo-inspector-box">
                  <div class="volo-inspector-title">{t("ov.panel_publish")}</div>
                  <div class="volo-auto-grid">
                    <div class="volo-field">
                      <label>{t("cnt.page_slug")}</label>
                      <input
                        class="volo-input"
                        placeholder="about"
                        value={editPage.slug || ""}
                        onInput={(e) => updatePageDraft({ slug: (e.currentTarget as HTMLInputElement).value })}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("cnt.page_template")}</label>
                      <ConsoleSelect
                        value={editPage.template || "default"}
                        onChange={(val) => updatePageDraft({ template: val })}
                        options={[
                          { value: "default", label: t("cnt.page_tpl_default") },
                          { value: "wide", label: t("cnt.page_tpl_wide") },
                          { value: "minimal", label: t("cnt.page_tpl_minimal") },
                        ]}
                      />
                    </div>
                    <div class="volo-field">
                      <label>{t("cnt.lbl_page_locale")}</label>
                      <ConsoleSelect
                        value={editPage.lang || "zh-CN"}
                        onChange={(val) => updatePageDraft({ lang: val })}
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
                        onInput={(e) =>
                          updatePageDraft({ translation_group: (e.currentTarget as HTMLInputElement).value })
                        }
                      />
                    </div>
                  </div>
                  <div class="volo-check-grid" style={{ marginBottom: 0 }}>
                    <SwitchToggle
                      checked={editPage.is_published !== false}
                      onChange={(v) => updatePageDraft({ is_published: v })}
                      label={t("cnt.page_published")}
                    />
                    <SwitchToggle
                      checked={!!editPage.show_toc}
                      onChange={(v) => updatePageDraft({ show_toc: v })}
                      label={t("cnt.page_show_toc")}
                    />
                    <SwitchToggle
                      checked={editPage.allow_comment !== false}
                      onChange={(v) => updatePageDraft({ allow_comment: v })}
                      label={t("art.allow_comment")}
                    />
                  </div>
                </div>
              </div>

              <div class="volo-form-actions">
                <button type="button" class="volo-btn" onClick={() => void closePageEditor()}>
                  {t("common.cancel")}
                </button>
                <button type="submit" class="volo-btn volo-btn-primary" disabled={savingPage}>
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
                <div class="volo-filter-controls">
                  <ConsoleSelect
                    searchable={false}
                    value={pageStatusFilter}
                    onChange={(val) => {
                      setPageStatusFilter(val);
                      setPageIdx(1);
                    }}
                    options={[
                      { value: "", label: t("art.all") },
                      { value: "published", label: t("cnt.page_published") },
                      { value: "draft", label: t("cnt.page_hidden") },
                    ]}
                  />
                  <input
                    class="volo-input volo-search-input"
                    placeholder={t("art.search_ph")}
                    value={pageQuery}
                    onInput={(e) => {
                      setPageQuery((e.currentTarget as HTMLInputElement).value);
                      setPageIdx(1);
                    }}
                  />
                  <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={openNewPageEditor}>
                    + {t("cnt.new_page")}
                  </button>
                </div>
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
                    {loading && pages.length === 0 ? (
                      <TableSkeleton cols={5} />
                    ) : displayPages.length === 0 ? (
                      <ConsoleEmpty colSpan={5} />
                    ) : (
                      displayPages.map((p) => (
                        <tr key={p.id}>
                          <td class="col-fluid" data-label={t("cnt.page_col")}>
                            <div class="volo-cell-main">
                              <div class="volo-cell-title">{p.title}</div>
                              <div class="volo-cell-meta">
                                <span class="volo-code-chip">/{p.slug}</span>
                                <span>{p.lang}</span>
                              </div>
                            </div>
                          </td>
                          <td class="col-status" data-label={t("art.status")}>
                            <span class={`volo-status-pill ${p.is_published ? "ok" : "warn"}`}>
                              <span class="volo-status-dot" />
                              {p.is_published ? t("cnt.page_published") : t("cnt.page_hidden")}
                            </span>
                          </td>
                          <td class="col-num" data-label={t("art.metrics")}>
                            <div class="volo-metric-pair">
                              <span class="volo-metric-item" title="PV">
                                {p.view_count ?? 0}
                              </span>
                              <span class="volo-metric-item" title={t("nav.comments")}>
                                💬 {p.comment_count ?? 0}
                              </span>
                            </div>
                          </td>
                          <td class="col-date" data-label={t("art.updated")}>
                            {formatDateTime(p.updated_at)}
                          </td>
                          <td class="col-act" data-label={t("art.actions")}>
                            <div class="volo-row-actions">
                              <a class="volo-act-btn" href={`/${encodeURIComponent(p.slug)}`} target="_blank" rel="noreferrer">
                                {t("common.preview")} ↗
                              </a>
                              <button type="button" class="volo-act-btn" onClick={() => void openExistingPageEditor(p.id)}>
                                {t("common.edit")}
                              </button>
                              <button
                                type="button"
                                class="volo-act-btn danger"
                                onClick={async () => {
                                  if (!(await openConfirm({ message: t("cnt.confirm_del_page"), danger: true }))) return;
                                  await apiFetch(`/api/admin/pages/${p.id}`, { method: "DELETE" });
                                  showToast(t("common.deleted"));
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
              <Pagination
                total={pageStatusFilter ? filteredPages.length : subTotal}
                page={pageIdx}
                pageSize={pageSize}
                onPageSizeChange={(sz) => {
                  setPageSize(sz);
                  setPageIdx(1);
                }}
                onChange={setPageIdx}
              />
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
            <div class="volo-filter-controls">
              <input
                class="volo-input volo-search-input"
                placeholder={t("cnt.cmt_search_ph")}
                value={cmtQuery}
                onInput={(e) => {
                  setCmtQuery((e.currentTarget as HTMLInputElement).value);
                  setPageIdx(1);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void loadSub();
                }}
              />
              <div class="volo-filter-pills">
                {[
                  { val: "", label: t("cnt.cmt_all"), count: cmtStatusCounts.all },
                  { val: "pending", label: t("cnt.cmt_pending"), count: cmtStatusCounts.pending },
                  { val: "approved", label: t("cnt.cmt_approved"), count: cmtStatusCounts.approved },
                  { val: "rejected", label: t("cnt.cmt_rejected"), count: cmtStatusCounts.rejected },
                ].map((it) => (
                  <button
                    key={it.val}
                    type="button"
                    class={`volo-filter-pill ${cmtStatus === it.val ? "active" : ""}`}
                    onClick={() => {
                      setCmtStatus(it.val);
                      setPageIdx(1);
                    }}
                  >
                    {it.label}
                    {typeof it.count === "number" ? ` (${it.count})` : ""}
                  </button>
                ))}
              </div>
            </div>
          </div>

          <BatchBar count={selCmtIds.length} onClear={() => setSelCmtIds([])}>
            <button type="button" class="volo-act-btn" onClick={() => void runBatchComments("approve")}>
              {t("cnt.cmt_approve")}
            </button>
            <button type="button" class="volo-act-btn" onClick={() => void runBatchComments("reject")}>
              {t("cnt.cmt_reject")}
            </button>
            {isAdmin && (
              <button type="button" class="volo-act-btn danger" onClick={() => void runBatchComments("ban_ip")}>
                {t("cnt.cmt_ban_ip")}
              </button>
            )}
            <button type="button" class="volo-act-btn danger" onClick={() => void runBatchComments("delete")}>
              {t("common.delete")}
            </button>
          </BatchBar>

          <MobileSelectAllBar
            checked={
              displayComments.length > 0 &&
              displayComments.every((row) => selCmtIds.includes((row.comment || row).id))
            }
            count={selCmtIds.length}
            total={displayComments.length}
            onToggle={(on) => {
              const pageIds = displayComments.map((row) => (row.comment || row).id);
              setSelCmtIds(
                on
                  ? Array.from(new Set([...selCmtIds, ...pageIds]))
                  : selCmtIds.filter((id) => !pageIds.includes(id))
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
                      checked={
                        displayComments.length > 0 &&
                        displayComments.every((row) => selCmtIds.includes((row.comment || row).id))
                      }
                      onChange={(e) => {
                        const on = (e.currentTarget as HTMLInputElement).checked;
                        const pageIds = displayComments.map((row) => (row.comment || row).id);
                        setSelCmtIds(
                          on
                            ? Array.from(new Set([...selCmtIds, ...pageIds]))
                            : selCmtIds.filter((id) => !pageIds.includes(id))
                        );
                      }}
                    />
                  </th>
                  <th class="col-fluid">{t("cnt.cmt_col_content")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-date">{t("art.updated")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading && comments.length === 0 ? (
                  <TableSkeleton cols={5} />
                ) : displayComments.length === 0 ? (
                  <ConsoleEmpty text={t("cnt.cmt_empty")} colSpan={5} />
                ) : (
                  displayComments.map((row) => {
                    const c = row.comment || row;
                    const canBanIp = isAdmin && Boolean(c.ip_masked || c.client_ip || c.author_name);
                    const targetTitle = row.target_title || row.target_slug || c.target_id;
                    const targetHref =
                      row.target_url ||
                      (c.target_type === "page"
                        ? `/${encodeURIComponent(row.target_slug || c.target_id)}`
                        : `/posts/${encodeURIComponent(row.target_slug || c.target_id)}`);
                    const isBoxOpen = cmtBox?.id === c.id;

                    return (
                      <tr key={c.id}>
                        <td class="col-check">
                          <input
                            type="checkbox"
                            checked={selCmtIds.includes(c.id)}
                            onChange={() => toggleSel(selCmtIds, setSelCmtIds, c.id)}
                          />
                        </td>
                        <td class="col-fluid" data-label={t("cnt.cmt_col_content")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{c.author_name}</span>
                              {c.geo_region && <span class="volo-mini-tag">{c.geo_region}</span>}
                              {(c.client_ip || c.ip_masked) && (
                                <span class="volo-code-chip">{c.client_ip || c.ip_masked}</span>
                              )}
                              <a class="volo-help" href={targetHref} target="_blank" rel="noreferrer">
                                {t("cnt.cmt_on", { target: targetTitle })} ↗
                              </a>
                            </div>
                            <div
                              style={{
                                whiteSpace: "pre-wrap",
                                overflowWrap: "anywhere",
                                wordBreak: "break-word",
                                fontSize: "13.5px",
                                margin: "3px 0",
                              }}
                            >
                              {c.content_md}
                            </div>
                            {isBoxOpen && cmtBox && (
                              <div style={{ marginTop: "6px" }}>
                                <textarea
                                  class="volo-textarea"
                                  rows={3}
                                  placeholder={cmtBox.mode === "reply" ? t("cnt.cmt_reply_ph") : ""}
                                  value={cmtBox.text}
                                  onInput={(e) =>
                                    setCmtBox({ ...cmtBox, text: (e.currentTarget as HTMLTextAreaElement).value })
                                  }
                                  onKeyDown={(e) => {
                                    if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                                      e.preventDefault();
                                      void submitCommentBox();
                                    }
                                    if (e.key === "Escape") setCmtBox(null);
                                  }}
                                />
                                <div class="volo-row-actions" style={{ marginTop: "6px" }}>
                                  <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={() => void submitCommentBox()}>
                                    {cmtBox.mode === "reply" ? t("cnt.cmt_reply_btn") : t("common.save")}
                                  </button>
                                  <button type="button" class="volo-btn volo-btn-sm" onClick={() => setCmtBox(null)}>
                                    {t("common.cancel")}
                                  </button>
                                </div>
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <span
                            class={`volo-status-pill ${
                              c.status === "approved" ? "ok" : c.status === "rejected" ? "danger" : "warn"
                            }`}
                          >
                            <span class="volo-status-dot" />
                            {cmtStatusLabel(c.status)}
                          </span>
                        </td>
                        <td class="col-date" data-label={t("art.updated")}>
                          {formatDateTime(c.created_at)}
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
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
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() =>
                                setCmtBox(
                                  cmtBox?.id === c.id && cmtBox?.mode === "reply"
                                    ? null
                                    : { id: c.id, mode: "reply", text: "" }
                                )
                              }
                            >
                              {t("cnt.cmt_reply_btn")}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={() =>
                                setCmtBox(
                                  cmtBox?.id === c.id && cmtBox?.mode === "edit"
                                    ? null
                                    : { id: c.id, mode: "edit", text: c.content_md || "" }
                                )
                              }
                            >
                              {t("common.edit")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("cnt.cmt_reject"),
                                  hidden: c.status === "rejected",
                                  onClick: async () => {
                                    await apiFetch(`/api/admin/comments/${c.id}`, {
                                      method: "PATCH",
                                      body: JSON.stringify({ status: "rejected" }),
                                    });
                                    void loadSub();
                                  },
                                },
                                {
                                  label: t("cnt.cmt_ban_ip"),
                                  hidden: !canBanIp,
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("cnt.cmt_confirm_ban_ip"), danger: true }))) return;
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
                                  },
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("cnt.confirm_del_cmt"), danger: true }))) return;
                                    await apiFetch(`/api/admin/comments/${c.id}`, { method: "DELETE" });
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
            total={subTotal}
            page={pageIdx}
            pageSize={pageSize}
            onPageSizeChange={(sz) => {
              setPageSize(sz);
              setPageIdx(1);
            }}
            onChange={setPageIdx}
          />
        </div>
      )}

      {/* 7. Media Library */}
      {sub === "media" && (() => {
        const effectiveMediaTotal = mediaAllTotal || subTotal || media.length;
        return (
          <div
            class={`volo-card volo-media-dropzone ${isDraggingMedia ? "is-dragging" : ""}`}
            onDragOver={(e) => {
              e.preventDefault();
              setIsDraggingMedia(true);
            }}
            onDragLeave={(e) => {
              if (!e.currentTarget.contains(e.relatedTarget as Node)) setIsDraggingMedia(false);
            }}
            onDrop={(e) => {
              e.preventDefault();
              setIsDraggingMedia(false);
              if (e.dataTransfer?.files?.length) void uploadMediaFiles(e.dataTransfer.files);
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <div class="volo-cell-title">
                  <h3 class="volo-card-title">{t("cnt.media_head")}</h3>
                  <span class="volo-badge">{t("cnt.media_total_stat", { count: effectiveMediaTotal, mb: totalMB })}</span>
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

                {(storageDriverSig.value || "local") === "local" && (
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    disabled={reconcilingMedia}
                    onClick={() => void handleReconcileDisk()}
                  >
                    {t("cnt.media_scan_btn")}
                  </button>
                )}

                {isAdmin && (
                  <button type="button" class="volo-btn volo-btn-sm volo-btn-danger" onClick={() => void handleCleanOrphans()}>
                    {orphanCount > 0 ? t("cnt.media_clean_orphans", { count: orphanCount }) : t("cnt.clean_orphans")}
                  </button>
                )}

                <UploadButton
                  accept="image/*,audio/*,video/*,.pdf,.zip"
                  label={t("cnt.media_upload")}
                  busy={uploadingMedia}
                  primary
                  multiple
                  onFiles={(files) => void uploadMediaFiles(files)}
                />
              </div>
            </div>

            <div class="volo-filter-bar">
              <div class="volo-filter-pills">
                <button
                  type="button"
                  class={`volo-filter-pill ${mediaRefFilter === "all" ? "active" : ""}`}
                  onClick={() => {
                    setMediaRefFilter("all");
                    setPageIdx(1);
                  }}
                >
                  {t("cnt.media_filter_all")} ({effectiveMediaTotal})
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
                  {t("cnt.media_filter_used")} ({Math.max(0, effectiveMediaTotal - orphanCount)})
                </button>
              </div>

              <div class="volo-filter-controls">
                <ConsoleSelect
                  searchable={false}
                  value={mediaSort}
                  onChange={(val) => {
                    setMediaSort((val as "newest" | "size" | "name") || "newest");
                    setPageIdx(1);
                  }}
                  options={[
                    { value: "newest", label: t("cnt.media_sort_newest") },
                    { value: "size", label: t("cnt.media_sort_size") },
                    { value: "name", label: t("cnt.media_sort_name") },
                  ]}
                />
                <ConsoleSelect
                  searchable={false}
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
                  class="volo-input volo-search-input"
                  autoComplete="off"
                  placeholder={t("cnt.media_search_ph")}
                  value={mediaQuery}
                  onInput={(e) => {
                    setMediaQuery((e.currentTarget as HTMLInputElement).value);
                    setPageIdx(1);
                  }}
                />
              </div>
            </div>

            <BatchBar count={selMediaIds.length} onClear={() => setSelMediaIds([])}>
              <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteMedia()}>
                {t("common.delete")}
              </button>
            </BatchBar>

            {loading && media.length === 0 ? (
              <div class="volo-table-wrap">
                <table class="volo-table">
                  <tbody>
                    <TableSkeleton cols={5} />
                  </tbody>
                </table>
              </div>
            ) : displayMedia.length === 0 ? (
              <ConsoleEmpty />
            ) : mediaView === "grid" ? (
              <div class="volo-media-grid">
                {displayMedia.map((m) => {
                  const displayName = m.original_name || m.id;
                  const mime = String(m.mime_type || "");
                  const isImage = mime.startsWith("image/");
                  const isAudio = mime.startsWith("audio/");
                  const extBadge = (displayName.split(".").pop() || mime.split("/").pop() || "FILE").toUpperCase();
                  const isSelected = selMediaIds.includes(m.id);
                  return (
                    <div
                      key={m.id}
                      class={`volo-media-card ${m.is_orphan ? "is-orphan" : ""} ${isSelected ? "is-selected" : ""}`}
                    >
                      <div class="volo-media-thumb" onClick={() => openAssetLightbox(m)}>
                        <label class="volo-media-select-cb" onClick={(e) => e.stopPropagation()}>
                          <input
                            type="checkbox"
                            checked={isSelected}
                            onChange={() => toggleSel(selMediaIds, setSelMediaIds, m.id)}
                          />
                        </label>
                        {isImage ? (
                          <img src={m.public_url} alt={m.alt_text || displayName} loading="lazy" />
                        ) : isAudio ? (
                          <div class="volo-media-type-placeholder">
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
              <>
                <MobileSelectAllBar
                  checked={displayMedia.length > 0 && displayMedia.every((m) => selMediaIds.includes(m.id))}
                  count={selMediaIds.length}
                  total={displayMedia.length}
                  onToggle={(on) => {
                    const pageIds = displayMedia.map((m) => m.id);
                    setSelMediaIds(
                      on
                        ? Array.from(new Set([...selMediaIds, ...pageIds]))
                        : selMediaIds.filter((id) => !pageIds.includes(id))
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
                            checked={displayMedia.length > 0 && displayMedia.every((m) => selMediaIds.includes(m.id))}
                            onChange={(e) => {
                              const on = (e.currentTarget as HTMLInputElement).checked;
                              const pageIds = displayMedia.map((m) => m.id);
                              setSelMediaIds(
                                on
                                  ? Array.from(new Set([...selMediaIds, ...pageIds]))
                                  : selMediaIds.filter((id) => !pageIds.includes(id))
                              );
                            }}
                          />
                        </th>
                        <th class="col-fluid">{t("cnt.media_col_asset")}</th>
                        <th class="col-status">{t("art.status")}</th>
                        <th class="col-cat">{t("cnt.media_col_mime")}</th>
                        <th class="col-num">{t("cnt.media_col_size")}</th>
                        <th class="col-date">{t("cnt.media_col_uploaded")}</th>
                        <th class="col-act col-act-th">{t("art.actions")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {displayMedia.map((m) => {
                        const displayName = m.original_name || m.id;
                        const mime = String(m.mime_type || "");
                        return (
                          <tr key={m.id}>
                            <td class="col-check">
                              <input
                                type="checkbox"
                                checked={selMediaIds.includes(m.id)}
                                onChange={() => toggleSel(selMediaIds, setSelMediaIds, m.id)}
                              />
                            </td>
                            <td class="col-fluid" data-label={t("cnt.media_col_asset")}>
                              <div class="volo-cell-main">
                                <div class="volo-cell-title">
                                  <button type="button" class="volo-act-btn" onClick={() => openAssetLightbox(m)}>
                                    {displayName}
                                  </button>
                                  {m.storage_driver && <span class="volo-mini-tag">{m.storage_driver}</span>}
                                </div>
                                <div class="volo-cell-meta">
                                  <span class="volo-code-chip">{m.public_url}</span>
                                </div>
                              </div>
                            </td>
                            <td class="col-status" data-label={t("art.status")}>
                              <span class={`volo-status-pill ${m.is_orphan ? "warn" : "ok"}`}>
                                <span class="volo-status-dot" />
                                {m.is_orphan
                                  ? t("cnt.media_badge_orphan")
                                  : t("cnt.media_badge_used", { count: m.ref_count || 1 })}
                              </span>
                            </td>
                            <td class="col-cat" data-label={t("cnt.media_col_mime")}>
                              <div class="volo-cell-main">
                                <span class="volo-cat-chip">{mime || "file"}</span>
                                {m.width > 0 && <span class="volo-help">{m.width}×{m.height}</span>}
                              </div>
                            </td>
                            <td class="col-num" data-label={t("cnt.media_col_size")}>
                              {formatAssetSize(m.size_bytes)}
                            </td>
                            <td class="col-date" data-label={t("cnt.media_col_uploaded")}>
                              {formatDateTime(m.created_at)}
                            </td>
                            <td class="col-act" data-label={t("art.actions")}>
                              <div class="volo-row-actions">
                                <button type="button" class="volo-act-btn" onClick={() => openAssetLightbox(m)}>
                                  {t("common.edit")}
                                </button>
                                <button type="button" class="volo-act-btn" onClick={() => copyMediaMarkdown(m)}>
                                  {t("cnt.media_copy_md")}
                                </button>
                                <ActionMenu
                                  items={[
                                    {
                                      label: t("cnt.media_copy"),
                                      onClick: () => copyMediaUrl(m),
                                    },
                                    {
                                      label: `${t("cnt.media_open_new")} ↗`,
                                      href: m.public_url,
                                      target: "_blank",
                                    },
                                    {
                                      label: t("common.delete"),
                                      danger: true,
                                      onClick: () => void handleDeleteMediaItem(m),
                                    },
                                  ]}
                                />
                              </div>
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </>
            )}

            <Pagination
              total={mediaRefFilter === "used" ? filteredMedia.length : subTotal}
              page={pageIdx}
              pageSize={mediaPageSize}
              pageSizeOptions={[24, 48, 96]}
              onPageSizeChange={(sz) => {
                setMediaPageSize(sz);
                setPageIdx(1);
              }}
              onChange={setPageIdx}
            />

          {/* Lightbox Asset Preview & Metadata Edit Modal */}
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
                  <a class="volo-btn" href={previewAsset.public_url} target="_blank" rel="noreferrer">
                    {t("cnt.media_open_new")} ↗
                  </a>
                  <button
                    type="button"
                    class="volo-btn volo-btn-primary"
                    disabled={savingAssetMeta}
                    onClick={() => void saveAssetMetadata()}
                  >
                    {t("common.save")}
                  </button>
                </>
              ) : null
            }
          >
            {previewAsset && (
              <>
                <div class="volo-lightbox-img-wrap">
                  {String(previewAsset.mime_type || "").startsWith("image/") ? (
                    <img src={previewAsset.public_url} alt={previewAsset.alt_text || previewAsset.original_name} />
                  ) : String(previewAsset.mime_type || "").startsWith("audio/") ? (
                    <audio controls src={previewAsset.public_url} class="volo-w-full" />
                  ) : (
                    <div class="volo-media-type-placeholder">
                      <span class="volo-code-chip">{previewAsset.mime_type || "FILE"}</span>
                      <span>{previewAsset.original_name}</span>
                    </div>
                  )}
                </div>

                <div class="volo-grid-2">
                  <div class="volo-field">
                    <label>{t("cnt.col_name")}</label>
                    <input
                      class="volo-input"
                      value={assetMetaForm.original_name}
                      onInput={(e) =>
                        setAssetMetaForm({
                          ...assetMetaForm,
                          original_name: (e.currentTarget as HTMLInputElement).value,
                        })
                      }
                    />
                  </div>
                  <div class="volo-field">
                    <label>{t("cnt.media_alt_text")}</label>
                    <input
                      class="volo-input"
                      placeholder={t("cnt.media_alt_ph")}
                      value={assetMetaForm.alt_text}
                      onInput={(e) =>
                        setAssetMetaForm({
                          ...assetMetaForm,
                          alt_text: (e.currentTarget as HTMLInputElement).value,
                        })
                      }
                    />
                  </div>
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
                    <span>
                      {formatAssetSize(previewAsset.size_bytes)} · {previewAsset.storage_driver || "local"}
                    </span>
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
        );
      })()}
    </div>
  );
}
