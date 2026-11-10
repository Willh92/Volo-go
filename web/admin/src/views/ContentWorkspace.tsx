import { useEffect, useRef, useState } from "preact/hooks";
import {
  apiFetch,
  clearLocalDraft,
  currentUserSig,
  dirtyEditorSig,
  loadLocalDraft,
  LocalDraft,
  openConfirm,
  resetConsoleScroll,
  saveLocalDraft,
  showToast,
  storageDriverSig,
  uploadWithProgress,
} from "../api";
import { CM6Editor } from "../components/CM6Editor";
import { MediaAssetRecord, MediaPickButton } from "../components/MediaLibraryModal";
import {
  ActionMenu,
  AnimatedModal,
  BatchBar,
  ConsoleDatePicker,
  ConsoleEmpty,
  ConsoleSelect,
  DragGripIcon,
  MobileSelectAllBar,
  MoveTopIcon,
  paginateItems,
  Pagination,
  SwitchToggle,
  TableSkeleton,
  UploadButton,
  UploadProgressBar,
  useDebouncedValue,
  useFlipList,
  usePointerSortableList,
  UserModerationModal,
  UserModerationTarget,
  isAccountBannedNow,
  isAccountMutedNow,
  runQuickUserUnmod,
  PlusIcon,
  CloseIcon,
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
  reading_status: "",
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

function summarizeUA(ua?: string): string {
  if (!ua) return "";
  const s = String(ua).trim();
  if (!s) return "";
  const browser = /Edg\//i.test(s)
    ? "Edge"
    : /OPR\/|Opera/i.test(s)
    ? "Opera"
    : /Firefox\//i.test(s)
    ? "Firefox"
    : /Chrome\//i.test(s) && !/Chromium/i.test(s)
    ? "Chrome"
    : /Safari\//i.test(s) && !/Chrome/i.test(s)
    ? "Safari"
    : /curl\//i.test(s)
    ? "curl"
    : "";
  const os = /iPhone|iPad|iOS/i.test(s)
    ? "iOS"
    : /Android/i.test(s)
    ? "Android"
    : /Mac OS X|Macintosh/i.test(s)
    ? "macOS"
    : /Windows/i.test(s)
    ? "Windows"
    : /Linux/i.test(s)
    ? "Linux"
    : "";
  return [browser, os].filter(Boolean).join(" · ") || s.slice(0, 22);
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
  const [tagPageSize, setTagPageSize] = useState(80);
  const [mediaPageSize, setMediaPageSize] = useState(24);
  const [subTotal, setSubTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const reqSeqRef = useRef(0);
  const pageLoadSeqRef = useRef(0);
  const chapLoadSeqRef = useRef(0);
  const savingPageRef = useRef(false);

  // Pages (list + full-width Studio Page Editor + LocalDraft + Theme Templates)
  const [pages, setPages] = useState<any[]>([]);
  const [pageQuery, setPageQuery] = useState("");
  const [pageStatusFilter, setPageStatusFilter] = useState("");
  const debouncedPageQuery = useDebouncedValue(pageQuery, 250);
  const [pageEditorOpen, setPageEditorOpen] = useState(false);
  const [editPage, setEditPage] = useState<any | null>(null);
  const [pendingPageDraft, setPendingPageDraft] = useState<LocalDraft | null>(null);
  const [themePageTemplates, setThemePageTemplates] = useState<Array<{ value: string; label: string }>>([]);
  const [savingPage, setSavingPage] = useState(false);
  const savePageRef = useRef<() => void>(() => {});

  // Categories
  const [categories, setCategories] = useState<any[]>([]);
  const [catQuery, setCatQuery] = useState("");
  const debouncedCatQuery = useDebouncedValue(catQuery, 250);
  const [catModalOpen, setCatModalOpen] = useState(false);
  const [catForm, setCatForm] = useState({ ...EMPTY_CAT });
  const [swappingCat, setSwappingCat] = useState(false);
  const catFlip = useFlipList<HTMLTableSectionElement>();

  // Tags (1,000+ scale + Unused filter + Merge modal)
  const [tags, setTags] = useState<any[]>([]);
  const [tagFilter, setTagFilter] = useState("");
  const [tagSort, setTagSort] = useState<"count" | "name">("count");
  const [tagUnusedOnly, setTagUnusedOnly] = useState(false);
  const debouncedTagFilter = useDebouncedValue(tagFilter, 250);
  const [selTagIds, setSelTagIds] = useState<string[]>([]);
  const [tagModalOpen, setTagModalOpen] = useState(false);
  const [tagForm, setTagForm] = useState({ ...EMPTY_TAG });
  const [mergeSourceTag, setMergeSourceTag] = useState<any | null>(null);
  const [mergeTargetId, setMergeTargetId] = useState("");
  const [mergeTargetName, setMergeTargetName] = useState("");
  const [mergeSearchQ, setMergeSearchQ] = useState("");
  const debouncedMergeQ = useDebouncedValue(mergeSearchQ, 220);
  const [mergeCandidates, setMergeCandidates] = useState<any[]>([]);
  const [mergingTag, setMergingTag] = useState(false);

  // Series + Paginated Articles checklist
  const [series, setSeries] = useState<any[]>([]);
  const [serQuery, setSerQuery] = useState("");
  const debouncedSerQuery = useDebouncedValue(serQuery, 250);
  const [knownArticlesMap, setKnownArticlesMap] = useState<Record<string, any>>({});
  const [serPickerArticles, setSerPickerArticles] = useState<any[]>([]);
  const [serSelPage, setSerSelPage] = useState(1);
  const [serArtPage, setSerArtPage] = useState(1);
  const [serArtTotal, setSerArtTotal] = useState(0);
  const [serModalOpen, setSerModalOpen] = useState(false);
  const [serForm, setSerForm] = useState({ ...EMPTY_SER });
  const [serArtFilter, setSerArtFilter] = useState("");
  const debouncedSerArtFilter = useDebouncedValue(serArtFilter, 250);
  const serFlip = useFlipList<HTMLDivElement>();

  // Books + Manual Chapters tree modal
  const [books, setBooks] = useState<any[]>([]);
  const [bookQuery, setBookQuery] = useState("");
  const [bookFilter, setBookFilter] = useState("");
  const debouncedBookQuery = useDebouncedValue(bookQuery, 250);
  const [bookModalOpen, setBookModalOpen] = useState(false);
  const [bookForm, setBookForm] = useState({ ...EMPTY_BOOK });
  const [bookSeriesOpts, setBookSeriesOpts] = useState<any[] | null>(null);
  const [seriesLabels, setSeriesLabels] = useState<Record<string, string>>({});
  const [swappingBook, setSwappingBook] = useState(false);
  const bookFlip = useFlipList<HTMLTableSectionElement>();
  const [chapModalOpen, setChapModalOpen] = useState(false);
  const [chapBook, setChapBook] = useState<any | null>(null);
  const [chapItems, setChapItems] = useState<BookChapterDraft[]>([]);
  const [chapQuery, setChapQuery] = useState("");
  const [chapPage, setChapPage] = useState(1);
  const [collapsedChapUids, setCollapsedChapUids] = useState<string[]>([]);
  const [syncingChaps, setSyncingChaps] = useState(false);

  // Comments + Custom IP Ban Modal
  const [comments, setComments] = useState<any[]>([]);
  const [cmtStatus, setCmtStatus] = useState("");
  const [cmtStatusCounts, setCmtStatusCounts] = useState<{
    all?: number;
    pending?: number;
    approved?: number;
    rejected?: number;
    trash?: number;
  }>({});
  const [cmtTrashRetentionDays, setCmtTrashRetentionDays] = useState<number | null>(null);
  const [cmtQuery, setCmtQuery] = useState("");
  const debouncedCmtQuery = useDebouncedValue(cmtQuery, 250);
  const [selCmtIds, setSelCmtIds] = useState<string[]>([]);
  const [cmtBox, setCmtBox] = useState<{
    id: string;
    mode: "reply" | "edit";
    text: string;
    // Guest identity (edit mode only); orig* track the loaded values so only changed fields are PATCHed.
    authorName?: string;
    avatarUrl?: string;
    authorUrl?: string;
    origAuthorName?: string;
    origAvatarUrl?: string;
    origAuthorUrl?: string;
  } | null>(null);
  const cmtTrashMode = cmtStatus === "trash";
  const [banIpModal, setBanIpModal] = useState<{
    commentId?: string;
    batchIds?: string[];
    ipLabel?: string;
  } | null>(null);
  const [banDurationHrs, setBanDurationHrs] = useState("720");
  const [banReason, setBanReason] = useState("comment spam");
  const [banRejectPending, setBanRejectPending] = useState(true);
  const [banningIp, setBanningIp] = useState(false);
  const [cmtUserModTarget, setCmtUserModTarget] = useState<UserModerationTarget | null>(null);

  // Media Library (10,000+ scale)
  const [media, setMedia] = useState<MediaAssetRecord[]>([]);
  const [mediaAllTotal, setMediaAllTotal] = useState(0);
  const [mediaTotalBytes, setMediaTotalBytes] = useState<number | null>(null);
  const [orphanCount, setOrphanCount] = useState(0);
  const [mediaView, setMediaView] = useState<"grid" | "list">("grid");
  const [mediaRefFilter, setMediaRefFilter] = useState<"all" | "orphan" | "used">("all");
  const [mediaKindFilter, setMediaKindFilter] = useState<"all" | "image" | "video" | "audio" | "other">("all");
  const [mediaMonthFilter, setMediaMonthFilter] = useState("");
  const [mediaSort, setMediaSort] = useState<"newest" | "size" | "name">("newest");
  const [mediaQuery, setMediaQuery] = useState("");
  const debouncedMediaQuery = useDebouncedValue(mediaQuery, 250);
  const [selMediaIds, setSelMediaIds] = useState<string[]>([]);
  const [isDraggingMedia, setIsDraggingMedia] = useState(false);
  const [uploadingMedia, setUploadingMedia] = useState(false);
  const [mediaUploadPct, setMediaUploadPct] = useState<number | null>(null);
  const [reconcilingMedia, setReconcilingMedia] = useState(false);
  const [previewAsset, setPreviewAsset] = useState<MediaAssetRecord | null>(null);
  const [assetMetaForm, setAssetMetaForm] = useState({ original_name: "", alt_text: "" });
  const [savingAssetMeta, setSavingAssetMeta] = useState(false);
  const [brokenMediaIds, setBrokenMediaIds] = useState<Record<string, boolean>>({});

  const pageDraftKey =
    pageEditorOpen && editPage ? (editPage.id ? `page:${editPage.id}` : "page:new") : "";

  useEffect(() => {
    setSub(activeTab);
    setPageEditorOpen(false);
    setEditPage(null);
    setPendingPageDraft(null);
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

  // Load active theme's localized_page_templates when in pages tab
  useEffect(() => {
    if (sub !== "pages") return;
    let cancelled = false;
    apiFetch<{ themes?: any[]; items?: any[]; active_theme?: string; appearance?: { active_theme?: string } }>(
      `/api/admin/themes?locale=${encodeURIComponent(localeSig.value)}`,
    )
      .then((res) => {
        if (cancelled) return;
        const list = res.themes || res.items || [];
        const activeId = res.appearance?.active_theme || res.active_theme;
        const active = list.find((th: any) => th.active || th.id === activeId) || list[0];
        const rawTpls = active?.localized_page_templates || active?.page_templates || [];
        const parsed: Array<{ value: string; label: string }> = [];
        if (Array.isArray(rawTpls)) {
          rawTpls.forEach((tpl: any) => {
            if (typeof tpl === "string") {
              parsed.push({ value: tpl, label: tpl });
            } else if (tpl && (tpl.key || tpl.id || tpl.value || tpl.slug)) {
              const v = String(tpl.key || tpl.id || tpl.value || tpl.slug);
              parsed.push({ value: v, label: String(tpl.label || tpl.name || tpl.title || v) });
            }
          });
        } else if (rawTpls && typeof rawTpls === "object") {
          Object.entries(rawTpls).forEach(([k, v]) => {
            parsed.push({ value: k, label: String(v || k) });
          });
        }
        setThemePageTemplates(parsed);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [sub, localeSig.value]);

  // Auto-save Page LocalDraft every 1.2s (never overwrite unreviewed pendingPageDraft)
  useEffect(() => {
    if (!pageEditorOpen || !editPage || !pageDraftKey || pendingPageDraft !== null) return;
    const timer = window.setTimeout(() => {
      if (editPage.title || editPage.content_md) {
        void saveLocalDraft(pageDraftKey, {
          title: editPage.title || "",
          slug: editPage.slug || "",
          summary_md: "",
          content_md: editPage.content_md || "",
          template: editPage.template || "default",
          is_published: editPage.is_published !== false,
          show_toc: Boolean(editPage.show_toc),
          allow_comment: editPage.allow_comment !== false,
          lang: editPage.lang || localeSig.value,
          translation_group: editPage.translation_group || "",
          saved_at: Date.now(),
        });
      }
    }, 1200);
    return () => window.clearTimeout(timer);
  }, [pageEditorOpen, editPage, pageDraftKey, pendingPageDraft]);

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
    setPendingPageDraft(null);
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
        qs.set("per_page", String(tagPageSize));
        qs.set("sort", tagSort);
        if (tagUnusedOnly) qs.set("unused", "1");
        if (debouncedTagFilter.trim()) qs.set("q", debouncedTagFilter.trim());
        const r = await apiFetch<{ items?: any[]; tags?: any[]; total?: number }>(`/api/admin/tags?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || r.tags || [];
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
        // Seed knownArticlesMap from s.articles returned by GET /api/admin/series
        const seeded: Record<string, any> = {};
        list.forEach((s: any) => {
          if (Array.isArray(s.articles)) {
            s.articles.forEach((a: any) => {
              if (a?.id) seeded[a.id] = a;
            });
          }
        });
        if (Object.keys(seeded).length > 0) {
          setKnownArticlesMap((prev) => ({ ...prev, ...seeded }));
        }
      } else if (sub === "books") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(pageSize));
        if (debouncedBookQuery.trim()) qs.set("q", debouncedBookQuery.trim());
        if (bookFilter) {
          if (bookFilter.startsWith("rs:")) qs.set("reading_status", bookFilter.slice(3));
          else qs.set("source_type", bookFilter);
        }
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
          status_counts?: { all?: number; pending?: number; approved?: number; rejected?: number; trash?: number };
          trash_retention_days?: number;
        }>(`/api/admin/comments?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        setComments(list);
        setSubTotal(typeof r.total === "number" ? r.total : list.length);
        if (r.status_counts) setCmtStatusCounts(r.status_counts);
        if (typeof r.trash_retention_days === "number") setCmtTrashRetentionDays(r.trash_retention_days);
      } else if (sub === "media") {
        const qs = new URLSearchParams();
        qs.set("page", String(pageIdx));
        qs.set("per_page", String(mediaPageSize));
        qs.set("sort", mediaSort);
        if (mediaKindFilter !== "all") qs.set("mime", mediaKindFilter);
        if (mediaRefFilter === "orphan") {
          qs.set("usage", "unused");
          qs.set("orphan_only", "1");
        } else if (mediaRefFilter === "used") {
          qs.set("usage", "used");
        } else {
          qs.set("usage", "all");
        }
        if (mediaMonthFilter.trim()) qs.set("month", mediaMonthFilter.trim());
        if (debouncedMediaQuery.trim()) qs.set("q", debouncedMediaQuery.trim());
        const r = await apiFetch<{
          items: MediaAssetRecord[];
          total?: number;
          all_total?: number;
          total_bytes?: number;
          orphan_count?: number;
          storage_driver?: string;
        }>(`/api/admin/media?${qs.toString()}`);
        if (reqId !== reqSeqRef.current) return;
        const list = r.items || [];
        const tot = typeof r.total === "number" ? r.total : list.length;
        setMedia(list);
        setSubTotal(tot);
        if (typeof r.total_bytes === "number") {
          setMediaTotalBytes(r.total_bytes);
        } else {
          setMediaTotalBytes(null);
        }
        if (typeof r.all_total === "number") {
          setMediaAllTotal(r.all_total);
        } else if (
          mediaRefFilter === "all" &&
          mediaKindFilter === "all" &&
          !mediaMonthFilter.trim() &&
          !debouncedMediaQuery.trim()
        ) {
          setMediaAllTotal(r.all_total ?? tot);
        } else if (mediaAllTotal === 0) {
          setMediaAllTotal(r.all_total ?? tot);
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
    tagPageSize,
    mediaPageSize,
    debouncedPageQuery,
    pageStatusFilter,
    debouncedCatQuery,
    debouncedTagFilter,
    tagSort,
    tagUnusedOnly,
    debouncedSerQuery,
    debouncedBookQuery,
    bookFilter,
    cmtStatus,
    debouncedCmtQuery,
    mediaRefFilter,
    mediaKindFilter,
    mediaMonthFilter,
    mediaSort,
    debouncedMediaQuery,
  ]);

  const searchMergeTargetTags = async (q: string) => {
    if (!mergeSourceTag) return;
    const qs = new URLSearchParams({ per_page: "50", sort: "count" });
    if (q.trim()) qs.set("q", q.trim());
    const res = await apiFetch<{ items?: any[]; tags?: any[] }>(`/api/admin/tags?${qs.toString()}`).catch(() => ({
      items: [] as any[],
      tags: [] as any[],
    }));
    const list = (res.items || res.tags || []).filter((tg: any) => tg.id !== mergeSourceTag.id);
    if (list.length === 0) return;
    setMergeCandidates((prev) => {
      const seen = new Set(prev.map((tg: any) => tg.id));
      const merged = [...prev];
      for (const tg of list) {
        if (tg?.id && !seen.has(tg.id)) {
          seen.add(tg.id);
          merged.push(tg);
        }
      }
      return merged;
    });
  };

  // Tag Merge candidate search
  useEffect(() => {
    if (!mergeSourceTag) return;
    let cancelled = false;
    const qs = new URLSearchParams({ per_page: "50", sort: "count" });
    if (debouncedMergeQ.trim()) qs.set("q", debouncedMergeQ.trim());
    apiFetch<{ items?: any[]; tags?: any[] }>(`/api/admin/tags?${qs.toString()}`)
      .then((res) => {
        if (cancelled) return;
        const list = (res.items || res.tags || []).filter((tg: any) => tg.id !== mergeSourceTag.id);
        setMergeCandidates(list);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [mergeSourceTag, debouncedMergeQ]);

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

  // Lazily hydrate missing Series modal articles for current 15-items/page slice whenever page or article IDs change
  const serArticleIdsKey = serForm.article_ids.join(",");
  useEffect(() => {
    if (!serModalOpen || serForm.article_ids.length === 0) return;
    const SER_HYDRATE_PAGE_SIZE = 15;
    const totalSelPages = Math.max(1, Math.ceil(serForm.article_ids.length / SER_HYDRATE_PAGE_SIZE));
    const safeSelPage = Math.min(Math.max(1, serSelPage), totalSelPages);
    const selSlice = serForm.article_ids.slice(
      (safeSelPage - 1) * SER_HYDRATE_PAGE_SIZE,
      safeSelPage * SER_HYDRATE_PAGE_SIZE
    );
    const safeArtPage = Math.max(1, serArtPage);
    const artPageSlice = serForm.article_ids.slice(
      (safeArtPage - 1) * SER_HYDRATE_PAGE_SIZE,
      safeArtPage * SER_HYDRATE_PAGE_SIZE
    );
    const targetIds = Array.from(new Set([...selSlice, ...artPageSlice]));
    const missing = targetIds.filter((id) => !knownArticlesMap[id]);
    if (missing.length === 0) return;
    let cancelled = false;
    Promise.all(
      missing.map((id) =>
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
  }, [serModalOpen, serSelPage, serArtPage, serArticleIdsKey]);

  const updatePageDraft = (patch: Record<string, any>) => {
    setEditPage((prev: any) => (prev ? { ...prev, ...patch } : prev));
    dirtyEditorSig.value = true;
  };

  const openNewPageEditor = async () => {
    const seq = ++pageLoadSeqRef.current;
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
    const saved = await loadLocalDraft("page:new");
    if (seq !== pageLoadSeqRef.current) return;
    if (saved && (saved.title || saved.content_md)) {
      setPendingPageDraft(saved);
    } else {
      setPendingPageDraft(null);
    }
  };

  const openExistingPageEditor = async (pageId: string) => {
    const seq = ++pageLoadSeqRef.current;
    try {
      const full = await apiFetch<{ page: any }>(`/api/admin/pages/${pageId}`);
      if (seq !== pageLoadSeqRef.current) return;
      dirtyEditorSig.value = false;
      const p = full.page;
      setEditPage({ template: "default", ...p });
      setPageEditorOpen(true);
      resetConsoleScroll();
      const saved = await loadLocalDraft(`page:${pageId}`);
      if (seq !== pageLoadSeqRef.current) return;
      const updatedMs = p?.updated_at ? new Date(p.updated_at).getTime() || Number(p.updated_at) || 0 : 0;
      if (
        saved &&
        saved.saved_at > updatedMs &&
        (saved.content_md !== (p?.content_md || "") || saved.title !== (p?.title || ""))
      ) {
        setPendingPageDraft(saved);
      } else {
        setPendingPageDraft(null);
      }
    } catch (err: any) {
      if (seq === pageLoadSeqRef.current) showToast(err.message);
    }
  };

  const applyPendingPageDraft = () => {
    if (!pendingPageDraft) return;
    setEditPage((prev: any) =>
      prev
        ? {
            ...prev,
            title: pendingPageDraft.title ?? prev.title,
            slug: pendingPageDraft.slug ?? prev.slug,
            content_md: pendingPageDraft.content_md ?? prev.content_md,
            template: pendingPageDraft.template || prev.template || "default",
            is_published:
              typeof pendingPageDraft.is_published === "boolean"
                ? pendingPageDraft.is_published
                : prev.is_published,
            show_toc:
              typeof pendingPageDraft.show_toc === "boolean" ? pendingPageDraft.show_toc : prev.show_toc,
            allow_comment:
              typeof pendingPageDraft.allow_comment === "boolean"
                ? pendingPageDraft.allow_comment
                : prev.allow_comment,
            lang: pendingPageDraft.lang || prev.lang,
            translation_group: pendingPageDraft.translation_group ?? prev.translation_group,
          }
        : prev
    );
    dirtyEditorSig.value = true;
    setPendingPageDraft(null);
    showToast(t("art.draft_restored"), "ok");
  };

  const discardPendingPageDraft = () => {
    if (pageDraftKey) void clearLocalDraft(pageDraftKey);
    setPendingPageDraft(null);
  };

  const closePageEditor = async () => {
    if (!(await confirmLeaveDirtyPage())) return;
    ++pageLoadSeqRef.current;
    dirtyEditorSig.value = false;
    setPageEditorOpen(false);
    setEditPage(null);
    setPendingPageDraft(null);
    resetConsoleScroll();
  };

  const savePage = async (e?: Event) => {
    e?.preventDefault();
    if (!editPage || savingPage || savingPageRef.current) return;
    if (!String(editPage.title || "").trim()) return;
    savingPageRef.current = true;
    setSavingPage(true);
    try {
      const prevKey = editPage.id ? `page:${editPage.id}` : "page:new";
      const payload = {
        ...editPage,
        title: String(editPage.title || "").trim(),
        template: editPage.template || "default",
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
      void clearLocalDraft(prevKey);
      setPendingPageDraft(null);
      dirtyEditorSig.value = false;
      showToast(t("common.saved"));
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      savingPageRef.current = false;
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

  const fetchAllForReorder = async (endpoint: string): Promise<any[]> => {
    const perPage = 200;
    const first = await apiFetch<{ items: any[]; total?: number }>(`${endpoint}?page=1&per_page=${perPage}`);
    const all = [...(first.items || [])];
    const total = typeof first.total === "number" ? first.total : all.length;
    const maxPages = Math.min(20, Math.ceil(total / perPage));
    for (let p = 2; p <= maxPages; p++) {
      const next = await apiFetch<{ items: any[] }>(`${endpoint}?page=${p}&per_page=${perPage}`);
      if (!next.items?.length) break;
      all.push(...next.items);
    }
    return all;
  };

  const swapCatOrder = async (idx: number, dir: -1 | 1) => {
    if (swappingCat || catQuery.trim() || idx < 0 || idx >= categories.length) return;
    const safePage = Math.max(1, pageIdx);
    const baseOffset = (safePage - 1) * pageSize;
    const globalIdx = baseOffset + idx;
    const totalCats = Math.max(subTotal, baseOffset + categories.length);
    if ((dir === -1 && globalIdx <= 0) || (dir === 1 && globalIdx >= totalCats - 1)) return;

    const targetIdx = idx + dir;
    const prevCats = categories;
    setSwappingCat(true);

    if (targetIdx >= 0 && targetIdx < categories.length && totalCats <= categories.length) {
      const reordered = [...categories];
      [reordered[idx], reordered[targetIdx]] = [reordered[targetIdx], reordered[idx]];
      const nextCats = reordered.map((item, i) => ({ ...item, sort_order: (i + 1) * 10 }));
      catFlip.captureFlip();
      setCategories(nextCats);
      try {
        await apiFetch("/api/admin/categories/reorder", {
          method: "POST",
          body: JSON.stringify({ ordered_ids: nextCats.map((x) => x.id) }),
        });
      } catch (err: any) {
        setCategories(prevCats);
        showToast(err.message);
        void loadSub();
      } finally {
        setSwappingCat(false);
      }
      return;
    }

    // Multi-page or cross-page move: reorder complete list atomically
    try {
      const all = await fetchAllForReorder("/api/admin/categories");
      const curItem = categories[idx];
      const curAllIdx = all.findIndex((x) => x.id === curItem.id);
      const adjAllIdx = curAllIdx + dir;
      if (curAllIdx < 0 || adjAllIdx < 0 || adjAllIdx >= all.length) return;
      const reorderedAll = [...all];
      [reorderedAll[curAllIdx], reorderedAll[adjAllIdx]] = [reorderedAll[adjAllIdx], reorderedAll[curAllIdx]];
      const normalizedAll = reorderedAll.map((item, i) => ({ ...item, sort_order: (i + 1) * 10 }));
      catFlip.captureFlip();
      setCategories(normalizedAll.slice(baseOffset, baseOffset + pageSize));
      setSubTotal(normalizedAll.length);
      await apiFetch("/api/admin/categories/reorder", {
        method: "POST",
        body: JSON.stringify({ ordered_ids: normalizedAll.map((x) => x.id) }),
      });
    } catch (err: any) {
      setCategories(prevCats);
      showToast(err.message);
      void loadSub();
    } finally {
      setSwappingCat(false);
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

  const submitMergeTag = async (e?: Event) => {
    e?.preventDefault();
    if (!mergeSourceTag || !mergeTargetId || mergeTargetId === mergeSourceTag.id || mergingTag) return;
    setMergingTag(true);
    try {
      await apiFetch("/api/admin/tags/merge", {
        method: "POST",
        body: JSON.stringify({ source_id: mergeSourceTag.id, target_id: mergeTargetId }),
      });
      setMergeSourceTag(null);
      setMergeTargetId("");
      setMergeSearchQ("");
      setSelTagIds((prev) => prev.filter((id) => id !== mergeSourceTag.id));
      showToast(t("cnt.tag_merged"), "ok");
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setMergingTag(false);
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
    serFlip.captureFlip();
    const next = [...serForm.article_ids];
    [next[idx], next[targetIdx]] = [next[targetIdx], next[idx]];
    setSerForm({ ...serForm, article_ids: next });
  };

  const moveSeriesArticleTo = (fromIdx: number, toIdx: number) => {
    const total = serForm.article_ids.length;
    if (total <= 1 || fromIdx < 0 || fromIdx >= total) return;
    const clamped = Math.max(0, Math.min(total - 1, toIdx));
    if (clamped === fromIdx) return;
    serFlip.captureFlip();
    const next = [...serForm.article_ids];
    const [moved] = next.splice(fromIdx, 1);
    next.splice(clamped, 0, moved);
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
        reading_status: bookForm.reading_status || "",
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
    if (swappingBook || bookQuery.trim() || bookFilter || idx < 0 || idx >= books.length) return;
    const safePage = Math.max(1, pageIdx);
    const baseOffset = (safePage - 1) * pageSize;
    const globalIdx = baseOffset + idx;
    const totalBooks = Math.max(subTotal, baseOffset + books.length);
    if ((dir === -1 && globalIdx <= 0) || (dir === 1 && globalIdx >= totalBooks - 1)) return;

    const targetIdx = idx + dir;
    const prevBooks = books;
    setSwappingBook(true);

    if (targetIdx >= 0 && targetIdx < books.length && totalBooks <= books.length) {
      const reordered = [...books];
      [reordered[idx], reordered[targetIdx]] = [reordered[targetIdx], reordered[idx]];
      const nextBooks = reordered.map((item, i) => ({ ...item, sort_order: (i + 1) * 10 }));
      bookFlip.captureFlip();
      setBooks(nextBooks);
      try {
        await apiFetch("/api/admin/books/reorder", {
          method: "POST",
          body: JSON.stringify({ ordered_ids: nextBooks.map((x) => x.id) }),
        });
      } catch (err: any) {
        setBooks(prevBooks);
        showToast(err.message);
        void loadSub();
      } finally {
        setSwappingBook(false);
      }
      return;
    }

    // Multi-page or cross-page move: reorder complete list atomically
    try {
      const all = await fetchAllForReorder("/api/admin/books");
      const curItem = books[idx];
      const curAllIdx = all.findIndex((x) => x.id === curItem.id);
      const adjAllIdx = curAllIdx + dir;
      if (curAllIdx < 0 || adjAllIdx < 0 || adjAllIdx >= all.length) return;
      const reorderedAll = [...all];
      [reorderedAll[curAllIdx], reorderedAll[adjAllIdx]] = [reorderedAll[adjAllIdx], reorderedAll[curAllIdx]];
      const normalizedAll = reorderedAll.map((item, i) => ({ ...item, sort_order: (i + 1) * 10 }));
      bookFlip.captureFlip();
      setBooks(normalizedAll.slice(baseOffset, baseOffset + pageSize));
      setSubTotal(normalizedAll.length);
      await apiFetch("/api/admin/books/reorder", {
        method: "POST",
        body: JSON.stringify({ ordered_ids: normalizedAll.map((x) => x.id) }),
      });
    } catch (err: any) {
      setBooks(prevBooks);
      showToast(err.message);
      void loadSub();
    } finally {
      setSwappingBook(false);
    }
  };

  useEffect(() => {
    if (series.length === 0) return;
    setSeriesLabels((prev) => {
      const next = { ...prev };
      for (const s of series) {
        if (s?.id && s.title) next[s.id] = s.title;
      }
      return next;
    });
  }, [series]);

  useEffect(() => {
    if (!bookModalOpen) {
      setBookSeriesOpts(null);
      return;
    }
    const sid = bookForm.series_id;
    if (!sid || seriesLabels[sid] || series.some((s) => s.id === sid)) return;
    let cancelled = false;
    apiFetch<any>(`/api/admin/series/${encodeURIComponent(sid)}`)
      .then((res) => {
        if (cancelled) return;
        const item = res?.series || res;
        if (item?.id && item.title) {
          setSeriesLabels((prev) => ({ ...prev, [item.id]: item.title }));
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [bookModalOpen, bookForm.series_id]);

  const openManageChapters = async (book: any) => {
    const seq = ++chapLoadSeqRef.current;
    try {
      const res = await apiFetch<{ book: any; chapters: any[] }>(`/api/admin/books/${book.id}`);
      if (seq !== chapLoadSeqRef.current) return;
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
      if (seq === chapLoadSeqRef.current) showToast(err.message);
    }
  };

  const syncModalChapters = async () => {
    if (!chapBook || syncingChaps) return;
    const seq = ++chapLoadSeqRef.current;
    setSyncingChaps(true);
    try {
      const syncRes = await apiFetch<{ chapter_count?: number }>(`/api/admin/books/${chapBook.id}/sync`, {
        method: "POST",
        body: "{}",
      });
      const res = await apiFetch<{ book: any; chapters: any[] }>(`/api/admin/books/${chapBook.id}`);
      if (seq !== chapLoadSeqRef.current) return;
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
      showToast(t("cnt.book_synced", { count: syncRes.chapter_count ?? res.chapters?.length ?? 0 }));
      void loadSub();
    } catch (err: any) {
      if (seq === chapLoadSeqRef.current) showToast(err.message);
    } finally {
      setSyncingChaps(false);
    }
  };

  const clampChapDepth = (v: unknown) => Math.max(0, Math.min(3, Number(v) || 0));

  const canMoveChapUp = (items: BookChapterDraft[], idx: number): boolean => {
    if (idx <= 0 || idx >= items.length) return false;
    const d = clampChapDepth(items[idx].depth);
    return clampChapDepth(items[idx - 1].depth) >= d;
  };

  const canMoveChapDown = (items: BookChapterDraft[], idx: number): boolean => {
    if (idx < 0 || idx >= items.length - 1) return false;
    const d = clampChapDepth(items[idx].depth);
    let blockEnd = idx + 1;
    while (blockEnd < items.length && clampChapDepth(items[blockEnd].depth) > d) {
      blockEnd++;
    }
    return blockEnd < items.length && clampChapDepth(items[blockEnd].depth) === d;
  };

  // Move entire chapter subtree (node + contiguous deeper descendants) past adjacent sibling subtree
  const moveChapterSubtree = (idx: number, dir: -1 | 1) => {
    if (dir === -1 ? !canMoveChapUp(chapItems, idx) : !canMoveChapDown(chapItems, idx)) return;
    chapSortHook.captureFlip();
    setChapItems((prev) => {
      if (idx < 0 || idx >= prev.length) return prev;
      const d = clampChapDepth(prev[idx].depth);
      let blockEnd = idx + 1;
      while (blockEnd < prev.length && clampChapDepth(prev[blockEnd].depth) > d) {
        blockEnd++;
      }
      if (dir === -1) {
        if (!canMoveChapUp(prev, idx)) return prev;
        let prevStart = idx - 1;
        while (prevStart > 0 && clampChapDepth(prev[prevStart].depth) > d) {
          prevStart--;
        }
        if (clampChapDepth(prev[prevStart].depth) < d) return prev;
        const block = prev.slice(idx, blockEnd);
        const before = prev.slice(0, prevStart);
        const mid = prev.slice(prevStart, idx);
        const after = prev.slice(blockEnd);
        return [...before, ...block, ...mid, ...after];
      } else {
        if (blockEnd >= prev.length) return prev;
        const nextD = clampChapDepth(prev[blockEnd].depth);
        if (nextD !== d) return prev;
        let nextEnd = blockEnd + 1;
        while (nextEnd < prev.length && clampChapDepth(prev[nextEnd].depth) > nextD) {
          nextEnd++;
        }
        const block = prev.slice(idx, blockEnd);
        const nextBlock = prev.slice(blockEnd, nextEnd);
        return [...prev.slice(0, idx), ...nextBlock, ...block, ...prev.slice(nextEnd)];
      }
    });
  };

  const moveChapterToTop = (idx: number) => {
    if (!canMoveChapUp(chapItems, idx)) return;
    chapSortHook.captureFlip();
    setChapItems((prev) => {
      if (!canMoveChapUp(prev, idx)) return prev;
      const d = clampChapDepth(prev[idx].depth);
      let blockEnd = idx + 1;
      while (blockEnd < prev.length && clampChapDepth(prev[blockEnd].depth) > d) {
        blockEnd++;
      }
      let targetPos = 0;
      if (d > 0) {
        let p = idx - 1;
        while (p >= 0 && clampChapDepth(prev[p].depth) >= d) {
          p--;
        }
        targetPos = p + 1;
      }
      if (targetPos >= idx) return prev;
      const block = prev.slice(idx, blockEnd);
      return [...prev.slice(0, targetPos), ...block, ...prev.slice(targetPos, idx), ...prev.slice(blockEnd)];
    });
  };

  const chapSliceUidsRef = useRef<string[]>([]);
  const chapSortHook = usePointerSortableList(
    chapSliceUidsRef.current,
    (_nextUids, fromSliceIdx, toSliceIdx) => {
      const sliceUids = chapSliceUidsRef.current;
      const movedUid = sliceUids[fromSliceIdx];
      const targetUid = sliceUids[toSliceIdx];
      if (!movedUid || !targetUid || movedUid === targetUid) return;
      const collapsedSet = new Set(collapsedChapUids);
      setChapItems((prev) => {
        const srcStart = prev.findIndex((c) => c._uid === movedUid);
        if (srcStart < 0) return prev;
        const srcD = Math.max(0, Math.min(3, Number(prev[srcStart].depth) || 0));
        let srcEnd = srcStart + 1;
        if (srcD === 0 && collapsedSet.has(movedUid)) {
          while (srcEnd < prev.length && (Number(prev[srcEnd].depth) || 0) > 0) {
            srcEnd++;
          }
        }
        const block = prev.slice(srcStart, srcEnd);
        const rest = [...prev.slice(0, srcStart), ...prev.slice(srcEnd)];
        const dstIdx = rest.findIndex((c) => c._uid === targetUid);
        if (dstIdx < 0) return prev;
        let insertAt = dstIdx;
        if (toSliceIdx > fromSliceIdx) {
          insertAt = dstIdx + 1;
          const dstD = Math.max(0, Math.min(3, Number(rest[dstIdx].depth) || 0));
          if (dstD === 0 && collapsedSet.has(targetUid)) {
            while (insertAt < rest.length && (Number(rest[insertAt].depth) || 0) > 0) {
              insertAt++;
            }
          }
        }
        rest.splice(insertAt, 0, ...block);
        return rest;
      });
    },
    !chapQuery.trim()
  );

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

  const runBatchComments = async (action: "approve" | "reject" | "delete" | "ban_ip" | "restore" | "purge") => {
    if (!selCmtIds.length) return;
    if (action === "ban_ip") {
      setBanDurationHrs("720");
      setBanReason("comment spam");
      setBanRejectPending(true);
      setBanIpModal({ batchIds: [...selCmtIds], ipLabel: `${selCmtIds.length}` });
      return;
    }
    if (action === "delete") {
      if (!(await openConfirm({ message: t("cnt.confirm_batch_del_cmts", { n: selCmtIds.length }), danger: true }))) {
        return;
      }
    }
    if (action === "purge") {
      if (!(await openConfirm({ message: t("cnt.cmt_confirm_batch_purge", { n: selCmtIds.length }), danger: true }))) {
        return;
      }
    }
    try {
      await apiFetch("/api/admin/comments/batch", {
        method: "POST",
        body: JSON.stringify({ ids: selCmtIds, action }),
      });
      setSelCmtIds([]);
      showToast(
        action === "delete"
          ? t("cnt.cmt_moved_to_trash")
          : action === "restore"
            ? t("cnt.cmt_restored")
            : action === "purge"
              ? t("cnt.cmt_purged")
              : t("common.saved")
      );
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const emptyCommentTrash = async () => {
    if (!(await openConfirm({ message: t("cnt.cmt_confirm_empty_trash"), danger: true }))) return;
    try {
      const r = await apiFetch<{ purged?: number; kept_placeholders?: number }>("/api/admin/comments/trash/empty", {
        method: "POST",
      });
      setSelCmtIds([]);
      showToast(t("cnt.cmt_trash_emptied", { n: r?.purged ?? 0 }), "ok");
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    }
  };

  const submitBanIpModal = async (e?: Event) => {
    e?.preventDefault();
    if (!banIpModal || banningIp) return;
    setBanningIp(true);
    try {
      const hrs = Number(banDurationHrs) || 0;
      const reason = banReason.trim() || "comment spam";
      if (banIpModal.batchIds && banIpModal.batchIds.length > 0) {
        await apiFetch("/api/admin/comments/batch", {
          method: "POST",
          body: JSON.stringify({
            ids: banIpModal.batchIds,
            action: "ban_ip",
            duration_hours: hrs,
            expires_hours: hrs,
            reason,
            note: reason,
            reject_pending: banRejectPending,
          }),
        });
        if (banRejectPending) {
          await apiFetch("/api/admin/comments/batch", {
            method: "POST",
            body: JSON.stringify({ ids: banIpModal.batchIds, action: "reject" }),
          }).catch(() => {});
        }
        setSelCmtIds([]);
      } else if (banIpModal.commentId) {
        await apiFetch(`/api/admin/comments/${banIpModal.commentId}/ban-ip`, {
          method: "POST",
          body: JSON.stringify({
            duration_hours: hrs,
            expires_hours: hrs,
            reason,
            note: reason,
            reject_pending: banRejectPending,
          }),
        });
        if (banRejectPending) {
          const targetRow = comments.find((r) => (r.comment || r).id === banIpModal.commentId);
          const targetIp = (targetRow?.comment || targetRow)?.client_ip || (targetRow?.comment || targetRow)?.ip_masked;
          const matchingPendingIds = comments
            .map((r) => r.comment || r)
            .filter(
              (cm) =>
                cm.id === banIpModal.commentId ||
                (targetIp && (cm.client_ip === targetIp || cm.ip_masked === targetIp) && cm.status === "pending")
            )
            .map((cm) => cm.id);
          if (matchingPendingIds.length > 0) {
            await apiFetch("/api/admin/comments/batch", {
              method: "POST",
              body: JSON.stringify({ ids: matchingPendingIds, action: "reject" }),
            }).catch(() => {});
          }
        }
      }
      setBanIpModal(null);
      showToast(t("cnt.cmt_ip_banned"), "ok");
      void loadSub();
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setBanningIp(false);
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
        const patch: Record<string, string> = { content_md: txt };
        const nextName = (cmtBox.authorName ?? "").trim();
        const nextAvatar = (cmtBox.avatarUrl ?? "").trim();
        const nextSite = (cmtBox.authorUrl ?? "").trim();
        if (cmtBox.authorName !== undefined && nextName !== (cmtBox.origAuthorName ?? "")) {
          if (!nextName) {
            showToast(t("cnt.cmt_guest_name_required"));
            return;
          }
          patch.author_name = nextName;
        }
        if (cmtBox.avatarUrl !== undefined && nextAvatar !== (cmtBox.origAvatarUrl ?? "")) patch.avatar_url = nextAvatar;
        if (cmtBox.authorUrl !== undefined && nextSite !== (cmtBox.origAuthorUrl ?? "")) patch.author_url = nextSite;
        await apiFetch(`/api/admin/comments/${cmtBox.id}`, {
          method: "PATCH",
          body: JSON.stringify(patch),
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
    setMediaUploadPct(0);
    const totalBytes = Math.max(1, files.reduce((acc, f) => acc + Math.max(1, f.size), 0));
    const perFileLoaded = new Array<number>(files.length).fill(0);
    const updateAggPct = () => {
      const loaded = perFileLoaded.reduce((a, b) => a + b, 0);
      setMediaUploadPct(Math.min(100, Math.round((loaded / totalBytes) * 100)));
    };
    let okCount = 0;
    let cursor = 0;
    const workerCount = Math.min(3, files.length);
    try {
      await Promise.all(
        Array.from({ length: workerCount }, async () => {
          while (cursor < files.length) {
            const idx = cursor++;
            const f = files[idx];
            const fSize = Math.max(1, f.size);
            const fd = new FormData();
            fd.append("file", f);
            await uploadWithProgress("/api/admin/media/upload", fd, (pct) => {
              perFileLoaded[idx] = (pct / 100) * fSize;
              updateAggPct();
            });
            perFileLoaded[idx] = fSize;
            updateAggPct();
            okCount++;
          }
        })
      );
      if (okCount > 0) {
        showToast(okCount > 1 ? t("cnt.media_uploaded_n", { count: okCount }) : t("cnt.media_uploaded"));
        await loadSub();
      }
    } catch (err: any) {
      showToast(err.message);
      if (okCount > 0) await loadSub();
    } finally {
      setUploadingMedia(false);
      setMediaUploadPct(null);
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
    const mime = String(m.mime_type || "");
    const md = mime.startsWith("image/")
      ? `![${label}](${m.public_url})`
      : mime.startsWith("video/")
      ? `<video controls preload="metadata" src="${m.public_url}"></video>`
      : `[${label}](${m.public_url})`;
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

  const computedMediaBytes =
    mediaTotalBytes !== null
      ? mediaTotalBytes
      : media.reduce((acc, m) => acc + Number(m.size_bytes || 0), 0);
  const totalMB = (computedMediaBytes / (1024 * 1024)).toFixed(2);

  const displayPages = pages.length > pageSize ? paginateItems(pages, pageIdx, pageSize).slice : pages;
  const displayCats = categories.length > pageSize ? paginateItems(categories, pageIdx, pageSize).slice : categories;
  const displayTags = tags.length > tagPageSize ? paginateItems(tags, pageIdx, tagPageSize).slice : tags;
  const displaySeries = series.length > pageSize ? paginateItems(series, pageIdx, pageSize).slice : series;
  const displayBooks = books.length > pageSize ? paginateItems(books, pageIdx, pageSize).slice : books;
  const displayComments = comments.length > pageSize ? paginateItems(comments, pageIdx, pageSize).slice : comments;
  const displayMedia = media.length > mediaPageSize ? paginateItems(media, pageIdx, mediaPageSize).slice : media;

  // Build deduplicated Page Template options (builtin + active theme localized_page_templates)
  const builtinTplValues = new Set(["default", "wide", "minimal"]);
  const pageTemplateOptions = [
    { value: "default", label: t("cnt.page_tpl_default") },
    { value: "wide", label: t("cnt.page_tpl_wide") },
    { value: "minimal", label: t("cnt.page_tpl_minimal") },
    ...themePageTemplates.filter((tp) => !builtinTplValues.has(tp.value)),
  ];

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
                <PlusIcon size={14} /> {t("cnt.new_cat")}
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
              <tbody ref={catFlip.containerRef}>
                {loading && categories.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : displayCats.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  displayCats.map((c) => {
                    const absIdx = categories.findIndex((x) => x.id === c.id);
                    const baseOffset = (Math.max(1, pageIdx) - 1) * pageSize;
                    const globalIdx = absIdx >= 0 ? baseOffset + absIdx : -1;
                    const totalCats = Math.max(subTotal, baseOffset + categories.length);
                    const isCatFiltered = Boolean(catQuery.trim());
                    return (
                      <tr key={c.id} data-flip-key={c.id}>
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
                              disabled={isCatFiltered || swappingCat || absIdx < 0 || globalIdx <= 0}
                              title={isCatFiltered ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
                              onClick={() => void swapCatOrder(absIdx, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={isCatFiltered || swappingCat || absIdx < 0 || globalIdx >= totalCats - 1}
                              title={isCatFiltered ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
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
              <button
                type="button"
                class={`volo-filter-pill ${tagUnusedOnly ? "active" : ""}`}
                onClick={() => {
                  setTagUnusedOnly((v) => !v);
                  setPageIdx(1);
                }}
              >
                {t("cnt.tag_unused_only")}
              </button>
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
                <PlusIcon size={14} /> {t("cnt.new_tag")}
              </button>
            </div>
          </div>

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
                            class="volo-act-btn"
                            onClick={() => {
                              setMergeSourceTag(tg);
                              setMergeTargetId("");
                              setMergeTargetName("");
                              setMergeSearchQ("");
                            }}
                          >
                            {t("cnt.tag_merge")}
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

          <BatchBar count={selTagIds.length} onClear={() => setSelTagIds([])}>
            <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteTags()}>
              {t("common.delete")}
            </button>
          </BatchBar>

          <Pagination
            total={subTotal}
            page={pageIdx}
            pageSize={tagPageSize}
            pageSizeOptions={[40, 80, 160]}
            onPageSizeChange={(sz) => {
              setTagPageSize(sz);
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

          <AnimatedModal
            open={!!mergeSourceTag}
            onClose={() => setMergeSourceTag(null)}
            title={t("cnt.tag_merge_title")}
            subtitle={mergeSourceTag ? t("cnt.tag_merge_desc", { name: mergeSourceTag.name }) : ""}
            maxWidth="480px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setMergeSourceTag(null)}>
                  {t("common.cancel")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-primary"
                  disabled={!mergeTargetId || mergingTag}
                  onClick={() => void submitMergeTag()}
                >
                  {t("cnt.tag_merge")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void submitMergeTag(e)} autoComplete="off">
              <div class="volo-field">
                <label>{t("cnt.tag_filter_ph")}</label>
                <input
                  class="volo-input"
                  placeholder={t("cnt.tag_filter_ph")}
                  value={mergeSearchQ}
                  onInput={(e) => setMergeSearchQ((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <label>{t("cnt.tag_merge_target")}</label>
                <ConsoleSelect
                  value={mergeTargetId}
                  fallbackLabel={mergeTargetName || undefined}
                  onSearch={searchMergeTargetTags}
                  onChange={(v) => {
                    setMergeTargetId(v);
                    const found = mergeCandidates.find((tg) => tg.id === v);
                    setMergeTargetName(found ? `#${found.name}` : "");
                  }}
                  options={[
                    { value: "", label: t("cnt.tag_merge_target") },
                    ...mergeCandidates.map((tg) => ({
                      value: tg.id,
                      label: `#${tg.name}`,
                      badge: String(tg.article_count ?? 0),
                    })),
                  ]}
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
                  setSerSelPage(1);
                  setSerArtPage(1);
                  setSerModalOpen(true);
                }}
              >
                <PlusIcon size={14} /> {t("cnt.new_series")}
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
                        <div class="volo-cell-main" style={{ minWidth: 0 }}>
                          <div class="volo-cell-title" style={{ minWidth: 0, overflowWrap: "anywhere" }}>{s.title}</div>
                          {s.description && (
                            <div class="volo-cell-meta" style={{ minWidth: 0 }}>
                              <span style={{ minWidth: 0, overflowWrap: "anywhere" }}>{s.description}</span>
                            </div>
                          )}
                        </div>
                      </td>
                      <td class="col-cat" data-label={t("cnt.col_slug")}>
                        <span class="volo-code-chip is-wrap">/series/{s.slug}</span>
                      </td>
                      <td class="col-num" data-label={t("cnt.col_articles")}>
                        <span class="volo-cat-chip">
                          {Array.isArray(s.article_ids)
                            ? s.article_ids.length
                            : Array.isArray(s.articles)
                            ? s.articles.length
                            : (s.article_count ?? 0)}
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
                              if (Array.isArray(s.articles) && s.articles.length > 0) {
                                setKnownArticlesMap((prev) => {
                                  const next = { ...prev };
                                  s.articles.forEach((a: any) => {
                                    if (a?.id) next[a.id] = a;
                                  });
                                  return next;
                                });
                              }
                              const ids = Array.isArray(s.article_ids)
                                ? [...s.article_ids]
                                : Array.isArray(s.articles)
                                ? s.articles.map((a: any) => a.id)
                                : [];
                              setSerForm({
                                id: s.id,
                                title: s.title || "",
                                slug: s.slug || "",
                                description: s.description || "",
                                cover_url: s.cover_url || "",
                                sort_order: s.sort_order ?? 10,
                                article_ids: ids,
                              });
                              setSerArtFilter("");
                              setSerSelPage(1);
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

              {serForm.article_ids.length > 0 && (() => {
                const pgSel = paginateItems(serForm.article_ids, serSelPage, 15);
                const selOffset = (pgSel.safePage - 1) * 15;
                return (
                  <div style={{ marginBottom: "12px" }}>
                    <div ref={serFlip.containerRef} class="volo-subcard" style={{ maxHeight: "220px", overflowY: "auto" }}>
                      <div class="volo-subcard-title">
                        <span>
                          {t("cnt.ser_articles_lbl")} ({serForm.article_ids.length})
                        </span>
                      </div>
                      {pgSel.slice.map((aid, localIdx) => {
                        const idx = selOffset + localIdx;
                        const art = knownArticlesMap[aid];
                        const titleText = art?.title || aid;
                        return (
                          <div
                            key={aid}
                            data-flip-key={aid}
                            class="volo-input-row volo-series-sel-row"
                            style={{ marginBottom: "6px", minWidth: 0, flexWrap: "wrap" }}
                          >
                            <input
                              key={`${aid}-${idx}`}
                              type="number"
                              min={1}
                              max={serForm.article_ids.length}
                              class="volo-input"
                              style={{ width: "54px", padding: "3px 6px", textAlign: "center", flexShrink: 0 }}
                              defaultValue={String(idx + 1)}
                              aria-label={t("cnt.lbl_sort")}
                              onKeyDown={(e) => {
                                if (e.key === "Enter") {
                                  e.preventDefault();
                                  const target = Number((e.currentTarget as HTMLInputElement).value) || idx + 1;
                                  moveSeriesArticleTo(idx, target - 1);
                                }
                              }}
                              onBlur={(e) => {
                                const target = Number((e.currentTarget as HTMLInputElement).value) || idx + 1;
                                moveSeriesArticleTo(idx, target - 1);
                              }}
                            />
                            <span
                              class="volo-series-sel-title"
                              style={{
                                flex: "1 1 160px",
                                minWidth: 0,
                                overflow: "hidden",
                                textOverflow: "ellipsis",
                                whiteSpace: "nowrap",
                                overflowWrap: "anywhere",
                                fontSize: "13px",
                              }}
                              title={titleText}
                            >
                              {titleText}
                            </span>
                            <div class="volo-row-actions volo-series-sel-acts" style={{ flexShrink: 0 }}>
                              <button
                                type="button"
                                class="volo-act-btn"
                                disabled={idx === 0}
                                title={t("cnt.move_top")}
                                onClick={() => moveSeriesArticleTo(idx, 0)}
                              >
                                ⇈
                              </button>
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
                                class="volo-act-btn"
                                disabled={idx === serForm.article_ids.length - 1}
                                title={t("cnt.move_bottom")}
                                onClick={() => moveSeriesArticleTo(idx, serForm.article_ids.length - 1)}
                              >
                                ⇊
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
                                <CloseIcon size={13} />
                              </button>
                            </div>
                          </div>
                        );
                      })}
                    </div>
                    {serForm.article_ids.length > 15 && (
                      <Pagination total={serForm.article_ids.length} page={pgSel.safePage} pageSize={15} onChange={setSerSelPage} />
                    )}
                  </div>
                );
              })()}

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
                          style={{ justifyContent: "space-between", gap: "8px", minWidth: 0 }}
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
                                overflowWrap: "anywhere",
                              }}
                            >
                              {art.title || art.slug}
                            </span>
                          </span>
                          <span class="volo-code-chip" style={{ flexShrink: 0, maxWidth: "42%", overflow: "hidden", textOverflow: "ellipsis" }}>
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
                  { value: "rs:reading", label: t("cnt.book_rs_reading") },
                  { value: "rs:finished", label: t("cnt.book_rs_finished") },
                  { value: "rs:wishlist", label: t("cnt.book_rs_wishlist") },
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
                <PlusIcon size={14} /> {t("cnt.new_book")}
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
              <tbody ref={bookFlip.containerRef}>
                {loading && books.length === 0 ? (
                  <TableSkeleton cols={4} />
                ) : displayBooks.length === 0 ? (
                  <ConsoleEmpty colSpan={4} />
                ) : (
                  displayBooks.map((b) => {
                    const absIdx = books.findIndex((x) => x.id === b.id);
                    const baseOffset = (Math.max(1, pageIdx) - 1) * pageSize;
                    const globalIdx = absIdx >= 0 ? baseOffset + absIdx : -1;
                    const totalBooks = Math.max(subTotal, baseOffset + books.length);
                    const isBookFiltered = Boolean(bookQuery.trim() || bookFilter);
                    return (
                      <tr key={b.id} data-flip-key={b.id}>
                        <td class="col-fluid" data-label={t("cnt.book_col")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">{b.title}</div>
                            <div class="volo-cell-meta">
                              <span class="volo-code-chip">/books/{b.slug}</span>
                              {b.description && <span>{b.description}</span>}
                              {(b.source_url || b.series_id) && (
                                <span class="volo-help" style={{ overflowWrap: "anywhere" }}>
                                  {t("cnt.book_source", { src: b.source_url || b.series_id })}
                                </span>
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
                              disabled={isBookFiltered || swappingBook || absIdx < 0 || globalIdx <= 0}
                              title={isBookFiltered ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
                              onClick={() => void swapBookOrder(absIdx, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={isBookFiltered || swappingBook || absIdx < 0 || globalIdx >= totalBooks - 1}
                              title={isBookFiltered ? t("ui.sort_disabled_filtered") : t("cnt.lbl_sort")}
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
                      fallbackLabel={
                        bookForm.series_id
                          ? seriesLabels[bookForm.series_id] ||
                            (bookForm as any).series_title ||
                            bookForm.series_id
                          : t("art.series_none")
                      }
                      onSearch={async (q) => {
                        try {
                          const qs = new URLSearchParams({ per_page: "100" });
                          if (q) qs.set("q", q);
                          const res = await apiFetch<{ items?: any[]; series?: any[] }>(`/api/admin/series?${qs.toString()}`);
                          const items = res.items || res.series || [];
                          setBookSeriesOpts(items);
                          if (items.length > 0) {
                            setSeriesLabels((prev) => {
                              const next = { ...prev };
                              for (const s of items) {
                                if (s?.id && s.title) next[s.id] = s.title;
                              }
                              return next;
                            });
                          }
                        } catch {}
                      }}
                      options={(() => {
                        const base = bookSeriesOpts ?? series;
                        const opts = [
                          { value: "", label: t("art.series_none") },
                          ...base.map((s) => ({ value: s.id, label: s.title })),
                        ];
                        if (
                          bookForm.series_id &&
                          !opts.some((o) => o.value === bookForm.series_id)
                        ) {
                          opts.push({
                            value: bookForm.series_id,
                            label:
                              seriesLabels[bookForm.series_id] ||
                              (bookForm as any).series_title ||
                              bookForm.series_id,
                          });
                        }
                        return opts;
                      })()}
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
                <div class="volo-field">
                  <label>{t("art.status")}</label>
                  <ConsoleSelect
                    value={bookForm.reading_status || ""}
                    onChange={(val) => setBookForm({ ...bookForm, reading_status: val })}
                    options={[
                      { value: "", label: t("art.all") },
                      { value: "reading", label: t("cnt.book_rs_reading") },
                      { value: "finished", label: t("cnt.book_rs_finished") },
                      { value: "wishlist", label: t("cnt.book_rs_wishlist") },
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
                {chapItems.length > 0 && (
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
                {Boolean(chapBook?.source_url || chapBook?.series_id) && (
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    disabled={syncingChaps}
                    onClick={() => void syncModalChapters()}
                  >
                    {syncingChaps ? t("shell.loading") : t("cnt.book_sync_toc")}
                  </button>
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
                    setChapPage(next.length > 80 ? Math.max(1, Math.ceil(next.length / 100)) : 1);
                  }}
                >
                  <PlusIcon size={14} /> {t("cnt.chap_add")}
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

                const useChapPaging = chapItems.length > 80;
                const pagedRows = paginateItems(visibleRows, useChapPaging ? chapPage : 1, useChapPaging ? 100 : Math.max(100, visibleRows.length));
                chapSliceUidsRef.current = pagedRows.slice.map((r) => r.ch._uid);

                return visibleRows.length === 0 ? (
                  <ConsoleEmpty />
                ) : (
                  <>
                    <div ref={chapSortHook.listRef} class="volo-chap-tree">
                      {pagedRows.slice.map(({ ch, idx, hasChildren, isCollapsed }, sliceIdx) => {
                        const depth = clampChapDepth(ch.depth);
                        const canDrag = !q && pagedRows.slice.length > 1;
                        const canUp = !q && canMoveChapUp(chapItems, idx);
                        const canDown = !q && canMoveChapDown(chapItems, idx);
                        const handleProps = chapSortHook.getHandleProps(sliceIdx);
                        return (
                          <div
                            key={ch._uid}
                            data-flip-key={ch._uid}
                            data-sort-idx={sliceIdx}
                            class={`volo-chap-row volo-chap-indent ${chapSortHook.draggingIdx === sliceIdx ? "is-dragging" : ""} ${chapSortHook.grabbingIdx === sliceIdx ? "is-grabbing" : ""}`}
                            style={{ "--chap-depth": depth } as any}
                          >
                            {canDrag && (
                              <span
                                class="volo-drag-handle"
                                title={t("app.drag_sort")}
                                aria-label={t("app.drag_sort")}
                                {...(handleProps as any)}
                              >
                                <DragGripIcon />
                              </span>
                            )}
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
                                  width: "22px",
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
                                <PlusIcon size={14} />
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={!canUp}
                                title={t("app.move_top")}
                                aria-label={t("app.move_top")}
                                onClick={() => moveChapterToTop(idx)}
                              >
                                <MoveTopIcon />
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={!canUp}
                                title={t("app.move_up")}
                                onClick={() => moveChapterSubtree(idx, -1)}
                              >
                                ↑
                              </button>
                              <button
                                type="button"
                                class="volo-chap-btn"
                                disabled={!canDown}
                                title={t("app.move_down")}
                                onClick={() => moveChapterSubtree(idx, 1)}
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
                                <CloseIcon size={13} />
                              </button>
                            </div>
                          </div>
                        );
                      })}
                    </div>
                    {useChapPaging && (
                      <Pagination
                        total={visibleRows.length}
                        page={pagedRows.safePage}
                        pageSize={100}
                        onChange={setChapPage}
                      />
                    )}
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

              {pendingPageDraft && (
                <div class="volo-draft-banner">
                  <span>{t("ov.draft_banner", { time: formatDateTime(pendingPageDraft.saved_at) })}</span>
                  <div class="volo-row-actions">
                    <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={applyPendingPageDraft}>
                      {t("art.draft_restore")}
                    </button>
                    <button type="button" class="volo-btn volo-btn-sm" onClick={discardPendingPageDraft}>
                      {t("art.draft_discard")}
                    </button>
                  </div>
                </div>
              )}

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
                        options={pageTemplateOptions}
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
                  <div class="volo-check-row-end">
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
                  <button
                    type="button"
                    class="volo-btn volo-btn-primary volo-btn-sm"
                    onClick={() => void openNewPageEditor()}
                  >
                    <PlusIcon size={14} /> {t("cnt.new_page")}
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
                                <span class="volo-mini-tag">
                                  {!p.template || p.template === "default"
                                    ? t("cnt.tpl_default")
                                    : pageTemplateOptions.find((o) => o.value === p.template)?.label || p.template}
                                </span>
                                <span>
                                  {availableLocalesSig.value.find((l) => l.code === p.lang)?.name ||
                                    (p.lang === "zh-TW" ? "繁體中文" : p.lang)}
                                </span>
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
                total={subTotal}
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
                  { val: "trash", label: t("cnt.cmt_trash"), count: cmtStatusCounts.trash },
                ].map((it) => (
                  <button
                    key={it.val}
                    type="button"
                    class={`volo-filter-pill ${cmtStatus === it.val ? "active" : ""}`}
                    onClick={() => {
                      if ((cmtStatus === "trash") !== (it.val === "trash")) {
                        setSelCmtIds([]);
                        setCmtBox(null);
                      }
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

          {cmtTrashMode && (
            <div
              class="volo-row-actions"
              style={{ justifyContent: "space-between", alignItems: "center", flexWrap: "wrap", gap: "8px", margin: "0 0 10px" }}
            >
              <span class="volo-help">
                {cmtTrashRetentionDays && cmtTrashRetentionDays > 0
                  ? t("cnt.cmt_trash_hint_auto", { n: cmtTrashRetentionDays })
                  : t("cnt.cmt_trash_hint_manual")}
              </span>
              {isAdmin && (
                <button
                  type="button"
                  class="volo-btn volo-btn-danger volo-btn-sm"
                  disabled={!cmtStatusCounts.trash}
                  onClick={() => void emptyCommentTrash()}
                >
                  {t("cnt.cmt_empty_trash")}
                </button>
              )}
            </div>
          )}

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
                    const cmtUserId = String(c.user_id || row.user_id || "");
                    const canModUser = isAdmin && Boolean(cmtUserId) && cmtUserId !== currentUserSig.value?.id;
                    const nowMs = Date.now();
                    const uModState = {
                      status: row.user_status ?? c.user_status,
                      ban_until: row.user_ban_until ?? c.user_ban_until,
                      muted_until: row.user_muted_until ?? c.user_muted_until,
                    };
                    const cmtUserBanned =
                      Boolean(cmtUserId) &&
                      (Boolean(row.user_banned ?? c.user_banned) || isAccountBannedNow(uModState, nowMs));
                    const cmtUserMuted =
                      Boolean(cmtUserId) &&
                      (Boolean(row.user_muted ?? c.user_muted) || isAccountMutedNow(uModState, nowMs));
                    const cmtUserShielded =
                      Boolean(row.is_shielded ?? c.is_shielded) ||
                      (Boolean(row.user_shield_comments ?? c.user_shield_comments) && (cmtUserBanned || cmtUserMuted));
                    const targetTitle = row.target_title || row.target_slug || c.target_id;
                    const targetHref =
                      row.target_url ||
                      (c.target_type === "page"
                        ? `/${encodeURIComponent(row.target_slug || c.target_id)}`
                        : `/posts/${encodeURIComponent(row.target_slug || c.target_id)}`);
                    const safeAuthorUrl = /^https?:\/\//i.test(String(c.author_url || ""))
                      ? String(c.author_url)
                      : "";
                    const uaSummary = summarizeUA(c.user_agent);
                    const matchedParent = c.parent_id
                      ? comments.map((r) => r.comment || r).find((cm) => cm.id === c.parent_id)
                      : null;
                    const parentObj = row.parent_comment || matchedParent;
                    const parentAuthor =
                      row.parent_author || parentObj?.author_name || (c.parent_id ? `#${String(c.parent_id).slice(0, 6)}` : "");
                    const parentSnippet = String(row.parent_content || parentObj?.content_md || "").slice(0, 90);
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
                              {safeAuthorUrl ? (
                                <a href={safeAuthorUrl} target="_blank" rel="noopener noreferrer">
                                  {c.author_name} ↗
                                </a>
                              ) : (
                                <span>{c.author_name}</span>
                              )}
                              {cmtUserBanned && (
                                <span class="volo-status-pill danger">{t("sys.user_status_banned")}</span>
                              )}
                              {cmtUserMuted && (
                                <span class="volo-status-pill warn">{t("sys.user_status_muted")}</span>
                              )}
                              {cmtUserShielded && (
                                <span class="volo-mini-tag">{t("sys.user_shielded_badge")}</span>
                              )}
                              {c.geo_region && <span class="volo-mini-tag">{c.geo_region}</span>}
                              {uaSummary && (
                                <span class="volo-mini-tag" title={c.user_agent}>
                                  {uaSummary}
                                </span>
                              )}
                              {(c.client_ip || c.ip_masked) && (
                                <span class="volo-code-chip">{c.client_ip || c.ip_masked}</span>
                              )}
                              <a class="volo-help" href={targetHref} target="_blank" rel="noreferrer">
                                {t("cnt.cmt_on", { target: targetTitle })} ↗
                              </a>
                            </div>
                            {parentAuthor && (
                              <div class="volo-help" style={{ fontSize: "12px", opacity: 0.85 }}>
                                ↳ {t("cnt.cmt_reply_to", { author: parentAuthor })}
                                {parentSnippet ? `: “${parentSnippet}${parentSnippet.length >= 90 ? "…" : ""}”` : ""}
                              </div>
                            )}
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
                                {cmtBox.mode === "edit" && (
                                  <div class="volo-auto-grid" style={{ marginBottom: "6px" }}>
                                    <div class="volo-field" style={{ marginBottom: 0 }}>
                                      <label>{t("cnt.cmt_guest_name")}</label>
                                      <input
                                        class="volo-input"
                                        maxLength={48}
                                        value={cmtBox.authorName ?? ""}
                                        onInput={(e) =>
                                          setCmtBox({ ...cmtBox, authorName: (e.currentTarget as HTMLInputElement).value })
                                        }
                                      />
                                    </div>
                                    <div class="volo-field" style={{ marginBottom: 0 }}>
                                      <label>{t("cnt.cmt_guest_avatar")}</label>
                                      <input
                                        class="volo-input"
                                        type="url"
                                        maxLength={512}
                                        placeholder="https://…"
                                        value={cmtBox.avatarUrl ?? ""}
                                        onInput={(e) =>
                                          setCmtBox({ ...cmtBox, avatarUrl: (e.currentTarget as HTMLInputElement).value })
                                        }
                                      />
                                    </div>
                                    <div class="volo-field" style={{ marginBottom: 0 }}>
                                      <label>{t("cnt.cmt_guest_site")}</label>
                                      <input
                                        class="volo-input"
                                        type="url"
                                        maxLength={512}
                                        placeholder="https://…"
                                        value={cmtBox.authorUrl ?? ""}
                                        onInput={(e) =>
                                          setCmtBox({ ...cmtBox, authorUrl: (e.currentTarget as HTMLInputElement).value })
                                        }
                                      />
                                    </div>
                                  </div>
                                )}
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
                          {cmtTrashMode ? (
                            <span class="volo-status-pill" title={cmtStatusLabel(c.status)}>
                              <span class="volo-status-dot" />
                              {t("cnt.cmt_deleted")}
                            </span>
                          ) : (
                            <span
                              class={`volo-status-pill ${
                                c.status === "approved" ? "ok" : c.status === "rejected" ? "danger" : "warn"
                              }`}
                            >
                              <span class="volo-status-dot" />
                              {cmtStatusLabel(c.status)}
                            </span>
                          )}
                        </td>
                        <td class="col-date" data-label={cmtTrashMode ? t("cnt.cmt_deleted_at") : t("art.updated")}>
                          {cmtTrashMode ? (
                            <span title={formatDateTime(c.created_at)}>{formatDateTime(c.updated_at || c.created_at)}</span>
                          ) : (
                            formatDateTime(c.created_at)
                          )}
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            {cmtTrashMode ? (
                              <>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  onClick={async () => {
                                    try {
                                      await apiFetch(`/api/admin/comments/${c.id}/restore`, { method: "POST" });
                                      showToast(t("cnt.cmt_restored"), "ok");
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  }}
                                >
                                  {t("cnt.cmt_restore")}
                                </button>
                                <button
                                  type="button"
                                  class="volo-act-btn danger"
                                  onClick={async () => {
                                    if (!(await openConfirm({ message: t("cnt.cmt_confirm_purge"), danger: true }))) return;
                                    try {
                                      await apiFetch(`/api/admin/comments/${c.id}?purge=1`, { method: "DELETE" });
                                      showToast(t("cnt.cmt_purged"));
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  }}
                                >
                                  {t("cnt.cmt_purge")}
                                </button>
                              </>
                            ) : (
                            <>
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
                                    : {
                                        id: c.id,
                                        mode: "edit",
                                        text: c.content_md || "",
                                        authorName: c.author_name || "",
                                        avatarUrl: c.avatar_url || "",
                                        authorUrl: c.author_url || "",
                                        origAuthorName: c.author_name || "",
                                        origAvatarUrl: c.avatar_url || "",
                                        origAuthorUrl: c.author_url || "",
                                      }
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
                                  label: t("sys.user_mod_btn"),
                                  hidden: !canModUser,
                                  danger: true,
                                  onClick: () =>
                                    setCmtUserModTarget({
                                      userId: cmtUserId,
                                      label: c.author_name ? `${c.author_name} (${cmtUserId.slice(0, 8)})` : cmtUserId,
                                      defaultAction: cmtUserMuted && !cmtUserBanned ? "mute" : "ban",
                                      banReason: String(row.user_ban_reason ?? c.user_ban_reason ?? ""),
                                      shieldComments:
                                        row.user_shield_comments !== undefined || c.user_shield_comments !== undefined
                                          ? Boolean(row.user_shield_comments ?? c.user_shield_comments)
                                          : true,
                                      shieldText: String(row.user_shield_text ?? c.user_shield_text ?? ""),
                                    }),
                                },
                                {
                                  label: t("sys.user_unban"),
                                  hidden: !canModUser || !cmtUserBanned,
                                  onClick: () => void runQuickUserUnmod(cmtUserId, "unban", () => void loadSub()),
                                },
                                {
                                  label: t("sys.user_unmute"),
                                  hidden: !canModUser || !cmtUserMuted,
                                  onClick: () => void runQuickUserUnmod(cmtUserId, "unmute", () => void loadSub()),
                                },
                                {
                                  label: t("cnt.cmt_ban_ip"),
                                  hidden: !canBanIp,
                                  danger: true,
                                  onClick: () => {
                                    setBanDurationHrs("720");
                                    setBanReason("comment spam");
                                    setBanRejectPending(true);
                                    setBanIpModal({
                                      commentId: c.id,
                                      ipLabel: c.client_ip || c.ip_masked || c.author_name,
                                    });
                                  },
                                },
                                {
                                  label: t("common.delete"),
                                  danger: true,
                                  onClick: async () => {
                                    if (!(await openConfirm({ message: t("cnt.confirm_del_cmt"), danger: true }))) return;
                                    try {
                                      await apiFetch(`/api/admin/comments/${c.id}`, { method: "DELETE" });
                                      showToast(t("cnt.cmt_moved_to_trash"));
                                      void loadSub();
                                    } catch (err: any) {
                                      showToast(err.message);
                                    }
                                  },
                                },
                              ]}
                            />
                            </>
                            )}
                          </div>
                        </td>
                      </tr>
                    );
                  })
                )}
              </tbody>
            </table>
          </div>

          <BatchBar count={selCmtIds.length} onClear={() => setSelCmtIds([])}>
            {cmtTrashMode ? (
              <>
                <button type="button" class="volo-act-btn" onClick={() => void runBatchComments("restore")}>
                  {t("cnt.cmt_restore")}
                </button>
                <button type="button" class="volo-act-btn danger" onClick={() => void runBatchComments("purge")}>
                  {t("cnt.cmt_purge")}
                </button>
              </>
            ) : (
              <>
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
              </>
            )}
          </BatchBar>

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

          <UserModerationModal
            target={cmtUserModTarget}
            onClose={() => setCmtUserModTarget(null)}
            onSaved={() => void loadSub()}
          />

          <AnimatedModal
            open={!!banIpModal}
            onClose={() => setBanIpModal(null)}
            title={t("cnt.cmt_ban_ip")}
            subtitle={banIpModal?.ipLabel || ""}
            maxWidth="460px"
            footer={
              <>
                <button type="button" class="volo-btn" onClick={() => setBanIpModal(null)}>
                  {t("common.cancel")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-danger"
                  disabled={banningIp}
                  onClick={() => void submitBanIpModal()}
                >
                  {t("cnt.cmt_ban_ip")}
                </button>
              </>
            }
          >
            <form onSubmit={(e) => void submitBanIpModal(e)} autoComplete="off">
              <div class="volo-field">
                <label>{t("cnt.cmt_ban_duration")}</label>
                <ConsoleSelect
                  searchable={false}
                  value={banDurationHrs}
                  onChange={setBanDurationHrs}
                  options={[
                    { value: "1", label: t("cnt.cmt_ban_1h") },
                    { value: "24", label: t("cnt.cmt_ban_24h") },
                    { value: "168", label: t("cnt.cmt_ban_7d") },
                    { value: "720", label: t("cnt.cmt_ban_30d") },
                    { value: "0", label: t("cnt.cmt_ban_perm") },
                  ]}
                />
              </div>
              <div class="volo-field">
                <label>{t("cnt.cmt_ban_reason")}</label>
                <input
                  class="volo-input"
                  value={banReason}
                  onInput={(e) => setBanReason((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field" style={{ marginBottom: 0 }}>
                <SwitchToggle
                  checked={banRejectPending}
                  onChange={setBanRejectPending}
                  label={t("cnt.cmt_ban_reject_pending")}
                />
              </div>
            </form>
          </AnimatedModal>
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

            {uploadingMedia && mediaUploadPct !== null && (
              <div style={{ marginBottom: "12px" }}>
                <UploadProgressBar
                  progress={mediaUploadPct}
                  label={t("cnt.media_uploading_pct", { pct: mediaUploadPct })}
                />
              </div>
            )}

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
                <ConsoleDatePicker
                  mode="month"
                  ariaLabel={t("cnt.media_month_lbl")}
                  value={mediaMonthFilter}
                  onChange={(val) => {
                    setMediaMonthFilter(val);
                    setPageIdx(1);
                  }}
                />
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
                    setMediaKindFilter(val as "all" | "image" | "video" | "audio" | "other");
                    setPageIdx(1);
                  }}
                  options={[
                    { value: "all", label: t("cnt.media_type_all") },
                    { value: "image", label: t("cnt.media_type_image") },
                    { value: "video", label: t("cnt.media_type_video") },
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
                <div class={`volo-media-grid ${selMediaIds.length > 0 ? "has-selection" : ""}`}>
                  {displayMedia.map((m) => {
                    const displayName = m.original_name || m.id;
                    const mime = String(m.mime_type || "");
                    const isImage = mime.startsWith("image/") && !brokenMediaIds[m.id];
                    const isVideo = mime.startsWith("video/") && !brokenMediaIds[m.id];
                    const isAudio = mime.startsWith("audio/");
                    const isSmallImg = m.width > 0 && m.width <= 160 && m.height > 0 && m.height <= 140;
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
                              aria-label={displayName}
                              checked={isSelected}
                              onChange={() => toggleSel(selMediaIds, setSelMediaIds, m.id)}
                            />
                          </label>
                          {isImage ? (
                            <img
                              src={m.public_url}
                              alt={m.alt_text || displayName}
                              loading="lazy"
                              decoding="async"
                              class={isSmallImg ? "is-small-img" : ""}
                              onError={() => setBrokenMediaIds((prev) => ({ ...prev, [m.id]: true }))}
                            />
                          ) : isVideo ? (
                            <video
                              src={m.public_url}
                              preload="metadata"
                              muted
                              playsInline
                              onError={() => setBrokenMediaIds((prev) => ({ ...prev, [m.id]: true }))}
                            />
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
                            <button
                              type="button"
                              class="volo-act-btn danger is-icon"
                              title={t("common.delete")}
                              aria-label={t("common.delete")}
                              onClick={() => void handleDeleteMediaItem(m)}
                            >
                              <svg
                                viewBox="0 0 16 16"
                                width="13"
                                height="13"
                                fill="none"
                                stroke="currentColor"
                                stroke-width="1.5"
                                stroke-linecap="round"
                                stroke-linejoin="round"
                                aria-hidden="true"
                              >
                                <path d="M2.5 4.2h11M6.2 4.2V2.8h3.6v1.4M4 4.2l.7 9h6.6l.7-9M6.6 6.7v4.2M9.4 6.7v4.2" />
                              </svg>
                            </button>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>
              </>
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
                        const isImage = mime.startsWith("image/") && !brokenMediaIds[m.id];
                        const isVideo = mime.startsWith("video/") && !brokenMediaIds[m.id];
                        const isAudio = mime.startsWith("audio/");
                        const isSmallImg = m.width > 0 && m.width <= 160 && m.height > 0 && m.height <= 140;
                        const extBadge = (displayName.split(".").pop() || mime.split("/").pop() || "FILE")
                          .toUpperCase()
                          .slice(0, 5);
                        const driverLabel =
                          m.storage_driver === "local"
                            ? t("cnt.driver_local_short")
                            : m.storage_driver === "s3"
                              ? "S3"
                              : m.storage_driver;
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
                              <div class="volo-media-list-asset">
                                <button
                                  type="button"
                                  class="volo-media-list-thumb"
                                  onClick={() => openAssetLightbox(m)}
                                  aria-label={displayName}
                                >
                                  {isImage ? (
                                    <img
                                      src={m.public_url}
                                      alt={m.alt_text || displayName}
                                      loading="lazy"
                                      decoding="async"
                                      class={isSmallImg ? "is-small-img" : ""}
                                      onError={() => setBrokenMediaIds((prev) => ({ ...prev, [m.id]: true }))}
                                    />
                                  ) : isVideo ? (
                                    <video
                                      src={m.public_url}
                                      preload="metadata"
                                      muted
                                      playsInline
                                      style={{ width: "100%", height: "100%", objectFit: "cover" }}
                                      onError={() => setBrokenMediaIds((prev) => ({ ...prev, [m.id]: true }))}
                                    />
                                  ) : isAudio ? (
                                    <svg
                                      width="18"
                                      height="18"
                                      viewBox="0 0 24 24"
                                      fill="none"
                                      stroke="currentColor"
                                      stroke-width="1.9"
                                      stroke-linecap="round"
                                      stroke-linejoin="round"
                                      aria-hidden="true"
                                    >
                                      <path d="M9 18V5l12-2v13" />
                                      <circle cx="6" cy="18" r="3" />
                                      <circle cx="18" cy="16" r="3" />
                                    </svg>
                                  ) : (
                                    <span class="volo-media-list-ext">{extBadge}</span>
                                  )}
                                </button>
                                <div class="volo-cell-main" style={{ minWidth: 0 }}>
                                  <div class="volo-cell-title" style={{ minWidth: 0 }}>
                                    <button
                                      type="button"
                                      class="volo-act-btn"
                                      style={{ minWidth: 0, maxWidth: "100%", overflowWrap: "anywhere", textAlign: "left" }}
                                      onClick={() => openAssetLightbox(m)}
                                    >
                                      {displayName}
                                    </button>
                                    {driverLabel && <span class="volo-mini-tag">{driverLabel}</span>}
                                  </div>
                                  <div class="volo-cell-meta" style={{ minWidth: 0 }}>
                                    <span class="volo-code-chip is-wrap">{m.public_url}</span>
                                  </div>
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

            <BatchBar count={selMediaIds.length} onClear={() => setSelMediaIds([])}>
              <button type="button" class="volo-act-btn danger" onClick={() => void batchDeleteMedia()}>
                {t("common.delete")}
              </button>
            </BatchBar>

            <Pagination
              total={subTotal}
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
                  {String(previewAsset.mime_type || "").startsWith("image/") && !brokenMediaIds[previewAsset.id] ? (
                    <img
                      src={previewAsset.public_url}
                      alt={previewAsset.alt_text || previewAsset.original_name}
                      onError={() => setBrokenMediaIds((prev) => ({ ...prev, [previewAsset.id]: true }))}
                    />
                  ) : String(previewAsset.mime_type || "").startsWith("video/") && !brokenMediaIds[previewAsset.id] ? (
                    <video
                      controls
                      preload="metadata"
                      src={previewAsset.public_url}
                      style={{ maxWidth: "100%", maxHeight: "56vh", borderRadius: "8px" }}
                      onError={() => setBrokenMediaIds((prev) => ({ ...prev, [previewAsset.id]: true }))}
                    />
                  ) : String(previewAsset.mime_type || "").startsWith("audio/") ? (
                    <audio controls src={previewAsset.public_url} class="volo-w-full" />
                  ) : (
                    <div class="volo-media-type-placeholder" style={{ minWidth: 0, padding: "12px" }}>
                      <span class="volo-code-chip">{previewAsset.mime_type || "FILE"}</span>
                      <span style={{ minWidth: 0, maxWidth: "100%", overflowWrap: "anywhere", textAlign: "center" }}>
                        {previewAsset.original_name}
                      </span>
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
