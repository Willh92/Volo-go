import { useEffect, useState } from "preact/hooks";
import { apiFetch, showToast, storageDriverSig } from "../api";
import { t } from "../i18n";
import { AnimatedModal } from "./UIControls";

export interface MediaAssetRecord {
  id: string;
  original_name: string;
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

export function MediaLibraryModal({
  open,
  onClose,
  onSelect,
  filterKind = "all",
  title,
}: MediaLibraryModalProps) {
  const [items, setItems] = useState<MediaAssetRecord[]>([]);
  const [loading, setLoading] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [query, setQuery] = useState("");
  const [kindTab, setKindTab] = useState<"all" | "image" | "audio">(filterKind);

  useEffect(() => {
    setKindTab(filterKind);
  }, [filterKind, open]);

  const loadAssets = async () => {
    setLoading(true);
    try {
      const res = await apiFetch<{
        items: MediaAssetRecord[];
        storage_driver?: string;
      }>("/api/admin/media");
      setItems(res.items || []);
      if (res.storage_driver) {
        storageDriverSig.value = res.storage_driver;
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    if (open) {
      void loadAssets();
    }
  }, [open]);

  const handleUploadAndPick = async (file: File) => {
    setUploading(true);
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{ item: MediaAssetRecord }>("/api/admin/media/upload", {
        method: "POST",
        body: fd,
      });
      if (res.item) {
        showToast(t("ed.uploaded", { name: file.name }));
        onSelect(res.item);
        onClose();
      }
    } catch (err: any) {
      showToast(err.message);
    } finally {
      setUploading(false);
    }
  };

  const filtered = items.filter((m) => {
    const mime = (m.mime_type || "").toLowerCase();
    if (kindTab === "image" && !mime.startsWith("image/")) return false;
    if (kindTab === "audio" && !mime.startsWith("audio/")) return false;
    if (query.trim()) {
      const q = query.trim().toLowerCase();
      return (
        (m.original_name || "").toLowerCase().includes(q) ||
        (m.public_url || "").toLowerCase().includes(q)
      );
    }
    return true;
  });

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
      maxWidth="840px"
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
                onClick={() => setKindTab(k)}
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
          <input
            class="volo-input"
            style={{ width: "220px" }}
            placeholder={t("cnt.media_search_ph")}
            value={query}
            onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
          />
          <label class="volo-btn volo-btn-primary volo-btn-sm" style={{ cursor: "pointer" }}>
            {uploading ? t("ed.uploading") : t("cnt.media_upload_pick")}
            <input
              type="file"
              accept={acceptAttr}
              style={{ display: "none" }}
              disabled={uploading}
              onChange={(e) => {
                const f = (e.target as HTMLInputElement).files?.[0];
                if (f) void handleUploadAndPick(f);
                (e.target as HTMLInputElement).value = "";
              }}
            />
          </label>
        </div>
      </div>

      {loading ? (
        <div class="volo-empty-state">{t("shell.loading")}</div>
      ) : filtered.length === 0 ? (
        <div class="volo-empty-state">{t("tbl.empty")}</div>
      ) : (
        <div class="volo-media-picker-grid">
          {filtered.map((m) => {
            const isImg = (m.mime_type || "").startsWith("image/");
            const isAudio = (m.mime_type || "").startsWith("audio/");
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
                  {isImg ? (
                    <img src={m.public_url} alt={m.original_name} loading="lazy" />
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
                      <span>{(m.mime_type || "FILE").split("/").pop()?.toUpperCase()}</span>
                    </div>
                  )}
                  <span class={`volo-media-ref-pill ${m.is_orphan ? "orphan" : "used"}`}>
                    {m.is_orphan ? t("cnt.media_orphan_short") : t("cnt.media_used_short")}
                  </span>
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
  const driver = storageDriverSig.value;

  if (driver && driver !== "local") {
    return null;
  }

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
