import { useEffect, useRef, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import type { CSSProperties, ComponentChildren } from "preact";
import { confirmSig, resolveConfirm } from "../api";
import type { LocaleOption } from "../i18n";
import { t } from "../i18n";

export function useDebouncedValue<T>(value: T, delay = 250): T {
  const [debounced, setDebounced] = useState<T>(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), delay);
    return () => window.clearTimeout(id);
  }, [value, delay]);
  return debounced;
}

export function computePortalMenuStyle(
  anchorRect: DOMRect,
  opts?: { minW?: number; maxW?: number; maxH?: number; estH?: number; align?: "left" | "right" }
): { style: CSSProperties; flipUp: boolean } {
  const vv = typeof window !== "undefined" ? window.visualViewport : null;
  const vvTop = vv?.offsetTop ?? 0;
  const vvLeft = vv?.offsetLeft ?? 0;
  const vpW = vv?.width ?? (typeof window !== "undefined" ? window.innerWidth : 1024);
  const vpH = vv?.height ?? (typeof window !== "undefined" ? window.innerHeight : 768);
  const margin = 8;
  const prefMaxW = opts?.maxW ?? 340;
  const prefMaxH = opts?.maxH ?? 268;
  const maxAllowedW = Math.max(136, Math.min(prefMaxW, vpW - margin * 2));
  const minW = Math.min(maxAllowedW, Math.max(opts?.minW ?? 164, Math.round(anchorRect.width)));

  const spaceBelow = vvTop + vpH - anchorRect.bottom - margin;
  const spaceAbove = anchorRect.top - vvTop - margin;
  const flipUp = spaceBelow < Math.min(220, prefMaxH) && spaceAbove > spaceBelow;
  const availH = Math.max(96, (flipUp ? spaceAbove : spaceBelow) - 4);
  const actualMaxH = Math.min(prefMaxH, availH);
  const openUpH = opts?.estH ? Math.min(actualMaxH, Math.max(44, opts.estH)) : actualMaxH;

  const preferRight =
    opts?.align === "right" ||
    (anchorRect.left + minW > vvLeft + vpW - margin && anchorRect.right > vvLeft + vpW / 2);

  const leftPx = preferRight
    ? Math.max(vvLeft + margin, Math.min(vvLeft + vpW - margin - minW, Math.round(anchorRect.right - minW)))
    : Math.max(vvLeft + margin, Math.min(vvLeft + vpW - margin - minW, Math.round(anchorRect.left)));

  const topPx = flipUp
    ? Math.max(vvTop + margin, Math.round(anchorRect.top - openUpH - 4))
    : Math.max(vvTop + margin, Math.round(anchorRect.bottom + 4));

  const style: CSSProperties = {
    position: "fixed",
    top: `${topPx}px`,
    left: `${leftPx}px`,
    minWidth: `${minW}px`,
    maxWidth: `${Math.max(minW, Math.min(maxAllowedW, vvLeft + vpW - margin - leftPx))}px`,
    maxHeight: `${actualMaxH}px`,
    zIndex: 2200,
  };

  return { style, flipUp };
}

export interface SelectOption {
  value: string;
  label: string;
  badge?: string;
}

export interface ConsoleSelectProps {
  value: string;
  options: SelectOption[];
  onChange: (nextValue: string) => void;
  fallbackLabel?: string;
  onSearch?: (query: string) => void | Promise<void>;
  style?: CSSProperties;
  className?: string;
  disabled?: boolean;
  align?: "left" | "right";
  searchable?: boolean;
  searchPlaceholder?: string;
}

export function ConsoleSelect({
  value,
  options,
  onChange,
  fallbackLabel,
  onSearch,
  style,
  className = "",
  disabled = false,
  align = "left",
  searchable,
  searchPlaceholder,
}: ConsoleSelectProps) {
  const [open, setOpen] = useState(false);
  const [closing, setClosing] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [menuStyle, setMenuStyle] = useState<CSSProperties>({});
  const [focusIdx, setFocusIdx] = useState<number>(-1);
  const [searchQ, setSearchQ] = useState("");
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const searchRef = useRef<HTMLInputElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);
  const onSearchRef = useRef(onSearch);
  onSearchRef.current = onSearch;

  const isSearchable = searchable ?? (Boolean(onSearch) || options.length > 10);
  const selected = options.find((o) => o.value === value) ?? null;
  const displayLabel = selected
    ? selected.label
    : value === ""
    ? fallbackLabel || ""
    : fallbackLabel || String(value);

  const filteredOptions =
    isSearchable && searchQ.trim()
      ? options.filter((o) => {
          const q = searchQ.trim().toLowerCase();
          return (
            o.label.toLowerCase().includes(q) ||
            o.value.toLowerCase().includes(q) ||
            (o.badge && o.badge.toLowerCase().includes(q))
          );
        })
      : options;

  const updateMenuPosition = () => {
    const el = btnRef.current || wrapRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const vv = window.visualViewport;
    const vvTop = vv?.offsetTop ?? 0;
    const vpH = vv?.height ?? window.innerHeight;
    if (rect.bottom < vvTop - 40 || rect.top > vvTop + vpH + 40) {
      setOpen(false);
      setClosing(false);
      return;
    }
    const estRows = Math.max(1, filteredOptions.length);
    const estH = menuRef.current?.offsetHeight || estRows * 35 + (isSearchable ? 50 : 12);
    const pos = computePortalMenuStyle(rect, {
      minW: 164,
      maxW: 340,
      maxH: isSearchable ? 300 : 264,
      estH,
      align,
    });
    setMenuStyle(pos.style);
    setFlipUp(pos.flipUp);
  };

  const closeAnimated = () => {
    if (!open || closing) return;
    setClosing(true);
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    closeTimerRef.current = window.setTimeout(() => {
      setOpen(false);
      setClosing(false);
      setSearchQ("");
    }, 130);
  };

  useEffect(() => {
    return () => {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    };
  }, []);

  useEffect(() => {
    if (!open || !onSearchRef.current) return;
    const q = searchQ.trim();
    const timer = window.setTimeout(() => {
      void onSearchRef.current?.(q);
    }, 220);
    return () => window.clearTimeout(timer);
  }, [open, searchQ]);

  useEffect(() => {
    if (open) updateMenuPosition();
  }, [open, filteredOptions.length, isSearchable]);

  useEffect(() => {
    if (!open) return;
    const onDocDown = (ev: MouseEvent) => {
      const target = ev.target as Node;
      if (wrapRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      closeAnimated();
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") closeAnimated();
    };
    const onScrollOrResize = (ev: Event) => {
      if (ev.type === "scroll" && menuRef.current && ev.target instanceof Node && menuRef.current.contains(ev.target)) {
        return;
      }
      updateMenuPosition();
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", onScrollOrResize);
    window.addEventListener("scroll", onScrollOrResize, true);
    vv?.addEventListener("resize", onScrollOrResize);
    vv?.addEventListener("scroll", onScrollOrResize);
    const isCoarse = Boolean(window.matchMedia?.("(pointer: coarse)")?.matches);
    if (isSearchable && !isCoarse) {
      window.requestAnimationFrame(() => searchRef.current?.focus());
    }
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", onScrollOrResize);
      window.removeEventListener("scroll", onScrollOrResize, true);
      vv?.removeEventListener("resize", onScrollOrResize);
      vv?.removeEventListener("scroll", onScrollOrResize);
    };
  }, [open, closing, isSearchable]);

  const toggleOpen = () => {
    if (disabled) return;
    if (open && !closing) {
      closeAnimated();
      return;
    }
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    setClosing(false);
    setSearchQ("");
    updateMenuPosition();
    const curIdx = options.findIndex((o) => o.value === value);
    setFocusIdx(curIdx >= 0 ? curIdx : 0);
    setOpen(true);
  };

  const handleKeyDown = (e: KeyboardEvent) => {
    if (disabled) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else if (focusIdx >= 0 && focusIdx < filteredOptions.length) {
        onChange(filteredOptions[focusIdx].value);
        closeAnimated();
      }
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i + 1) % Math.max(1, filteredOptions.length));
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i - 1 + filteredOptions.length) % Math.max(1, filteredOptions.length));
      }
    }
  };

  const portalMenu =
    open && typeof document !== "undefined"
      ? createPortal(
          <div
            ref={menuRef}
            class={`volo-cselect-menu is-portal ${flipUp ? "flip-up" : ""} ${closing ? "is-closing" : ""}`}
            style={menuStyle}
            role="listbox"
          >
            {isSearchable && (
              <div class="volo-cselect-search-wrap">
                <input
                  ref={searchRef}
                  type="text"
                  class="volo-cselect-search"
                  placeholder={searchPlaceholder || t("ui.select_search_ph")}
                  value={searchQ}
                  onInput={(e) => {
                    setSearchQ((e.currentTarget as HTMLInputElement).value);
                    setFocusIdx(0);
                  }}
                  onKeyDown={(e) => {
                    if (e.key === "ArrowDown") {
                      e.preventDefault();
                      setFocusIdx((i) => (i + 1) % Math.max(1, filteredOptions.length));
                    } else if (e.key === "ArrowUp") {
                      e.preventDefault();
                      setFocusIdx((i) => (i - 1 + filteredOptions.length) % Math.max(1, filteredOptions.length));
                    } else if (e.key === "Enter") {
                      e.preventDefault();
                      if (focusIdx >= 0 && focusIdx < filteredOptions.length) {
                        onChange(filteredOptions[focusIdx].value);
                        closeAnimated();
                      }
                    } else if (e.key === "Escape") {
                      e.preventDefault();
                      closeAnimated();
                    }
                  }}
                />
              </div>
            )}
            <div class="volo-cselect-list">
              {filteredOptions.length === 0 ? (
                <div class="volo-cselect-empty">{t("tbl.empty")}</div>
              ) : (
                filteredOptions.map((opt, idx) => {
                  const isAct = opt.value === value;
                  const isFocused = idx === focusIdx;
                  return (
                    <button
                      key={opt.value}
                      type="button"
                      role="option"
                      aria-selected={isAct}
                      class={`volo-cselect-item ${isAct ? "active" : ""} ${isFocused ? "focused" : ""}`}
                      onMouseEnter={() => setFocusIdx(idx)}
                      onClick={() => {
                        onChange(opt.value);
                        closeAnimated();
                      }}
                    >
                      <span class="volo-cselect-item-text">{opt.label}</span>
                      <span class="volo-cselect-item-right">
                        {opt.badge && <span class="volo-code-chip">{opt.badge}</span>}
                        {isAct && (
                          <svg
                            class="volo-cselect-check"
                            viewBox="0 0 20 20"
                            fill="none"
                            stroke="currentColor"
                            stroke-width="2.2"
                            stroke-linecap="round"
                            stroke-linejoin="round"
                          >
                            <polyline points="4.5 10.5 8.5 14.5 15.5 6.5" />
                          </svg>
                        )}
                      </span>
                    </button>
                  );
                })
              )}
            </div>
          </div>,
          document.body
        )
      : null;

  return (
    <div
      ref={wrapRef}
      class={`volo-cselect ${open && !closing ? "is-open" : ""} ${disabled ? "is-disabled" : ""} ${className}`.trim()}
      style={style}
    >
      <button
        ref={btnRef}
        type="button"
        class="volo-cselect-btn"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open && !closing}
        onClick={toggleOpen}
        onKeyDown={handleKeyDown}
      >
        <span class="volo-cselect-label">{displayLabel}</span>
        <svg
          class="volo-cselect-chev"
          viewBox="0 0 20 20"
          fill="none"
          stroke="currentColor"
          stroke-width="1.9"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <polyline points="5.5 7.5 10 12.5 14.5 7.5" />
        </svg>
      </button>
      {portalMenu}
    </div>
  );
}

