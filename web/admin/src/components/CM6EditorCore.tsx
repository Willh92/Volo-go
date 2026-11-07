import { useEffect, useRef, useState } from "preact/hooks";
import { Compartment, EditorState } from "@codemirror/state";
import { EditorView, keymap, lineNumbers, placeholder } from "@codemirror/view";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";
import { markdown } from "@codemirror/lang-markdown";
import { apiFetch, showToast } from "../api";
import { localeSig, t } from "../i18n";
import type { CM6EditorProps } from "./CM6Editor";

export function CM6EditorCore({ value, onChange, quickInserts = [] }: CM6EditorProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const phCompartment = useRef(new Compartment());
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;

  const curLocale = localeSig.value;
  const [previewHTML, setPreviewHTML] = useState<string>("");
  const [showPreview, setShowPreview] = useState<boolean>(true);
  const [uploading, setUploading] = useState<boolean>(false);

  const insertSnippet = (snippet: string) => {
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert: snippet },
      selection: { anchor: sel.from + snippet.length },
    });
    view.focus();
  };

  const uploadMediaFile = async (file: File) => {
    setUploading(true);
    try {
      const fd = new FormData();
      fd.append("file", file);
      const res = await apiFetch<{ asset: { public_url: string; orig_filename: string; mime_type: string } }>(
        "/api/admin/media/upload",
        { method: "POST", body: fd }
      );
      const url = res.asset.public_url;
      const name = res.asset.orig_filename || "asset";
      if (res.asset.mime_type?.startsWith("audio/")) {
        insertSnippet(`\n[${t("ed.audio_label")}: ${name}](${url})\n`);
      } else {
        insertSnippet(`![${name}](${url})`);
      }
      showToast(t("ed.uploaded", { name }));
    } catch (err: any) {
      showToast(err?.message || t("ed.upload_failed"));
    } finally {
      setUploading(false);
    }
  };

  useEffect(() => {
    if (!hostRef.current) return;

    const domHandlers = EditorView.domEventHandlers({
      paste(event) {
        const items = event.clipboardData?.items;
        if (!items) return false;
        for (let i = 0; i < items.length; i++) {
          const item = items[i];
          if (item.kind === "file" && item.type.startsWith("image/")) {
            const f = item.getAsFile();
            if (f) {
              event.preventDefault();
              void uploadMediaFile(f);
              return true;
            }
          }
        }
        return false;
      },
      drop(event) {
        const files = event.dataTransfer?.files;
        if (files && files.length > 0) {
          event.preventDefault();
          void uploadMediaFile(files[0]);
          return true;
        }
        return false;
      },
    });

    const state = EditorState.create({
      doc: value,
      extensions: [
        lineNumbers(),
        history(),
        markdown(),
        EditorView.lineWrapping,
        phCompartment.current.of(placeholder(t("ed.placeholder"))),
        keymap.of([...defaultKeymap, ...historyKeymap]),
        domHandlers,
        EditorView.updateListener.of((update) => {
          if (update.docChanged) {
            onChangeRef.current(update.state.doc.toString());
          }
        }),
      ],
    });

    const view = new EditorView({
      state,
      parent: hostRef.current,
    });
    viewRef.current = view;

    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, []);

  // Reconfigure CM6 placeholder reactively when language switches
  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      effects: phCompartment.current.reconfigure(placeholder(t("ed.placeholder"))),
    });
  }, [curLocale]);

  // Synchronize external value updates (e.g. loading an article or restoring a revision/draft)
  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const cur = view.state.doc.toString();
    if (cur !== value) {
      view.dispatch({
        changes: { from: 0, to: cur.length, insert: value },
      });
    }
  }, [value]);

  // Debounced live server-rendered goldmark + chroma preview
  useEffect(() => {
    if (!showPreview) return;
    const timer = window.setTimeout(async () => {
      try {
        const res = await apiFetch<{ html: string }>("/api/admin/articles/preview", {
          method: "POST",
          body: JSON.stringify({ content_md: value, locale: curLocale }),
        });
        setPreviewHTML(res.html || "");
      } catch {
        // Ignore transient preview errors while typing
      }
    }, 260);
    return () => window.clearTimeout(timer);
  }, [value, showPreview, curLocale]);

  return (
    <div>
      <div class="volo-toolbar">
        <button type="button" class="volo-btn volo-btn-sm" onClick={() => insertSnippet("## ")}>{t("ed.h2")}</button>
        <button type="button" class="volo-btn volo-btn-sm" onClick={() => insertSnippet(t("ed.bold_sample"))}>{t("ed.bold")}</button>
        <button type="button" class="volo-btn volo-btn-sm" onClick={() => insertSnippet("`code`")}>{t("ed.code")}</button>
        <button type="button" class="volo-btn volo-btn-sm" onClick={() => insertSnippet("\n```go\n\n```\n")}>{t("ed.code_block")}</button>
        <button
          type="button"
          class="volo-btn volo-btn-sm"
          onClick={() => insertSnippet(t("ed.lock_tpl"))}
        >
          {t("ed.lock")}
        </button>
        <button
          type="button"
          class="volo-btn volo-btn-sm"
          onClick={() => insertSnippet(t("ed.book_tpl"))}
        >
          {t("ed.book")}
        </button>
        <button
          type="button"
          class="volo-btn volo-btn-sm"
          onClick={() => insertSnippet("\n:::bookshelf\n:::\n")}
        >
          {t("ed.bookshelf")}
        </button>
        {quickInserts.map((qi) => (
          <button
            key={`${qi.plugin_id || "core"}:${qi.id}`}
            type="button"
            class="volo-btn volo-btn-sm"
            onClick={() => insertSnippet(qi.template)}
          >
            + {qi.label}
          </button>
        ))}
        <label class="volo-btn volo-btn-sm" style={{ cursor: "pointer" }}>
          {uploading ? t("ed.uploading") : t("ed.upload_media")}
          <input
            type="file"
            accept="image/*,audio/*"
            style={{ display: "none" }}
            onChange={(e) => {
              const f = (e.currentTarget as HTMLInputElement).files?.[0];
              if (f) void uploadMediaFile(f);
              (e.currentTarget as HTMLInputElement).value = "";
            }}
          />
        </label>
        <button
          type="button"
          class="volo-btn volo-btn-sm"
          style={{ marginLeft: "auto" }}
          onClick={() => setShowPreview(!showPreview)}
        >
          {showPreview ? t("ed.hide_preview") : t("ed.show_preview")}
        </button>
      </div>

      <div class={showPreview ? "volo-editor-split" : ""}>
        <div class="volo-cm-host" ref={hostRef} />
        {showPreview && (
          <div
            class="volo-preview-pane"
            dangerouslySetInnerHTML={{ __html: previewHTML || `<p style='color:var(--volo-muted)'>${t("ed.live_preview_empty")}</p>` }}
          />
        )}
      </div>
    </div>
  );
}
