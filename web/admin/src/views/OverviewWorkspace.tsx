import { useEffect, useRef, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import type { CSSProperties } from "preact";
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
} from "../api";
import { CM6Editor, QuickInsertItem } from "../components/CM6Editor";
import { MediaPickButton } from "../components/MediaLibraryModal";
import {
  ActionMenu,
  AnimatedModal,
  BatchBar,
  computePortalMenuStyle,
  ConsoleDatePicker,
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
import {
  availableLocalesSig,
  formatDateTime,
  localeSig,
  localizeAuditAction,
  localizeAuditResource,
  t,
} from "../i18n";

export type OverviewSection = "dashboard" | "editor" | "articles" | "drafts";

interface ChartPoint {
  x: number;
  y: number;
}

const DONUT_PALETTE = [
  "#2d9d78",
  "#3b82f6",
  "#e59934",
  "#9b6bcc",
  "#e06377",
  "#2bb3b1",
  "#c28e46",
];

function formatCompactNum(raw: number): string {
  const n = Number(raw || 0);
  if (!Number.isFinite(n)) return "0";
  const abs = Math.abs(n);
  if (abs >= 1_000_000) return `${(n / 1_000_000).toFixed(1).replace(/\.0$/, "")}M`;
  if (abs >= 1_000) return `${(n / 1_000).toFixed(1).replace(/\.0$/, "")}k`;
  return String(Math.round(n));
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

function pickSvgIndex(
  svg: SVGSVGElement,
  clientX: number,
  count: number,
  totalW: number,
  padL: number,
  plotW: number
): number | null {
  const rect = svg.getBoundingClientRect();
  if (rect.width <= 0 || count <= 0) return null;
  const svgX = ((clientX - rect.left) / rect.width) * totalW;
  const rawIdx = count <= 1 ? 0 : Math.round(((svgX - padL) / plotW) * (count - 1));
  return Math.max(0, Math.min(count - 1, rawIdx));
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
  oldLine?: number;
  newLine?: number;
}

type DiffChunk =
  | { type: "line"; line: DiffLine }
  | { type: "fold"; id: string; lines: DiffLine[] };

function computeLineDiff(oldText: string, newText: string): DiffLine[] {
  const a = oldText.split("\n");
  const b = newText.split("\n");
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (
    suf < a.length - pre &&
    suf < b.length - pre &&
    a[a.length - 1 - suf] === b[b.length - 1 - suf]
  ) {
    suf++;
  }

  const out: DiffLine[] = [];
  for (let k = 0; k < pre; k++) {
    out.push({ kind: "same", text: `  ${a[k]}`, oldLine: k + 1, newLine: k + 1 });
  }

  const aMid = a.slice(pre, a.length - suf);
  const bMid = b.slice(pre, b.length - suf);
  const m = aMid.length;
  const n = bMid.length;

  if (m > 0 || n > 0) {
    if (m * n <= 360_000) {
      const width = n + 1;
      const dp = new Int32Array((m + 1) * width);
      for (let i = m - 1; i >= 0; i--) {
        const row = i * width;
        const nextRow = (i + 1) * width;
        for (let j = n - 1; j >= 0; j--) {
          dp[row + j] =
            aMid[i] === bMid[j]
              ? dp[nextRow + j + 1] + 1
              : Math.max(dp[nextRow + j], dp[row + j + 1]);
        }
      }
      let i = 0;
      let j = 0;
      while (i < m && j < n) {
        if (aMid[i] === bMid[j]) {
          out.push({ kind: "same", text: `  ${aMid[i]}`, oldLine: pre + i + 1, newLine: pre + j + 1 });
          i++;
          j++;
        } else if (dp[(i + 1) * width + j] >= dp[i * width + j + 1]) {
          out.push({ kind: "del", text: `- ${aMid[i]}`, oldLine: pre + i + 1 });
          i++;
        } else {
          out.push({ kind: "add", text: `+ ${bMid[j]}`, newLine: pre + j + 1 });
          j++;
        }
      }
      while (i < m) {
        out.push({ kind: "del", text: `- ${aMid[i]}`, oldLine: pre + i + 1 });
        i++;
      }
      while (j < n) {
        out.push({ kind: "add", text: `+ ${bMid[j]}`, newLine: pre + j + 1 });
        j++;
      }
    } else {
      for (let i = 0; i < m; i++) {
        out.push({ kind: "del", text: `- ${aMid[i]}`, oldLine: pre + i + 1 });
      }
      for (let j = 0; j < n; j++) {
        out.push({ kind: "add", text: `+ ${bMid[j]}`, newLine: pre + j + 1 });
      }
    }
  }

  for (let k = 0; k < suf; k++) {
    const oi = a.length - suf + k;
    const ni = b.length - suf + k;
    out.push({ kind: "same", text: `  ${a[oi]}`, oldLine: oi + 1, newLine: ni + 1 });
  }
  return out;
}

function buildHunkChunks(lines: DiffLine[], expanded: Record<string, boolean>, ctx = 3): DiffChunk[] {
  const hasChange = lines.some((l) => l.kind !== "same");
  if (!hasChange) {
    if (lines.length <= ctx * 2 + 2 || expanded["all"]) {
      return lines.map((line) => ({ type: "line", line }));
    }
    return [
      ...lines.slice(0, ctx).map((line) => ({ type: "line" as const, line })),
      { type: "fold", id: "all", lines: lines.slice(ctx, lines.length - ctx) },
      ...lines.slice(lines.length - ctx).map((line) => ({ type: "line" as const, line })),
    ];
  }
  const chunks: DiffChunk[] = [];
  let idx = 0;
  while (idx < lines.length) {
    if (lines[idx].kind !== "same") {
      chunks.push({ type: "line", line: lines[idx++] });
      continue;
    }
    let end = idx;
    while (end < lines.length && lines[end].kind === "same") end++;
    const run = lines.slice(idx, end);
    const atStart = idx === 0;
    const atEnd = end === lines.length;
    const keepHead = atStart ? 0 : ctx;
    const keepTail = atEnd ? 0 : ctx;
    const foldId = `f:${idx}:${end}`;
    if (run.length > keepHead + keepTail + 1 && !expanded[foldId]) {
      for (let k = 0; k < keepHead; k++) chunks.push({ type: "line", line: run[k] });
      chunks.push({ type: "fold", id: foldId, lines: run.slice(keepHead, run.length - keepTail) });
      for (let k = run.length - keepTail; k < run.length; k++) chunks.push({ type: "line", line: run[k] });
    } else {
      for (const ln of run) chunks.push({ type: "line", line: ln });
    }
    idx = end;
  }
  return chunks;
}

function MonthlyArticlesChart({
  items,
  isHistory = false,
}: {
  items: Array<{ month: string; count: number }>;
  isHistory?: boolean;
}) {
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const series = items.length > 0 ? items : buildFallbackMonths(12);
  const W = 320;
  const H = 164;
  const padL = 36;
  const padR = 12;
  const padT = 16;
  const padB = 28;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const maxVal = Math.max(2, ...series.map((d) => Number(d.count || 0)));
  const yMax = Math.ceil(maxVal / 2) * 2;
  const n = Math.max(1, series.length);
  const slotW = plotW / n;
  const barW = Math.max(2.5, Math.min(18, slotW * (n > 24 ? 0.72 : 0.62)));

  const years = new Set(series.map((d) => String(d.month || "").slice(0, 4)).filter(Boolean));
  const multiYear = isHistory || years.size > 1;
  const maxLabels = multiYear ? 4 : 5;
  const labelIndices = new Set<number>();
  if (n <= maxLabels) {
    for (let i = 0; i < n; i++) labelIndices.add(i);
  } else {
    labelIndices.add(0);
    labelIndices.add(n - 1);
    for (let k = 1; k < maxLabels - 1; k++) {
      labelIndices.add(Math.round((k * (n - 1)) / (maxLabels - 1)));
    }
  }

  const pickBarIndex = (svg: SVGSVGElement, clientX: number): number | null => {
    const rect = svg.getBoundingClientRect();
    if (!rect.width) return null;
    const svgX = ((clientX - rect.left) / rect.width) * W;
    const raw = Math.floor((svgX - padL) / slotW);
    return Math.max(0, Math.min(n - 1, raw));
  };

  const active = hoverIdx !== null && series[hoverIdx] ? series[hoverIdx] : null;
  const activeCx = hoverIdx !== null && series[hoverIdx] ? padL + slotW * (hoverIdx + 0.5) : null;

  const onTouch = (e: TouchEvent) => {
    const touch = e.touches?.[0];
    if (!touch) return;
    const idx = pickBarIndex(e.currentTarget as SVGSVGElement, touch.clientX);
    if (idx !== null) setHoverIdx(idx);
  };

  return (
    <div class="volo-chart-stage" style={{ flex: 1, display: "flex", alignItems: "center" }}>
      {active && activeCx !== null && (
        <div
          class="volo-chart-tooltip"
          style={{
            left: activeCx > W * 0.5 ? "auto" : `${Math.max(4, (activeCx / W) * 100)}%`,
            right: activeCx > W * 0.5 ? `${Math.max(4, ((W - activeCx) / W) * 100)}%` : "auto",
          }}
        >
          <div style={{ fontWeight: 650 }}>{active.month}</div>
          <div>
            {t("ov.published")}: {Number(active.count || 0).toLocaleString()}
          </div>
        </div>
      )}
      <svg
        viewBox={`0 0 ${W} ${H}`}
        style={{ width: "100%", height: "auto", display: "block", touchAction: "pan-y" }}
        onMouseMove={(e) => {
          const idx = pickBarIndex(e.currentTarget as SVGSVGElement, e.clientX);
          if (idx !== null) setHoverIdx(idx);
        }}
        onMouseLeave={() => setHoverIdx(null)}
        onTouchStart={onTouch}
        onTouchMove={onTouch}
      >
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
              <text x={padL - 5} y={y + 3.5} text-anchor="end" font-size="10.5" fill="var(--volo-muted)">
                {formatCompactNum(val)}
              </text>
            </g>
          );
        })}
        {hoverIdx !== null && series[hoverIdx] && (
          <rect
            x={padL + slotW * hoverIdx}
            y={padT}
            width={slotW}
            height={plotH}
            rx="3"
            fill="var(--volo-accent)"
            fill-opacity="0.08"
          />
        )}
        {series.map((d, idx) => {
          const val = Number(d.count || 0);
          const cx = padL + slotW * (idx + 0.5);
          const rawH = (val / yMax) * plotH;
          const barH = val > 0 ? Math.max(2.5, rawH) : 1.5;
          const barY = padT + plotH - barH;
          const isHover = hoverIdx === idx;
          const showLbl = labelIndices.has(idx);
          const anchor =
            n > maxLabels && idx === 0 ? "start" : n > maxLabels && idx === n - 1 ? "end" : "middle";
          const lblX =
            anchor === "start" ? Math.max(padL, cx - barW / 2) : anchor === "end" ? Math.min(W - padR, cx + barW / 2) : cx;
          const lblText = multiYear ? String(d.month) : String(d.month).slice(2);
          return (
            <g key={`${d.month}-${idx}`}>
              <rect
                x={cx - barW / 2}
                y={barY}
                width={barW}
                height={barH}
                rx={Math.min(3, barW / 2)}
                fill={val > 0 ? "var(--volo-accent)" : "var(--volo-line)"}
                fill-opacity={val > 0 ? (isHover ? "1" : "0.82") : "0.65"}
              />
              {val > 0 && (n <= 16 || isHover) && (
                <text
                  x={cx}
                  y={Math.max(10.5, barY - 3.5)}
                  text-anchor="middle"
                  font-size="9.5"
                  font-weight="600"
                  fill={isHover ? "var(--volo-accent)" : "var(--volo-muted)"}
                >
                  {formatCompactNum(val)}
                </text>
              )}
              {showLbl && (
                <text
                  x={lblX}
                  y={H - 6}
                  text-anchor={anchor}
                  font-size={multiYear ? "9.5" : "10.5"}
                  fill="var(--volo-muted)"
                >
                  {lblText}
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
  items: Array<{ name: string; count?: number; article_count?: number }>;
  emptyText: string;
  prefix?: string;
}) {
  const allPositive = (items || [])
    .map((it) => ({
      name: it.name,
      count: Number(it.count ?? it.article_count ?? 0),
    }))
    .filter((it) => it.count > 0)
    .sort((a, b) => b.count - a.count);
  const total = allPositive.reduce((acc, it) => acc + it.count, 0);

  if (allPositive.length === 0 || total <= 0) {
    return (
      <div class="volo-empty-state" style={{ padding: "28px 10px", flex: 1, display: "flex", alignItems: "center", justifyContent: "center" }}>
        {emptyText}
      </div>
    );
  }

  const segments: Array<{ name: string; count: number; isOthers?: boolean }> =
    allPositive.length > 5
      ? [
          ...allPositive.slice(0, 5),
          {
            name: t("ov.chart_others"),
            count: allPositive.slice(5).reduce((acc, it) => acc + it.count, 0),
            isOthers: true,
          },
        ]
      : allPositive;

  const r = 38;
  const cx = 52;
  const cy = 52;
  const circ = 2 * Math.PI * r;
  let offsetAcc = 0;

  return (
    <div class="volo-donut-wrap">
      <svg width="104" height="104" viewBox="0 0 104 104" style={{ flexShrink: 0 }}>
        <title>{total.toLocaleString()}</title>
        <circle cx={cx} cy={cy} r={r} fill="none" stroke="var(--volo-line)" stroke-width="15" />
        {segments.map((it, idx) => {
          const val = Number(it.count || 0);
          const frac = val / total;
          const segLen = Math.max(0, frac * circ);
          const curOffset = offsetAcc;
          offsetAcc += segLen;
          const color = DONUT_PALETTE[idx % DONUT_PALETTE.length];
          const pct = Math.round(frac * 100);
          const dispName = `${it.isOthers ? "" : prefix}${it.name}`;
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
              <title>{`${dispName}: ${val.toLocaleString()} (${pct}%)`}</title>
            </circle>
          );
        })}
        <text
          x={cx}
          y={cy - 1}
          text-anchor="middle"
          font-size={total >= 10000 ? "13.5" : "15"}
          font-weight="700"
          fill="var(--volo-text)"
        >
          {formatCompactNum(total)}
        </text>
        <text x={cx} y={cy + 14} text-anchor="middle" font-size="11" fill="var(--volo-muted)">
          {t("ov.chart_total")}
        </text>
      </svg>

      <div class="volo-donut-legend">
        {segments.map((it, idx) => {
          const val = Number(it.count || 0);
          const pct = Math.round((val / total) * 100);
          const color = DONUT_PALETTE[idx % DONUT_PALETTE.length];
          const dispName = `${it.isOthers ? "" : prefix}${it.name}`;
          return (
            <div key={`${it.name}-${idx}`} class="volo-donut-legend-row">
              <div style={{ display: "flex", alignItems: "center", gap: "7px", minWidth: 0 }}>
                <span class="volo-donut-dot" style={{ background: color }} />
                <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={dispName}>
                  {dispName}
                </span>
              </div>
              <span class="volo-help" style={{ fontFamily: "var(--volo-mono)", whiteSpace: "nowrap", flexShrink: 0 }} title={val.toLocaleString()}>
                {formatCompactNum(val)} ({pct}%)
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
  const [monthlyRangeMode, setMonthlyRangeMode] = useState<"auto" | "12m" | "history">("auto");
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const [isNarrowChart, setIsNarrowChart] = useState<boolean>(() =>
    typeof window !== "undefined" ? window.matchMedia("(max-width: 640px)").matches : false
  );
  const chartStageRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mql = window.matchMedia("(max-width: 640px)");
    const onChange = (ev: MediaQueryListEvent) => setIsNarrowChart(ev.matches);
    setIsNarrowChart(mql.matches);
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, []);
  const [articles, setArticles] = useState<any[]>([]);
  const [artTotal, setArtTotal] = useState<number>(0);
  const [artPageSize, setArtPageSize] = useState<number>(20);
  const [statusCounts, setStatusCounts] = useState<{
    all?: number;
    published?: number;
    scheduled?: number;
    draft?: number;
  }>({});
  const [artLoading, setArtLoading] = useState<boolean>(false);
  const [importingZip, setImportingZip] = useState<boolean>(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [batchCatId, setBatchCatId] = useState<string>("");

  const [categories, setCategories] = useState<any[]>([]);
  const [seriesList, setSeriesList] = useState<any[]>([]);
  const [allTags, setAllTags] = useState<any[]>([]);
  const [totalTagCount, setTotalTagCount] = useState<number>(0);
  const [statusFilter, setStatusFilter] = useState<string>(section === "drafts" ? "draft" : "all");
  const [catFilter, setCatFilter] = useState<string>("");
  const [tagFilter, setTagFilter] = useState<string>("");
  const [visFilter, setVisFilter] = useState<string>("");
  const [artSort, setArtSort] = useState<string>("updated");
  const [query, setQuery] = useState<string>("");
  const debouncedQuery = useDebouncedValue(query, 250);
  const [artPage, setArtPage] = useState<number>(1);
  const artReqSeqRef = useRef<number>(0);
  const [collapsedPanels, setCollapsedPanels] = useState<Record<string, boolean>>({});
  const [expandedDiffFolds, setExpandedDiffFolds] = useState<Record<string, boolean>>({});

  // Studio Editor state (`null` = not editing, `""` = new post, `"id"` = editing post)
  const [editingID, setEditingID] = useState<string | null>(null);
  const [title, setTitle] = useState("");
  const [slug, setSlug] = useState("");
  const [categoryID, setCategoryID] = useState("");
  const [editingCategoryName, setEditingCategoryName] = useState("");
  const [tagsText, setTagsText] = useState("");
  const [tagDraft, setTagDraft] = useState("");
  const [tagPoolOpen, setTagPoolOpen] = useState(false);
  const [tagPoolQuery, setTagPoolQuery] = useState("");
  const [tagPoolSort, setTagPoolSort] = useState<"count" | "name">("count");
  const [remoteTags, setRemoteTags] = useState<any[]>([]);
  const [tagSuggestOpen, setTagSuggestOpen] = useState(false);
  const [tagSuggestIdx, setTagSuggestIdx] = useState(0);
  const [tagSuggestStyle, setTagSuggestStyle] = useState<CSSProperties>({});
  const tagBoxRef = useRef<HTMLDivElement | null>(null);
  const tagInputRef = useRef<HTMLInputElement | null>(null);
  const tagSuggestRef = useRef<HTMLDivElement | null>(null);
  const debouncedTagDraft = useDebouncedValue(tagDraft, 180);
  const debouncedTagPoolQ = useDebouncedValue(tagPoolQuery, 200);

  // Quick-Create Category / Series modal inside Article Editor
  const [quickCreateKind, setQuickCreateKind] = useState<"category" | "series" | null>(null);
  const [quickCreateName, setQuickCreateName] = useState("");
  const [quickCreateSlug, setQuickCreateSlug] = useState("");
  const [quickCreateSaving, setQuickCreateSaving] = useState(false);

  const [seriesID, setSeriesID] = useState("");
  const [summaryMD, setSummaryMD] = useState("");
  const [contentMD, setContentMD] = useState("");
  const [coverURL, setCoverURL] = useState("");
  const [status, setStatus] = useState("published");
  const [publishedAtStr, setPublishedAtStr] = useState("");
  const [scheduledAtStr, setScheduledAtStr] = useState("");
  const [isPinned, setIsPinned] = useState(false);
  const [allowComment, setAllowComment] = useState(true);
  const [visibilityMode, setVisibilityMode] = useState<"public" | "password">("public");
  const [hasSavedPassword, setHasSavedPassword] = useState(false);
  const [password, setPassword] = useState("");
  const [clearPassword, setClearPassword] = useState(false);
  const [audioURL, setAudioURL] = useState("");
  const [audioDuration, setAudioDuration] = useState(0);
  const [audioMime, setAudioMime] = useState("audio/mpeg");
  const [lang, setLang] = useState("zh-CN");
  const [translationGroup, setTranslationGroup] = useState("");
  const [revisionNote, setRevisionNote] = useState("");
  const [previewURL, setPreviewURL] = useState("");
  const [inlinePreviewHTML, setInlinePreviewHTML] = useState<string | null>(null);

  const [pendingDraft, setPendingDraft] = useState<LocalDraft | null>(null);
  const [inlineRevs, setInlineRevs] = useState<any[]>([]);
  const [revisions, setRevisions] = useState<any[] | null>(null);
  const [diffRevID, setDiffRevID] = useState<string | null>(null);
  const [shareTTL, setShareTTL] = useState<string>("72");
  const [shareURL, setShareURL] = useState<string>("");

  const editingIDRef = useRef<string | null>(null);
  const sectionRef = useRef<OverviewSection>(section);
  editingIDRef.current = editingID;
  sectionRef.current = section;

  const draftKey = editingID === "" ? "article:new" : editingID ? `article:${editingID}` : "";

  const startNewArticle = async (catsOverride?: any[]) => {
    const catList = catsOverride || categories;
    dirtyEditorSig.value = false;
    editingIDRef.current = "";
    setEditingID("");
    setTitle("");
    setSlug("");
    setCategoryID(catList[0]?.id || "");
    setEditingCategoryName(catList[0]?.name || "");
    setTagsText("");
    setTagDraft("");
    setTagPoolOpen(false);
    setTagPoolQuery("");
    setTagSuggestOpen(false);
    setSeriesID("");
    setSummaryMD("");
    setContentMD("");
    setCoverURL("");
    setStatus("published");
    setPublishedAtStr("");
    setScheduledAtStr("");
    setIsPinned(false);
    setAllowComment(true);
    setVisibilityMode("public");
    setHasSavedPassword(false);
    setPassword("");
    setClearPassword(false);
    setAudioURL("");
    setAudioDuration(0);
    setLang(localeSig.value);
    setTranslationGroup("");
    setRevisionNote("");
    setPreviewURL("");
    setShareURL("");
    setInlineRevs([]);
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

  const loadTaxonomies = async () => {
    const [cats, sers, tgs] = await Promise.all([
      apiFetch<{ items: any[]; total?: number }>("/api/admin/categories?per_page=200").catch(() => ({ items: [] })),
      apiFetch<{ items: any[]; total?: number }>("/api/admin/series?per_page=200").catch(() => ({ items: [] })),
      apiFetch<{ items: any[]; total?: number }>("/api/admin/tags?sort=count&per_page=200").catch(() => ({
        items: [] as any[],
        total: undefined as number | undefined,
      })),
    ]);
    const catItems = cats.items || [];
    const serItems = sers.items || [];
    const tagItems = tgs.items || [];
    setCategories(catItems);
    setSeriesList(serItems);
    setAllTags(tagItems);
    setTotalTagCount(typeof tgs.total === "number" ? tgs.total : tagItems.length);
    return catItems;
  };

  const searchRemoteCategories = async (q: string) => {
    const trimmed = q.trim();
    if (!trimmed) return;
    const res = await apiFetch<{ items?: any[] }>(
      `/api/admin/categories?q=${encodeURIComponent(trimmed)}&per_page=50`
    ).catch(() => ({ items: [] as any[] }));
    const incoming = res.items || [];
    if (incoming.length === 0) return;
    setCategories((prev) => {
      const seen = new Set(prev.map((c: any) => c.id));
      const merged = [...prev];
      for (const c of incoming) {
        if (c?.id && !seen.has(c.id)) {
          seen.add(c.id);
          merged.push(c);
        }
      }
      return merged;
    });
  };

  const searchRemoteTags = async (q: string) => {
    const trimmed = q.trim();
    if (!trimmed) return;
    const res = await apiFetch<{ items?: any[]; tags?: any[] }>(
      `/api/admin/tags?q=${encodeURIComponent(trimmed)}&sort=count&per_page=50`
    ).catch(() => ({ items: [] as any[], tags: [] as any[] }));
    const incoming = res.items || res.tags || [];
    if (incoming.length === 0) return;
    setAllTags((prev) => {
      const seen = new Set(prev.map((tg: any) => tg.id));
      const merged = [...prev];
      for (const tg of incoming) {
        if (tg?.id && !seen.has(tg.id)) {
          seen.add(tg.id);
          merged.push(tg);
        }
      }
      return merged;
    });
  };

  const loadArticlesPage = async (opts?: {
    page?: number;
    perPage?: number;
    status?: string;
    cat?: string;
    tag?: string;
    vis?: string;
    sort?: string;
    q?: string;
  }) => {
    const reqId = ++artReqSeqRef.current;
    const effPage = opts?.page ?? artPage;
    const effPerPage = opts?.perPage ?? artPageSize;
    const effStatus = opts?.status ?? statusFilter;
    const effCat = opts?.cat ?? catFilter;
    const effTag = opts?.tag ?? tagFilter;
    const effVis = opts?.vis ?? visFilter;
    const effSort = opts?.sort ?? artSort;
    const effQ = opts?.q ?? debouncedQuery;

    setArtLoading(true);
    try {
      const qs = new URLSearchParams();
      qs.set("page", String(effPage));
      qs.set("per_page", String(effPerPage));
      if (effStatus && effStatus !== "all") qs.set("status", effStatus);
      else qs.set("status", "all");
      if (effCat) qs.set("category_id", effCat);
      if (effTag) qs.set("tag_id", effTag);
      if (effVis) qs.set("visibility", effVis);
      if (effSort) qs.set("sort", effSort);
      if (effQ.trim()) qs.set("q", effQ.trim());

      const arts = await apiFetch<{
        items: any[];
        total?: number;
        status_counts?: { all?: number; published?: number; scheduled?: number; draft?: number };
      }>(`/api/admin/articles?${qs.toString()}`).catch(() => ({ items: [] as any[], total: 0, status_counts: undefined }));

      if (reqId !== artReqSeqRef.current) return;
      const nextItems = arts.items || [];
      setArticles(nextItems);
      setArtTotal(typeof arts.total === "number" ? arts.total : nextItems.length);
      if (arts.status_counts) {
        setStatusCounts(arts.status_counts);
      }
      const validIds = new Set(nextItems.map((r: any) => r.article?.id));
      setSelectedIds((prev) => prev.filter((id) => validIds.has(id)));
    } finally {
      if (reqId === artReqSeqRef.current) setArtLoading(false);
    }
  };

  const loadArticlesAndTaxonomies = async (effStatus = statusFilter, effCat = catFilter, effQuery = debouncedQuery) => {
    const [cats] = await Promise.all([
      loadTaxonomies(),
      loadArticlesPage({ status: effStatus, cat: effCat, q: effQuery }),
    ]);
    return cats;
  };

  const openEditArticle = async (id: string) => {
    editingIDRef.current = id;
    try {
      const data = await apiFetch<any>(`/api/admin/articles/${encodeURIComponent(id)}`);
      const a = data.article;
      const locked = Boolean(data.password || data.is_password_protected);
      dirtyEditorSig.value = false;
      editingIDRef.current = a.id;
      setEditingID(a.id);
      setTitle(a.title || "");
      setSlug(a.slug || "");
      setCategoryID(a.category_id || "");
      setEditingCategoryName(String((a as any)?.category_name || (data as any)?.category_name || ""));
      setTagsText((data.tags || []).join(", "));
      setTagDraft("");
      setTagPoolOpen(false);
      setTagPoolQuery("");
      setTagSuggestOpen(false);
      setSeriesID(data.series_id || "");
      setSummaryMD(a.summary_md || "");
      setContentMD(a.content_md || "");
      setCoverURL(a.cover_url || "");
      setStatus(a.status || "published");
      setPublishedAtStr(msToLocalDatetimeInput(a.published_at));
      setScheduledAtStr(msToLocalDatetimeInput(a.scheduled_at));
      setIsPinned(!!a.is_pinned);
      setAllowComment(a.allow_comment !== false);
      setVisibilityMode(locked ? "password" : "public");
      setHasSavedPassword(locked);
      setPassword(data.password || "");
      setClearPassword(false);
      setAudioURL(a.audio_url || "");
      setAudioDuration(a.audio_duration_sec || 0);
      setAudioMime(a.audio_mime || "audio/mpeg");
      setLang(a.lang || "zh-CN");
      setTranslationGroup(a.translation_group || "");
      setRevisionNote("");
      setPreviewURL(data.preview_url || a.preview_url || "");
      setShareURL("");
      resetConsoleScroll();
      apiFetch<{ items: any[] }>(`/api/admin/articles/${encodeURIComponent(a.id)}/revisions`)
        .then((r) => setInlineRevs(r.items || []))
        .catch(() => setInlineRevs([]));
      const saved = await loadLocalDraft(`article:${id}`);
      if (saved && saved.content_md && saved.content_md !== a.content_md) {
        setPendingDraft(saved);
      } else {
        setPendingDraft(null);
      }
    } catch (err: any) {
      editingIDRef.current = null;
      showToast(err.message, "err");
    }
  };

  // Synchronize sidebar section selection & CommandPalette jump
  useEffect(() => {
    setArtPage(1);
    setSelectedIds([]);
    const jumpID = sessionStorage.getItem("volo_jump_article_id");
    if (jumpID) {
      sessionStorage.removeItem("volo_jump_article_id");
      void loadTaxonomies();
      void loadOverview(trendDays);
      void openEditArticle(jumpID);
      return;
    }
    if (section === "editor") {
      void loadTaxonomies().then((cats) => {
        if (editingIDRef.current === null) {
          void startNewArticle(cats);
        }
      });
      void loadOverview(trendDays);
      return;
    }
    dirtyEditorSig.value = false;
    editingIDRef.current = null;
    setEditingID(null);
    void loadTaxonomies();
    if (section === "drafts") {
      if (statusFilter !== "draft") {
        setStatusFilter("draft");
      } else {
        void loadArticlesPage({ page: 1, status: "draft" });
      }
    } else if (section === "articles") {
      const nextSt = statusFilter === "draft" ? "all" : statusFilter;
      if (nextSt !== statusFilter) {
        setStatusFilter(nextSt);
      } else {
        void loadArticlesPage({ page: 1, status: nextSt });
      }
    } else {
      void loadOverview(trendDays);
    }
  }, [section]);

  useEffect(() => {
    if (section === "dashboard") {
      void loadOverview(trendDays);
    }
  }, [curLocale]);

  useEffect(() => {
    setHoverIdx(null);
    if (section === "dashboard") {
      void loadOverview(trendDays);
    }
  }, [trendDays]);

  useEffect(() => {
    if (hoverIdx === null) return;
    const onDocTouch = (e: TouchEvent) => {
      const target = e.target as Node | null;
      if (target && chartStageRef.current?.contains(target)) return;
      setHoverIdx(null);
    };
    document.addEventListener("touchstart", onDocTouch, { passive: true });
    return () => document.removeEventListener("touchstart", onDocTouch);
  }, [hoverIdx]);

  useEffect(() => {
    if (section !== "articles" && section !== "drafts") return;
    setSelectedIds([]);
    void loadArticlesPage({
      page: artPage,
      perPage: artPageSize,
      status: statusFilter,
      cat: catFilter,
      tag: tagFilter,
      vis: visFilter,
      sort: artSort,
      q: debouncedQuery,
    });
  }, [artPage, artPageSize, statusFilter, catFilter, tagFilter, visFilter, artSort, debouncedQuery]);

  // Dynamic tag search when typing in tag picker or tag pool filter
  useEffect(() => {
    const activeQ = (debouncedTagDraft.trim() || debouncedTagPoolQ.trim()).trim();
    if (!activeQ || (editingID === null && section !== "editor")) {
      setRemoteTags([]);
      return;
    }
    let cancelled = false;
    apiFetch<{ items?: any[]; tags?: any[] }>(`/api/admin/tags?q=${encodeURIComponent(activeQ)}&sort=count&per_page=50`)
      .then((res) => {
        if (cancelled) return;
        const list = res.items || res.tags || [];
        setRemoteTags(list);
        if (list.length > 0) {
          setAllTags((prev) => {
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
        }
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [debouncedTagDraft, debouncedTagPoolQ, editingID, section]);

  // Reposition or close tag typeahead portal menu on scroll/resize/outside click
  useEffect(() => {
    if (!tagSuggestOpen) return;
    const updatePos = () => {
      if (!tagBoxRef.current) return;
      const rect = tagBoxRef.current.getBoundingClientRect();
      const vv = window.visualViewport;
      const vvTop = vv?.offsetTop ?? 0;
      const vpH = vv?.height ?? window.innerHeight;
      if (rect.bottom < vvTop - 40 || rect.top > vvTop + vpH + 40) {
        setTagSuggestOpen(false);
        return;
      }
      const estH = tagSuggestRef.current?.offsetHeight || 196;
      const pos = computePortalMenuStyle(rect, { minW: 220, maxW: 360, maxH: 260, estH, align: "left" });
      setTagSuggestStyle(pos.style);
    };
    updatePos();
    const onDocDown = (ev: MouseEvent) => {
      const target = ev.target as Node;
      if (tagBoxRef.current?.contains(target) || tagSuggestRef.current?.contains(target)) return;
      setTagSuggestOpen(false);
    };
    const vv = window.visualViewport;
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    vv?.addEventListener("resize", updatePos);
    vv?.addEventListener("scroll", updatePos);
    document.addEventListener("mousedown", onDocDown);
    return () => {
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
      vv?.removeEventListener("resize", updatePos);
      vv?.removeEventListener("scroll", updatePos);
      document.removeEventListener("mousedown", onDocDown);
    };
  }, [tagSuggestOpen, tagDraft]);

  // Auto-persist local draft to IndexedDB every 1.2s while editing (never overwrite unreviewed pendingDraft)
  useEffect(() => {
    if (editingID === null || !draftKey || pendingDraft !== null) return;
    const timer = window.setTimeout(() => {
      if (title || contentMD) {
        void saveLocalDraft(draftKey, {
          title,
          slug,
          summary_md: summaryMD,
          content_md: contentMD,
          category_id: categoryID,
          tags_text: tagsText,
          series_id: seriesID,
          cover_url: coverURL,
          visibility: visibilityMode,
          status,
          allow_comment: allowComment,
          lang,
          translation_group: translationGroup,
          saved_at: Date.now(),
        });
      }
    }, 1200);
    return () => window.clearTimeout(timer);
  }, [
    editingID,
    draftKey,
    pendingDraft,
    title,
    slug,
    summaryMD,
    contentMD,
    categoryID,
    tagsText,
    seriesID,
    coverURL,
    visibilityMode,
    status,
    allowComment,
    lang,
    translationGroup,
  ]);

  const closeEditor = async () => {
    if (dirtyEditorSig.value) {
      const ok = await openConfirm({
        title: t("ui.unsaved_title"),
        message: t("ui.unsaved_msg"),
        confirmText: t("ui.unsaved_leave"),
        danger: true,
      });
      if (!ok) return;
      dirtyEditorSig.value = false;
    }
    editingIDRef.current = null;
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
    dirtyEditorSig.value = true;
  };

  const removeTagToken = (tagName: string) => {
    const filtered = selectedTags.filter((x) => x.toLowerCase() !== tagName.toLowerCase());
    setTagsText(filtered.join(", "));
    dirtyEditorSig.value = true;
  };

  const toggleTagChip = (tagName: string) => {
    if (selectedTags.some((x) => x.toLowerCase() === tagName.toLowerCase())) {
      removeTagToken(tagName);
    } else {
      addTagTokens(tagName);
    }
  };

  const handleSave = async (overrideStatus?: string, stayOpen = false) => {
    if (!title.trim()) {
      showToast(t("art.title_ph"), "warn");
      return;
    }
    const nextStatus = overrideStatus || status;
    try {
      const finalTags = parseTagList(tagDraft.trim() ? `${tagsText}, ${tagDraft}` : tagsText);
      const shouldClearPwd = visibilityMode === "public" ? clearPassword || hasSavedPassword : false;
      const payload: any = {
        category_id: categoryID,
        title: title.trim(),
        slug,
        summary_md: summaryMD,
        content_md: contentMD,
        cover_url: coverURL,
        status: nextStatus,
        published_at: localDatetimeInputToMs(publishedAtStr),
        scheduled_at: nextStatus === "scheduled" ? localDatetimeInputToMs(scheduledAtStr) : 0,
        is_pinned: isPinned,
        allow_comment: allowComment,
        clear_password: shouldClearPwd,
        audio_url: audioURL,
        audio_duration_sec: Number(audioDuration) || 0,
        audio_mime: audioMime,
        lang,
        translation_group: translationGroup,
        tags: finalTags,
        series_id: seriesID,
        revision_note: revisionNote,
      };
      if (visibilityMode === "password" && password.trim() !== "") {
        payload.password = password.trim();
      } else if (visibilityMode === "public" && !shouldClearPwd) {
        payload.password = "";
      }

      const isCreate = editingID === "" || editingID === null;
      const res = isCreate
        ? await apiFetch<any>("/api/admin/articles", { method: "POST", body: JSON.stringify(payload) })
        : await apiFetch<any>(`/api/admin/articles/${encodeURIComponent(editingID)}`, {
            method: "PUT",
            body: JSON.stringify(payload),
          });

      if (draftKey) await clearLocalDraft(draftKey);
      dirtyEditorSig.value = false;
      if (overrideStatus) setStatus(overrideStatus);
      if (tagDraft.trim()) {
        setTagsText(finalTags.join(", "));
        setTagDraft("");
      }
      setRevisionNote("");
      showToast(t("common.saved"), "ok");

      if (stayOpen) {
        const savedArt = res?.article;
        if (savedArt?.id) {
          editingIDRef.current = savedArt.id;
          setEditingID(savedArt.id);
          if (savedArt.slug) setSlug(savedArt.slug);
          setHasSavedPassword(Boolean(res?.is_password_protected));
          setClearPassword(false);
          apiFetch<{ items: any[] }>(`/api/admin/articles/${encodeURIComponent(savedArt.id)}/revisions`)
            .then((rv) => setInlineRevs(rv.items || []))
            .catch(() => {});
        }
        void loadArticlesAndTaxonomies();
        void loadOverview(trendDays);
        return;
      }

      editingIDRef.current = null;
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
      showToast(err.message, "err");
    }
  };

  const saveRef = useRef(handleSave);
  const openEditRef = useRef(openEditArticle);
  saveRef.current = handleSave;
  openEditRef.current = openEditArticle;

  useEffect(() => {
    const onGlobalSave = () => {
      if (editingIDRef.current !== null || sectionRef.current === "editor") {
        void saveRef.current(undefined, true);
      }
    };
    const onJumpEdit = (ev: Event) => {
      const id = (ev as CustomEvent).detail || sessionStorage.getItem("volo_jump_article_id");
      if (id) {
        sessionStorage.removeItem("volo_jump_article_id");
        void openEditRef.current(String(id));
      }
    };
    window.addEventListener("volo:save", onGlobalSave);
    window.addEventListener("volo:edit-article", onJumpEdit);
    return () => {
      window.removeEventListener("volo:save", onGlobalSave);
      window.removeEventListener("volo:edit-article", onJumpEdit);
      dirtyEditorSig.value = false;
    };
  }, []);

  const handlePreview = async () => {
    const directUrl = shareURL || previewURL;
    if (directUrl) {
      window.open(directUrl, "_blank", "noopener");
      return;
    }
    if (editingID && slug.trim()) {
      window.open(`/posts/${encodeURIComponent(slug.trim())}?preview=1`, "_blank", "noopener");
      return;
    }
    if (!contentMD.trim()) {
      showToast(t("ov.preview_save_first"), "warn");
      return;
    }
    try {
      const res = await apiFetch<{ html: string }>("/api/admin/articles/preview", {
        method: "POST",
        body: JSON.stringify({ content_md: contentMD, locale: lang }),
      });
      setInlinePreviewHTML(res.html || "");
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const togglePinArticle = async (id: string) => {
    try {
      await apiFetch(`/api/admin/articles/${encodeURIComponent(id)}/pin`, { method: "POST", body: "{}" });
      showToast(t("common.saved"), "ok");
      void loadArticlesAndTaxonomies();
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const deleteArticle = async (id: string) => {
    if (!(await openConfirm({ message: t("ov.confirm_del_post"), danger: true }))) return;
    try {
      await apiFetch(`/api/admin/articles/${encodeURIComponent(id)}`, { method: "DELETE" });
      showToast(t("common.deleted"), "ok");
      setSelectedIds((prev) => prev.filter((x) => x !== id));
      void loadArticlesAndTaxonomies();
      void loadOverview(trendDays);
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const runBatchAction = async (
    action: "publish" | "draft" | "pin" | "unpin" | "category" | "delete",
    targetCatId?: string
  ) => {
    if (selectedIds.length === 0) return;
    if (action === "delete") {
      const selectedTitles = articles
        .filter((r) => selectedIds.includes(r.article?.id))
        .map((r) => r.article?.title || r.article?.slug || r.article?.id);
      const ok = await openConfirm({
        message: t("ov.confirm_batch_del", { n: selectedIds.length }),
        danger: true,
        items: selectedTitles,
      });
      if (!ok) return;
    }
    try {
      const body: Record<string, any> = { ids: selectedIds, action };
      if (action === "category") body.category_id = targetCatId ?? "";
      await apiFetch("/api/admin/articles/batch", {
        method: "POST",
        body: JSON.stringify(body),
      });
      showToast(t("ov.batch_done", { n: selectedIds.length }), "ok");
      setSelectedIds([]);
      setBatchCatId("");
      void loadArticlesAndTaxonomies();
      void loadOverview(trendDays);
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const handleBatchImport = async (file: File) => {
    setImportingZip(true);
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{ imported_count: number }>("/api/admin/articles/import", {
        method: "POST",
        body: fd,
      });
      showToast(t("art.imported_toast", { count: res.imported_count }), "ok");
      void loadArticlesAndTaxonomies();
      void loadOverview(trendDays);
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setImportingZip(false);
    }
  };

  const openRevisions = async (initialDiffId: string | null = null) => {
    if (!editingID) return;
    try {
      const res = await apiFetch<{ items: any[] }>(`/api/admin/articles/${encodeURIComponent(editingID)}/revisions`);
      const items = res.items || [];
      setInlineRevs(items);
      setDiffRevID(initialDiffId);
      setRevisions(items);
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const restoreRevision = async (revID: string) => {
    if (!editingID) return;
    if (!(await openConfirm({ message: t("ov.confirm_rollback"), danger: true }))) return;
    try {
      const res = await apiFetch<{ article: any }>(
        `/api/admin/articles/${encodeURIComponent(editingID)}/revisions/${encodeURIComponent(revID)}/restore`,
        { method: "POST", body: "{}" }
      );
      setTitle(res.article.title || "");
      setSummaryMD(res.article.summary_md || "");
      setContentMD(res.article.content_md || "");
      dirtyEditorSig.value = false;
      setRevisions(null);
      showToast(t("art.rev_restored"), "ok");
      apiFetch<{ items: any[] }>(`/api/admin/articles/${encodeURIComponent(editingID)}/revisions`)
        .then((rv) => setInlineRevs(rv.items || []))
        .catch(() => {});
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const issueShareLink = async () => {
    if (!editingID) return;
    try {
      const res = await apiFetch<{ share_url: string }>(
        `/api/admin/articles/${encodeURIComponent(editingID)}/share-token`,
        {
          method: "POST",
          body: JSON.stringify({ ttl_hours: Number(shareTTL) || 72 }),
        }
      );
      setShareURL(res.share_url);
      showToast(t("art.share_generated"), "ok");
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const submitQuickCreate = async (e?: Event) => {
    e?.preventDefault();
    const nm = quickCreateName.trim();
    if (!nm || !quickCreateKind || quickCreateSaving) return;
    setQuickCreateSaving(true);
    try {
      if (quickCreateKind === "category") {
        const maxSort = categories.reduce((mx, c) => Math.max(mx, Number(c.sort_order || 0)), 0);
        const res = await apiFetch<any>("/api/admin/categories", {
          method: "POST",
          body: JSON.stringify({
            name: nm,
            slug: quickCreateSlug.trim(),
            sort_order: maxSort + 10,
            description: "",
          }),
        });
        const refreshed = await loadTaxonomies();
        const createdId =
          res?.category?.id ||
          res?.id ||
          refreshed.find((c: any) => String(c.name || "").toLowerCase() === nm.toLowerCase())?.id ||
          "";
        if (createdId) setCategoryID(createdId);
      } else {
        const maxSort = seriesList.reduce((mx, s) => Math.max(mx, Number(s.sort_order || 0)), 0);
        const res = await apiFetch<any>("/api/admin/series", {
          method: "POST",
          body: JSON.stringify({
            title: nm,
            slug: quickCreateSlug.trim(),
            sort_order: maxSort + 10,
            cover_url: "",
            description: "",
            article_ids: [],
          }),
        });
        await loadTaxonomies();
        const serRes = await apiFetch<{ items: any[] }>("/api/admin/series?per_page=200").catch(() => ({ items: [] }));
        const sList = serRes.items || [];
        setSeriesList(sList);
        const createdId =
          res?.series?.id ||
          res?.id ||
          sList.find((s: any) => String(s.title || "").toLowerCase() === nm.toLowerCase())?.id ||
          "";
        if (createdId) setSeriesID(createdId);
      }
      showToast(t("common.saved"), "ok");
      setQuickCreateKind(null);
      setQuickCreateName("");
      setQuickCreateSlug("");
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setQuickCreateSaving(false);
    }
  };

  const catNameMap: Record<string, string> = {};
  categories.forEach((c) => {
    catNameMap[c.id] = c.name;
  });

  // 1. Full-Width Single-Column Studio Editor (`section === "editor"` or editing an existing post)
  if (editingID !== null || section === "editor") {
    const quickInserts: QuickInsertItem[] = overview?.quick_inserts || [];
    const activeTagsLower = new Set(selectedTags.map((s) => s.toLowerCase()));

    // Merge initial top tags + dynamic search tags without duplicates
    const mergedTagMap = new Map<string, any>();
    [...allTags, ...remoteTags].forEach((tg) => {
      const key = String(tg?.name || "").trim().toLowerCase();
      if (key && !mergedTagMap.has(key)) {
        mergedTagMap.set(key, tg);
      }
    });
    const mergedTags = Array.from(mergedTagMap.values());
    const getTagCount = (tg: any) => Number(tg?.article_count ?? tg?.count ?? 0);

    // Typeahead suggestions (up to 10 matches sorted by prefix then count DESC + optional Create #tag row)
    const draftTrim = tagDraft.trim().replace(/^#+/, "");
    const draftLower = draftTrim.toLowerCase();
    const typeaheadMatches = draftLower
      ? mergedTags
          .filter((tg) => {
            const nm = String(tg.name || "");
            const low = nm.toLowerCase();
            return !activeTagsLower.has(low) && low.includes(draftLower);
          })
          .sort((a, b) => {
            const aLow = String(a.name || "").toLowerCase();
            const bLow = String(b.name || "").toLowerCase();
            const aPref = aLow.startsWith(draftLower) ? 0 : 1;
            const bPref = bLow.startsWith(draftLower) ? 0 : 1;
            if (aPref !== bPref) return aPref - bPref;
            const cDiff = getTagCount(b) - getTagCount(a);
            if (cDiff !== 0) return cDiff;
            return aLow.localeCompare(bLow);
          })
          .slice(0, 10)
      : [];

    const hasExactTag =
      !draftLower ||
      activeTagsLower.has(draftLower) ||
      mergedTags.some((tg) => String(tg.name || "").toLowerCase() === draftLower);

    const suggestOptions: Array<{ name: string; count: number; isCreate?: boolean }> = [
      ...typeaheadMatches.map((tg) => ({ name: String(tg.name), count: getTagCount(tg) })),
      ...(draftTrim && !hasExactTag ? [{ name: draftTrim, count: 0, isCreate: true }] : []),
    ];
    const safeSuggestIdx =
      suggestOptions.length > 0 ? Math.max(0, Math.min(tagSuggestIdx, suggestOptions.length - 1)) : 0;

    // Bounded Tag Pool (top 80 matches sorted by Popular or A-Z)
    const poolQueryLower = tagPoolQuery.trim().toLowerCase();
    const visiblePoolTags = mergedTags
      .filter((tg) => !poolQueryLower || String(tg.name || "").toLowerCase().includes(poolQueryLower))
      .sort((a, b) => {
        if (tagPoolSort === "name") {
          return String(a.name || "").localeCompare(String(b.name || ""));
        }
        const cDiff = getTagCount(b) - getTagCount(a);
        if (cDiff !== 0) return cDiff;
        return String(a.name || "").localeCompare(String(b.name || ""));
      })
      .slice(0, 80);

    return (
      <div>
        <div class="volo-card">
          <div class="volo-card-head">
            <h2 class="volo-card-title">
              {!editingID ? t("art.new") : `${t("common.edit")}: ${title || t("art.title")}`}
            </h2>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void handlePreview()}>
                {t("common.preview")} ↗
              </button>
              {status === "draft" ? (
                <>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => void handleSave("published", false)}
                  >
                    {t("ov.publish_btn")}
                  </button>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm volo-btn-primary"
                    onClick={() => void handleSave("draft", true)}
                  >
                    {t("ov.save_draft")}
                  </button>
                </>
              ) : (
                <>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => void handleSave("draft", true)}
                  >
                    {t("ov.save_draft")}
                  </button>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm volo-btn-primary"
                    onClick={() => void handleSave(status === "scheduled" ? "scheduled" : "published", false)}
                  >
                    {status === "scheduled" ? t("art.scheduled") : t("ov.publish_btn")}
                  </button>
                </>
              )}
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void closeEditor()}>
                {t("common.cancel")}
              </button>
            </div>
          </div>

          {pendingDraft && (
            <div class="volo-subcard" style={{ background: "var(--volo-accent-soft)" }}>
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
                      if (pendingDraft.category_id !== undefined) setCategoryID(pendingDraft.category_id);
                      if (pendingDraft.tags_text !== undefined) setTagsText(pendingDraft.tags_text);
                      if (pendingDraft.series_id !== undefined) setSeriesID(pendingDraft.series_id);
                      if (pendingDraft.cover_url !== undefined) setCoverURL(pendingDraft.cover_url);
                      if (pendingDraft.visibility === "password" || pendingDraft.visibility === "public") {
                        setVisibilityMode(pendingDraft.visibility);
                      }
                      if (pendingDraft.status) setStatus(pendingDraft.status);
                      if (typeof pendingDraft.allow_comment === "boolean") setAllowComment(pendingDraft.allow_comment);
                      if (pendingDraft.lang) setLang(pendingDraft.lang);
                      if (pendingDraft.translation_group !== undefined) {
                        setTranslationGroup(pendingDraft.translation_group);
                      }
                      dirtyEditorSig.value = true;
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
            <div class="volo-subcard" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: "12px", flexWrap: "wrap" }}>
              <div style={{ wordBreak: "break-all", fontSize: "12.5px" }}>
                <strong>{t("art.share_label")}</strong>{" "}
                <a href={shareURL} target="_blank" rel="noreferrer">{shareURL}</a>
              </div>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => {
                  void navigator.clipboard?.writeText(shareURL);
                  showToast(t("art.share_copied"), "ok");
                }}
              >
                {t("art.share_copy")}
              </button>
            </div>
          )}

          <form
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault();
              void handleSave(undefined, false);
            }}
          >
            <div class="volo-field">
              <input
                class="volo-editor-title-input"
                required
                aria-label={t("art.title")}
                placeholder={t("art.title_ph")}
                value={title}
                onInput={(e) => {
                  setTitle((e.currentTarget as HTMLInputElement).value);
                  dirtyEditorSig.value = true;
                }}
              />
            </div>

            <div class="volo-field">
              <label>{t("art.md_body")}</label>
              <CM6Editor
                value={contentMD}
                onChange={(next) => {
                  setContentMD(next);
                  dirtyEditorSig.value = true;
                }}
                quickInserts={quickInserts}
                onSave={() => void handleSave(undefined, true)}
              />
            </div>

            <div class="volo-editor-panels">
              {/* 1. Publishing & Visibility */}
              <div class={`volo-inspector-box ${collapsedPanels.publish ? "is-collapsed" : ""}`}>
                <h4 class="volo-inspector-title">
                  <button
                    type="button"
                    class="volo-inspector-toggle"
                    aria-expanded={!collapsedPanels.publish}
                    onClick={() => setCollapsedPanels((p) => ({ ...p, publish: !p.publish }))}
                  >
                    <span>{t("ov.panel_publish")}</span>
                    <span class="volo-inspector-chev">▾</span>
                  </button>
                </h4>
                {!collapsedPanels.publish && (
                  <>
                    <div class="volo-auto-grid">
                      <div class="volo-field">
                        <label>{t("art.status")}</label>
                        <ConsoleSelect
                          value={status}
                          onChange={(v) => {
                            setStatus(v);
                            dirtyEditorSig.value = true;
                          }}
                          options={[
                            { value: "published", label: t("art.published") },
                            { value: "draft", label: t("art.draft") },
                            { value: "scheduled", label: t("art.scheduled") },
                          ]}
                        />
                      </div>
                      {status === "scheduled" && (
                        <div class="volo-field">
                          <label>{t("art.scheduled_at")}</label>
                          <ConsoleDatePicker
                            mode="datetime"
                            clearable={false}
                            ariaLabel={t("art.scheduled_at")}
                            value={scheduledAtStr}
                            onChange={(val) => {
                              setScheduledAtStr(val);
                              dirtyEditorSig.value = true;
                            }}
                          />
                        </div>
                      )}
                      <div class="volo-field">
                        <label>{t("art.slug")}</label>
                        <input
                          class="volo-input"
                          placeholder={t("art.slug_placeholder")}
                          value={slug}
                          onInput={(e) => {
                            setSlug((e.currentTarget as HTMLInputElement).value);
                            dirtyEditorSig.value = true;
                          }}
                        />
                      </div>
                      <div class="volo-field">
                        <div class="volo-label-row">
                          <label>{t("art.category")}</label>
                          <button
                            type="button"
                            class="volo-quick-add-btn"
                            title={t("cnt.new_cat")}
                            aria-label={t("cnt.new_cat")}
                            onClick={() => {
                              setQuickCreateName("");
                              setQuickCreateSlug("");
                              setQuickCreateKind("category");
                            }}
                          >
                            +
                          </button>
                        </div>
                        <ConsoleSelect
                          value={categoryID}
                          fallbackLabel={editingCategoryName || undefined}
                          onSearch={searchRemoteCategories}
                          onChange={(v) => {
                            setCategoryID(v);
                            const found = categories.find((c) => c.id === v);
                            setEditingCategoryName(found ? found.name : "");
                            dirtyEditorSig.value = true;
                          }}
                          options={[
                            { value: "", label: t("art.uncategorized") },
                            ...categories.map((c) => ({ value: c.id, label: c.name })),
                          ]}
                        />
                      </div>
                      <div class="volo-field">
                        <label>{t("ov.visibility")}</label>
                        <ConsoleSelect
                          value={visibilityMode}
                          onChange={(v) => {
                            const nextVis = v === "password" ? "password" : "public";
                            setVisibilityMode(nextVis);
                            dirtyEditorSig.value = true;
                            if (nextVis === "public") {
                              setPassword("");
                              setClearPassword(hasSavedPassword);
                            } else {
                              setClearPassword(false);
                            }
                          }}
                          options={[
                            { value: "public", label: t("ov.vis_public") },
                            { value: "password", label: t("ov.vis_password") },
                          ]}
                        />
                      </div>
                      {visibilityMode === "password" && (
                        <div class="volo-field">
                          <label>{t("art.password")}</label>
                          <input
                            type="password"
                            name="article_lock_passphrase"
                            autoComplete="new-password"
                            class="volo-input"
                            placeholder={hasSavedPassword ? "••••••••" : t("art.password_placeholder")}
                            value={password}
                            onInput={(e) => {
                              setPassword((e.currentTarget as HTMLInputElement).value);
                              setClearPassword(false);
                              dirtyEditorSig.value = true;
                            }}
                          />
                        </div>
                      )}
                      <div class="volo-field">
                        <label>{t("art.published_at")}</label>
                        <ConsoleDatePicker
                          mode="datetime"
                          clearable={true}
                          ariaLabel={t("art.published_at")}
                          value={publishedAtStr}
                          onChange={(val) => {
                            setPublishedAtStr(val);
                            dirtyEditorSig.value = true;
                          }}
                        />
                      </div>
                      <div class="volo-field">
                        <div class="volo-label-row">
                          <label>{t("art.series")}</label>
                          <button
                            type="button"
                            class="volo-quick-add-btn"
                            title={t("cnt.new_series")}
                            aria-label={t("cnt.new_series")}
                            onClick={() => {
                              setQuickCreateName("");
                              setQuickCreateSlug("");
                              setQuickCreateKind("series");
                            }}
                          >
                            +
                          </button>
                        </div>
                        <ConsoleSelect
                          value={seriesID}
                          onChange={(v) => {
                            setSeriesID(v);
                            dirtyEditorSig.value = true;
                          }}
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
                          onChange={(v) => {
                            setLang(v);
                            dirtyEditorSig.value = true;
                          }}
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
                          onInput={(e) => {
                            setTranslationGroup((e.currentTarget as HTMLInputElement).value);
                            dirtyEditorSig.value = true;
                          }}
                        />
                      </div>
                    </div>
                    <div class="volo-check-grid" style={{ marginBottom: 0 }}>
                      <SwitchToggle
                        checked={isPinned}
                        onChange={(v) => {
                          setIsPinned(v);
                          dirtyEditorSig.value = true;
                        }}
                        label={t("art.pin_top")}
                      />
                      <SwitchToggle
                        checked={allowComment}
                        onChange={(v) => {
                          setAllowComment(v);
                          dirtyEditorSig.value = true;
                        }}
                        label={t("art.allow_comment")}
                      />
                    </div>
                  </>
                )}
              </div>

              {/* 2. Capsule Tags Selector & Bounded Preset Pool */}
              <div class={`volo-inspector-box ${collapsedPanels.tags ? "is-collapsed" : ""}`}>
                <div class="volo-inspector-title">
                  <button
                    type="button"
                    class="volo-inspector-toggle"
                    style={{ width: "auto", flex: 1 }}
                    aria-expanded={!collapsedPanels.tags}
                    onClick={() => setCollapsedPanels((p) => ({ ...p, tags: !p.tags }))}
                  >
                    <span>{t("art.free_tags")}</span>
                    <span class="volo-inspector-chev">▾</span>
                  </button>
                  {!collapsedPanels.tags && (totalTagCount > 0 || allTags.length > 0) && (
                    <button
                      type="button"
                      class={`volo-chip-btn ${tagPoolOpen ? "active" : ""}`}
                      onClick={() => setTagPoolOpen(!tagPoolOpen)}
                    >
                      {t("art.toggle_tag_pool", { count: totalTagCount || allTags.length })}
                    </button>
                  )}
                </div>
                {!collapsedPanels.tags && (
                  <>
                    <div
                      ref={tagBoxRef}
                      class="volo-tag-capsule-box"
                  style={{ cursor: "text" }}
                  onClick={(e) => {
                    if ((e.target as HTMLElement).tagName !== "BUTTON") {
                      tagInputRef.current?.focus();
                    }
                  }}
                >
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
                    ref={tagInputRef}
                    class="volo-tag-inline-input"
                    value={tagDraft}
                    placeholder={selectedTags.length === 0 ? t("art.tags_placeholder") : t("art.tag_input_more")}
                    onFocus={() => {
                      if (tagDraft.trim()) setTagSuggestOpen(true);
                    }}
                    onInput={(e) => {
                      const v = (e.currentTarget as HTMLInputElement).value;
                      setTagDraft(v);
                      setTagSuggestIdx(0);
                      setTagSuggestOpen(Boolean(v.trim()));
                    }}
                    onKeyDown={(e) => {
                      if (tagSuggestOpen && suggestOptions.length > 0) {
                        if (e.key === "ArrowDown") {
                          e.preventDefault();
                          setTagSuggestIdx((idx) => (idx + 1) % suggestOptions.length);
                          return;
                        }
                        if (e.key === "ArrowUp") {
                          e.preventDefault();
                          setTagSuggestIdx((idx) => (idx - 1 + suggestOptions.length) % suggestOptions.length);
                          return;
                        }
                        if (e.key === "Escape") {
                          e.preventDefault();
                          setTagSuggestOpen(false);
                          return;
                        }
                        if (e.key === "Enter") {
                          e.preventDefault();
                          const chosen = suggestOptions[safeSuggestIdx];
                          if (chosen) {
                            addTagTokens(chosen.name);
                            dirtyEditorSig.value = true;
                            setTagSuggestOpen(false);
                            return;
                          }
                        }
                      }
                      if (e.key === "Enter" || e.key === "," || e.key === "，") {
                        e.preventDefault();
                        if (tagDraft.trim()) {
                          addTagTokens(tagDraft);
                          dirtyEditorSig.value = true;
                          setTagSuggestOpen(false);
                        }
                      } else if (e.key === "Backspace" && !tagDraft && selectedTags.length > 0) {
                        removeTagToken(selectedTags[selectedTags.length - 1]);
                        dirtyEditorSig.value = true;
                      }
                    }}
                    onBlur={() => {
                      window.setTimeout(() => {
                        setTagSuggestOpen(false);
                      }, 120);
                      if (tagDraft.trim()) {
                        addTagTokens(tagDraft);
                        dirtyEditorSig.value = true;
                      }
                    }}
                  />
                </div>

                {tagSuggestOpen &&
                  suggestOptions.length > 0 &&
                  typeof document !== "undefined" &&
                  createPortal(
                    <div
                      ref={tagSuggestRef}
                      class="volo-cselect-menu is-portal"
                      style={tagSuggestStyle}
                      role="listbox"
                    >
                      <div class="volo-cselect-list">
                        {suggestOptions.map((opt, idx) => (
                          <button
                            key={`${opt.isCreate ? "new" : "tag"}-${opt.name}`}
                            type="button"
                            role="option"
                            aria-selected={idx === safeSuggestIdx}
                            class={`volo-cselect-item ${idx === safeSuggestIdx ? "focused" : ""}`}
                            onMouseEnter={() => setTagSuggestIdx(idx)}
                            onMouseDown={(e) => {
                              e.preventDefault();
                              addTagTokens(opt.name);
                              dirtyEditorSig.value = true;
                              setTagSuggestOpen(false);
                              tagInputRef.current?.focus();
                            }}
                          >
                            <span class="volo-cselect-item-text">
                              {opt.isCreate ? t("art.tag_create", { tag: opt.name }) : `#${opt.name}`}
                            </span>
                            {!opt.isCreate && (
                              <span class="volo-cselect-item-right">
                                <span class="volo-code-chip">{opt.count}</span>
                              </span>
                            )}
                          </button>
                        ))}
                      </div>
                    </div>,
                    document.body
                  )}

                {tagPoolOpen && (allTags.length > 0 || remoteTags.length > 0) && (
                  <div class="volo-tag-pool-box">
                    <div class="volo-tag-pool-head">
                      <input
                        type="text"
                        class="volo-input volo-tag-pool-search"
                        placeholder={t("art.tag_pool_search_ph")}
                        value={tagPoolQuery}
                        onInput={(e) => setTagPoolQuery((e.currentTarget as HTMLInputElement).value)}
                      />
                      <div class="volo-filter-pills">
                        <button
                          type="button"
                          class={`volo-filter-pill ${tagPoolSort === "count" ? "active" : ""}`}
                          onClick={() => setTagPoolSort("count")}
                        >
                          {t("art.tag_sort_popular")}
                        </button>
                        <button
                          type="button"
                          class={`volo-filter-pill ${tagPoolSort === "name" ? "active" : ""}`}
                          onClick={() => setTagPoolSort("name")}
                        >
                          {t("art.tag_sort_az")}
                        </button>
                      </div>
                    </div>
                    <div class="volo-tag-pool-scroll">
                      {visiblePoolTags.length === 0 ? (
                        <span class="volo-help">{t("tbl.empty")}</span>
                      ) : (
                        visiblePoolTags.map((tg) => {
                          const active = activeTagsLower.has(String(tg.name || "").toLowerCase());
                          const cnt = getTagCount(tg);
                          return (
                            <button
                              key={tg.id || tg.name}
                              type="button"
                              class={`volo-chip-btn ${active ? "active" : ""}`}
                              onClick={() => {
                                toggleTagChip(tg.name);
                                dirtyEditorSig.value = true;
                              }}
                            >
                              #{tg.name}
                              {cnt > 0 ? <span style={{ opacity: 0.65, marginLeft: "3px" }}>({cnt})</span> : null}
                            </button>
                          );
                        })
                      )}
                    </div>
                  </div>
                )}
                  </>
                )}
              </div>

              {/* 3. Media & Summary */}
              <div class={`volo-inspector-box ${collapsedPanels.media ? "is-collapsed" : ""}`}>
                <button
                  type="button"
                  class="volo-inspector-toggle"
                  aria-expanded={!collapsedPanels.media}
                  onClick={() => setCollapsedPanels((p) => ({ ...p, media: !p.media }))}
                >
                  <span>{t("ov.panel_media")}</span>
                  <span class="volo-inspector-chev">▾</span>
                </button>
                {!collapsedPanels.media && (
                  <>
                    <div class="volo-auto-grid">
                      <div class="volo-field">
                        <label>{t("art.cover_url")}</label>
                        <div class="volo-input-row">
                          <input
                            class="volo-input"
                            value={coverURL}
                            onInput={(e) => {
                              setCoverURL((e.currentTarget as HTMLInputElement).value);
                              dirtyEditorSig.value = true;
                            }}
                          />
                          <MediaPickButton
                            filterKind="image"
                            onPick={(url) => {
                              setCoverURL(url);
                              dirtyEditorSig.value = true;
                            }}
                          />
                        </div>
                        {coverURL.trim() && (
                          <img
                            src={coverURL}
                            alt="cover"
                            style={{
                              marginTop: "6px",
                              maxHeight: "84px",
                              width: "100%",
                              objectFit: "cover",
                              borderRadius: "8px",
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
                            onInput={(e) => {
                              setAudioURL((e.currentTarget as HTMLInputElement).value);
                              dirtyEditorSig.value = true;
                            }}
                          />
                          <MediaPickButton
                            filterKind="audio"
                            onPick={(url) => {
                              setAudioURL(url);
                              dirtyEditorSig.value = true;
                            }}
                          />
                        </div>
                      </div>
                      <div class="volo-field">
                        <label>{t("art.audio_duration")}</label>
                        <input
                          type="number"
                          min={0}
                          class="volo-input"
                          value={audioDuration}
                          onInput={(e) => {
                            setAudioDuration(Number((e.currentTarget as HTMLInputElement).value) || 0);
                            dirtyEditorSig.value = true;
                          }}
                        />
                      </div>
                    </div>
                    <div class="volo-field" style={{ marginBottom: 0 }}>
                      <label>{t("art.summary")}</label>
                      <textarea
                        class="volo-textarea"
                        rows={3}
                        value={summaryMD}
                        onInput={(e) => {
                          setSummaryMD((e.currentTarget as HTMLTextAreaElement).value);
                          dirtyEditorSig.value = true;
                        }}
                      />
                    </div>
                  </>
                )}
              </div>

              {/* 4. Revisions History & Timed Share Link */}
              <div class={`volo-inspector-box ${collapsedPanels.revisions ? "is-collapsed" : ""}`}>
                <div class="volo-inspector-title">
                  <button
                    type="button"
                    class="volo-inspector-toggle"
                    style={{ width: "auto", flex: 1 }}
                    aria-expanded={!collapsedPanels.revisions}
                    onClick={() => setCollapsedPanels((p) => ({ ...p, revisions: !p.revisions }))}
                  >
                    <span>{t("art.revisions")}</span>
                    <span class="volo-inspector-chev">▾</span>
                  </button>
                  {!collapsedPanels.revisions && editingID && (
                    <button type="button" class="volo-act-btn" onClick={() => void openRevisions()}>
                      {t("art.diff_btn")} ({inlineRevs.length})
                    </button>
                  )}
                </div>
                {!collapsedPanels.revisions && (
                  <>
                    <div class={editingID ? "volo-auto-grid" : ""}>
                      <div class="volo-field" style={{ marginBottom: editingID && inlineRevs.length > 0 ? "10px" : 0 }}>
                        <label>{t("art.rev_note")}</label>
                        <input
                          class="volo-input"
                          placeholder={t("art.rev_note_placeholder")}
                          value={revisionNote}
                          onInput={(e) => setRevisionNote((e.currentTarget as HTMLInputElement).value)}
                        />
                      </div>
                      {editingID && (
                        <div class="volo-field" style={{ marginBottom: inlineRevs.length > 0 ? "10px" : 0 }}>
                          <label>{t("art.share_token")}</label>
                          <div class="volo-input-row">
                            <ConsoleSelect
                              style={{ width: "92px", flexShrink: 0 }}
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
                            <button type="button" class="volo-btn volo-btn-sm" onClick={() => void issueShareLink()}>
                              {t("art.share_token")}
                            </button>
                          </div>
                        </div>
                      )}
                    </div>
                    {editingID && inlineRevs.length > 0 && (
                      <div style={{ display: "flex", flexDirection: "column", gap: "6px" }}>
                        {inlineRevs.slice(0, 4).map((rv) => (
                          <div
                            key={rv.id}
                            style={{
                              display: "flex",
                              alignItems: "center",
                              justifyContent: "space-between",
                              gap: "6px",
                              fontSize: "12px",
                              padding: "6px 10px",
                              borderRadius: "7px",
                              background: "var(--volo-surface)",
                              border: "1px solid var(--volo-line)",
                            }}
                          >
                            <span class="volo-help" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                              {formatDateTime(rv.created_at)} · {rv.note || t("art.snapshot")}
                            </span>
                            <div style={{ display: "inline-flex", gap: "4px", flexShrink: 0 }}>
                              <button type="button" class="volo-act-btn" onClick={() => void openRevisions(rv.id)}>
                                {t("art.diff_btn")}
                              </button>
                              <button type="button" class="volo-act-btn" onClick={() => void restoreRevision(rv.id)}>
                                {t("art.rollback")}
                              </button>
                            </div>
                          </div>
                        ))}
                      </div>
                    )}
                  </>
                )}
              </div>
            </div>

            <div class="volo-form-actions">
              <button type="button" class="volo-btn" onClick={() => void closeEditor()}>
                {t("common.cancel")}
              </button>
              <button type="button" class="volo-btn" onClick={() => void handleSave("draft", true)}>
                {t("ov.save_draft")}
              </button>
              <button type="submit" class="volo-btn volo-btn-primary">
                {t("art.save")}
              </button>
            </div>
          </form>
        </div>

        {/* Quick-Create Category / Series Modal */}
        <AnimatedModal
          open={quickCreateKind !== null}
          onClose={() => setQuickCreateKind(null)}
          title={quickCreateKind === "category" ? t("cnt.new_cat") : t("cnt.new_series")}
          maxWidth="440px"
          footer={
            <>
              <button type="button" class="volo-btn" onClick={() => setQuickCreateKind(null)}>
                {t("common.cancel")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                disabled={quickCreateSaving || !quickCreateName.trim()}
                onClick={() => void submitQuickCreate()}
              >
                {t("common.create")}
              </button>
            </>
          }
        >
          <form onSubmit={(e) => void submitQuickCreate(e)}>
            <div class="volo-field">
              <label>{quickCreateKind === "category" ? t("cnt.cat_name") : t("cnt.series_title")}</label>
              <input
                class="volo-input"
                required
                autoFocus
                value={quickCreateName}
                onInput={(e) => setQuickCreateName((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div class="volo-field" style={{ marginBottom: 0 }}>
              <label>{t("art.slug")}</label>
              <input
                class="volo-input"
                placeholder={t("art.slug_placeholder")}
                value={quickCreateSlug}
                onInput={(e) => setQuickCreateSlug((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
          </form>
        </AnimatedModal>

        {/* Unsaved Draft Inline Preview Modal */}
        <AnimatedModal
          open={inlinePreviewHTML !== null}
          onClose={() => setInlinePreviewHTML(null)}
          title={title || t("common.preview")}
          maxWidth="780px"
          footer={
            <button type="button" class="volo-btn" onClick={() => setInlinePreviewHTML(null)}>
              {t("common.close")}
            </button>
          }
        >
          <div
            class="volo-preview-pane"
            style={{ height: "auto", maxHeight: "65vh" }}
            dangerouslySetInnerHTML={{ __html: inlinePreviewHTML || "" }}
          />
        </AnimatedModal>

        {/* Revisions & Line Diff Viewer Modal */}
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
            <ConsoleEmpty text={t("art.revisions_empty")} />
          ) : (
            revisions.map((r) => {
              const showDiff = diffRevID === r.id;
              const diffLines = showDiff ? computeLineDiff(r.content_md || "", contentMD) : [];
              const diffChunks = showDiff ? buildHunkChunks(diffLines, expandedDiffFolds) : [];
              return (
                <div key={r.id} class="volo-subcard">
                  <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: "8px", flexWrap: "wrap", marginBottom: "6px" }}>
                    <div>
                      <strong>{r.title}</strong>{" "}
                      <span class="volo-help">· {formatDateTime(r.created_at)} · {r.note || t("art.snapshot")}</span>
                    </div>
                    <div style={{ display: "flex", gap: "6px" }}>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        onClick={() => {
                          setExpandedDiffFolds({});
                          setDiffRevID(showDiff ? null : r.id);
                        }}
                      >
                        {t("art.diff_btn")}
                      </button>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm volo-btn-primary"
                        onClick={() => void restoreRevision(r.id)}
                      >
                        {t("art.rollback")}
                      </button>
                    </div>
                  </div>
                  {showDiff ? (
                    <div>
                      <div class="volo-help" style={{ marginBottom: "4px" }}>{t("art.diff_title")}</div>
                      <div class="volo-diff-box">
                        {diffChunks.map((ch, idx) =>
                          ch.type === "fold" ? (
                            <button
                              key={ch.id}
                              type="button"
                              class="volo-diff-hunk-btn"
                              onClick={() =>
                                setExpandedDiffFolds((prev) => ({ ...prev, [ch.id]: true }))
                              }
                            >
                              ↕ {t("art.diff_expand_lines", { n: ch.lines.length })}
                            </button>
                          ) : (
                            <div
                              key={idx}
                              class={`volo-diff-line ${
                                ch.line.kind === "add"
                                  ? "volo-diff-add"
                                  : ch.line.kind === "del"
                                  ? "volo-diff-del"
                                  : ""
                              }`}
                            >
                              <span class="volo-diff-ln">{ch.line.oldLine ?? ""}</span>
                              <span class="volo-diff-ln">{ch.line.newLine ?? ""}</span>
                              <span class="volo-diff-code">{ch.line.text}</span>
                            </div>
                          )
                        )}
                      </div>
                    </div>
                  ) : (
                    <pre style={{ margin: 0, maxHeight: "110px", overflow: "auto", fontSize: "11.5px", background: "var(--volo-surface)", padding: "8px", borderRadius: "6px" }}>
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
    const displayArts =
      articles.length > artPageSize ? paginateItems(articles, artPage, artPageSize).slice : articles;
    const pageIds = displayArts.map((r) => r.article.id);
    const allPageSelected = pageIds.length > 0 && pageIds.every((id) => selectedIds.includes(id));

    const togglePageAll = (checked: boolean) => {
      if (checked) {
        setSelectedIds((prev) => Array.from(new Set([...prev, ...pageIds])));
      } else {
        const rm = new Set(pageIds);
        setSelectedIds((prev) => prev.filter((id) => !rm.has(id)));
      }
    };

    return (
      <div class="volo-card">
        <div class="volo-filter-bar">
          <div class="volo-filter-pills">
            {(["all", "published", "draft", "scheduled"] as const).map((st) => {
              const cnt = statusCounts[st];
              return (
                <button
                  key={st}
                  type="button"
                  class={`volo-filter-pill ${statusFilter === st ? "active" : ""}`}
                  onClick={() => {
                    setStatusFilter(st);
                    setArtPage(1);
                  }}
                >
                  {t(`art.${st}`)}
                  {typeof cnt === "number" ? ` (${formatCompactNum(cnt)})` : ""}
                </button>
              );
            })}
          </div>
          <div class="volo-filter-controls">
            <ConsoleSelect
              style={{ width: "148px" }}
              value={catFilter}
              onSearch={searchRemoteCategories}
              onChange={(v) => {
                setCatFilter(v);
                setArtPage(1);
              }}
              options={[
                { value: "", label: t("art.all_categories") },
                ...categories.map((c) => ({ value: c.id, label: c.name })),
              ]}
            />
            <ConsoleSelect
              style={{ width: "144px" }}
              value={tagFilter}
              onSearch={searchRemoteTags}
              onChange={(v) => {
                setTagFilter(v);
                setArtPage(1);
              }}
              options={[
                { value: "", label: t("art.all_tags") },
                ...allTags.map((tg) => ({ value: tg.id, label: `#${tg.name}` })),
              ]}
            />
            <ConsoleSelect
              style={{ width: "144px" }}
              value={visFilter}
              onChange={(v) => {
                setVisFilter(v);
                setArtPage(1);
              }}
              options={[
                { value: "", label: t("art.vis_all") },
                { value: "public", label: t("ov.vis_public") },
                { value: "password", label: t("ov.vis_password") },
                { value: "locked", label: t("tbl.locked_block") },
                { value: "pinned", label: t("tbl.pinned") },
              ]}
            />
            <ConsoleSelect
              style={{ width: "152px" }}
              value={artSort}
              onChange={(v) => {
                setArtSort(v);
                setArtPage(1);
              }}
              options={[
                { value: "updated", label: t("art.sort_updated") },
                { value: "published", label: t("art.sort_published") },
                { value: "views", label: t("art.sort_views") },
                { value: "comments", label: t("art.sort_comments") },
                { value: "title", label: t("art.sort_title") },
              ]}
            />
            <input
              class="volo-input volo-search-input"
              placeholder={t("art.search_placeholder")}
              value={query}
              onInput={(e) => {
                setQuery((e.currentTarget as HTMLInputElement).value);
                setArtPage(1);
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  setArtPage(1);
                  void loadArticlesPage({ page: 1, q: query });
                }
              }}
            />
            <UploadButton
              accept=".md,.markdown,.zip"
              busy={importingZip}
              label={t("art.import_md")}
              onFile={(f) => void handleBatchImport(f)}
            />
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

        <MobileSelectAllBar
          checked={allPageSelected}
          count={selectedIds.length}
          total={pageIds.length}
          onToggle={togglePageAll}
        />

        <div class="volo-table-wrap">
          <table class="volo-table">
            <thead>
              <tr>
                <th class="col-check" style={{ width: "40px" }}>
                  <input
                    type="checkbox"
                    aria-label={t("ov.select_all")}
                    checked={allPageSelected}
                    onChange={(e) => togglePageAll((e.currentTarget as HTMLInputElement).checked)}
                  />
                </th>
                <th class="col-fluid">{t("art.title")}</th>
                <th class="col-status">{t("art.status")}</th>
                <th class="col-cat">{t("art.category")}</th>
                <th class="col-num">{t("art.metrics")}</th>
                <th class="col-date">{t("art.updated")}</th>
                <th class="col-act col-act-th">{t("art.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {artLoading ? (
                <TableSkeleton cols={7} />
              ) : displayArts.length === 0 ? (
                <ConsoleEmpty colSpan={7} text={t("art.empty", { btn: t("art.new") })} />
              ) : (
                displayArts.map((row) => {
                  const a = row.article;
                  const catLabel = catNameMap[a.category_id] || (a as any).category_name || (row as any).category_name;
                  const isChecked = selectedIds.includes(a.id);
                  return (
                    <tr key={a.id}>
                      <td class="col-check">
                        <input
                          type="checkbox"
                          aria-label={a.title}
                          checked={isChecked}
                          onChange={(e) => {
                            const checked = (e.currentTarget as HTMLInputElement).checked;
                            setSelectedIds((prev) =>
                              checked ? (prev.includes(a.id) ? prev : [...prev, a.id]) : prev.filter((id) => id !== a.id)
                            );
                          }}
                        />
                      </td>
                      <td class="col-fluid" data-label={t("art.title")}>
                        <div class="volo-cell-main">
                          <div class="volo-cell-title">
                            <a
                              href={`#edit-${a.id}`}
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
                      <td class="col-status" data-label={t("art.status")}>
                        <span class={`volo-status-pill ${a.status || "draft"}`}>
                          <span class="volo-status-dot" />
                          {t(`art.${a.status}`)}
                        </span>
                      </td>
                      <td class="col-cat" data-label={t("art.category")}>
                        <span class={`volo-cat-chip ${catLabel ? "" : "muted"}`}>
                          {catLabel || t("art.uncategorized")}
                        </span>
                      </td>
                      <td class="col-num" data-label={t("art.metrics")}>
                        <div class="volo-metric-pair">
                          <span class="volo-metric-item" title={`PV: ${(a.view_count ?? 0).toLocaleString()}`}>
                            <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                              <path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7-10-7-10-7Z" />
                              <circle cx="12" cy="12" r="3" />
                            </svg>
                            {formatCompactNum(a.view_count ?? 0)}
                          </span>
                          <span class="volo-metric-item" title={`${t("nav.comments")}: ${(a.comment_count ?? 0).toLocaleString()}`}>
                            <svg width="12.5" height="12.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9">
                              <path d="M21 15a2 2 0 0 1-2 2H8l-5 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
                            </svg>
                            {formatCompactNum(a.comment_count ?? 0)}
                          </span>
                        </div>
                      </td>
                      <td class="col-date" data-label={t("art.updated")}>{formatDateTime(a.updated_at)}</td>
                      <td class="col-act" data-label={t("art.actions")}>
                        <div class="volo-row-actions">
                          <a
                            class="volo-act-btn"
                            href={`/posts/${encodeURIComponent(a.slug)}${a.status !== "published" ? "?preview=1" : ""}`}
                            target="_blank"
                            rel="noreferrer"
                          >
                            {t("common.view")}
                          </a>
                          <button type="button" class="volo-act-btn" onClick={() => void openEditArticle(a.id)}>
                            {t("common.edit")}
                          </button>
                          <ActionMenu
                            items={[
                              {
                                label: a.is_pinned ? t("art.unpin_action") : t("art.pin_action"),
                                onClick: () => void togglePinArticle(a.id),
                              },
                              {
                                label: t("common.delete"),
                                danger: true,
                                onClick: () => void deleteArticle(a.id),
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

        <BatchBar count={selectedIds.length} onClear={() => setSelectedIds([])}>
          <button type="button" class="volo-act-btn" onClick={() => void runBatchAction("publish")}>
            {t("art.published")}
          </button>
          <button type="button" class="volo-act-btn" onClick={() => void runBatchAction("draft")}>
            {t("art.draft")}
          </button>
          <button type="button" class="volo-act-btn" onClick={() => void runBatchAction("pin")}>
            {t("art.pin_action")}
          </button>
          <button type="button" class="volo-act-btn" onClick={() => void runBatchAction("unpin")}>
            {t("art.unpin_action")}
          </button>
          <ConsoleSelect
            style={{ width: "148px" }}
            value={batchCatId}
            onSearch={searchRemoteCategories}
            onChange={(val) => {
              if (val === "__prompt__") return;
              setBatchCatId(val);
              void runBatchAction("category", val === "__none__" ? "" : val);
            }}
            options={[
              { value: "__prompt__", label: t("ov.batch_move_cat") },
              { value: "__none__", label: t("art.uncategorized") },
              ...categories.map((c) => ({ value: c.id, label: c.name })),
            ]}
          />
          <button type="button" class="volo-act-btn danger" onClick={() => void runBatchAction("delete")}>
            {t("common.delete")}
          </button>
        </BatchBar>

        <Pagination
          total={artTotal}
          page={artPage}
          pageSize={artPageSize}
          onPageSizeChange={(sz) => {
            setArtPageSize(sz);
            setArtPage(1);
          }}
          onChange={setArtPage}
        />
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

  const W = isNarrowChart ? 460 : 860;
  const H = isNarrowChart ? 204 : 228;
  const padL = isNarrowChart ? 40 : 52;
  const padR = isNarrowChart ? 18 : 34;
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
  const xLabelStep = isNarrowChart ? (n <= 7 ? 1 : Math.ceil(n / 5)) : n <= 10 ? 1 : Math.ceil(n / 8);
  const minTickGap = Math.max(1, Math.floor(xLabelStep * 0.6));
  const topArticles: any[] = overview?.top_articles || [];
  const topReferers: any[] = overview?.top_referers || [];
  const recentLogs: any[] = overview?.recent_logs || [];
  const noticeBoard: string = overview?.notice_board || "";
  const pendingCommentCount = Number(overview?.counts?.pending_comments ?? 0);

  const activeTrend = hoverIdx !== null && rawSeries[hoverIdx] ? rawSeries[hoverIdx] : null;
  const activePvPt = hoverIdx !== null && pvPts[hoverIdx] ? pvPts[hoverIdx] : null;

  const handleTrendTouch = (e: TouchEvent) => {
    const touch = e.touches?.[0];
    if (!touch) return;
    const idx = pickSvgIndex(e.currentTarget as SVGSVGElement, touch.clientX, n, W, padL, plotW);
    if (idx !== null) setHoverIdx(idx);
  };

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

      {/* Pending Comments Quick-Action Banner */}
      {pendingCommentCount > 0 && (
        <div
          class="volo-draft-banner"
          style={{ marginBottom: "14px" }}
        >
          <span>{t("ov.pending_banner", { n: pendingCommentCount })}</span>
          <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={() => onNavigate?.("comments")}>
            {t("ov.review_now")} →
          </button>
        </div>
      )}

      {/* Content & Runtime Summary KPIs */}
      {overview ? (
        <div class="volo-kpi-grid">
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("articles")}>
            <div class="volo-kpi-label">{t("ov.published")}</div>
            <div class="volo-kpi-val" title={Number(overview.counts?.published ?? 0).toLocaleString()}>
              {formatCompactNum(overview.counts?.published ?? 0)}
            </div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("drafts")}>
            <div class="volo-kpi-label">{t("ov.drafts")}</div>
            <div class="volo-kpi-val" title={Number(overview.counts?.drafts ?? 0).toLocaleString()}>
              {formatCompactNum(overview.counts?.drafts ?? 0)}
            </div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("pages")}>
            <div class="volo-kpi-label">{t("ov.pages")}</div>
            <div class="volo-kpi-val" title={Number(overview.counts?.pages ?? 0).toLocaleString()}>
              {formatCompactNum(overview.counts?.pages ?? 0)}
            </div>
          </div>
          <div class="volo-kpi volo-kpi-click" onClick={() => onNavigate?.("comments")}>
            <div class="volo-kpi-label">{t("ov.pending_comments")}</div>
            <div class="volo-kpi-val" title={pendingCommentCount.toLocaleString()}>
              {formatCompactNum(pendingCommentCount)}
            </div>
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
      ) : (
        <div class="volo-kpi-grid" aria-hidden="true">
          {[0, 1, 2, 3, 4].map((i) => (
            <div key={i} class="volo-kpi">
              <div class="volo-skel-line" style={{ width: "54%", height: "12px", marginBottom: "10px" }} />
              <div class="volo-skel-line" style={{ width: "42%", height: "22px" }} />
            </div>
          ))}
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
            <div class="volo-kpi-val" title={rangePV.toLocaleString()}>{formatCompactNum(rangePV)}</div>
          </div>
          <div class="volo-trend-kpi">
            <div class="volo-kpi-label">{t("ov.kpi_uv")}</div>
            <div class="volo-kpi-val" title={rangeUV.toLocaleString()}>{formatCompactNum(rangeUV)}</div>
          </div>
          <div class="volo-trend-kpi">
            <div class="volo-kpi-label">{t("ov.kpi_privacy")}</div>
            <div style={{ display: "flex", alignItems: "baseline", gap: "8px", flexWrap: "wrap" }}>
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
              <span style={{ width: "10px", height: "10px", borderRadius: "3px", background: "var(--volo-chart-uv)" }} />
              {t("ov.legend_uv")}
            </span>
          </div>

          <div class="volo-chart-stage" ref={chartStageRef}>
            {activeTrend && activePvPt && (
              <div
                class="volo-chart-tooltip"
                style={{
                  left: activePvPt.x > W * 0.5 ? "auto" : `${Math.max(4, (activePvPt.x / W) * 100)}%`,
                  right: activePvPt.x > W * 0.5 ? `${Math.max(4, ((W - activePvPt.x) / W) * 100)}%` : "auto",
                }}
              >
                <div style={{ fontWeight: 650, marginBottom: "2px" }}>{activeTrend.stat_date}</div>
                <div style={{ display: "flex", alignItems: "center", gap: "10px" }}>
                  <span>PV: <strong>{Number(activeTrend.pv || 0).toLocaleString()}</strong></span>
                  <span style={{ color: "var(--volo-chart-uv-on-dark)" }}>UV: <strong>{Number(activeTrend.uv || 0).toLocaleString()}</strong></span>
                </div>
              </div>
            )}

            <svg
              viewBox={`0 0 ${W} ${H}`}
              style={{ width: "100%", height: "auto", display: "block", cursor: "crosshair", touchAction: "pan-y" }}
              onMouseMove={(e) => {
                const idx = pickSvgIndex(e.currentTarget as SVGSVGElement, e.clientX, n, W, padL, plotW);
                if (idx !== null) setHoverIdx(idx);
              }}
              onMouseLeave={() => setHoverIdx(null)}
              onTouchStart={handleTrendTouch}
              onTouchMove={handleTrendTouch}
            >
              <defs>
                <linearGradient id="voloPvGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="var(--volo-accent)" stop-opacity="0.22" />
                  <stop offset="100%" stop-color="var(--volo-accent)" stop-opacity="0.01" />
                </linearGradient>
                <linearGradient id="voloUvGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="0%" stop-color="var(--volo-chart-uv)" stop-opacity="0.16" />
                  <stop offset="100%" stop-color="var(--volo-chart-uv)" stop-opacity="0.01" />
                </linearGradient>
              </defs>

              {rawSeries.map((d, idx) => {
                const showTick = idx === n - 1 || (idx % xLabelStep === 0 && n - 1 - idx >= minTickGap);
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
                      font-size="11.5"
                      fill="var(--volo-muted)"
                    >
                      {formatCompactNum(val)}
                    </text>
                  </g>
                );
              })}

              {uvArea && <path d={uvArea} fill="url(#voloUvGrad)" />}
              {pvArea && <path d={pvArea} fill="url(#voloPvGrad)" />}
              {pvLine && <path d={pvLine} fill="none" stroke="var(--volo-accent)" stroke-width="2.5" stroke-linecap="round" />}
              {uvLine && <path d={uvLine} fill="none" stroke="var(--volo-chart-uv)" stroke-width="2" stroke-dasharray="5 4" stroke-linecap="round" />}

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
                const showLabel = idx === n - 1 || (idx % xLabelStep === 0 && n - 1 - idx >= minTickGap);
                const isHover = hoverIdx === idx;
                return (
                  <g key={d.stat_date}>
                    <circle
                      cx={u.x}
                      cy={u.y}
                      r={isHover ? 4.4 : 2.6}
                      fill="var(--volo-surface)"
                      stroke="var(--volo-chart-uv)"
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
                        font-size="11.5"
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
                  <span class="volo-badge" title={Number(it.hits ?? 0).toLocaleString()}>
                    {t("ov.hits_unit", { hits: formatCompactNum(it.hits ?? 0) })}
                  </span>
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
                  <span class="volo-badge" title={Number(it.hits ?? 0).toLocaleString()}>
                    {t("ov.hits_unit", { hits: formatCompactNum(it.hits ?? 0) })}
                  </span>
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
          {(() => {
            const monthly12: Array<{ month: string; count: number }> = overview?.monthly_articles || [];
            const monthlyHist: Array<{ month: string; count: number }> = overview?.monthly_history || [];
            const hasHist = monthlyHist.length > 0;
            const recent12Sum = monthly12.reduce((acc, x) => acc + Number(x.count || 0), 0);
            const hasOutside12m = monthlyHist.some((h) => !monthly12.some((m) => m.month === h.month));
            const effectiveMode: "12m" | "history" = !hasHist
              ? "12m"
              : monthlyRangeMode === "auto"
                ? hasOutside12m || recent12Sum === 0
                  ? "history"
                  : "12m"
                : monthlyRangeMode;
            return (
              <div class="volo-mini-chart-card">
                <div class="volo-mini-chart-head">
                  <h4 class="volo-mini-chart-title" style={{ margin: 0 }}>{t("ov.chart_monthly")}</h4>
                  {hasHist && (
                    <div class="volo-chart-toggle">
                      <button
                        type="button"
                        class={`volo-chart-toggle-btn ${effectiveMode === "history" ? "is-active" : ""}`}
                        onClick={() => setMonthlyRangeMode("history")}
                      >
                        {t("ov.monthly_history")}
                      </button>
                      <button
                        type="button"
                        class={`volo-chart-toggle-btn ${effectiveMode === "12m" ? "is-active" : ""}`}
                        onClick={() => setMonthlyRangeMode("12m")}
                      >
                        {t("ov.monthly_12m")}
                      </button>
                    </div>
                  )}
                </div>
                <MonthlyArticlesChart
                  items={effectiveMode === "history" ? monthlyHist : monthly12}
                  isHistory={effectiveMode === "history"}
                />
              </div>
            );
          })()}
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
              style={{
                margin: 0,
                whiteSpace: "pre-wrap",
                lineHeight: 1.65,
                fontSize: "13.5px",
                maxHeight: "220px",
                overflowY: "auto",
                overflowWrap: "anywhere",
              }}
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
                <div key={lg.id} class="volo-rank-row volo-audit-rank-row">
                  <div style={{ display: "flex", alignItems: "center", gap: "8px", minWidth: 0, flex: "1 1 0" }}>
                    <span class="volo-code-chip" title={lg.action}>
                      {localizeAuditAction(lg.action)}
                    </span>
                    <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", fontSize: "13px", minWidth: 0 }}>
                      <strong>{lg.actor_name || "-"}</strong>
                      {lg.resource_type ? ` · ${localizeAuditResource(lg.resource_type)}` : ""}
                    </span>
                  </div>
                  <span class="volo-help volo-audit-rank-time" style={{ fontFamily: "var(--volo-mono)", fontSize: "11.5px", whiteSpace: "nowrap", flexShrink: 0 }}>
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
            <div key={idx} class="volo-card" style={{ overflowX: "auto" }} dangerouslySetInnerHTML={{ __html: c.html || "" }} />
          ))}
        </div>
      )}
    </div>
  );
}