const DP_MONTH_KEYS = [
  "dp.m1",
  "dp.m2",
  "dp.m3",
  "dp.m4",
  "dp.m5",
  "dp.m6",
  "dp.m7",
  "dp.m8",
  "dp.m9",
  "dp.m10",
  "dp.m11",
  "dp.m12",
];
const DP_WEEK_KEYS = ["dp.w0", "dp.w1", "dp.w2", "dp.w3", "dp.w4", "dp.w5", "dp.w6"];

function pad2(n: number): string {
  return String(Math.max(0, Math.floor(n))).padStart(2, "0");
}

function parseDatePickerValue(raw: string, mode: "month" | "datetime") {
  const s = String(raw || "").trim();
  if (mode === "month") {
    const m = /^(\d{4})-(\d{2})/.exec(s);
    if (m) {
      const y = Number(m[1]);
      const mo = Math.min(12, Math.max(1, Number(m[2])));
      return { valid: true, y, m: mo, d: 1, hh: 0, mm: 0 };
    }
  } else {
    const dt = /^(\d{4})-(\d{2})-(\d{2})(?:[T\s](\d{2}):(\d{2}))?/.exec(s);
    if (dt) {
      const y = Number(dt[1]);
      const mo = Math.min(12, Math.max(1, Number(dt[2])));
      const maxD = new Date(y, mo, 0).getDate();
      const d = Math.min(maxD, Math.max(1, Number(dt[3])));
      const hh = dt[4] !== undefined ? Math.min(23, Math.max(0, Number(dt[4]))) : 0;
      const mm = dt[5] !== undefined ? Math.min(59, Math.max(0, Number(dt[5]))) : 0;
      return { valid: true, y, m: mo, d, hh, mm };
    }
  }
  const now = new Date();
  return {
    valid: false,
    y: now.getFullYear(),
    m: now.getMonth() + 1,
    d: now.getDate(),
    hh: now.getHours(),
    mm: now.getMinutes(),
  };
}

export interface ConsoleDatePickerProps {
  value: string;
  onChange: (val: string) => void;
  mode?: "month" | "datetime";
  placeholder?: string;
  clearable?: boolean;
  disabled?: boolean;
  className?: string;
  style?: CSSProperties;
  ariaLabel?: string;
}

