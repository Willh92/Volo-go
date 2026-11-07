import { useEffect, useState } from "preact/hooks";
import {
  apiFetch,
  clearLocalDraft,
  currentUserSig,
  loadLocalDraft,
  LocalDraft,
  resetConsoleScroll,
  saveLocalDraft,
  showToast,
} from "../api";
import { CM6Editor, QuickInsertItem } from "../components/CM6Editor";
import { MediaPickButton } from "../components/MediaLibraryModal";
import { AnimatedModal, ConsoleSelect, paginateItems, Pagination } from "../components/UIControls";
import { availableLocalesSig, formatDateTime, localeSig, t } from "../i18n";

export type OverviewSection = "dashboard" | "editor" | "articles" | "drafts";

interface ChartPoint {
  x: number;
  y: number;
}

const DONUT_PALETTE = [
  "#246b51",
  "#2d6583",
  "#c9822b",
  "#70517d",
  "#87525f",
  "#3b7a75",
  "#8c6d3f",
];

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

function buildFallbackMonths(months = 12): Array<{ month: string; count: number }> {
  const out: Array<{ month: string; count: number }> = [];
  const now = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  for (let i = months - 1; i >= 0; i--) {
    const dt = new Date(now.getFullYear(), now.getMonth() - i, 1);
    out.push({
      month: `${dt.getFullYear()}-${pad(dt.getMonth() + 1)}`,
      count: 0,
    });
  }
  return out;
}

function parseTagList(raw: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  raw
    .split(/[,，]/)
    .map((s) => s.trim())
    .filter(Boolean)
    .forEach((item) => {
      const k = item.toLowerCase();
      if (!seen.has(k)) {
        seen.add(k);
        out.push(item);
      }
    });
  return out;
}

