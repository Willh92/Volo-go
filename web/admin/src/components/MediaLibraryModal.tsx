import { useEffect, useState } from "preact/hooks";
import { apiFetch, showToast, storageDriverSig, uploadWithProgress } from "../api";
import { t } from "../i18n";
import {
  AnimatedModal,
  ConsoleEmpty,
  ConsoleSelect,
  PaginationBar,
  UploadButton,
  UploadProgressBar,
  paginateItems,
  useDebouncedValue,
} from "./UIControls";

export interface MediaAssetRecord {
  id: string;
  original_name: string;
  orig_filename?: string;
  alt_text?: string;
  mime_type: string;
  size_bytes: number;
  width: number;
  height: number;
  public_url: string;
  storage_driver: string;
  created_at: number;
  ref_count?: number;
  is_orphan?: boolean;
}

export interface MediaLibraryModalProps {
  open: boolean;
  onClose: () => void;
  onSelect: (asset: MediaAssetRecord) => void;
  filterKind?: "all" | "image" | "audio";
  title?: string;
}

const MODAL_PAGE_SIZE = 36;

export function MediaLibraryModal({
  open,
  onClose,
  onSelect,
  filterKind = "all",
  title,
}: MediaLibraryModalProps) {
  const [items, setItems] = useState<MediaAssetRecord[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  const [reconciling, setReconciling] = useState(false);
  const [query, setQuery] = useState("");
  const debouncedQuery = useDebouncedValue(query, 250);
  const [kindTab, setKindTab] = useState<"all" | "image" | "audio">(filterKind);
  const [usageTab, setUsageTab] = useState<"all" | "used" | "unused">("all");
  const [monthFilter, setMonthFilter] = useState("");
  const [brokenIds, setBrokenIds] = useState<Record<string, boolean>>({});

  useEffect(() => {
    setKindTab(filterKind);
    setPage(1);
  }, [filterKind, open]);

  const loadAssets = async (
    targetPage = page,
    q = debouncedQuery,
    kind = kindTab,
    usage = usageTab,
    month = monthFilter
  ) => {
    setLoading(true);
    try {
      const params = new URLSearchParams({
        refs: usage === "all" ? "0" : "1",
        page: String(targetPage),
        per_page: String(MODAL_PAGE_SIZE),
      });
      if (q.trim()) params.set("q", q.trim());
      if (kind === "image") params.set("mime", "image");
      else if (kind === "audio") params.set("mime", "audio");
      if (usage !== "all") {
        params.set("usage", usage);
        if (usage === "unused") params.set("orphan_only", "1");
      }
      if (month.trim()) params.set("month", month.trim());

      const res = await apiFetch<{
        items: MediaAssetRecord[];
        total?: number;
        storage_driver?: string;
      }>(`/api/admin/media?${params.toString()}`);

      const normalized = (res.items || []).map((it) => ({
        ...it,
        original_name: it.original_name || it.orig_filename || it.id,
      }));
      setItems(normalized);
      setTotal(typeof res.total === "number" ? res.total : normalized.length);
      if (res.storage_driver) {
        storageDriverSig.value = res.storage_driver;
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) {
      void loadAssets(page, debouncedQuery, kindTab, usageTab, monthFilter);
    }
  }, [open, page, debouncedQuery, kindTab, usageTab, monthFilter]);

  const handleReconcile = async () => {
    setReconciling(true);
    try {
      await apiFetch("/api/admin/media/reconcile", { method: "POST", body: "{}" });
      setPage(1);
      await loadAssets(1, debouncedQuery, kindTab, usageTab, monthFilter);
      showToast(t("cnt.media_reconciled"), "ok");
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setReconciling(false);
    }
  };

  const handleUploadFiles = async (files: File[]) => {
    if (files.length === 0) return;
    setUploading(true);
    setUploadPct(0);
    const totalBytes = Math.max(1, files.reduce((acc, f) => acc + Math.max(1, f.size), 0));
    const perFileLoaded = new Array<number>(files.length).fill(0);
    const updateAggPct = () => {
      const loaded = perFileLoaded.reduce((a, b) => a + b, 0);
      setUploadPct(Math.min(100, Math.round((loaded / totalBytes) * 100)));
    };
    try {
      let lastPicked: MediaAssetRecord | null = null;
      let uploadedCount = 0;
      let cursor = 0;
      const workerCount = Math.min(3, files.length);
      await Promise.all(
        Array.from({ length: workerCount }, async () => {
          while (cursor < files.length) {
            const idx = cursor++;
            const file = files[idx];
            const fSize = Math.max(1, file.size);
            const fd = new FormData();
            fd.append("file", file);
            const res = await uploadWithProgress<{ asset?: MediaAssetRecord; item?: MediaAssetRecord }>(
              "/api/admin/media/upload",
              fd,
              (pct) => {
                perFileLoaded[idx] = (pct / 100) * fSize;
                updateAggPct();
              }
            );
            perFileLoaded[idx] = fSize;
            updateAggPct();
            const picked = res.asset || res.item;
            if (picked) {
              lastPicked = {
                ...picked,
                original_name: picked.original_name || picked.orig_filename || file.name,
              };
              uploadedCount++;
            }
          }
        })
      );
      if (files.length === 1 && lastPicked) {
        const chosen = lastPicked as MediaAssetRecord;
        showToast(t("ed.uploaded", { name: chosen.original_name }), "ok");
        onSelect(chosen);
        onClose();
      } else if (uploadedCount > 0) {
        showToast(t("cnt.media_uploaded_n", { count: uploadedCount }), "ok");
        setPage(1);
        await loadAssets(1, debouncedQuery, kindTab, usageTab, monthFilter);
      }
    } catch (err: any) {
      showToast(err.message, "err");
    } finally {
      setUploading(false);
      setUploadPct(null);
    }
  };

  const displayItems =
    items.length > MODAL_PAGE_SIZE ? paginateItems(items, page, MODAL_PAGE_SIZE).slice : items;

  const acceptAttr =
    filterKind === "image"
      ? "image/*"
      : filterKind === "audio"
      ? "audio/*"
      : "image/*,audio/*";

  return (
    <AnimatedModal
      open={open}
      onClose={onClose}
      title={title || t("cnt.media_picker_title")}
      subtitle={t("cnt.media_picker_desc")}
      maxWidth="860px"
      footer={
        <button type="button" class="volo-btn" onClick={onClose}>
          {t("common.close")}
        </button>
      }
    >
      <div class="volo-filter-bar" style={{ marginBottom: "14px" }}>
        {filterKind === "all" ? (
          <div class="volo-filter-pills">
            {(["all", "image", "audio"] as const).map((k) => (
              <button
                key={k}
                type="button"
                class={`volo-filter-pill ${kindTab === k ? "active" : ""}`}
                onClick={() => {
                  setKindTab(k);
                  setPage(1);
                }}
              >
                {k === "all"
                  ? t("cnt.media_type_all")
                  : k === "image"
                  ? t("cnt.media_type_image")
                  : t("cnt.media_type_audio")}
              </button>
            ))}
          </div>
        ) : (
          <span class="volo-badge">
            {filterKind === "image" ? t("cnt.media_type_image") : t("cnt.media_type_audio")}
          </span>
        )}

        <div class="volo-filter-controls">
          <ConsoleSelect
            searchable={false}
            style={{ width: "128px" }}
            value={usageTab}
            onChange={(v) => {
              setUsageTab((v as "all" | "used" | "unused") || "all");
              setPage(1);
            }}
            options={[
              { value: "all", label: t("cnt.media_filter_all") },
              { value: "used", label: t("cnt.media_filter_used") },
              { value: "unused", label: t("cnt.media_filter_orphan") },
            ]}
          />
          <input
            type="month"
            class="volo-input"
            style={{ width: "142px" }}
            aria-label={t("cnt.media_month_lbl")}
            value={monthFilter}
            onInput={(e) => {
              setMonthFilter((e.currentTarget as HTMLInputElement).value);
              setPage(1);
            }}
          />
          <input
            class="volo-input volo-search-input"
            placeholder={t("cnt.media_search_ph")}
            value={query}
            onInput={(e) => {
              setQuery((e.target as HTMLInputElement).value);
              setPage(1);
            }}
          />
          {storageDriverSig.value === "local" && (
            <button
              type="button"
              class="volo-btn volo-btn-sm"
              disabled={reconciling}
              onClick={() => void handleReconcile()}
            >
              {reconciling ? t("shell.loading") : t("cnt.media_scan_btn")}
            </button>
          )}
          <UploadButton
            accept={acceptAttr}
            multiple
            busy={uploading}
            primary
            label={t("cnt.media_upload_pick")}
            onFiles={(files) => void handleUploadFiles(files)}
          />
        </div>
      </div>

      {uploading && uploadPct !== null && (
        <div style={{ marginBottom: "12px" }}>
          <UploadProgressBar progress={uploadPct} label={t("cnt.media_uploading_pct", { pct: uploadPct })} />
        </div>
      )}

      {loading ? (
        <div class="volo-empty-state">{t("shell.loading")}</div>
      ) : displayItems.length === 0 ? (
        <ConsoleEmpty text={t("tbl.empty")} />
      ) : (
        <>
          <div class="volo-media-picker-grid">
            {displayItems.map((m) => {
              const isImg = (m.mime_type || "").startsWith("image/");
              const isAudio = (m.mime_type || "").startsWith("audio/");
              const isBroken = Boolean(brokenIds[m.id]);
              return (
                <button
                  key={m.id}
                  type="button"
                  class="volo-media-picker-card"
                  onClick={() => {
                    onSelect(m);
                    onClose();
                  }}
                >
                  <div class="volo-media-picker-thumb">
                    {isImg && !isBroken ? (
                      <img
                        src={m.public_url}
                        alt={m.alt_text || m.original_name}
                        loading="lazy"
                        onError={() => setBrokenIds((prev) => ({ ...prev, [m.id]: true }))}
                      />
                    ) : isAudio ? (
                      <div class="volo-media-type-placeholder">
                        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
                          <path d="M9 18V5l12-2v13" />
                          <circle cx="6" cy="18" r="3" />
                          <circle cx="18" cy="16" r="3" />
                        </svg>
                        <span>AUDIO</span>
                      </div>
                    ) : (
                      <div class="volo-media-type-placeholder">
                        <span>{isBroken ? "IMG" : (m.mime_type || "FILE").split("/").pop()?.toUpperCase()}</span>
                      </div>
                    )}
                    {typeof m.ref_count === "number" && (
                      <span class={`volo-media-ref-pill ${m.is_orphan ? "orphan" : "used"}`}>
                        {m.is_orphan ? t("cnt.media_orphan_short") : t("cnt.media_used_short")}
                      </span>
                    )}
                  </div>
                  <div class="volo-media-picker-meta">
                    <div class="volo-media-picker-name" title={m.original_name}>
                      {m.original_name}
                    </div>
                    <div class="volo-media-picker-sub">
                      {m.width > 0 ? `${m.width}×${m.height} · ` : ""}
                      {Math.max(1, Math.round((m.size_bytes || 0) / 1024))} KB
                    </div>
                  </div>
                </button>
              );
            })}
          </div>
          <PaginationBar
            total={total}
            page={page}
            pageSize={MODAL_PAGE_SIZE}
            onChange={(nextPage) => setPage(nextPage)}
          />
        </>
      )}
    </AnimatedModal>
  );
}

export interface MediaPickButtonProps {
  onPick: (url: string, asset: MediaAssetRecord) => void;
  filterKind?: "all" | "image" | "audio";
  label?: string;
  className?: string;
}

export function MediaPickButton({
  onPick,
  filterKind = "image",
  label,
  className = "",
}: MediaPickButtonProps) {
  const [open, setOpen] = useState(false);

  return (
    <>
      <button
        type="button"
        class={`volo-btn volo-btn-sm ${className}`.trim()}
        onClick={() => setOpen(true)}
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2.5" />
          <circle cx="8.5" cy="8.5" r="1.5" />
          <path d="m21 15-5-5L5 21" />
        </svg>
        <span>{label || t("cnt.media_pick_btn")}</span>
      </button>
      <MediaLibraryModal
        open={open}
        onClose={() => setOpen(false)}
        filterKind={filterKind}
        onSelect={(asset) => onPick(asset.public_url, asset)}
      />
    </>
  );
}