export function ConsoleDatePicker({
  value,
  onChange,
  mode = "datetime",
  placeholder,
  clearable = true,
  disabled = false,
  className = "",
  style,
  ariaLabel,
}: ConsoleDatePickerProps) {
  const [open, setOpen] = useState(false);
  const [closing, setClosing] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [popStyle, setPopStyle] = useState<CSSProperties>({});

  const parsed = parseDatePickerValue(value, mode);
  const [viewYear, setViewYear] = useState(parsed.y);
  const [viewMonth, setViewMonth] = useState(parsed.m);
  const [draftDate, setDraftDate] = useState(`${parsed.y}-${pad2(parsed.m)}-${pad2(parsed.d)}`);
  const [draftHour, setDraftHour] = useState(parsed.hh);
  const [draftMinute, setDraftMinute] = useState(parsed.mm);
  const [hourInput, setHourInput] = useState(pad2(parsed.hh));
  const [minInput, setMinInput] = useState(pad2(parsed.mm));

  const wrapRef = useRef<HTMLDivElement | null>(null);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const popRef = useRef<HTMLDivElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);

  const syncFromProp = () => {
    const p = parseDatePickerValue(value, mode);
    setViewYear(p.y);
    setViewMonth(p.m);
    setDraftDate(`${p.y}-${pad2(p.m)}-${pad2(p.d)}`);
    setDraftHour(p.hh);
    setDraftMinute(p.mm);
    setHourInput(pad2(p.hh));
    setMinInput(pad2(p.mm));
  };

  const updatePosition = () => {
    const el = btnRef.current || wrapRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const vv = window.visualViewport;
    const vvTop = vv?.offsetTop ?? 0;
    const vpH = vv?.height ?? window.innerHeight;
    if (rect.bottom < vvTop - 40 || rect.top > vvTop + vpH + 40) {
      setOpen(false);
      setClosing(false);
      return;
    }
    const isMonth = mode === "month";
    const estH = popRef.current?.offsetHeight || (isMonth ? 252 : 368);
    const pos = computePortalMenuStyle(rect, {
      minW: isMonth ? 252 : 288,
      maxW: isMonth ? 276 : 308,
      maxH: isMonth ? 330 : 430,
      estH,
    });
    setPopStyle({
      ...pos.style,
      width: pos.style.minWidth,
      zIndex: 10050,
    });
    setFlipUp(pos.flipUp);
  };

  const closeAnimated = () => {
    if (!open || closing) return;
    setClosing(true);
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    closeTimerRef.current = window.setTimeout(() => {
      setOpen(false);
      setClosing(false);
    }, 130);
  };

  useEffect(() => {
    return () => {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    updatePosition();
    const raf = window.requestAnimationFrame(updatePosition);
    const onDocDown = (ev: MouseEvent) => {
      const target = ev.target as Node;
      if (wrapRef.current?.contains(target) || popRef.current?.contains(target)) return;
      closeAnimated();
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") {
        ev.stopPropagation();
        closeAnimated();
      }
    };
    const onRelayout = (ev: Event) => {
      if (ev.type === "scroll" && popRef.current && ev.target instanceof Node && popRef.current.contains(ev.target)) {
        return;
      }
      updatePosition();
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc, true);
    window.addEventListener("resize", onRelayout);
    window.addEventListener("scroll", onRelayout, true);
    vv?.addEventListener("resize", onRelayout);
    vv?.addEventListener("scroll", onRelayout);
    return () => {
      window.cancelAnimationFrame(raf);
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc, true);
      window.removeEventListener("resize", onRelayout);
      window.removeEventListener("scroll", onRelayout, true);
      vv?.removeEventListener("resize", onRelayout);
      vv?.removeEventListener("scroll", onRelayout);
    };
  }, [open, closing, mode]);

  const toggleOpen = () => {
    if (disabled) return;
    if (open && !closing) {
      closeAnimated();
      return;
    }
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    syncFromProp();
    setClosing(false);
    updatePosition();
    setOpen(true);
  };

  const shiftMonth = (delta: number) => {
    const d = new Date(viewYear, viewMonth - 1 + delta, 1);
    setViewYear(d.getFullYear());
    setViewMonth(d.getMonth() + 1);
  };

  const setClampedHour = (h: number) => {
    const next = ((h % 24) + 24) % 24;
    setDraftHour(next);
    setHourInput(pad2(next));
  };

  const setClampedMin = (m: number) => {
    const next = ((m % 60) + 60) % 60;
    setDraftMinute(next);
    setMinInput(pad2(next));
  };

  const commitDatetime = (overrideDate?: string, overrideH?: number, overrideM?: number) => {
    const dt = overrideDate ?? draftDate;
    const rawH = Number.isFinite(Number(hourInput)) ? Math.min(23, Math.max(0, Number(hourInput))) : draftHour;
    const rawM = Number.isFinite(Number(minInput)) ? Math.min(59, Math.max(0, Number(minInput))) : draftMinute;
    const hh = overrideH !== undefined ? overrideH : rawH;
    const mm = overrideM !== undefined ? overrideM : rawM;
    onChange(`${dt}T${pad2(hh)}:${pad2(mm)}`);
    closeAnimated();
  };

  const now = new Date();
  const nowY = now.getFullYear();
  const nowM = now.getMonth() + 1;
  const nowD = now.getDate();
  const todayYmd = `${nowY}-${pad2(nowM)}-${pad2(nowD)}`;
  const curYm = `${nowY}-${pad2(nowM)}`;

  const formattedText = parsed.valid
    ? mode === "month"
      ? `${parsed.y}-${pad2(parsed.m)}`
      : `${parsed.y}-${pad2(parsed.m)}-${pad2(parsed.d)} ${pad2(parsed.hh)}:${pad2(parsed.mm)}`
    : "";
  const placeholderText =
    placeholder || (mode === "month" ? t("dp.placeholder_month") : t("dp.placeholder_datetime"));

  const firstDow = new Date(viewYear, viewMonth - 1, 1).getDay();
  const dayCells =
    mode === "datetime"
      ? Array.from({ length: 42 }, (_, i) => {
          const cellDate = new Date(viewYear, viewMonth - 1, i - firstDow + 1);
          const cy = cellDate.getFullYear();
          const cm = cellDate.getMonth() + 1;
          const cd = cellDate.getDate();
          const ymd = `${cy}-${pad2(cm)}-${pad2(cd)}`;
          return { cy, cm, cd, ymd, outside: cm !== viewMonth };
        })
      : [];

  const portalPop =
    open && typeof document !== "undefined"
      ? createPortal(
          <div
            ref={popRef}
            class={`volo-cselect-menu is-portal volo-datepicker-pop ${flipUp ? "flip-up" : ""} ${closing ? "is-closing" : ""}`}
            style={popStyle}
            role="dialog"
            aria-label={ariaLabel || placeholderText}
          >
            {mode === "month" ? (
              <>
                <div class="volo-dp-head">
                  <button
                    type="button"
                    class="volo-dp-nav-btn"
                    onClick={() => setViewYear((y) => y - 1)}
                    aria-label="-1Y"
                  >
                    ‹
                  </button>
                  <button
                    type="button"
                    class="volo-dp-title-btn"
                    onClick={() => setViewYear(nowY)}
                  >
                    {t("dp.year_fmt", { year: viewYear })}
                  </button>
                  <button
                    type="button"
                    class="volo-dp-nav-btn"
                    onClick={() => setViewYear((y) => y + 1)}
                    aria-label="+1Y"
                  >
                    ›
                  </button>
                </div>

                <div class="volo-dp-month-grid">
                  {DP_MONTH_KEYS.map((mk, idx) => {
                    const mNum = idx + 1;
                    const ym = `${viewYear}-${pad2(mNum)}`;
                    const isSelected = parsed.valid && parsed.y === viewYear && parsed.m === mNum;
                    const isThisMonth = ym === curYm;
                    return (
                      <button
                        key={mk}
                        type="button"
                        class={`volo-dp-month-cell ${isSelected ? "is-selected" : ""} ${isThisMonth ? "is-today" : ""}`}
                        onClick={() => {
                          onChange(ym);
                          closeAnimated();
                        }}
                      >
                        {t(mk)}
                      </button>
                    );
                  })}
                </div>

                <div class="volo-dp-foot">
                  <button
                    type="button"
                    class="volo-dp-foot-link"
                    onClick={() => {
                      onChange(curYm);
                      closeAnimated();
                    }}
                  >
                    {t("dp.this_month")}
                  </button>
                  {clearable && (
                    <button
                      type="button"
                      class="volo-dp-foot-link is-muted"
                      onClick={() => {
                        onChange("");
                        closeAnimated();
                      }}
                    >
                      {t("dp.clear")}
                    </button>
                  )}
                </div>
              </>
            ) : (
              <>
                <div class="volo-dp-head">
                  <div class="volo-dp-nav-grp">
                    <button
                      type="button"
                      class="volo-dp-nav-btn"
                      onClick={() => setViewYear((y) => y - 1)}
                      aria-label="-1Y"
                    >
                      «
                    </button>
                    <button
                      type="button"
                      class="volo-dp-nav-btn"
                      onClick={() => shiftMonth(-1)}
                      aria-label="-1M"
                    >
                      ‹
                    </button>
                  </div>
                  <button
                    type="button"
                    class="volo-dp-title-btn"
                    onClick={() => {
                      setViewYear(nowY);
                      setViewMonth(nowM);
                    }}
                  >
                    {t("dp.ym_fmt", { year: viewYear, month: t(DP_MONTH_KEYS[viewMonth - 1]) })}
                  </button>
                  <div class="volo-dp-nav-grp">
                    <button
                      type="button"
                      class="volo-dp-nav-btn"
                      onClick={() => shiftMonth(1)}
                      aria-label="+1M"
                    >
                      ›
                    </button>
                    <button
                      type="button"
                      class="volo-dp-nav-btn"
                      onClick={() => setViewYear((y) => y + 1)}
                      aria-label="+1Y"
                    >
                      »
                    </button>
                  </div>
                </div>

                <div class="volo-dp-week-row">
                  {DP_WEEK_KEYS.map((wk) => (
                    <span key={wk} class="volo-dp-week-lbl">
                      {t(wk)}
                    </span>
                  ))}
                </div>

                <div class="volo-dp-day-grid">
                  {dayCells.map((cell) => {
                    const isSelected = cell.ymd === draftDate;
                    const isToday = cell.ymd === todayYmd;
                    return (
                      <button
                        key={cell.ymd}
                        type="button"
                        class={`volo-dp-day-cell ${cell.outside ? "is-outside" : ""} ${isToday ? "is-today" : ""} ${isSelected ? "is-selected" : ""}`}
                        onClick={() => {
                          setDraftDate(cell.ymd);
                          if (cell.outside) {
                            setViewYear(cell.cy);
                            setViewMonth(cell.cm);
                          }
                        }}
                        onDblClick={() => commitDatetime(cell.ymd)}
                      >
                        {cell.cd}
                      </button>
                    );
                  })}
                </div>

                <div class="volo-dp-time-bar">
                  <span class="volo-dp-time-label">
                    <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
                      <circle cx="12" cy="12" r="9" />
                      <polyline points="12 7 12 12 15.5 14" />
                    </svg>
                    {t("dp.time_lbl")}
                  </span>
                  <div class="volo-dp-time-controls">
                    <div
                      class="volo-dp-stepper"
                      onWheel={(e) => {
                        e.preventDefault();
                        setClampedHour(draftHour + (e.deltaY < 0 ? 1 : -1));
                      }}
                    >
                      <button type="button" class="volo-dp-step-btn" onClick={() => setClampedHour(draftHour - 1)} tabIndex={-1}>
                        ‹
                      </button>
                      <input
                        type="text"
                        inputMode="numeric"
                        maxLength={2}
                        class="volo-dp-time-in"
                        aria-label="HH"
                        value={hourInput}
                        onInput={(e) => {
                          const raw = (e.currentTarget as HTMLInputElement).value.replace(/\D/g, "").slice(0, 2);
                          setHourInput(raw);
                          if (raw !== "") {
                            const n = Math.min(23, Math.max(0, Number(raw)));
                            setDraftHour(n);
                          }
                        }}
                        onBlur={() => setClampedHour(hourInput === "" ? draftHour : Math.min(23, Math.max(0, Number(hourInput))))}
                        onKeyDown={(e) => {
                          if (e.key === "ArrowUp") {
                            e.preventDefault();
                            setClampedHour(draftHour + 1);
                          } else if (e.key === "ArrowDown") {
                            e.preventDefault();
                            setClampedHour(draftHour - 1);
                          } else if (e.key === "Enter") {
                            e.preventDefault();
                            commitDatetime();
                          }
                        }}
                      />
                      <button type="button" class="volo-dp-step-btn" onClick={() => setClampedHour(draftHour + 1)} tabIndex={-1}>
                        ›
                      </button>
                    </div>
                    <span class="volo-dp-time-sep">:</span>
                    <div
                      class="volo-dp-stepper"
                      onWheel={(e) => {
                        e.preventDefault();
                        setClampedMin(draftMinute + (e.deltaY < 0 ? 1 : -1));
                      }}
                    >
                      <button type="button" class="volo-dp-step-btn" onClick={() => setClampedMin(draftMinute - 1)} tabIndex={-1}>
                        ‹
                      </button>
                      <input
                        type="text"
                        inputMode="numeric"
                        maxLength={2}
                        class="volo-dp-time-in"
                        aria-label="MM"
                        value={minInput}
                        onInput={(e) => {
                          const raw = (e.currentTarget as HTMLInputElement).value.replace(/\D/g, "").slice(0, 2);
                          setMinInput(raw);
                          if (raw !== "") {
                            const n = Math.min(59, Math.max(0, Number(raw)));
                            setDraftMinute(n);
                          }
                        }}
                        onBlur={() => setClampedMin(minInput === "" ? draftMinute : Math.min(59, Math.max(0, Number(minInput))))}
                        onKeyDown={(e) => {
                          if (e.key === "ArrowUp") {
                            e.preventDefault();
                            setClampedMin(draftMinute + 1);
                          } else if (e.key === "ArrowDown") {
                            e.preventDefault();
                            setClampedMin(draftMinute - 1);
                          } else if (e.key === "Enter") {
                            e.preventDefault();
                            commitDatetime();
                          }
                        }}
                      />
                      <button type="button" class="volo-dp-step-btn" onClick={() => setClampedMin(draftMinute + 1)} tabIndex={-1}>
                        ›
                      </button>
                    </div>
                  </div>
                </div>

                <div class="volo-dp-foot">
                  <div class="volo-dp-foot-left">
                    <button
                      type="button"
                      class="volo-dp-foot-link"
                      onClick={() => {
                        const cur = new Date();
                        const ymd = `${cur.getFullYear()}-${pad2(cur.getMonth() + 1)}-${pad2(cur.getDate())}`;
                        commitDatetime(ymd, cur.getHours(), cur.getMinutes());
                      }}
                    >
                      {t("dp.now")}
                    </button>
                    {clearable && (
                      <button
                        type="button"
                        class="volo-dp-foot-link is-muted"
                        onClick={() => {
                          onChange("");
                          closeAnimated();
                        }}
                      >
                        {t("dp.clear")}
                      </button>
                    )}
                  </div>
                  <button
                    type="button"
                    class="volo-btn volo-btn-primary volo-btn-sm volo-dp-ok-btn"
                    onClick={() => commitDatetime()}
                  >
                    {t("common.confirm")}
                  </button>
                </div>
              </>
            )}
          </div>,
          document.body
        )
      : null;

  return (
    <div
      ref={wrapRef}
      class={`volo-cselect volo-datepicker ${open && !closing ? "is-open" : ""} ${disabled ? "is-disabled" : ""} ${className}`.trim()}
      style={style}
    >
      <button
        ref={btnRef}
        type="button"
        class="volo-cselect-btn volo-datepicker-btn"
        disabled={disabled}
        aria-label={ariaLabel}
        aria-haspopup="dialog"
        aria-expanded={open && !closing}
        onClick={toggleOpen}
        onKeyDown={(e) => {
          if (!disabled && (e.key === "Enter" || e.key === " " || e.key === "ArrowDown")) {
            e.preventDefault();
            toggleOpen();
          }
        }}
      >
        <span class="volo-dp-trigger-main">
          <svg
            class="volo-dp-cal-icon"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="1.85"
            stroke-linecap="round"
            stroke-linejoin="round"
            aria-hidden="true"
          >
            <rect x="3" y="4.5" width="18" height="16.5" rx="2.5" />
            <line x1="16" y1="2.5" x2="16" y2="6.5" />
            <line x1="8" y1="2.5" x2="8" y2="6.5" />
            <line x1="3" y1="9.5" x2="21" y2="9.5" />
          </svg>
          <span class={`volo-cselect-label ${formattedText ? "volo-dp-val" : "volo-dp-placeholder"}`}>
            {formattedText || placeholderText}
          </span>
        </span>
        <span class="volo-dp-trigger-right">
          {clearable && Boolean(formattedText) && !disabled && (
            <span
              role="button"
              tabIndex={0}
              class="volo-dp-clear"
              title={t("dp.clear")}
              aria-label={t("dp.clear")}
              onClick={(e) => {
                e.stopPropagation();
                onChange("");
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  e.stopPropagation();
                  onChange("");
                }
              }}
            >
              ×
            </span>
          )}
          <svg
            class="volo-cselect-chev"
            viewBox="0 0 20 20"
            fill="none"
            stroke="currentColor"
            stroke-width="1.9"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <polyline points="5.5 7.5 10 12.5 14.5 7.5" />
          </svg>
        </span>
      </button>
      {portalPop}
    </div>
  );
}

