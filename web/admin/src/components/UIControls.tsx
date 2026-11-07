import { useEffect, useRef, useState } from "preact/hooks";
import type { JSX } from "preact";
import type { LocaleOption } from "../i18n";
import { t } from "../i18n";

export interface SelectOption {
  value: string;
  label: string;
  badge?: string;
}

export interface ConsoleSelectProps {
  value: string;
  options: SelectOption[];
  onChange: (nextValue: string) => void;
  style?: JSX.CSSProperties;
  className?: string;
  disabled?: boolean;
  align?: "left" | "right";
}

export function ConsoleSelect({
  value,
  options,
  onChange,
  style,
  className = "",
  disabled = false,
  align = "left",
}: ConsoleSelectProps) {
  const [open, setOpen] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [focusIdx, setFocusIdx] = useState<number>(-1);
  const wrapRef = useRef<HTMLDivElement | null>(null);

  const selected = options.find((o) => o.value === value) || options[0];

  useEffect(() => {
    if (!open) return;
    const onDocDown = (ev: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(ev.target as Node)) {
        setOpen(false);
      }
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  const toggleOpen = () => {
    if (disabled) return;
    if (!open && wrapRef.current) {
      const rect = wrapRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - rect.bottom;
      setFlipUp(spaceBelow < 230 && rect.top > 220);
      const curIdx = options.findIndex((o) => o.value === value);
      setFocusIdx(curIdx >= 0 ? curIdx : 0);
    }
    setOpen((prev) => !prev);
  };

  const handleKeyDown = (e: KeyboardEvent) => {
    if (disabled) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      if (!open) {
        toggleOpen();
      } else if (focusIdx >= 0 && focusIdx < options.length) {
        onChange(options[focusIdx].value);
        setOpen(false);
      }
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!open) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i + 1) % Math.max(1, options.length));
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!open) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i - 1 + options.length) % Math.max(1, options.length));
      }
    }
  };

  return (
    <div
      ref={wrapRef}
      class={`volo-cselect ${open ? "is-open" : ""} ${disabled ? "is-disabled" : ""} ${className}`.trim()}
      style={style}
    >
      <button
        type="button"
        class="volo-cselect-btn"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={toggleOpen}
        onKeyDown={handleKeyDown}
      >
        <span class="volo-cselect-label">{selected ? selected.label : value}</span>
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

      {open && (
        <div
          class={`volo-cselect-menu ${flipUp ? "flip-up" : ""} ${align === "right" ? "align-right" : ""}`}
          role="listbox"
        >
          {options.map((opt, idx) => {
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
                  setOpen(false);
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
          })}
        </div>
      )}
    </div>
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
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const current = locales.find((l) => l.code === value) || locales[0];

  useEffect(() => {
    if (!open) return;
    const onDocDown = (ev: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(ev.target as Node)) {
        setOpen(false);
      }
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  return (
    <div ref={wrapRef} class={`volo-cselect volo-locale-pop ${open ? "is-open" : ""}`}>
      <button
        type="button"
        class={pill ? "volo-locale-trigger" : "volo-cselect-btn"}
        title={t("shell.lang_title")}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen((p) => !p)}
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

      {open && (
        <div class="volo-cselect-menu align-right volo-locale-menu" role="listbox">
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
                  setOpen(false);
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
        </div>
      )}
    </div>
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
  onChange: (nextPage: number) => void;
}

export function Pagination({ total, page, pageSize = 15, onChange }: PaginationProps) {
  if (total <= pageSize) return null;
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
      <span class="volo-pagination-info">
        {t("ui.pg_summary", { total, page: safePage, pages: totalPages })}
      </span>
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
    </div>
  );
}
