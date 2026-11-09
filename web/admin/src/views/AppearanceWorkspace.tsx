import { useEffect, useRef, useState } from "preact/hooks";
import {
  accentSig,
  apiFetch,
  applyThemeAttributes,
  dirtyEditorSig,
  openConfirm,
  resetConsoleScroll,
  showToast,
  themeModeSig,
} from "../api";
import {
  ensureLocaleLoaded,
  exportFullTemplateJSON,
  exportLocalePackJSON,
  localeSig,
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
  paginateItems,
  Pagination,
  PlusIcon,
  SwitchToggle,
  TableSkeleton,
  UploadButton,
  usePointerSortableList,
} from "../components/UIControls";
import { MediaPickButton } from "../components/MediaLibraryModal";
import { CM6CssEditor } from "../components/CM6Editor";

export type AppearanceTab = "themes" | "locales" | "plugins";

const CLEAR_SECRET_SENTINEL = "__VOLO_CLEAR_SECRET__";

const ACCENTS: Array<{ id: string; swatch: string }> = [
  { id: "forest", swatch: "#246b51" },
  { id: "ocean", swatch: "#2d6583" },
  { id: "teal", swatch: "#286c70" },
  { id: "amber", swatch: "#855c27" },
  { id: "plum", swatch: "#70517d" },
  { id: "rose", swatch: "#87525f" },
];

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

function getSchemaFieldGroup(f: any): string {
  return (
    resolveSchemaText(f?.localized_group) ||
    resolveSchemaText(f?.group_label) ||
    resolveSchemaText(f?.group)
  );
}