export interface ActionMenuItem {
  label: string;
  onClick?: () => void;
  href?: string;
  target?: string;
  download?: string;
  danger?: boolean;
  disabled?: boolean;
  hidden?: boolean;
}

export function ActionMenu({
  items,
  label,
}: {
  items: ActionMenuItem[];
  label?: string;
}) {
  const visibleItems = items.filter((it) => !it.hidden);
  const [open, setOpen] = useState(false);
  const [closing, setClosing] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [menuStyle, setMenuStyle] = useState<CSSProperties>({});
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);

  const updatePos = () => {
    if (!btnRef.current) return;
    const rect = btnRef.current.getBoundingClientRect();
    const vv = window.visualViewport;
    const vvTop = vv?.offsetTop ?? 0;
    const vpH = vv?.height ?? window.innerHeight;
    if (rect.bottom < vvTop - 40 || rect.top > vvTop + vpH + 40) {
      setOpen(false);
      setClosing(false);
      return;
    }
    const estH = menuRef.current?.offsetHeight || visibleItems.length * 36 + 12;
    const pos = computePortalMenuStyle(rect, { minW: 152, maxW: 260, maxH: 280, estH, align: "right" });
    setMenuStyle(pos.style);
    setFlipUp(pos.flipUp);
  };

  const closeAnimated = () => {
    if (!open || closing) return;
    setClosing(true);
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    closeTimerRef.current = window.setTimeout(() => {
      setOpen(false);
      setClosing(false);
    }, 130);
  };

  useEffect(() => {
    return () => {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDocDown = (ev: MouseEvent) => {
      const target = ev.target as Node;
      if (btnRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      closeAnimated();
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") closeAnimated();
    };
    const onScrollOrResize = (ev: Event) => {
      if (ev.type === "scroll" && menuRef.current && ev.target instanceof Node && menuRef.current.contains(ev.target)) {
        return;
      }
      updatePos();
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", onScrollOrResize);
    window.addEventListener("scroll", onScrollOrResize, true);
    vv?.addEventListener("resize", onScrollOrResize);
    vv?.addEventListener("scroll", onScrollOrResize);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", onScrollOrResize);
      window.removeEventListener("scroll", onScrollOrResize, true);
      vv?.removeEventListener("resize", onScrollOrResize);
      vv?.removeEventListener("scroll", onScrollOrResize);
    };
  }, [open, closing]);

  if (visibleItems.length === 0) return null;

  const toggle = () => {
    if (open && !closing) {
      closeAnimated();
    } else {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
      setClosing(false);
      updatePos();
      setOpen(true);
    }
  };

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        class={`volo-act-btn volo-act-more ${open && !closing ? "active" : ""}`.trim()}
        aria-haspopup="menu"
        aria-expanded={open && !closing}
        aria-label={label || t("ui.more_actions")}
        title={label || t("ui.more_actions")}
        onClick={toggle}
      >
        ⋯
      </button>
      {open &&
        typeof document !== "undefined" &&
        createPortal(
          <div
            ref={menuRef}
            role="menu"
            class={`volo-cselect-menu is-portal volo-action-menu ${flipUp ? "flip-up" : ""} ${closing ? "is-closing" : ""}`}
            style={menuStyle}
          >
            {visibleItems.map((it, idx) =>
              it.href ? (
                <a
                  key={`${it.label}-${idx}`}
                  role="menuitem"
                  href={it.href}
                  target={it.target}
                  rel={it.target === "_blank" ? "noreferrer" : undefined}
                  download={it.download}
                  class={`volo-cselect-item ${it.danger ? "is-danger" : ""}`}
                  onClick={() => closeAnimated()}
                >
                  <span class="volo-cselect-item-text">{it.label}</span>
                </a>
              ) : (
                <button
                  key={`${it.label}-${idx}`}
                  type="button"
                  role="menuitem"
                  disabled={it.disabled}
                  class={`volo-cselect-item ${it.danger ? "is-danger" : ""}`}
                  onClick={() => {
                    closeAnimated();
                    it.onClick?.();
                  }}
                >
                  <span class="volo-cselect-item-text">{it.label}</span>
                </button>
              )
            )}
          </div>,
          document.body
        )}
    </>
  );
}

