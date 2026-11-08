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
  WidgetType,
} from "@codemirror/view";
import {
  addCursorAbove,
  addCursorBelow,
  copyLineDown,
  copyLineUp,
  cursorCharLeft,
  cursorCharRight,
  cursorDocEnd,
  cursorDocStart,
  cursorGroupLeft,
  cursorGroupRight,
  cursorLineBoundaryBackward,
  cursorLineBoundaryForward,
  cursorLineBoundaryLeft,
  cursorLineBoundaryRight,
  cursorLineDown,
  cursorLineUp,
  cursorPageDown,
  cursorPageUp,
  deleteCharBackward,
  deleteCharForward,
  deleteGroupBackward,
  deleteGroupForward,
  deleteLine,
  deleteLineBoundaryBackward,
  deleteLineBoundaryForward,
  emacsStyleKeymap,
  history,
  historyKeymap,
  insertBlankLine,
  insertNewlineKeepIndent,
  insertTab,
  moveLineDown,
  moveLineUp,
  redo,
  selectAll,
  selectCharLeft,
  selectCharRight,
  selectDocEnd,
  selectDocStart,
  selectGroupLeft,
  selectGroupRight,
  selectLine,
  selectLineBoundaryBackward,
  selectLineBoundaryForward,
  selectLineBoundaryLeft,
  selectLineBoundaryRight,
  selectLineDown,
  selectLineUp,
  selectPageDown,
  selectPageUp,
  simplifySelection,
  toggleTabFocusMode,
  undo,
} from "@codemirror/commands";
import type { KeyBinding } from "@codemirror/view";
import { apiFetch, openConfirm, showToast } from "../api";
import { localeSig, t } from "../i18n";
import {
  AnimatedModal,
  computePortalMenuStyle,
  ConsoleEmpty,
  ConsoleSelect,
  notifyClosePopovers,
  PaginationBar,
  paginateItems,
  PlusIcon,
  POPOVER_CLOSE_EVENT,
  useDebouncedValue,
} from "./UIControls";
import { MediaLibraryModal } from "./MediaLibraryModal";
import type { CM6CssEditorProps, CM6EditorProps } from "./CM6Editor";

type EditorMode = "ir" | "wysiwyg" | "sv";

function dedentTwoSpaces(view: EditorView): boolean {
  const { state } = view;
  if (state.readOnly) return false;
  const changes: Array<{ from: number; to: number; insert: string }> = [];
  const seen = new Set<number>();
  for (const r of state.selection.ranges) {
    const startLine = state.doc.lineAt(r.from).number;
    const endLine = state.doc.lineAt(r.to).number;
    for (let ln = startLine; ln <= endLine; ln++) {
      if (seen.has(ln)) continue;
      seen.add(ln);
      const line = state.doc.line(ln);
      if (line.text.startsWith("\t")) {
        changes.push({ from: line.from, to: line.from + 1, insert: "" });
      } else if (line.text.startsWith("  ")) {
        changes.push({ from: line.from, to: line.from + 2, insert: "" });
      } else if (line.text.startsWith(" ")) {
        changes.push({ from: line.from, to: line.from + 1, insert: "" });
      }
    }
  }
  if (changes.length > 0) {
    view.dispatch({ changes, userEvent: "input.dedent" });
  }
  return true;
}

const voloIndentWithTab: KeyBinding = {
  key: "Tab",
  run: insertTab,
  shift: dedentTwoSpaces,
};

const voloBaseKeymap: KeyBinding[] = [
  { key: "ArrowLeft", run: cursorCharLeft, shift: selectCharLeft, preventDefault: true },
  { key: "Mod-ArrowLeft", mac: "Alt-ArrowLeft", run: cursorGroupLeft, shift: selectGroupLeft, preventDefault: true },
  { mac: "Cmd-ArrowLeft", run: cursorLineBoundaryLeft, shift: selectLineBoundaryLeft, preventDefault: true },
  { key: "ArrowRight", run: cursorCharRight, shift: selectCharRight, preventDefault: true },
  { key: "Mod-ArrowRight", mac: "Alt-ArrowRight", run: cursorGroupRight, shift: selectGroupRight, preventDefault: true },
  { mac: "Cmd-ArrowRight", run: cursorLineBoundaryRight, shift: selectLineBoundaryRight, preventDefault: true },
  { key: "ArrowUp", run: cursorLineUp, shift: selectLineUp, preventDefault: true },
  { mac: "Cmd-ArrowUp", run: cursorDocStart, shift: selectDocStart },
  { mac: "Ctrl-ArrowUp", run: cursorPageUp, shift: selectPageUp },
  { key: "ArrowDown", run: cursorLineDown, shift: selectLineDown, preventDefault: true },
  { mac: "Cmd-ArrowDown", run: cursorDocEnd, shift: selectDocEnd },
  { mac: "Ctrl-ArrowDown", run: cursorPageDown, shift: selectPageDown },
  { key: "PageUp", run: cursorPageUp, shift: selectPageUp },
  { key: "PageDown", run: cursorPageDown, shift: selectPageDown },
  { key: "Home", run: cursorLineBoundaryBackward, shift: selectLineBoundaryBackward, preventDefault: true },
  { key: "Mod-Home", run: cursorDocStart, shift: selectDocStart },
  { key: "End", run: cursorLineBoundaryForward, shift: selectLineBoundaryForward, preventDefault: true },
  { key: "Mod-End", run: cursorDocEnd, shift: selectDocEnd },
  { key: "Enter", run: insertNewlineKeepIndent, shift: insertNewlineKeepIndent },
  { key: "Mod-a", run: selectAll },
  { key: "Backspace", run: deleteCharBackward, shift: deleteCharBackward, preventDefault: true },
  { key: "Delete", run: deleteCharForward, preventDefault: true },
  { key: "Mod-Backspace", mac: "Alt-Backspace", run: deleteGroupBackward, preventDefault: true },
  { key: "Mod-Delete", mac: "Alt-Delete", run: deleteGroupForward, preventDefault: true },
  { mac: "Mod-Backspace", run: deleteLineBoundaryBackward, preventDefault: true },
  { mac: "Mod-Delete", run: deleteLineBoundaryForward, preventDefault: true },
  { key: "Alt-ArrowUp", run: moveLineUp },
  { key: "Shift-Alt-ArrowUp", run: copyLineUp },
  { key: "Alt-ArrowDown", run: moveLineDown },
  { key: "Shift-Alt-ArrowDown", run: copyLineDown },
  { key: "Mod-Alt-ArrowUp", run: addCursorAbove },
  { key: "Mod-Alt-ArrowDown", run: addCursorBelow },
  { key: "Escape", run: simplifySelection },
  { key: "Mod-Enter", run: insertBlankLine },
  { key: "Alt-l", mac: "Ctrl-l", run: selectLine },
  { key: "Shift-Mod-k", run: deleteLine },
  { key: "Ctrl-m", mac: "Shift-Alt-m", run: toggleTabFocusMode },
  ...emacsStyleKeymap.map((b) => ({ mac: b.key, run: b.run, shift: b.shift })),
];

