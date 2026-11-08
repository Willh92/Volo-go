import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import type { CSSProperties } from "preact";
import { Compartment, EditorState, Prec, RangeSetBuilder } from "@codemirror/state";
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
import { defaultKeymap, history, historyKeymap, indentWithTab, redo, undo } from "@codemirror/commands";
import { markdownKeymap, markdownLanguage, pasteURLAsLink } from "@codemirror/lang-markdown";
import { apiFetch, openConfirm, showToast } from "../api";
import { localeSig, t } from "../i18n";
import {
  AnimatedModal,
  computePortalMenuStyle,
  ConsoleEmpty,
  ConsoleSelect,
  PaginationBar,
  paginateItems,
  useDebouncedValue,
} from "./UIControls";
import { MediaLibraryModal } from "./MediaLibraryModal";
import type { CM6CssEditorProps, CM6EditorProps } from "./CM6Editor";

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
    case "figure": {
      if (el.classList.contains("volo-codeblock") || el.hasAttribute("data-lang")) {
        const codeEl = el.querySelector("code");
        const preEl = el.querySelector("pre");
        const cls = codeEl?.className || "";
        const langMatch = /language-([a-zA-Z0-9_-]+)/.exec(cls);
        const lang =
          el.getAttribute("data-lang") ||
          (langMatch ? langMatch[1] : "") ||
          (el.querySelector(".volo-code-lang")?.textContent || "").trim();
        const srcNode = (codeEl || preEl || el).cloneNode(true) as HTMLElement;
        srcNode.querySelectorAll(".ln, .lnt, .volo-code-head, button").forEach((n) => n.remove());
        const raw = srcNode.textContent || "";
        return `\n\`\`\`${lang}\n${raw.replace(/\n$/, "")}\n\`\`\`\n\n`;
      }
      return inner();
    }
    case "code":
      if (el.parentElement?.tagName.toLowerCase() === "pre") {
        const clone = el.cloneNode(true) as HTMLElement;
        clone.querySelectorAll(".ln, .lnt").forEach((n) => n.remove());
        return clone.textContent || "";
      }
      return `\`${el.textContent || ""}\``;
    case "pre": {
      const codeEl = el.querySelector("code");
      const cls = codeEl?.className || "";
      const langMatch = /language-([a-zA-Z0-9_-]+)/.exec(cls);
      const figLang = el.closest("figure[data-lang]")?.getAttribute("data-lang") || "";
      const lang = figLang || (langMatch ? langMatch[1] : "");
      const srcNode = (codeEl || el).cloneNode(true) as HTMLElement;
      srcNode.querySelectorAll(".ln, .lnt, .volo-code-head, button").forEach((n) => n.remove());
      const raw = srcNode.textContent || "";
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
  const [headingStyle, setHeadingStyle] = useState<CSSProperties>({});
  const headingBtnRef = useRef<HTMLButtonElement | null>(null);
  const headingMenuRef = useRef<HTMLDivElement | null>(null);

  const [mediaMenuOpen, setMediaMenuOpen] = useState<boolean>(false);
  const [mediaStyle, setMediaStyle] = useState<CSSProperties>({});
  const mediaBtnRef = useRef<HTMLButtonElement | null>(null);
  const mediaMenuRef = useRef<HTMLDivElement | null>(null);

  const [mediaModalOpen, setMediaModalOpen] = useState<boolean>(false);
  const [dynQuickInserts, setDynQuickInserts] = useState<CM6EditorProps["quickInserts"]>([]);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  // Quick Insert Modal State
  const [qiOpen, setQiOpen] = useState<boolean>(false);
  const [qiTab, setQiTab] = useState<"media" | "books" | "lock" | "cite">("media");
  const [qiMedia, setQiMedia] = useState<any[]>([]);
  const [qiMediaTotal, setQiMediaTotal] = useState<number>(0);
  const [qiMediaPage, setQiMediaPage] = useState<number>(1);
  const [qiMediaSearch, setQiMediaSearch] = useState<string>("");
  const [qiMediaMime, setQiMediaMime] = useState<"" | "image" | "audio">("");
  const debouncedQiMediaQ = useDebouncedValue(qiMediaSearch, 250);

  const [qiBooks, setQiBooks] = useState<any[]>([]);
  const [qiBooksTotal, setQiBooksTotal] = useState<number>(0);
  const [qiBooksPage, setQiBooksPage] = useState<number>(1);
  const [qiBookSearch, setQiBookSearch] = useState<string>("");
  const debouncedQiBookQ = useDebouncedValue(qiBookSearch, 250);

  const [qiArticles, setQiArticles] = useState<any[]>([]);
  const [qiArtTotal, setQiArtTotal] = useState<number>(0);
  const [qiArtPage, setQiArtPage] = useState<number>(1);
  const [qiArtSearch, setQiArtSearch] = useState<string>("");
  const debouncedQiArtQ = useDebouncedValue(qiArtSearch, 250);

  const [lockMode, setLockMode] = useState<"pwd" | "user">("pwd");
  const [lockPwd, setLockPwd] = useState<string>("");
  const [lockHint, setLockHint] = useState<string>("");
  const [lockBody, setLockBody] = useState<string>("");

  const switchMode = (next: EditorMode) => {
    if (next === "wysiwyg" && mode !== "wysiwyg" && /(?:^|\n):::(?:lock|book|bookshelf)\b/.test(value || "")) {
      void openConfirm({
        title: t("ed.wysiwyg_warn_title"),
        message: t("ed.wysiwyg_warn_msg"),
        confirmText: t("ui.confirm_ok"),
        danger: false,
      }).then((ok) => {
        if (ok) {
          setMode("wysiwyg");
          localStorage.setItem("volo_editor_mode", "wysiwyg");
        }
      });
      return;
    }
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
          const list = Array.from(files);
          void (async () => {
            for (const f of list) {
              await uploadMediaFile(f);
            }
          })();
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
        markdownLanguage,
        Prec.high(keymap.of(markdownKeymap)),
        pasteURLAsLink,
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
    const ctrl = new AbortController();
    const timer = window.setTimeout(async () => {
      try {
        const res = await apiFetch<{ html: string }>("/api/admin/articles/preview", {
          method: "POST",
          body: JSON.stringify({ content_md: value, locale: curLocale }),
          signal: ctrl.signal,
        });
        if (ctrl.signal.aborted) return;
        const html = res.html || "";
        setPreviewHTML(html);
        if (mode === "wysiwyg" && wysiwygRef.current && document.activeElement !== wysiwygRef.current) {
          wysiwygRef.current.innerHTML = html || `<p><br></p>`;
        }
      } catch {
        // Ignore aborted or transient preview errors while typing
      }
    }, 180);
    return () => {
      window.clearTimeout(timer);
      ctrl.abort();
    };
  }, [value, mode, curLocale]);

  useEffect(() => {
    if (!fullscreen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || e.defaultPrevented) return;
      if (
        document.querySelector(".volo-modal-backdrop") ||
        document.querySelector(".volo-cselect-menu.is-portal")
      ) {
        return;
      }
      setFullscreen(false);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [fullscreen]);

  useEffect(() => {
    if (!headingOpen) return;
    const updatePos = () => {
      if (!headingBtnRef.current) return;
      const rect = headingBtnRef.current.getBoundingClientRect();
      if (rect.bottom < 0 || rect.top > window.innerHeight || rect.right < 0 || rect.left > window.innerWidth) {
        setHeadingOpen(false);
        return;
      }
      setHeadingStyle(computePortalMenuStyle(rect, { minW: 112, maxW: 180, maxH: 260, align: "left" }).style);
    };
    updatePos();
    const onDocDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (headingBtnRef.current?.contains(target) || headingMenuRef.current?.contains(target)) return;
      setHeadingOpen(false);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setHeadingOpen(false);
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
    };
  }, [headingOpen]);

  useEffect(() => {
    if (!mediaMenuOpen) return;
    const updatePos = () => {
      if (!mediaBtnRef.current) return;
      const rect = mediaBtnRef.current.getBoundingClientRect();
      if (rect.bottom < 0 || rect.top > window.innerHeight || rect.right < 0 || rect.left > window.innerWidth) {
        setMediaMenuOpen(false);
        return;
      }
      setMediaStyle(computePortalMenuStyle(rect, { minW: 180, maxW: 260, maxH: 200, align: "left" }).style);
    };
    updatePos();
    const onDocDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (mediaBtnRef.current?.contains(target) || mediaMenuRef.current?.contains(target)) return;
      setMediaMenuOpen(false);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMediaMenuOpen(false);
    };
    document.addEventListener("mousedown", onDocDown);
    document.addEventListener("keydown", onEsc);
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    return () => {
      document.removeEventListener("mousedown", onDocDown);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
    };
  }, [mediaMenuOpen]);

  // Load Quick Insert data dynamically with server-side search & pagination
  useEffect(() => {
    if (!qiOpen || qiTab !== "media") return;
    const params = new URLSearchParams({
      refs: "0",
      page: String(qiMediaPage),
      per_page: "18",
    });
    if (debouncedQiMediaQ.trim()) params.set("q", debouncedQiMediaQ.trim());
    if (qiMediaMime) params.set("mime", qiMediaMime);
    apiFetch<{ items?: any[]; media?: any[]; total?: number }>(`/api/admin/media?${params.toString()}`)
      .then((r) => {
        const list = r.items || r.media || [];
        setQiMedia(list);
        setQiMediaTotal(typeof r.total === "number" ? r.total : list.length);
      })
      .catch(() => {});
  }, [qiOpen, qiTab, qiMediaPage, debouncedQiMediaQ, qiMediaMime]);

  useEffect(() => {
    if (!qiOpen || qiTab !== "books") return;
    const params = new URLSearchParams({
      page: String(qiBooksPage),
      per_page: "10",
    });
    if (debouncedQiBookQ.trim()) params.set("q", debouncedQiBookQ.trim());
    apiFetch<{ items?: any[]; books?: any[]; total?: number }>(`/api/admin/books?${params.toString()}`)
      .then((r) => {
        const list = r.items || r.books || [];
        setQiBooks(list);
        setQiBooksTotal(typeof r.total === "number" ? r.total : list.length);
      })
      .catch(() => {});
  }, [qiOpen, qiTab, qiBooksPage, debouncedQiBookQ]);

  useEffect(() => {
    if (!qiOpen || qiTab !== "cite") return;
    const params = new URLSearchParams({
      page: String(qiArtPage),
      per_page: "20",
    });
    if (debouncedQiArtQ.trim()) params.set("q", debouncedQiArtQ.trim());
    apiFetch<{ items?: any[]; articles?: any[]; total?: number }>(`/api/admin/articles?${params.toString()}`)
      .then((r) => {
        const list = (r.items || r.articles || []).map((it: any) => it.article || it);
        setQiArticles(list);
        setQiArtTotal(typeof r.total === "number" ? r.total : list.length);
      })
      .catch(() => {});
  }, [qiOpen, qiTab, qiArtPage, debouncedQiArtQ]);

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

          {/* Heading H1-H6 Popover (Portal-rendered to escape mobile overflow-x clipping) */}
          <div class="volo-pop-wrap">
            <button
              ref={headingBtnRef}
              type="button"
              class={`volo-ed-btn ${headingOpen ? "active" : ""}`}
              title={t("ed.heading")}
              onClick={() => {
                setMediaMenuOpen(false);
                setHeadingOpen((p) => !p);
              }}
            >
              <span style={{ fontWeight: 750, fontSize: "12.5px" }}>H▾</span>
            </button>
            {headingOpen &&
              typeof document !== "undefined" &&
              createPortal(
                <div
                  ref={headingMenuRef}
                  class="volo-cselect-menu is-portal volo-ed-pop"
                  style={headingStyle}
                >
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
                </div>,
                document.body
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

          {/* Upload Media / Insert from Media Library (Portal-rendered to escape mobile overflow-x clipping) */}
          <div class="volo-pop-wrap">
            <button
              ref={mediaBtnRef}
              type="button"
              class={`volo-ed-btn ${mediaMenuOpen ? "active" : ""}`}
              title={uploading ? t("ed.uploading") : t("ed.upload_media")}
              onClick={() => {
                setHeadingOpen(false);
                setMediaMenuOpen((v) => !v);
              }}
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
              multiple
              accept="image/*,audio/*"
              style={{ display: "none" }}
              onChange={(e) => {
                const files = Array.from((e.currentTarget as HTMLInputElement).files || []);
                (e.currentTarget as HTMLInputElement).value = "";
                void (async () => {
                  for (const f of files) {
                    await uploadMediaFile(f);
                  }
                })();
              }}
            />
            {mediaMenuOpen &&
              typeof document !== "undefined" &&
              createPortal(
                <div
                  ref={mediaMenuRef}
                  class="volo-cselect-menu is-portal volo-ed-pop volo-ed-media-pop"
                  style={mediaStyle}
                >
                  <button
                    type="button"
                    class="volo-cselect-item"
                    onClick={() => {
                      setMediaMenuOpen(false);
                      fileInputRef.current?.click();
                    }}
                  >
                    <span class="volo-cselect-item-text">{t("ed.upload_local_file")}</span>
                  </button>
                  <button
                    type="button"
                    class="volo-cselect-item"
                    onClick={() => {
                      setMediaMenuOpen(false);
                      setMediaModalOpen(true);
                    }}
                  >
                    <span class="volo-cselect-item-text">{t("ed.insert_from_library")}</span>
                  </button>
                </div>,
                document.body
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

          {fullscreen && onSave && (
            <button
              type="button"
              class="volo-btn volo-btn-primary volo-btn-sm"
              onClick={() => onSave()}
            >
              {t("common.save")}
            </button>
          )}

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
          <div>
            <div class="volo-filter-bar" style={{ marginBottom: "10px", gap: "6px", flexWrap: "wrap" }}>
              {(
                [
                  ["", t("cnt.media_type_all")],
                  ["image", t("cnt.media_type_image")],
                  ["audio", t("cnt.media_type_audio")],
                ] as const
              ).map(([mVal, mLabel]) => (
                <button
                  key={mVal || "all"}
                  type="button"
                  class={`volo-filter-pill ${qiMediaMime === mVal ? "active" : ""}`}
                  onClick={() => {
                    setQiMediaMime(mVal);
                    setQiMediaPage(1);
                  }}
                >
                  {mLabel}
                </button>
              ))}
              <input
                class="volo-input"
                style={{ flex: "1 1 180px", minWidth: "140px", height: "32px" }}
                placeholder={t("cnt.media_search_ph")}
                value={qiMediaSearch}
                onInput={(e) => {
                  setQiMediaSearch((e.currentTarget as HTMLInputElement).value);
                  setQiMediaPage(1);
                }}
              />
            </div>
            {(qiMedia.length > 18 ? paginateItems(qiMedia, qiMediaPage, 18).slice : qiMedia).length === 0 ? (
              <ConsoleEmpty text={t("tbl.empty")} />
            ) : (
              <div class="volo-qi-grid">
                {(qiMedia.length > 18 ? paginateItems(qiMedia, qiMediaPage, 18).slice : qiMedia).map((m) => {
                  const name = m.orig_filename || m.original_name || m.id;
                  const isImg = String(m.mime_type || "").startsWith("image/");
                  const isAudio = String(m.mime_type || "").startsWith("audio/");
                  return (
                    <button
                      key={m.id}
                      type="button"
                      class="volo-qi-media-card"
                      onClick={() => {
                        if (isImg) {
                          insertSnippet(`![${name}](${m.public_url})`);
                        } else if (isAudio) {
                          insertSnippet(`[${t("ed.audio_label")}: ${name}](${m.public_url})`);
                        } else {
                          insertSnippet(`[${name}](${m.public_url})`);
                        }
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
                })}
              </div>
            )}
            <PaginationBar
              total={qiMediaTotal}
              page={qiMediaPage}
              pageSize={18}
              onChange={(p) => setQiMediaPage(p)}
            />
          </div>
        )}

        {qiTab === "books" && (
          <div>
            <div style={{ marginBottom: "10px", display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap" }}>
              <input
                class="volo-input"
                style={{ flex: "1 1 200px", minWidth: "150px", height: "34px" }}
                placeholder={t("ed.qi_search_books")}
                value={qiBookSearch}
                onInput={(e) => {
                  setQiBookSearch((e.currentTarget as HTMLInputElement).value);
                  setQiBooksPage(1);
                }}
              />
              <button
                type="button"
                class="volo-btn volo-btn-sm volo-btn-primary"
                style={{ flexShrink: 0 }}
                onClick={() => {
                  insertSnippet("\n:::bookshelf\n:::\n");
                  setQiOpen(false);
                }}
              >
                + {t("ed.bookshelf")}
              </button>
            </div>
            {(() => {
              const q = qiBookSearch.trim().toLowerCase();
              const filtered =
                qiBooks.length > 10 && q
                  ? qiBooks.filter(
                      (b) =>
                        String(b.title || "").toLowerCase().includes(q) ||
                        String(b.author || "").toLowerCase().includes(q) ||
                        String(b.slug || "").toLowerCase().includes(q)
                    )
                  : qiBooks;
              const rows = filtered.length > 10 ? paginateItems(filtered, qiBooksPage, 10).slice : filtered;
              if (rows.length === 0) {
                return <ConsoleEmpty text={t("tbl.empty")} />;
              }
              return (
                <div style={{ display: "flex", flexDirection: "column", gap: "8px", maxHeight: "260px", overflowY: "auto" }}>
                  {rows.map((b) => (
                    <div
                      key={b.id}
                      class="volo-subcard"
                      style={{
                        padding: "10px 12px",
                        margin: 0,
                        display: "flex",
                        justifyContent: "space-between",
                        alignItems: "center",
                        gap: "10px",
                      }}
                    >
                      <div style={{ minWidth: 0, flex: 1 }}>
                        <div style={{ fontWeight: 650, fontSize: "13px", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                          {b.title}
                        </div>
                        <div class="volo-help" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                          {b.description || `/books/${b.slug}`}
                        </div>
                      </div>
                      <button
                        type="button"
                        class="volo-btn volo-btn-sm"
                        style={{ flexShrink: 0 }}
                        onClick={() => {
                          const body = b.description || `[${b.title}](/books/${b.slug})`;
                          insertSnippet(`\n:::book title="${b.title}" status="${b.reading_status || "reading"}"\n${body}\n:::\n`);
                          setQiOpen(false);
                        }}
                      >
                        {t("ed.qi_insert_btn")}
                      </button>
                    </div>
                  ))}
                </div>
              );
            })()}
            <PaginationBar
              total={qiBooksTotal}
              page={qiBooksPage}
              pageSize={10}
              onChange={(p) => setQiBooksPage(p)}
            />
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
            <div class="volo-field" style={{ marginBottom: "10px" }}>
              <label>{t("ed.qi_cite")}</label>
              <input
                class="volo-input"
                placeholder={t("ed.qi_search_art")}
                value={qiArtSearch}
                onInput={(e) => {
                  setQiArtSearch((e.currentTarget as HTMLInputElement).value);
                  setQiArtPage(1);
                }}
              />
            </div>
            {(() => {
              const q = qiArtSearch.trim().toLowerCase();
              const filtered =
                qiArticles.length > 20 && q
                  ? qiArticles.filter(
                      (a) =>
                        String(a.title || "").toLowerCase().includes(q) ||
                        String(a.slug || "").toLowerCase().includes(q)
                    )
                  : qiArticles;
              const rows = filtered.length > 20 ? paginateItems(filtered, qiArtPage, 20).slice : filtered;
              if (rows.length === 0) {
                return <ConsoleEmpty text={t("tbl.empty")} />;
              }
              return (
                <div style={{ display: "flex", flexDirection: "column", gap: "6px", maxHeight: "240px", overflowY: "auto" }}>
                  {rows.map((a) => {
                    const st = String(a.status || "published");
                    const stClass = st === "published" ? "published" : st === "scheduled" ? "scheduled" : "draft";
                    return (
                      <div
                        key={a.id}
                        class="volo-subcard"
                        style={{
                          padding: "8px 12px",
                          margin: 0,
                          display: "flex",
                          justifyContent: "space-between",
                          alignItems: "center",
                          gap: "10px",
                        }}
                      >
                        <div style={{ minWidth: 0, flex: 1 }}>
                          <div style={{ display: "flex", alignItems: "center", gap: "6px", minWidth: 0 }}>
                            <span style={{ fontSize: "13px", fontWeight: 550, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                              {a.title || a.slug}
                            </span>
                            <span class={`volo-status-pill ${stClass}`} style={{ flexShrink: 0 }}>
                              {t(`art.${st}`)}
                            </span>
                          </div>
                          <div class="volo-help" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                            /posts/{a.slug}
                          </div>
                        </div>
                        <button
                          type="button"
                          class="volo-btn volo-btn-sm"
                          style={{ flexShrink: 0 }}
                          onClick={() => {
                            insertSnippet(`[${a.title || a.slug}](/posts/${a.slug})`);
                            setQiOpen(false);
                          }}
                        >
                          {t("ed.qi_insert_btn")}
                        </button>
                      </div>
                    );
                  })}
                </div>
              );
            })()}
            <PaginationBar
              total={qiArtTotal}
              page={qiArtPage}
              pageSize={20}
              onChange={(p) => setQiArtPage(p)}
            />
          </div>
        )}
      </AnimatedModal>
    </div>
  );
}

function buildCssSyntaxPlugin() {
  const buildDecos = (view: EditorView): DecorationSet => {
    const builder = new RangeSetBuilder<Decoration>();
    const doc = view.state.doc;
    for (const { from, to } of view.visibleRanges) {
      const slice = doc.sliceString(from, to);
      interface Tok {
        s: number;
        e: number;
        cls: string;
      }
      const toks: Tok[] = [];
      const used = new Uint8Array(slice.length);
      const claim = (s: number, e: number, cls: string) => {
        if (s >= e) return;
        for (let i = s; i < e; i++) if (used[i]) return;
        for (let i = s; i < e; i++) used[i] = 1;
        toks.push({ s: from + s, e: from + e, cls });
      };

      let m: RegExpExecArray | null;
      const cmtRe = /\/\*[\s\S]*?\*\//g;
      while ((m = cmtRe.exec(slice)) !== null) claim(m.index, m.index + m[0].length, "c");

      const strRe = /"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'/g;
      while ((m = strRe.exec(slice)) !== null) claim(m.index, m.index + m[0].length, "s");

      const atRe = /@[a-zA-Z-]+/g;
      while ((m = atRe.exec(slice)) !== null) claim(m.index, m.index + m[0].length, "k");

      const varRe = /--[a-zA-Z0-9_-]+/g;
      while ((m = varRe.exec(slice)) !== null) claim(m.index, m.index + m[0].length, "na");

      const propRe = /([a-zA-Z-]+)(\s*:)/g;
      while ((m = propRe.exec(slice)) !== null) claim(m.index, m.index + m[1].length, "kt");

      const numRe = /#[0-9a-fA-F]{3,8}\b|\b\d+(?:\.\d+)?(?:px|rem|em|vh|vw|dvh|%|s|ms|deg)?\b/g;
      while ((m = numRe.exec(slice)) !== null) claim(m.index, m.index + m[0].length, "m");

      toks.sort((a, b) => a.s - b.s || a.e - b.e);
      for (const tk of toks) {
        builder.add(tk.s, tk.e, Decoration.mark({ class: tk.cls }));
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
      update(u: ViewUpdate) {
        if (u.docChanged || u.viewportChanged) {
          this.decorations = buildDecos(u.view);
        }
      }
    },
    { decorations: (v) => v.decorations }
  );
}

export function CM6CssEditorCore({ value, onChange, placeholder: phText, onSave }: CM6CssEditorProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onSaveRef = useRef(onSave);
  onSaveRef.current = onSave;

  useEffect(() => {
    if (!hostRef.current) return;
    const state = EditorState.create({
      doc: value || "",
      extensions: [
        lineNumbers(),
        history(),
        EditorView.lineWrapping,
        buildCssSyntaxPlugin(),
        placeholder(phText || ""),
        keymap.of([
          {
            key: "Mod-s",
            run: () => {
              onSaveRef.current?.();
              return true;
            },
          },
          indentWithTab,
          ...defaultKeymap,
          ...historyKeymap,
        ]),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) {
            onChangeRef.current(u.state.doc.toString());
          }
        }),
      ],
    });
    const view = new EditorView({ state, parent: hostRef.current });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, []);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const cur = view.state.doc.toString();
    if (cur !== value) {
      view.dispatch({ changes: { from: 0, to: cur.length, insert: value || "" } });
    }
  }, [value]);

  return <div ref={hostRef} class="volo-cm-host volo-css-cm-host chroma" />;
}