function msToLocalDatetimeInput(ms?: number): string {
  if (!ms || ms <= 0) return "";
  const dt = new Date(ms);
  if (Number.isNaN(dt.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${dt.getFullYear()}-${pad(dt.getMonth() + 1)}-${pad(dt.getDate())}T${pad(dt.getHours())}:${pad(dt.getMinutes())}`;
}

function localDatetimeInputToMs(val: string): number {
  if (!val || !val.trim()) return 0;
  const parsed = new Date(val).getTime();
  return Number.isNaN(parsed) ? 0 : parsed;
}

interface DiffLine {
  kind: "same" | "add" | "del";
  text: string;
}

function computeLineDiff(oldText: string, newText: string): DiffLine[] {
  const a = oldText.split("\n").slice(0, 260);
  const b = newText.split("\n").slice(0, 260);
  const m = a.length;
  const n = b.length;
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array(n + 1).fill(0));
  for (let i = m - 1; i >= 0; i--) {
    for (let j = n - 1; j >= 0; j--) {
      if (a[i] === b[j]) {
        dp[i][j] = dp[i + 1][j + 1] + 1;
      } else {
        dp[i][j] = Math.max(dp[i + 1][j], dp[i][j + 1]);
      }
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (a[i] === b[j]) {
      out.push({ kind: "same", text: `  ${a[i]}` });
      i++;
      j++;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      out.push({ kind: "del", text: `- ${a[i]}` });
      i++;
    } else {
      out.push({ kind: "add", text: `+ ${b[j]}` });
      j++;
    }
  }
  while (i < m) {
    out.push({ kind: "del", text: `- ${a[i++]}` });
  }
  while (j < n) {
    out.push({ kind: "add", text: `+ ${b[j++]}` });
  }
  return out;
}

function MonthlyArticlesChart({ items }: { items: Array<{ month: string; count: number }> }) {
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const series = items.length > 0 ? items : buildFallbackMonths(12);
  const W = 320;
  const H = 158;
  const padL = 28;
  const padR = 14;
  const padT = 14;
  const padB = 26;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const maxVal = Math.max(2, ...series.map((d) => Number(d.count || 0)));
  const yMax = Math.ceil(maxVal / 2) * 2;
  const n = series.length;

  const pts: ChartPoint[] = series.map((d, idx) => ({
    x: padL + (n <= 1 ? plotW / 2 : (idx / (n - 1)) * plotW),
    y: padT + plotH - (Number(d.count || 0) / yMax) * plotH,
  }));
  const line = buildSmoothCurve(pts);
  const area =
    pts.length > 0
      ? `${line} L ${pts[pts.length - 1].x.toFixed(1)} ${(padT + plotH).toFixed(1)} L ${pts[0].x.toFixed(1)} ${(padT + plotH).toFixed(1)} Z`
      : "";

  const active = hoverIdx !== null && series[hoverIdx] ? series[hoverIdx] : null;
  const activePt = hoverIdx !== null && pts[hoverIdx] ? pts[hoverIdx] : null;

  return (
    <div class="volo-chart-stage" style={{ flex: 1, display: "flex", alignItems: "center" }}>
      {active && activePt && (
        <div
          class="volo-chart-tooltip"
          style={{
            left: activePt.x > W * 0.62 ? "auto" : `${Math.max(6, (activePt.x / W) * 100)}%`,
            right: activePt.x > W * 0.62 ? `${Math.max(6, ((W - activePt.x) / W) * 100)}%` : "auto",
          }}
        >
          <div style={{ fontWeight: 650 }}>{active.month}</div>
          <div>{t("ov.published")}: {active.count}</div>
        </div>
      )}
      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", height: "auto", display: "block" }}
        onMouseMove={(e) => {
          const rect = (e.currentTarget as SVGSVGElement).getBoundingClientRect();
          if (rect.width <= 0 || n === 0) return;
          const svgX = ((e.clientX - rect.left) / rect.width) * W;
          const rawIdx = n <= 1 ? 0 : Math.round(((svgX - padL) / plotW) * (n - 1));
          setHoverIdx(Math.max(0, Math.min(n - 1, rawIdx)));
        }}
        onMouseLeave={() => setHoverIdx(null)}
      >
        <defs>
          <linearGradient id="voloMonthlyGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stop-color="var(--volo-accent)" stop-opacity="0.24" />
            <stop offset="100%" stop-color="var(--volo-accent)" stop-opacity="0.02" />
          </linearGradient>
        </defs>
        {[0, 0.5, 1].map((ratio, idx) => {
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
                stroke-dasharray={ratio === 0 ? undefined : "3 3"}
              />
              <text x={padL - 5} y={y + 3.5} text-anchor="end" font-size="10" fill="var(--volo-muted)">
                {val}
              </text>
            </g>
          );
        })}
        {area && <path d={area} fill="url(#voloMonthlyGrad)" />}
        {line && <path d={line} fill="none" stroke="var(--volo-accent)" stroke-width="2.2" stroke-linecap="round" />}
        {activePt && (
          <line
            x1={activePt.x}
            y1={padT}
            x2={activePt.x}
            y2={padT + plotH}
            stroke="var(--volo-accent)"
            stroke-dasharray="3 3"
            stroke-opacity="0.55"
          />
        )}
        {series.map((d, idx) => {
          const p = pts[idx];
          const showLbl = idx === 0 || idx === Math.floor(n / 2) || idx === n - 1;
          const isHover = hoverIdx === idx;
          return (
            <g key={d.month}>
              <circle
                cx={p.x}
                cy={p.y}
                r={isHover ? 4.4 : 2.6}
                fill="var(--volo-surface)"
                stroke="var(--volo-accent)"
                stroke-width={isHover ? 2.4 : 1.8}
              />
              {showLbl && (
                <text x={p.x} y={H - 6} text-anchor="middle" font-size="10" fill="var(--volo-muted)">
                  {String(d.month).slice(2)}
                </text>
              )}
            </g>
          );
        })}
      </svg>
    </div>
  );
}

function DonutChart({
  items,
  emptyText,
  prefix = "",
}: {
  items: Array<{ name: string; count: number }>;
  emptyText: string;
  prefix?: string;
}) {
  const positive = (items || []).filter((it) => Number(it.count || 0) > 0).slice(0, 6);
  const total = positive.reduce((acc, it) => acc + Number(it.count || 0), 0);

  if (positive.length === 0 || total <= 0) {
    return (
      <div class="volo-empty-state" style={{ padding: "28px 10px", flex: 1, display: "flex", alignItems: "center", justifyContent: "center" }}>
        {emptyText}
      </div>
    );
  }

  const r = 38;
  const cx = 52;
  const cy = 52;
  const circ = 2 * Math.PI * r;
  let offsetAcc = 0;

  return (
    <div class="volo-donut-wrap">
      <svg width="104" height="104" viewBox="0 0 104 104" style={{ flexShrink: 0 }}>
        <circle cx={cx} cy={cy} r={r} fill="none" stroke="var(--volo-line)" stroke-width="15" />
        {positive.map((it, idx) => {
          const val = Number(it.count || 0);
          const frac = val / total;
          const segLen = Math.max(0, frac * circ);
          const curOffset = offsetAcc;
          offsetAcc += segLen;
          const color = DONUT_PALETTE[idx % DONUT_PALETTE.length];
          const pct = Math.round(frac * 100);
          return (
            <circle
              key={`${it.name}-${idx}`}
              cx={cx}
              cy={cy}
              r={r}
              fill="none"
              stroke={color}
              stroke-width="15"
              stroke-dasharray={`${segLen.toFixed(2)} ${circ.toFixed(2)}`}
              stroke-dashoffset={(-curOffset).toFixed(2)}
              transform={`rotate(-90 ${cx} ${cy})`}
            >
              <title>{`${prefix}${it.name}: ${val} (${pct}%)`}</title>
            </circle>
          );
        })}
        <text x={cx} y={cy - 1} text-anchor="middle" font-size="15" font-weight="700" fill="var(--volo-text)">
          {total}
        </text>
        <text x={cx} y={cy + 13} text-anchor="middle" font-size="9.5" fill="var(--volo-muted)">
          {t("ov.chart_total")}
        </text>
      </svg>

      <div class="volo-donut-legend">
        {positive.map((it, idx) => {
          const val = Number(it.count || 0);
          const pct = Math.round((val / total) * 100);
          const color = DONUT_PALETTE[idx % DONUT_PALETTE.length];
          return (
            <div key={`${it.name}-${idx}`} class="volo-donut-legend-row">
              <div style={{ display: "flex", alignItems: "center", gap: "7px", minWidth: 0 }}>
                <span class="volo-donut-dot" style={{ background: color }} />
                <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={`${prefix}${it.name}`}>
                  {prefix}{it.name}
                </span>
              </div>
              <span class="volo-help" style={{ fontFamily: "var(--volo-mono)", whiteSpace: "nowrap" }}>
                {val} ({pct}%)
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
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
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const [articles, setArticles] = useState<any[]>([]);
  const [categories, setCategories] = useState<any[]>([]);
  const [seriesList, setSeriesList] = useState<any[]>([]);
  const [allTags, setAllTags] = useState<any[]>([]);
  const [statusFilter, setStatusFilter] = useState<string>(section === "drafts" ? "draft" : "all");
  const [catFilter, setCatFilter] = useState<string>("");
  const [query, setQuery] = useState<string>("");
  const [artPage, setArtPage] = useState<number>(1);

  // Editor state (`null` = not editing, `""` = new post, `"id"` = editing post)
  const [editingID, setEditingID] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [slug, setSlug] = useState("");
  const [categoryID, setCategoryID] = useState("");
  const [tagsText, setTagsText] = useState("");
  const [tagDraft, setTagDraft] = useState("");
  const [tagPoolOpen, setTagPoolOpen] = useState(false);
  const [seriesID, setSeriesID] = useState("");
  const [summaryMD, setSummaryMD] = useState("");
  const [contentMD, setContentMD] = useState("");
  const [coverURL, setCoverURL] = useState("");
  const [status, setStatus] = useState("published");
  const [publishedAtStr, setPublishedAtStr] = useState("");
  const [scheduledAtStr, setScheduledAtStr] = useState("");
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
  const [diffRevID, setDiffRevID] = useState<string | null>(null);
  const [shareTTL, setShareTTL] = useState<string>("72");
  const [shareURL, setShareURL] = useState<string>("");

  const draftKey = editingID === "" ? "article:new" : editingID ? `article:${editingID}` : "";

  const startNewArticle = async (catsOverride?: any[]) => {
    const catList = catsOverride || categories;
    setEditingID("");
    setTitle("");
    setSlug("");
    setCategoryID(catList[0]?.id || "");
    setTagsText("");
    setTagDraft("");
    setTagPoolOpen(false);
    setSeriesID("");
    setSummaryMD("");
    setContentMD("");
    setCoverURL("");
    setStatus("published");
    setPublishedAtStr("");
    setScheduledAtStr("");
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
    resetConsoleScroll();
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
    const [arts, cats, sers, tgs] = await Promise.all([
      apiFetch<{ items: any[] }>(
        `/api/admin/articles?status=${encodeURIComponent(effStatus)}&category_id=${encodeURIComponent(effCat)}&q=${encodeURIComponent(effQuery)}`
      ).catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/categories").catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/series").catch(() => ({ items: [] })),
      apiFetch<{ items: any[] }>("/api/admin/tags").catch(() => ({ items: [] })),
    ]);
    setArticles(arts.items || []);
    setCategories(cats.items || []);
    setSeriesList(sers.items || []);
    setAllTags(tgs.items || []);
    return cats.items || [];
  };

  // Synchronize sidebar section selection
  useEffect(() => {
    setArtPage(1);
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
    setHoverIdx(null);
    void loadOverview(trendDays);
  }, [trendDays]);

  useEffect(() => {
    setArtPage(1);
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
      setTagDraft("");
      setTagPoolOpen(false);
      setSeriesID(data.series_id || "");
      setSummaryMD(a.summary_md || "");
      setContentMD(a.content_md || "");
      setCoverURL(a.cover_url || "");
      setStatus(a.status || "published");
      setPublishedAtStr(msToLocalDatetimeInput(a.published_at));
      setScheduledAtStr(msToLocalDatetimeInput(a.scheduled_at));
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
      resetConsoleScroll();
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
    setTagDraft("");
    resetConsoleScroll();
    if (section === "editor") {
      onNavigate?.("articles");
    }
  };

  const selectedTags = parseTagList(tagsText);

  const addTagTokens = (rawInput: string) => {
    const incoming = parseTagList(rawInput);
    if (incoming.length === 0) return;
    const merged = [...selectedTags];
    incoming.forEach((tg) => {
      if (!merged.some((x) => x.toLowerCase() === tg.toLowerCase())) {
        merged.push(tg);
      }
    });
    setTagsText(merged.join(", "));
    setTagDraft("");
  };

  const removeTagToken = (tagName: string) => {
    const filtered = selectedTags.filter((x) => x.toLowerCase() !== tagName.toLowerCase());
    setTagsText(filtered.join(", "));
  };

  const toggleTagChip = (tagName: string) => {
    if (selectedTags.some((x) => x.toLowerCase() === tagName.toLowerCase())) {
      removeTagToken(tagName);
    } else {
      addTagTokens(tagName);
    }
  };

  const saveArticle = async (e: Event) => {
    e.preventDefault();
    try {
      const finalTags = parseTagList(tagDraft.trim() ? `${tagsText}, ${tagDraft}` : tagsText);
      const payload: any = {
        category_id: categoryID,
        title,
        slug,
        summary_md: summaryMD,
        content_md: contentMD,
        cover_url: coverURL,
        status,
        published_at: localDatetimeInputToMs(publishedAtStr),
        scheduled_at: status === "scheduled" ? localDatetimeInputToMs(scheduledAtStr) : 0,
        is_pinned: isPinned,
        allow_comment: allowComment,
        clear_password: clearPassword,
        audio_url: audioURL,
        audio_duration_sec: Number(audioDuration) || 0,
        audio_mime: audioMime,
        lang,
        translation_group: translationGroup,
        tags: finalTags,
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
      setTagDraft("");
      resetConsoleScroll();
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

  const togglePinArticle = async (id: string) => {
    try {
      await apiFetch(`/api/admin/articles/${id}/pin`, { method: "POST", body: "{}" });
      showToast(t("common.saved"));
      void loadArticlesAndTaxonomies();
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
      setDiffRevID(null);
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
        body: JSON.stringify({ ttl_hours: Number(shareTTL) || 72 }),
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
    const activeTagsLower = new Set(selectedTags.map((s) => s.toLowerCase()));

    return (
      <div>
        <div class="volo-card">
          <div class="volo-card-head">
            <h2 class="volo-card-title">{!editingID ? t("art.new") : `${t("common.edit")}: ${title || t("art.title")}`}</h2>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
              {editingID && (
                <>
                  <button type="button" class="volo-btn volo-btn-sm" onClick={openRevisions}>
                    {t("art.revisions")}
                  </button>
                  <div style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
                    <span class="volo-help" style={{ whiteSpace: "nowrap" }}>{t("art.share_ttl")}:</span>
                    <ConsoleSelect
                      style={{ width: "96px" }}
                      value={shareTTL}
                      onChange={setShareTTL}
                      options={[
                        { value: "1", label: "1h" },
                        { value: "24", label: "24h" },
                        { value: "72", label: "72h" },
                        { value: "168", label: "7d" },
                        { value: "720", label: "30d" },
                      ]}
                    />
                    <button type="button" class="volo-btn volo-btn-sm" onClick={issueShareLink}>
                      {t("art.share_token")}
                    </button>
                  </div>
                </>
              )}
              <button type="button" class="volo-btn volo-btn-sm" onClick={closeEditor}>
                {t("common.cancel")}
              </button>
            </div>
          </div>

          {pendingDraft && (
            <div class="volo-card" style={{ background: "var(--volo-accent-soft)", padding: "12px 16px" }}>
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "12px", flexWrap: "wrap" }}>
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
            <div
              class="volo-subcard"
              style={{
                padding: "10px 14px",
                fontSize: "12.5px",
                display: "flex",
                alignItems: "center",
                justifyContent: "space-between",
                gap: "12px",
                flexWrap: "wrap",
                marginBottom: "14px",
              }}
            >
              <div style={{ wordBreak: "break-all" }}>
                <strong>{t("art.share_label")}</strong>{" "}
                <a href={shareURL} target="_blank" rel="noreferrer">{shareURL}</a>
              </div>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  void navigator.clipboard?.writeText(shareURL);
                  showToast(t("art.share_copied"));
                }}
              >
                {t("art.share_copy")}
              </button>
            </div>
          )}

          <form onSubmit={saveArticle} autoComplete="off">
            {/* 1. Prominent Full-Width Article Title */}
            <div class="volo-field" style={{ marginBottom: "14px" }}>
              <input
                class="volo-editor-title-input"
                required
                aria-label={t("art.title")}
                placeholder={t("art.title_ph")}
                value={title}
                onInput={(e) => setTitle((e.currentTarget as HTMLInputElement).value)}
              />
            </div>

            {/* 2. Full-Width Markdown Studio Editor Immediately Below Title */}
            <div class="volo-field" style={{ marginBottom: "16px" }}>
              <label>{t("art.md_body")}</label>
              <CM6Editor value={contentMD} onChange={setContentMD} quickInserts={quickInserts} />
            </div>

            {/* 3. Custom Summary */}
            <div class="volo-field">
              <label>{t("art.summary")}</label>
              <textarea
                class="volo-textarea"
                rows={2}
                value={summaryMD}
                onInput={(e) => setSummaryMD((e.currentTarget as HTMLTextAreaElement).value)}
              />
            </div>

            {/* 4. Capsule Tag Selector (Full Width, Zero Right-Side Whitespace) */}
            <div class="volo-field">
              <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "8px", marginBottom: "6px" }}>
                <label style={{ margin: 0 }}>{t("art.free_tags")}</label>
                {allTags.length > 0 && (
                  <button
                    type="button"
                    class={`volo-chip-btn ${tagPoolOpen ? "active" : ""}`}
                    onClick={() => setTagPoolOpen(!tagPoolOpen)}
                  >
                    {t("art.toggle_tag_pool", { count: allTags.length })}
                  </button>
                )}
              </div>
              <div class="volo-tag-capsule-box">
                {selectedTags.map((tg) => (
                  <span key={tg} class="volo-tag-pill">
                    #{tg}
                    <button
                      type="button"
                      aria-label={t("common.delete")}
                      onClick={() => removeTagToken(tg)}
                    >
                      ×
                    </button>
                  </span>
                ))}
                <input
                  class="volo-tag-inline-input"
                  value={tagDraft}
                  placeholder={selectedTags.length === 0 ? t("art.tags_placeholder") : t("art.tag_input_more")}
                  onInput={(e) => setTagDraft((e.currentTarget as HTMLInputElement).value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === "," || e.key === "，") {
                      e.preventDefault();
                      if (tagDraft.trim()) addTagTokens(tagDraft);
                    } else if (e.key === "Backspace" && !tagDraft && selectedTags.length > 0) {
                      removeTagToken(selectedTags[selectedTags.length - 1]);
                    }
                  }}
                  onBlur={() => {
                    if (tagDraft.trim()) addTagTokens(tagDraft);
                  }}
                />
              </div>
              {tagPoolOpen && allTags.length > 0 && (
                <div class="volo-tag-pool" style={{ marginTop: "8px" }}>
                  <span class="volo-help" style={{ fontSize: "11.5px" }}>{t("art.tag_pool_hint")}</span>
                  {allTags.map((tg) => {
                    const active = activeTagsLower.has(String(tg.name || "").toLowerCase());
                    return (
                      <button
                        key={tg.id}
                        type="button"
                        class={`volo-chip-btn ${active ? "active" : ""}`}
                        onClick={() => toggleTagChip(tg.name)}
                      >
                        #{tg.name}
                      </button>
                    );
                  })}
                </div>
              )}
            </div>

            {/* 5. Compact Publishing Settings Grid */}
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.slug")}</label>
                <input
                  class="volo-input"
                  placeholder={t("art.slug_placeholder")}
                  value={slug}
                  onInput={(e) => setSlug((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              <div class="volo-field">
                <label>{t("art.category")}</label>
                <ConsoleSelect
                  value={categoryID}
                  onChange={setCategoryID}
                  options={[
                    { value: "", label: t("art.uncategorized") },
                    ...categories.map((c) => ({ value: c.id, label: c.name })),
                  ]}
                />
              </div>
              <div class="volo-field">
                <label>{t("art.status")}</label>
                <ConsoleSelect
                  value={status}
                  onChange={setStatus}
                  options={[
                    { value: "published", label: t("art.published") },
                    { value: "draft", label: t("art.draft") },
                    { value: "scheduled", label: t("art.scheduled") },
                  ]}
                />
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.series")}</label>
                <ConsoleSelect
                  value={seriesID}
                  onChange={setSeriesID}
                  options={[
                    { value: "", label: t("art.series_none") },
                    ...seriesList.map((s) => ({ value: s.id, label: s.title })),
                  ]}
                />
              </div>
              <div class="volo-field">
                <label>{t("art.lang")}</label>
                <ConsoleSelect
                  value={lang}
                  onChange={setLang}
                  options={locales.map((loc) => ({
                    value: loc.code,
                    label: loc.name,
                    badge: loc.code,
                  }))}
                />
              </div>
              <div class="volo-field">
                <label>{t("art.trans_group")}</label>
                <input
                  class="volo-input"
                  placeholder={t("art.trans_placeholder")}
                  value={translationGroup}
                  onInput={(e) => setTranslationGroup((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            </div>

            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.password")}</label>
                <input
                  type="password"
                  name="article_lock_passphrase"
                  autoComplete="new-password"
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
                <label>{t("art.published_at")}</label>
                <input
                  type="datetime-local"
                  class="volo-input"
                  value={publishedAtStr}
                  onInput={(e) => setPublishedAtStr((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
              {status === "scheduled" ? (
                <div class="volo-field">
                  <label>{t("art.scheduled_at")}</label>
                  <input
                    type="datetime-local"
                    class="volo-input"
                    required
                    value={scheduledAtStr}
                    onInput={(e) => setScheduledAtStr((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              ) : (
                <div class="volo-field">
                  <label>{t("art.rev_note")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("art.rev_note_placeholder")}
                    value={revisionNote}
                    onInput={(e) => setRevisionNote((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              )}
            </div>

            {status === "scheduled" && (
              <div class="volo-field">
                <label>{t("art.rev_note")}</label>
                <input
                  class="volo-input"
                  placeholder={t("art.rev_note_placeholder")}
                  value={revisionNote}
                  onInput={(e) => setRevisionNote((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            )}

            {/* 6. Cover Image & Podcast Audio with Media Library Pickers */}
            <div class="volo-grid-3">
              <div class="volo-field">
                <label>{t("art.cover_url")}</label>
                <div class="volo-input-row">
                  <input
                    class="volo-input"
                    value={coverURL}
                    onInput={(e) => setCoverURL((e.currentTarget as HTMLInputElement).value)}
                  />
                  <MediaPickButton filterKind="image" onPick={(url) => setCoverURL(url)} />
                </div>
                {coverURL.trim() && (
                  <img
                    src={coverURL}
                    alt="cover"
                    style={{
                      marginTop: "6px",
                      maxHeight: "68px",
                      maxWidth: "140px",
                      objectFit: "cover",
                      borderRadius: "6px",
                      border: "1px solid var(--volo-line)",
                    }}
                  />
                )}
              </div>
              <div class="volo-field">
                <label>{t("art.audio_url")}</label>
                <div class="volo-input-row">
                  <input
                    class="volo-input"
                    value={audioURL}
                    onInput={(e) => setAudioURL((e.currentTarget as HTMLInputElement).value)}
                  />
                  <MediaPickButton filterKind="audio" onPick={(url) => setAudioURL(url)} />
                </div>
              </div>
              <div class="volo-field">
                <label>{t("art.audio_duration")}</label>
                <input
                  type="number"
                  min={0}
                  class="volo-input"
                  value={audioDuration}
                  onInput={(e) => setAudioDuration(Number((e.currentTarget as HTMLInputElement).value) || 0)}
                />
              </div>
            </div>

            <div style={{ display: "flex", alignItems: "center", gap: "18px", flexWrap: "wrap", margin: "8px 0 4px" }}>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={isPinned}
                  onChange={(e) => setIsPinned((e.currentTarget as HTMLInputElement).checked)}
                />
                {t("art.pin_top")}
              </label>
              <label class="volo-check-item">
                <input
                  type="checkbox"
                  checked={allowComment}
                  onChange={(e) => setAllowComment((e.currentTarget as HTMLInputElement).checked)}
                />
                {t("art.allow_comment")}
              </label>
              {editingID && (
                <label class="volo-check-item">
                  <input
                    type="checkbox"
                    checked={clearPassword}
                    onChange={(e) => {
                      const checked = (e.currentTarget as HTMLInputElement).checked;
                      setClearPassword(checked);
                      if (checked) setPassword("");
                    }}
                  />
                  {t("art.clear_password")}
                </label>
              )}
            </div>

            {/* 7. Right-Aligned Form Actions */}
            <div class="volo-form-actions">
              <button type="button" class="volo-btn" onClick={closeEditor}>
                {t("common.cancel")}
              </button>
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("art.save")}
              </button>
            </div>
          </form>
        </div>

        <AnimatedModal
          open={Boolean(revisions)}
          onClose={() => setRevisions(null)}
          title={t("art.revisions_max")}
          maxWidth="760px"
          footer={
            <button type="button" class="volo-btn" onClick={() => setRevisions(null)}>
              {t("common.close")}
            </button>
          }
        >
          {!revisions || revisions.length === 0 ? (
            <p class="volo-help">{t("art.revisions_empty")}</p>
          ) : (
            revisions.map((r) => {
              const showDiff = diffRevID === r.id;
              const diffLines = showDiff ? computeLineDiff(r.content_md || "", contentMD) : [];
              return (
                <div key={r.id} class="volo-card" style={{ padding: "12px", marginBottom: "10px" }}>
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "8px", flexWrap: "wrap", marginBottom: "6px" }}>
                    <div>
                      <strong>{r.title}</strong>{" "}
                      <span class="volo-help">· {formatDateTime(r.created_at)} · {r.note || t("art.snapshot")}</span>
                    </div>
                    <div style={{ display: "flex", gap: "6px" }}>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={() => setDiffRevID(showDiff ? null : r.id)}
                      >
                        {t("art.diff_btn")}
                      </button>
                      <button type="button" class="volo-btn volo-btn-sm volo-btn-primary" onClick={() => restoreRevision(r.id)}>
                        {t("art.rollback")}
                      </button>
                    </div>
                  </div>
                  {showDiff ? (
                    <div>
                      <div class="volo-help" style={{ marginBottom: "4px" }}>{t("art.diff_title")}</div>
                      <div class="volo-diff-box">
                        {diffLines.map((ln, idx) => (
                          <div
                            key={idx}
                            class={`volo-diff-line ${
                              ln.kind === "add" ? "volo-diff-add" : ln.kind === "del" ? "volo-diff-del" : ""
                            }`}
                          >
                            {ln.text}
                          </div>
                        ))}
                      </div>
                    </div>
                  ) : (
                    <pre style={{ margin: 0, maxHeight: "110px", overflow: "auto", fontSize: "11.5px", background: "var(--volo-raised)", padding: "8px", borderRadius: "6px" }}>
                      {(r.content_md || "").slice(0, 600)}
                    </pre>
                  )}
                </div>
              );
            })
          )}
        </AnimatedModal>
      </div>
    );
  }

  // 2. Article Management / Drafts View (`section === "articles" | "drafts"`)
  if (section === "articles" || section === "drafts") {
    const pagedArts = paginateItems(articles, artPage, 15);
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
            <ConsoleSelect
              style={{ width: "164px" }}
              value={catFilter}
              onChange={setCatFilter}
              options={[
                { value: "", label: t("art.all_categories") },
                ...categories.map((c) => ({ value: c.id, label: c.name })),
              ]}
            />
            <input
              class="volo-input"
              style={{ width: "204px" }}
              placeholder={t("art.search_placeholder")}
              value={query}
              onInput={(e) => setQuery((e.currentTarget as HTMLInputElement).value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  setArtPage(1);
                  void loadArticlesAndTaxonomies(statusFilter, catFilter, query);
                }
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
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
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
              {pagedArts.slice.length === 0 ? (
                <tr>
                  <td colSpan={6} class="volo-empty-state">
                    {t("art.empty", { btn: t("art.new") })}
                  </td>
                </tr>
              ) : (
                pagedArts.slice.map((row) => {
                  const a = row.article;
                  const catLabel = catNameMap[a.category_id];
                  return (
                    <tr key={a.id}>
                      <td class="col-fluid">
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">
                            <a
                              href="javascript:void(0)"
                              onClick={(e) => {
                                e.preventDefault();
                                void openEditArticle(a.id);
                              }}
                            >
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
                            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                              <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" />
                              <circle cx="12" cy="12" r="3" />
                            </svg>
                            {a.view_count ?? 0}
                          </span>
                          <span class="volo-metric-item" title={t("nav.comments")}>
                            <svg width="12.5" height="12.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                              <path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
                            </svg>
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
                          <button type="button" class="volo-act-btn" onClick={() => togglePinArticle(a.id)}>
                            {a.is_pinned ? t("art.unpin_action") : t("art.pin_action")}
                          </button>
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

        <Pagination total={articles.length} page={pagedArts.safePage} pageSize={15} onChange={setArtPage} />
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

  // SVG Chart Geometry (`padR = 34` prevents rightmost MM-DD label clipping)
  const W = 860;
  const H = 228;
  const padL = 44;
  const padR = 34;
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
  const uvArea =
    uvPts.length > 0
      ? `${uvLine} L ${uvPts[uvPts.length - 1].x.toFixed(1)} ${(padT + plotH).toFixed(1)} L ${uvPts[0].x.toFixed(1)} ${(padT + plotH).toFixed(1)} Z`
      : "";
  // Show every single date tick in 7-day mode (`n <= 10`)
  const xLabelStep = n <= 10 ? 1 : Math.ceil(n / 8);
  const topArticles: any[] = overview?.top_articles || [];
  const topReferers: any[] = overview?.top_referers || [];
  const recentLogs: any[] = overview?.recent_logs || [];
  const noticeBoard: string = overview?.notice_board || "";

  const activeTrend = hoverIdx !== null && rawSeries[hoverIdx] ? rawSeries[hoverIdx] : null;
  const activePvPt = hoverIdx !== null && pvPts[hoverIdx] ? pvPts[hoverIdx] : null;
  const activeUvPt = hoverIdx !== null && uvPts[hoverIdx] ? uvPts[hoverIdx] : null;

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
              <span style={{ width: "10px", height: "10px", borderRadius: "3px", background: "var(--volo-accent)" }} />
              {t("ov.legend_pv")}
            </span>
            <span style={{ display: "inline-flex", alignItems: "center", gap: "6px" }}>
              <span style={{ width: "10px", height: "10px", borderRadius: "3px", background: "#c9822b" }} />
              {t("ov.legend_uv")}
            </span>
          </div>

          <div class="volo-chart-stage">
            {activeTrend && activePvPt && (
              <div
                class="volo-chart-tooltip"
                style={{
                  left: activePvPt.x > W * 0.68 ? "auto" : `${Math.max(5, (activePvPt.x / W) * 100)}%`,
                  right: activePvPt.x > W * 0.68 ? `${Math.max(5, ((W - activePvPt.x) / W) * 100)}%` : "auto",
                }}
              >
                <div style={{ fontWeight: 650, marginBottom: "2px" }}>{activeTrend.stat_date}</div>
                <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
                  <span>PV: <strong>{activeTrend.pv}</strong></span>
                  <span style={{ color: "#f5be72" }}>UV: <strong>{activeTrend.uv}</strong></span>
                </div>
              </div>
            )}

            <svg
              viewBox={`0 0 ${W} ${H}`}
              style={{ width: "100%", height: "auto", display: "block", cursor: "crosshair" }}
              onMouseMove={(e) => {
                const rect = (e.currentTarget as SVGSVGElement).getBoundingClientRect();
                if (rect.width <= 0 || n === 0) return;
                const svgX = ((e.clientX - rect.left) / rect.width) * W;
                const rawIdx = n <= 1 ? 0 : Math.round(((svgX - padL) / plotW) * (n - 1));
                setHoverIdx(Math.max(0, Math.min(n - 1, rawIdx)));
              }}
              onMouseLeave={() => setHoverIdx(null)}
            >
              <defs>
                <linearGradient id="voloPvGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="var(--volo-accent)" stop-opacity="0.22" />
                  <stop offset="100%" stop-color="var(--volo-accent)" stop-opacity="0.01" />
                </linearGradient>
                <linearGradient id="voloUvGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="#c9822b" stop-opacity="0.16" />
                  <stop offset="100%" stop-color="#c9822b" stop-opacity="0.01" />
                </linearGradient>
              </defs>

              {/* Vertical X-axis dashed gridlines */}
              {rawSeries.map((d, idx) => {
                const showTick = idx % xLabelStep === 0 || idx === n - 1;
                if (!showTick) return null;
                const p = pvPts[idx];
                return (
                  <line
                    key={`vx-${d.stat_date}`}
                    x1={p.x}
                    y1={padT}
                    x2={p.x}
                    y2={padT + plotH}
                    stroke="var(--volo-line)"
                    stroke-dasharray="3 4"
                    stroke-opacity="0.65"
                  />
                );
              })}

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
                      stroke-dasharray={ratio === 0 ? undefined : "4 4"}
                    />
                    <text
                      x={padL - 8}
                      y={y + 4}
                      text-anchor="end"
                      font-size="11"
                      fill="var(--volo-muted)"
                    >
                      {val}
                    </text>
                  </g>
                );
              })}

              {uvArea && <path d={uvArea} fill="url(#voloUvGrad)" />}
              {pvArea && <path d={pvArea} fill="url(#voloPvGrad)" />}
              {pvLine && <path d={pvLine} fill="none" stroke="var(--volo-accent)" stroke-width="2.5" stroke-linecap="round" />}
              {uvLine && <path d={uvLine} fill="none" stroke="#c9822b" stroke-width="2" stroke-dasharray="5 4" stroke-linecap="round" />}

              {activePvPt && (
                <line
                  x1={activePvPt.x}
                  y1={padT}
                  x2={activePvPt.x}
                  y2={padT + plotH}
                  stroke="var(--volo-accent)"
                  stroke-dasharray="3 3"
                  stroke-opacity="0.6"
                />
              )}

              {rawSeries.map((d, idx) => {
                const p = pvPts[idx];
                const u = uvPts[idx];
                const showLabel = idx % xLabelStep === 0 || idx === n - 1;
                const isHover = hoverIdx === idx;
                return (
                  <g key={d.stat_date}>
                    <circle
                      cx={u.x}
                      cy={u.y}
                      r={isHover ? 4.4 : 2.6}
                      fill="var(--volo-surface)"
                      stroke="#c9822b"
                      stroke-width={isHover ? 2.2 : 1.6}
                    />
                    <circle
                      cx={p.x}
                      cy={p.y}
                      r={isHover ? 5 : 3.4}
                      fill="var(--volo-surface)"
                      stroke="var(--volo-accent)"
                      stroke-width={isHover ? 2.5 : 2}
                    />
                    {showLabel && (
                      <text
                        x={p.x}
                        y={H - 8}
                        text-anchor="middle"
                        font-size="11"
                        fill={isHover ? "var(--volo-text)" : "var(--volo-muted)"}
                        font-weight={isHover ? "650" : "400"}
                      >
                        {String(d.stat_date).slice(5)}
                      </text>
                    )}
                  </g>
                );
              })}
            </svg>
          </div>
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

      {/* Content Structure & Monthly Output Panel (3 Charts) */}
      <div class="volo-card">
        <div class="volo-card-head">
          <div>
            <h3 class="volo-card-title">{t("ov.data_head")}</h3>
            <div class="volo-help" style={{ marginTop: "2px" }}>{t("ov.data_sub")}</div>
          </div>
        </div>
        <div class="volo-grid-3">
          <div class="volo-mini-chart-card">
            <h4 class="volo-mini-chart-title">{t("ov.chart_monthly")}</h4>
            <MonthlyArticlesChart items={overview?.monthly_articles || []} />
          </div>
          <div class="volo-mini-chart-card">
            <h4 class="volo-mini-chart-title">{t("ov.chart_categories")}</h4>
            <DonutChart items={overview?.category_usage || []} emptyText={t("ov.chart_empty")} />
          </div>
          <div class="volo-mini-chart-card">
            <h4 class="volo-mini-chart-title">{t("ov.chart_top_tags")}</h4>
            <DonutChart items={overview?.top_tags || []} emptyText={t("ov.chart_empty")} prefix="#" />
          </div>
        </div>
      </div>

      {/* Notice Board & Recent Audit Logs Row */}
      <div class="volo-grid-2">
        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("ov.notice_head")}</h3>
            {isAdmin && (
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => onNavigate?.("settings_site")}>
                {t("ov.edit_notice")}
              </button>
            )}
          </div>
          {noticeBoard.trim() ? (
            <div
              class="volo-subcard"
              style={{ margin: 0, whiteSpace: "pre-wrap", lineHeight: 1.65, fontSize: "13.5px" }}
            >
              {noticeBoard}
            </div>
          ) : (
            <div class="volo-empty-state" style={{ padding: "26px 12px" }}>
              {t("ov.notice_empty")}
            </div>
          )}
        </div>

        <div class="volo-card">
          <div class="volo-card-head">
            <h3 class="volo-card-title">{t("ov.recent_logs_head")}</h3>
            {isAdmin && (
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => onNavigate?.("audit")}>
                {t("ov.view_all_logs")} →
              </button>
            )}
          </div>
          {recentLogs.length === 0 ? (
            <div class="volo-empty-state" style={{ padding: "26px 12px" }}>
              {t("ov.recent_logs_empty")}
            </div>
          ) : (
            <div>
              {recentLogs.slice(0, 6).map((lg: any) => (
                <div key={lg.id} class="volo-rank-row">
                  <div style={{ display: "flex", alignItems: "center", gap: "8px", minWidth: 0 }}>
                    <span class="volo-code-chip">{lg.action}</span>
                    <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontSize: "13px" }}>
                      <strong>{lg.actor_name || "-"}</strong>
                      {lg.resource_type ? ` · ${lg.resource_type}` : ""}
                    </span>
                  </div>
                  <span class="volo-help" style={{ fontFamily: "var(--volo-mono)", fontSize: "11.5px", whiteSpace: "nowrap" }}>
                    {formatDateTime(lg.created_at)}
                  </span>
                </div>
              ))}
            </div>
          )}
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