export interface LocalePopoverProps {
  value: string;
  locales: LocaleOption[];
  onChange: (nextCode: string) => void;
  pill?: boolean;
}

export function LocalePopover({ value, locales, onChange, pill = true }: LocalePopoverProps) {
  const [open, setOpen] = useState(false);
  const [closing, setClosing] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [menuStyle, setMenuStyle] = useState<CSSProperties>({});
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const menuRef = useRef<HTMLDivElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);
  const current = locales.find((l) => l.code === value) || locales[0];

  const updatePos = () => {
    const el = btnRef.current || wrapRef.current;
    if (!el) return;
    const rect = el.getBoundingClientRect();
    const estH = menuRef.current?.offsetHeight || Math.max(1, locales.length) * 36 + 12;
    const pos = computePortalMenuStyle(rect, { minW: 196, maxW: 280, maxH: 280, estH, align: "right" });
    setMenuStyle(pos.style);
    setFlipUp(pos.flipUp);
  };

  const closeAnimated = () => {
    if (!open || closing) return;
    setClosing(true);
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    closeTimerRef.current = window.setTimeout(() => {
      setOpen(false);
      setClosing(false);
    }, 140);
  };

  useEffect(() => {
    return () => {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    const onDocDown = (ev: MouseEvent) => {
      const target = ev.target as Node;
      if (wrapRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      closeAnimated();
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") closeAnimated();
    };
    const onScrollOrResize = (ev: Event) => {
      if (ev.type === "scroll" && menuRef.current && ev.target instanceof Node && menuRef.current.contains(ev.target)) {
        return;
      }
      updatePos();
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", onScrollOrResize);
    window.addEventListener("scroll", onScrollOrResize, true);
    vv?.addEventListener("resize", onScrollOrResize);
    vv?.addEventListener("scroll", onScrollOrResize);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", onScrollOrResize);
      window.removeEventListener("scroll", onScrollOrResize, true);
      vv?.removeEventListener("resize", onScrollOrResize);
      vv?.removeEventListener("scroll", onScrollOrResize);
    };
  }, [open, closing]);

  const toggle = () => {
    if (open && !closing) {
      closeAnimated();
    } else {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
      setClosing(false);
      updatePos();
      setOpen(true);
    }
  };

  return (
    <div ref={wrapRef} class={`volo-cselect volo-locale-pop ${open && !closing ? "is-open" : ""}`}>
      <button
        ref={btnRef}
        type="button"
        class={pill ? "volo-locale-trigger" : "volo-cselect-btn"}
        title={t("shell.lang_title")}
        aria-haspopup="listbox"
        aria-expanded={open && !closing}
        onClick={toggle}
      >
        <svg
          width="15"
          height="15"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          stroke-width="1.85"
          stroke-linecap="round"
          stroke-linejoin="round"
          style={{ flexShrink: 0 }}
        >
          <circle cx="12" cy="12" r="9.5" />
          <path d="M2.5 12h19" />
          <path d="M12 2.5a14.5 14.5 0 0 1 0 19 14.5 14.5 0 0 1 0-19" />
        </svg>
        <span class="volo-locale-trigger-text">{current ? current.name : value}</span>
        <svg
          class="volo-cselect-chev"
          viewBox="0 0 20 20"
          fill="none"
          stroke="currentColor"
          stroke-width="1.9"
          stroke-linecap="round"
          stroke-linejoin="round"
        >
          <polyline points="5.5 7.5 10 12.5 14.5 7.5" />
        </svg>
      </button>

      {open &&
        typeof document !== "undefined" &&
        createPortal(
          <div
            ref={menuRef}
            class={`volo-cselect-menu is-portal volo-locale-menu ${flipUp ? "flip-up" : ""} ${closing ? "is-closing" : ""}`}
            style={menuStyle}
            role="listbox"
          >
            <div class="volo-pop-caption">{t("shell.lang_title")}</div>
            {locales.map((loc) => {
              const isAct = loc.code === value;
              return (
                <button
                  key={loc.code}
                  type="button"
                  role="option"
                  aria-selected={isAct}
                  class={`volo-cselect-item ${isAct ? "active" : ""}`}
                  onClick={() => {
                    onChange(loc.code);
                    closeAnimated();
                  }}
                >
                  <span class="volo-cselect-item-text">{loc.name}</span>
                  <span class="volo-cselect-item-right">
                    <span class="volo-code-chip" style={{ fontSize: "10.5px", padding: "1px 5px" }}>
                      {loc.code}
                    </span>
                    {isAct && (
                      <svg
                        class="volo-cselect-check"
                        viewBox="0 0 20 20"
                        fill="none"
                        stroke="currentColor"
                        stroke-width="2.2"
                        stroke-linecap="round"
                        stroke-linejoin="round"
                      >
                        <polyline points="4.5 10.5 8.5 14.5 15.5 6.5" />
                      </svg>
                    )}
                  </span>
                </button>
              );
            })}
          </div>,
          document.body
        )}
    </div>
  );
}

let modalSeq = 0;
const openModalStack: number[] = [];

export interface AnimatedModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  subtitle?: string;
  maxWidth?: string;
  children: ComponentChildren;
  footer?: ComponentChildren;
}