function isInsideFencedCode(state: EditorState, lineNumber: number): boolean {
  let fenceChar = "";
  let fenceLen = 0;
  for (let n = 1; n < lineNumber; n++) {
    const m = /^\s*(`{3,}|~{3,})/.exec(state.doc.line(n).text);
    if (!m) continue;
    const ch = m[1][0];
    const len = m[1].length;
    if (!fenceChar) {
      fenceChar = ch;
      fenceLen = len;
    } else if (ch === fenceChar && len >= fenceLen) {
      fenceChar = "";
      fenceLen = 0;
    }
  }
  return fenceChar !== "";
}

const mdLinePrefixRe =
  /^(\s*(?:>\s*)*)(?:([-*+])\s+(\[[ xX]\]\s+)?|(\d+)([.)])\s+|)?(.*)$/;

const markdownEditingKeymap = [
  {
    key: "Enter",
    run(view: EditorView): boolean {
      const { state } = view;
      if (state.readOnly || state.selection.ranges.length !== 1 || !state.selection.main.empty) {
        return false;
      }
      const pos = state.selection.main.head;
      const line = state.doc.lineAt(pos);
      if (isInsideFencedCode(state, line.number)) return false;

      const beforeCursor = line.text.slice(0, pos - line.from);
      const afterCursor = line.text.slice(pos - line.from);
      const m = mdLinePrefixRe.exec(beforeCursor);
      if (!m) return false;
      const [, quotePrefix, bullet, taskBox, ordNum, ordDelim, restBefore] = m;
      const hasMarker = Boolean(quotePrefix.trim() || bullet || ordNum);
      if (!hasMarker) return false;

      // Empty list item / empty blockquote line -> exit block cleanly
      if (!restBefore.trim() && !afterCursor.trim()) {
        view.dispatch({
          changes: { from: line.from, to: line.to, insert: "" },
          selection: { anchor: line.from },
          userEvent: "input",
        });
        return true;
      }

      let nextPrefix = quotePrefix;
      if (bullet) {
        nextPrefix += `${bullet} ${taskBox ? "[ ] " : ""}`;
      } else if (ordNum) {
        nextPrefix += `${Number(ordNum) + 1}${ordDelim} `;
      }
      view.dispatch({
        changes: { from: pos, to: pos, insert: `\n${nextPrefix}` },
        selection: { anchor: pos + 1 + nextPrefix.length },
        scrollIntoView: true,
        userEvent: "input",
      });
      return true;
    },
  },
  {
    key: "Backspace",
    run(view: EditorView): boolean {
      const { state } = view;
      if (state.readOnly || state.selection.ranges.length !== 1 || !state.selection.main.empty) {
        return false;
      }
      const pos = state.selection.main.head;
      const line = state.doc.lineAt(pos);
      if (pos === line.from || isInsideFencedCode(state, line.number)) return false;
      const before = line.text.slice(0, pos - line.from);
      if (/^\s*(?:>\s*)+(?:[-*+]\s+(?:\[[ xX]\]\s+)?|\d+[.)]\s+)?$/.test(before) ||
          /^\s*(?:[-*+]\s+(?:\[[ xX]\]\s+)?|\d+[.)]\s+)$/.test(before)) {
        const indent = /^\s*/.exec(before)?.[0] || "";
        view.dispatch({
          changes: { from: line.from, to: pos, insert: indent },
          selection: { anchor: line.from + indent.length },
          userEvent: "delete.backward",
        });
        return true;
      }
      return false;
    },
  },
];

const pasteURLAsLinkExt = EditorView.domEventHandlers({
  paste(event, view) {
    const { state } = view;
    if (state.readOnly) return false;
    const text = event.clipboardData?.getData("text/plain")?.trim() || "";
    if (!/^https?:\/\/\S+$/i.test(text)) return false;
    const sel = state.selection.main;
    if (sel.empty) return false;
    const line = state.doc.lineAt(sel.from);
    if (sel.to > line.to || isInsideFencedCode(state, line.number)) return false;
    const selected = state.sliceDoc(sel.from, sel.to);
    if (!selected || /^https?:\/\//i.test(selected) || /[\[\]`]/.test(selected)) return false;
    event.preventDefault();
    const rep = `[${selected}](${text})`;
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert: rep },
      selection: { anchor: sel.from + rep.length },
      userEvent: "input.paste",
    });
    return true;
  },
});

