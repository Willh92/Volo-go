import { useEffect, useRef, useState } from "preact/hooks";
import type { CSSProperties, ComponentChildren, JSX } from "preact";
import { confirmSig, resolveConfirm } from "../api";
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
  style?: CSSProperties;
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
  const [closing, setClosing] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const [autoRight, setAutoRight] = useState(align === "right");
  const [focusIdx, setFocusIdx] = useState<number>(-1);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);

  const selected = options.find((o) => o.value === value) || options[0];

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
      if (wrapRef.current && !wrapRef.current.contains(ev.target as Node)) {
        closeAnimated();
      }
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") closeAnimated();
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open, closing]);

  const toggleOpen = () => {
    if (disabled) return;
    if (open && !closing) {
      closeAnimated();
      return;
    }
    if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
    setClosing(false);
    if (wrapRef.current) {
      const rect = wrapRef.current.getBoundingClientRect();
      const spaceBelow = window.innerHeight - rect.bottom;
      setFlipUp(spaceBelow < 230 && rect.top > 220);
      setAutoRight(align === "right" || (rect.left > window.innerWidth - 220 && rect.right > 180));
      const curIdx = options.findIndex((o) => o.value === value);
      setFocusIdx(curIdx >= 0 ? curIdx : 0);
    }
    setOpen(true);
  };

  const handleKeyDown = (e: KeyboardEvent) => {
    if (disabled) return;
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else if (focusIdx >= 0 && focusIdx < options.length) {
        onChange(options[focusIdx].value);
        closeAnimated();
      }
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i + 1) % Math.max(1, options.length));
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (!open || closing) {
        toggleOpen();
      } else {
        setFocusIdx((i) => (i - 1 + options.length) % Math.max(1, options.length));
      }
    }
  };

  return (
    <div
      ref={wrapRef}
      class={`volo-cselect ${open && !closing ? "is-open" : ""} ${disabled ? "is-disabled" : ""} ${className}`.trim()}
      style={style}
    >
      <button
        type="button"
        class="volo-cselect-btn"
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open && !closing}
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
          class={`volo-cselect-menu ${flipUp ? "flip-up" : ""} ${autoRight ? "align-right" : ""} ${closing ? "is-closing" : ""}`}
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
  const [closing, setClosing] = useState(false);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const closeTimerRef = useRef<number | undefined>(undefined);
  const current = locales.find((l) => l.code === value) || locales[0];

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
      if (wrapRef.current && !wrapRef.current.contains(ev.target as Node)) {
        closeAnimated();
      }
    };
    const onEsc = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") closeAnimated();
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
    };
  }, [open, closing]);

  const toggle = () => {
    if (open && !closing) {
      closeAnimated();
    } else {
      if (closeTimerRef.current) window.clearTimeout(closeTimerRef.current);
      setClosing(false);
      setOpen(true);
    }
  };

  return (
    <div ref={wrapRef} class={`volo-cselect volo-locale-pop ${open && !closing ? "is-open" : ""}`}>
      <button
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

      {open && (
        <div class={`volo-cselect-menu align-right volo-locale-menu ${closing ? "is-closing" : ""}`} role="listbox">
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
        </div>
      )}
    </div>
  );
}

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
    acquireModalScrollLock();
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCloseRef.current();
    };
    document.addEventListener("keydown", onEsc);
    return () => {
      document.removeEventListener("keydown", onEsc);
      releaseModalScrollLock();
    };
  }, [rendered]);

  if (!rendered) return null;

  return (
    <div
      class={`volo-modal-backdrop ${closing ? "is-closing" : ""}`}
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
  onFile: (file: File) => void;
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
          const f = e.dataTransfer?.files?.[0];
          if (f) onFile(f);
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
        style={{ display: "none" }}
        disabled={isDisabled}
        onChange={(e) => {
          const f = (e.currentTarget as HTMLInputElement).files?.[0];
          if (f) onFile(f);
          (e.currentTarget as HTMLInputElement).value = "";
        }}
      />
    </>
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
              aria-label="Remove"
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