export function AnimatedModal({
  open,
  onClose,
  title,
  subtitle,
  maxWidth = "640px",
  children,
  footer,
}: AnimatedModalProps) {
  const [rendered, setRendered] = useState(open);
  const [closing, setClosing] = useState(false);
  const [stackDepth, setStackDepth] = useState(0);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    if (open) {
      setRendered(true);
      setClosing(false);
    } else if (rendered) {
      setClosing(true);
      const timer = window.setTimeout(() => {
        setRendered(false);
        setClosing(false);
      }, 170);
      return () => window.clearTimeout(timer);
    }
  }, [open]);

  useEffect(() => {
    if (!rendered) return;
    const id = ++modalSeq;
    const depth = openModalStack.length;
    openModalStack.push(id);
    setStackDepth(depth);
    acquireModalScrollLock();
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (openModalStack[openModalStack.length - 1] !== id) return;
      if (typeof document !== "undefined" && document.querySelector(".volo-cselect-menu.is-portal:not(.is-closing)")) {
        return;
      }
      e.stopPropagation();
      onCloseRef.current();
    };
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("keydown", onEsc);
      const idx = openModalStack.indexOf(id);
      if (idx >= 0) openModalStack.splice(idx, 1);
      releaseModalScrollLock();
    };
  }, [rendered]);

  if (!rendered || typeof document === "undefined") return null;

  return createPortal(
    <div
      class={`volo-modal-backdrop ${closing ? "is-closing" : ""}`}
      style={{ zIndex: 1000 + stackDepth * 20 }}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        class={`volo-modal ${closing ? "is-closing" : ""}`}
        style={{ maxWidth }}
        role="dialog"
        aria-modal="true"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div class="volo-modal-head">
          <div style={{ minWidth: 0 }}>
            <h3 class="volo-card-title" style={{ fontSize: "16.5px" }}>{title}</h3>
            {subtitle && <p class="volo-sec-desc" style={{ marginTop: "3px" }}>{subtitle}</p>}
          </div>
          <button
            type="button"
            class="volo-modal-close"
            aria-label={t("common.close")}
            title={t("common.close")}
            onClick={onClose}
          >
            ×
          </button>
        </div>
        <div class="volo-modal-body">{children}</div>
        {footer && <div class="volo-form-actions">{footer}</div>}
      </div>
    </div>,
    document.body
  );
}

export function UploadProgressBar({
  progress,
  label,
}: {
  progress: number | null;
  label?: string;
}) {
  if (progress === null || progress === undefined) return null;
  const pct = Math.max(0, Math.min(100, Math.round(progress)));
  return (
    <div class="volo-upload-progress" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
      <div class="volo-upload-progress-head">
        <span>{label || t("ed.uploading")}</span>
        <span class="volo-code-chip">{pct}%</span>
      </div>
      <div class="volo-upload-progress-track">
        <div class="volo-upload-progress-fill" style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

export interface SwitchToggleProps {
  checked: boolean;
  onChange: (next: boolean) => void;
  label?: ComponentChildren;
  hint?: string;
  disabled?: boolean;
  className?: string;
}

export function SwitchToggle({
  checked,
  onChange,
  label,
  hint,
  disabled = false,
  className = "",
}: SwitchToggleProps) {
  return (
    <label
      class={`volo-switch-wrap ${disabled ? "is-disabled" : ""} ${className}`.trim()}
      onClick={(e) => {
        if (disabled) {
          e.preventDefault();
          return;
        }
        e.preventDefault();
        onChange(!checked);
      }}
    >
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        disabled={disabled}
        class={`volo-switch ${checked ? "is-on" : ""}`}
        onClick={(e) => {
          e.stopPropagation();
          if (!disabled) onChange(!checked);
        }}
      >
        <span class="volo-switch-thumb" />
      </button>
      {label && (
        <span class="volo-switch-copy">
          <span class="volo-switch-label">{label}</span>
          {hint && <small class="volo-help">{hint}</small>}
        </span>
      )}
    </label>
  );
}

export interface UploadButtonProps {
  accept?: string;
  onFile?: (file: File) => void;
  onFiles?: (files: File[]) => void;
  multiple?: boolean;
  busy?: boolean;
  disabled?: boolean;
  label: string;
  busyLabel?: string;
  primary?: boolean;
  small?: boolean;
  className?: string;
}

export function UploadButton({
  accept,
  onFile,
  onFiles,
  multiple = false,
  busy = false,
  disabled = false,
  label,
  busyLabel,
  primary = false,
  small = true,
  className = "",
}: UploadButtonProps) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [dragging, setDragging] = useState(false);
  const isDisabled = busy || disabled;

  const dispatchFiles = (list: FileList | null | undefined) => {
    const arr = list ? Array.from(list) : [];
    if (arr.length === 0) return;
    if (onFiles) {
      onFiles(multiple ? arr : arr.slice(0, 1));
    } else if (onFile) {
      if (multiple) {
        for (const f of arr) onFile(f);
      } else {
        onFile(arr[0]);
      }
    }
  };

  return (
    <>
      <button
        type="button"
        disabled={isDisabled}
        class={`volo-btn ${primary ? "volo-btn-primary" : ""} ${small ? "volo-btn-sm" : ""} ${dragging ? "is-dragover" : ""} ${className}`.trim()}
        onClick={() => inputRef.current?.click()}
        onDragOver={(e) => {
          e.preventDefault();
          if (!isDisabled) setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragging(false);
          if (isDisabled) return;
          dispatchFiles(e.dataTransfer?.files);
        }}
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
          <polyline points="17 8 12 3 7 8" />
          <line x1="12" y1="3" x2="12" y2="15" />
        </svg>
        <span>{busy ? busyLabel || t("ed.uploading") : label}</span>
      </button>
      <input
        ref={inputRef}
        type="file"
        accept={accept}
        multiple={multiple}
        style={{ display: "none" }}
        disabled={isDisabled}
        onChange={(e) => {
          dispatchFiles((e.currentTarget as HTMLInputElement).files);
          (e.currentTarget as HTMLInputElement).value = "";
        }}
      />
    </>
  );
}

export function MobileSelectAllBar({
  checked,
  count,
  total,
  onToggle,
}: {
  checked: boolean;
  count: number;
  total: number;
  onToggle: (next: boolean) => void;
}) {
  if (total <= 0) return null;
  return (
    <div class="volo-mobile-select-bar">
      <label class="volo-mobile-select-lbl">
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onToggle((e.currentTarget as HTMLInputElement).checked)}
        />
        <span>{t("ui.select_all_page")}</span>
      </label>
      {count > 0 && <span class="volo-mini-tag">{t("ui.batch_selected", { n: count })}</span>}
    </div>
  );
}

