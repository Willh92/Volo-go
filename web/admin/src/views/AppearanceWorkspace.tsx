import { useEffect, useRef, useState } from "preact/hooks";
import {
  ACCENT_OPTIONS as ACCENTS,
  accentSig,
  apiFetch,
  applyThemeAttributes,
  dirtyEditorSig,
  isAbortError,
  localizeAdminError,
  openConfirm,
  resetConsoleScroll,
  safeHttpUrl,
  safeMediaSrc,
  showToast,
  themeModeSig,
  uploadWithProgress,
} from "../api";
import {
  ensureLocaleLoaded,
  exportFullTemplateJSON,
  exportLocalePackJSON,
  formatDateTime,
  localeSig,
  registerExternalLocales,
  syncExternalLocales,
  t,
} from "../i18n";
import {
  ActionMenu,
  AnimatedModal,
  CloseIcon,
  ConsoleEmpty,
  ConsoleSelect,
  DragGripIcon,
  MoveTopIcon,
  NativeModalDialog,
  pagedSlice,
  paginateItems,
  Pagination,
  PlusIcon,
  SwitchToggle,
  TableSkeleton,
  TokenListInput,
  UploadButton,
  UploadProgressBar,
  useFlipList,
  usePointerSortableList,
} from "../components/UIControls";
import { MediaPickButton } from "../components/MediaLibraryModal";
import { CM6CssEditor } from "../components/CM6Editor";

export type AppearanceTab = "themes" | "locales" | "plugins";

const CLEAR_SECRET_SENTINEL = "__VOLO_CLEAR_SECRET__";

function renderAccentLabel(id: string): string {
  switch (id) {
    case "ocean":
      return t("accent.ocean");
    case "teal":
      return t("accent.teal");
    case "amber":
      return t("accent.amber");
    case "plum":
      return t("accent.plum");
    case "rose":
      return t("accent.rose");
    default:
      return t("accent.forest");
  }
}

function resolveSchemaText(raw: any): string {
  if (raw === null || raw === undefined) return "";
  if (typeof raw === "string") return raw.trim();
  if (typeof raw === "number" || typeof raw === "boolean") return String(raw);
  if (typeof raw === "object" && !Array.isArray(raw)) {
    const loc = String(localeSig.value || "zh-CN").trim();
    const locLow = loc.toLowerCase();
    const baseLang = locLow.split("-")[0];
    for (const [k, v] of Object.entries(raw)) {
      if (typeof v === "string" && v.trim() && k.toLowerCase() === locLow) return v.trim();
    }
    for (const [k, v] of Object.entries(raw)) {
      if (typeof v === "string" && v.trim() && k.toLowerCase().split("-")[0] === baseLang) return v.trim();
    }
    for (const fb of ["zh-CN", "en-US", "en", "zh-TW"]) {
      const v = (raw as Record<string, unknown>)[fb];
      if (typeof v === "string" && v.trim()) return v.trim();
    }
    for (const v of Object.values(raw)) {
      if (typeof v === "string" && v.trim()) return v.trim();
    }
  }
  return "";
}

function isSecretSchemaField(f: any): boolean {
  return Boolean(f?.secret || f?.type === "secret" || f?.type === "password");
}

function isTagListSchemaField(f: any): boolean {
  return f?.type === "tag_list" || f?.type === "token_list";
}

function coerceTagListValue(raw: any): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  const pushTok = (s: string) => {
    const trimmed = s.trim();
    if (!trimmed || seen.has(trimmed)) return;
    seen.add(trimmed);
    out.push(trimmed);
  };
  if (Array.isArray(raw)) {
    for (const item of raw) {
      if (item !== null && item !== undefined) pushTok(String(item));
    }
    return out;
  }
  if (typeof raw === "string" && raw.trim()) {
    for (const line of raw.split(/\r?\n/)) {
      const trimmedLine = line.trim();
      if (!trimmedLine || trimmedLine.startsWith("#") || trimmedLine.startsWith("//")) continue;
      for (const seg of trimmedLine.split(/[,，;；]+/)) {
        pushTok(seg);
      }
    }
  }
  return out;
}

function getSchemaFieldGroup(f: any): string {
  return (
    resolveSchemaText(f?.localized_group) ||
    resolveSchemaText(f?.group_label) ||
    resolveSchemaText(f?.group)
  );
}

function buildSchemaDefaults(schema: any[], clearSecrets = false): Record<string, any> {
  const out: Record<string, any> = {};
  for (const f of schema || []) {
    if (!f?.key) continue;
    if (isSecretSchemaField(f)) {
      out[f.key] = clearSecrets ? CLEAR_SECRET_SENTINEL : "";
    } else if (isTagListSchemaField(f)) {
      out[f.key] = coerceTagListValue(f.default);
    } else if (Array.isArray(f.default)) {
      out[f.key] = [...f.default];
    } else if (f.default !== undefined) {
      out[f.key] = f.default;
    } else if (f.type === "sortable_list") {
      out[f.key] = [];
    } else if (f.type === "boolean") {
      out[f.key] = false;
    } else if (f.type === "number") {
      out[f.key] = typeof f.min === "number" ? f.min : 0;
    } else {
      out[f.key] = "";
    }
  }
  return out;
}

function validateSchemaRequired(
  schema: any[],
  values: Record<string, any>,
  secretKeysSet?: Record<string, boolean>,
): string | null {
  for (const f of schema || []) {
    if (!f?.key) continue;
    const label = resolveSchemaText(f.label) || String(f.key);
    if (f.type === "number") {
      const rawVal = values[f.key] !== undefined ? values[f.key] : f.default;
      const isEmpty = rawVal === "" || rawVal === null || rawVal === undefined;
      if (!isEmpty) {
        const numVal = Number(rawVal);
        if (Number.isNaN(numVal)) {
          return t("app.cfg_required_missing", { field: label });
        }
        if (typeof f.min === "number" && numVal < f.min) {
          return `${label} >= ${f.min}`;
        }
        if (typeof f.max === "number" && numVal > f.max) {
          return `${label} <= ${f.max}`;
        }
      }
    }
    if (!f.required) continue;
    if (isSecretSchemaField(f)) {
      const cur = values[f.key];
      const hasSaved = (Boolean(secretKeysSet?.[f.key]) || cur === "********") && cur !== CLEAR_SECRET_SENTINEL;
      if (!hasSaved && (!cur || cur === CLEAR_SECRET_SENTINEL)) {
        return t("app.cfg_required_missing", { field: label });
      }
      continue;
    }
    const val = values[f.key] !== undefined ? values[f.key] : f.default;
    if (f.type === "sortable_list") {
      if (!Array.isArray(val) || val.length === 0) {
        return t("app.cfg_required_missing", { field: label });
      }
    } else if (isTagListSchemaField(f)) {
      if (coerceTagListValue(val).length === 0) {
        return t("app.cfg_required_missing", { field: label });
      }
    } else if (f.type === "number") {
      if (val === "" || val === null || val === undefined || Number.isNaN(Number(val))) {
        return t("app.cfg_required_missing", { field: label });
      }
    } else if (f.type !== "boolean") {
      if (String(val ?? "").trim() === "") {
        return t("app.cfg_required_missing", { field: label });
      }
    }
  }
  return null;
}

function matchesArchiveBaseId(baseName: string, knownId: string): boolean {
  const idLow = knownId.trim().toLowerCase();
  if (!idLow) return false;
  let rest = baseName.trim().toLowerCase();
  for (const pfx of ["volo-plugin-", "volo-theme-", "plugin-", "theme-"]) {
    if (rest.startsWith(pfx)) {
      rest = rest.slice(pfx.length);
      break;
    }
  }
  if (rest === idLow) return true;
  if (!rest.startsWith(idLow)) return false;
  const suffix = rest.slice(idLow.length);
  if (suffix.startsWith("_")) return true;
  if (!suffix.startsWith("-")) return false;
  const ver = suffix.startsWith("-v") ? suffix.slice(2) : suffix.slice(1);
  if (!ver) return false;
  const dotIdx = ver.indexOf(".");
  if (dotIdx <= 0) return false;
  for (let i = 0; i < dotIdx; i++) {
    const code = ver.charCodeAt(i);
    if (code < 48 || code > 57) return false;
  }
  return true;
}

function formatApproxPackageSize(it: any): string {
  const bytes = Number(it?.approx_size_bytes || it?.size_bytes || 0);
  if (Number.isFinite(bytes) && bytes > 0) {
    if (bytes >= 1024 * 1024) {
      return `~${(bytes / (1024 * 1024)).toFixed(bytes >= 10 * 1024 * 1024 ? 1 : 2).replace(/\.0+$/, "")} MB`;
    }
    return `~${Math.max(1, Math.round(bytes / 1024))} KB`;
  }
  const mb = Number(it?.approx_zip_mb || 0);
  if (Number.isFinite(mb) && mb > 0) {
    return mb >= 0.1 ? `~${mb.toFixed(1).replace(/\.0$/, "")} MB` : `~${Math.max(1, Math.round(mb * 1024))} KB`;
  }
  return "";
}

interface ZipPreflightInfo {
  file: File;
  kind: "theme" | "plugin";
  id: string;
  name: string;
  version: string;
  author: string;
  minHostVersion: string;
  installedVersion: string | null;
  sha256: string;
  parsedOk: boolean;
}

const MAX_ZIP_PREFLIGHT_BYTES = 64 * 1024 * 1024;

async function inspectZipPreflight(
  file: File,
  kind: "theme" | "plugin",
  installedList: any[],
): Promise<ZipPreflightInfo> {
  const manifestFile = (kind === "theme" ? "theme.json" : "manifest.json").toLowerCase();
  const baseName = file.name.replace(/\.zip$/i, "");
  let id = "";
  let name = "";
  let version = "";
  let author = "";
  let minHostVersion = "";
  let sha256 = "";
  let parsedOk = false;

  if (file.size > 0 && file.size <= MAX_ZIP_PREFLIGHT_BYTES) {
    let fullBuf: ArrayBuffer | null = null;
    try {
      if (typeof crypto !== "undefined" && crypto.subtle?.digest) {
        fullBuf = await file.arrayBuffer();
        const digest = await crypto.subtle.digest("SHA-256", fullBuf);
        sha256 = Array.from(new Uint8Array(digest))
          .map((b) => b.toString(16).padStart(2, "0"))
          .join("");
      }
    } catch {
      // Ignore WebCrypto errors in insecure contexts
    }

    try {
      const readSlice = async (start: number, end: number): Promise<ArrayBuffer> => {
        if (fullBuf) return fullBuf.slice(start, end);
        return file.slice(start, end).arrayBuffer();
      };
      const tailLen = Math.min(file.size, 65558);
      const tailBuf = await readSlice(file.size - tailLen, file.size);
      const tailView = new DataView(tailBuf);
      let eocd = -1;
      for (let i = tailLen - 22; i >= 0; i--) {
        if (tailView.getUint32(i, true) === 0x06054b50) {
          eocd = i;
          break;
        }
      }
      if (eocd >= 0) {
        const cdSize = tailView.getUint32(eocd + 12, true);
        const cdOffset = tailView.getUint32(eocd + 16, true);
        if (cdOffset < file.size && cdSize > 0 && cdSize <= 4 * 1024 * 1024) {
          const cdBuf = await readSlice(cdOffset, Math.min(file.size, cdOffset + cdSize));
          const cdBytes = new Uint8Array(cdBuf);
          const cdView = new DataView(cdBuf);
          const dec = new TextDecoder();
          let pos = 0;
          let folderHint = "";
          while (pos + 46 <= cdBytes.length && cdView.getUint32(pos, true) === 0x02014b50) {
            const method = cdView.getUint16(pos + 10, true);
            const compSize = cdView.getUint32(pos + 20, true);
            const nameLen = cdView.getUint16(pos + 28, true);
            const extraLen = cdView.getUint16(pos + 30, true);
            const commentLen = cdView.getUint16(pos + 32, true);
            const lhOffset = cdView.getUint32(pos + 42, true);
            if (pos + 46 + nameLen > cdBytes.length) break;
            const entryName = dec.decode(cdBytes.subarray(pos + 46, pos + 46 + nameLen)).replace(/\\/g, "/");
            const parts = entryName.split("/").filter(Boolean);
            if (
              parts[parts.length - 1]?.toLowerCase() === manifestFile &&
              parts.length <= 2 &&
              compSize > 0 &&
              compSize <= 262144 &&
              lhOffset + 30 < file.size
            ) {
              if (parts.length === 2) folderHint = parts[0];
              const sliceEnd = Math.min(file.size, lhOffset + 30 + nameLen + 1024 + compSize);
              const lhBuf = await readSlice(lhOffset, sliceEnd);
              const lhView = new DataView(lhBuf);
              if (lhBuf.byteLength >= 30 && lhView.getUint32(0, true) === 0x04034b50) {
                const lhNameLen = lhView.getUint16(26, true);
                const lhExtraLen = lhView.getUint16(28, true);
                const dataStart = 30 + lhNameLen + lhExtraLen;
                if (dataStart + compSize <= lhBuf.byteLength) {
                  const rawEntry = new Uint8Array(lhBuf, dataStart, compSize);
                  let jsonText = "";
                  if (method === 0) {
                    jsonText = dec.decode(rawEntry);
                  } else if (method === 8 && typeof DecompressionStream === "function") {
                    const ds = new DecompressionStream("deflate-raw");
                    const copy = new Uint8Array(rawEntry.byteLength);
                    copy.set(rawEntry);
                    const stream = new Blob([copy]).stream().pipeThrough(ds);
                    jsonText = await new Response(stream).text();
                  }
                  if (jsonText) {
                    const parsed = JSON.parse(jsonText);
                    parsedOk = true;
                    id = String(parsed?.id || folderHint || "").trim();
                    name = resolveSchemaText(parsed?.display_name ?? parsed?.name) || id;
                    version = String(parsed?.version || "").replace(/^[vV]/, "").trim();
                    author = resolveSchemaText(parsed?.author);
                    minHostVersion = String(parsed?.min_host_version || parsed?.engine_version || "")
                      .replace(/^[vV]/, "")
                      .trim();
                    break;
                  }
                }
              }
            }
            pos += 46 + nameLen + extraLen + commentLen;
          }
          if (!id && folderHint) id = folderHint;
        }
      }
    } catch {
      // Graceful fallback when archive cannot be parsed client-side
    }
  }

  if (!id) {
    for (const item of installedList) {
      const knownId = String(item?.id || "").trim();
      if (!knownId) continue;
      if (matchesArchiveBaseId(baseName, knownId)) {
        id = knownId;
        break;
      }
    }
  }

  const matched = id
    ? installedList.find((it) => String(it?.id || "") === id && !it?.is_builtin && !it?.builtin && it?.kind !== "builtin")
    : null;

  return {
    file,
    kind,
    id: id || baseName,
    name: name || id || baseName,
    version,
    author,
    minHostVersion,
    installedVersion: matched ? String(matched.version || "1.0.0").replace(/^[vV]/, "") : null,
    sha256,
    parsedOk,
  };
}

async function downloadZipBlob(url: string, filename: string): Promise<void> {
  try {
    const res = await fetch(url, {
      credentials: "same-origin",
      headers: {
        "X-Volo-Console": "1",
        ...(localeSig.value ? { "Accept-Language": localeSig.value } : {}),
      },
    });
    if (!res.ok) {
      let rawMsg = `HTTP ${res.status}`;
      const ct = res.headers.get("Content-Type") || "";
      if (ct.includes("application/json")) {
        const j = await res.json().catch(() => null);
        if (j?.error) rawMsg = String(j.error);
      }
      showToast(localizeAdminError(rawMsg), "err");
      return;
    }
    const blob = await res.blob();
    const a = document.createElement("a");
    const objUrl = URL.createObjectURL(blob);
    a.href = objUrl;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    window.setTimeout(() => URL.revokeObjectURL(objUrl), 1000);
  } catch (err: any) {
    showToast(err?.message || t("api.err_network"), "err");
  }
}

function reorderList<T>(items: T[], from: number, to: number): T[] | null {
  if (from < 0 || from >= items.length || to < 0 || to >= items.length || from === to) return null;
  const next = [...items];
  const [picked] = next.splice(from, 1);
  next.splice(to, 0, picked);
  return next;
}