function buildSchemaDefaults(schema: any[]): Record<string, any> {
  const out: Record<string, any> = {};
  for (const f of schema || []) {
    if (!f?.key) continue;
    if (isSecretSchemaField(f)) {
      out[f.key] = "";
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
    if (!f?.key || !f.required) continue;
    const label = resolveSchemaText(f.label) || String(f.key);
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
    } else if (f.type === "number") {
      if (val === "" || val === null || val === undefined || Number.isNaN(Number(val))) {
        return t("app.cfg_required_missing", { field: label });
      }
      if (typeof f.min === "number" && Number(val) < f.min) {
        return `${label} >= ${f.min}`;
      }
      if (typeof f.max === "number" && Number(val) > f.max) {
        return `${label} <= ${f.max}`;
      }
    } else if (f.type !== "boolean") {
      if (String(val ?? "").trim() === "") {
        return t("app.cfg_required_missing", { field: label });
      }
    }
  }
  return null;
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

  const moveItem = (idx: number, delta: number) => {
    const target = idx + delta;
    if (target < 0 || target >= enabledKeys.length) return;
    captureFlip();
    const next = [...enabledKeys];
    const [picked] = next.splice(idx, 1);
    next.splice(target, 0, picked);
    onChange(fieldKey, next);
  };

  const moveToTop = (idx: number) => {
    if (idx <= 0 || idx >= enabledKeys.length) return;
    captureFlip();
    const next = [...enabledKeys];
    const [picked] = next.splice(idx, 1);
    next.unshift(picked);
    onChange(fieldKey, next);
  };

  return (
    <div class="volo-field">
      <label>
        {label}
        {reqMark}
      </label>
      {description && <div class="volo-help">{description}</div>}
      <div ref={listRef} class="volo-sortable-list" style={{ maxHeight: "280px", overflowY: "auto" }}>
        {enabledKeys.map((k, idx) => {
          const handleProps = getHandleProps(idx);
          return (
            <div
              key={k}
              data-sort-idx={idx}
              class={`volo-sortable-row ${draggingIdx === idx ? "is-dragging" : ""} ${grabbingIdx === idx ? "is-grabbing" : ""}`}
            >
              <div class="volo-sortable-main">
                <span
                  class={`volo-drag-handle ${enabledKeys.length < 2 ? "is-disabled" : ""}`}
                  title={t("app.drag_sort")}
                  aria-label={t("app.drag_sort")}
                  {...(handleProps as any)}
                >
                  <DragGripIcon />
                </span>
                <SwitchToggle
                  checked={true}
                  onChange={(checked) => {
                    if (!checked) {
                      onChange(
                        fieldKey,
                        enabledKeys.filter((it) => it !== k)
                      );
                    }
                  }}
                />
                <span
                  class="volo-sortable-label"
                  onPointerDown={(e) => {
                    if (e.pointerType === "mouse") {
                      handleProps.onPointerDown(e as unknown as PointerEvent);
                    }
                  }}
                >
                  {idx + 1}. {optMap.get(k) || k}
                </span>
              </div>
              <div class="volo-sortable-acts">
                {showNavPlacement && (
                  <span class={`volo-badge ${idx < primaryLimit ? "volo-badge-ok" : ""}`}>
                    {idx < primaryLimit ? t("app.nav_slot_primary") : t("app.nav_slot_more")}
                  </span>
                )}
                <button
                  type="button"
                  class="volo-act-btn"
                  disabled={idx === 0}
                  title={t("app.move_top")}
                  aria-label={t("app.move_top")}
                  onClick={() => moveToTop(idx)}
                >
                  <MoveTopIcon />
                </button>
                <button
                  type="button"
                  class="volo-act-btn"
                  disabled={idx === 0}
                  title={t("app.move_up")}
                  aria-label={t("app.move_up")}
                  onClick={() => moveItem(idx, -1)}
                >
                  ↑
                </button>
                <button
                  type="button"
                  class="volo-act-btn"
                  disabled={idx === enabledKeys.length - 1}
                  title={t("app.move_down")}
                  aria-label={t("app.move_down")}
                  onClick={() => moveItem(idx, 1)}
                >
                  ↓
                </button>
              </div>
            </div>
          );
        })}
        {disabledOpts.map((opt) => (
          <div key={opt.value} class="volo-sortable-row is-off">
            <div class="volo-sortable-main">
              <span class="volo-drag-handle is-disabled" aria-hidden="true">
                <DragGripIcon />
              </span>
              <SwitchToggle
                checked={false}
                onChange={(checked) => {
                  if (checked) {
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

  const moveFree = (idx: number, delta: number) => {
    const target = idx + delta;
    if (target < 0 || target >= items.length) return;
    captureFlip();
    const next = [...items];
    const [picked] = next.splice(idx, 1);
    next.splice(target, 0, picked);
    onChange(fieldKey, next);
  };

  const moveFreeTop = (idx: number) => {
    if (idx <= 0 || idx >= items.length) return;
    captureFlip();
    const next = [...items];
    const [picked] = next.splice(idx, 1);
    next.unshift(picked);
    onChange(fieldKey, next);
  };

  const addFree = () => {
    const trimmed = freeItemDraft.trim();
    if (!trimmed || items.includes(trimmed)) return;
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
          {items.map((item, idx) => {
            const handleProps = getHandleProps(idx);
            return (
              <div
                key={item}
                data-sort-idx={idx}
                class={`volo-sortable-row ${draggingIdx === idx ? "is-dragging" : ""} ${grabbingIdx === idx ? "is-grabbing" : ""}`}
              >
                <div class="volo-sortable-main">
                  <span
                    class={`volo-drag-handle ${items.length < 2 ? "is-disabled" : ""}`}
                    title={t("app.drag_sort")}
                    aria-label={t("app.drag_sort")}
                    {...(handleProps as any)}
                  >
                    <DragGripIcon />
                  </span>
                  <span
                    class="volo-sortable-label"
                    onPointerDown={(e) => {
                      if (e.pointerType === "mouse") {
                        handleProps.onPointerDown(e as unknown as PointerEvent);
                      }
                    }}
                  >
                    {idx + 1}. {item}
                  </span>
                </div>
                <div class="volo-sortable-acts">
                  <button
                    type="button"
                    class="volo-act-btn"
                    disabled={idx === 0}
                    title={t("app.move_top")}
                    aria-label={t("app.move_top")}
                    onClick={() => moveFreeTop(idx)}
                  >
                    <MoveTopIcon />
                  </button>
                  <button
                    type="button"
                    class="volo-act-btn"
                    disabled={idx === 0}
                    title={t("app.move_up")}
                    aria-label={t("app.move_up")}
                    onClick={() => moveFree(idx, -1)}
                  >
                    ↑
                  </button>
                  <button
                    type="button"
                    class="volo-act-btn"
                    disabled={idx === items.length - 1}
                    title={t("app.move_down")}
                    aria-label={t("app.move_down")}
                    onClick={() => moveFree(idx, 1)}
                  >
                    ↓
                  </button>
                  <button
                    type="button"
                    class="volo-act-btn danger"
                    onClick={() => onChange(fieldKey, items.filter((_, i) => i !== idx))}
                  >
                    <CloseIcon size={13} />
                  </button>
                </div>
              </div>
            );
          })}
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
                  if (allowed.has(k) && v !== CLEAR_SECRET_SENTINEL) {
                    patch[k] = v;
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
    const next = [...plugins];
    const [moved] = next.splice(idx, 1);
    next.splice(targetIdx, 0, moved);
    setReorderingPlugin(true);
    try {
      const res = await apiFetch<{ items?: any[]; cron_jobs?: any[] }>(
        `/api/admin/plugins/reorder?locale=${encodeURIComponent(localeSig.value)}`,
        {
          method: "POST",
          body: JSON.stringify({ ordered_ids: next.map((item) => String(item.id)) }),
        },
      );
      if (Array.isArray(res?.items)) setPlugins(res.items);
      else setPlugins(next);
      if (Array.isArray(res?.cron_jobs)) setPluginCronJobs(res.cron_jobs);
      showToast(t("app.plugin_reordered"), "ok");
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setReorderingPlugin(false);
    }
  };

  const isAlreadyExistsConflict = (err: any): boolean => {
    if (err?.status === 409) return true;
    const raw = String(err?.payload?.error || err?.message || "");
    return /already exists|conflict/i.test(raw);
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
          <div class="volo-card">
            <div class="volo-sec-head">
              <div class="volo-sec-copy">
                <h3 class="volo-card-title">{t("app.gallery_head")}</h3>
                <p class="volo-sec-desc">{t("app.gallery_sub")}</p>
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
                      const res = await apiFetch<{ themes?: any[]; appearance?: any }>("/api/admin/themes/reload", {
                        method: "POST",
                        body: "{}",
                      });
                      setBrokenPreviews({});
                      if (Array.isArray(res.themes)) setThemes(res.themes);
                      else void loadData(true);
                      showToast(t("app.themes_reloaded"), "ok");
                    } catch (err: any) {
                      showToast(err.message, "err");
                    }
                  }}
                >
                  {t("app.themes_reload")}
                </button>
                <UploadButton
                  accept=".zip"
                  primary
                  small
                  busy={uploading}
                  label={t("app.install_theme_zip")}
                  onFile={async (f) => {
                    const uploadThemeZip = async (overwrite: boolean) => {
                      const fd = new FormData();
                      fd.append("file", f);
                      const qs = `locale=${encodeURIComponent(localeSig.value)}${overwrite ? "&overwrite=true" : ""}`;
                      await apiFetch(`/api/admin/themes/install?${qs}`, {
                        method: "POST",
                        body: fd,
                      });
                    };
                    setUploading(true);
                    try {
                      await uploadThemeZip(false);
                      setBrokenPreviews({});
                      showToast(t("app.theme_installed"), "ok");
                      void loadData(true);
                    } catch (err: any) {
                      if (isAlreadyExistsConflict(err)) {
                        const ok = await openConfirm({
                          message: t("app.confirm_overwrite_theme"),
                          danger: true,
                        });
                        if (ok) {
                          try {
                            await uploadThemeZip(true);
                            setBrokenPreviews({});
                            showToast(t("app.theme_installed"), "ok");
                            void loadData(true);
                          } catch (retryErr: any) {
                            showToast(retryErr.message, "err");
                          }
                        }
                      } else {
                        showToast(err.message, "err");
                      }
                    } finally {
                      setUploading(false);
                    }
                  }}
                />
              </div>
            </div>

            <div class="volo-theme-grid">
              {pagedThemes.slice.map((th) => {
                const isBuiltin = Boolean(th.is_builtin || th.builtin);
                const isActive = (appCfg.active_theme || "volo-studio") === th.id;
                const isMobileActive = Boolean(
                  appCfg.mobile_theme && appCfg.mobile_theme !== "same" && appCfg.mobile_theme === th.id,
                );
                const schema: any[] = th.localized_schema || th.config_schema || [];
                const previewSrc = th.preview_url || `/themes/${encodeURIComponent(th.id)}/preview`;
                const title = th.display_name || th.name || th.id;
                const desc = th.display_description || th.description || "";
                const hasBrokenImg = Boolean(brokenPreviews[th.id]);
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
                      </div>
                    </div>
                    <div class="volo-theme-body">
                      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "baseline", gap: "8px" }}>
                        <strong style={{ fontSize: "15px" }}>{title}</strong>
                        <span class="volo-help">v{th.version || "1.0.0"} · {th.author || "Volo"}</span>
                      </div>
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
                        <a
                          class="volo-btn volo-btn-sm"
                          href={`/api/admin/themes/${encodeURIComponent(th.id)}/export`}
                          download={`${th.id}.zip`}
                        >
                          {t("app.theme_export_zip")}
                        </a>
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
                    applyThemeAttributes(themeModeSig.value, nextAccent, false);
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
            <div style={{ display: "flex", gap: "8px", flexWrap: "wrap" }}>
              <UploadButton
                accept=".json,.zip"
                primary
                small
                busy={uploading}
                label={t("app.loc_install_btn")}
                onFile={async (f) => {
                  setUploading(true);
                  try {
                    const fd = new FormData();
                    fd.append("file", f);
                    const res = await apiFetch<{ items?: any[]; locales?: any[]; site_version?: number }>("/api/admin/locales/install", {
                      method: "POST",
                      body: fd,
                    });
                    if (res.locales) syncExternalLocales(res.locales);
                    if (res.items) setLocPacks(res.items);
                    if (typeof res.site_version === "number" && res.site_version > 0) setSiteVer(res.site_version);
                    await ensureLocaleLoaded(localeSig.value, true);
                    showToast(t("app.loc_installed"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  } finally {
                    setUploading(false);
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
                    if (res.locales) syncExternalLocales(res.locales);
                    if (res.items) setLocPacks(res.items);
                    if (typeof res.site_version === "number" && res.site_version > 0) setSiteVer(res.site_version);
                    await ensureLocaleLoaded(localeSig.value, true);
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

          <div class="volo-table-wrap">
            <table class="volo-table">
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
                  paginateItems(locPacks, locPage, 12).slice.map((p) => {
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
                                      const res = await apiFetch<{ items?: any[]; locales?: any[] }>(
                                        `/api/admin/locales/${encodeURIComponent(p.code)}`,
                                        { method: "DELETE" },
                                      );
                                      if (res.locales) syncExternalLocales(res.locales);
                                      if (res.items) setLocPacks(res.items);
                                      showToast(t("app.loc_deleted"), "ok");
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
        <div class="volo-card">
          <div class="volo-sec-head">
            <div class="volo-sec-copy">
              <h3 class="volo-card-title">{t("app.plugins_head")}</h3>
              <p class="volo-sec-desc">{t("app.plugins_desc")}</p>
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
                    if (Array.isArray(res?.items)) setPlugins(res!.items);
                    else void loadData(true);
                    if (Array.isArray(res?.cron_jobs)) setPluginCronJobs(res!.cron_jobs);
                    showToast(t("app.plugins_discovered"), "ok");
                  } catch (err: any) {
                    showToast(err.message, "err");
                  }
                }}
              >
                {t("app.plugins_discover")}
              </button>
              <UploadButton
                accept=".zip"
                primary
                small
                busy={uploading}
                label={t("app.install_zip")}
                onFile={async (f) => {
                  const uploadPluginZip = async (overwrite: boolean) => {
                    const fd = new FormData();
                    fd.append("file", f);
                    const qs = `locale=${encodeURIComponent(localeSig.value)}${overwrite ? "&overwrite=true" : ""}`;
                    await apiFetch(`/api/admin/plugins/install?${qs}`, {
                      method: "POST",
                      body: fd,
                    });
                  };
                  setUploading(true);
                  try {
                    await uploadPluginZip(false);
                    showToast(t("app.installed_toast"), "ok");
                    void loadData(true);
                  } catch (err: any) {
                    if (isAlreadyExistsConflict(err)) {
                      const ok = await openConfirm({
                        message: t("app.confirm_overwrite_plugin"),
                        danger: true,
                      });
                      if (ok) {
                        try {
                          await uploadPluginZip(true);
                          showToast(t("app.installed_toast"), "ok");
                          void loadData(true);
                        } catch (retryErr: any) {
                          showToast(retryErr.message, "err");
                        }
                      }
                    } else {
                      showToast(err.message, "err");
                    }
                  } finally {
                    setUploading(false);
                  }
                }}
              />
            </div>
          </div>

          <div class="volo-table-wrap">
            <table class="volo-table">
              <thead>
                <tr>
                  <th class="col-fluid">{t("app.plugin_col_info")}</th>
                  <th class="col-cat">{t("app.plugin_col_chain")}</th>
                  <th class="col-cat">{t("app.plugin_col_runtime")}</th>
                  <th class="col-status">{t("art.status")}</th>
                  <th class="col-act col-act-th">{t("art.actions")}</th>
                </tr>
              </thead>
              <tbody>
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
                    const rowCronJobs: any[] =
                      Array.isArray(p.cron_jobs) && p.cron_jobs.length > 0
                        ? p.cron_jobs
                        : pluginCronJobs.filter((j) => j?.plugin_id === p.id);
                    return (
                      <tr key={p.id}>
                        <td class="col-fluid" data-label={t("app.plugin_col_info")}>
                          <div class="volo-cell-main">
                            <div class="volo-cell-title">
                              <span>{resolveSchemaText(p.name) || p.id}</span>
                              <span class="volo-code-chip">{p.id}</span>
                            </div>
                            {resolveSchemaText(p.description) && (
                              <div class="volo-cell-meta"><span>{resolveSchemaText(p.description)}</span></div>
                            )}
                            {rowCronJobs.length > 0 && (
                              <div style={{ display: "flex", gap: "6px", flexWrap: "wrap", marginTop: "4px" }}>
                                {rowCronJobs.map((job) => (
                                  <span
                                    key={job.name}
                                    class="volo-code-chip"
                                    title={job.last_error ? `${job.name}: ${job.last_error}` : job.handler || job.name}
                                    style={
                                      job.last_error
                                        ? { borderColor: "var(--volo-danger)", color: "var(--volo-danger)" }
                                        : undefined
                                    }
                                  >
                                    {t("app.plugin_cron_badge", {
                                      name: String(job.name || "cron"),
                                      interval: formatCronInterval(job.interval_seconds),
                                      runs: Number(job.run_count || 0),
                                    })}
                                  </span>
                                ))}
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
                        <td class="col-cat" data-label={t("app.plugin_col_chain")}>
                          <div style={{ display: "inline-flex", alignItems: "center", gap: "4px", flexWrap: "wrap" }}>
                            <button
                              type="button"
                              class="volo-act-btn"
                              title={t("app.plugin_chain_hint")}
                              onClick={() => openPriorityEditor(p)}
                              style={{ fontVariantNumeric: "tabular-nums" }}
                            >
                              #{chainIdx >= 0 ? chainIdx + 1 : 1} · P{priorityNum}
                            </button>
                            {!pluginNeedle && plugins.length > 1 && (
                              <>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  title={t("app.move_up")}
                                  aria-label={t("app.move_up")}
                                  disabled={reorderingPlugin || chainIdx <= 0}
                                  onClick={() => void movePluginOrder(p.id, -1)}
                                  style={{ padding: "2px 6px", minWidth: "24px" }}
                                >
                                  ↑
                                </button>
                                <button
                                  type="button"
                                  class="volo-act-btn"
                                  title={t("app.move_down")}
                                  aria-label={t("app.move_down")}
                                  disabled={reorderingPlugin || chainIdx < 0 || chainIdx >= plugins.length - 1}
                                  onClick={() => void movePluginOrder(p.id, 1)}
                                  style={{ padding: "2px 6px", minWidth: "24px" }}
                                >
                                  ↓
                                </button>
                                {chainIdx > 1 && (
                                  <button
                                    type="button"
                                    class="volo-act-btn"
                                    title={t("app.move_top")}
                                    aria-label={t("app.move_top")}
                                    disabled={reorderingPlugin}
                                    onClick={() => void movePluginOrder(p.id, -chainIdx)}
                                    style={{ padding: "2px 6px", minWidth: "24px" }}
                                  >
                                    <MoveTopIcon />
                                  </button>
                                )}
                              </>
                            )}
                          </div>
                        </td>
                        <td class="col-cat" data-label={t("app.plugin_col_runtime")}>
                          <span class="volo-cat-chip">
                            {p.kind} · {p.runtime} · v{p.version}
                          </span>
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
                                try {
                                  const r = await apiFetch<{ items: any[] }>(
                                    `/api/admin/plugins/${encodeURIComponent(p.id)}/logs`,
                                  );
                                  setLogLevelFilter("");
                                  setLogTextFilter("");
                                  setLogsModal({ id: p.id, logs: r.items || [] });
                                  setLogsModalOpen(true);
                                } catch (err: any) {
                                  showToast(err.message, "err");
                                }
                              }}
                            >
                              {t("app.logs")}
                            </button>
                            {!isBuiltinPlugin && (
                              <a
                                class="volo-act-btn"
                                href={`/api/admin/plugins/${encodeURIComponent(p.id)}/export`}
                                download={`${p.id}.zip`}
                              >
                                {t("app.plugin_export_zip")}
                              </a>
                            )}
                            <ActionMenu
                              items={[
                                {
                                  label: t("app.plugin_set_priority"),
                                  onClick: () => openPriorityEditor(p),
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
                setThemeCfgVals(buildSchemaDefaults(themeSchema));
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
                setCfgValues(buildSchemaDefaults(pluginSchema));
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
                const parsed = Math.round(Number(prioValue));
                if (!Number.isFinite(parsed) || parsed < -1000 || parsed > 10000) {
                  showToast(`${t("app.plugin_prio_label")} (-1000 .. 10000)`, "err");
                  return;
                }
                try {
                  const res = await apiFetch<{ items?: any[] }>(
                    `/api/admin/plugins/${encodeURIComponent(prioModal.id)}/priority?locale=${encodeURIComponent(localeSig.value)}`,
                    {
                      method: "PATCH",
                      body: JSON.stringify({ priority: parsed }),
                    },
                  );
                  if (Array.isArray(res?.items)) setPlugins(res.items);
                  else void loadData(true);
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
            max={10000}
            step={1}
            value={prioValue}
            onInput={(e) => setPrioValue((e.currentTarget as HTMLInputElement).value)}
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
          <button type="button" class="volo-btn" onClick={() => setLogsModalOpen(false)}>
            {t("common.close")}
          </button>
        }
      >
        <div style={{ display: "flex", gap: "8px", flexWrap: "wrap", marginBottom: "10px" }}>
          <div style={{ width: "140px", maxWidth: "100%" }}>
            <ConsoleSelect
              value={logLevelFilter}
              onChange={(v) => setLogLevelFilter(v)}
              options={[
                { value: "", label: "ALL" },
                { value: "info", label: "INFO" },
                { value: "warn", label: "WARN" },
                { value: "error", label: "ERROR" },
              ]}
            />
          </div>
          <input
            type="search"
            class="volo-input"
            style={{ flex: "1 1 200px", minWidth: 0 }}
            placeholder={t("app.logs_filter_ph")}
            value={logTextFilter}
            onInput={(e) => setLogTextFilter((e.currentTarget as HTMLInputElement).value)}
          />
        </div>
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
          {!logsModal || filteredLogs.length === 0
            ? t("app.logs_empty")
            : filteredLogs
                .map((l: any) => {
                  const ts = l?.ts ? `${String(l.ts)} ` : "";
                  const lvl = String(l?.level || "info").toUpperCase();
                  const ev = l?.event ? `${String(l.event)}: ` : "";
                  const msg = String(l?.msg ?? l?.message ?? "");
                  return `${ts}[${lvl}] ${ev}${msg}`.trim();
                })
                .join("\n")}
        </pre>
      </AnimatedModal>
    </div>
  );
}