export interface BatchBarProps {
  count: number;
  onClear: () => void;
  children: ComponentChildren;
}

export function BatchBar({ count, onClear, children }: BatchBarProps) {
  if (count <= 0) return null;
  return (
    <div class="volo-batch-bar" role="region" aria-label={t("ui.batch_label")}>
      <div class="volo-batch-count">
        <span class="volo-batch-num">{count}</span>
        <span>{t("ui.batch_selected", { n: count })}</span>
      </div>
      <div class="volo-batch-actions">
        {children}
        <button type="button" class="volo-act-btn" onClick={onClear}>
          {t("ui.batch_clear")}
        </button>
      </div>
    </div>
  );
}

export function ConsoleEmpty({
  text,
  hint,
  colSpan,
}: {
  text?: string;
  hint?: string;
  colSpan?: number;
}) {
  const inner = (
    <div class="volo-empty-box">
      <svg width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
        <rect x="3" y="4" width="18" height="16" rx="2.5" />
        <path d="M3 14h5l2 2.5h4L16 14h5" />
      </svg>
      <div class="volo-empty-title">{text || t("tbl.empty")}</div>
      {hint && <div class="volo-help">{hint}</div>}
    </div>
  );
  if (colSpan) {
    return (
      <tr>
        <td colSpan={colSpan} class="volo-empty-state">
          {inner}
        </td>
      </tr>
    );
  }
  return <div class="volo-empty-state">{inner}</div>;
}

export function TableSkeleton({ cols = 5, rows = 4 }: { cols?: number; rows?: number }) {
  return (
    <>
      {Array.from({ length: rows }).map((_, rIdx) => (
        <tr key={rIdx} class="volo-skel-tr">
          {Array.from({ length: cols }).map((__, cIdx) => (
            <td key={cIdx}>
              <div class={`volo-skel-bar ${cIdx === 0 ? "is-wide" : ""}`} />
            </td>
          ))}
        </tr>
      ))}
    </>
  );
}

export function GlobalConfirmModal() {
  const cur = confirmSig.value;
  return (
    <AnimatedModal
      open={Boolean(cur)}
      onClose={() => resolveConfirm(false)}
      title={cur?.title || t("ui.confirm_title")}
      maxWidth="450px"
      footer={
        <>
          <button type="button" class="volo-btn" onClick={() => resolveConfirm(false)}>
            {t("common.cancel")}
          </button>
          <button
            type="button"
            class={`volo-btn ${cur?.danger !== false ? "volo-btn-danger" : "volo-btn-primary"}`}
            onClick={() => resolveConfirm(true)}
          >
            {cur?.confirmText || t("ui.confirm_ok")}
          </button>
        </>
      }
    >
      <p style={{ margin: "2px 0 8px", fontSize: "13.5px", lineHeight: 1.6, color: "var(--volo-text)" }}>
        {cur?.message || ""}
      </p>
      {cur?.items && cur.items.length > 0 && (
        <div class="volo-confirm-list">
          {cur.items.slice(0, 60).map((it, i) => (
            <div key={i} class="volo-confirm-list-item">
              {it}
            </div>
          ))}
          {cur.items.length > 60 && (
            <div class="volo-help" style={{ padding: "4px 6px" }}>
              +{cur.items.length - 60}…
            </div>
          )}
        </div>
      )}
    </AnimatedModal>
  );
}

export interface NavIconPreviewProps {
  icon?: string;
  fallbackLabel?: string;
  size?: number;
}

export function NavIconPreview({ icon, fallbackLabel, size = 16 }: NavIconPreviewProps) {
  const [imgErr, setImgErr] = useState(false);
  const raw = (icon || "").trim();
  const low = raw.toLowerCase();

  useEffect(() => {
    setImgErr(false);
  }, [raw]);

  const boxSize = Math.max(26, size + 12);

  if (
    low === "github" ||
    low.includes("github-icon") ||
    low === "https://github.com/favicon.ico"
  ) {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor">
          <path d="M12 .5C5.65.5.5 5.65.5 12c0 5.09 3.29 9.4 7.86 10.92.58.11.79-.25.79-.56 0-.28-.01-1.02-.02-2-3.2.7-3.88-1.54-3.88-1.54-.52-1.33-1.28-1.69-1.28-1.69-1.05-.72.08-.71.08-.71 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.7 1.26 3.36.96.1-.75.4-1.26.73-1.55-2.55-.29-5.24-1.28-5.24-5.68 0-1.26.45-2.28 1.18-3.09-.12-.29-.51-1.46.11-3.05 0 0 .97-.31 3.17 1.18a11.02 11.02 0 0 1 5.78 0c2.2-1.49 3.16-1.18 3.16-1.18.63 1.59.24 2.76.12 3.05.74.81 1.18 1.83 1.18 3.09 0 4.41-2.69 5.38-5.26 5.67.41.36.78 1.06.78 2.14 0 1.55-.01 2.79-.01 3.17 0 .31.21.67.8.56A11.51 11.51 0 0 0 23.5 12C23.5 5.65 18.35.5 12 .5Z" />
        </svg>
      </span>
    );
  }

  if (low === "admin" || low.includes("admin-panel")) {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="7" height="9" rx="1.5" />
          <rect x="14" y="3" width="7" height="5" rx="1.5" />
          <rect x="14" y="12" width="7" height="9" rx="1.5" />
          <rect x="3" y="16" width="7" height="5" rx="1.5" />
        </svg>
      </span>
    );
  }

  if (low === "logout" || low.includes("admin-logout")) {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
          <polyline points="16 17 21 12 16 7" />
          <line x1="21" y1="12" x2="9" y2="12" />
        </svg>
      </span>
    );
  }

  if (low === "book" || low === "books") {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M2 4h7a3 3 0 0 1 3 3v13a2 2 0 0 0-2-2H2z" />
          <path d="M22 4h-7a3 3 0 0 0-3 3v13a2 2 0 0 1 2-2h8z" />
        </svg>
      </span>
    );
  }

  if (low === "rss" || low === "feed") {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 11a9 9 0 0 1 9 9" />
          <path d="M4 4a16 16 0 0 1 16 16" />
          <circle cx="5" cy="19" r="1.5" fill="currentColor" />
        </svg>
      </span>
    );
  }

  if (low === "mail" || low === "email") {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="5" width="18" height="14" rx="2" />
          <path d="m3 7 9 6 9-6" />
        </svg>
      </span>
    );
  }

  if (low === "external") {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M14 4h6v6" />
          <path d="M10 14 20 4" />
          <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
        </svg>
      </span>
    );
  }

  if (low === "link") {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
          <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
        </svg>
      </span>
    );
  }

  if (!imgErr && (raw.startsWith("/") || raw.startsWith("http://") || raw.startsWith("https://") || raw.startsWith("data:"))) {
    return (
      <span class="volo-icon-preview-box" style={{ width: `${boxSize}px`, height: `${boxSize}px` }} title={raw}>
        <img
          src={raw}
          alt=""
          style={{ width: `${size + 2}px`, height: `${size + 2}px`, objectFit: "contain", borderRadius: "4px" }}
          onError={() => setImgErr(true)}
        />
      </span>
    );
  }

  const ch = (fallbackLabel || "").trim().charAt(0).toUpperCase();
  return (
    <span
      class="volo-icon-preview-box is-fallback"
      style={{ width: `${boxSize}px`, height: `${boxSize}px` }}
      title={raw || fallbackLabel || ""}
    >
      {ch ? (
        <span style={{ fontSize: "11.5px", fontWeight: 700 }}>{ch}</span>
      ) : (
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="9" />
          <path d="M3.6 9h16.8M3.6 15h16.8" />
          <path d="M12 3a14 14 0 0 1 0 18 14 14 0 0 1 0-18" />
        </svg>
      )}
    </span>
  );
}