function SortableRow({
  idx,
  total,
  label,
  dragging,
  grabbing,
  handleProps,
  beforeLabel,
  beforeActs,
  afterActs,
  onMove,
}: {
  idx: number;
  total: number;
  label: string;
  dragging: boolean;
  grabbing: boolean;
  handleProps: any;
  beforeLabel?: any;
  beforeActs?: any;
  afterActs?: any;
  onMove: (to: number) => void;
}) {
  return (
    <div
      data-sort-idx={idx}
      class={`volo-sortable-row ${dragging ? "is-dragging" : ""} ${grabbing ? "is-grabbing" : ""}`}
    >
      <div class="volo-sortable-main">
        <span
          class={`volo-drag-handle ${total < 2 ? "is-disabled" : ""}`}
          title={t("app.drag_sort")}
          aria-label={t("app.drag_sort")}
          {...handleProps}
        >
          <DragGripIcon />
        </span>
        {beforeLabel}
        <span
          class="volo-sortable-label"
          onPointerDown={(e) => {
            if (e.pointerType === "mouse") {
              handleProps.onPointerDown(e as unknown as PointerEvent);
            }
          }}
        >
          {idx + 1}. {label}
        </span>
      </div>
      <div class="volo-sortable-acts">
        {beforeActs}
        <button
          type="button"
          class="volo-act-btn"
          disabled={idx === 0}
          title={t("app.move_top")}
          aria-label={t("app.move_top")}
          onClick={() => onMove(0)}
        >
          <MoveTopIcon />
        </button>
        <button
          type="button"
          class="volo-act-btn"
          disabled={idx === 0}
          title={t("app.move_up")}
          aria-label={t("app.move_up")}
          onClick={() => onMove(idx - 1)}
        >
          ↑
        </button>
        <button
          type="button"
          class="volo-act-btn"
          disabled={idx === total - 1}
          title={t("app.move_down")}
          aria-label={t("app.move_down")}
          onClick={() => onMove(idx + 1)}
        >
          ↓
        </button>
        {afterActs}
      </div>
    </div>
  );
}

