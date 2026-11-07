import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Compartment, EditorState, RangeSetBuilder } from "@codemirror/state";
import {
  Decoration,
  DecorationSet,
  EditorView,
  keymap,
  lineNumbers,
  placeholder,
  ViewPlugin,
  ViewUpdate,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, redo, undo } from "@codemirror/commands";
import { markdown } from "@codemirror/lang-markdown";
import { apiFetch, showToast, storageDriverSig } from "../api";
import { localeSig, t } from "../i18n";
import { AnimatedModal, ConsoleSelect } from "./UIControls";
import { MediaLibraryModal } from "./MediaLibraryModal";
import type { CM6EditorProps } from "./CM6Editor";

type EditorMode = "ir" | "wysiwyg" | "sv";

function buildInstantRenderPlugin() {
  const hideDeco = Decoration.replace({});
  const buildDecos = (view: EditorView): DecorationSet => {
    const builder = new RangeSetBuilder<Decoration>();
    const state = view.state;
    const activeLine = state.doc.lineAt(state.selection.main.head).number;
    let inFence = false;

    for (const { from, to } of view.visibleRanges) {
      let pos = from;
      while (pos <= to) {
        const line = state.doc.lineAt(pos);
        const text = line.text;
        const isCursorLine = line.number === activeLine;

        if (/^\s*```/.test(text)) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-fence" }));
          inFence = !inFence;
          pos = line.to + 1;
          continue;
        }

        if (inFence) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-codeblock-line" }));
          pos = line.to + 1;
          continue;
        }

        // Headings (# .. ######)
        const hm = /^(#{1,6})\s+/.exec(text);
        if (hm) {
          const lvl = hm[1].length;
          builder.add(line.from, line.from, Decoration.line({ class: `cm-ir-h cm-ir-h${lvl}` }));
          if (!isCursorLine) {
            builder.add(line.from, line.from + hm[0].length, hideDeco);
          }
        } else if (/^\s*>\s?/.test(text)) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-quote" }));
        } else if (/^\s*(?:---|\*\*\*|___)\s*$/.test(text)) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-hr" }));
        }

        // Inline decorations on non-cursor lines (sorted strictly by position)
        if (!isCursorLine && text.length > 0 && text.length < 2000) {
          interface SpanOp {
            from: number;
            to: number;
            deco: Decoration;
          }
          const ops: SpanOp[] = [];
          const occupied = new Uint8Array(text.length);
          const markFree = (s: number, e: number) => {
            for (let i = s; i < e; i++) if (occupied[i]) return false;
            for (let i = s; i < e; i++) occupied[i] = 1;
            return true;
          };

          // Inline code `...`
          const codeRe = /`([^`\n]+)`/g;
          let m: RegExpExecArray | null;
          while ((m = codeRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 1, deco: hideDeco });
              ops.push({
                from: line.from + s + 1,
                to: line.from + e - 1,
                deco: Decoration.mark({ class: "cm-ir-inline-code" }),
              });
              ops.push({ from: line.from + e - 1, to: line.from + e, deco: hideDeco });
            }
          }

          // Bold **...**
          const boldRe = /\*\*([^*\n]+)\*\*/g;
          while ((m = boldRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 2, deco: hideDeco });
              ops.push({
                from: line.from + s + 2,
                to: line.from + e - 2,
                deco: Decoration.mark({ class: "cm-ir-bold" }),
              });
              ops.push({ from: line.from + e - 2, to: line.from + e, deco: hideDeco });
            }
          }

          // Strikethrough ~~...~~
          const strikeRe = /~~([^~\n]+)~~/g;
          while ((m = strikeRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 2, deco: hideDeco });
              ops.push({
                from: line.from + s + 2,
                to: line.from + e - 2,
                deco: Decoration.mark({ class: "cm-ir-strike" }),
              });
              ops.push({ from: line.from + e - 2, to: line.from + e, deco: hideDeco });
            }
          }

          // Italic *...*
          const italicRe = /(?:^|[^*])\*([^*\s][^*\n]*?)\*(?!\*)/g;
          while ((m = italicRe.exec(text)) !== null) {
            const fullOffset = m[0].startsWith("*") ? 0 : 1;
            const s = m.index + fullOffset;
            const e = m.index + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 1, deco: hideDeco });
              ops.push({
                from: line.from + s + 1,
                to: line.from + e - 1,
                deco: Decoration.mark({ class: "cm-ir-italic" }),
              });
              ops.push({ from: line.from + e - 1, to: line.from + e, deco: hideDeco });
            }
          }

          // Markdown Links [label](url) (not images)
          const linkRe = /(?:^|[^!])\[([^\]\n]+)\]\(([^)\n]+)\)/g;
          while ((m = linkRe.exec(text)) !== null) {
            const lead = m[0].startsWith("[") ? 0 : 1;
            const s = m.index + lead;
            const e = m.index + m[0].length;
            const labelLen = m[1].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 1, deco: hideDeco });
              ops.push({
                from: line.from + s + 1,
                to: line.from + s + 1 + labelLen,
                deco: Decoration.mark({ class: "cm-ir-link" }),
              });
              ops.push({ from: line.from + s + 1 + labelLen, to: line.from + e, deco: hideDeco });
            }
          }

          ops.sort((a, b) => a.from - b.from || a.to - b.to);
          for (const op of ops) {
            if (op.from < op.to) {
              builder.add(op.from, op.to, op.deco);
            }
          }
        }

        pos = line.to + 1;
      }
    }
    return builder.finish();
  };

  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      constructor(view: EditorView) {
        this.decorations = buildDecos(view);
      }
      update(update: ViewUpdate) {
        if (update.docChanged || update.viewportChanged || update.selectionSet) {
          this.decorations = buildDecos(update.view);
        }
      }
    },
    { decorations: (v) => v.decorations }
  );
}