export function paginateItems<T>(
  items: T[],
  page: number,
  pageSize = 15
): { slice: T[]; safePage: number; totalPages: number } {
  const totalPages = Math.max(1, Math.ceil(items.length / pageSize));
  const safePage = Math.min(Math.max(1, page), totalPages);
  const start = (safePage - 1) * pageSize;
  return {
    slice: items.slice(start, start + pageSize),
    safePage,
    totalPages,
  };
}

export interface PaginationProps {
  total: number;
  page: number;
  pageSize?: number;
  pageSizeOptions?: number[];
  onPageSizeChange?: (nextSize: number) => void;
  onChange: (nextPage: number) => void;
}

export function Pagination({
  total,
  page,
  pageSize = 20,
  pageSizeOptions = [20, 50, 100],
  onPageSizeChange,
  onChange,
}: PaginationProps) {
  if (total <= 0) return null;
  const minOpt = pageSizeOptions.length > 0 ? Math.min(...pageSizeOptions) : pageSize;
  const showSizePicker = Boolean(onPageSizeChange) && (total > minOpt || pageSize !== pageSizeOptions[0]);
  if (!onPageSizeChange && total <= pageSize) return null;

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const safePage = Math.min(Math.max(1, page), totalPages);

  const buildPages = (): Array<number | "..."> => {
    if (totalPages <= 7) {
      return Array.from({ length: totalPages }, (_, i) => i + 1);
    }
    const out: Array<number | "..."> = [1];
    const left = Math.max(2, safePage - 1);
    const right = Math.min(totalPages - 1, safePage + 1);
    if (left > 2) out.push("...");
    for (let p = left; p <= right; p++) out.push(p);
    if (right < totalPages - 1) out.push("...");
    out.push(totalPages);
    return out;
  };

  return (
    <div class="volo-pagination">
      <div class="volo-pagination-left">
        <span class="volo-mini-tag">{t("ui.pg_total", { total })}</span>
        {totalPages > 1 && (
          <span class="volo-pagination-info">
            {t("ui.pg_summary", { total, page: safePage, pages: totalPages })}
          </span>
        )}
        {showSizePicker && onPageSizeChange && (
          <ConsoleSelect
            className="volo-pg-size-select"
            searchable={false}
            value={String(pageSize)}
            options={pageSizeOptions.map((sz) => ({
              value: String(sz),
              label: t("ui.pg_per_page", { n: sz }),
            }))}
            onChange={(v) => onPageSizeChange(Number(v) || 20)}
          />
        )}
      </div>
      {totalPages > 1 && (
        <div class="volo-pagination-btns">
          <button
            type="button"
            class="volo-pg-btn"
            disabled={safePage <= 1}
            onClick={() => onChange(safePage - 1)}
          >
            {t("ui.pg_prev")}
          </button>
          {buildPages().map((p, idx) =>
            p === "..." ? (
              <span key={`dots-${idx}`} class="volo-pg-dots">
                …
              </span>
            ) : (
              <button
                key={p}
                type="button"
                class={`volo-pg-btn ${p === safePage ? "active" : ""}`}
                onClick={() => onChange(p)}
              >
                {p}
              </button>
            )
          )}
          <button
            type="button"
            class="volo-pg-btn"
            disabled={safePage >= totalPages}
            onClick={() => onChange(safePage + 1)}
          >
            {t("ui.pg_next")}
          </button>
        </div>
      )}
    </div>
  );
}

export const PaginationBar = Pagination;

let modalLockCount = 0;
let savedBodyOverflow = "";
let savedBodyPaddingRight = "";

function acquireModalScrollLock() {
  if (typeof window === "undefined" || typeof document === "undefined") return;
  if (modalLockCount === 0) {
    savedBodyOverflow = document.body.style.overflow;
    savedBodyPaddingRight = document.body.style.paddingRight;
    const sbw = Math.max(0, window.innerWidth - document.documentElement.clientWidth);
    const hasGutter =
      typeof CSS !== "undefined" &&
      typeof CSS.supports === "function" &&
      CSS.supports("scrollbar-gutter: stable");
    if (sbw > 0 && !hasGutter) {
      const basePad = parseFloat(window.getComputedStyle(document.body).paddingRight || "0") || 0;
      document.body.style.paddingRight = `${basePad + sbw}px`;
    }
    document.body.style.overflow = "hidden";
  }
  modalLockCount += 1;
}

function releaseModalScrollLock() {
  if (typeof document === "undefined" || modalLockCount <= 0) return;
  modalLockCount -= 1;
  if (modalLockCount === 0) {
    document.body.style.overflow = savedBodyOverflow;
    document.body.style.paddingRight = savedBodyPaddingRight;
  }
}

export function TokenListInput({
  values,
  onChange,
  placeholder,
  uppercase = false,
}: {
  values: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  uppercase?: boolean;
}) {
  const [draft, setDraft] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);
  const safeValues = Array.isArray(values) ? values.filter(Boolean) : [];

  const commitTokens = (raw: string) => {
    const parts = raw
      .split(/[,，;；\r\n]+/)
      .map((s) => (uppercase ? s.trim().toUpperCase() : s.trim()))
      .filter(Boolean);
    if (parts.length === 0) return;
    const seen = new Set(safeValues.map((v) => (uppercase ? v.toUpperCase() : v)));
    const next = [...safeValues];
    for (const p of parts) {
      const key = uppercase ? p.toUpperCase() : p;
      if (!seen.has(key)) {
        seen.add(key);
        next.push(p);
      }
    }
    onChange(next);
  };

  return (
    <div>
      {safeValues.length > 0 && (
        <div class="volo-token-head">
          <span>{t("ui.token_count", { count: safeValues.length })}</span>
          <button
            type="button"
            class="volo-act-btn danger"
            onClick={() => onChange([])}
          >
            {t("ui.token_clear")}
          </button>
        </div>
      )}
      <div
        class="volo-tag-capsule-box volo-token-box"
        onClick={() => inputRef.current?.focus()}
      >
        {safeValues.map((item, idx) => (
          <span key={`${item}-${idx}`} class="volo-tag-pill">
            <span>{item}</span>
            <button
              type="button"
              aria-label={t("common.delete")}
              onClick={(e) => {
                e.stopPropagation();
                onChange(safeValues.filter((_, i) => i !== idx));
              }}
            >
              ×
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          type="text"
          class="volo-tag-inline-input"
          value={draft}
          placeholder={placeholder || t("ui.token_placeholder")}
          onInput={(e) => {
            const val = (e.currentTarget as HTMLInputElement).value;
            if (/[,，;；\n]/.test(val)) {
              commitTokens(val);
              setDraft("");
            } else {
              setDraft(val);
            }
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              if (draft.trim()) {
                e.preventDefault();
                commitTokens(draft);
                setDraft("");
              }
            } else if (e.key === "Backspace" && !draft && safeValues.length > 0) {
              onChange(safeValues.slice(0, -1));
            }
          }}
          onPaste={(e) => {
            const text = e.clipboardData?.getData("text") || "";
            if (/[,，;；\r\n]/.test(text)) {
              e.preventDefault();
              commitTokens(text);
              setDraft("");
            }
          }}
          onBlur={() => {
            if (draft.trim()) {
              commitTokens(draft);
              setDraft("");
            }
          }}
        />
      </div>
    </div>
  );
}