function SortableFixedOptsField({
  fieldKey,
  label,
  reqMark,
  description,
  allOpts,
  enabledKeys,
  disabledOpts,
  showNavPlacement,
  primaryLimit,
  onChange,
}: {
  fieldKey: string;
  label: string;
  reqMark: string;
  description?: string;
  allOpts: Array<{ value: string; label: string }>;
  enabledKeys: string[];
  disabledOpts: Array<{ value: string; label: string }>;
  showNavPlacement: boolean;
  primaryLimit: number;
  onChange: (key: string, next: any) => void;
}) {
  const optMap = new Map(allOpts.map((o) => [o.value, o.label]));
  const { listRef, draggingIdx, grabbingIdx, captureFlip, getHandleProps } = usePointerSortableList(
    enabledKeys,
    (next) => onChange(fieldKey, next),
    enabledKeys.length > 1
  );

  const moveItem = (idx: number, to: number) => {
    const next = reorderList(enabledKeys, idx, to);
    if (next) {
      captureFlip();
      onChange(fieldKey, next);
    }
  };

  return (
    <div class="volo-field">
      <label>
        {label}
        {reqMark}
      </label>
      {description && <div class="volo-help">{description}</div>}
      <div
        ref={listRef}
        class="volo-sortable-list"
        style={{ maxHeight: "280px", overflowY: "auto", margin: "4px -4px 0", padding: "2px 4px" }}
      >
        {enabledKeys.map((k, idx) => (
          <SortableRow
            key={k}
            idx={idx}
            total={enabledKeys.length}
            label={optMap.get(k) || k}
            dragging={draggingIdx === idx}
            grabbing={grabbingIdx === idx}
            handleProps={getHandleProps(idx)}
            onMove={(to) => moveItem(idx, to)}
            beforeLabel={
              <SwitchToggle
                checked={true}
                onChange={(checked) => {
                  if (!checked) {
                    captureFlip();
                    onChange(fieldKey, enabledKeys.filter((it) => it !== k));
                  }
                }}
              />
            }
            beforeActs={
              showNavPlacement && (
                <span class={`volo-badge ${idx < primaryLimit ? "volo-badge-ok" : ""}`}>
                  {idx < primaryLimit ? t("app.nav_slot_primary") : t("app.nav_slot_more")}
                </span>
              )
            }
          />
        ))}
        {disabledOpts.map((opt) => (
          <div key={opt.value} data-flip-row="1" class="volo-sortable-row is-off">
            <div class="volo-sortable-main">
              <span class="volo-drag-handle is-disabled" aria-hidden="true">
                <DragGripIcon />
              </span>
              <SwitchToggle
                checked={false}
                onChange={(checked) => {
                  if (checked) {
                    captureFlip();
                    onChange(fieldKey, [...enabledKeys, opt.value]);
                  }
                }}
                label={opt.label}
              />
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function SortableFreeItemsField({
  fieldKey,
  label,
  reqMark,
  description,
  ph,
  items,
  freeItemDraft,
  setFreeItemDraft,
  onChange,
}: {
  fieldKey: string;
  label: string;
  reqMark: string;
  description?: string;
  ph?: string;
  items: string[];
  freeItemDraft: string;
  setFreeItemDraft: (v: string) => void;
  onChange: (key: string, next: any) => void;
}) {
  const { listRef, draggingIdx, grabbingIdx, captureFlip, getHandleProps } = usePointerSortableList(
    items,
    (next) => onChange(fieldKey, next),
    items.length > 1
  );

  const moveFree = (idx: number, to: number) => {
    const next = reorderList(items, idx, to);
    if (next) {
      captureFlip();
      onChange(fieldKey, next);
    }
  };

  const addFree = () => {
    const trimmed = freeItemDraft.trim();
    if (!trimmed || items.includes(trimmed)) return;
    captureFlip();
    onChange(fieldKey, [...items, trimmed]);
    setFreeItemDraft("");
  };

  return (
    <div class="volo-field">
      <label>
        {label}
        {reqMark}
      </label>
      {description && <div class="volo-help">{description}</div>}
      <div class="volo-input-row" style={{ marginBottom: items.length > 0 ? "6px" : 0 }}>
        <input
          type="text"
          class="volo-input"
          placeholder={ph || ""}
          value={freeItemDraft}
          onInput={(e) => setFreeItemDraft((e.currentTarget as HTMLInputElement).value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              if (e.isComposing || e.keyCode === 229) return;
              e.preventDefault();
              addFree();
            }
          }}
        />
        <button type="button" class="volo-btn volo-btn-sm" onClick={addFree}>
          <PlusIcon size={15} />
        </button>
      </div>
      {items.length > 0 && (
        <div ref={listRef} class="volo-sortable-list">
          {items.map((item, idx) => (
            <SortableRow
              key={item}
              idx={idx}
              total={items.length}
              label={item}
              dragging={draggingIdx === idx}
              grabbing={grabbingIdx === idx}
              handleProps={getHandleProps(idx)}
              onMove={(to) => moveFree(idx, to)}
              afterActs={
                <button
                  type="button"
                  class="volo-act-btn danger"
                  onClick={() => {
                    captureFlip();
                    onChange(fieldKey, items.filter((_, i) => i !== idx));
                  }}
                >
                  <CloseIcon size={13} />
                </button>
              }
            />
          ))}
        </div>
      )}
    </div>
  );
}

function SchemaFieldControl({
  field: f,
  values,
  onChange,
  secretKeysSet,
  namePrefix,
}: {
  field: any;
  values: Record<string, any>;
  onChange: (key: string, next: any) => void;
  secretKeysSet?: Record<string, boolean>;
  namePrefix: string;
}) {
  const [freeItemDraft, setFreeItemDraft] = useState("");
  const [showSecret, setShowSecret] = useState(false);
  const initialMaskedRef = useRef(values[f.key] === "********");
  const label = resolveSchemaText(f.label) || String(f.key);
  const desc = resolveSchemaText(f.description) || resolveSchemaText(f.help) || undefined;
  const reqMark = f.required ? " *" : "";
  const isSecret = isSecretSchemaField(f);
  const phText = resolveSchemaText(f.placeholder);
  const ph = phText ? phText : undefined;

  if (f.type === "boolean") {
    return (
      <div class="volo-field">
        <SwitchToggle
          checked={Boolean(values[f.key] ?? f.default)}
          onChange={(next) => onChange(f.key, next)}
          label={`${label}${reqMark}`}
          hint={desc}
        />
      </div>
    );
  }

  if (isTagListSchemaField(f)) {
    const rawCur = values[f.key] !== undefined ? values[f.key] : f.default;
    return (
      <div class="volo-field">
        <label>
          {label}
          {reqMark}
        </label>
        {desc && <div class="volo-help">{desc}</div>}
        <TokenListInput
          values={coerceTagListValue(rawCur)}
          onChange={(next) => onChange(f.key, next)}
          placeholder={ph}
        />
      </div>
    );
  }

  if (f.type === "sortable_list") {
    const hasFixedOpts = Array.isArray(f.options) && f.options.length > 0;
    const rawCur = values[f.key] !== undefined ? values[f.key] : f.default;
    if (hasFixedOpts) {
      const allOpts: Array<{ value: string; label: string }> = f.options.map((op: any) =>
        typeof op === "object" && op !== null
          ? { value: String(op.value), label: resolveSchemaText(op.label) || String(op.value) }
          : { value: String(op), label: String(op) },
      );
      const optMap = new Map(allOpts.map((o) => [o.value, o.label]));
      const rawExpanded: string[] = [];
      for (const rawItem of Array.isArray(rawCur) ? rawCur : []) {
        const str = String(rawItem).trim();
        if (f.key === "header_nav_items" && str.toLowerCase() === "custom") {
          for (const opt of allOpts) {
            if (opt.value.startsWith("custom:")) rawExpanded.push(opt.value);
          }
        } else {
          rawExpanded.push(str);
        }
      }
      const enabledKeys: string[] = rawExpanded.filter(
        (k: string, idx: number, arr: string[]) => optMap.has(k) && arr.indexOf(k) === idx,
      );
      const enabledSet = new Set(enabledKeys);
      const disabledOpts = allOpts.filter((o) => !enabledSet.has(o.value));
      const showNavPlacement = f.key === "header_nav_items";
      const primaryLimit = Math.max(0, Number(values.header_nav_primary_count ?? 5));

      return (
        <SortableFixedOptsField
          fieldKey={f.key}
          label={label}
          reqMark={reqMark}
          description={desc}
          allOpts={allOpts}
          enabledKeys={enabledKeys}
          disabledOpts={disabledOpts}
          showNavPlacement={showNavPlacement}
          primaryLimit={primaryLimit}
          onChange={onChange}
        />
      );
    }

    const items: string[] = (Array.isArray(rawCur) ? rawCur : []).map((v: any) => String(v)).filter(Boolean);
    return (
      <SortableFreeItemsField
        fieldKey={f.key}
        label={label}
        reqMark={reqMark}
        description={desc}
        ph={ph}
        items={items}
        freeItemDraft={freeItemDraft}
        setFreeItemDraft={setFreeItemDraft}
        onChange={onChange}
      />
    );
  }

  if (isSecret) {
    const curVal = values[f.key];
    const isClearing = curVal === CLEAR_SECRET_SENTINEL;
    const isMaskedPlaceholder = curVal === "********";
    const isSealed = Boolean(secretKeysSet?.[f.key]) || initialMaskedRef.current || isMaskedPlaceholder;
    const displayVal = isClearing || isMaskedPlaceholder ? "" : String(curVal ?? "");
    return (
      <div class="volo-field">
        <label>
          {label}
          {reqMark}{" "}
          {isSealed && !isClearing && (
            <span class="volo-badge volo-badge-ok">{t("app.secret_sealed_hint")}</span>
          )}
        </label>
        {desc && <div class="volo-help">{desc}</div>}
        <div style={{ display: "flex", gap: "8px", alignItems: "center" }}>
          <input
            type={(showSecret ? "text" : "password") as "password"}
            class="volo-input"
            style={{ flex: 1, minWidth: 0 }}
            name={`${namePrefix}_${f.key}`}
            autoComplete="new-password"
            disabled={isClearing}
            placeholder={isClearing ? t("app.clear_saved_secret") : isSealed ? "********" : ph || "********"}
            value={displayVal}
            onInput={(e) => {
              const nextVal = (e.currentTarget as HTMLInputElement).value;
              if (nextVal === "" && initialMaskedRef.current) {
                onChange(f.key, "********");
              } else {
                onChange(f.key, nextVal);
              }
            }}
          />
          <button
            type="button"
            class="volo-btn volo-btn-sm"
            disabled={isClearing || displayVal === ""}
            onClick={() => setShowSecret((prev) => !prev)}
          >
            {t(showSecret ? "app.secret_hide" : "app.secret_show")}
          </button>
        </div>
        <div style={{ marginTop: "6px" }}>
          <SwitchToggle
            checked={isClearing}
            onChange={(clear) => {
              setShowSecret(false);
              onChange(
                f.key,
                clear ? CLEAR_SECRET_SENTINEL : initialMaskedRef.current ? "********" : "",
              );
            }}
            label={t("app.clear_saved_secret")}
          />
        </div>
      </div>
    );
  }

  return (
    <div class="volo-field">
      <label>
        {label}
        {reqMark}
      </label>
      {desc && <div class="volo-help">{desc}</div>}
      {f.type === "select" && Array.isArray(f.options) ? (
        <ConsoleSelect
          value={String(values[f.key] ?? f.default ?? "")}
          onChange={(v) => onChange(f.key, v)}
          options={f.options.map((op: any) =>
            typeof op === "object" && op !== null
              ? { value: String(op.value), label: resolveSchemaText(op.label) || String(op.value) }
              : { value: String(op), label: String(op) },
          )}
        />
      ) : f.type === "color" ? (
        (() => {
          const rawColor = String(values[f.key] ?? f.default ?? "");
          const swatchVal = /^#[0-9a-fA-F]{6}$/.test(rawColor.trim()) ? rawColor.trim() : "#246b51";
          return (
            <div class="volo-input-row">
              <input
                type="color"
                value={swatchVal}
                aria-label={label}
                style={{
                  width: "40px",
                  height: "36px",
                  padding: "2px",
                  border: "1px solid var(--volo-line)",
                  borderRadius: "8px",
                  background: "var(--volo-surface)",
                  cursor: "pointer",
                  flexShrink: 0,
                }}
                onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
              />
              <input
                type="text"
                class="volo-input"
                name={`${namePrefix}_${f.key}`}
                autoComplete="off"
                required={Boolean(f.required)}
                placeholder={ph || "#246b51"}
                value={rawColor}
                onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
              />
            </div>
          );
        })()
      ) : f.type === "image" ? (
        <div class="volo-input-row">
          <input
            type="text"
            class="volo-input"
            name={`${namePrefix}_${f.key}`}
            autoComplete="off"
            required={Boolean(f.required)}
            placeholder={ph}
            value={String(values[f.key] ?? f.default ?? "")}
            onInput={(e) => onChange(f.key, (e.currentTarget as HTMLInputElement).value)}
          />
          <MediaPickButton filterKind="image" onPick={(url) => onChange(f.key, url)} />
        </div>
      ) : f.type === "textarea" ? (
        <textarea
          class="volo-textarea"
          rows={3}
          required={Boolean(f.required)}
          placeholder={ph}
          value={String(values[f.key] ?? f.default ?? "")}
          onInput={(e) => onChange(f.key, (e.currentTarget as HTMLTextAreaElement).value)}
        />
      ) : (
        <input
          type={(f.type === "number" ? "number" : f.type === "url" ? "url" : "text") as "text"}
          class="volo-input"
          name={`${namePrefix}_${f.key}`}
          autoComplete="off"
          required={Boolean(f.required)}
          placeholder={ph}
          min={f.type === "number" && typeof f.min === "number" ? f.min : undefined}
          max={f.type === "number" && typeof f.max === "number" ? f.max : undefined}
          step={f.type === "number" && typeof f.step === "number" ? f.step : undefined}
          value={String(values[f.key] ?? f.default ?? "")}
          onInput={(e) => {
            const raw = (e.currentTarget as HTMLInputElement).value;
            onChange(f.key, f.type === "number" ? (raw === "" ? "" : Number(raw)) : raw);
          }}
        />
      )}
    </div>
  );
}

function SchemaConfigPanel({
  targetId,
  schema,
  values,
  onChange,
  onMergeValues,
  secretKeysSet,
  namePrefix,
}: {
  targetId: string;
  schema: any[];
  values: Record<string, any>;
  onChange: (key: string, next: any) => void;
  onMergeValues: (patch: Record<string, any>) => void;
  secretKeysSet?: Record<string, boolean>;
  namePrefix: string;
}) {
  const [groupFilter, setGroupFilter] = useState<string>("__all__");
  const [searchQ, setSearchQ] = useState<string>("");

  useEffect(() => {
    setGroupFilter("__all__");
    setSearchQ("");
  }, [targetId]);

  const groups: string[] = [];
  for (const f of schema) {
    const g = getSchemaFieldGroup(f);
    if (g && !groups.includes(g)) groups.push(g);
  }

  const qLower = searchQ.trim().toLowerCase();
  const visibleFields = schema.filter((f) => {
    if (!f?.key) return false;
    if (groupFilter !== "__all__" && getSchemaFieldGroup(f) !== groupFilter) return false;
    if (qLower) {
      const hay = `${f.key} ${resolveSchemaText(f.label)} ${resolveSchemaText(f.description)} ${resolveSchemaText(f.help)} ${getSchemaFieldGroup(f)}`.toLowerCase();
      if (!hay.includes(qLower)) return false;
    }
    return true;
  });

  const exportConfigJSON = () => {
    const out: Record<string, any> = {};
    for (const f of schema) {
      if (!f?.key || isSecretSchemaField(f)) continue;
      const val = values[f.key] !== undefined ? values[f.key] : f.default;
      if (val !== CLEAR_SECRET_SENTINEL) {
        out[f.key] = val;
      }
    }
    const blob = new Blob([JSON.stringify(out, null, 2) + "\n"], { type: "application/json;charset=utf-8" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `${targetId || "config"}-config.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  return (
    <form autoComplete="off" onSubmit={(e) => e.preventDefault()}>
      <div
        class="volo-input-row"
        style={{ justifyContent: "space-between", flexWrap: "wrap", gap: "8px", marginBottom: "12px" }}
      >
        {(schema.length >= 8 || groups.length > 0) && (
          <input
            type="text"
            class="volo-input volo-search-input"
            style={{ flex: "1 1 180px", minWidth: 0 }}
            placeholder={t("app.cfg_search_ph")}
            value={searchQ}
            onInput={(e) => setSearchQ((e.currentTarget as HTMLInputElement).value)}
          />
        )}
        <div class="volo-input-row" style={{ gap: "6px", flexWrap: "wrap", marginLeft: "auto" }}>
          <button type="button" class="volo-btn volo-btn-sm" onClick={exportConfigJSON}>
            {t("app.cfg_export_json")}
          </button>
          <UploadButton
            accept=".json"
            small
            label={t("app.cfg_import_json")}
            onFile={async (file) => {
              try {
                const raw = await file.text();
                const parsed = JSON.parse(raw);
                if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
                  showToast(t("app.cfg_import_invalid"), "err");
                  return;
                }
                const allowed = new Map<string, any>();
                for (const f of schema) {
                  if (f?.key && !isSecretSchemaField(f)) allowed.set(f.key, f);
                }
                const patch: Record<string, any> = {};
                for (const [k, v] of Object.entries(parsed as Record<string, any>)) {
                  const fDef = allowed.get(k);
                  if (fDef && v !== CLEAR_SECRET_SENTINEL) {
                    patch[k] = isTagListSchemaField(fDef) ? coerceTagListValue(v) : v;
                  }
                }
                onMergeValues(patch);
                showToast(t("app.cfg_imported_toast"), "ok");
              } catch {
                showToast(t("app.cfg_import_invalid"), "err");
              }
            }}
          />
        </div>
      </div>

      {groups.length > 1 && (
        <div class="volo-filter-pills" style={{ marginBottom: "12px" }}>
          <button
            type="button"
            class={`volo-filter-pill ${groupFilter === "__all__" ? "active" : ""}`}
            onClick={() => setGroupFilter("__all__")}
          >
            {t("art.all")}
          </button>
          {groups.map((g) => (
            <button
              key={g}
              type="button"
              class={`volo-filter-pill ${groupFilter === g ? "active" : ""}`}
              onClick={() => setGroupFilter(g)}
            >
              {g}
            </button>
          ))}
        </div>
      )}

      {visibleFields.length === 0 ? (
        <div class="volo-help" style={{ textAlign: "center", padding: "18px 8px" }}>
          {t("common.empty")}
        </div>
      ) : (
        visibleFields.map((f: any, idx: number) => {
          const curGroup = getSchemaFieldGroup(f);
          const prevGroup = idx > 0 ? getSchemaFieldGroup(visibleFields[idx - 1]) : "";
          const showGroupHeader = groupFilter === "__all__" && groups.length > 0 && curGroup && curGroup !== prevGroup;
          return (
            <div key={f.key}>
              {showGroupHeader && (
                <div
                  style={{
                    fontSize: "12px",
                    fontWeight: 600,
                    color: "var(--volo-muted)",
                    textTransform: "uppercase",
                    letterSpacing: "0.04em",
                    marginTop: idx === 0 ? "2px" : "14px",
                    marginBottom: "8px",
                    paddingBottom: "4px",
                    borderBottom: "1px solid var(--volo-line)",
                  }}
                >
                  {curGroup}
                </div>
              )}
              <SchemaFieldControl
                field={f}
                values={values}
                onChange={onChange}
                secretKeysSet={secretKeysSet}
                namePrefix={namePrefix}
              />
            </div>
          );
        })
      )}
    </form>
  );
}

type RemoteExtKind = "plugin" | "theme" | "locale";

interface RemoteInstallModalProps {
  open: boolean;
  kind: RemoteExtKind;
  onClose: () => void;
  onInstalled: (res: any, kind: RemoteExtKind) => void;
}

function RemoteInstallModal({ open, kind, onClose, onInstalled }: RemoteInstallModalProps) {
  const [urlVal, setUrlVal] = useState("");
  const [shaVal, setShaVal] = useState("");
  const [autoAct, setAutoAct] = useState(kind === "plugin");
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [statusMsg, setStatusMsg] = useState("");
  const [statusTone, setStatusTone] = useState<"err" | "warn">("err");
  const [catLoading, setCatLoading] = useState(false);
  const [catItems, setCatItems] = useState<any[]>([]);
  const abortRef = useRef<AbortController | null>(null);
  const catAbortRef = useRef<AbortController | null>(null);
  const catSeqRef = useRef(0);
  const urlInputId = `volo-ext-url-${kind}`;
  const shaInputId = `volo-ext-sha-${kind}`;
  const locked = busy || confirming;

  const loadCatalog = async (refresh = false) => {
    catAbortRef.current?.abort();
    const ctrl = new AbortController();
    catAbortRef.current = ctrl;
    const seq = ++catSeqRef.current;
    setCatLoading(true);
    try {
      const base =
        kind === "plugin"
          ? "/api/admin/plugins/catalog"
          : kind === "theme"
            ? "/api/admin/themes/catalog"
            : "/api/admin/locales/catalog";
      const qs = `locale=${encodeURIComponent(localeSig.value)}${refresh ? "&refresh=1" : ""}`;
      const res = await apiFetch<{ items?: any[] }>(`${base}?${qs}`, { signal: ctrl.signal });
      if (seq !== catSeqRef.current) return;
      setCatItems(Array.isArray(res?.items) ? res.items : []);
    } catch (err: any) {
      if (seq !== catSeqRef.current || isAbortError(err)) return;
      if (refresh) {
        setStatusTone("err");
        setStatusMsg(err.message || "");
      }
    } finally {
      if (seq === catSeqRef.current) setCatLoading(false);
    }
  };

  useEffect(() => {
    if (!open) {
      abortRef.current?.abort();
      abortRef.current = null;
      catAbortRef.current?.abort();
      catAbortRef.current = null;
      catSeqRef.current++;
      setBusy(false);
      setConfirming(false);
      setStatusMsg("");
      return;
    }
    setUrlVal("");
    setShaVal("");
    setAutoAct(kind === "plugin");
    setStatusMsg("");
    setCatItems([]);
    void loadCatalog(false);
    return () => {
      catAbortRef.current?.abort();
    };
  }, [open, kind]);

  const executeRemoteInstall = async (
    rawUrl: string,
    rawSha: string,
    overwrite: boolean,
    extra?: { checksumUrl?: string; assetName?: string },
  ): Promise<void> => {
    const cleanUrl = safeHttpUrl(rawUrl);
    if (!cleanUrl) {
      setStatusTone("err");
      setStatusMsg(t("app.ext_url_required"));
      return;
    }
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setBusy(true);
    setStatusMsg(t("app.ext_downloading_status"));
    try {
      const endpoint =
        kind === "plugin"
          ? `/api/admin/plugins/install-url?locale=${encodeURIComponent(localeSig.value)}`
          : kind === "theme"
            ? `/api/admin/themes/install-url?locale=${encodeURIComponent(localeSig.value)}`
            : `/api/admin/locales/install-url?locale=${encodeURIComponent(localeSig.value)}`;
      const checksumUrl = safeHttpUrl(extra?.checksumUrl);
      const assetName = String(extra?.assetName || "").trim();
      const baseFields = {
        url: cleanUrl,
        sha256: rawSha.trim(),
        ...(checksumUrl ? { checksum_url: checksumUrl } : {}),
        ...(assetName ? { asset_name: assetName } : {}),
        overwrite,
      };
      const bodyObj =
        kind === "plugin"
          ? { ...baseFields, enable: autoAct }
          : kind === "theme"
            ? { ...baseFields, activate: autoAct }
            : baseFields;
      const res = await apiFetch<any>(endpoint, {
        method: "POST",
        body: JSON.stringify(bodyObj),
        signal: ctrl.signal,
      });
      setBusy(false);
      setStatusMsg("");
      onInstalled(res, kind);
      onClose();
      showToast(
        kind === "plugin"
          ? t("app.installed_toast")
          : kind === "theme"
            ? t("app.theme_installed")
            : t("app.loc_installed"),
        "ok",
      );
    } catch (err: any) {
      setBusy(false);
      abortRef.current = null;
      if (isAbortError(err)) {
        setStatusTone("warn");
        setStatusMsg(t("app.ext_install_canceled"));
        return;
      }
      if (!overwrite && (err?.status === 409 || /already exists|conflict/i.test(String(err?.message || "")))) {
        setStatusMsg("");
        setConfirming(true);
        const ok = await openConfirm({
          message:
            kind === "plugin"
              ? t("app.confirm_overwrite_plugin")
              : kind === "theme"
                ? t("app.confirm_overwrite_theme")
                : t("ext.confirm_overwrite_locale"),
          danger: true,
        });
        setConfirming(false);
        if (ok) {
          await executeRemoteInstall(cleanUrl, rawSha, true, extra);
        }
        return;
      }
      setStatusTone("err");
      setStatusMsg(err.message || "");
    }
  };

  const modalTitle =
    kind === "plugin"
      ? t("app.ext_url_modal_plugin_title")
      : kind === "theme"
        ? t("app.ext_url_modal_theme_title")
        : t("ext.url_modal_locale_title");

  return (
    <NativeModalDialog
      open={open}
      onClose={onClose}
      busy={locked}
      busyGuardHint={t("app.ext_busy_close_guard")}
      title={modalTitle}
      subtitle={t("app.ext_url_modal_sub")}
      maxWidth="640px"
      footer={
        <>
          {busy ? (
            <button
              type="button"
              class="volo-btn volo-btn-danger"
              onClick={() => abortRef.current?.abort()}
            >
              {t("app.ext_cancel_transfer")}
            </button>
          ) : (
            <button type="button" class="volo-btn" disabled={confirming} onClick={onClose}>
              {t("common.cancel")}
            </button>
          )}
          <button
            type="button"
            class="volo-btn volo-btn-primary"
            disabled={locked || !urlVal.trim()}
            onClick={() => void executeRemoteInstall(urlVal, shaVal, false)}
          >
            {t("app.ext_install_start")}
          </button>
        </>
      }
    >
      <div class="volo-field">
        <label htmlFor={urlInputId}>
          {kind === "locale" ? t("ext.url_locale_label") : t("app.ext_url_label")}
        </label>
        <input
          id={urlInputId}
          type="url"
          class="volo-input"
          autoFocus
          disabled={locked}
          placeholder={
            kind === "plugin"
              ? t("app.ext_url_plugin_ph")
              : kind === "theme"
                ? t("app.ext_url_theme_ph")
                : t("ext.url_locale_ph")
          }
          value={urlVal}
          onInput={(e) => setUrlVal((e.currentTarget as HTMLInputElement).value)}
        />
      </div>
      <div class="volo-field">
        <label htmlFor={shaInputId}>{t("app.ext_sha256_label")}</label>
        <input
          id={shaInputId}
          type="text"
          class="volo-input"
          disabled={locked}
          placeholder={t("app.ext_sha256_ph")}
          value={shaVal}
          onInput={(e) => setShaVal((e.currentTarget as HTMLInputElement).value)}
        />
      </div>
      {kind !== "locale" && (
        <div class="volo-field">
          <SwitchToggle
            checked={autoAct}
            disabled={locked}
            onChange={setAutoAct}
            label={kind === "plugin" ? t("app.ext_auto_enable") : t("app.ext_auto_activate")}
          />
        </div>
      )}

      {busy && (
        <div style={{ marginBottom: "12px" }}>
          <progress class="volo-native-progress" aria-label={t("app.ext_downloading_status")} />
          <div role="status" class="volo-help" style={{ marginTop: "6px" }}>
            {statusMsg || t("app.ext_downloading_status")}
          </div>
        </div>
      )}
      {!busy && statusMsg && (
        <div
          role={statusTone === "err" ? "alert" : "status"}
          class="volo-help"
          style={{
            marginBottom: "10px",
            color: statusTone === "err" ? "var(--volo-danger)" : "var(--volo-warn, var(--volo-muted))",
          }}
        >
          {statusMsg}
        </div>
      )}

      <div
        style={{
          display: "flex",
          justifyContent: "space-between",
          alignItems: "center",
          gap: "8px",
          marginTop: "6px",
          marginBottom: "8px",
        }}
      >
        <strong style={{ fontSize: "13px" }}>{t("app.ext_catalog_head")}</strong>
        <button
          type="button"
          class="volo-btn volo-btn-sm"
          disabled={locked || catLoading}
          onClick={() => void loadCatalog(true)}
        >
          {t("app.ext_catalog_refresh")}
        </button>
      </div>
      {catItems.length === 0 ? (
        <div class="volo-help">{catLoading ? t("app.ext_checking") : t("app.ext_catalog_empty")}</div>
      ) : (
        <div class="volo-ext-catalog">
          {catItems.map((it) => {
            const itemKey = String(it.id || it.code || "");
            const isInst = Boolean(it.installed);
            const hasUpd = Boolean(it.has_update);
            const compat = it.compatible_with_host !== false;
            const dlUrl = safeHttpUrl(it.download_url);
            const relHref = safeHttpUrl(it.release_url || it.homepage);
            const sizeBadge = formatApproxPackageSize(it);
            const hasVerifiedHash = Boolean(it.sha256 || it.checksum_url);
            const assetName =
              String(it.asset_name || "").trim() ||
              (dlUrl ? decodeURIComponent(dlUrl.split("?")[0].split("/").pop() || "") : "");
            return (
              <div key={itemKey} class="volo-ext-catalog-card">
                <div style={{ minWidth: 0, flex: 1 }}>
                  <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center" }}>
                    <strong style={{ fontSize: "13.5px" }}>{it.display_name || it.name || itemKey}</strong>
                    {it.code && <span class="volo-code-chip">{it.code}</span>}
                    {it.version && <span class="volo-code-chip">v{it.version}</span>}
                    {sizeBadge && <span class="volo-code-chip">{sizeBadge}</span>}
                    {hasVerifiedHash && (
                      <span class="volo-badge volo-badge-ok">{t("ext.sha256sums_badge")}</span>
                    )}
                    {isInst && (
                      <span class={`volo-badge ${hasUpd ? "" : "volo-badge-ok"}`}>
                        {t("app.ext_catalog_installed", { ver: it.installed_version || it.version || "1.0.0" })}
                      </span>
                    )}
                    {!compat && it.min_host_version && (
                      <span class="volo-status-pill danger" style={{ fontSize: "11px" }}>
                        &gt;= v{it.min_host_version}
                      </span>
                    )}
                  </div>
                  {(it.display_summary || it.description) && (
                    <div class="volo-help" style={{ marginTop: "3px" }}>
                      {resolveSchemaText(it.display_summary || it.description)}
                    </div>
                  )}
                </div>
                <div style={{ display: "flex", gap: "6px", alignItems: "center", flexShrink: 0, flexWrap: "wrap" }}>
                  {relHref && (
                    <a
                      class="volo-btn volo-btn-sm"
                      href={relHref}
                      target="_blank"
                      rel="noopener noreferrer"
                    >
                      {t("app.ext_view_release")} ↗
                    </a>
                  )}
                  {dlUrl && !it.is_builtin && (
                    <button
                      type="button"
                      class={`volo-btn volo-btn-sm ${!isInst || hasUpd ? "volo-btn-primary" : ""}`}
                      disabled={locked || !compat}
                      onClick={() => {
                        setUrlVal(dlUrl);
                        setShaVal(String(it.sha256 || ""));
                        void executeRemoteInstall(dlUrl, String(it.sha256 || ""), isInst, {
                          checksumUrl: String(it.checksum_url || ""),
                          assetName,
                        });
                      }}
                    >
                      {hasUpd
                        ? t("app.ext_update_now")
                        : isInst
                          ? t("app.ext_catalog_reinstall")
                          : t("app.ext_catalog_install_one")}
                    </button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </NativeModalDialog>
  );
}

interface ExtensionUpdateModalProps {
  target: { kind: "plugin" | "theme"; id: string; name: string; status: any } | null;
  onClose: () => void;
  onUpgraded: (res: any, kind: "plugin" | "theme") => void;
}

function ExtensionUpdateModal({ target, onClose, onUpgraded }: ExtensionUpdateModalProps) {
  const [busy, setBusy] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  const [errTone, setErrTone] = useState<"err" | "warn">("err");
  const abortRef = useRef<AbortController | null>(null);
  const lastTargetRef = useRef(target);
  if (target) lastTargetRef.current = target;
  const view = target || lastTargetRef.current;

  useEffect(() => {
    if (!target) {
      abortRef.current?.abort();
      abortRef.current = null;
      setBusy(false);
      setErrMsg("");
      return;
    }
    setErrMsg("");
  }, [target]);

  const st = view?.status || {};
  const onlineAllowed = Boolean(st.online_updatable ?? st.can_online_update);
  const compat = st.compatible_with_host !== false;
  const latestVer = String(st.latest_version || "");
  const curVer = String(st.current_version || "");
  const relUrl = safeHttpUrl(st.release_url);
  const sizeBadge = formatApproxPackageSize(st);
  const hasVerifiedHash = Boolean(st.sha256 || st.checksum_url);

  const runOnlineUpgrade = async () => {
    if (!target || busy || !onlineAllowed || !compat) return;
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setBusy(true);
    setErrMsg("");
    try {
      const endpoint =
        target.kind === "plugin"
          ? `/api/admin/plugins/${encodeURIComponent(target.id)}/upgrade?locale=${encodeURIComponent(localeSig.value)}`
          : `/api/admin/themes/${encodeURIComponent(target.id)}/upgrade?locale=${encodeURIComponent(localeSig.value)}`;
      const res = await apiFetch<any>(endpoint, {
        method: "POST",
        body: "{}",
        signal: ctrl.signal,
      });
      setBusy(false);
      onUpgraded(res, target.kind);
      onClose();
      showToast(t("app.ext_upgraded_toast", { name: target.name, ver: latestVer || "latest" }), "ok");
    } catch (err: any) {
      setBusy(false);
      abortRef.current = null;
      if (isAbortError(err)) {
        setErrTone("warn");
        setErrMsg(t("app.ext_install_canceled"));
        return;
      }
      setErrTone("err");
      setErrMsg(err.message || "");
    }
  };

  return (
    <NativeModalDialog
      open={Boolean(target)}
      onClose={onClose}
      busy={busy}
      busyGuardHint={t("app.ext_busy_close_guard")}
      title={t("app.ext_update_modal_title", { name: view?.name || "" })}
      subtitle={curVer && latestVer ? `v${curVer} → v${latestVer}` : undefined}
      maxWidth="560px"
      footer={
        <>
          {relUrl && (
            <a
              class={`volo-btn ${!onlineAllowed ? "volo-btn-primary" : ""}`}
              style={{ marginRight: "auto" }}
              href={relUrl}
              target="_blank"
              rel="noopener noreferrer"
            >
              {t("app.ext_view_release")} ↗
            </a>
          )}
          {busy ? (
            <button
              type="button"
              class="volo-btn volo-btn-danger"
              onClick={() => abortRef.current?.abort()}
            >
              {t("app.ext_cancel_transfer")}
            </button>
          ) : (
            <button type="button" class="volo-btn" autoFocus onClick={onClose}>
              {t("common.close")}
            </button>
          )}
          {onlineAllowed && (
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              disabled={busy || !compat}
              onClick={() => void runOnlineUpgrade()}
            >
              {t("app.ext_confirm_upgrade", { ver: latestVer || "latest" })}
            </button>
          )}
        </>
      }
    >
      {(sizeBadge || hasVerifiedHash) && (
        <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center", marginBottom: "10px" }}>
          {sizeBadge && <span class="volo-code-chip">{sizeBadge}</span>}
          {hasVerifiedHash && <span class="volo-badge volo-badge-ok">{t("ext.sha256sums_badge")}</span>}
        </div>
      )}
      {!compat && st.min_host_version && (
        <div
          role="alert"
          class="volo-help"
          style={{
            marginBottom: "10px",
            padding: "8px 10px",
            borderRadius: "8px",
            border: "1px solid var(--volo-danger)",
            color: "var(--volo-danger)",
          }}
        >
          {t("app.ext_incompat_host", { min: String(st.min_host_version) })}
        </div>
      )}
      {!onlineAllowed && (
        <div
          class="volo-help"
          style={{
            marginBottom: "10px",
            padding: "8px 10px",
            borderRadius: "8px",
            border: "1px solid var(--volo-line)",
            background: "var(--volo-raised)",
          }}
        >
          {t("app.ext_manual_only_hint")}
        </div>
      )}
      <div class="volo-field">
        <label>{t("app.ext_changelog_label")}</label>
        <div class="volo-ext-changelog">{String(st.changelog || "").trim() || t("app.ext_no_changelog")}</div>
      </div>
      {onlineAllowed && <div class="volo-help">{t("app.ext_rollback_hint")}</div>}
      {busy && (
        <div style={{ marginTop: "12px" }}>
          <progress class="volo-native-progress" aria-label={t("app.ext_upgrading_status")} />
          <div role="status" class="volo-help" style={{ marginTop: "6px" }}>
            {t("app.ext_upgrading_status")}
          </div>
        </div>
      )}
      {!busy && errMsg && (
        <div
          role={errTone === "err" ? "alert" : "status"}
          class="volo-help"
          style={{
            marginTop: "10px",
            color: errTone === "err" ? "var(--volo-danger)" : "var(--volo-warn, var(--volo-muted))",
          }}
        >
          {errMsg}
        </div>
      )}
    </NativeModalDialog>
  );
}

interface BatchUpdateItem {
  id: string;
  name: string;
  curVer: string;
  latestVer: string;
}

type BatchRowState = "pending" | "upgrading" | "done" | "failed" | "canceled";

function BatchUpdateModal({
  target,
  onClose,
  onItemUpgraded,
}: {
  target: { kind: "plugin" | "theme"; items: BatchUpdateItem[] } | null;
  onClose: () => void;
  onItemUpgraded: (res: any, kind: "plugin" | "theme") => void;
}) {
  const [running, setRunning] = useState(false);
  const [states, setStates] = useState<Record<string, BatchRowState>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const abortRef = useRef<AbortController | null>(null);
  const canceledRef = useRef(false);
  const lastRef = useRef(target);
  if (target) lastRef.current = target;
  const view = target || lastRef.current;

  useEffect(() => {
    if (!target) {
      canceledRef.current = true;
      abortRef.current?.abort();
      abortRef.current = null;
      setRunning(false);
      return;
    }
    canceledRef.current = false;
    const init: Record<string, BatchRowState> = {};
    for (const it of target.items) init[it.id] = "pending";
    setStates(init);
    setErrors({});
    setRunning(true);

    void (async () => {
      let okCount = 0;
      let hadError = false;
      for (let i = 0; i < target.items.length; i++) {
        const it = target.items[i];
        if (canceledRef.current) {
          setStates((prev) => ({ ...prev, [it.id]: "canceled" }));
          continue;
        }
        setStates((prev) => ({ ...prev, [it.id]: "upgrading" }));
        const ctrl = new AbortController();
        abortRef.current = ctrl;
        try {
          const endpoint =
            target.kind === "plugin"
              ? `/api/admin/plugins/${encodeURIComponent(it.id)}/upgrade?locale=${encodeURIComponent(localeSig.value)}`
              : `/api/admin/themes/${encodeURIComponent(it.id)}/upgrade?locale=${encodeURIComponent(localeSig.value)}`;
          const res = await apiFetch<any>(endpoint, {
            method: "POST",
            body: "{}",
            signal: ctrl.signal,
          });
          okCount++;
          onItemUpgraded(res, target.kind);
          setStates((prev) => ({ ...prev, [it.id]: "done" }));
        } catch (err: any) {
          if (isAbortError(err) || canceledRef.current) {
            canceledRef.current = true;
            setStates((prev) => {
              const next = { ...prev, [it.id]: "canceled" as BatchRowState };
              for (let j = i + 1; j < target.items.length; j++) {
                next[target.items[j].id] = "canceled";
              }
              return next;
            });
            break;
          }
          hadError = true;
          setStates((prev) => ({ ...prev, [it.id]: "failed" }));
          setErrors((prev) => ({ ...prev, [it.id]: err?.message || "Failed" }));
        }
      }
      abortRef.current = null;
      setRunning(false);
      if (!canceledRef.current && !hadError) {
        onClose();
        showToast(t("ext.batch_done_toast", { ok: okCount, total: target.items.length }), "ok");
      }
    })();

    return () => {
      canceledRef.current = true;
      abortRef.current?.abort();
    };
  }, [target]);

  const list = view?.items || [];
  const doneCount = list.filter((it) => states[it.id] === "done").length;

  return (
    <NativeModalDialog
      open={Boolean(target)}
      onClose={onClose}
      busy={running}
      busyGuardHint={t("app.ext_busy_close_guard")}
      title={t("ext.batch_modal_title", { done: doneCount, total: list.length })}
      subtitle={t("ext.batch_modal_sub")}
      maxWidth="540px"
      footer={
        running ? (
          <button
            type="button"
            class="volo-btn volo-btn-danger"
            onClick={() => {
              canceledRef.current = true;
              abortRef.current?.abort();
            }}
          >
            {t("ext.batch_cancel_rest")}
          </button>
        ) : (
          <button type="button" class="volo-btn volo-btn-primary" autoFocus onClick={onClose}>
            {t("common.close")}
          </button>
        )
      }
    >
      {running && (
        <div style={{ marginBottom: "10px" }}>
          <progress class="volo-native-progress" value={doneCount} max={Math.max(1, list.length)} />
        </div>
      )}
      <div class="volo-ext-catalog">
        {list.map((it) => {
          const st = states[it.id] || "pending";
          const pillTone =
            st === "done" ? "ok" : st === "failed" ? "danger" : st === "upgrading" ? "warn" : "";
          const stLabel =
            st === "done"
              ? t("ext.batch_st_done")
              : st === "failed"
                ? t("ext.batch_st_failed")
                : st === "upgrading"
                  ? t("ext.batch_st_upgrading")
                  : st === "canceled"
                    ? t("ext.batch_st_canceled")
                    : t("ext.batch_st_pending");
          return (
            <div key={it.id} class="volo-ext-catalog-card">
              <div style={{ minWidth: 0, flex: 1 }}>
                <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center" }}>
                  <strong style={{ fontSize: "13.5px" }}>{it.name}</strong>
                  <span class="volo-code-chip">
                    v{it.curVer} → v{it.latestVer}
                  </span>
                </div>
                {errors[it.id] && (
                  <div class="volo-help" style={{ marginTop: "3px", color: "var(--volo-danger)" }}>
                    {errors[it.id]}
                  </div>
                )}
              </div>
              <span class={`volo-status-pill ${pillTone}`}>
                <span class="volo-status-dot" />
                {stLabel}
              </span>
            </div>
          );
        })}
      </div>
    </NativeModalDialog>
  );
}

function ZipPreflightModal({
  info,
  onClose,
  onCompleted,
}: {
  info: ZipPreflightInfo | null;
  onClose: () => void;
  onCompleted: (res: any, kind: "theme" | "plugin", autoAfter: boolean, pkgId: string) => Promise<void>;
}) {
  const [autoAfter, setAutoAfter] = useState(true);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [pct, setPct] = useState<number | null>(null);
  const [errMsg, setErrMsg] = useState("");
  const abortRef = useRef<AbortController | null>(null);
  const lastRef = useRef(info);
  if (info) lastRef.current = info;
  const view = info || lastRef.current;

  useEffect(() => {
    if (!info) {
      abortRef.current?.abort();
      abortRef.current = null;
      setBusy(false);
      setConfirming(false);
      setPct(null);
      setErrMsg("");
      return;
    }
    setAutoAfter(info.kind === "plugin");
    setPct(null);
    setErrMsg("");
  }, [info]);

  const locked = busy || confirming;

  const runZipInstall = async (overwrite: boolean): Promise<void> => {
    if (!info) return;
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setBusy(true);
    setPct(0);
    setErrMsg("");
    try {
      const fd = new FormData();
      fd.append("file", info.file);
      const extraParam =
        info.kind === "plugin"
          ? `&enable=${autoAfter ? "true" : "false"}`
          : `&activate=${autoAfter ? "true" : "false"}`;
      const qs = `locale=${encodeURIComponent(localeSig.value)}${overwrite ? "&overwrite=true" : ""}${extraParam}`;
      const endpoint =
        info.kind === "plugin"
          ? `/api/admin/plugins/install?${qs}`
          : `/api/admin/themes/install?${qs}`;
      const res = await uploadWithProgress<any>(endpoint, fd, setPct, "POST", ctrl.signal);
      await onCompleted(res, info.kind, autoAfter, info.parsedOk ? info.id : "");
      setBusy(false);
      setPct(null);
      onClose();
      showToast(info.kind === "plugin" ? t("app.installed_toast") : t("app.theme_installed"), "ok");
    } catch (err: any) {
      setBusy(false);
      setPct(null);
      abortRef.current = null;
      if (isAbortError(err)) {
        return;
      }
      if (!overwrite && (err?.status === 409 || /already exists|conflict/i.test(String(err?.message || "")))) {
        setConfirming(true);
        const ok = await openConfirm({
          message: info.kind === "plugin" ? t("app.confirm_overwrite_plugin") : t("app.confirm_overwrite_theme"),
          danger: true,
        });
        setConfirming(false);
        if (ok) {
          await runZipInstall(true);
        }
        return;
      }
      setErrMsg(err.message || "");
    }
  };

  const fileSizeLabel = view ? formatApproxPackageSize({ size_bytes: view.file.size }) : "";

  return (
    <NativeModalDialog
      open={Boolean(info)}
      onClose={onClose}
      busy={locked}
      busyGuardHint={t("app.ext_busy_close_guard")}
      title={t("ext.preflight_title")}
      subtitle={t("ext.preflight_sub")}
      maxWidth="540px"
      footer={
        <>
          {busy ? (
            <button
              type="button"
              class="volo-btn volo-btn-danger"
              onClick={() => abortRef.current?.abort()}
            >
              {t("common.cancel")}
            </button>
          ) : (
            <button type="button" class="volo-btn" disabled={confirming} onClick={onClose}>
              {t("common.cancel")}
            </button>
          )}
          <button
            type="button"
            class="volo-btn volo-btn-primary"
            disabled={locked}
            onClick={() => void runZipInstall(Boolean(view?.installedVersion))}
          >
            {t("ext.preflight_confirm_btn")}
          </button>
        </>
      }
    >
      {view && (
        <div style={{ display: "flex", flexDirection: "column", gap: "10px" }}>
          <div class="volo-ext-catalog-card" style={{ flexDirection: "column", gap: "6px" }}>
            <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center" }}>
              <span class="volo-help">{t("ext.preflight_file")}:</span>
              <span class="volo-code-chip">{view.file.name}</span>
              {fileSizeLabel && <span class="volo-code-chip">{fileSizeLabel}</span>}
            </div>
            <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center" }}>
              <span class="volo-help">{t("ext.preflight_pkg")}:</span>
              <strong>{view.name}</strong>
              <span class="volo-code-chip">{view.id}</span>
              {view.author && <span class="volo-help">· {view.author}</span>}
            </div>
            <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", alignItems: "center" }}>
              <span class="volo-help">{t("ext.preflight_ver")}:</span>
              {view.installedVersion ? (
                <span class="volo-badge">
                  v{view.installedVersion} → v{view.version || "?"}
                </span>
              ) : (
                <span class="volo-badge volo-badge-ok">
                  {t("ext.preflight_new_install", { ver: view.version || "1.0.0" })}
                </span>
              )}
              {view.minHostVersion && (
                <span class="volo-code-chip">
                  {t("ext.preflight_host_ok", { min: view.minHostVersion })}
                </span>
              )}
            </div>
            {!view.parsedOk && <div class="volo-help">{t("ext.preflight_unparsed")}</div>}
            {view.sha256 && (
              <div style={{ display: "flex", flexDirection: "column", gap: "3px" }}>
                <span class="volo-help">{t("ext.preflight_sha256")}:</span>
                <code class="volo-code-chip" style={{ wordBreak: "break-all", fontSize: "11px" }}>
                  {view.sha256}
                </code>
              </div>
            )}
          </div>

          {view.installedVersion && (
            <div
              class="volo-help"
              style={{
                padding: "8px 10px",
                borderRadius: "8px",
                border: "1px solid color-mix(in srgb, var(--volo-warn) 40%, var(--volo-line))",
                background: "var(--volo-raised)",
              }}
            >
              {t("ext.preflight_overwrite_warn")}
            </div>
          )}

          <SwitchToggle
            checked={autoAfter}
            disabled={locked}
            onChange={setAutoAfter}
            label={view.kind === "plugin" ? t("app.ext_auto_enable") : t("app.ext_auto_activate")}
          />

          {busy && pct !== null && (
            <UploadProgressBar
              progress={pct}
              label={t("cnt.media_uploading_pct", { pct })}
              onCancel={() => abortRef.current?.abort()}
            />
          )}
          {!busy && errMsg && (
            <div role="alert" class="volo-help" style={{ color: "var(--volo-danger)" }}>
              {errMsg}
            </div>
          )}
        </div>
      )}
    </NativeModalDialog>
  );
}

export function AppearanceWorkspace({
  activeTab = "themes",
  onNavigate,
}: {
  activeTab?: AppearanceTab;
  onNavigate?: (sec: string) => void;
}) {
  const curLocale = localeSig.value;
  const [sub, setSub] = useState<AppearanceTab>(activeTab);
  const [loading, setLoading] = useState<boolean>(true);
  const [uploading, setUploading] = useState<boolean>(false);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  const locUploadAbortRef = useRef<AbortController | null>(null);
  const [triggeringCronKey, setTriggeringCronKey] = useState<string>("");
  const [remoteInstallKind, setRemoteInstallKind] = useState<RemoteExtKind | null>(null);
  const [updModalTarget, setUpdModalTarget] = useState<{
    kind: "plugin" | "theme";
    id: string;
    name: string;
    status: any;
  } | null>(null);
  const [batchModalTarget, setBatchModalTarget] = useState<{
    kind: "plugin" | "theme";
    items: BatchUpdateItem[];
  } | null>(null);
  const [zipPreflight, setZipPreflight] = useState<ZipPreflightInfo | null>(null);
  const [zipDropKind, setZipDropKind] = useState<"theme" | "plugin" | null>(null);
  const [checkingUpdates, setCheckingUpdates] = useState<boolean>(false);
  const [checkingSingleKey, setCheckingSingleKey] = useState<string>("");

  const [themes, setThemes] = useState<any[]>([]);
  const [themeQuery, setThemeQuery] = useState<string>("");
  const [themePage, setThemePage] = useState<number>(1);
  const [brokenPreviews, setBrokenPreviews] = useState<Record<string, boolean>>({});
  const [appCfg, setAppCfg] = useState<any>(null);
  const [appVer, setAppVer] = useState<number>(0);

  const [themeModal, setThemeModal] = useState<any | null>(null);
  const [themeModalOpen, setThemeModalOpen] = useState<boolean>(false);
  const [themeCfgVals, setThemeCfgVals] = useState<Record<string, any>>({});

  const [locPacks, setLocPacks] = useState<any[]>([]);
  const [localesDir, setLocalesDir] = useState<string>("");
  const [defaultLocale, setDefaultLocale] = useState<string>("zh-CN");
  const [siteVer, setSiteVer] = useState<number>(0);
  const [locPage, setLocPage] = useState<number>(1);
  const draftCssRef = useRef<string | null>(null);

  const [plugins, setPlugins] = useState<any[]>([]);
  const [pluginCronJobs, setPluginCronJobs] = useState<any[]>([]);
  const [reorderingPlugin, setReorderingPlugin] = useState<boolean>(false);
  const pluginFlip = useFlipList<HTMLTableSectionElement>();
  const [prioModal, setPrioModal] = useState<{ id: string; name: string } | null>(null);
  const [prioModalOpen, setPrioModalOpen] = useState<boolean>(false);
  const [prioValue, setPrioValue] = useState<string>("100");
  const [pluginQuery, setPluginQuery] = useState<string>("");
  const [pluginPage, setPluginPage] = useState<number>(1);
  const [cfgModal, setCfgModal] = useState<any | null>(null);
  const [cfgModalOpen, setCfgModalOpen] = useState<boolean>(false);
  const [cfgValues, setCfgValues] = useState<Record<string, any>>({});
  const [logsModal, setLogsModal] = useState<{ id: string; logs: any[] } | null>(null);
  const [logsModalOpen, setLogsModalOpen] = useState<boolean>(false);
  const [logLevelFilter, setLogLevelFilter] = useState<string>("");
  const [logTextFilter, setLogTextFilter] = useState<string>("");
  const [logPage, setLogPage] = useState<number>(1);
  const [logLimit, setLogLimit] = useState<number>(100);
  const [logsRefreshing, setLogsRefreshing] = useState<boolean>(false);

  const fetchPluginLogs = async (pluginId: string, limit = logLimit) => {
    setLogsRefreshing(true);
    try {
      const r = await apiFetch<{ items: any[] }>(
        `/api/admin/plugins/${encodeURIComponent(pluginId)}/logs?limit=${limit}`,
      );
      setLogsModal({ id: pluginId, logs: r.items || [] });
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setLogsRefreshing(false);
    }
  };

  const triggerSingleCronJob = async (pluginId: string, jobName: string) => {
    const key = `${pluginId}:${jobName}`;
    if (triggeringCronKey) return;
    setTriggeringCronKey(key);
    try {
      await apiFetch(`/api/admin/plugins/${encodeURIComponent(pluginId)}/cron`, {
        method: "POST",
        body: JSON.stringify({ name: jobName }),
      });
      showToast(t("app.plugin_cron_triggered", { count: 1 }), "ok");
      void loadData(true);
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setTriggeringCronKey("");
    }
  };

  const formatCronInterval = (secRaw: any): string => {
    const sec = Math.max(1, Number(secRaw || 0));
    if (sec % 86400 === 0) return `${sec / 86400}d`;
    if (sec % 3600 === 0) return `${sec / 3600}h`;
    if (sec % 60 === 0) return `${sec / 60}m`;
    return `${sec}s`;
  };

  const openPriorityEditor = (p: any) => {
    const curPrio = typeof p?.priority === "number" ? p.priority : 100;
    setPrioModal({ id: String(p.id), name: resolveSchemaText(p.name) || String(p.id) });
    setPrioValue(String(curPrio));
    setPrioModalOpen(true);
  };

  const movePluginOrder = async (pluginID: string, delta: number) => {
    if (reorderingPlugin || pluginQuery.trim() !== "") return;
    const idx = plugins.findIndex((item) => item.id === pluginID);
    const targetIdx = idx + delta;
    if (idx < 0 || targetIdx < 0 || targetIdx >= plugins.length) return;
    const prevPlugins = plugins;
    const next = [...plugins];
    const [moved] = next.splice(idx, 1);
    next.splice(targetIdx, 0, moved);
    setReorderingPlugin(true);
    pluginFlip.captureFlip();
    setPlugins(next);
    try {
      const res = await apiFetch<{ items?: any[]; cron_jobs?: any[] }>(
        `/api/admin/plugins/reorder?locale=${encodeURIComponent(localeSig.value)}`,
        {
          method: "POST",
          body: JSON.stringify({ ordered_ids: next.map((item) => String(item.id)) }),
        },
      );
      if (Array.isArray(res?.items)) setPlugins(res.items);
      if (Array.isArray(res?.cron_jobs)) setPluginCronJobs(res.cron_jobs);
    } catch (err: any) {
      pluginFlip.captureFlip();
      setPlugins(prevPlugins);
      showToast(err.message, "err");
      void loadData(true);
    } finally {
      setReorderingPlugin(false);
    }
  };

  const applyExtensionMutationResult = (res: any, kind: RemoteExtKind) => {
    if (kind === "theme") {
      setBrokenPreviews({});
      const nextThemes = Array.isArray(res?.themes) ? res.themes : Array.isArray(res?.items) ? res.items : null;
      if (nextThemes) setThemes(nextThemes);
      if (res?.appearance) {
        setAppCfg(
          draftCssRef.current !== null
            ? { ...res.appearance, custom_css: draftCssRef.current }
            : res.appearance,
        );
      }
      const nextVer = res?.appearance_version ?? res?.version;
      if (typeof nextVer === "number" && nextVer > 0) {
        setAppVer(nextVer);
      }
      if (!nextThemes) {
        void loadData(true);
      }
    } else if (kind === "plugin") {
      if (Array.isArray(res?.items)) {
        setPlugins(res.items);
        if (Array.isArray(res?.cron_jobs)) setPluginCronJobs(res.cron_jobs);
      } else {
        void loadData(true);
      }
    } else {
      if (Array.isArray(res?.locales)) {
        registerExternalLocales(res.locales);
        syncExternalLocales(res.locales);
      }
      if (Array.isArray(res?.items)) {
        setLocPacks(res.items);
      } else {
        void loadData(true);
      }
      if (typeof res?.site_version === "number" && res.site_version > 0) {
        setSiteVer(res.site_version);
      }
      void ensureLocaleLoaded(localeSig.value, true);
    }
  };

  const openZipPreflight = async (file: File, kind: "theme" | "plugin") => {
    const info = await inspectZipPreflight(file, kind, kind === "theme" ? themes : plugins);
    setZipPreflight(info);
  };

  const isItemOnlineUpdatable = (it: any): boolean => {
    const cap = String(it?.update_capability || "");
    if (cap === "none") return false;
    const st = it?.update_status;
    if (!st) return false;
    const hasUpd = Boolean(
      st.has_update || st.state === "update_available" || st.status === "update_available",
    );
    const canOnline = Boolean(st.online_updatable ?? st.can_online_update);
    const compat = st.compatible_with_host !== false;
    return hasUpd && canOnline && compat;
  };

  const updatableThemes: BatchUpdateItem[] = themes.filter(isItemOnlineUpdatable).map((th) => ({
    id: String(th.id),
    name: String(th.display_name || th.name || th.id),
    curVer: String(th.version || "1.0.0"),
    latestVer: String(th.update_status?.latest_version || ""),
  }));

  const updatablePlugins: BatchUpdateItem[] = plugins.filter(isItemOnlineUpdatable).map((p) => ({
    id: String(p.id),
    name: String(resolveSchemaText(p.name) || p.id),
    curVer: String(p.version || "1.0.0"),
    latestVer: String(p.update_status?.latest_version || ""),
  }));

  const runBatchUpdateCheck = async (kind: "plugin" | "theme") => {
    if (checkingUpdates) return;
    setCheckingUpdates(true);
    try {
      const endpoint =
        kind === "plugin"
          ? `/api/admin/plugins/check-updates?locale=${encodeURIComponent(localeSig.value)}`
          : `/api/admin/themes/check-updates?locale=${encodeURIComponent(localeSig.value)}`;
      const res = await apiFetch<any>(endpoint, {
        method: "POST",
        body: JSON.stringify({ force: true }),
      });
      if (kind === "plugin") {
        if (Array.isArray(res?.items)) setPlugins(res.items);
        if (Array.isArray(res?.cron_jobs)) setPluginCronJobs(res.cron_jobs);
      } else if (Array.isArray(res?.themes)) {
        setThemes(res.themes);
      }
      const s = res?.summary || {};
      const checked = Number(s.checked || 0);
      const updates = Number(s.updates || 0);
      const unsupported = Number(s.unsupported || 0);
      const errors = Number(s.errors || 0);
      if (updates > 0) {
        showToast(t("app.ext_check_summary_has_upd", { checked, updates, unsupported }), "ok");
      } else if (errors > 0) {
        showToast(t("app.ext_check_summary_err", { checked, errors }), "err");
      } else {
        showToast(t("app.ext_check_summary_latest", { checked, unsupported }), "ok");
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setCheckingUpdates(false);
    }
  };

  const runSingleUpdateCheck = async (kind: "plugin" | "theme", id: string, name: string) => {
    const key = `${kind}:${id}`;
    if (checkingSingleKey) return;
    setCheckingSingleKey(key);
    try {
      const endpoint =
        kind === "plugin"
          ? `/api/admin/plugins/${encodeURIComponent(id)}/check-update?locale=${encodeURIComponent(localeSig.value)}`
          : `/api/admin/themes/${encodeURIComponent(id)}/check-update?locale=${encodeURIComponent(localeSig.value)}`;
      const res = await apiFetch<any>(endpoint, {
        method: "POST",
        body: JSON.stringify({ force: true }),
      });
      if (kind === "plugin" && res?.plugin) {
        setPlugins((prev) => prev.map((it) => (it.id === id ? res.plugin : it)));
      } else if (kind === "theme" && Array.isArray(res?.themes)) {
        setThemes(res.themes);
      }
      const st = res?.status;
      if (st?.has_update) {
        setUpdModalTarget({ kind, id, name, status: st });
      } else if (st?.state === "error") {
        showToast(st.error || t("app.ext_check_err_short"), "err");
      } else if (st?.state === "up_to_date") {
        showToast(`${name}: ${t("app.ext_up_to_date")}`, "ok");
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setCheckingSingleKey("");
    }
  };

  useEffect(() => {
    if (activeTab !== "themes" && draftCssRef.current !== null && !dirtyEditorSig.value) {
      draftCssRef.current = null;
    }
    setSub(activeTab);
    resetConsoleScroll();
  }, [activeTab]);

  useEffect(() => {
    return () => {
      if (draftCssRef.current !== null) {
        draftCssRef.current = null;
        dirtyEditorSig.value = false;
      }
    };
  }, []);

  const loadData = async (silent = false) => {
    if (!silent) setLoading(true);
    try {
      if (sub === "themes") {
        const [th, st] = await Promise.all([
          apiFetch<{ themes?: any[]; items?: any[]; appearance?: any }>(
            `/api/admin/themes?locale=${encodeURIComponent(localeSig.value)}`,
          ),
          apiFetch<any>("/api/admin/settings"),
        ]);
        setThemes(th.themes || th.items || []);
        const nextApp = { ...(st.domains?.appearance || {}), ...(th.appearance || {}) };
        if (draftCssRef.current !== null) {
          nextApp.custom_css = draftCssRef.current;
        }
        setAppCfg(nextApp);
        setAppVer(st.versions?.appearance || 0);
      } else if (sub === "locales") {
        const [res, siteRes] = await Promise.all([
          apiFetch<{
            locales_dir?: string;
            default_locale?: string;
            site_version?: number;
            items?: any[];
            locales?: any[];
          }>("/api/admin/locales"),
          apiFetch<{ version?: number }>("/api/admin/settings/site").catch(() => ({ version: 0 })),
        ]);
        setLocPacks(res.items || []);
        setLocalesDir(res.locales_dir || "./locales");
        setDefaultLocale(res.default_locale || "zh-CN");
        setSiteVer(typeof res.site_version === "number" && res.site_version > 0 ? res.site_version : siteRes.version || 0);
        if (res.locales) {
          syncExternalLocales(res.locales);
        }
      } else {
        const pl = await apiFetch<{ items: any[]; cron_jobs?: any[] }>(
          `/api/admin/plugins?locale=${encodeURIComponent(localeSig.value)}`,
        );
        setPlugins(pl.items || []);
        setPluginCronJobs(pl.cron_jobs || []);
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void loadData();
  }, [sub, curLocale]);

  const activateTheme = async (themeID: string, target: "desktop" | "mobile" = "desktop") => {
    if (!appCfg) return;
    try {
      const res = await apiFetch<{ appearance?: any; version?: number }>("/api/admin/themes/activate", {
        method: "POST",
        body: JSON.stringify({ theme_id: themeID, target }),
      });
      if (res?.appearance) {
        setAppCfg(
          draftCssRef.current !== null
            ? { ...res.appearance, custom_css: draftCssRef.current }
            : res.appearance,
        );
      }
      if (typeof res?.version === "number") setAppVer(res.version);
      showToast(t("common.saved"), "ok");
    } catch (err: any) {
      showToast(err.message, "err");
    }
  };

  const [exportingCode, setExportingCode] = useState<string>("");

  const downloadStarterTemplate = async (base: "zh-CN" | "en-US") => {
    const jsonText = await exportFullTemplateJSON(base);
    const blob = new Blob([jsonText], { type: "application/json;charset=utf-8" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `volo-locale-template-${base}.json`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  const handleExportLocale = async (code: string) => {
    if (exportingCode) return;
    setExportingCode(code);
    try {
      const jsonText = await exportLocalePackJSON(code);
      const blob = new Blob([jsonText], { type: "application/json;charset=utf-8" });
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = `${code}.json`;
      a.click();
      URL.revokeObjectURL(a.href);
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setExportingCode("");
    }
  };

  const themeSchema: any[] = themeModal?.localized_schema || themeModal?.config_schema || [];
  const pluginSchema: any[] = cfgModal?.localized_schema || cfgModal?.config_schema || [];

  const themeNeedle = themeQuery.trim().toLowerCase();
  const filteredThemes = themeNeedle
    ? themes.filter((th) => {
        const hay = `${th.id || ""} ${th.display_name || ""} ${th.name || ""} ${th.author || ""} ${th.display_description || ""} ${th.description || ""}`.toLowerCase();
        return hay.includes(themeNeedle);
      })
    : themes;
  const pagedThemes = paginateItems(filteredThemes, themePage, 12);

  const pluginNeedle = pluginQuery.trim().toLowerCase();
  const filteredPlugins = pluginNeedle
    ? plugins.filter((p) => {
        const hay = `${p.id || ""} ${resolveSchemaText(p.name)} ${resolveSchemaText(p.description)} ${p.kind || ""} ${p.runtime || ""}`.toLowerCase();
        return hay.includes(pluginNeedle);
      })
    : plugins;
  const pagedPlugins = paginateItems(filteredPlugins, pluginPage, 12);

  const filteredLogs = (logsModal?.logs || []).filter((l: any) => {
    const lvl = String(l?.level || "info").toLowerCase();
    if (logLevelFilter && lvl !== logLevelFilter.toLowerCase()) return false;
    const q = logTextFilter.trim().toLowerCase();
    if (!q) return true;
    const msg = String(l?.msg ?? l?.message ?? "");
    const ev = String(l?.event ?? "");
    const ts = String(l?.ts ?? "");
    return `${ts} ${lvl} ${ev} ${msg}`.toLowerCase().includes(q);
  });

  return (
    <div>
      <div class="volo-subtabs">
        {(["themes", "locales", "plugins"] as AppearanceTab[]).map((tab) => (
          <button
            key={tab}
            type="button"
            class={`volo-tab ${sub === tab ? "active" : ""}`}
            onClick={() => {
              setSub(tab);
              resetConsoleScroll();
              onNavigate?.(tab);
            }}
          >
            {tab === "themes" ? t("app.themes") : tab === "locales" ? t("app.locales") : t("app.plugins")}
          </button>
        ))}
      </div>

      {sub === "themes" && appCfg && (
        <>
          <div
            class={`volo-card volo-zip-dropzone ${zipDropKind === "theme" ? "is-zip-dropover" : ""}`}
            onDragOver={(e) => {
              if (e.dataTransfer?.types?.includes("Files")) {
                e.preventDefault();
                if (zipDropKind !== "theme") setZipDropKind("theme");
              }
            }}
            onDragLeave={(e) => {
              if (!e.currentTarget.contains(e.relatedTarget as Node)) {
                setZipDropKind(null);
              }
            }}
            onDrop={(e) => {
              setZipDropKind(null);
              if (e.defaultPrevented || !e.dataTransfer?.files?.length) return;
              const dropped = Array.from(e.dataTransfer.files).find((f) => /\.zip$/i.test(f.name));
              if (!dropped) return;
              e.preventDefault();
              void openZipPreflight(dropped, "theme");
            }}
          >
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("app.gallery_head")}</h3>
                <p class="volo-sec-desc">
                  {t("app.gallery_sub")} · <span class="volo-help">{t("ext.drop_zip_hint")}</span>
                </p>
              </div>
              <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
                <input
                  type="search"
                  class="volo-input"
                  style={{ width: "210px", maxWidth: "100%" }}
                  placeholder={t("app.theme_search_ph")}
                  value={themeQuery}
                  onInput={(e) => {
                    setThemeQuery((e.currentTarget as HTMLInputElement).value);
                    setThemePage(1);
                  }}
                />
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  onClick={async () => {
                    try {
                      const res = await apiFetch<{ themes?: any[]; appearance?: any; appearance_version?: number }>("/api/admin/themes/reload", {
                        method: "POST",
                        body: "{}",
                      });
                      applyExtensionMutationResult(res, "theme");
                      showToast(t("app.themes_reloaded"), "ok");
                    } catch (err: any) {
                      showToast(err.message, "err");
                    }
                  }}
                >
                  {t("app.themes_reload")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  disabled={checkingUpdates}
                  onClick={() => void runBatchUpdateCheck("theme")}
                >
                  {checkingUpdates ? t("app.ext_checking") : t("app.ext_check_all")}
                </button>
                <button
                  type="button"
                  class="volo-btn volo-btn-sm"
                  onClick={() => setRemoteInstallKind("theme")}
                >
                  {t("app.ext_url_install_btn")}
                </button>
                <UploadButton
                  accept=".zip"
                  primary
                  small
                  busy={uploading}
                  label={t("app.install_theme_zip")}
                  onFile={(f) => void openZipPreflight(f, "theme")}
                />
              </div>
            </div>
            {updatableThemes.length >= 2 && (
              <div
                class="volo-ext-catalog-card"
                style={{ alignItems: "center", marginBottom: "14px" }}
              >
                <span style={{ fontSize: "13px", fontWeight: 500 }}>
                  {t("ext.update_banner_msg", { n: updatableThemes.length })}
                </span>
                <button
                  type="button"
                  class="volo-btn volo-btn-sm volo-btn-primary"
                  onClick={() => setBatchModalTarget({ kind: "theme", items: updatableThemes })}
                >
                  {t("ext.update_all_btn", { n: updatableThemes.length })}
                </button>
              </div>
            )}

            <div class="volo-theme-grid">
              {pagedThemes.slice.map((th) => {
                const isBuiltin = Boolean(th.is_builtin || th.builtin);
                const isActive = (appCfg.active_theme || "volo-studio") === th.id;
                const isMobileActive = Boolean(
                  appCfg.mobile_theme && appCfg.mobile_theme !== "same" && appCfg.mobile_theme === th.id,
                );
                const schema: any[] = th.localized_schema || th.config_schema || [];
                const previewSrc = safeMediaSrc(th.preview_url) || `/themes/${encodeURIComponent(th.id)}/preview`;
                const title = th.display_name || th.name || th.id;
                const desc = th.display_description || th.description || "";
                const hasBrokenImg = Boolean(brokenPreviews[th.id]);
                const updCap = String(th.update_capability || "none");
                const updSt = th.update_status || null;
                return (
                  <div key={th.id} class={`volo-theme-card ${isActive || isMobileActive ? "active" : ""}`}>
                    <div class="volo-theme-thumb">
                      {!hasBrokenImg ? (
                        <img
                          src={previewSrc}
                          alt={title}
                          loading="lazy"
                          onError={() => setBrokenPreviews((prev) => ({ ...prev, [th.id]: true }))}
                        />
                      ) : (
                        <div
                          style={{
                            width: "100%",
                            height: "100%",
                            minHeight: "140px",
                            display: "flex",
                            flexDirection: "column",
                            alignItems: "center",
                            justifyContent: "center",
                            gap: "4px",
                            background: "color-mix(in srgb, var(--volo-accent) 9%, var(--volo-raised))",
                            color: "var(--volo-muted)",
                            padding: "12px",
                            textAlign: "center",
                          }}
                        >
                          <strong style={{ color: "var(--volo-text)", fontSize: "14px" }}>{title}</strong>
                          <span class="volo-code-chip">{th.id}</span>
                        </div>
                      )}
                      <div class="volo-theme-badges">
                        {isActive && <span class="volo-badge volo-badge-ok">{t("app.theme_active_badge")}</span>}
                        {isMobileActive && (
                          <span class="volo-badge volo-badge-ok">{t("app.theme_mobile_badge")}</span>
                        )}
                        <span class="volo-badge">
                          {isBuiltin ? t("app.theme_builtin") : t("app.theme_external")}
                        </span>
                        {updCap !== "none" && updSt?.state === "up_to_date" && (
                          <span class="volo-badge volo-badge-ok">{t("app.ext_up_to_date")}</span>
                        )}
                      </div>
                    </div>
                    <div class="volo-theme-body">
                      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "8px" }}>
                        <strong style={{ fontSize: "15px" }}>{title}</strong>
                        <span class="volo-help">
                          v{th.version || "1.0.0"} · {th.author || "Volo"}
                          {safeHttpUrl(th.homepage) && (
                            <>
                              {" · "}
                              <a
                                href={safeHttpUrl(th.homepage)}
                                target="_blank"
                                rel="noopener noreferrer"
                                style={{ color: "var(--volo-accent)" }}
                              >
                                ↗
                              </a>
                            </>
                          )}
                        </span>
                      </div>
                      {updCap !== "none" && updSt?.has_update && (
                        <div>
                          <button
                            type="button"
                            class={`volo-btn volo-btn-sm ${updSt.online_updatable ? "volo-btn-primary" : ""}`}
                            onClick={() =>
                              setUpdModalTarget({
                                kind: "theme",
                                id: String(th.id),
                                name: String(title),
                                status: updSt,
                              })
                            }
                          >
                            {`v${th.version || "1.0.0"} → v${updSt.latest_version} · ${
                              updSt.online_updatable ? t("app.ext_update_now") : t("app.ext_manual_update_badge")
                            }`}
                          </button>
                        </div>
                      )}
                      {updCap !== "none" && !updSt?.has_update && updSt?.state === "error" && (
                        <div>
                          <span class="volo-status-pill danger" title={updSt.error || ""}>
                            <span class="volo-status-dot" />
                            {t("app.ext_check_err_short")}
                          </span>
                        </div>
                      )}
                      <div class="volo-help" style={{ flex: 1 }}>{desc}</div>
                      <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", marginTop: "6px" }}>
                        {!isActive && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm volo-btn-primary"
                            onClick={() => void activateTheme(th.id, "desktop")}
                          >
                            {t("app.theme_activate")}
                          </button>
                        )}
                        {!isMobileActive ? (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm"
                            onClick={() => void activateTheme(th.id, "mobile")}
                          >
                            {t("app.theme_activate_mobile")}
                          </button>
                        ) : (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm"
                            onClick={() => void activateTheme("same", "mobile")}
                          >
                            {t("app.mobile_follow_desktop")}
                          </button>
                        )}
                        <a
                          class="volo-btn volo-btn-sm"
                          href={`/?skin=${encodeURIComponent(th.id)}`}
                          target="_blank"
                          rel="noreferrer"
                        >
                          {t("common.preview")} ↗
                        </a>
                        <button
                          type="button"
                          class="volo-btn volo-btn-sm"
                          onClick={() =>
                            void downloadZipBlob(
                              `/api/admin/themes/${encodeURIComponent(th.id)}/export`,
                              `${th.id}.zip`,
                            )
                          }
                        >
                          {t("app.theme_export_zip")}
                        </button>
                        {updCap !== "none" && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm"
                            disabled={checkingSingleKey === `theme:${th.id}`}
                            onClick={() => void runSingleUpdateCheck("theme", String(th.id), String(title))}
                          >
                            {checkingSingleKey === `theme:${th.id}` ? t("app.ext_checking") : t("app.ext_check_one")}
                          </button>
                        )}
                        {schema.length > 0 && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm"
                            onClick={() => {
                              const defaults = buildSchemaDefaults(schema);
                              const saved = appCfg?.theme_configs?.[th.id] || th.config_values || {};
                              setThemeCfgVals({
                                ...defaults,
                                ...(th.config_values || {}),
                                ...saved,
                              });
                              setThemeModal(th);
                              setThemeModalOpen(true);
                            }}
                          >
                            {t("app.theme_settings")}
                          </button>
                        )}
                        {!isBuiltin && !isActive && !isMobileActive && (
                          <button
                            type="button"
                            class="volo-btn volo-btn-sm volo-btn-danger"
                            onClick={async () => {
                              if (
                                !(await openConfirm({
                                  message: t("app.theme_uninstall_confirm", { id: th.id }),
                                  danger: true,
                                }))
                              ) {
                                return;
                              }
                              try {
                                await apiFetch(`/api/admin/themes/${encodeURIComponent(th.id)}`, {
                                  method: "DELETE",
                                });
                                showToast(t("common.deleted"), "ok");
                                void loadData(true);
                              } catch (err: any) {
                                showToast(err.message, "err");
                              }
                            }}
                          >
                            {t("app.uninstall")}
                          </button>
                        )}
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
            <Pagination
              total={filteredThemes.length}
              page={pagedThemes.safePage}
              pageSize={12}
              onChange={(p) => {
                setThemePage(p);
                resetConsoleScroll();
              }}
            />
          </div>

          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("app.themes_head")}</h3>
                <p class="volo-sec-desc">{t("app.themes_desc")}</p>
              </div>
            </div>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("app.default_mode")}</label>
                <ConsoleSelect
                  value={appCfg.default_mode || "auto"}
                  options={[
                    { value: "auto", label: t("app.mode_auto") },
                    { value: "system", label: t("app.mode_system") },
                    { value: "light", label: t("app.mode_light") },
                    { value: "dark", label: t("app.mode_dark") },
                  ]}
                  onChange={(v) => setAppCfg({ ...appCfg, default_mode: v })}
                />
              </div>
              <div class="volo-field">
                <label>{t("app.mobile_theme_label")}</label>
                <ConsoleSelect
                  value={appCfg.mobile_theme === "same" ? "" : appCfg.mobile_theme || ""}
                  options={[
                    { value: "", label: t("app.mobile_follow_desktop") },
                    ...themes.map((th) => ({
                      value: String(th.id),
                      label: String(th.display_name || th.name || th.id),
                    })),
                  ]}
                  onChange={(v) => setAppCfg({ ...appCfg, mobile_theme: v })}
                />
              </div>
            </div>

            <div class="volo-field">
              <label>{t("app.shared_accent")}</label>
              <div style={{ display: "flex", gap: "6px", flexWrap: "wrap" }}>
                {ACCENTS.map((ac) => (
                  <button
                    key={ac.id}
                    type="button"
                    class={`volo-btn volo-btn-sm ${(appCfg.accent || accentSig.value) === ac.id ? "volo-btn-primary" : ""}`}
                    onClick={() => {
                      setAppCfg({ ...appCfg, accent: ac.id });
                    }}
                  >
                    <span class="volo-swatch-dot" style={{ background: ac.swatch, width: "11px", height: "11px" }} />
                    {renderAccentLabel(ac.id)}
                  </button>
                ))}
              </div>
            </div>

            <div class="volo-field">
              <label>{t("app.custom_css")}</label>
              <CM6CssEditor
                value={appCfg.custom_css || ""}
                onChange={(css) => {
                  if (css !== (appCfg.custom_css || "")) {
                    draftCssRef.current = css;
                    dirtyEditorSig.value = true;
                  }
                  setAppCfg({ ...appCfg, custom_css: css });
                }}
              />
            </div>

            <div class="volo-form-actions">
              <a
                class="volo-btn"
                href={`/?skin=${encodeURIComponent(appCfg.active_theme || "volo-studio")}&device=desktop`}
                target="_blank"
                rel="noreferrer"
              >
                {t("app.preview_theme")} ↗
              </a>
              {appCfg.mobile_theme && appCfg.mobile_theme !== "same" && (
                <a
                  class="volo-btn"
                  href={`/?skin=${encodeURIComponent(appCfg.mobile_theme)}&device=mobile`}
                  target="_blank"
                  rel="noreferrer"
                >
                  {t("app.preview_mobile_theme")} ↗
                </a>
              )}
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={async () => {
                  try {
                    const nextAccent = appCfg.accent || accentSig.value || "forest";
                    const res = await apiFetch<any>("/api/admin/settings/appearance", {
                      method: "PATCH",
                      body: JSON.stringify({ expected_version: appVer, ...appCfg, accent: nextAccent }),
                    });
                    draftCssRef.current = null;
                    dirtyEditorSig.value = false;
                    setAppCfg(res.value || { ...appCfg, accent: nextAccent });
                    setAppVer(res.version);
                    const hasLocalMode = (() => {
                      try {
                        return Boolean(localStorage.getItem("volo_theme_mode"));
                      } catch {
                        return false;
                      }
                    })();
                    const nextMode =
                      !hasLocalMode && (res.value?.default_mode || appCfg.default_mode)
                        ? String(res.value?.default_mode || appCfg.default_mode)
                        : themeModeSig.value;
                    applyThemeAttributes(nextMode, nextAccent, false);
                    showToast(t("common.saved"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("common.save")}
              </button>
            </div>
          </div>
        </>
      )}

      {sub === "locales" && (
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.loc_head")}</h3>
              <p class="volo-sec-desc">
                {t("app.loc_desc")}{" "}
                <span class="volo-code-chip" style={{ verticalAlign: "middle" }}>
                  {t("app.loc_dir_label")} {localesDir || "./locales"}
                </span>
              </p>
            </div>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => setRemoteInstallKind("locale")}
              >
                {t("ext.install_url_btn")}
              </button>
              <UploadButton
                accept=".json,.zip"
                primary
                small
                busy={uploading}
                label={t("app.loc_install_btn")}
                onFile={async (f) => {
                  const ctrl = new AbortController();
                  locUploadAbortRef.current = ctrl;
                  setUploading(true);
                  setUploadPct(0);
                  try {
                    const fd = new FormData();
                    fd.append("file", f);
                    const res = await uploadWithProgress<{ items?: any[]; locales?: any[]; site_version?: number }>(
                      "/api/admin/locales/install",
                      fd,
                      setUploadPct,
                      "POST",
                      ctrl.signal,
                    );
                    applyExtensionMutationResult(res, "locale");
                    showToast(t("app.loc_installed"), "ok");
                  } catch (err: any) {
                    if (!isAbortError(err)) {
                      showToast(err.message, "err");
                    }
                  } finally {
                    locUploadAbortRef.current = null;
                    setUploading(false);
                    setUploadPct(null);
                  }
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={async () => {
                  try {
                    const res = await apiFetch<{ items?: any[]; locales?: any[]; site_version?: number }>("/api/admin/locales/reload", {
                      method: "POST",
                      body: "{}",
                    });
                    applyExtensionMutationResult(res, "locale");
                    showToast(t("app.loc_reloaded"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("app.loc_reload_btn")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void downloadStarterTemplate("zh-CN")}>
                {t("app.loc_tpl_zh")}
              </button>
              <button type="button" class="volo-btn volo-btn-sm" onClick={() => void downloadStarterTemplate("en-US")}>
                {t("app.loc_tpl_en")}
              </button>
            </div>
          </div>
          {uploading && uploadPct !== null && (
            <div style={{ marginBottom: "12px" }}>
              <UploadProgressBar
                progress={uploadPct}
                label={t("cnt.media_uploading_pct", { pct: uploadPct })}
                onCancel={() => locUploadAbortRef.current?.abort()}
              />
            </div>
          )}

          <div class="volo-table-wrap">
            <table class="volo-table volo-table-compact">
              <thead>
                <tr>
                  <th class="col-fluid">{t("app.loc_col_lang")}</th>
                  <th class="col-cat">{t("app.loc_col_code")}</th>
                  <th class="col-status">{t("app.loc_col_source")}</th>
                  <th class="col-num">{t("app.loc_col_keys")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
                {loading ? (
                  <TableSkeleton cols={5} />
                ) : locPacks.length === 0 ? (
                  <ConsoleEmpty colSpan={5} />
                ) : (
                  pagedSlice(locPacks, locPage, 12).map((p) => {
                    const isDef = p.code === defaultLocale;
                    const isUI = p.code === curLocale;
                    const srcLabel =
                      p.source === "external"
                        ? t("app.loc_src_external")
                        : p.source === "override"
                          ? t("app.loc_src_override")
                          : t("app.loc_src_builtin");
                    return (
                      <tr key={p.code}>
                        <td class="col-fluid" data-label={t("app.loc_col_lang")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{p.name || p.code}</span>
                              {isDef && <span class="volo-mini-tag">{t("app.loc_badge_default")}</span>}
                              {isUI && <span class="volo-mini-tag warn">{t("app.loc_badge_ui")}</span>}
                            </div>
                            <div class="volo-cell-meta">
                              {p.author && <span>{p.author}</span>}
                              {p.version && <span>v{p.version}</span>}
                              {p.file && <span class="volo-code-chip">{p.file}</span>}
                            </div>
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("app.loc_col_code")}>
                          <div class="volo-cell-meta">
                            <span class="volo-code-chip">{p.code}</span>
                            <span class="volo-cat-chip">{String(p.dir || "ltr").toUpperCase()}</span>
                          </div>
                        </td>
                        <td class="col-status" data-label={t("app.loc_col_source")}>
                          <span class={`volo-status-pill ${p.source === "external" ? "ok" : p.source === "override" ? "warn" : ""}`}>
                            <span class="volo-status-dot" />
                            {srcLabel}
                          </span>
                        </td>
                        <td class="col-num" data-label={t("app.loc_col_keys")}>
                          <div class="volo-cell-main">
                            <span>{t("app.loc_keys_stat", { total: p.total_keys ?? 0 })}</span>
                            {p.custom_keys > 0 && (
                              <span class="volo-help">{t("app.loc_custom_stat", { custom: p.custom_keys })}</span>
                            )}
                          </div>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            {!isUI && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={async () => {
                                  const next = await ensureLocaleLoaded(p.code, true);
                                  await apiFetch("/api/admin/me", {
                                    method: "PATCH",
                                    body: JSON.stringify({ preferred_locale: next }),
                                  }).catch(() => {});
                                }}
                              >
                                {t("app.loc_use_ui")}
                              </button>
                            )}
                            <button
                              type="button"
                              class="volo-act-btn"
                              disabled={exportingCode === p.code}
                              onClick={() => void handleExportLocale(p.code)}
                            >
                              {t("app.loc_export")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("app.loc_set_default"),
                                  hidden: isDef,
                                  onClick: async () => {
                                    try {
                                      const saved = await apiFetch<{ version?: number }>("/api/admin/settings/site", {
                                        method: "PATCH",
                                        body: JSON.stringify({
                                          ...(siteVer > 0 ? { expected_version: siteVer } : {}),
                                          default_locale: p.code,
                                        }),
                                      });
                                      if (typeof saved?.version === "number") setSiteVer(saved.version);
                                      setDefaultLocale(p.code);
                                      showToast(t("common.saved"), "ok");
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
                                  },
                                },
                                {
                                  label: t("app.loc_remove"),
                                  hidden: !p.has_external,
                                  danger: true,
                                  onClick: async () => {
                                    if (
                                      !(await openConfirm({
                                        message: t("app.loc_confirm_del", { code: p.code }),
                                        danger: true,
                                      }))
                                    ) {
                                      return;
                                    }
                                    try {
                                      const res = await apiFetch<{ items?: any[]; locales?: any[]; site_version?: number }>(
                                        `/api/admin/locales/${encodeURIComponent(p.code)}`,
                                        { method: "DELETE" },
                                      );
                                      applyExtensionMutationResult(res, "locale");
                                      showToast(t("app.loc_deleted"), "ok");
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
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
            total={locPacks.length}
            page={locPage}
            pageSize={12}
            onChange={(p) => {
              setLocPage(p);
              resetConsoleScroll();
            }}
          />
        </div>
      )}

      {sub === "plugins" && (
        <div
          class={`volo-card volo-zip-dropzone ${zipDropKind === "plugin" ? "is-zip-dropover" : ""}`}
          onDragOver={(e) => {
            if (e.dataTransfer?.types?.includes("Files")) {
              e.preventDefault();
              if (zipDropKind !== "plugin") setZipDropKind("plugin");
            }
          }}
          onDragLeave={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node)) {
              setZipDropKind(null);
            }
          }}
          onDrop={(e) => {
            setZipDropKind(null);
            if (e.defaultPrevented || !e.dataTransfer?.files?.length) return;
            const dropped = Array.from(e.dataTransfer.files).find((f) => /\.zip$/i.test(f.name));
            if (!dropped) return;
            e.preventDefault();
            void openZipPreflight(dropped, "plugin");
          }}
        >
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.plugins_head")}</h3>
              <p class="volo-sec-desc">
                {t("app.plugins_desc")} · <span class="volo-help">{t("ext.drop_zip_hint")}</span>
              </p>
            </div>
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center" }}>
              <input
                type="search"
                class="volo-input"
                style={{ width: "210px", maxWidth: "100%" }}
                placeholder={t("app.plugin_search_ph")}
                value={pluginQuery}
                onInput={(e) => {
                  setPluginQuery((e.currentTarget as HTMLInputElement).value);
                  setPluginPage(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={async () => {
                  try {
                    let res: { items?: any[]; cron_jobs?: any[] } | null = null;
                    try {
                      res = await apiFetch<{ items?: any[]; cron_jobs?: any[] }>(
                        `/api/admin/plugins/discover?locale=${encodeURIComponent(localeSig.value)}`,
                        { method: "POST", body: "{}" },
                      );
                    } catch {
                      res = await apiFetch<{ items?: any[]; cron_jobs?: any[] }>(
                        `/api/admin/plugins/reload?locale=${encodeURIComponent(localeSig.value)}`,
                        { method: "POST", body: "{}" },
                      );
                    }
                    applyExtensionMutationResult(res, "plugin");
                    showToast(t("app.plugins_discovered"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("app.plugins_discover")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                disabled={checkingUpdates}
                onClick={() => void runBatchUpdateCheck("plugin")}
              >
                {checkingUpdates ? t("app.ext_checking") : t("app.ext_check_all")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-sm"
                onClick={() => setRemoteInstallKind("plugin")}
              >
                {t("app.ext_url_install_btn")}
              </button>
              <UploadButton
                accept=".zip"
                primary
                small
                busy={uploading}
                label={t("app.install_zip")}
                onFile={(f) => void openZipPreflight(f, "plugin")}
              />
            </div>
          </div>
          {updatablePlugins.length >= 2 && (
            <div
              class="volo-ext-catalog-card"
              style={{ alignItems: "center", marginBottom: "14px" }}
            >
              <span style={{ fontSize: "13px", fontWeight: 500 }}>
                {t("ext.update_banner_msg", { n: updatablePlugins.length })}
              </span>
              <button
                type="button"
                class="volo-btn volo-btn-sm volo-btn-primary"
                onClick={() => setBatchModalTarget({ kind: "plugin", items: updatablePlugins })}
              >
                {t("ext.update_all_btn", { n: updatablePlugins.length })}
              </button>
            </div>
          )}

          <div class="volo-table-wrap">
            <table class="volo-table volo-table-compact">
              <thead>
                <tr>
                  <th class="col-fluid">{t("app.plugin_col_info")}</th>
                  <th class="col-chain">{t("app.plugin_col_chain")}</th>
                  <th class="col-cat">{t("app.plugin_col_runtime")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody ref={pluginFlip.containerRef}>
                {loading ? (
                  <TableSkeleton cols={5} />
                ) : filteredPlugins.length === 0 ? (
                  <ConsoleEmpty colSpan={5} text={t("app.plugins_empty")} />
                ) : (
                  pagedPlugins.slice.map((p) => {
                    const pSchema: any[] = p.localized_schema || p.config_schema || [];
                    const isCircuitOpen = p.circuit_state === "open" || p.circuit_state === "CIRCUIT_OPEN";
                    const isBuiltinPlugin = Boolean(p.is_builtin || p.builtin || p.kind === "builtin");
                    const chainIdx = plugins.findIndex((item) => item.id === p.id);
                    const priorityNum = typeof p.priority === "number" ? p.priority : 100;
                    const isFiltered = Boolean(pluginNeedle);
                    const updCap = String(p.update_capability || "none");
                    const updSt = p.update_status || null;
                    const pDisplayName = String(resolveSchemaText(p.name) || p.id);
                    const rowCronJobs: any[] =
                      Array.isArray(p.cron_jobs) && p.cron_jobs.length > 0
                        ? p.cron_jobs
                        : pluginCronJobs.filter((j) => j?.plugin_id === p.id);
                    return (
                      <tr key={p.id} data-flip-key={p.id}>
                        <td class="col-fluid" data-label={t("app.plugin_col_info")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{resolveSchemaText(p.name) || p.id}</span>
                              <span class="volo-code-chip">{p.id}</span>
                              {safeHttpUrl(p.homepage) && (
                                <a
                                  class="volo-code-chip"
                                  href={safeHttpUrl(p.homepage)}
                                  target="_blank"
                                  rel="noopener noreferrer"
                                >
                                  ↗
                                </a>
                              )}
                            </div>
                            {resolveSchemaText(p.description) && (
                              <div class="volo-cell-meta"><span>{resolveSchemaText(p.description)}</span></div>
                            )}
                            {rowCronJobs.length > 0 && (
                              <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", marginTop: "4px", alignItems: "center" }}>
                                {rowCronJobs.map((job) => {
                                  const jobName = String(job.name || "cron");
                                  const isRunning = Boolean(job.running || job.is_running || triggeringCronKey === `${p.id}:${jobName}`);
                                  const rawLast = job.last_run_at;
                                  const lastRunLabel =
                                    rawLast && rawLast !== "0" && Number(rawLast) !== 0
                                      ? formatDateTime(
                                          typeof rawLast === "number" && rawLast > 0 && rawLast < 1e12
                                            ? rawLast * 1000
                                            : rawLast,
                                        )
                                      : "";
                                  return (
                                    <span
                                      key={jobName}
                                      class="volo-code-chip"
                                      title={job.last_error ? `${jobName}: ${job.last_error}` : job.handler || jobName}
                                      style={{
                                        display: "inline-flex",
                                        alignItems: "center",
                                        gap: "6px",
                                        ...(job.last_error
                                          ? { borderColor: "var(--volo-danger)", color: "var(--volo-danger)" }
                                          : {}),
                                      }}
                                    >
                                      <span>
                                        {t("app.plugin_cron_badge", {
                                          name: jobName,
                                          interval: formatCronInterval(job.interval_seconds),
                                          runs: Number(job.run_count || 0),
                                        })}
                                        {lastRunLabel ? ` · ${lastRunLabel}` : ""}
                                      </span>
                                      {isRunning && (
                                        <span class="volo-status-pill warn" style={{ padding: "0 6px", fontSize: "11px" }}>
                                          <span class="volo-status-dot" />
                                          {t("app.plugin_cron_running")}
                                        </span>
                                      )}
                                      {Boolean(p.is_enabled) && (
                                        <button
                                          type="button"
                                          class="volo-act-btn"
                                          style={{ padding: "1px 6px", height: "20px", fontSize: "11px" }}
                                          disabled={isRunning || Boolean(triggeringCronKey)}
                                          onClick={() => void triggerSingleCronJob(String(p.id), jobName)}
                                        >
                                          ▶ {t("app.plugin_cron_run_one")}
                                        </button>
                                      )}
                                    </span>
                                  );
                                })}
                              </div>
                            )}
                            {p.last_error && (
                              <div
                                style={{
                                  color: "var(--volo-danger)",
                                  fontSize: "12px",
                                  overflowWrap: "anywhere",
                                  wordBreak: "break-word",
                                }}
                              >
                                {t("app.last_error", { err: p.last_error })}
                              </div>
                            )}
                          </div>
                        </td>
                        <td class="col-chain" data-label={t("app.plugin_col_chain")}>
                          <div class="volo-chain-ctrl">
                            <button
                              type="button"
                              class="volo-act-btn volo-prio-btn"
                              title={t("app.plugin_chain_hint")}
                              onClick={() => openPriorityEditor(p)}
                            >
                              #{chainIdx >= 0 ? chainIdx + 1 : 1} · P{priorityNum}
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              title={isFiltered ? t("ui.sort_disabled_filtered") : t("app.move_up")}
                              aria-label={t("app.move_up")}
                              disabled={isFiltered || reorderingPlugin || chainIdx <= 0}
                              onClick={() => void movePluginOrder(p.id, -1)}
                            >
                              ↑
                            </button>
                            <button
                              type="button"
                              class="volo-act-btn"
                              title={isFiltered ? t("ui.sort_disabled_filtered") : t("app.move_down")}
                              aria-label={t("app.move_down")}
                              disabled={isFiltered || reorderingPlugin || chainIdx < 0 || chainIdx >= plugins.length - 1}
                              onClick={() => void movePluginOrder(p.id, 1)}
                            >
                              ↓
                            </button>
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("app.plugin_col_runtime")}>
                          <div style={{ display: "flex", flexDirection: "column", gap: "4px", alignItems: "flex-start" }}>
                            <span class="volo-cat-chip">
                              {p.kind} · {p.runtime} · v{p.version}
                            </span>
                            {updCap !== "none" && updSt?.has_update && (
                              <button
                                type="button"
                                class={`volo-btn volo-btn-sm ${updSt.online_updatable ? "volo-btn-primary" : ""}`}
                                onClick={() =>
                                  setUpdModalTarget({
                                    kind: "plugin",
                                    id: String(p.id),
                                    name: pDisplayName,
                                    status: updSt,
                                  })
                                }
                              >
                                {`v${p.version} → v${updSt.latest_version} · ${
                                  updSt.online_updatable ? t("app.ext_update_now") : t("app.ext_manual_update_badge")
                                }`}
                              </button>
                            )}
                            {updCap !== "none" && !updSt?.has_update && updSt?.state === "up_to_date" && (
                              <span class="volo-badge volo-badge-ok">{t("app.ext_up_to_date")}</span>
                            )}
                            {updCap !== "none" && !updSt?.has_update && updSt?.state === "error" && (
                              <span class="volo-status-pill danger" title={updSt.error || ""}>
                                <span class="volo-status-dot" />
                                {t("app.ext_check_err_short")}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-status" data-label={t("art.status")}>
                          <div class="volo-plugin-status-stack">
                            <SwitchToggle
                              checked={Boolean(p.is_enabled)}
                              label={p.is_enabled ? t("app.enabled") : t("app.disabled")}
                              onChange={async () => {
                                try {
                                  await apiFetch(
                                    `/api/admin/plugins/${encodeURIComponent(p.id)}/${p.is_enabled ? "disable" : "enable"}`,
                                    { method: "POST", body: "{}" },
                                  );
                                  void loadData(true);
                                } catch (err: any) {
                                  showToast(err.message, "err");
                                }
                              }}
                            />
                            {isCircuitOpen && (
                              <span class="volo-status-pill danger">
                                <span class="volo-status-dot" />
                                {t("app.circuit_open", { count: p.failure_count })}
                              </span>
                            )}
                          </div>
                        </td>
                        <td class="col-act" data-label={t("art.actions")}>
                          <div class="volo-row-actions">
                            {pSchema.length > 0 && (
                              <button
                                type="button"
                                class="volo-act-btn"
                                onClick={() => {
                                  const defaults = buildSchemaDefaults(pSchema);
                                  setCfgModal(p);
                                  setCfgValues({ ...defaults, ...(p.public_config || {}) });
                                  setCfgModalOpen(true);
                                }}
                              >
                                {t("app.configure")}
                              </button>
                            )}
                            <button
                              type="button"
                              class="volo-act-btn"
                              onClick={async () => {
                                setLogLevelFilter("");
                                setLogTextFilter("");
                                setLogPage(1);
                                setLogsModal({ id: p.id, logs: [] });
                                setLogsModalOpen(true);
                                await fetchPluginLogs(p.id, logLimit);
                              }}
                            >
                              {t("app.logs")}
                            </button>
                            <ActionMenu
                              items={[
                                {
                                  label: t("app.ext_update_now"),
                                  hidden: !(updCap !== "none" && updSt?.has_update),
                                  onClick: () =>
                                    setUpdModalTarget({
                                      kind: "plugin",
                                      id: String(p.id),
                                      name: pDisplayName,
                                      status: updSt,
                                    }),
                                },
                                {
                                  label:
                                    checkingSingleKey === `plugin:${p.id}`
                                      ? t("app.ext_checking")
                                      : t("app.ext_check_one"),
                                  hidden: updCap === "none",
                                  disabled: checkingSingleKey === `plugin:${p.id}`,
                                  onClick: () => void runSingleUpdateCheck("plugin", String(p.id), pDisplayName),
                                },
                                {
                                  label: t("app.plugin_set_priority"),
                                  onClick: () => openPriorityEditor(p),
                                },
                                {
                                  label: t("app.move_top"),
                                  hidden: isFiltered || chainIdx <= 0 || plugins.length < 2,
                                  onClick: () => {
                                    if (!reorderingPlugin && chainIdx > 0) {
                                      void movePluginOrder(p.id, -chainIdx);
                                    }
                                  },
                                },
                                {
                                  label: t("app.plugin_export_zip"),
                                  hidden: isBuiltinPlugin,
                                  onClick: () =>
                                    void downloadZipBlob(
                                      `/api/admin/plugins/${encodeURIComponent(p.id)}/export`,
                                      `${p.id}.zip`,
                                    ),
                                },
                                {
                                  label: t("app.plugin_trigger_cron"),
                                  hidden: !(Boolean(p.is_enabled) && rowCronJobs.length > 0),
                                  onClick: async () => {
                                    try {
                                      const res = await apiFetch<{ triggered?: number; cron_jobs?: any[] }>(
                                        `/api/admin/plugins/${encodeURIComponent(p.id)}/trigger-cron`,
                                        { method: "POST", body: "{}" },
                                      );
                                      showToast(
                                        t("app.plugin_cron_triggered", { count: Number(res?.triggered || rowCronJobs.length || 1) }),
                                        "ok",
                                      );
                                      void loadData(true);
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
                                  },
                                },
                                {
                                  label: t("app.reset_circuit"),
                                  hidden: !isCircuitOpen,
                                  onClick: async () => {
                                    try {
                                      await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}/reset-circuit`, {
                                        method: "POST",
                                        body: "{}",
                                      });
                                      showToast(t("app.circuit_reset_toast"), "ok");
                                      void loadData(true);
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
                                  },
                                },
                                {
                                  label: t("app.plugin_reload"),
                                  onClick: async () => {
                                    try {
                                      await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}/reload`, {
                                        method: "POST",
                                        body: "{}",
                                      });
                                      showToast(t("app.plugin_reloaded"), "ok");
                                      void loadData(true);
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
                                  },
                                },
                                {
                                  label: t("app.plugin_clear_kv"),
                                  danger: true,
                                  onClick: async () => {
                                    if (
                                      !(await openConfirm({
                                        message: t("app.plugin_confirm_clear_kv", { id: p.id }),
                                        danger: true,
                                      }))
                                    ) {
                                      return;
                                    }
                                    try {
                                      await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}/kv`, {
                                        method: "DELETE",
                                      });
                                      showToast(t("app.plugin_kv_cleared"), "ok");
                                      void loadData(true);
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
                                  },
                                },
                                {
                                  label: t("app.uninstall"),
                                  hidden: !(p.kind === "external" && !p.is_builtin && !p.builtin),
                                  danger: true,
                                  onClick: async () => {
                                    if (
                                      !(await openConfirm({
                                        message: t("app.confirm_uninstall", { id: p.id }),
                                        danger: true,
                                      }))
                                    ) {
                                      return;
                                    }
                                    try {
                                      await apiFetch(`/api/admin/plugins/${encodeURIComponent(p.id)}`, {
                                        method: "DELETE",
                                      });
                                      showToast(t("common.deleted"), "ok");
                                      void loadData(true);
                                    } catch (err: any) {
                                      showToast(err.message, "err");
                                    }
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
            total={filteredPlugins.length}
            page={pagedPlugins.safePage}
            pageSize={12}
            onChange={(p) => {
              setPluginPage(p);
              resetConsoleScroll();
            }}
          />
        </div>
      )}

      <AnimatedModal
        open={themeModalOpen}
        onClose={() => setThemeModalOpen(false)}
        title={t("app.theme_cfg_title", {
          name: themeModal?.display_name || themeModal?.name || themeModal?.id || "",
        })}
        maxWidth="720px"
        footer={
          <>
            <button
              type="button"
              class="volo-btn"
              style={{ marginRight: "auto" }}
              onClick={async () => {
                if (!(await openConfirm({ message: t("app.confirm_reset_cfg"), danger: false }))) return;
                setThemeCfgVals(buildSchemaDefaults(themeSchema, true));
              }}
            >
              {t("app.reset_defaults")}
            </button>
            <button type="button" class="volo-btn" onClick={() => setThemeModalOpen(false)}>
              {t("common.cancel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                if (!themeModal) return;
                const reqErr = validateSchemaRequired(themeSchema, themeCfgVals, themeModal?.secret_keys_set);
                if (reqErr) {
                  showToast(reqErr, "err");
                  return;
                }
                try {
                  const res = await apiFetch<{ appearance?: any; version?: number }>(
                    `/api/admin/themes/${encodeURIComponent(themeModal.id)}/config`,
                    {
                      method: "PUT",
                      body: JSON.stringify(themeCfgVals),
                    },
                  );
                  if (res?.appearance) setAppCfg(res.appearance);
                  if (typeof res?.version === "number") setAppVer(res.version);
                  setThemeModalOpen(false);
                  showToast(t("common.saved"), "ok");
                  void loadData(true);
                } catch (err: any) {
                  showToast(err.message, "err");
                }
              }}
            >
              {t("common.save")}
            </button>
          </>
        }
      >
        <SchemaConfigPanel
          targetId={themeModal?.id || "theme"}
          schema={themeSchema}
          values={themeCfgVals}
          onChange={(key, next) => setThemeCfgVals((prev) => ({ ...prev, [key]: next }))}
          onMergeValues={(patch) => setThemeCfgVals((prev) => ({ ...prev, ...patch }))}
          secretKeysSet={themeModal?.secret_keys_set}
          namePrefix="theme_opt"
        />
      </AnimatedModal>

      <AnimatedModal
        open={cfgModalOpen}
        onClose={() => setCfgModalOpen(false)}
        title={t("app.cfg_title", { name: resolveSchemaText(cfgModal?.name) || cfgModal?.id || "" })}
        maxWidth="720px"
        footer={
          <>
            <button
              type="button"
              class="volo-btn"
              style={{ marginRight: "auto" }}
              onClick={async () => {
                if (!(await openConfirm({ message: t("app.confirm_reset_cfg"), danger: false }))) return;
                setCfgValues(buildSchemaDefaults(pluginSchema, true));
              }}
            >
              {t("app.reset_defaults")}
            </button>
            <button type="button" class="volo-btn" onClick={() => setCfgModalOpen(false)}>
              {t("common.cancel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                if (!cfgModal) return;
                const reqErr = validateSchemaRequired(pluginSchema, cfgValues, cfgModal?.secret_keys_set);
                if (reqErr) {
                  showToast(reqErr, "err");
                  return;
                }
                try {
                  await apiFetch(`/api/admin/plugins/${encodeURIComponent(cfgModal.id)}/config`, {
                    method: "PATCH",
                    body: JSON.stringify(cfgValues),
                  });
                  setCfgModalOpen(false);
                  showToast(t("common.saved"), "ok");
                  void loadData(true);
                } catch (err: any) {
                  showToast(err.message, "err");
                }
              }}
            >
              {t("common.save")}
            </button>
          </>
        }
      >
        <SchemaConfigPanel
          targetId={cfgModal?.id || "plugin"}
          schema={pluginSchema}
          values={cfgValues}
          onChange={(key, next) => setCfgValues((prev) => ({ ...prev, [key]: next }))}
          onMergeValues={(patch) => setCfgValues((prev) => ({ ...prev, ...patch }))}
          secretKeysSet={cfgModal?.secret_keys_set}
          namePrefix="plugin_cfg"
        />
      </AnimatedModal>

      <AnimatedModal
        open={prioModalOpen}
        onClose={() => setPrioModalOpen(false)}
        title={t("app.plugin_prio_title", { name: prioModal?.name || "" })}
        maxWidth="440px"
        footer={
          <>
            <button type="button" class="volo-btn" onClick={() => setPrioModalOpen(false)}>
              {t("common.cancel")}
            </button>
            <button
              type="button"
              class="volo-btn volo-btn-primary"
              onClick={async () => {
                if (!prioModal) return;
                const rawNum = Math.round(Number(prioValue));
                if (!Number.isFinite(rawNum)) {
                  showToast(`${t("app.plugin_prio_label")} (-1000 .. 1000)`, "err");
                  return;
                }
                const parsed = Math.max(-1000, Math.min(1000, rawNum));
                if (parsed !== rawNum) setPrioValue(String(parsed));
                try {
                  const res = await apiFetch<{ items?: any[] }>(
                    `/api/admin/plugins/${encodeURIComponent(prioModal.id)}/priority?locale=${encodeURIComponent(localeSig.value)}`,
                    {
                      method: "PATCH",
                      body: JSON.stringify({ priority: parsed }),
                    },
                  );
                  if (Array.isArray(res?.items)) {
                    pluginFlip.captureFlip();
                    setPlugins(res.items);
                  } else {
                    void loadData(true);
                  }
                  setPrioModalOpen(false);
                  showToast(t("app.plugin_prio_saved"), "ok");
                } catch (err: any) {
                  showToast(err.message, "err");
                }
              }}
            >
              {t("common.save")}
            </button>
          </>
        }
      >
        <div class="volo-field" style={{ marginBottom: 0 }}>
          <label>{t("app.plugin_prio_label")}</label>
          <input
            type="number"
            class="volo-input"
            min={-1000}
            max={1000}
            step={1}
            value={prioValue}
            onInput={(e) => setPrioValue((e.currentTarget as HTMLInputElement).value)}
            onBlur={() => {
              const n = Math.round(Number(prioValue));
              if (Number.isFinite(n)) {
                setPrioValue(String(Math.max(-1000, Math.min(1000, n))));
              }
            }}
          />
          <div class="volo-help" style={{ marginTop: "8px" }}>
            {t("app.plugin_chain_hint")}
          </div>
        </div>
      </AnimatedModal>

      <AnimatedModal
        open={logsModalOpen}
        onClose={() => setLogsModalOpen(false)}
        title={t("app.logs_title", { id: logsModal?.id || "" })}
        maxWidth="680px"
        footer={
          <>
            {logsModal?.id && (
              <button
                type="button"
                class="volo-btn volo-btn-danger"
                disabled={!logsModal.logs?.length}
                onClick={async () => {
                  if (!logsModal?.id) return;
                  if (
                    !(await openConfirm({
                      message: t("app.logs_clear_confirm", { id: logsModal.id }),
                      danger: true,
                    }))
                  ) {
                    return;
                  }
                  try {
                    await apiFetch(`/api/admin/plugins/${encodeURIComponent(logsModal.id)}/logs`, {
                      method: "DELETE",
                    });
                    setLogsModal((prev) => (prev ? { ...prev, logs: [] } : prev));
                    setLogPage(1);
                    showToast(t("app.logs_cleared"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("app.logs_clear_btn")}
              </button>
            )}
            <button type="button" class="volo-btn" onClick={() => setLogsModalOpen(false)}>
              {t("common.close")}
            </button>
          </>
        }
      >
        <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", alignItems: "center", marginBottom: "10px" }}>
          <div style={{ width: "120px", maxWidth: "100%" }}>
            <ConsoleSelect
              searchable={false}
              value={logLevelFilter}
              onChange={(v) => {
                setLogLevelFilter(v);
                setLogPage(1);
              }}
              options={[
                { value: "", label: "ALL" },
                { value: "info", label: "INFO" },
                { value: "warn", label: "WARN" },
                { value: "error", label: "ERROR" },
              ]}
            />
          </div>
          <div style={{ width: "112px", maxWidth: "100%" }}>
            <ConsoleSelect
              searchable={false}
              value={String(logLimit)}
              onChange={(v) => {
                const nextLim = Number(v) || 100;
                setLogLimit(nextLim);
                setLogPage(1);
                if (logsModal?.id) {
                  void fetchPluginLogs(logsModal.id, nextLim);
                }
              }}
              options={[
                { value: "100", label: "100" },
                { value: "250", label: "250" },
                { value: "500", label: "500" },
              ]}
            />
          </div>
          <input
            type="search"
            class="volo-input"
            style={{ flex: "1 1 180px", minWidth: 0 }}
            placeholder={t("app.logs_filter_ph")}
            value={logTextFilter}
            onInput={(e) => {
              setLogTextFilter((e.currentTarget as HTMLInputElement).value);
              setLogPage(1);
            }}
          />
          {logsModal?.id && (
            <button
              type="button"
              class="volo-btn volo-btn-sm"
              disabled={logsRefreshing}
              onClick={() => void fetchPluginLogs(logsModal.id, logLimit)}
            >
              {t("common.refresh")}
            </button>
          )}
        </div>
        {(() => {
          const pagedPluginLogs = paginateItems(filteredLogs, logPage, 50);
          return (
            <>
              <pre
                style={{
                  maxHeight: "360px",
                  overflow: "auto",
                  whiteSpace: "pre-wrap",
                  overflowWrap: "anywhere",
                  fontSize: "12px",
                  background: "var(--volo-raised)",
                  padding: "12px",
                  borderRadius: "8px",
                  margin: 0,
                }}
              >
                {!logsModal || pagedPluginLogs.slice.length === 0
                  ? t("app.logs_empty")
                  : pagedPluginLogs.slice
                      .map((l: any) => {
                        const ts = l?.ts ? `${String(l.ts)} ` : "";
                        const lvl = String(l?.level || "info").toUpperCase();
                        const ev = l?.event ? `${String(l.event)}: ` : "";
                        const msg = String(l?.msg ?? l?.message ?? "");
                        return `${ts}[${lvl}] ${ev}${msg}`.trim();
                      })
                      .join("\n")}
              </pre>
              {filteredLogs.length > 50 && (
                <Pagination
                  total={filteredLogs.length}
                  page={pagedPluginLogs.safePage}
                  pageSize={50}
                  onChange={setLogPage}
                />
              )}
            </>
          );
        })()}
      </AnimatedModal>

      <RemoteInstallModal
        open={remoteInstallKind !== null}
        kind={remoteInstallKind || "plugin"}
        onClose={() => setRemoteInstallKind(null)}
        onInstalled={(res, installedKind) => {
          applyExtensionMutationResult(res, installedKind);
        }}
      />

      <ExtensionUpdateModal
        target={updModalTarget}
        onClose={() => setUpdModalTarget(null)}
        onUpgraded={(res, upgradedKind) => {
          applyExtensionMutationResult(res, upgradedKind);
        }}
      />

      <BatchUpdateModal
        target={batchModalTarget}
        onClose={() => setBatchModalTarget(null)}
        onItemUpgraded={(res, upgradedKind) => {
          applyExtensionMutationResult(res, upgradedKind);
        }}
      />

      <ZipPreflightModal
        info={zipPreflight}
        onClose={() => setZipPreflight(null)}
        onCompleted={async (res, completedKind, autoAfter, pkgId) => {
          applyExtensionMutationResult(res, completedKind);
          if (!pkgId) return;
          if (completedKind === "theme" && autoAfter && res?.appearance?.active_theme !== pkgId) {
            try {
              const actRes = await apiFetch<{ appearance?: any; version?: number }>("/api/admin/themes/activate", {
                method: "POST",
                body: JSON.stringify({ theme_id: pkgId, target: "desktop" }),
              });
              applyExtensionMutationResult(actRes, "theme");
            } catch {
              // Ignore post-install activation error; theme is already installed
            }
          } else if (completedKind === "plugin") {
            const list = Array.isArray(res?.items) ? res.items : null;
            const found = list ? list.find((it: any) => String(it?.id || "") === pkgId) : null;
            if (found && Boolean(found.is_enabled) !== autoAfter) {
              try {
                await apiFetch(
                  `/api/admin/plugins/${encodeURIComponent(pkgId)}/${autoAfter ? "enable" : "disable"}`,
                  { method: "POST", body: "{}" },
                );
                void loadData(true);
              } catch {
                // Ignore post-install toggle error
              }
            }
          }
        }}
      />
    </div>
  );
}