function serializeNodeToMarkdown(node: Node): string {
  if (node.nodeType === Node.TEXT_NODE) {
    return node.textContent || "";
  }
  if (node.nodeType !== Node.ELEMENT_NODE) return "";
  const el = node as HTMLElement;
  const tag = el.tagName.toLowerCase();

  // Preserve custom directive blocks if present
  const rawMd = el.getAttribute("data-raw-md");
  if (rawMd) return `\n${rawMd}\n`;

  const inner = () => Array.from(el.childNodes).map(serializeNodeToMarkdown).join("");

  switch (tag) {
    case "h1":
      return `\n# ${inner().trim()}\n\n`;
    case "h2":
      return `\n## ${inner().trim()}\n\n`;
    case "h3":
      return `\n### ${inner().trim()}\n\n`;
    case "h4":
      return `\n#### ${inner().trim()}\n\n`;
    case "h5":
      return `\n##### ${inner().trim()}\n\n`;
    case "h6":
      return `\n###### ${inner().trim()}\n\n`;
    case "p":
    case "div": {
      const txt = inner().trim();
      return txt ? `${txt}\n\n` : "\n";
    }
    case "br":
      return "\n";
    case "strong":
    case "b":
      return `**${inner()}**`;
    case "em":
    case "i":
      return `*${inner()}*`;
    case "del":
    case "s":
    case "strike":
      return `~~${inner()}~~`;
    case "code":
      if (el.parentElement?.tagName.toLowerCase() === "pre") {
        return el.textContent || "";
      }
      return `\`${el.textContent || ""}\``;
    case "pre": {
      const codeEl = el.querySelector("code");
      const cls = codeEl?.className || "";
      const langMatch = /language-([a-zA-Z0-9_-]+)/.exec(cls);
      const lang = langMatch ? langMatch[1] : "";
      const raw = (codeEl ? codeEl.textContent : el.textContent) || "";
      return `\n\`\`\`${lang}\n${raw.replace(/\n$/, "")}\n\`\`\`\n\n`;
    }
    case "blockquote": {
      const lines = inner()
        .trim()
        .split("\n")
        .map((l) => `> ${l}`)
        .join("\n");
      return `\n${lines}\n\n`;
    }
    case "ul": {
      const items = Array.from(el.children)
        .filter((c) => c.tagName.toLowerCase() === "li")
        .map((li) => {
          const cb = li.querySelector('input[type="checkbox"]') as HTMLInputElement | null;
          const txt = Array.from(li.childNodes)
            .filter((n) => !(n instanceof HTMLElement && n.tagName.toLowerCase() === "input"))
            .map(serializeNodeToMarkdown)
            .join("")
            .trim();
          if (cb) return `- [${cb.checked ? "x" : " "}] ${txt}`;
          return `- ${txt}`;
        })
        .join("\n");
      return `\n${items}\n\n`;
    }
    case "ol": {
      const items = Array.from(el.children)
        .filter((c) => c.tagName.toLowerCase() === "li")
        .map((li, idx) => `${idx + 1}. ${Array.from(li.childNodes).map(serializeNodeToMarkdown).join("").trim()}`)
        .join("\n");
      return `\n${items}\n\n`;
    }
    case "a": {
      const href = el.getAttribute("href") || "";
      return `[${inner() || href}](${href})`;
    }
    case "img": {
      const src = el.getAttribute("src") || "";
      const alt = el.getAttribute("alt") || "";
      return `![${alt}](${src})`;
    }
    case "hr":
      return `\n---\n\n`;
    case "table": {
      const rows = Array.from(el.querySelectorAll("tr"));
      if (rows.length === 0) return "";
      const matrix = rows.map((tr) =>
        Array.from(tr.querySelectorAll("th,td")).map((cell) => (cell.textContent || "").trim().replace(/\|/g, "\\|"))
      );
      const cols = Math.max(1, ...matrix.map((r) => r.length));
      const padRow = (r: string[]) => {
        const copy = [...r];
        while (copy.length < cols) copy.push("");
        return `| ${copy.join(" | ")} |`;
      };
      const head = padRow(matrix[0]);
      const sep = `| ${Array.from({ length: cols }, () => "---").join(" | ")} |`;
      const body = matrix.slice(1).map(padRow).join("\n");
      return `\n${head}\n${sep}${body ? `\n${body}` : ""}\n\n`;
    }
    default:
      return inner();
  }
}