const SAFE_IR_COLOR_RE = /^(?:#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20}|rgb\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}\s*\))$/;

function parseMarkdownImageTarget(rawTarget: string): string {
  const s = rawTarget.trim();
  if (s.startsWith("<") && s.includes(">")) {
    return s.slice(1, s.indexOf(">")).trim();
  }
  const m = /^(\S+)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*$/.exec(s);
  return m ? m[1] : s;
}

function resolveIRImageSrc(rawSrc: string): string {
  const s = rawSrc.trim();
  if (!s || /^(?:javascript|vbscript|file):/i.test(s) || s.startsWith("//") || s.startsWith("/\\")) return "";
  if (/^(?:https?:\/\/|data:image\/|blob:|\/)/i.test(s)) return s;
  if (s.startsWith("./")) return "/" + s.slice(2).replace(/^\/+/, "");
  if (/^(?:image|images|uploads)\//i.test(s)) return "/" + s;
  return s;
}

function buildInstantRenderPlugin() {
  const hideDeco = Decoration.replace({});
  const buildDecos = (view: EditorView): DecorationSet => {
    const builder = new RangeSetBuilder<Decoration>();
    const state = view.state;
    if (view.visibleRanges.length === 0) return builder.finish();

    const activeLine = state.doc.lineAt(state.selection.main.head).number;
    let inFence = false;
    let scannedLine = 0;

    const advanceFenceTo = (targetExclusiveLine: number) => {
      for (let ln = scannedLine + 1; ln < targetExclusiveLine; ln++) {
        if (/^\s*```/.test(state.doc.line(ln).text)) {
          inFence = !inFence;
        }
      }
      if (targetExclusiveLine - 1 > scannedLine) {
        scannedLine = targetExclusiveLine - 1;
      }
    };

    for (const { from, to } of view.visibleRanges) {
      let pos = from;
      while (pos <= to) {
        const line = state.doc.lineAt(pos);
        if (line.number <= scannedLine) {
          pos = line.to + 1;
          continue;
        }
        advanceFenceTo(line.number);
        scannedLine = line.number;

        const text = line.text;
        const isCursorLine = line.number === activeLine;

        const fenceMatch = /^(\s*```)([\w+-]*)(.*)$/.exec(text);
        if (fenceMatch) {
          const opening = !inFence;
          builder.add(
            line.from,
            line.from,
            Decoration.line({ class: opening ? "cm-ir-fence cm-ir-fence-open" : "cm-ir-fence cm-ir-fence-close" })
          );
          if (!isCursorLine && text.length > 0) {
            const ticksLen = fenceMatch[1].length;
            const langLen = fenceMatch[2].length;
            if (opening && langLen > 0) {
              builder.add(line.from, line.from + ticksLen, hideDeco);
              builder.add(
                line.from + ticksLen,
                line.from + ticksLen + langLen,
                Decoration.mark({ class: "cm-ir-fence-lang" })
              );
            } else {
              builder.add(line.from, line.to, Decoration.mark({ class: "cm-ir-fence-muted" }));
            }
          }
          inFence = !inFence;
          pos = line.to + 1;
          continue;
        }

        if (inFence) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-codeblock-line chroma" }));
          if (text.length > 0 && text.length < 1200) {
            interface CodeTok {
              s: number;
              e: number;
              cls: string;
            }
            const toks: CodeTok[] = [];
            const used = new Uint8Array(text.length);
            const claim = (s: number, e: number, cls: string) => {
              if (s >= e) return;
              for (let i = s; i < e; i++) if (used[i]) return;
              for (let i = s; i < e; i++) used[i] = 1;
              toks.push({ s, e, cls });
            };
            let cm: RegExpExecArray | null;
            const cmtRe = /(?:^|\s)(#.*$|\/\/.*$)/g;
            while ((cm = cmtRe.exec(text)) !== null) {
              const idx = cm.index + (cm[0].length - cm[1].length);
              claim(idx, idx + cm[1].length, "c1");
            }
            const strRe = /"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/g;
            while ((cm = strRe.exec(text)) !== null) {
              claim(cm.index, cm.index + cm[0].length, "s2");
            }
            const flagRe = /(?:^|\s)(--?[a-zA-Z][\w-]*)/g;
            while ((cm = flagRe.exec(text)) !== null) {
              const idx = cm.index + (cm[0].length - cm[1].length);
              claim(idx, idx + cm[1].length, "na");
            }
            const kwRe = /\b(?:sudo|apt|apt-get|yum|dnf|apk|cd|mkdir|tar|wget|curl|make|export|return|func|const|let|var|if|else|for|import|from|package|type|struct)\b/g;
            while ((cm = kwRe.exec(text)) !== null) {
              claim(cm.index, cm.index + cm[0].length, "k");
            }
            const numRe = /\b\d+(?:\.\d+)?\b/g;
            while ((cm = numRe.exec(text)) !== null) {
              claim(cm.index, cm.index + cm[0].length, "mi");
            }
            toks.sort((a, b) => a.s - b.s);
            for (const tk of toks) {
              builder.add(line.from + tk.s, line.from + tk.e, Decoration.mark({ class: tk.cls }));
            }
          }
          pos = line.to + 1;
          continue;
        }

        interface SpanOp {
          from: number;
          to: number;
          deco: Decoration;
        }
        const ops: SpanOp[] = [];
        const occupied = new Uint8Array(Math.min(text.length, 2400));
        const markFree = (s: number, e: number) => {
          if (s < 0 || e > occupied.length || s >= e) return false;
          for (let i = s; i < e; i++) if (occupied[i]) return false;
          for (let i = s; i < e; i++) occupied[i] = 1;
          return true;
        };

        // Line-level block decorations (must be emitted at line.from BEFORE inline ops)
        const hm = /^(#{1,6})\s+/.exec(text);
        const qm = /^(\s*>\s?)(?:\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\]\s*)?/i.exec(text);
        const dm = /^\s*:::(?:bookshelf|book|lock)?.*$/.exec(text);
        const hrm = /^\s*(?:---|\*\*\*|___|<hr\s*\/?>)\s*$/i.exec(text);
        const soloBr = /^\s*<br\s*\/?>\s*$/i.exec(text);
        const centerLine = /^(\s*<center>)(.*)(<\/center>\s*)$/i.exec(text);

        if (hm) {
          const lvl = hm[1].length;
          builder.add(line.from, line.from, Decoration.line({ class: `cm-ir-h cm-ir-h${lvl}` }));
          if (!isCursorLine && markFree(0, hm[0].length)) {
            ops.push({ from: line.from, to: line.from + hm[0].length, deco: hideDeco });
          }
        } else if (qm) {
          const alertKind = qm[2] ? qm[2].toLowerCase() : "";
          builder.add(
            line.from,
            line.from,
            Decoration.line({ class: alertKind ? `cm-ir-quote cm-ir-callout cm-ir-callout-${alertKind}` : "cm-ir-quote" })
          );
          if (!isCursorLine && markFree(0, qm[1].length)) {
            ops.push({ from: line.from, to: line.from + qm[1].length, deco: hideDeco });
            if (qm[2] && markFree(qm[1].length, qm[0].length)) {
              ops.push({
                from: line.from + qm[1].length,
                to: line.from + qm[0].length,
                deco: Decoration.mark({ class: "cm-ir-callout-tag" }),
              });
            }
          }
        } else if (dm) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-directive" }));
        } else if (hrm) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-hr" }));
          if (!isCursorLine && markFree(0, text.length)) {
            ops.push({ from: line.from, to: line.to, deco: hideDeco });
          }
        } else if (soloBr) {
          if (!isCursorLine && markFree(0, text.length)) {
            ops.push({ from: line.from, to: line.to, deco: hideDeco });
          }
        } else if (centerLine) {
          builder.add(line.from, line.from, Decoration.line({ class: "cm-ir-center" }));
          if (!isCursorLine) {
            const openLen = centerLine[1].length;
            const closeStart = text.length - centerLine[3].length;
            if (markFree(0, openLen)) {
              ops.push({ from: line.from, to: line.from + openLen, deco: hideDeco });
            }
            if (markFree(closeStart, text.length)) {
              ops.push({ from: line.from + closeStart, to: line.to, deco: hideDeco });
            }
          }
        }

        // Inline decorations on non-cursor lines (sorted strictly by position)
        if (!isCursorLine && text.length > 0 && text.length <= 2400) {
          // Task checkbox (- [ ] / - [x])
          const tm = /^(\s*[-*+]\s+)(\[[ xX]\])(\s+)/.exec(text);
          if (tm) {
            const boxStart = tm[1].length;
            const boxEnd = boxStart + tm[2].length;
            if (markFree(boxStart, boxEnd)) {
              const checked = /x/i.test(tm[2]);
              ops.push({
                from: line.from + boxStart,
                to: line.from + boxEnd,
                deco: Decoration.mark({ class: checked ? "cm-ir-task is-checked" : "cm-ir-task" }),
              });
            }
          }

          // Inline code `...` (claim first so raw tags/flags inside backticks stay literal)
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

          // Inline <br> / <br/> / <br /> inside prose lines
          const brRe = /<br\s*\/?>/gi;
          while ((m = brRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              const trailingOnly = /^\s*$/.test(text.slice(e));
              ops.push({
                from: line.from + s,
                to: line.from + e,
                deco: trailingOnly ? hideDeco : Decoration.replace({ widget: new BrIRWidget() }),
              });
            }
          }

          // Legacy & toolbar <font color="...">...</font> (supports unquoted #hex and multi-spaces)
          const fontRe = /(<font\s+[^>]*?color\s*=\s*(?:"([^"]+)"|'([^']+)'|([^\s"'>]+))[^>]*>)([^<\n]+)(<\/font>)/gi;
          while ((m = fontRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const rawColor = (m[2] || m[3] || m[4] || "").trim();
            if (SAFE_IR_COLOR_RE.test(rawColor) && markFree(s, e)) {
              const openEnd = s + m[1].length;
              const closeStart = e - m[6].length;
              ops.push({ from: line.from + s, to: line.from + openEnd, deco: hideDeco });
              ops.push({
                from: line.from + openEnd,
                to: line.from + closeStart,
                deco: Decoration.mark({ attributes: { style: `color: ${rawColor};` } }),
              });
              ops.push({ from: line.from + closeStart, to: line.from + e, deco: hideDeco });
            }
          }

          // Inline <span style="color: ...">...</span>
          const spanRe = /(<span\s+style\s*=\s*(?:"([^"]+)"|'([^']+)')\s*>)([^<\n]+)(<\/span>)/gi;
          while ((m = spanRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const rawStyle = (m[2] || m[3] || "").trim();
            const colorMatch = /^\s*color\s*:\s*([^;]+)\s*;?\s*$/i.exec(rawStyle);
            const safeColor = colorMatch ? colorMatch[1].trim() : "";
            if (safeColor && SAFE_IR_COLOR_RE.test(safeColor) && markFree(s, e)) {
              const openEnd = s + m[1].length;
              const closeStart = e - m[5].length;
              ops.push({ from: line.from + s, to: line.from + openEnd, deco: hideDeco });
              ops.push({
                from: line.from + openEnd,
                to: line.from + closeStart,
                deco: Decoration.mark({ attributes: { style: `color: ${safeColor};` } }),
              });
              ops.push({ from: line.from + closeStart, to: line.from + e, deco: hideDeco });
            }
          }

          // Highlight <mark>...</mark>
          const markRe = /(<mark>)([^<\n]+)(<\/mark>)/gi;
          while ((m = markRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 6, deco: hideDeco });
              ops.push({
                from: line.from + s + 6,
                to: line.from + e - 7,
                deco: Decoration.mark({ class: "cm-ir-mark" }),
              });
              ops.push({ from: line.from + e - 7, to: line.from + e, deco: hideDeco });
            }
          }

          // Underline <u>...</u>
          const uRe = /(<u>)([^<\n]+)(<\/u>)/gi;
          while ((m = uRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({ from: line.from + s, to: line.from + s + 3, deco: hideDeco });
              ops.push({
                from: line.from + s + 3,
                to: line.from + e - 4,
                deco: Decoration.mark({ class: "cm-ir-underline" }),
              });
              ops.push({ from: line.from + e - 4, to: line.from + e, deco: hideDeco });
            }
          }

          // HTML inline formatting tags (<strong>/<b>, <em>/<i>, <s>/<del>)
          const htmlFmtRe = /(<(strong|b|em|i|s|del)>)([^<\n]+)(<\/\2>)/gi;
          while ((m = htmlFmtRe.exec(text)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              const tagLow = m[2].toLowerCase();
              const cls =
                tagLow === "strong" || tagLow === "b"
                  ? "cm-ir-bold"
                  : tagLow === "em" || tagLow === "i"
                  ? "cm-ir-italic"
                  : "cm-ir-strike";
              const openEnd = s + m[1].length;
              const closeStart = e - m[4].length;
              ops.push({ from: line.from + s, to: line.from + openEnd, deco: hideDeco });
              ops.push({
                from: line.from + openEnd,
                to: line.from + closeStart,
                deco: Decoration.mark({ class: cls }),
              });
              ops.push({ from: line.from + closeStart, to: line.from + e, deco: hideDeco });
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

          // Markdown Images ![alt](url) on non-cursor lines (claim BEFORE linkRe)
          const mdImgRe = /!\[([^\]\n]*)\]\(([^)\n]+)\)/g;
          while ((m = mdImgRe.exec(text)) !== null) {
            const alt = (m[1] || "").trim();
            const rawTarget = parseMarkdownImageTarget(m[2] || "");
            const resolvedSrc = resolveIRImageSrc(rawTarget);
            if (!resolvedSrc) continue;
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({
                from: line.from + s,
                to: line.from + e,
                deco: Decoration.replace({ widget: new ImageIRWidget(resolvedSrc, rawTarget, alt, line.from + s) }),
              });
            }
          }

          // Inline HTML <img ... src="..."> on non-cursor lines
          const htmlImgRe = /<img\b[^>]*\bsrc\s*=\s*(?:"([^"]+)"|'([^']+)'|([^\s>]+))[^>]*\/?>/gi;
          while ((m = htmlImgRe.exec(text)) !== null) {
            const rawTarget = (m[1] || m[2] || m[3] || "").trim();
            const resolvedSrc = resolveIRImageSrc(rawTarget);
            if (!resolvedSrc) continue;
            const altM = /\balt\s*=\s*(?:"([^"]*)"|'([^']*)')/i.exec(m[0]);
            const alt = (altM ? altM[1] ?? altM[2] ?? "" : "").trim();
            const s = m.index;
            const e = s + m[0].length;
            if (markFree(s, e)) {
              ops.push({
                from: line.from + s,
                to: line.from + e,
                deco: Decoration.replace({ widget: new ImageIRWidget(resolvedSrc, rawTarget, alt, line.from + s) }),
              });
            }
          }

          // Inline <audio ... src="..."></audio> player on non-cursor lines
          const audioRe = /<audio\b[^>]*\bsrc=(?:"([^"]+)"|'([^']+)'|([^\s>]+))[^>]*>\s*(?:<\/audio>)?/gi;
          while ((m = audioRe.exec(text)) !== null) {
            const rawSrc = (m[1] || m[2] || m[3] || "").trim();
            if (!rawSrc || !/^(?:\/|https?:\/\/)/i.test(rawSrc)) continue;
            const s = m.index;
            const e = m.index + m[0].length;
            if (markFree(s, e)) {
              ops.push({
                from: line.from + s,
                to: line.from + e,
                deco: Decoration.replace({ widget: new AudioIRWidget(rawSrc) }),
              });
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

class BrIRWidget extends WidgetType {
  eq(): boolean {
    return true;
  }
  toDOM(): HTMLElement {
    return document.createElement("br");
  }
}

class ImageIRWidget extends WidgetType {
  constructor(
    readonly src: string,
    readonly rawSrc: string,
    readonly alt: string,
    readonly pos: number
  ) {
    super();
  }
  eq(other: ImageIRWidget): boolean {
    return this.src === other.src && this.rawSrc === other.rawSrc && this.alt === other.alt && this.pos === other.pos;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = document.createElement("span");
    wrap.className = "cm-ir-img-wrap";
    wrap.title = this.alt ? `${this.alt} (${this.rawSrc})` : this.rawSrc;

    const img = document.createElement("img");
    img.className = "cm-ir-img";
    img.src = this.src;
    img.alt = this.alt;
    img.loading = "lazy";
    img.decoding = "async";

    const fb = document.createElement("span");
    fb.className = "cm-ir-img-fallback";
    fb.hidden = true;
    fb.textContent = this.alt ? `🖼 ${this.alt} · ${this.rawSrc}` : `🖼 ${this.rawSrc}`;

    img.onerror = () => {
      img.hidden = true;
      fb.hidden = false;
    };

    wrap.addEventListener("mousedown", (e) => {
      e.preventDefault();
      const safePos = Math.min(this.pos, view.state.doc.length);
      view.dispatch({ selection: { anchor: safePos } });
      view.focus();
    });

    wrap.appendChild(img);
    wrap.appendChild(fb);
    return wrap;
  }
  ignoreEvent(): boolean {
    return true;
  }
}

class AudioIRWidget extends WidgetType {
  constructor(readonly src: string) {
    super();
  }
  eq(other: AudioIRWidget): boolean {
    return this.src === other.src;
  }
  toDOM(): HTMLElement {
    const el = document.createElement("audio");
    el.controls = true;
    el.preload = "metadata";
    el.src = this.src;
    el.className = "cm-ir-audio";
    return el;
  }
  ignoreEvent(): boolean {
    return true;
  }
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
    case "u":
      return `<u>${inner()}</u>`;
    case "mark":
      return `<mark>${inner()}</mark>`;
    case "font": {
      const col = (el.getAttribute("color") || el.style?.color || "").trim();
      if (col && SAFE_IR_COLOR_RE.test(col)) {
        return `<font color="${col}">${inner()}</font>`;
      }
      return inner();
    }
    case "span": {
      const col = (el.style?.color || "").trim();
      if (col && SAFE_IR_COLOR_RE.test(col)) {
        return `<font color="${col}">${inner()}</font>`;
      }
      return inner();
    }
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
    case "audio": {
      const src = (el.getAttribute("src") || el.querySelector("source")?.getAttribute("src") || "").trim();
      if (!src) return "";
      return `\n<audio controls preload="metadata" src="${src}"></audio>\n\n`;
    }
    case "hr":
      return `\n---\n\n`;
    case "table": {
      const rows = Array.from(el.querySelectorAll("tr"));
      if (rows.length === 0) return "";
      const matrix = rows.map((tr) =>
        Array.from(tr.querySelectorAll("th,td")).map((cell) =>
          (cell.textContent || "").trim().replace(/\\/g, "\\\\").replace(/\|/g, "\\|")
        )
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

const EDITOR_SWATCHES = [
  { color: "#ff0000", label: "#FF0000" },
  { color: "#ea580c", label: "#EA580C" },
  { color: "#ca8a04", label: "#CA8A04" },
  { color: "#16a34a", label: "#16A34A" },
  { color: "#2563eb", label: "#2563EB" },
  { color: "#7c3aed", label: "#7C3AED" },
  { color: "#64748b", label: "#64748B" },
] as const;

const EDITOR_CALLOUTS = [
  { kind: "NOTE", color: "#2563eb" },
  { kind: "TIP", color: "#16a34a" },
  { kind: "IMPORTANT", color: "#7c3aed" },
  { kind: "WARNING", color: "#ca8a04" },
  { kind: "CAUTION", color: "#dc2626" },
] as const;

type ToolbarPopKind = "" | "heading" | "color" | "callout" | "media" | "book";

export function CM6EditorCore({ value, onChange, quickInserts = [], onSave }: CM6EditorProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const wysiwygRef = useRef<HTMLDivElement | null>(null);
  const previewRef = useRef<HTMLDivElement | null>(null);
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
  const [syncScroll, setSyncScroll] = useState<boolean>(() => {
    return localStorage.getItem("volo_editor_sync_scroll") !== "0";
  });
  const syncScrollRef = useRef(syncScroll);
  syncScrollRef.current = syncScroll;
  const scrollDriverRef = useRef<{ source: "cm" | "pv" | null; timer: number }>({ source: null, timer: 0 });

  const [previewHTML, setPreviewHTML] = useState<string>("");
  const [uploading, setUploading] = useState<boolean>(false);
  const [fullscreen, setFullscreen] = useState<boolean>(false);
  const [showOutline, setShowOutline] = useState<boolean>(false);
  const [mobilePane, setMobilePane] = useState<"write" | "preview" | "outline">("write");

  const [activePop, setActivePop] = useState<ToolbarPopKind>("");
  const [popStyle, setPopStyle] = useState<CSSProperties>({});
  const popBtnRefs = useRef<Record<string, HTMLButtonElement | null>>({});
  const popMenuRef = useRef<HTMLDivElement | null>(null);
  const [edTip, setEdTip] = useState<{ text: string; x: number; y: number; below: boolean } | null>(null);

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

  const togglePop = (kind: Exclude<ToolbarPopKind, "">) => {
    setEdTip(null);
    setActivePop((prev) => (prev === kind ? "" : kind));
  };

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

  const getSelectedText = (): string => {
    if (mode === "wysiwyg") {
      return window.getSelection()?.toString() || "";
    }
    const view = viewRef.current;
    if (!view) return "";
    const sel = view.state.selection.main;
    return sel.empty ? "" : view.state.sliceDoc(sel.from, sel.to);
  };

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
      else if (before === "<u>") document.execCommand("underline");
      else if (before === "<mark>") document.execCommand("insertHTML", false, `<mark>${getSelectedText() || fallback}</mark>`);
      else document.execCommand("insertHTML", false, `<code>${getSelectedText() || fallback}</code>`);
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

  const applyColorStyle = (color: string) => {
    if (mode === "wysiwyg" && wysiwygRef.current) {
      wysiwygRef.current.focus();
      document.execCommand("foreColor", false, color);
      onChangeRef.current(htmlToMarkdown(wysiwygRef.current));
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const rawSel = view.state.sliceDoc(sel.from, sel.to);
    const unwrapped = (rawSel || "text").replace(/^<(?:font|span)\b[^>]*>([\s\S]*?)<\/(?:font|span)>$/i, "$1");
    const before = `<font color="${color}">`;
    const after = `</font>`;
    const insert = `${before}${unwrapped}${after}`;
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert },
      selection: { anchor: sel.from + before.length, head: sel.from + before.length + unwrapped.length },
    });
    view.focus();
  };

  const clearInlineStyle = () => {
    if (mode === "wysiwyg" && wysiwygRef.current) {
      wysiwygRef.current.focus();
      document.execCommand("removeFormat");
      onChangeRef.current(htmlToMarkdown(wysiwygRef.current));
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const stripTags = (s: string) =>
      s
        .replace(/<\/?(?:font|mark|u)\b[^>]*>/gi, "")
        .replace(/<span\s+style\s*=\s*(?:"[^"]*"|'[^']*')\s*>([\s\S]*?)<\/span>/gi, "$1");
    if (!sel.empty) {
      const cleaned = stripTags(view.state.sliceDoc(sel.from, sel.to));
      view.dispatch({
        changes: { from: sel.from, to: sel.to, insert: cleaned },
        selection: { anchor: sel.from, head: sel.from + cleaned.length },
      });
    } else {
      const line = view.state.doc.lineAt(sel.head);
      const cleaned = stripTags(line.text);
      if (cleaned !== line.text) {
        view.dispatch({
          changes: { from: line.from, to: line.to, insert: cleaned },
          selection: { anchor: Math.min(line.from + cleaned.length, sel.head) },
        });
      }
    }
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
      if (res.asset.mime_type?.startsWith("audio/") || file.type?.startsWith("audio/")) {
        insertSnippet(`\n<audio controls preload="metadata" src="${url}"></audio>\n`);
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

  const [recording, setRecording] = useState<boolean>(false);
  const [recordSec, setRecordSec] = useState<number>(0);
  const mediaRecorderRef = useRef<MediaRecorder | null>(null);
  const recordStreamRef = useRef<MediaStream | null>(null);
  const recordChunksRef = useRef<BlobPart[]>([]);
  const recordTimerRef = useRef<number>(0);

  const cleanupRecordingTracks = () => {
    if (recordTimerRef.current) {
      window.clearInterval(recordTimerRef.current);
      recordTimerRef.current = 0;
    }
    if (recordStreamRef.current) {
      recordStreamRef.current.getTracks().forEach((tr) => tr.stop());
      recordStreamRef.current = null;
    }
  };

  useEffect(() => {
    return () => {
      try {
        if (mediaRecorderRef.current && mediaRecorderRef.current.state !== "inactive") {
          mediaRecorderRef.current.onstop = null;
          mediaRecorderRef.current.stop();
        }
      } catch {
        /* ignore */
      }
      mediaRecorderRef.current = null;
      cleanupRecordingTracks();
    };
  }, []);

  const formatRecordClock = (sec: number): string => {
    const m = Math.floor(sec / 60);
    const s = sec % 60;
    return `${String(m).padStart(2, "0")}:${String(s).padStart(2, "0")}`;
  };

  const toggleAudioRecording = async () => {
    setEdTip(null);
    if (recording && mediaRecorderRef.current) {
      try {
        if (mediaRecorderRef.current.state !== "inactive") {
          mediaRecorderRef.current.stop();
        }
      } catch {
        setRecording(false);
        cleanupRecordingTracks();
      }
      return;
    }

    if (
      (typeof window !== "undefined" && !window.isSecureContext) ||
      typeof navigator === "undefined" ||
      !navigator.mediaDevices?.getUserMedia ||
      typeof MediaRecorder === "undefined"
    ) {
      showToast(t("ed.record_unsupported"), "warn");
      return;
    }

    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      const candidates: Array<{ mime: string; blobType: string; ext: string }> = [
        { mime: "audio/webm;codecs=opus", blobType: "audio/webm", ext: "weba" },
        { mime: "audio/webm", blobType: "audio/webm", ext: "weba" },
        { mime: "audio/ogg;codecs=opus", blobType: "audio/ogg", ext: "ogg" },
        { mime: "audio/mp4", blobType: "audio/mp4", ext: "m4a" },
      ];
      const chosen =
        candidates.find((c) => typeof MediaRecorder.isTypeSupported === "function" && MediaRecorder.isTypeSupported(c.mime)) ||
        candidates[1];

      const recorder =
        typeof MediaRecorder.isTypeSupported === "function" && MediaRecorder.isTypeSupported(chosen.mime)
          ? new MediaRecorder(stream, { mimeType: chosen.mime })
          : new MediaRecorder(stream);

      recordStreamRef.current = stream;
      mediaRecorderRef.current = recorder;
      recordChunksRef.current = [];
      setRecordSec(0);
      setRecording(true);

      recordTimerRef.current = window.setInterval(() => {
        setRecordSec((prev) => prev + 1);
      }, 1000);

      recorder.ondataavailable = (ev: BlobEvent) => {
        if (ev.data && ev.data.size > 0) {
          recordChunksRef.current.push(ev.data);
        }
      };

      recorder.onerror = () => {
        setRecording(false);
        mediaRecorderRef.current = null;
        cleanupRecordingTracks();
      };

      recorder.onstop = () => {
        const chunks = recordChunksRef.current.slice();
        recordChunksRef.current = [];
        setRecording(false);
        mediaRecorderRef.current = null;
        cleanupRecordingTracks();
        if (chunks.length === 0) return;
        const actualMime = (recorder.mimeType || chosen.blobType).toLowerCase();
        let ext = chosen.ext;
        let blobType = chosen.blobType;
        if (actualMime.includes("ogg")) {
          ext = "ogg";
          blobType = "audio/ogg";
        } else if (actualMime.includes("mp4") || actualMime.includes("aac")) {
          ext = "m4a";
          blobType = "audio/mp4";
        } else {
          ext = "weba";
          blobType = "audio/webm";
        }
        const now = new Date();
        const stamp = [
          now.getFullYear(),
          String(now.getMonth() + 1).padStart(2, "0"),
          String(now.getDate()).padStart(2, "0"),
          "-",
          String(now.getHours()).padStart(2, "0"),
          String(now.getMinutes()).padStart(2, "0"),
          String(now.getSeconds()).padStart(2, "0"),
        ].join("");
        const blob = new Blob(chunks, { type: blobType });
        if (blob.size === 0) return;
        const file = new File([blob], `recording-${stamp}.${ext}`, { type: blobType });
        void uploadMediaFile(file);
      };

      recorder.start(250);
      showToast(t("ed.record_started"), "ok");
    } catch (err: any) {
      cleanupRecordingTracks();
      setRecording(false);
      const errName = String(err?.name || "");
      if (errName === "NotFoundError" || errName === "DevicesNotFoundError") {
        showToast(t("ed.record_no_device"), "warn");
      } else {
        showToast(t("ed.record_denied"), "err");
      }
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
        key: "Mod-u",
        run: () => {
          wrapSelection("<u>", "</u>", "text");
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
        Prec.high(keymap.of(markdownEditingKeymap)),
        pasteURLAsLinkExt,
        EditorView.lineWrapping,
        phCompartment.current.of(placeholder(t("ed.placeholder"))),
        shortcutKeymap,
        keymap.of([...voloBaseKeymap, ...historyKeymap]),
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

  // Bidirectional heading-anchored + proportional scroll sync in Split Preview (`mode === "sv"`)
  useEffect(() => {
    if (mode !== "sv") return;
    const view = viewRef.current;
    const cmScroller = view?.scrollDOM;
    const pvPane = previewRef.current;
    if (!view || !cmScroller || !pvPane) return;

    interface ScrollAnchor {
      cm: number;
      pv: number;
    }

    const computeAnchors = (): ScrollAnchor[] | null => {
      const cmMax = Math.max(0, cmScroller.scrollHeight - cmScroller.clientHeight);
      const pvMax = Math.max(0, pvPane.scrollHeight - pvPane.clientHeight);
      if (cmMax <= 0 && pvMax <= 0) return null;
      const anchors: ScrollAnchor[] = [{ cm: 0, pv: 0 }];

      const mdOffsets: number[] = [];
      let fenced = false;
      for (let ln = 1; ln <= view.state.doc.lines; ln++) {
        const lineObj = view.state.doc.line(ln);
        if (/^\s*```/.test(lineObj.text)) {
          fenced = !fenced;
          continue;
        }
        if (!fenced && /^#{1,6}\s+\S/.test(lineObj.text)) {
          mdOffsets.push(lineObj.from);
        }
      }

      const pvHeads = pvPane.querySelectorAll<HTMLElement>(
        ":scope > h1, :scope > h2, :scope > h3, :scope > h4, :scope > h5, :scope > h6"
      );
      if (mdOffsets.length > 0 && mdOffsets.length === pvHeads.length && cmMax > 0 && pvMax > 0) {
        const paneTop = pvPane.getBoundingClientRect().top;
        for (let i = 0; i < mdOffsets.length; i++) {
          const cmY = Math.min(cmMax, Math.max(0, view.lineBlockAt(mdOffsets[i]).top - 8));
          const pvY = Math.min(pvMax, Math.max(0, pvHeads[i].getBoundingClientRect().top - paneTop + pvPane.scrollTop - 8));
          const prev = anchors[anchors.length - 1];
          if (cmY >= prev.cm && pvY >= prev.pv && (cmY > prev.cm || pvY > prev.pv)) {
            anchors.push({ cm: cmY, pv: pvY });
          }
        }
      }

      const last = anchors[anchors.length - 1];
      anchors.push({ cm: Math.max(last.cm, cmMax), pv: Math.max(last.pv, pvMax) });
      return anchors;
    };

    const interpolate = (val: number, anchors: ScrollAnchor[], fromK: "cm" | "pv", toK: "cm" | "pv"): number => {
      const end = anchors[anchors.length - 1];
      if (end[fromK] <= 0 || val <= 1) return 0;
      if (val >= end[fromK] - 1) return end[toK];
      for (let i = 0; i < anchors.length - 1; i++) {
        const a = anchors[i];
        const b = anchors[i + 1];
        if (val <= b[fromK]) {
          const span = b[fromK] - a[fromK];
          const ratio = span > 0 ? (val - a[fromK]) / span : 0;
          return a[toK] + ratio * (b[toK] - a[toK]);
        }
      }
      return end[toK];
    };

    const lockDriver = (src: "cm" | "pv") => {
      scrollDriverRef.current.source = src;
      if (scrollDriverRef.current.timer) {
        window.clearTimeout(scrollDriverRef.current.timer);
      }
      scrollDriverRef.current.timer = window.setTimeout(() => {
        scrollDriverRef.current.source = null;
        scrollDriverRef.current.timer = 0;
      }, 110);
    };

    const syncCmToPv = () => {
      if (!syncScrollRef.current) return;
      if (scrollDriverRef.current.source === "pv") return;
      const anchors = computeAnchors();
      if (!anchors) return;
      const nextTop = interpolate(cmScroller.scrollTop, anchors, "cm", "pv");
      if (Math.abs(pvPane.scrollTop - nextTop) >= 1) {
        lockDriver("cm");
        pvPane.scrollTop = nextTop;
      }
    };

    const syncPvToCm = () => {
      if (!syncScrollRef.current) return;
      if (scrollDriverRef.current.source === "cm") return;
      const anchors = computeAnchors();
      if (!anchors) return;
      const nextTop = interpolate(pvPane.scrollTop, anchors, "pv", "cm");
      if (Math.abs(cmScroller.scrollTop - nextTop) >= 1) {
        lockDriver("pv");
        cmScroller.scrollTop = nextTop;
      }
    };

    cmScroller.addEventListener("scroll", syncCmToPv, { passive: true });
    pvPane.addEventListener("scroll", syncPvToCm, { passive: true });
    const rafId = window.requestAnimationFrame(syncCmToPv);

    return () => {
      window.cancelAnimationFrame(rafId);
      cmScroller.removeEventListener("scroll", syncCmToPv);
      pvPane.removeEventListener("scroll", syncPvToCm);
    };
  }, [mode, previewHTML, syncScroll]);

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

  // Unified portal popover positioning for Heading / Color / Callout / Media / Bookshelf menus
  useEffect(() => {
    if (!activePop) return;
    const updatePos = () => {
      const btn = popBtnRefs.current[activePop];
      if (!btn) return;
      const rect = btn.getBoundingClientRect();
      const vv = window.visualViewport;
      const vvTop = vv?.offsetTop ?? 0;
      const vpH = vv?.height ?? window.innerHeight;
      if (rect.bottom < vvTop - 40 || rect.top > vvTop + vpH + 40) {
        setActivePop("");
        return;
      }
      const dims =
        activePop === "heading"
          ? { minW: 112, maxW: 180, maxH: 260, estH: 228 }
          : activePop === "color"
          ? { minW: 224, maxW: 260, maxH: 260, estH: 176 }
          : activePop === "callout"
          ? { minW: 196, maxW: 240, maxH: 250, estH: 200 }
          : activePop === "book"
          ? { minW: 236, maxW: 290, maxH: 180, estH: 112 }
          : { minW: 184, maxW: 260, maxH: 200, estH: 120 };
      const estH = popMenuRef.current?.offsetHeight || dims.estH;
      setPopStyle(computePortalMenuStyle(rect, { ...dims, estH, align: "left" }).style);
    };
    notifyClosePopovers(999999, false);
    updatePos();
    const onDocDown = (e: MouseEvent) => {
      const target = e.target as Node;
      const btn = popBtnRefs.current[activePop];
      if (btn?.contains(target) || popMenuRef.current?.contains(target)) return;
      setActivePop("");
    };
    const onPopClose = (ev: Event) => {
      const detail = (ev as CustomEvent<{ exceptId?: number }>).detail;
      if (detail?.exceptId === 999999) return;
      setActivePop("");
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setActivePop("");
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDocDown, true);
    document.addEventListener("keydown", onEsc);
    window.addEventListener(POPOVER_CLOSE_EVENT, onPopClose);
    window.addEventListener("resize", updatePos);
    window.addEventListener("scroll", updatePos, true);
    vv?.addEventListener("resize", updatePos);
    vv?.addEventListener("scroll", updatePos);
    return () => {
      document.removeEventListener("mousedown", onDocDown, true);
      document.removeEventListener("keydown", onEsc);
      window.removeEventListener(POPOVER_CLOSE_EVENT, onPopClose);
      window.removeEventListener("resize", updatePos);
      window.removeEventListener("scroll", updatePos, true);
      vv?.removeEventListener("resize", updatePos);
      vv?.removeEventListener("scroll", updatePos);
    };
  }, [activePop]);

  useEffect(() => {
    if (!edTip) return;
    const dismiss = () => setEdTip(null);
    window.addEventListener("scroll", dismiss, true);
    window.addEventListener("resize", dismiss);
    return () => {
      window.removeEventListener("scroll", dismiss, true);
      window.removeEventListener("resize", dismiss);
    };
  }, [edTip]);

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

  const handleToolbarPointerOver = (e: PointerEvent) => {
    if (e.pointerType === "touch") return;
    if (typeof window !== "undefined" && window.matchMedia?.("(hover: none)").matches) return;
    const btn = (e.target as HTMLElement | null)?.closest("[data-tip]") as HTMLElement | null;
    if (!btn) {
      setEdTip(null);
      return;
    }
    const text = btn.getAttribute("data-tip") || "";
    if (!text) {
      setEdTip(null);
      return;
    }
    const r = btn.getBoundingClientRect();
    const below = r.top < 46;
    const x = Math.max(16, Math.min(window.innerWidth - 16, Math.round(r.left + r.width / 2)));
    const y = below ? Math.round(r.bottom + 8) : Math.round(r.top - 8);
    setEdTip({ text, x, y, below });
  };

  const mergedQuickInserts = quickInserts.length > 0 ? quickInserts : dynQuickInserts || [];

  return (
    <div class={`volo-studio-editor ${fullscreen ? "is-fullscreen" : ""}`}>
      {/* Top Sticky Rich Icon Toolbar */}
      <div
        class="volo-ed-toolbar"
        onPointerOver={handleToolbarPointerOver as any}
        onPointerLeave={() => setEdTip(null)}
        onPointerDown={() => setEdTip(null)}
      >
        <div class="volo-ed-tools">
          {/* Group 1: Undo / Redo */}
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.undo")}
            aria-label={t("ed.undo")}
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
            data-tip={t("ed.redo")}
            aria-label={t("ed.redo")}
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

          {/* Group 2: Heading, Typography & Color/Highlight */}
          <div class="volo-pop-wrap">
            <button
              ref={(el) => {
                popBtnRefs.current.heading = el;
              }}
              type="button"
              class={`volo-ed-btn ${activePop === "heading" ? "active" : ""}`}
              data-tip={t("ed.heading")}
              aria-label={t("ed.heading")}
              onClick={() => togglePop("heading")}
            >
              <span style={{ fontWeight: 750, fontSize: "12.5px" }}>H▾</span>
            </button>
          </div>

          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.bold")}
            aria-label={t("ed.bold")}
            onClick={() => wrapSelection("**", "**", "bold")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round">
              <path d="M6 4h8a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z" />
              <path d="M6 12h9a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.italic")}
            aria-label={t("ed.italic")}
            onClick={() => wrapSelection("*", "*", "italic")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="19" y1="4" x2="10" y2="4" />
              <line x1="14" y1="20" x2="5" y2="20" />
              <line x1="15" y1="4" x2="9" y2="20" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.strike")}
            aria-label={t("ed.strike")}
            onClick={() => wrapSelection("~~", "~~", "text")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M16 4H9a3 3 0 0 0-2.83 4" />
              <path d="M14 12a4 4 0 0 1 0 8H6" />
              <line x1="4" y1="12" x2="20" y2="12" />
            </svg>
          </button>

          {/* Text Color, Highlight (<mark>) & Underline (<u>) Popover */}
          <div class="volo-pop-wrap">
            <button
              ref={(el) => {
                popBtnRefs.current.color = el;
              }}
              type="button"
              class={`volo-ed-btn ${activePop === "color" ? "active" : ""}`}
              data-tip={t("ed.text_color")}
              aria-label={t("ed.text_color")}
              onClick={() => togglePop("color")}
            >
              <span class="volo-ed-color-glyph">
                <span>A</span>
                <i />
              </span>
            </button>
          </div>

          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.code")}
            aria-label={t("ed.code")}
            onClick={() => wrapSelection("`", "`", "code")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <polyline points="16 18 22 12 16 6" />
              <polyline points="8 6 2 12 8 18" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          {/* Group 3: Quote, Callout, Bullet / Ordered / Task Lists */}
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.quote")}
            aria-label={t("ed.quote")}
            onClick={() => prefixCurrentLine("> ")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M3 21c3 0 7-1 7-8V5c0-1.25-.756-2.017-2-2H4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2 1 0 1 0 1 1v1c0 1-1 2-2 2s-1 .008-1 1.031V20c0 1 0 1 1 1z" />
              <path d="M15 21c3 0 7-1 7-8V5c0-1.25-.757-2.017-2-2h-4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2h.75c0 2.25.25 4-2.75 4v3c0 1 0 1 1 1z" />
            </svg>
          </button>

          <div class="volo-pop-wrap">
            <button
              ref={(el) => {
                popBtnRefs.current.callout = el;
              }}
              type="button"
              class={`volo-ed-btn ${activePop === "callout" ? "active" : ""}`}
              data-tip={t("ed.callout")}
              aria-label={t("ed.callout")}
              onClick={() => togglePop("callout")}
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <circle cx="12" cy="12" r="9" />
                <line x1="12" y1="8" x2="12" y2="12" />
                <line x1="12" y1="16" x2="12.01" y2="16" />
              </svg>
            </button>
          </div>

          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.ul")}
            aria-label={t("ed.ul")}
            onClick={() => prefixCurrentLine("- ")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <line x1="8" y1="6" x2="21" y2="6" />
              <line x1="8" y1="12" x2="21" y2="12" />
              <line x1="8" y1="18" x2="21" y2="18" />
              <circle cx="4" cy="6" r="1" fill="currentColor" />
              <circle cx="4" cy="12" r="1" fill="currentColor" />
              <circle cx="4" cy="18" r="1" fill="currentColor" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.ol")}
            aria-label={t("ed.ol")}
            onClick={() => prefixCurrentLine("1. ")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <line x1="10" y1="6" x2="21" y2="6" />
              <line x1="10" y1="12" x2="21" y2="12" />
              <line x1="10" y1="18" x2="21" y2="18" />
              <path d="M4 6h1v4" />
              <path d="M4 10h2" />
              <path d="M6 18H4c0-1 2-2 2-3s-1-1.5-2-1" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.task")}
            aria-label={t("ed.task")}
            onClick={() => prefixCurrentLine("- [ ] ")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="5" width="6" height="6" rx="1.2" />
              <path d="m3 17 2 2 4-4" />
              <line x1="13" y1="8" x2="21" y2="8" />
              <line x1="13" y1="17" x2="21" y2="17" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          {/* Group 4: Link, Media, Table, Code Block, Divider */}
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.link")}
            aria-label={t("ed.link")}
            onClick={() => wrapSelection("[", "](https://)", t("ed.link_text_ph"))}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
              <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
            </svg>
          </button>

          <div class="volo-pop-wrap">
            <button
              ref={(el) => {
                popBtnRefs.current.media = el;
              }}
              type="button"
              class={`volo-ed-btn ${activePop === "media" ? "active" : ""}`}
              data-tip={uploading ? t("ed.uploading") : t("ed.upload_media")}
              aria-label={uploading ? t("ed.uploading") : t("ed.upload_media")}
              onClick={() => togglePop("media")}
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
          </div>

          <button
            type="button"
            class={`volo-ed-btn ${recording ? "is-recording" : ""}`}
            data-tip={recording ? t("ed.record_stop", { time: formatRecordClock(recordSec) }) : t("ed.record_toggle")}
            aria-label={recording ? t("ed.record_stop", { time: formatRecordClock(recordSec) }) : t("ed.record_toggle")}
            aria-pressed={recording}
            onClick={() => void toggleAudioRecording()}
          >
            {recording ? (
              <>
                <span class="volo-ed-rec-dot" aria-hidden="true" />
                <span>{formatRecordClock(recordSec)}</span>
              </>
            ) : (
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <rect x="9" y="2" width="6" height="12" rx="3" />
                <path d="M5 10a7 7 0 0 0 14 0" />
                <line x1="12" y1="17" x2="12" y2="21" />
                <line x1="8" y1="21" x2="16" y2="21" />
              </svg>
            )}
          </button>

          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.table")}
            aria-label={t("ed.table")}
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
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.code_block")}
            aria-label={t("ed.code_block")}
            onClick={() => insertSnippet("\n```bash\n\n```\n")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="4" width="18" height="16" rx="2" />
              <polyline points="8 10 11 12.5 8 15" />
              <line x1="13" y1="15" x2="16" y2="15" />
            </svg>
          </button>
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.hr")}
            aria-label={t("ed.hr")}
            onClick={() => insertSnippet("\n---\n")}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round">
              <line x1="4" y1="12" x2="20" y2="12" />
            </svg>
          </button>

          <span class="volo-ed-sep" />

          {/* Group 5: Volo Extension Shortcuts (Reader Lock, Password Lock, Embedded Bookshelf, Quick Insert Hub) */}
          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.sc_lock_user")}
            aria-label={t("ed.sc_lock_user")}
            onClick={() => {
              const sel = getSelectedText().trim();
              if (sel) {
                wrapSelection(`\n:::lock type="user"\n`, `\n:::\n`, sel);
              } else {
                insertSnippet(`\n:::lock type="user"\n${t("ed.lock_user_ph")}\n:::\n`);
              }
            }}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
              <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
              <circle cx="9" cy="7" r="4" />
              <rect x="15" y="13" width="7" height="6" rx="1.2" />
              <path d="M17 13v-1.5a1.5 1.5 0 0 1 3 0V13" />
            </svg>
          </button>

          <button
            type="button"
            class="volo-ed-btn"
            data-tip={t("ed.sc_lock_pwd")}
            aria-label={t("ed.sc_lock_pwd")}
            onClick={() => {
              const sel = getSelectedText().trim();
              if (sel) setLockBody(sel);
              setLockMode("pwd");
              setQiTab("lock");
              setQiOpen(true);
            }}
          >
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
              <rect x="5" y="11" width="14" height="10" rx="2" />
              <path d="M8 11V7a4 4 0 0 1 8 0v4" />
              <circle cx="12" cy="16" r="1" fill="currentColor" />
            </svg>
          </button>

          <div class="volo-pop-wrap">
            <button
              ref={(el) => {
                popBtnRefs.current.book = el;
              }}
              type="button"
              class={`volo-ed-btn ${activePop === "book" ? "active" : ""}`}
              data-tip={t("ed.sc_bookshelf")}
              aria-label={t("ed.sc_bookshelf")}
              onClick={() => togglePop("book")}
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.85" stroke-linecap="round" stroke-linejoin="round">
                <path d="M2 4h6a4 4 0 0 1 4 4v13a3 3 0 0 0-3-3H2z" />
                <path d="M22 4h-6a4 4 0 0 0-4 4v13a3 3 0 0 1 3-3h7z" />
              </svg>
            </button>
          </div>

          {/* Quick Insert + Modal Trigger */}
          <button
            type="button"
            class="volo-ed-btn volo-ed-btn-accent"
            data-tip={t("ed.quick_insert")}
            aria-label={t("ed.quick_insert")}
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
              <PlusIcon size={12} /> {qi.label}
            </button>
          ))}
        </div>

        {/* Right Controls: 3-Mode Switcher + Sync Scroll (in sv) + Outline + Fullscreen */}
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

          {mode === "sv" && (
            <button
              type="button"
              class={`volo-ed-btn ${syncScroll ? "active" : ""}`}
              data-tip={t("ed.sync_scroll")}
              aria-label={t("ed.sync_scroll")}
              onClick={() =>
                setSyncScroll((prev) => {
                  const next = !prev;
                  localStorage.setItem("volo_editor_sync_scroll", next ? "1" : "0");
                  return next;
                })
              }
            >
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round">
                <path d="m7 7 3-3 3 3" />
                <path d="M10 4v11" />
                <path d="m17 17-3 3-3-3" />
                <path d="M14 20V9" />
              </svg>
            </button>
          )}

          <button
            type="button"
            class={`volo-ed-btn ${showOutline ? "active" : ""}`}
            data-tip={t("ed.outline")}
            aria-label={t("ed.outline")}
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
            data-tip={fullscreen ? t("ed.exit_fullscreen") : t("ed.fullscreen")}
            aria-label={fullscreen ? t("ed.exit_fullscreen") : t("ed.fullscreen")}
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

      {/* Portal Popovers for Heading / Color / Callout / Media / Bookshelf */}
      {activePop &&
        typeof document !== "undefined" &&
        createPortal(
          <div ref={popMenuRef} class="volo-cselect-menu is-portal volo-ed-pop" style={popStyle}>
            {activePop === "heading" &&
              [1, 2, 3, 4, 5, 6].map((lvl) => (
                <button
                  key={lvl}
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    prefixCurrentLine(`${"#".repeat(lvl)} `);
                    setActivePop("");
                  }}
                >
                  <span style={{ fontWeight: 700, fontSize: `${Math.max(12, 17 - lvl)}px` }}>H{lvl}</span>
                </button>
              ))}

            {activePop === "color" && (
              <div class="volo-color-pop">
                <div class="volo-color-swatches">
                  {EDITOR_SWATCHES.map((sw) => (
                    <button
                      key={sw.color}
                      type="button"
                      class="volo-color-swatch"
                      title={sw.label}
                      style={{ background: sw.color }}
                      onClick={() => {
                        applyColorStyle(sw.color);
                        setActivePop("");
                      }}
                    />
                  ))}
                </div>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    wrapSelection("<mark>", "</mark>", "text");
                    setActivePop("");
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.color_mark")}</span>
                </button>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    wrapSelection("<u>", "</u>", "text");
                    setActivePop("");
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.underline")}</span>
                </button>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    clearInlineStyle();
                    setActivePop("");
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.color_clear")}</span>
                </button>
              </div>
            )}

            {activePop === "callout" &&
              EDITOR_CALLOUTS.map((co) => (
                <button
                  key={co.kind}
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    const sel = getSelectedText().trim() || "...";
                    insertSnippet(`\n> [!${co.kind}]\n> ${sel.replace(/\n/g, "\n> ")}\n`);
                    setActivePop("");
                  }}
                >
                  <span
                    style={{
                      display: "inline-flex",
                      alignItems: "center",
                      gap: "7px",
                      fontWeight: 650,
                      fontSize: "12.5px",
                    }}
                  >
                    <i style={{ width: "8px", height: "8px", borderRadius: "999px", background: co.color, display: "inline-block" }} />
                    [!{co.kind}]
                  </span>
                </button>
              ))}

            {activePop === "media" && (
              <>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    fileInputRef.current?.click();
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.upload_local_file")}</span>
                </button>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    setMediaModalOpen(true);
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.insert_from_library")}</span>
                </button>
              </>
            )}

            {activePop === "book" && (
              <>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    insertSnippet("\n:::bookshelf\n:::\n");
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.sc_bookshelf_all")}</span>
                </button>
                <button
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    setQiTab("books");
                    setQiOpen(true);
                  }}
                >
                  <span class="volo-cselect-item-text">{t("ed.sc_book_pick")}</span>
                </button>
              </>
            )}
          </div>,
          document.body
        )}

      {/* Instant Dark Floating Tooltip Bubble */}
      {edTip &&
        !activePop &&
        typeof document !== "undefined" &&
        createPortal(
          <div
            class={`volo-ed-tooltip ${edTip.below ? "is-below" : ""}`}
            style={{ left: `${edTip.x}px`, top: `${edTip.y}px` }}
            role="tooltip"
          >
            {edTip.text}
          </div>,
          document.body
        )}

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
            ref={previewRef}
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
            insertSnippet(`\n<audio controls preload="metadata" src="${asset.public_url}"></audio>\n`);
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
                  const fmtAttr = (k: string, raw: unknown) => {
                    const v = String(raw ?? "").replace(/[\r\n]+/g, " ").trim();
                    if (!v.includes('"')) return `${k}="${v}"`;
                    if (!v.includes("'")) return `${k}='${v}'`;
                    return `${k}="${v.replace(/"/g, "'")}"`;
                  };
                  const attr =
                    lockMode === "user"
                      ? `type="user"${lockHint.trim() ? ` ${fmtAttr("hint", lockHint)}` : ""}`
                      : `${fmtAttr("pwd", lockPwd || "123456")}${lockHint.trim() ? ` ${fmtAttr("hint", lockHint)}` : ""}`;
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
                          insertSnippet(`\n<audio controls preload="metadata" src="${m.public_url}"></audio>\n`);
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
                <PlusIcon size={13} /> {t("ed.bookshelf")}
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
                          const fmtAttr = (k: string, raw: unknown) => {
                            const v = String(raw ?? "").replace(/[\r\n]+/g, " ").trim();
                            if (!v.includes('"')) return `${k}="${v}"`;
                            if (!v.includes("'")) return `${k}='${v}'`;
                            return `${k}="${v.replace(/"/g, "'")}"`;
                          };
                          if (b.slug) {
                            insertSnippet(`\n:::book ${fmtAttr("slug", b.slug)}\n:::\n`);
                          } else {
                            insertSnippet(
                              `\n:::book ${fmtAttr("title", b.title)} ${fmtAttr("status", b.reading_status || "reading")}\n:::\n`
                            );
                          }
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
          voloIndentWithTab,
          ...voloBaseKeymap,
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