function htmlToMarkdown(root: HTMLElement): string {
  return Array.from(root.childNodes)
    .map(serializeNodeToMarkdown)
    .join("")
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

interface OutlineItem {
  level: number;
  text: string;
  line: number;
}

export function CM6EditorCore({ value, onChange, quickInserts = [], onSave }: CM6EditorProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const wysiwygRef = useRef<HTMLDivElement | null>(null);
  const phCompartment = useRef(new Compartment());
  const modeCompartment = useRef(new Compartment());
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onSaveRef = useRef(onSave);
  onSaveRef.current = onSave;

  const curLocale = localeSig.value;
  const [mode, setMode] = useState<EditorMode>(() => {
    const saved = localStorage.getItem("volo_editor_mode");
    return saved === "wysiwyg" || saved === "sv" || saved === "ir" ? saved : "ir";
  });
  const [previewHTML, setPreviewHTML] = useState<string>("");
  const [uploading, setUploading] = useState<boolean>(false);
  const [fullscreen, setFullscreen] = useState<boolean>(false);
  const [showOutline, setShowOutline] = useState<boolean>(false);
  const [mobilePane, setMobilePane] = useState<"write" | "preview" | "outline">("write");
  const [headingOpen, setHeadingOpen] = useState<boolean>(false);
  const [mediaMenuOpen, setMediaMenuOpen] = useState<boolean>(false);
  const [mediaModalOpen, setMediaModalOpen] = useState<boolean>(false);
  const [dynQuickInserts, setDynQuickInserts] = useState<CM6EditorProps["quickInserts"]>([]);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  // Quick Insert Modal State
  const [qiOpen, setQiOpen] = useState<boolean>(false);
  const [qiTab, setQiTab] = useState<"media" | "books" | "lock" | "cite">("media");
  const [qiMedia, setQiMedia] = useState<any[]>([]);
  const [qiBooks, setQiBooks] = useState<any[]>([]);
  const [qiArticles, setQiArticles] = useState<any[]>([]);
  const [qiArtSearch, setQiArtSearch] = useState<string>("");
  const [lockMode, setLockMode] = useState<"pwd" | "user">("pwd");
  const [lockPwd, setLockPwd] = useState<string>("");
  const [lockHint, setLockHint] = useState<string>("");
  const [lockBody, setLockBody] = useState<string>("");

  const switchMode = (next: EditorMode) => {
    setMode(next);
    localStorage.setItem("volo_editor_mode", next);
  };

  // Word, char, line stats + live outline
  const { stats, outline } = useMemo(() => {
    const raw = value || "";
    const linesArr = raw.split("\n");
    const cjk = (raw.match(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g) || []).length;
    const latin = (
      raw
        .replace(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g, " ")
        .match(/[A-Za-z0-9_'-]+/g) || []
    ).length;
    const toc: OutlineItem[] = [];
    let fenced = false;
    linesArr.forEach((ln, idx) => {
      if (/^\s*```/.test(ln)) {
        fenced = !fenced;
        return;
      }
      if (fenced) return;
      const m = /^(#{1,4})\s+(.+)$/.exec(ln);
      if (m) {
        toc.push({
          level: m[1].length,
          text: m[2].replace(/[*_`~#]/g, "").trim(),
          line: idx + 1,
        });
      }
    });
    return {
      stats: { words: cjk + latin, chars: raw.length, lines: linesArr.length },
      outline: toc,
    };
  }, [value]);

  const insertSnippet = (snippet: string) => {
    if (mode === "wysiwyg") {
      const next = `${value}${value.endsWith("\n") || !value ? "" : "\n"}${snippet}`;
      onChangeRef.current(next);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert: snippet },
      selection: { anchor: sel.from + snippet.length },
    });
    view.focus();
  };

  const wrapSelection = (before: string, after: string, fallback: string) => {
    if (mode === "wysiwyg" && wysiwygRef.current) {
      wysiwygRef.current.focus();
      if (before === "**") document.execCommand("bold");
      else if (before === "*") document.execCommand("italic");
      else if (before === "~~") document.execCommand("strikeThrough");
      else document.execCommand("insertHTML", false, `<code>${fallback}</code>`);
      onChangeRef.current(htmlToMarkdown(wysiwygRef.current));
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const selected = view.state.sliceDoc(sel.from, sel.to) || fallback;
    const insert = `${before}${selected}${after}`;
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert },
      selection: { anchor: sel.from + before.length, head: sel.from + before.length + selected.length },
    });
    view.focus();
  };

  const prefixCurrentLine = (prefix: string) => {
    if (mode === "wysiwyg" && wysiwygRef.current) {
      wysiwygRef.current.focus();
      if (prefix.startsWith("#")) {
        const lvl = prefix.trim().length;
        document.execCommand("formatBlock", false, `H${lvl}`);
      } else if (prefix === "- ") {
        document.execCommand("insertUnorderedList");
      } else if (prefix === "1. ") {
        document.execCommand("insertOrderedList");
      } else if (prefix === "> ") {
        document.execCommand("formatBlock", false, "BLOCKQUOTE");
      }
      onChangeRef.current(htmlToMarkdown(wysiwygRef.current));
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const line = view.state.doc.lineAt(view.state.selection.main.from);
    const cleaned = line.text.replace(/^(?:#{1,6}\s+|>\s+|-\s+\[[ xX]\]\s+|-\s+|\d+\.\s+)/, "");
    const nextText = `${prefix}${cleaned}`;
    view.dispatch({
      changes: { from: line.from, to: line.to, insert: nextText },
      selection: { anchor: line.from + nextText.length },
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

  const modeExtensions = (m: EditorMode) =>
    m === "ir" ? [buildInstantRenderPlugin()] : [lineNumbers()];

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

    const shortcutKeymap = keymap.of([
      {
        key: "Mod-s",
        run: () => {
          if (onSaveRef.current) {
            onSaveRef.current();
          } else {
            window.dispatchEvent(new CustomEvent("volo:save"));
          }
          return true;
        },
      },
      {
        key: "Mod-b",
        run: () => {
          wrapSelection("**", "**", "bold");
          return true;
        },
      },
      {
        key: "Mod-i",
        run: () => {
          wrapSelection("*", "*", "italic");
          return true;
        },
      },
      {
        key: "Mod-k",
        run: () => {
          wrapSelection("[", "](https://)", t("ed.link_text_ph"));
          return true;
        },
      },
    ]);

    const state = EditorState.create({
      doc: value,
      extensions: [
        modeCompartment.current.of(modeExtensions(mode)),
        history(),
        markdown(),
        EditorView.lineWrapping,
        phCompartment.current.of(placeholder(t("ed.placeholder"))),
        shortcutKeymap,
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

  useEffect(() => {
    if (quickInserts.length > 0) return;
    apiFetch<{ items?: CM6EditorProps["quickInserts"]; quick_inserts?: CM6EditorProps["quickInserts"] }>(
      "/api/admin/plugins/quick-inserts"
    )
      .then((r) => setDynQuickInserts(r.items || r.quick_inserts || []))
      .catch(() => {});
  }, [quickInserts.length]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      effects: modeCompartment.current.reconfigure(modeExtensions(mode)),
    });
  }, [mode]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      effects: phCompartment.current.reconfigure(placeholder(t("ed.placeholder"))),
    });
  }, [curLocale]);

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

  // Fetch server-rendered HTML for Split Preview (`sv`) and Visual (`wysiwyg`) modes
  useEffect(() => {
    if (mode === "ir") return;
    const timer = window.setTimeout(async () => {
      try {
        const res = await apiFetch<{ html: string }>("/api/admin/articles/preview", {
          method: "POST",
          body: JSON.stringify({ content_md: value, locale: curLocale }),
        });
        const html = res.html || "";
        setPreviewHTML(html);
        if (mode === "wysiwyg" && wysiwygRef.current && document.activeElement !== wysiwygRef.current) {
          wysiwygRef.current.innerHTML = html || `<p><br></p>`;
        }
      } catch {
        // Ignore transient preview errors while typing
      }
    }, 180);
    return () => window.clearTimeout(timer);
  }, [value, mode, curLocale]);

  useEffect(() => {
    if (!fullscreen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setFullscreen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [fullscreen]);

  useEffect(() => {
    if (!headingOpen) return;
    const close = () => setHeadingOpen(false);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [headingOpen]);

  useEffect(() => {
    if (!mediaMenuOpen) return;
    const close = () => setMediaMenuOpen(false);
    document.addEventListener("click", close);
    return () => document.removeEventListener("click", close);
  }, [mediaMenuOpen]);

  // Load Quick Insert data when modal opens
  useEffect(() => {
    if (!qiOpen) return;
    if (qiTab === "media" && qiMedia.length === 0) {
      apiFetch<{ items?: any[]; media?: any[] }>("/api/admin/media")
        .then((r) => setQiMedia(r.items || r.media || []))
        .catch(() => {});
    } else if (qiTab === "books" && qiBooks.length === 0) {
      apiFetch<{ items?: any[]; books?: any[] }>("/api/admin/books")
        .then((r) => setQiBooks(r.items || r.books || []))
        .catch(() => {});
    } else if (qiTab === "cite" && qiArticles.length === 0) {
      apiFetch<{ items?: any[]; articles?: any[] }>("/api/admin/articles")
        .then((r) =>
          setQiArticles((r.items || r.articles || []).map((it: any) => it.article || it))
        )
        .catch(() => {});
    }
  }, [qiOpen, qiTab]);

  const jumpToLine = (lineNo: number) => {
    setMobilePane("write");
    const view = viewRef.current;
    if (!view || mode === "wysiwyg") return;
    const safe = Math.min(Math.max(1, lineNo), view.state.doc.lines);
    const ln = view.state.doc.line(safe);
    view.dispatch({
      selection: { anchor: ln.from },
      effects: EditorView.scrollIntoView(ln.from, { y: "start" }),
    });
    view.focus();
  };

  const mergedQuickInserts = quickInserts.length > 0 ? quickInserts : dynQuickInserts || [];

  return (
    <div class={`volo-studio-editor ${fullscreen ? "is-fullscreen" : ""}`}>
      {/* Top Sticky Rich Icon Toolbar */}
      <div class="volo-ed-toolbar">
        <div class="volo-ed-tools">
          <button
            type="button"
            class="volo-ed-btn"
            title={t("ed.undo")}
            onClick={() => {
              if (mode === "wysiwyg") document.execCommand("undo");
              else if (viewRef.current) undo(viewRef.current);
            }}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M9 14 4 9l5-5" />
              <path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5v0a5.5 5.5 0 0 1-5.5 5.5H11" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            title={t("ed.redo")}
            onClick={() => {
              if (mode === "wysiwyg") document.execCommand("redo");
              else if (viewRef.current) redo(viewRef.current);
            }}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="m15 14 5-5-5-5" />
              <path d="M20 9H9.5A5.5 5.5 0 0 0 4 14.5v0A5.5 5.5 0 0 0 9.5 20H13" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          {/* Heading H1-H6 Popover */}
          <div class="volo-pop-wrap" onClick={(e) => e.stopPropagation()}>
            <button
              type="button"
              class="volo-ed-btn"
              title={t("ed.heading")}
              onClick={() => setHeadingOpen((p) => !p)}
            >
              <span style={{ fontWeight: 750, fontSize: "12.5px" }}>H▾</span>
            </button>
            {headingOpen && (
              <div class="volo-cselect-menu" style={{ minWidth: "110px" }}>
                {[1, 2, 3, 4, 5, 6].map((lvl) => (
                  <button
                    key={lvl}
                    type="button"
                    class="volo-cselect-item"
                    onClick={() => {
                      prefixCurrentLine(`${"#".repeat(lvl)} `);
                      setHeadingOpen(false);
                    }}
                  >
                    <span style={{ fontWeight: 700, fontSize: `${Math.max(12, 17 - lvl)}px` }}>H{lvl}</span>
                  </button>
                ))}
              </div>
            )}
          </div>

          <button type="button" class="volo-ed-btn" title={t("ed.bold")} onClick={() => wrapSelection("**", "**", "bold")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round">
              <path d="M6 4h8a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z" />
              <path d="M6 12h9a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.italic")} onClick={() => wrapSelection("*", "*", "italic")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="19" y1="4" x2="10" y2="4" />
              <line x1="14" y1="20" x2="5" y2="20" />
              <line x1="15" y1="4" x2="9" y2="20" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.strike")} onClick={() => wrapSelection("~~", "~~", "text")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M16 4H9a3 3 0 0 0-2.83 4" />
              <path d="M14 12a4 4 0 0 1 0 8H6" />
              <line x1="4" y1="12" x2="20" y2="12" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.code")} onClick={() => wrapSelection("`", "`", "code")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <polyline points="16 18 22 12 16 6" />
              <polyline points="8 6 2 12 8 18" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          <button type="button" class="volo-ed-btn" title={t("ed.quote")} onClick={() => prefixCurrentLine("> ")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M3 21c3 0 7-1 7-8V5c0-1.25-.756-2.017-2-2H4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2 1 0 1 0 1 1v1c0 1-1 2-2 2s-1 .008-1 1.031V20c0 1 0 1 1 1z" />
              <path d="M15 21c3 0 7-1 7-8V5c0-1.25-.757-2.017-2-2h-4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2h.75c0 2.25.25 4-2.75 4v3c0 1 0 1 1 1z" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.ul")} onClick={() => prefixCurrentLine("- ")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <line x1="8" y1="6" x2="21" y2="6" />
              <line x1="8" y1="12" x2="21" y2="12" />
              <line x1="8" y1="18" x2="21" y2="18" />
              <circle cx="4" cy="6" r="1" fill="currentColor" />
              <circle cx="4" cy="12" r="1" fill="currentColor" />
              <circle cx="4" cy="18" r="1" fill="currentColor" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.ol")} onClick={() => prefixCurrentLine("1. ")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <line x1="10" y1="6" x2="21" y2="6" />
              <line x1="10" y1="12" x2="21" y2="12" />
              <line x1="10" y1="18" x2="21" y2="18" />
              <path d="M4 6h1v4" />
              <path d="M4 10h2" />
              <path d="M6 18H4c0-1 2-2 2-3s-1-1.5-2-1" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.task")} onClick={() => prefixCurrentLine("- [ ] ")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="5" width="6" height="6" rx="1.2" />
              <path d="m3 17 2 2 4-4" />
              <line x1="13" y1="8" x2="21" y2="8" />
              <line x1="13" y1="17" x2="21" y2="17" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          <button type="button" class="volo-ed-btn" title={t("ed.link")} onClick={() => wrapSelection("[", "](https://)", t("ed.link_text_ph"))}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
              <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            title={t("ed.table")}
            onClick={() => insertSnippet(`\n${t("ed.table_tpl")}\n`)}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <line x1="3" y1="9" x2="21" y2="9" />
              <line x1="3" y1="15" x2="21" y2="15" />
              <line x1="9" y1="3" x2="9" y2="21" />
              <line x1="15" y1="3" x2="15" y2="21" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.code_block")} onClick={() => insertSnippet("\n```go\n\n```\n")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="4" width="18" height="16" rx="2" />
              <polyline points="8 10 11 12.5 8 15" />
              <line x1="13" y1="15" x2="16" y2="15" />
            </svg>
          </button>
          <button type="button" class="volo-ed-btn" title={t("ed.hr")} onClick={() => insertSnippet("\n---\n")}>
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round">
              <line x1="4" y1="12" x2="20" y2="12" />
            </svg>
          </button>

          {/* Upload Media / Insert from Media Library (works for both local & S3) */}
          <div class="volo-pop-wrap" onClick={(e) => e.stopPropagation()}>
            <button
              type="button"
              class={`volo-ed-btn ${mediaMenuOpen ? "active" : ""}`}
              title={uploading ? t("ed.uploading") : t("ed.upload_media")}
              onClick={() => setMediaMenuOpen((v) => !v)}
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <rect x="3" y="3" width="18" height="18" rx="2" />
                <circle cx="8.5" cy="8.5" r="1.5" />
                <polyline points="21 15 16 10 5 21" />
              </svg>
            </button>
            <input
              ref={fileInputRef}
              type="file"
              accept="image/*,audio/*"
              style={{ display: "none" }}
              onChange={(e) => {
                const f = (e.currentTarget as HTMLInputElement).files?.[0];
                if (f) void uploadMediaFile(f);
                (e.currentTarget as HTMLInputElement).value = "";
              }}
            />
            {mediaMenuOpen && (
              <div class="volo-pop-menu volo-ed-media-pop">
                <button
                  type="button"
                  class="volo-pop-item"
                  onClick={() => {
                    setMediaMenuOpen(false);
                    fileInputRef.current?.click();
                  }}
                >
                  <span>{t("ed.upload_local_file")}</span>
                </button>
                <button
                  type="button"
                  class="volo-pop-item"
                  onClick={() => {
                    setMediaMenuOpen(false);
                    setMediaModalOpen(true);
                  }}
                >
                  <span>{t("ed.insert_from_library")}</span>
                </button>
              </div>
            )}
          </div>

          {/* Quick Insert + Modal Trigger */}
          <button
            type="button"
            class="volo-ed-btn volo-ed-btn-accent"
            title={t("ed.quick_insert")}
            onClick={() => setQiOpen(true)}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round">
              <line x1="12" y1="5" x2="12" y2="19" />
              <line x1="5" y1="12" x2="19" y2="12" />
            </svg>
          </button>

          {mergedQuickInserts.map((qi) => (
            <button
              key={`${qi.plugin_id || "core"}:${qi.id}`}
              type="button"
              class="volo-ed-chip"
              onClick={() => insertSnippet(qi.template)}
            >
              + {qi.label}
            </button>
          ))}
        </div>

        {/* Right Controls: Outline + 3-Mode Switcher + Fullscreen */}
        <div class="volo-ed-right">
          <div class="volo-ed-modes" role="tablist">
            <button
              type="button"
              class={`volo-ed-mode-btn ${mode === "ir" ? "active" : ""}`}
              onClick={() => {
                switchMode("ir");
                setMobilePane("write");
              }}
            >
              {t("ed.mode_ir")}
            </button>
            <button
              type="button"
              class={`volo-ed-mode-btn ${mode === "wysiwyg" ? "active" : ""}`}
              onClick={() => {
                switchMode("wysiwyg");
                setMobilePane("write");
              }}
            >
              {t("ed.mode_wysiwyg")}
            </button>
            <button
              type="button"
              class={`volo-ed-mode-btn ${mode === "sv" ? "active" : ""}`}
              onClick={() => switchMode("sv")}
            >
              {t("ed.mode_sv")}
            </button>
          </div>

          <button
            type="button"
            class={`volo-ed-btn ${showOutline ? "active" : ""}`}
            title={t("ed.outline")}
            onClick={() =>
              setShowOutline((v) => {
                const next = !v;
                if (next) setMobilePane("outline");
                else if (mobilePane === "outline") setMobilePane("write");
                return next;
              })
            }
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <line x1="3" y1="6" x2="21" y2="6" />
              <line x1="7" y1="12" x2="21" y2="12" />
              <line x1="11" y1="18" x2="21" y2="18" />
            </svg>
          </button>

          <button
            type="button"
            class={`volo-ed-btn ${fullscreen ? "active" : ""}`}
            title={fullscreen ? t("ed.exit_fullscreen") : t("ed.fullscreen")}
            onClick={() => setFullscreen((v) => !v)}
          >
            {fullscreen ? (
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <path d="M8 3v3a2 2 0 0 1-2 2H3" />
                <path d="M21 8h-3a2 2 0 0 1-2-2V3" />
                <path d="M3 16h3a2 2 0 0 1 2 2v3" />
                <path d="M16 21v-3a2 2 0 0 1 2-2h3" />
              </svg>
            ) : (
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <polyline points="15 3 21 3 21 9" />
                <polyline points="9 21 3 21 3 15" />
                <line x1="21" y1="3" x2="14" y2="10" />
                <line x1="3" y1="21" x2="10" y2="14" />
              </svg>
            )}
          </button>
        </div>
      </div>

      {/* Mobile Segmented Pane Bar when Split View or Outline is active */}
      {(mode === "sv" || showOutline) && (
        <div class="volo-ed-mobile-tabs">
          <button
            type="button"
            class={`volo-filter-pill ${mobilePane === "write" ? "active" : ""}`}
            onClick={() => setMobilePane("write")}
          >
            {t("ed.mob_write")}
          </button>
          {mode === "sv" && (
            <button
              type="button"
              class={`volo-filter-pill ${mobilePane === "preview" ? "active" : ""}`}
              onClick={() => setMobilePane("preview")}
            >
              {t("ed.mob_preview")}
            </button>
          )}
          {showOutline && (
            <button
              type="button"
              class={`volo-filter-pill ${mobilePane === "outline" ? "active" : ""}`}
              onClick={() => setMobilePane("outline")}
            >
              {t("ed.outline")}
            </button>
          )}
        </div>
      )}

      {/* Main Editor Body */}
      <div class={`volo-ed-body mode-${mode} mob-pane-${mobilePane} ${showOutline ? "with-outline" : ""}`}>
        {/* CM6 Host stays mounted for undo history; hidden only in pure visual mode */}
        <div
          class={`volo-cm-host ${mode === "ir" ? "is-ir-mode" : ""}`}
          ref={hostRef}
          style={{ display: mode === "wysiwyg" ? "none" : undefined }}
        />

        {/* Visual WYSIWYG Editable Surface */}
        {mode === "wysiwyg" && (
          <div
            ref={wysiwygRef}
            class="volo-wysiwyg-surface"
            contentEditable
            onInput={(e) => {
              const md = htmlToMarkdown(e.currentTarget as HTMLElement);
              onChangeRef.current(md);
            }}
            onBlur={(e) => {
              const md = htmlToMarkdown(e.currentTarget as HTMLElement);
              onChangeRef.current(md);
            }}
          />
        )}

        {/* Split Live Preview Pane */}
        {mode === "sv" && (
          <div
            class="volo-preview-pane"
            dangerouslySetInnerHTML={{
              __html: previewHTML || `<p style="color:var(--volo-muted)">${t("ed.live_preview_empty")}</p>`,
            }}
          />
        )}

        {/* Optional Collapsible Outline Drawer */}
        {showOutline && (
          <aside class="volo-ed-outline">
            <div class="volo-ed-outline-head">{t("ed.outline")}</div>
            {outline.length === 0 ? (
              <div class="volo-help" style={{ padding: "10px 12px" }}>
                {t("ed.outline_empty")}
              </div>
            ) : (
              <div class="volo-ed-outline-list">
                {outline.map((item, idx) => (
                  <button
                    key={`${item.line}-${idx}`}
                    type="button"
                    class="volo-ed-outline-item"
                    style={{ paddingLeft: `${10 + (item.level - 1) * 12}px` }}
                    onClick={() => jumpToLine(item.line)}
                  >
                    {item.text}
                  </button>
                ))}
              </div>
            )}
          </aside>
        )}
      </div>

      {/* Bottom Live Stats Footer */}
      <div class="volo-ed-footer">
        <span>{t("ed.stats_bar", { words: stats.words, chars: stats.chars, lines: stats.lines })}</span>
        <span>{uploading ? t("ed.uploading") : t(`ed.mode_${mode}`)}</span>
      </div>

      {/* Media Library Picker Modal */}
      <MediaLibraryModal
        open={mediaModalOpen}
        onClose={() => setMediaModalOpen(false)}
        filterKind="all"
        onSelect={(asset) => {
          const name = asset.original_name || asset.id;
          const isImg = (asset.mime_type || "").startsWith("image/");
          const isAudio = (asset.mime_type || "").startsWith("audio/");
          if (isImg) {
            insertSnippet(`![${name}](${asset.public_url})`);
          } else if (isAudio) {
            insertSnippet(`[${t("ed.audio_label")}: ${name}](${asset.public_url})`);
          } else {
            insertSnippet(`[${name}](${asset.public_url})`);
          }
        }}
      />

      {/* Quick Insert 4-Tab AnimatedModal */}
      <AnimatedModal
        open={qiOpen}
        onClose={() => setQiOpen(false)}
        title={t("ed.quick_insert")}
        maxWidth="660px"
        footer={
          qiTab === "lock" ? (
            <>
              <button type="button" class="volo-btn" onClick={() => setQiOpen(false)}>
                {t("common.cancel")}
              </button>
              <button
                type="button"
                class="volo-btn volo-btn-primary"
                onClick={() => {
                  const attr =
                    lockMode === "user"
                      ? `type="user"${lockHint ? ` hint="${lockHint}"` : ""}`
                      : `pwd="${lockPwd || "123456"}"${lockHint ? ` hint="${lockHint}"` : ""}`;
                  insertSnippet(`\n:::lock ${attr}\n${lockBody || "..."}\n:::\n`);
                  setQiOpen(false);
                }}
              >
                {t("ed.qi_insert_btn")}
              </button>
            </>
          ) : (
            <button type="button" class="volo-btn" onClick={() => setQiOpen(false)}>
              {t("common.close")}
            </button>
          )
        }
      >
        <div class="volo-subtabs" style={{ marginBottom: "14px" }}>
          {(["media", "books", "lock", "cite"] as const).map((tabId) => (
            <button
              key={tabId}
              type="button"
              class={`volo-tab ${qiTab === tabId ? "active" : ""}`}
              onClick={() => setQiTab(tabId)}
            >
              {t(`ed.qi_${tabId}`)}
            </button>
          ))}
        </div>

        {qiTab === "media" && (
          <div class="volo-qi-grid">
            {qiMedia.length === 0 ? (
              <div class="volo-help">{t("tbl.empty")}</div>
            ) : (
              qiMedia.slice(0, 24).map((m) => {
                const name = m.orig_filename || m.original_name || m.id;
                const isImg = String(m.mime_type || "").startsWith("image/");
                return (
                  <button
                    key={m.id}
                    type="button"
                    class="volo-qi-media-card"
                    onClick={() => {
                      insertSnippet(isImg ? `![${name}](${m.public_url})` : `[${name}](${m.public_url})`);
                      setQiOpen(false);
                    }}
                  >
                    {isImg ? (
                      <img src={m.public_url} alt={name} loading="lazy" />
                    ) : (
                      <div class="volo-qi-file-badge">{m.mime_type || "FILE"}</div>
                    )}
                    <span>{name}</span>
                  </button>
                );
              })
            )}
          </div>
        )}

        {qiTab === "books" && (
          <div>
            <div style={{ marginBottom: "10px", display: "flex", justifyContent: "flex-end" }}>
              <button
                type="button"
                class="volo-btn volo-btn-sm volo-btn-primary"
                onClick={() => {
                  insertSnippet("\n:::bookshelf\n:::\n");
                  setQiOpen(false);
                }}
              >
                + {t("ed.bookshelf")}
              </button>
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: "8px", maxHeight: "260px", overflowY: "auto" }}>
              {qiBooks.map((b) => (
                <div key={b.id} class="volo-subcard" style={{ padding: "10px 12px", margin: 0, display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                  <div>
                    <div style={{ fontWeight: 650, fontSize: "13px" }}>{b.title}</div>
                    <div class="volo-help">{b.description || `/books/${b.slug}`}</div>
                  </div>
                  <button
                    type="button"
                    class="volo-btn volo-btn-sm"
                    onClick={() => {
                      const body = b.description || `[${b.title}](/books/${b.slug})`;
                      insertSnippet(`\n:::book title="${b.title}" status="reading"\n${body}\n:::\n`);
                      setQiOpen(false);
                    }}
                  >
                    {t("ed.qi_insert_btn")}
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}

        {qiTab === "lock" && (
          <form autoComplete="off" onSubmit={(e) => e.preventDefault()}>
            <div class="volo-grid-2">
              <div class="volo-field">
                <label>{t("ed.qi_lock_mode")}</label>
                <ConsoleSelect
                  value={lockMode}
                  onChange={(v) => setLockMode(v as "pwd" | "user")}
                  options={[
                    { value: "pwd", label: t("ed.qi_lock_pwd_mode") },
                    { value: "user", label: t("ed.qi_lock_user_mode") },
                  ]}
                />
              </div>
              {lockMode === "pwd" ? (
                <div class="volo-field">
                  <label>{t("ed.qi_lock_pwd_lbl")}</label>
                  <input
                    class="volo-input"
                    name="qi_block_lock_token"
                    autoComplete="off"
                    value={lockPwd}
                    onInput={(e) => setLockPwd((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              ) : (
                <div class="volo-field">
                  <label>{t("ed.qi_lock_hint_lbl")}</label>
                  <input
                    class="volo-input"
                    value={lockHint}
                    onInput={(e) => setLockHint((e.currentTarget as HTMLInputElement).value)}
                  />
                </div>
              )}
            </div>
            {lockMode === "pwd" && (
              <div class="volo-field">
                <label>{t("ed.qi_lock_hint_lbl")}</label>
                <input
                  class="volo-input"
                  value={lockHint}
                  onInput={(e) => setLockHint((e.currentTarget as HTMLInputElement).value)}
                />
              </div>
            )}
            <div class="volo-field" style={{ marginBottom: 0 }}>
              <label>{t("ed.qi_lock_body_lbl")}</label>
              <textarea
                class="volo-textarea"
                style={{ minHeight: "88px" }}
                value={lockBody}
                onInput={(e) => setLockBody((e.currentTarget as HTMLTextAreaElement).value)}
              />
            </div>
          </form>
        )}

        {qiTab === "cite" && (
          <div>
            <div class="volo-field">
              <label>{t("ed.qi_cite")}</label>
              <input
                class="volo-input"
                placeholder={t("ed.qi_search_art")}
                value={qiArtSearch}
                onInput={(e) => setQiArtSearch((e.currentTarget as HTMLInputElement).value)}
              />
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: "6px", maxHeight: "240px", overflowY: "auto" }}>
              {qiArticles
                .filter((a) => !qiArtSearch || String(a.title || "").toLowerCase().includes(qiArtSearch.toLowerCase()))
                .slice(0, 20)
                .map((a) => (
                  <div key={a.id} class="volo-subcard" style={{ padding: "8px 12px", margin: 0, display: "flex", justifyContent: "space-between", alignItems: "center" }}>
                    <span style={{ fontSize: "13px", fontWeight: 550 }}>{a.title}</span>
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      onClick={() => {
                        insertSnippet(`[${a.title}](/posts/${a.slug})`);
                        setQiOpen(false);
                      }}
                    >
                      {t("ed.qi_insert_btn")}
                    </button>
                  </div>
                ))}
            </div>
          </div>
        )}
      </AnimatedModal>
    </div>
  );
}
