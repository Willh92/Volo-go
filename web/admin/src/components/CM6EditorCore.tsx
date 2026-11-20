import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { createPortal } from "preact";
import type { CSSProperties } from "preact";
import { Compartment, EditorState, Prec, RangeSetBuilder, StateEffect, StateField } from "@codemirror/state";
import type { Extension } from "@codemirror/state";
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
import { apiFetch, readBootConfig, safeHref, safeMediaSrc, showToast, trimEndChars } from "../api";
import { parseClipboardHtml } from "./clipboardHtmlParser";
import { localeSig, t } from "../i18n";
import {
  cleanClipboardUrl,
  CODE_BLOCK_LANGS,
  collapseNewlines,
  computeCssContext,
  computeFenceDelim,
  DASH_COMMENT_LANGS,
  detectIRCodeLang,
  escapeMarkdownProseText,
  findCssStringRanges,
  HASH_COMMENT_LANGS,
  IR_CODE_RE,
  IR_FN_REF_RE,
  IR_INLINE_MATH_RE,
  IR_ML_ANCHOR_RE,
  NO_HASH_COMMENT_LANGS,
  parseQuotePrefix,
  resolveFenceLangFamily,
  splitGfmTableRow,
  stripQuotePrefix,
} from "./markdownHelpers";
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
  SvgIcon,
  useDebouncedValue,
} from "./UIControls";
import { MediaLibraryModal } from "./MediaLibraryModal";
import type { CM6CssEditorProps, CM6EditorProps } from "./CM6Editor";
import {
  buildCMImeHandlers,
  createPropSyncTracker,
  isPropSyncUpdate,
  recordEditorEmit,
  syncCMViewValue,
} from "./cm6PropSync";

const POP_DIMS: Record<string, { minW: number; maxW: number; maxH: number; estH: number }> = {
  heading: { minW: 112, maxW: 180, maxH: 260, estH: 228 },
  color: { minW: 224, maxW: 260, maxH: 310, estH: 236 },
  callout: { minW: 196, maxW: 240, maxH: 250, estH: 200 },
  codeblock: { minW: 168, maxW: 220, maxH: 300, estH: 280 },
  book: { minW: 236, maxW: 290, maxH: 180, estH: 112 },
  math: { minW: 224, maxW: 280, maxH: 240, estH: 172 },
  export: { minW: 236, maxW: 300, maxH: 220, estH: 142 },
};

type EditorMode = "ir" | "wysiwyg" | "sv";

async function copyTextToClipboard(text: string): Promise<void> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      /* fallback for non-secure LAN contexts */
    }
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  ta.style.pointerEvents = "none";
  document.body.appendChild(ta);
  ta.select();
  try {
    document.execCommand("copy");
  } finally {
    ta.remove();
  }
}

interface SearchMatchRange {
  from: number;
  to: number;
}

interface CMSearchState {
  decos: DecorationSet;
  matches: SearchMatchRange[];
  activeIdx: number;
}

const setSearchStateEffect = StateEffect.define<{ matches: SearchMatchRange[]; activeIdx: number }>();

const cmSearchHighlightField = StateField.define<CMSearchState>({
  create: () => ({ decos: Decoration.none, matches: [], activeIdx: 0 }),
  update(prev, tr) {
    for (const ef of tr.effects) {
      if (ef.is(setSearchStateEffect)) {
        const { matches, activeIdx } = ef.value;
        if (matches.length === 0) return { decos: Decoration.none, matches: [], activeIdx: 0 };
        const b = new RangeSetBuilder<Decoration>();
        const docLen = tr.state.doc.length;
        const clamped: SearchMatchRange[] = [];
        matches.forEach((m, idx) => {
          const s = Math.max(0, Math.min(docLen, m.from));
          const e = Math.max(s, Math.min(docLen, m.to));
          clamped.push({ from: s, to: e });
          if (s < e) {
            b.add(
              s,
              e,
              Decoration.mark({
                class: idx === activeIdx ? "cm-searchMatch cm-searchMatch-selected" : "cm-searchMatch",
              })
            );
          }
        });
        return { decos: b.finish(), matches: clamped, activeIdx };
      }
    }
    if (!tr.docChanged || prev.matches.length === 0) return prev;
    return {
      decos: prev.decos.map(tr.changes),
      matches: prev.matches.map((m) => ({
        from: tr.changes.mapPos(m.from, 1),
        to: tr.changes.mapPos(m.to, -1),
      })),
      activeIdx: prev.activeIdx,
    };
  },
  provide: (f) => EditorView.decorations.from(f, (v) => v.decos),
});

function getActiveSearchLineRange(
  state: EditorState
): { from: number; to: number; startLine: number; endLine: number } | null {
  const sh = state.field(cmSearchHighlightField, false);
  if (!sh || sh.matches.length === 0) return null;
  const idx = Math.max(0, Math.min(sh.activeIdx, sh.matches.length - 1));
  const m = sh.matches[idx];
  if (!m) return null;
  const docLen = state.doc.length;
  const from = Math.max(0, Math.min(docLen, m.from));
  const to = Math.max(from, Math.min(docLen, m.to));
  return {
    from,
    to,
    startLine: state.doc.lineAt(from).number,
    endLine: state.doc.lineAt(to).number,
  };
}

function mapSelectedLines(
  view: EditorView,
  userEvent: string,
  fn: (line: { from: number; text: string }) => { from: number; to: number; insert: string } | null
): boolean {
  const { state } = view;
  if (state.readOnly) return false;
  const changes: Array<{ from: number; to: number; insert: string }> = [];
  const seen = new Set<number>();
  for (const r of state.selection.ranges) {
    const startLine = state.doc.lineAt(r.from).number;
    const endLine =
      r.to > r.from && state.doc.lineAt(r.to).from === r.to
        ? Math.max(startLine, state.doc.lineAt(r.to).number - 1)
        : state.doc.lineAt(r.to).number;
    for (let ln = startLine; ln <= endLine; ln++) {
      if (seen.has(ln)) continue;
      seen.add(ln);
      const ch = fn(state.doc.line(ln));
      if (ch) changes.push(ch);
    }
  }
  if (changes.length > 0) {
    view.dispatch({ changes, userEvent });
  }
  return true;
}

const indentTwoSpaces = (view: EditorView): boolean =>
  mapSelectedLines(view, "input.indent", (line) => ({ from: line.from, to: line.from, insert: "  " }));

const dedentTwoSpaces = (view: EditorView): boolean =>
  mapSelectedLines(view, "input.dedent", (line) => {
    const n = line.text.startsWith("\t") ? 1 : line.text.startsWith("  ") ? 2 : line.text.startsWith(" ") ? 1 : 0;
    return n > 0 ? { from: line.from, to: line.from + n, insert: "" } : null;
  });

const voloIndentWithTab: KeyBinding = {
  key: "Tab",
  run: (view) => {
    const sel = view.state.selection.main;
    if (!sel.empty) return indentTwoSpaces(view);
    view.dispatch({
      changes: { from: sel.from, to: sel.to, insert: "  " },
      selection: { anchor: sel.from + 2 },
      userEvent: "input",
    });
    return true;
  },
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

interface FenceOpenMatch {
  quotePrefixLen: number;
  quoteDepth: number;
  leadSpaces: number;
  fenceChar: "`" | "~";
  fenceLen: number;
  lang: string;
  langOffset: number;
  langRawLen: number;
}

function countQuotePrefix(text: string): { depth: number; len: number } {
  const { prefixLen, markerCount } = parseQuotePrefix(text);
  return { depth: Math.min(3, markerCount), len: prefixLen };
}

function matchFenceOpen(text: string): FenceOpenMatch | null {
  const { prefixLen, markerCount } = parseQuotePrefix(text);
  const tail = text.slice(prefixLen);
  const m = /^( {0,3})(`{3,}|~{3,})([^\n\r\u2028\u2029]*)/.exec(tail);
  if (!m || m[0].length !== tail.length) return null;
  const qDepth = Math.min(3, markerCount);
  const fenceChar = m[2][0] as "`" | "~";
  const rest = m[3];
  if (fenceChar === "`" && rest.includes("`")) return null;
  const leadTrim = /^\s*/.exec(rest)?.[0].length ?? 0;
  const langTok = /^[^\s`~]*/.exec(rest.slice(leadTrim))?.[0] ?? "";
  return {
    quotePrefixLen: prefixLen,
    quoteDepth: qDepth,
    leadSpaces: prefixLen + m[1].length,
    fenceChar,
    fenceLen: m[2].length,
    lang: langTok,
    langOffset: prefixLen + m[1].length + m[2].length + leadTrim,
    langRawLen: langTok.length,
  };
}

function matchFenceClose(text: string, fenceChar: string, fenceLen: number, fenceQuoteDepth = 0): boolean {
  const { prefixLen, markerCount } = parseQuotePrefix(text);
  if (Math.min(3, markerCount) !== fenceQuoteDepth) return false;
  const rest = text.slice(prefixLen);
  let sp = 0;
  while (sp < 3 && sp < rest.length && rest[sp] === " ") sp++;
  let fl = 0;
  while (sp + fl < rest.length && rest[sp + fl] === fenceChar) fl++;
  if (fl < 3 || fl < fenceLen) return false;
  for (let i = sp + fl; i < rest.length; i++) {
    if (!/\s/.test(rest[i])) return false;
  }
  return true;
}

function trailingHorizWsLen(s: string): number {
  let i = s.length;
  while (i > 0 && (s[i - 1] === " " || s[i - 1] === "\t")) i--;
  return s.length - i;
}

function stripHorizWsBeforeNewline(s: string): string {
  if (!s.includes("\n")) return s;
  return s
    .split("\n")
    .map((line, i, arr) => (i < arr.length - 1 ? line.slice(0, line.length - trailingHorizWsLen(line)) : line))
    .join("\n");
}

function isInsideFencedCode(state: EditorState, lineNumber: number): boolean {
  if (lineNumber <= 1 || lineNumber > state.doc.lines) return false;
  const st = getIRDocScan(state.doc).fenceState[lineNumber] || 0;
  return st === 2 || st === 3;
}

function isInsideMathBlock(state: EditorState, lineNumber: number): boolean {
  if (lineNumber < 1 || lineNumber > state.doc.lines) return false;
  const scan = getIRDocScan(state.doc);
  if (scan.mathBlocks.some((mb) => lineNumber >= mb.startLine && lineNumber <= mb.endLine)) return true;
  let openDollar = false;
  let openBrk = false;
  for (let ln = 1; ln <= lineNumber; ln++) {
    if (scan.fenceState[ln] !== 0) continue;
    const t = state.doc.line(ln).text;
    if (/^\s*\$\$\s*$/.test(t)) openDollar = !openDollar;
    else if (/^\s*\\\[\s*$/.test(t)) openBrk = true;
    else if (/^\s*\\\]\s*$/.test(t)) openBrk = false;
  }
  return openDollar || openBrk;
}

const mdLinePrefixRe =
  /^(\s*(?:>\s*)*)(?:([-*+])\s+(\[[ xX]\]\s+)?|(\d+)([.)])\s+|)?(.*)$/;

function getGfmTableCellRanges(lineFrom: number, lineText: string): Array<{ from: number; to: number }> {
  const trimmed = lineText.trim();
  if (!trimmed.includes("|")) return [];
  const pipePositions: number[] = [];
  for (let i = 0; i < lineText.length; i++) {
    const ch = lineText[i];
    if (ch === "\\" && i + 1 < lineText.length && (lineText[i + 1] === "|" || lineText[i + 1] === "\\")) {
      i++;
      continue;
    }
    if (ch === "|") {
      pipePositions.push(i);
    }
  }
  if (pipePositions.length === 0) return [];
  const ranges: Array<{ from: number; to: number }> = [];
  const firstNonSpace = lineText.search(/\S/);
  const lastNonSpace = lineText.length - 1 - [...lineText].reverse().findIndex((c) => /\S/.test(c));
  if (firstNonSpace >= 0 && firstNonSpace < pipePositions[0]) {
    ranges.push({ from: lineFrom + firstNonSpace, to: lineFrom + pipePositions[0] });
  }
  for (let p = 0; p < pipePositions.length - 1; p++) {
    ranges.push({ from: lineFrom + pipePositions[p] + 1, to: lineFrom + pipePositions[p + 1] });
  }
  const lastPipe = pipePositions[pipePositions.length - 1];
  if (lastNonSpace > lastPipe) {
    ranges.push({ from: lineFrom + lastPipe + 1, to: lineFrom + lineText.length });
  }
  return ranges.map((r) => {
    const raw = lineText.slice(r.from - lineFrom, r.to - lineFrom);
    const lead = raw.match(/^\s*/)?.[0].length ?? 0;
    const trail = raw.length - raw.trimEnd().length;
    const contentFrom = r.from + lead;
    const contentTo = Math.max(contentFrom, r.to - trail);
    if (contentFrom === contentTo && r.to > r.from) {
      const mid = Math.min(r.to, r.from + 1);
      return { from: mid, to: mid };
    }
    return { from: contentFrom, to: contentTo };
  });
}

function navigateGfmTableCell(view: EditorView, dir: 1 | -1): boolean {
  const { state } = view;
  if (state.readOnly || state.selection.ranges.length !== 1) return false;
  const head = state.selection.main.head;
  const curLine = state.doc.lineAt(head);
  if (isInsideFencedCode(state, curLine.number)) return false;
  const scan = getIRDocScan(state.doc);
  const tbl = scan.tables.find((t) => t.kind === "md" && curLine.number >= t.startLine && curLine.number <= t.endLine);
  if (!tbl) return false;

  const cells = getGfmTableCellRanges(curLine.from, curLine.text);
  if (cells.length === 0) return false;

  let curIdx = cells.findIndex((c, idx) =>
    head <= c.to || idx === cells.length - 1 ? head <= c.to + 1 : false
  );
  if (curIdx < 0) curIdx = dir > 0 ? 0 : cells.length - 1;

  const nextIdx = curIdx + dir;
  if (nextIdx >= 0 && nextIdx < cells.length) {
    const target = cells[nextIdx];
    view.dispatch({
      selection: { anchor: target.from, head: target.to },
      scrollIntoView: true,
      userEvent: "select",
    });
    return true;
  }

  // Move across rows (skipping the delimiter row at tbl.startLine + 1)
  let targetLn = curLine.number + dir;
  if (targetLn === tbl.startLine + 1) targetLn += dir;

  if (targetLn >= tbl.startLine && targetLn <= tbl.endLine) {
    const rowLine = state.doc.line(targetLn);
    const rowCells = getGfmTableCellRanges(rowLine.from, rowLine.text);
    if (rowCells.length > 0) {
      const pick = dir > 0 ? rowCells[0] : rowCells[rowCells.length - 1];
      view.dispatch({
        selection: { anchor: pick.from, head: pick.to },
        scrollIntoView: true,
        userEvent: "select",
      });
      return true;
    }
  }

  // Pressing Tab on the last cell of the last row appends a new empty row
  if (dir > 0 && curLine.number === tbl.endLine) {
    const colCount = Math.max(1, cells.length);
    const emptyRow = `\n| ${Array.from({ length: colCount }, () => " ").join(" | ")} |`;
    const insertFrom = curLine.to;
    const firstCellCursor = insertFrom + 3; // after `\n| `
    view.dispatch({
      changes: { from: insertFrom, to: insertFrom, insert: emptyRow },
      selection: { anchor: firstCellCursor },
      scrollIntoView: true,
      userEvent: "input",
    });
    return true;
  }
  return false;
}

function resolveEnclosingFenceLang(state: EditorState, lineNumber: number): string {
  const scan = getIRDocScan(state.doc);
  if ((scan.fenceState[lineNumber] || 0) !== 2) return "";
  for (let ln = lineNumber - 1; ln >= 1; ln--) {
    if (scan.fenceState[ln] === 1) {
      const open = matchFenceOpen(state.doc.line(ln).text);
      return (open?.lang || scan.fenceAutoLang.get(ln) || "code").toLowerCase();
    }
    if (scan.fenceState[ln] !== 2) break;
  }
  return "code";
}

function toggleEditorComment(view: EditorView, cssOnly = false): boolean {
  const { state } = view;
  if (state.readOnly) return false;
  const sel = state.selection.main;
  const startLn = state.doc.lineAt(sel.from).number;
  const endLn =
    sel.to > sel.from && state.doc.lineAt(sel.to).from === sel.to
      ? Math.max(startLn, state.doc.lineAt(sel.to).number - 1)
      : state.doc.lineAt(sel.to).number;

  let blockPair: [string, string] | null = null;
  let lineTok = "";
  if (cssOnly) {
    blockPair = ["/* ", " */"];
  } else {
    const fLang = resolveEnclosingFenceLang(state, startLn);
    if (!fLang || fLang === "html" || fLang === "xml" || fLang === "svg" || fLang === "md" || fLang === "markdown") {
      blockPair = ["<!-- ", " -->"];
    } else if (fLang === "css") {
      blockPair = ["/* ", " */"];
    } else if (HASH_COMMENT_LANGS.has(fLang)) {
      lineTok = "#";
    } else if (DASH_COMMENT_LANGS.has(fLang)) {
      lineTok = "--";
    } else {
      lineTok = "//";
    }
  }

  if (lineTok) {
    const lines: Array<{ from: number; text: string }> = [];
    let allCommented = true;
    let hasNonEmpty = false;
    let minIndent = Infinity;
    const tokEsc = lineTok.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    const checkRe = new RegExp(`^([ \\t]*)${tokEsc} ?`);
    for (let ln = startLn; ln <= endLn; ln++) {
      const l = state.doc.line(ln);
      lines.push({ from: l.from, text: l.text });
      if (!l.text.trim()) continue;
      hasNonEmpty = true;
      const leadLen = /^[ \t]*/.exec(l.text)?.[0].length ?? 0;
      if (leadLen < minIndent) minIndent = leadLen;
      if (!checkRe.test(l.text)) allCommented = false;
    }
    if (!hasNonEmpty) {
      const l = state.doc.line(startLn);
      const ins = `${lineTok} `;
      view.dispatch({
        changes: { from: l.from, to: l.from, insert: ins },
        selection: { anchor: l.from + ins.length },
        userEvent: "input",
      });
      return true;
    }
    const changes: Array<{ from: number; to: number; insert: string }> = [];
    for (const l of lines) {
      if (!l.text.trim()) continue;
      if (allCommented) {
        const m = checkRe.exec(l.text);
        if (m) {
          changes.push({ from: l.from + m[1].length, to: l.from + m[0].length, insert: "" });
        }
      } else {
        const pos = l.from + (Number.isFinite(minIndent) ? minIndent : 0);
        changes.push({ from: pos, to: pos, insert: `${lineTok} ` });
      }
    }
    if (changes.length > 0) view.dispatch({ changes, userEvent: "input" });
    return true;
  }

  const [openTok, closeTok] = blockPair!;
  const openTrim = openTok.trim();
  const closeTrim = closeTok.trim();
  if (!sel.empty && startLn === endLn) {
    const raw = state.sliceDoc(sel.from, sel.to);
    if (raw.startsWith(openTrim) && raw.endsWith(closeTrim) && raw.length >= openTrim.length + closeTrim.length) {
      const inner = raw.slice(openTrim.length, raw.length - closeTrim.length).replace(/^ | $/g, "");
      view.dispatch({
        changes: { from: sel.from, to: sel.to, insert: inner },
        selection: { anchor: sel.from, head: sel.from + inner.length },
        userEvent: "input",
      });
      return true;
    }
  }
  const changes: Array<{ from: number; to: number; insert: string }> = [];
  for (let ln = startLn; ln <= endLn; ln++) {
    const l = state.doc.line(ln);
    const trimmed = l.text.trim();
    if (!trimmed && startLn !== endLn) continue;
    const lead = /^[ \t]*/.exec(l.text)?.[0] || "";
    const bodyFrom = l.from + lead.length;
    const bodyTo = l.to - trailingHorizWsLen(l.text);
    if (trimmed.startsWith(openTrim) && trimmed.endsWith(closeTrim) && trimmed.length >= openTrim.length + closeTrim.length) {
      const inner = trimmed.slice(openTrim.length, trimmed.length - closeTrim.length).replace(/^ | $/g, "");
      changes.push({ from: bodyFrom, to: bodyTo, insert: inner });
    } else {
      changes.push({ from: bodyFrom, to: bodyTo, insert: `${openTok}${trimmed}${closeTok}` });
    }
  }
  if (changes.length > 0) view.dispatch({ changes, userEvent: "input" });
  return true;
}

const CODE_AUTO_PAIRS: Record<string, string> = {
  "(": ")",
  "[": "]",
  "{": "}",
  '"': '"',
  "'": "'",
  "`": "`",
};

const MD_WRAP_PAIRS: Record<string, string> = {
  ...CODE_AUTO_PAIRS,
  "*": "*",
  "~": "~",
  "=": "=",
  "$": "$",
};

const CLOSING_SKIP_CHARS = new Set([")", "]", "}", '"', "'", "`"]);

function buildSmartTypingExt(cssOnly = false): Extension {
  return EditorView.inputHandler.of((view, from, to, text) => {
    if (view.composing || text.length !== 1 || view.state.readOnly || view.state.selection.ranges.length !== 1) {
      return false;
    }
    const doc = view.state.doc;
    const line = doc.lineAt(from);
    const inCode = cssOnly || isInsideFencedCode(view.state, line.number);

    // 1. Wrap non-empty single-line selection with matching pair instead of overwriting
    if (from < to && to <= line.to) {
      const closeCh = (inCode ? CODE_AUTO_PAIRS : MD_WRAP_PAIRS)[text];
      if (closeCh) {
        const selected = view.state.sliceDoc(from, to);
        view.dispatch({
          changes: { from, to, insert: `${text}${selected}${closeCh}` },
          selection: { anchor: from + 1, head: to + 1 },
          userEvent: "input.type",
        });
        return true;
      }
      return false;
    }

    // 2. Inside fenced code / CSS editor: skip-over closing delimiter & auto-close opening delimiter
    if (from === to && inCode) {
      const nextCh = from < line.to ? view.state.sliceDoc(from, from + 1) : "";
      const prevCh = from > line.from ? view.state.sliceDoc(from - 1, from) : "";
      if (CLOSING_SKIP_CHARS.has(text) && nextCh === text && prevCh !== "\\") {
        if ((text === '"' || text === "'" || text === "`") && prevCh === text) {
          // Empty pair "|": step out cleanly
          view.dispatch({ selection: { anchor: from + 1 }, userEvent: "select" });
          return true;
        }
        if (text === ")" || text === "]" || text === "}") {
          view.dispatch({ selection: { anchor: from + 1 }, userEvent: "select" });
          return true;
        }
      }
      const closeCh = CODE_AUTO_PAIRS[text];
      if (closeCh && (!nextCh || /^[\s)\]};,:>]$/.test(nextCh))) {
        if ((text === '"' || text === "'" || text === "`") && /[A-Za-z0-9_\\]/.test(prevCh)) {
          return false;
        }
        view.dispatch({
          changes: { from, to, insert: `${text}${closeCh}` },
          selection: { anchor: from + 1 },
          userEvent: "input.type",
        });
        return true;
      }
    }
    return false;
  });
}

function deleteAutoPair(view: EditorView, pos: number, lineTo: number): boolean {
  if (pos >= lineTo) return false;
  const { state } = view;
  if (CODE_AUTO_PAIRS[state.sliceDoc(pos - 1, pos)] === state.sliceDoc(pos, pos + 1)) {
    view.dispatch({
      changes: { from: pos - 1, to: pos + 1, insert: "" },
      selection: { anchor: pos - 1 },
      userEvent: "delete.backward",
    });
    return true;
  }
  return false;
}

function expandIndentedBracesOnEnter(view: EditorView): boolean {
  const { state } = view;
  if (state.readOnly || state.selection.ranges.length !== 1 || !state.selection.main.empty) return false;
  const pos = state.selection.main.head;
  const line = state.doc.lineAt(pos);
  if (pos <= line.from || pos >= line.to) return false;
  const prev = state.sliceDoc(pos - 1, pos);
  const next = state.sliceDoc(pos, pos + 1);
  if ((prev === "{" && next === "}") || (prev === "[" && next === "]") || (prev === "(" && next === ")")) {
    const indent = /^[ \t]*/.exec(line.text)?.[0] || "";
    const innerLine = `\n${indent}  `;
    const closeLine = `\n${indent}`;
    view.dispatch({
      changes: { from: pos, to: pos, insert: `${innerLine}${closeLine}` },
      selection: { anchor: pos + innerLine.length },
      scrollIntoView: true,
      userEvent: "input",
    });
    return true;
  }
  return false;
}

const markdownEditingKeymap: KeyBinding[] = [
  {
    key: "Tab",
    run: (view) => {
      if (navigateGfmTableCell(view, 1)) return true;
      const { state } = view;
      if (state.readOnly) return false;
      const sel = state.selection.main;
      const fromLn = state.doc.lineAt(sel.from).number;
      const toLn = state.doc.lineAt(sel.to).number;
      if (fromLn !== toLn) return indentTwoSpaces(view);
      const line = state.doc.line(fromLn);
      if (!isInsideFencedCode(state, fromLn) && /^\s*(?:[-*+]|\d+[.)])\s/.test(line.text)) {
        return indentTwoSpaces(view);
      }
      if (!sel.empty) return indentTwoSpaces(view);
      view.dispatch({
        changes: { from: sel.from, to: sel.to, insert: "  " },
        selection: { anchor: sel.from + 2 },
        userEvent: "input",
      });
      return true;
    },
    shift: (view) => {
      if (navigateGfmTableCell(view, -1)) return true;
      return dedentTwoSpaces(view);
    },
  },
  {
    key: "Enter",
    run(view: EditorView): boolean {
      const { state } = view;
      if (state.readOnly || state.selection.ranges.length !== 1 || !state.selection.main.empty) {
        return false;
      }
      const pos = state.selection.main.head;
      const line = state.doc.lineAt(pos);
      if (isInsideFencedCode(state, line.number)) {
        return expandIndentedBracesOnEnter(view);
      }

      const beforeCursor = line.text.slice(0, pos - line.from);
      const afterCursor = line.text.slice(pos - line.from);
      const m = mdLinePrefixRe.exec(beforeCursor);
      if (!m) return false;
      const [, quotePrefix, bullet, taskBox, ordNum, ordDelim, restBefore] = m;
      const hasMarker = Boolean(quotePrefix.trim() || bullet || ordNum);
      if (!hasMarker) return false;

      // Empty list item / empty blockquote line -> step out one nesting level or exit block cleanly
      if (!restBefore.trim() && !afterCursor.trim()) {
        if ((bullet || ordNum) && quotePrefix.trim()) {
          view.dispatch({
            changes: { from: line.from, to: line.to, insert: quotePrefix },
            selection: { anchor: line.from + quotePrefix.length },
            userEvent: "input",
          });
          return true;
        }
        const qCount = (quotePrefix.match(/>/g) || []).length;
        if (qCount >= 2) {
          const outerQuote = quotePrefix.replace(/>\s*$/, "");
          view.dispatch({
            changes: { from: line.from, to: line.to, insert: outerQuote },
            selection: { anchor: line.from + outerQuote.length },
            userEvent: "input",
          });
          return true;
        }
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
      if (pos === line.from) return false;
      if (isInsideFencedCode(state, line.number)) {
        return deleteAutoPair(view, pos, line.to);
      }
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
    const rawText = event.clipboardData?.getData("text/plain")?.trim() || "";
    const text = safeHref(cleanClipboardUrl(rawText, "href"), {
      allowRelative: false,
      allowHash: false,
      allowMailto: false,
    });
    if (!text || !/^https?:\/\//i.test(text)) return false;
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

// Shared Multi-Mirror Vendor Loader for KaTeX & Mermaid across Editor Modes (ir, sv, wysiwyg)
const DEFAULT_VENDOR_CDN_BASES = [
  "https://cdn.jsdelivr.net/npm",
  "https://fastly.jsdelivr.net/npm",
  "https://unpkg.com",
];
function normalizeVendorCDNBase(raw: string): string | null {
  const trimmed = trimEndChars(raw.trim().replace(/[<>"'`\s]+/g, ""), "/");
  if (!trimmed) return null;
  if (trimmed.startsWith("/") && !trimmed.startsWith("//") && !trimmed.startsWith("/\\")) {
    return encodeURI(trimmed);
  }
  if (trimmed.startsWith("https://") || trimmed.startsWith("http://")) {
    try {
      const u = new URL(trimmed);
      if (u.protocol === "https:" || u.protocol === "http:") {
        return encodeURI(trimEndChars(u.origin + u.pathname, "/"));
      }
    } catch {
      return null;
    }
  }
  return null;
}

function getVendorCDNBases(): string[] {
  const out: string[] = [];
  const custom = normalizeVendorCDNBase(readBootConfig().vendor_cdn_prefix || "");
  if (custom) {
    out.push(custom);
  }
  for (const base of DEFAULT_VENDOR_CDN_BASES) {
    if (!out.includes(base)) out.push(base);
  }
  return out;
}
const vendorScriptCache = new Map<string, Promise<void>>();
let editorKatexPromise: Promise<any> | null = null;
let editorMermaidPromise: Promise<any> | null = null;
let editorMermaidSeq = 0;

function loadEditorVendorStyle(id: string, subPath: string): Promise<void> {
  if (typeof document === "undefined" || document.getElementById(id)) return Promise.resolve();
  const clean = subPath.replace(/^\/+/, "");
  const bases = getVendorCDNBases();
  return new Promise((resolve) => {
    const attempt = (idx: number) => {
      if (idx >= bases.length) {
        resolve();
        return;
      }
      const link = document.createElement("link");
      link.id = id;
      link.rel = "stylesheet";
      link.href = `${bases[idx]}/${clean}`;
      link.onload = () => resolve();
      link.onerror = () => {
        link.remove();
        attempt(idx + 1);
      };
      document.head.appendChild(link);
    };
    attempt(0);
  });
}

function loadEditorVendorScript(subPath: string): Promise<void> {
  const clean = subPath.replace(/^\/+/, "");
  const existing = vendorScriptCache.get(clean);
  if (existing) return existing;
  const bases = getVendorCDNBases();
  const p = new Promise<void>((resolve, reject) => {
    const attempt = (idx: number) => {
      if (idx >= bases.length) {
        vendorScriptCache.delete(clean);
        reject(new Error(`vendor script load failed: ${clean}`));
        return;
      }
      const s = document.createElement("script");
      let settled = false;
      const timer = window.setTimeout(() => {
        if (settled) return;
        settled = true;
        s.remove();
        attempt(idx + 1);
      }, 7000);
      s.src = `${bases[idx]}/${clean}`;
      s.async = true;
      s.onload = () => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timer);
        resolve();
      };
      s.onerror = () => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timer);
        s.remove();
        attempt(idx + 1);
      };
      document.head.appendChild(s);
    };
    attempt(0);
  });
  vendorScriptCache.set(clean, p);
  return p;
}

function ensureEditorKaTeX(): Promise<any> {
  const w = window as any;
  if (w.katex && typeof w.katex.render === "function") return Promise.resolve(w.katex);
  if (!editorKatexPromise) {
    void loadEditorVendorStyle("volo-katex-css", "katex@0.16.11/dist/katex.min.css");
    editorKatexPromise = loadEditorVendorScript("katex@0.16.11/dist/katex.min.js")
      .then(() => (window as any).katex)
      .catch((err) => {
        editorKatexPromise = null;
        throw err;
      });
  }
  return editorKatexPromise;
}

function ensureEditorMermaid(): Promise<any> {
  const w = window as any;
  if (w.mermaid && typeof w.mermaid.render === "function") return Promise.resolve(w.mermaid);
  if (!editorMermaidPromise) {
    editorMermaidPromise = loadEditorVendorScript("mermaid@10/dist/mermaid.min.js")
      .then(() => (window as any).mermaid)
      .catch((err) => {
        editorMermaidPromise = null;
        throw err;
      });
  }
  return editorMermaidPromise;
}

let mermaidThemeObserverInstalled = false;
let lastMermaidThemeDark: boolean | null = null;

function isEffectiveMermaidDark(): boolean {
  if (typeof document === "undefined") return false;
  const attr = document.documentElement.getAttribute("data-theme");
  if (attr === "dark") return true;
  if (attr === "light") return false;
  return typeof window !== "undefined" && Boolean(window.matchMedia?.("(prefers-color-scheme: dark)").matches);
}

function ensureMermaidThemeObserver(): void {
  if (mermaidThemeObserverInstalled || typeof document === "undefined") return;
  mermaidThemeObserverInstalled = true;
  lastMermaidThemeDark = isEffectiveMermaidDark();
  let rafId = 0;
  const checkThemeChange = () => {
    if (rafId) cancelAnimationFrame(rafId);
    rafId = requestAnimationFrame(() => {
      rafId = 0;
      const nextDark = isEffectiveMermaidDark();
      if (nextDark === lastMermaidThemeDark) return;
      lastMermaidThemeDark = nextDark;
      const targets = document.querySelectorAll<HTMLElement>("[data-mermaid-code]");
      targets.forEach((el) => {
        const code = el.getAttribute("data-mermaid-code") || "";
        if (code) renderMermaidSvg(code, el);
      });
    });
  };
  const mo = new MutationObserver(checkThemeChange);
  mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  const mql = window.matchMedia?.("(prefers-color-scheme: dark)");
  mql?.addEventListener?.("change", checkThemeChange);
}

function renderMermaidSvg(code: string, target: HTMLElement, onReady?: () => void): void {
  const trimmed = code.trim();
  if (!trimmed) {
    target.removeAttribute("data-mermaid-code");
    target.textContent = "";
    return;
  }
  ensureMermaidThemeObserver();
  target.setAttribute("data-mermaid-code", trimmed);
  if (!target.querySelector("svg")) {
    target.textContent = trimmed;
  }
  target.classList.add("cm-ir-mermaid-pending");
  void ensureEditorMermaid()
    .then(async (mermaid) => {
      if (!mermaid || !target.isConnected) return;
      const isDark = isEffectiveMermaidDark();
      mermaid.initialize({
        startOnLoad: false,
        theme: isDark ? "dark" : "default",
        securityLevel: "strict",
      });
      const id = `volo-ed-mmd-${++editorMermaidSeq}`;
      const res = await mermaid.render(id, trimmed);
      if (!target.isConnected) return;
      target.classList.remove("cm-ir-mermaid-pending");
      if (res && typeof res.svg === "string") {
        const doc = new DOMParser().parseFromString(res.svg, "image/svg+xml");
        const svgEl = doc.querySelector("svg");
        if (svgEl) {
          svgEl.querySelectorAll("script").forEach((s) => s.remove());
          svgEl.querySelectorAll("*").forEach((el) => {
            for (const attr of Array.from(el.attributes)) {
              const name = attr.name.toLowerCase();
              if (name.startsWith("on")) {
                el.removeAttribute(attr.name);
              } else if (name === "href" || name === "xlink:href") {
                if (!safeHref(attr.value, { allowMailto: false })) {
                  el.removeAttribute(attr.name);
                }
              }
            }
          });
          target.replaceChildren(document.importNode(svgEl, true));
          onReady?.();
        }
      }
    })
    .catch(() => {
      if (target.isConnected) {
        target.classList.remove("cm-ir-mermaid-pending");
      }
    });
}

function mkEl<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  cls?: string,
  text?: string
): HTMLElementTagNameMap[K] {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text !== undefined) el.textContent = text;
  return el;
}

function triggerCopyBtnFeedback(
  btn: HTMLButtonElement,
  text: string,
  copyTxt: string,
  copiedTxt: string,
  onError?: () => void
): void {
  void copyTextToClipboard(text)
    .then(() => {
      const prevTimer = (btn as any)._copyTimer;
      if (prevTimer) window.clearTimeout(prevTimer);
      btn.setAttribute("data-copied", "true");
      btn.textContent = `✓ ${copiedTxt}`;
      (btn as any)._copyTimer = window.setTimeout(() => {
        (btn as any)._copyTimer = 0;
        if (btn.isConnected) {
          btn.removeAttribute("data-copied");
          btn.textContent = copyTxt;
        }
      }, 1400);
    })
    .catch(() => {
      onError?.();
    });
}

function populateCodeLangSelect(
  select: HTMLSelectElement,
  explicitLang: string,
  autoLang: string,
  plainTxt: string,
  markSelectedAttr = false
): void {
  const seen = new Set<string>();
  for (const item of CODE_BLOCK_LANGS) {
    seen.add(item.value);
    const label =
      item.value === ""
        ? autoLang
          ? `${autoLang.toUpperCase()} · auto`
          : plainTxt
        : item.label;
    const opt = mkEl("option", undefined, label);
    opt.value = item.value;
    if (markSelectedAttr && item.value === explicitLang) {
      opt.selected = true;
      opt.setAttribute("selected", "");
    }
    select.appendChild(opt);
  }
  if (explicitLang && !seen.has(explicitLang)) {
    const custom = mkEl("option", undefined, explicitLang.toUpperCase());
    custom.value = explicitLang;
    if (markSelectedAttr) {
      custom.selected = true;
      custom.setAttribute("selected", "");
    }
    select.insertBefore(custom, select.firstChild);
  }
  select.value = explicitLang;
}

const codeHeadBoundRoots = new WeakSet<HTMLElement>();
const codeHeadHostOpts = new WeakMap<
  HTMLElement,
  { copyLabel: string; copiedLabel: string; onLangChange?: () => void }
>();

export function enhanceRenderedPreviewDOM(
  root: HTMLElement,
  opts: {
    isWysiwyg?: boolean;
    copyLabel?: string;
    copiedLabel?: string;
    plainLabel?: string;
    onLangChange?: () => void;
  } = {}
): void {
  // 0. Ensure external images bypass strict hotlink Referer checks and fix &amp;-escaped URLs
  root.querySelectorAll<HTMLImageElement>("img").forEach((img) => {
    const rawSrc = img.getAttribute("src") || "";
    const cleaned = cleanClipboardUrl(rawSrc, "src");
    if (!cleaned) {
      img.removeAttribute("src");
      return;
    }
    if (cleaned !== rawSrc) {
      img.setAttribute("src", cleaned);
    }
    if (!/^(?:\/(?!\/)|data:|blob:)/i.test(cleaned)) {
      img.referrerPolicy = "no-referrer";
    }
  });

  // 1. KaTeX math blocks & inline spans (preserving data-math-src for lossless WYSIWYG round-tripping)
  const mathNodes = Array.from(root.querySelectorAll<HTMLElement>(".volo-math-block, .volo-math-inline"));
  if (mathNodes.length > 0) {
    for (const el of mathNodes) {
      if (!el.hasAttribute("data-math-src")) {
        const raw = (el.textContent || "").trim();
        const isBlock = el.classList.contains("volo-math-block");
        let stripped = raw;
        if (isBlock) {
          if (stripped.startsWith("$$") || stripped.startsWith("\\[")) stripped = stripped.slice(2);
          if (stripped.endsWith("$$") || stripped.endsWith("\\]")) stripped = stripped.slice(0, -2);
          stripped = stripped.trim();
        } else {
          stripped = raw.replace(/^\$|\$$/g, "");
        }
        el.setAttribute("data-math-src", stripped);
        el.setAttribute("data-math-display", isBlock ? "1" : "0");
      }
      if (opts.isWysiwyg) {
        el.contentEditable = "false";
      }
    }
    void ensureEditorKaTeX()
      .then((katex) => {
        if (!katex || !root.isConnected) return;
        for (const el of mathNodes) {
          if (!el.isConnected || el.getAttribute("data-math-rendered") === "1") continue;
          const expr = el.getAttribute("data-math-src") || "";
          const displayMode = el.getAttribute("data-math-display") === "1";
          try {
            katex.render(expr, el, { displayMode, throwOnError: false });
            el.setAttribute("data-math-rendered", "1");
          } catch {
            /* keep raw math text on invalid syntax */
          }
        }
      })
      .catch(() => {});
  }

  // 2. Mermaid diagrams (preserving data-mermaid-src for lossless WYSIWYG round-tripping)
  const mermaidNodes = Array.from(root.querySelectorAll<HTMLElement>("pre.mermaid, div.mermaid"));
  for (const el of mermaidNodes) {
    const raw = el.getAttribute("data-mermaid-src") ?? (el.textContent || "");
    el.setAttribute("data-mermaid-src", raw);
    if (opts.isWysiwyg) {
      el.contentEditable = "false";
    }
    if (!el.querySelector("svg")) {
      renderMermaidSvg(raw, el);
    }
  }

  // 2b. Protected directive cards & callout titles in WYSIWYG
  if (opts.isWysiwyg) {
    for (const el of Array.from(
      root.querySelectorAll<HTMLElement>("[data-volo-md-b64], [data-md-toc], [data-md-media-href], .volo-callout__title")
    )) {
      el.contentEditable = "false";
    }
  }

  // 3. Split Preview visual boundary markers (<!--more-->, :::lock)
  if (!opts.isWysiwyg) {
    const mkMarker = (txt: string) => {
      const m = mkEl("div", "volo-pv-marker");
      m.appendChild(mkEl("span", undefined, txt));
      return m;
    };
    for (const p of Array.from(root.querySelectorAll<HTMLElement>("p"))) {
      if ((p.textContent || "").trim() === "§VOLO_MORE_MARKER§") {
        p.replaceWith(mkMarker("<!--more-->"));
      }
    }
    for (const lk of Array.from(root.querySelectorAll<HTMLElement>(".volo-lock-block--unlocked"))) {
      if (lk.querySelector(":scope > .volo-pv-marker")) continue;
      lk.prepend(mkMarker(`:::lock (${lk.getAttribute("data-lock-type") || "password"})`));
    }
  }

  // 4. Unified Code Block Header (.volo-code-block + .volo-code-head) in Split Preview & WYSIWYG
  const copyTxt = opts.copyLabel || "Copy";
  const copiedTxt = opts.copiedLabel || "Copied";
  const plainTxt = opts.plainLabel || "CODE";
  codeHeadHostOpts.set(root, { copyLabel: copyTxt, copiedLabel: copiedTxt, onLangChange: opts.onLangChange });
  if (!codeHeadBoundRoots.has(root)) {
    codeHeadBoundRoots.add(root);
    root.addEventListener("click", (ev) => {
      const btn = (ev.target as HTMLElement | null)?.closest(".volo-copy-btn") as HTMLButtonElement | null;
      if (!btn || !root.contains(btn)) return;
      ev.preventDefault();
      ev.stopPropagation();
      const host = btn.closest("figure.volo-codeblock, .volo-code-block") as HTMLElement | null;
      const pre = host?.querySelector("pre") as HTMLElement | null;
      if (!pre) return;
      const cfg = codeHeadHostOpts.get(root);
      triggerCopyBtnFeedback(
        btn,
        extractPreCodeLines(pre),
        cfg?.copyLabel || "Copy",
        cfg?.copiedLabel || "Copied",
        () => showToast(t("api.err_network"), "err")
      );
    });
    root.addEventListener("change", (ev) => {
      const target = ev.target;
      if (!(target instanceof HTMLSelectElement) || !target.classList.contains("volo-code-lang-select") || !root.contains(target)) {
        return;
      }
      ev.stopPropagation();
      const host = target.closest("figure.volo-codeblock, .volo-code-block") as HTMLElement | null;
      const nextLang = target.value.trim().toLowerCase();
      if (host) {
        if (nextLang) host.setAttribute("data-lang", nextLang);
        else host.removeAttribute("data-lang");
      }
      for (const opt of Array.from(target.options)) {
        const match = opt.value === nextLang;
        opt.selected = match;
        if (match) opt.setAttribute("selected", "");
        else opt.removeAttribute("selected");
      }
      codeHeadHostOpts.get(root)?.onLangChange?.();
    });
  }
  for (const pre of Array.from(root.querySelectorAll<HTMLElement>("pre:not(.mermaid)"))) {
    const fig = pre.closest("figure.volo-codeblock") as HTMLElement | null;
    const container = fig || pre.parentElement;
    if (fig && fig.querySelector(":scope > .volo-code-head")) continue;
    if (!fig && container?.classList.contains("volo-code-block")) continue;

    const codeEl = pre.querySelector("code");
    const cls = `${pre.className || ""} ${codeEl?.className || ""}`;
    const langMatch = /(?:language|lang)-([a-zA-Z0-9_+#.-]+)/.exec(cls);
    const explicitLang = (
      fig?.getAttribute("data-lang") ||
      pre.getAttribute("data-lang") ||
      codeEl?.getAttribute("data-lang") ||
      (langMatch ? langMatch[1] : "")
    )
      .trim()
      .toLowerCase();
    const detectedLang = (
      fig?.getAttribute("data-detected-lang") ||
      pre.getAttribute("data-detected-lang") ||
      ""
    )
      .trim()
      .toLowerCase();

    let host: HTMLElement;
    if (fig) {
      fig.classList.add("volo-code-block");
      host = fig;
    } else {
      const wrap = mkEl("figure", "volo-codeblock volo-code-block");
      if (explicitLang) wrap.setAttribute("data-lang", explicitLang);
      if (detectedLang) wrap.setAttribute("data-detected-lang", detectedLang);
      pre.replaceWith(wrap);
      wrap.appendChild(pre);
      host = wrap;
    }

    const head = mkEl("div", "volo-code-head");
    head.contentEditable = "false";

    if (opts.isWysiwyg) {
      const select = mkEl("select", "volo-code-lang-select");
      select.setAttribute("aria-label", t("ed.code_lang_ph"));
      populateCodeLangSelect(select, explicitLang, detectedLang, plainTxt, true);
      head.appendChild(select);
    } else {
      const langSpan = mkEl("span", "volo-code-lang", (explicitLang || detectedLang || "code").toUpperCase());
      if (!explicitLang) langSpan.setAttribute("data-auto-lang", "1");
      head.appendChild(langSpan);
    }

    const btn = mkEl("button", "volo-copy-btn", copyTxt);
    btn.type = "button";
    head.appendChild(btn);
    host.insertBefore(head, host.firstChild);
  }
}

function formatDirectiveAttr(k: string, raw: unknown): string {
  const v = String(raw ?? "").replace(/[\r\n]+/g, " ").trim();
  if (!v.includes('"')) return `${k}="${v}"`;
  if (!v.includes("'")) return `${k}='${v}'`;
  return `${k}="${v.replace(/"/g, "'")}"`;
}

function escapeMdLinkLabel(label: string): string {
  return label.replace(/\\([[\]])|([[\]])/g, (_, esc: string | undefined, br: string | undefined) =>
    esc ? `\\${esc}` : `\\${br}`
  );
}

function parseMarkdownImageTarget(rawTarget: string): string {
  const s = rawTarget.trim();
  if (s.startsWith("<") && s.includes(">")) {
    return s.slice(1, s.indexOf(">")).trim();
  }
  const m = /^(\S+)(?:\s+(?:"[^"]*"|'[^']*'|\([^)]*\)))?\s*$/.exec(s);
  return m ? m[1] : s;
}

function resolveIRImageSrc(rawSrc: string): string {
  let s = rawSrc.trim();
  if (!s) return "";
  if (s.startsWith("./")) s = "/" + s.slice(2).replace(/^\/+/, "");
  else if (/^(?:image|images|uploads)\//i.test(s)) s = "/" + s;
  return cleanClipboardUrl(s, "src");
}

function isVideoMedia(mime?: string, url?: string): boolean {
  if ((mime || "").toLowerCase().startsWith("video/")) return true;
  const cleanUrl = (url || "").split(/[?#]/)[0].toLowerCase();
  return /\.(?:mp4|webm|mov|m4v|ogv)$/.test(cleanUrl);
}

function isAudioMedia(mime?: string, url?: string): boolean {
  if ((mime || "").toLowerCase().startsWith("audio/")) return true;
  const cleanUrl = (url || "").split(/[?#]/)[0].toLowerCase();
  return /\.(?:mp3|m4a|wav|ogg|flac|aac|opus|weba)$/.test(cleanUrl);
}

const irFocusEffect = StateEffect.define<boolean>();
const irImgDimReadyEffect = StateEffect.define<string>();
const irFocusedField = StateField.define<boolean>({
  create: () => false,
  update(val, tr) {
    for (const e of tr.effects) {
      if (e.is(irFocusEffect)) return e.value;
    }
    return val;
  },
});

interface IRImageSpan {
  lineNumber: number;
  spanFrom: number;
  spanTo: number;
  imgFrom: number;
  imgTo: number;
  resolvedSrc: string;
  rawTarget: string;
  alt: string;
  href: string;
}

interface IRTableBlock {
  kind: "html" | "md";
  startLine: number;
  endLine: number;
  from: number;
  to: number;
  raw: string;
}

interface IRMermaidBlock {
  startLine: number;
  endLine: number;
  from: number;
  to: number;
  code: string;
}

interface IRMathBlock {
  startLine: number;
  endLine: number;
  from: number;
  to: number;
  expr: string;
}

interface IRMultiLinkSpan {
  startLine: number;
  endLine: number;
  openFrom: number;
  openTo: number;
  closeFrom: number;
  closeTo: number;
}

interface IRDocScan {
  tables: IRTableBlock[];
  mermaids: IRMermaidBlock[];
  mathBlocks: IRMathBlock[];
  tableLines: Set<number>;
  multiLinks: IRMultiLinkSpan[];
  images: IRImageSpan[];
  fenceState: Uint8Array;
  fenceNoHash: Uint8Array;
  /** 0=generic, 1=c/go/js/ts/rs, 2=sh/py/yaml/nginx/toml, 3=json, 4=html/xml, 5=css, 6=diff, 7=sql */
  fenceFam: Uint8Array;
  /** 1 when line enters inside an unclosed multi-line block comment (`/*…* /` or `<!--…-->`) */
  fenceBlockCmt: Uint8Array;
  /** On fence open line (`fenceState === 1`), stores closing line number (or 0 if unclosed) */
  fenceEndLine: Int32Array;
  /** Auto-detected language name for unlabeled fences (`fenceStartLn -> lang`) */
  fenceAutoLang: Map<number, string>;
  calloutKind: Uint8Array;
  /** Nesting depth of `>` blockquote markers (`0..3`) */
  quoteDepth: Uint8Array;
  /** Bitmask on quote lines: `1` = first line of quote card, `2` = last line of quote card */
  quoteEdge: Uint8Array;
  /** 0 = none, 1 = center, 2 = right, 3 = left — lines inside a multi-line `<div align=…>` / `<center>` block */
  alignKind: Uint8Array;
  /** 1 on the standalone opening / closing tag lines of such a block (hidden off-cursor in IR mode) */
  alignTagLine: Uint8Array;
  /** 0 = none, 1 = opening `:::lock`, 2 = inner line, 3 = closing `:::` */
  lockState: Uint8Array;
}

function scanQuotedEnd(s: string, start: number, q: string): number {
  let i = start + 1;
  while (i < s.length) {
    if (s[i] === "\\") {
      i += 2;
      continue;
    }
    if (s[i++] === q) break;
  }
  return i;
}

function populateFenceBlockMeta(
  linesText: string[],
  startLn: number,
  endLn: number,
  fam: number,
  noHash: number,
  qDepth: number,
  fenceFam: Uint8Array,
  fenceNoHash: Uint8Array,
  fenceBlockCmt: Uint8Array
): void {
  const supportsHtmlCmt = fam === 4;
  const supportsLuaCmt = fam === 8;
  const supportsSlashCmt = fam === 0 || fam === 1 || fam === 3 || fam === 5 || fam === 7 || fam === 10 || fam === 12;
  let inBlock = false;

  for (let k = startLn; k <= endLn; k++) {
    fenceFam[k] = fam;
    fenceNoHash[k] = noHash;
    fenceBlockCmt[k] = inBlock ? 1 : 0;
    const rawLine = linesText[k];
    if (!rawLine || rawLine.length > 1600) continue;
    const s = qDepth > 0 ? stripQuotePrefix(rawLine, qDepth).stripped : rawLine;
    if (!s) continue;

    if (supportsHtmlCmt) {
      let i = 0;
      while (i < s.length) {
        if (inBlock) {
          const closeIdx = s.indexOf("-->", i);
          if (closeIdx < 0) break;
          inBlock = false;
          i = closeIdx + 3;
        } else {
          const openIdx = s.indexOf("<!--", i);
          if (openIdx < 0) break;
          inBlock = true;
          i = openIdx + 4;
        }
      }
    } else if (supportsLuaCmt) {
      let i = 0;
      while (i < s.length) {
        if (inBlock) {
          const closeIdx = s.indexOf("]]", i);
          if (closeIdx < 0) break;
          inBlock = false;
          i = closeIdx + 2;
          continue;
        }
        if (s[i] === "-" && s[i + 1] === "-") {
          if (s[i + 2] === "[" && s[i + 3] === "[") {
            inBlock = true;
            i += 4;
            continue;
          }
          break;
        }
        if (s[i] === '"' || s[i] === "'") {
          i = scanQuotedEnd(s, i, s[i]);
          continue;
        }
        i++;
      }
    } else if (supportsSlashCmt) {
      let i = 0;
      while (i < s.length) {
        if (inBlock) {
          const closeIdx = s.indexOf("*/", i);
          if (closeIdx < 0) break;
          inBlock = false;
          i = closeIdx + 2;
          continue;
        }
        const ch = s[i];
        const nx = s[i + 1];
        if (fam !== 12 && ch === "/" && nx === "/") break;
        if ((fam === 7 || fam === 12) && ch === "-" && nx === "-") break;
        if (!noHash && ch === "#" && nx !== "[" && (i === 0 || /\s/.test(s[i - 1]))) break;
        if (fam !== 12 && ch === "/" && nx === "*") {
          inBlock = true;
          i += 2;
          continue;
        }
        if (ch === '"' || ch === "'" || ch === "`") {
          if (ch === "'" && /[A-Za-z_]/.test(nx || "") && s[i + 2] !== "'") {
            i += 2;
            continue;
          }
          i = scanQuotedEnd(s, i, ch);
          continue;
        }
        i++;
      }
    }
  }
}

const CALLOUT_CODE_MAP: Record<string, number> = {
  note: 1,
  tip: 2,
  important: 3,
  warning: 4,
  caution: 5,
};
const CALLOUT_NAMES = ["", "note", "tip", "important", "warning", "caution"] as const;

const irDocScanCache = new WeakMap<EditorState["doc"], IRDocScan>();

function isGfmTableDelimiterLine(text: string): boolean {
  const trimmed = text.trim();
  if (!trimmed.includes("|")) return false;
  const hasOuterPipes = trimmed.startsWith("|") && trimmed.endsWith("|");
  let s = trimmed;
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|")) s = s.slice(0, -1);
  const parts = s.split("|");
  if (parts.length < 2 && !hasOuterPipes) return false;
  for (const p of parts) {
    if (!/^:?-+:?$/.test(p.trim())) return false;
  }
  return true;
}

function readHtmlAttr(openTag: string, attrName: string): string {
  const re = new RegExp(`\\b${attrName}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s"'>]+))`, "i");
  const m = re.exec(openTag);
  return m ? (m[1] ?? m[2] ?? m[3] ?? "").trim() : "";
}

function isLikelyInlineMath(expr: string): boolean {
  if (!expr) return false;
  if (/^\d[\d,.]*$/.test(expr) && expr.includes(",")) return false;
  if (/^\d/.test(expr) && !expr.includes("\\")) {
    if (/[\u3000-\u303f\u4e00-\u9fff\uff00-\uffef]/.test(expr)) return false;
  }
  return true;
}

function getIRDocScan(doc: EditorState["doc"]): IRDocScan {
  const cached = irDocScanCache.get(doc);
  if (cached) return cached;

  const tables: IRTableBlock[] = [];
  const mermaids: IRMermaidBlock[] = [];
  const mathBlocks: IRMathBlock[] = [];
  const tableLines = new Set<number>();
  const multiLinks: IRMultiLinkSpan[] = [];
  const images: IRImageSpan[] = [];

  const total = doc.lines;
  const fenceState = new Uint8Array(total + 1);
  const fenceNoHash = new Uint8Array(total + 1);
  const fenceFam = new Uint8Array(total + 1);
  const fenceBlockCmt = new Uint8Array(total + 1);
  const fenceEndLine = new Int32Array(total + 1);
  const fenceAutoLang = new Map<number, string>();
  const calloutKind = new Uint8Array(total + 1);
  const quoteDepth = new Uint8Array(total + 1);
  const quoteEdge = new Uint8Array(total + 1);
  const lockState = new Uint8Array(total + 1);
  const linesText = new Array<string>(total + 1);
  const lineFroms = new Int32Array(total + 1);
  linesText[0] = "";

  // Single O(N) linear pass via doc.iterLines() to populate line texts, offsets, CommonMark §4.5 fences & callouts
  let iterLn = 0;
  let curFrom = 0;
  let fenceChar = "";
  let fenceLen = 0;
  let fenceLang = "";
  let fenceQuoteDepth = 0;
  let curFenceNoHash = 0;
  let curFenceFam = 0;
  let fenceStartLn = -1;
  let activeCallout = 0;
  let inLockBlock = false;

  const finalizeFenceMeta = (openLn: number, bodyEndLn: number, closeLn: number, rawLang: string, qDepth: number) => {
    if (openLn <= 0) return;
    if (closeLn > openLn) fenceEndLine[openLn] = closeLn;
    if (bodyEndLn <= openLn || rawLang === "mermaid") return;
    let fam = curFenceFam;
    let noHash = curFenceNoHash;
    if (!rawLang) {
      const det = detectIRCodeLang(linesText, openLn + 1, bodyEndLn, qDepth);
      if (det.lang) {
        fenceAutoLang.set(openLn, det.lang);
        fam = det.fam;
        noHash = det.noHash;
      }
    }
    populateFenceBlockMeta(linesText, openLn + 1, bodyEndLn, fam, noHash, qDepth, fenceFam, fenceNoHash, fenceBlockCmt);
  };

  const cursor = doc.iterLines();
  while (!cursor.next().done) {
    iterLn++;
    const text = cursor.value;
    linesText[iterLn] = text;
    lineFroms[iterLn] = curFrom;
    curFrom += text.length + 1;

    if (fenceChar && fenceQuoteDepth > 0 && countQuotePrefix(text).depth < fenceQuoteDepth) {
      finalizeFenceMeta(fenceStartLn, iterLn - 1, 0, fenceLang, fenceQuoteDepth);
      fenceChar = "";
      fenceLen = 0;
      fenceLang = "";
      fenceQuoteDepth = 0;
      curFenceNoHash = 0;
      curFenceFam = 0;
      fenceStartLn = -1;
    }

    if (!fenceChar) {
      const openM = matchFenceOpen(text);
      if (openM) {
        fenceState[iterLn] = 1;
        fenceChar = openM.fenceChar;
        fenceLen = openM.fenceLen;
        fenceLang = openM.lang.toLowerCase();
        fenceQuoteDepth = openM.quoteDepth;
        curFenceNoHash = NO_HASH_COMMENT_LANGS.has(fenceLang) ? 1 : 0;
        curFenceFam = resolveFenceLangFamily(fenceLang);
        fenceStartLn = iterLn;
        if (openM.quoteDepth > 0) {
          quoteDepth[iterLn] = openM.quoteDepth;
          calloutKind[iterLn] = activeCallout;
        } else {
          activeCallout = 0;
        }
        if (inLockBlock) lockState[iterLn] = 2;
        continue;
      }
      if (!inLockBlock && /^\s*:::lock(?:\s|$)/i.test(text)) {
        lockState[iterLn] = 1;
        inLockBlock = true;
      } else if (inLockBlock && /^\s*:::\s*$/.test(text)) {
        lockState[iterLn] = 3;
        inLockBlock = false;
      } else if (inLockBlock) {
        lockState[iterLn] = 2;
      }
      const qm = /^(\s*(?:>\s?)+)(?:\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\](?:\s|$))?/i.exec(text);
      if (qm) {
        quoteDepth[iterLn] = Math.min(3, (qm[1].match(/>/g) || []).length);
        if (qm[2]) {
          activeCallout = CALLOUT_CODE_MAP[qm[2].toLowerCase()] || 0;
        }
        calloutKind[iterLn] = activeCallout;
      } else {
        activeCallout = 0;
      }
    } else if (matchFenceClose(text, fenceChar, fenceLen, fenceQuoteDepth)) {
      fenceState[iterLn] = 3;
      if (fenceQuoteDepth > 0) {
        quoteDepth[iterLn] = fenceQuoteDepth;
        calloutKind[iterLn] = activeCallout;
      }
      if (inLockBlock) lockState[iterLn] = 2;
      finalizeFenceMeta(fenceStartLn, iterLn - 1, iterLn, fenceLang, fenceQuoteDepth);
      if (fenceLang === "mermaid" && fenceStartLn > 0 && iterLn > fenceStartLn) {
        const openFrom = lineFroms[fenceStartLn];
        const closeTo = lineFroms[iterLn] + text.length;
        const rawLines: string[] = [];
        for (let k = fenceStartLn + 1; k <= iterLn - 1; k++) {
          rawLines.push(fenceQuoteDepth > 0 ? stripQuotePrefix(linesText[k], fenceQuoteDepth).stripped : linesText[k]);
        }
        const code = rawLines.join("\n");
        if (code.trim()) {
          mermaids.push({
            startLine: fenceStartLn,
            endLine: iterLn,
            from: openFrom,
            to: closeTo,
            code,
          });
          for (let k = fenceStartLn; k <= iterLn; k++) tableLines.add(k);
        }
      }
      fenceChar = "";
      fenceLen = 0;
      fenceLang = "";
      fenceQuoteDepth = 0;
      curFenceNoHash = 0;
      curFenceFam = 0;
      fenceStartLn = -1;
    } else {
      fenceState[iterLn] = 2;
      fenceNoHash[iterLn] = curFenceNoHash;
      fenceFam[iterLn] = curFenceFam;
      if (fenceQuoteDepth > 0) {
        quoteDepth[iterLn] = fenceQuoteDepth;
        calloutKind[iterLn] = activeCallout;
      }
      if (inLockBlock) lockState[iterLn] = 2;
    }
  }
  if (fenceChar && fenceStartLn > 0 && total > fenceStartLn) {
    finalizeFenceMeta(fenceStartLn, total, 0, fenceLang, fenceQuoteDepth);
  }
  for (let ln = 1; ln <= total; ln++) {
    if (quoteDepth[ln] === 0) continue;
    if (ln === 1 || quoteDepth[ln - 1] === 0 || calloutKind[ln - 1] !== calloutKind[ln]) {
      quoteEdge[ln] |= 1;
    }
    if (ln === total || quoteDepth[ln + 1] === 0 || calloutKind[ln + 1] !== calloutKind[ln]) {
      quoteEdge[ln] |= 2;
    }
  }

  // Multi-line alignment containers: `<div align=center>` … `</div>`, `<p align="right">` … `</p>`,
  // `<center>` … `</center>`, `<div style="text-align:center">` … (Solo / GitHub README style).
  const alignKind = new Uint8Array(total + 1);
  const alignTagLine = new Uint8Array(total + 1);
  for (let ln = 1; ln < total; ln++) {
    if (fenceState[ln] !== 0) continue;
    const text = linesText[ln];
    if (!text || text.length > 400 || !text.includes("<")) continue;
    const om = /^\s*<(div|p|center|section)\b([^<>]*)>\s*$/i.exec(text);
    if (!om) continue;
    const tag = om[1].toLowerCase();
    let kind = 0;
    if (tag === "center") {
      kind = 1;
    } else {
      const al = (
        readHtmlAttr(om[0], "align") ||
        (/text-align\s*:\s*(center|right|left)/i.exec(readHtmlAttr(om[0], "style"))?.[1] ?? "")
      ).toLowerCase();
      kind = al === "center" ? 1 : al === "right" ? 2 : al === "left" ? 3 : 0;
    }
    if (!kind) continue;
    const openRe = new RegExp(`<${tag}\\b[^<>]*>`, "gi");
    const closeRe = new RegExp(`</${tag}\\s*>`, "gi");
    let depth = 1;
    let endLn = -1;
    const limit = Math.min(total, ln + 400);
    for (let k = ln + 1; k <= limit; k++) {
      if (fenceState[k] !== 0) continue;
      const lt = linesText[k];
      if (!lt || !lt.includes("<")) continue;
      depth += (lt.match(openRe) || []).length;
      depth -= (lt.match(closeRe) || []).length;
      if (depth <= 0) {
        endLn = k;
        break;
      }
    }
    if (endLn < 0) continue;
    alignTagLine[ln] = 1;
    if (/^\s*<\/[a-z]+\s*>\s*$/i.test(linesText[endLn])) alignTagLine[endLn] = 1;
    // Later (inner) containers overwrite outer ones, so nested alignment wins.
    for (let k = ln + 1; k < endLn; k++) alignKind[k] = kind;
  }

  // Second O(N) pass over non-fenced lines using direct array lookups + watermarks (zero B-tree traversal)
  let noCloseMathUntil = 0;
  let noCloseBrkMathUntil = 0;
  let noCloseTableUntil = 0;

  for (let ln = 1; ln <= total; ln++) {
    if (fenceState[ln] !== 0) continue;
    const text = linesText[ln];
    if (!text) continue;
    const lineFrom = lineFroms[ln];
    const lineTo = lineFrom + text.length;

    // 0. Display math block ($$...$$ or \[...\] single-line or multi-line)
    if (text.includes("$$") || text.includes("\\[")) {
      const singleMath = /^\s*(?:\$\$([^\n$]+)\$\$|\\\[([^\n]+?)\\\])\s*$/.exec(text);
      const singleExpr = (singleMath?.[1] || singleMath?.[2] || "").trim();
      if (singleExpr) {
        mathBlocks.push({
          startLine: ln,
          endLine: ln,
          from: lineFrom,
          to: lineTo,
          expr: singleExpr,
        });
        tableLines.add(ln);
        continue;
      }
      const isDollarOpen = /^\s*\$\$\s*$/.test(text);
      const isBrkOpen = !isDollarOpen && /^\s*\\\[\s*$/.test(text);
      if ((isDollarOpen && ln >= noCloseMathUntil) || (isBrkOpen && ln >= noCloseBrkMathUntil)) {
        if (ln < total) {
          const closeRe = isDollarOpen ? /^\s*\$\$\s*$/ : /^\s*\\\]\s*$/;
          let endLn = -1;
          let hitFence = false;
          const limit = Math.min(total, ln + 120);
          for (let k = ln + 1; k <= limit; k++) {
            if (fenceState[k] !== 0) {
              hitFence = true;
              break;
            }
            if (closeRe.test(linesText[k])) {
              endLn = k;
              break;
            }
          }
          if (endLn > ln + 1) {
            const endTo = lineFroms[endLn] + linesText[endLn].length;
            const expr = doc.sliceString(lineFroms[ln + 1], lineFroms[endLn - 1] + linesText[endLn - 1].length).trim();
            if (expr) {
              mathBlocks.push({
                startLine: ln,
                endLine: endLn,
                from: lineFrom,
                to: endTo,
                expr,
              });
              for (let k = ln; k <= endLn; k++) tableLines.add(k);
              ln = endLn;
              continue;
            }
          } else if (endLn < 0 && !hitFence) {
            if (isDollarOpen) noCloseMathUntil = limit;
            else noCloseBrkMathUntil = limit;
          }
        }
      }
    }

    // 1. HTML <table>...</table> block (single-line or multi-line, tracking nested <table> depth)
    if (/^\s*<table\b/i.test(text) && ln >= noCloseTableUntil) {
      let endLn = -1;
      let hitFence = false;
      let tableDepth = 0;
      const limit = Math.min(total, ln + 500);
      for (let k = ln; k <= limit; k++) {
        if (k > ln && fenceState[k] !== 0) {
          hitFence = true;
          break;
        }
        const lt = linesText[k];
        if (lt.includes("<")) {
          const opens = lt.match(/<table\b/gi);
          const closes = lt.match(/<\/table>/gi);
          if (opens) tableDepth += opens.length;
          if (closes) tableDepth -= closes.length;
          if (tableDepth <= 0 && closes) {
            endLn = k;
            break;
          }
        }
      }
      if (endLn >= ln) {
        const endTo = lineFroms[endLn] + linesText[endLn].length;
        const raw = doc.sliceString(lineFrom, endTo);
        if (/^\s*<table\b[\s\S]*<\/table>\s*$/i.test(raw)) {
          tables.push({
            kind: "html",
            startLine: ln,
            endLine: endLn,
            from: lineFrom,
            to: endTo,
            raw,
          });
          for (let k = ln; k <= endLn; k++) tableLines.add(k);
          ln = endLn;
          continue;
        }
      } else if (!hitFence) {
        noCloseTableUntil = limit;
      }
    }

    // 2. GFM Markdown pipe table block
    if (
      ln < total &&
      fenceState[ln + 1] === 0 &&
      text.includes("|") &&
      !/^\s*(?:>|#{1,6}\s)/.test(text) &&
      isGfmTableDelimiterLine(linesText[ln + 1])
    ) {
      let endLn = ln + 1;
      while (endLn + 1 <= total && fenceState[endLn + 1] === 0) {
        const nt = linesText[endLn + 1];
        if (!nt.trim() || !nt.includes("|") || /^\s*(?:>|#{1,6}\s)/.test(nt)) break;
        endLn++;
      }
      const endTo = lineFroms[endLn] + linesText[endLn].length;
      const raw = doc.sliceString(lineFrom, endTo);
      tables.push({
        kind: "md",
        startLine: ln,
        endLine: endLn,
        from: lineFrom,
        to: endTo,
        raw,
      });
      for (let k = ln; k <= endLn; k++) tableLines.add(k);
      ln = endLn;
      continue;
    }

    // 3. Multi-line HTML <a ...>...</a>
    if (text.includes("<a") || text.includes("<A")) {
      const lastOpenA = text.toLowerCase().lastIndexOf("<a");
      if (lastOpenA >= 0 && /^<a(?:\s|>|$)/i.test(text.slice(lastOpenA)) && !/<\/a>/i.test(text.slice(lastOpenA))) {
        let endLn = -1;
        const limit = Math.min(total, ln + 5);
        for (let k = ln + 1; k <= limit; k++) {
          if (fenceState[k] !== 0) break;
          if (/<\/a>/i.test(linesText[k])) {
            endLn = k;
            break;
          }
        }
        if (endLn > ln) {
          const endTo = lineFroms[endLn] + linesText[endLn].length;
          const chunk = doc.sliceString(lineFrom, endTo);
          IR_ML_ANCHOR_RE.lastIndex = 0;
          let mm: RegExpExecArray | null;
          let matchedEndLn = -1;
          while ((mm = IR_ML_ANCHOR_RE.exec(chunk)) !== null) {
            if (!mm[0].includes("\n")) continue;
            if (/<(?:table|div|p|ul|ol|h[1-6]|blockquote|pre)\b/i.test(mm[2])) continue;
            const absStart = lineFrom + mm.index;
            const absEnd = absStart + mm[0].length;
            const sLn = doc.lineAt(absStart).number;
            const eLn = doc.lineAt(absEnd).number;
            multiLinks.push({
              startLine: sLn,
              endLine: eLn,
              openFrom: absStart,
              openTo: absStart + mm[1].length,
              closeFrom: absEnd - mm[3].length,
              closeTo: absEnd,
            });
            if (eLn > matchedEndLn) matchedEndLn = eLn;
          }
          if (matchedEndLn > ln) {
            ln = matchedEndLn;
          }
        }
      }
    }
  }

  // Third O(N) pass: pre-scan single-line Markdown / HTML images across the whole document so CM6 HeightMap
  // registers image heights before scrolling brings them into the viewport and probes natural dimensions early.
  for (let ln = 1; ln <= total; ln++) {
    if (fenceState[ln] !== 0 || tableLines.has(ln) || alignTagLine[ln] === 1) continue;
    const text = linesText[ln];
    if (!text || text.length > 2400) continue;
    if (!text.includes("![") && !text.includes("<img") && !text.includes("<IMG")) continue;

    const lineFrom = lineFroms[ln];
    const occ = new Uint8Array(text.length);
    const claim = (s: number, e: number) => {
      if (s < 0 || e > occ.length || s >= e) return false;
      for (let i = s; i < e; i++) if (occ[i]) return false;
      for (let i = s; i < e; i++) occ[i] = 1;
      return true;
    };

    for (const ml of multiLinks) {
      if (ln >= ml.startLine && ln <= ml.endLine) {
        const rs = Math.max(0, ml.openFrom - lineFrom);
        const re = Math.min(occ.length, ml.closeTo - lineFrom);
        for (let i = rs; i < re; i++) occ[i] = 1;
      }
    }

    let im: RegExpExecArray | null;
    IR_CODE_RE.lastIndex = 0;
    while ((im = IR_CODE_RE.exec(text)) !== null) {
      claim(im.index, im.index + im[0].length);
    }
    const htmlCodeRe = /<code\b[^<>]*>[^<\n]*<\/code>/gi;
    while ((im = htmlCodeRe.exec(text)) !== null) {
      claim(im.index, im.index + im[0].length);
    }

    const foundOnLine: IRImageSpan[] = [];
    const pushIRImgSpan = (
      s: number,
      e: number,
      imgFrom: number,
      imgTo: number,
      rawTarget: string,
      resolvedSrc: string,
      alt: string,
      href = "",
      htmlTag?: string
    ) => {
      if (!claim(s, e)) return;
      seedIRImageHintDims(resolvedSrc, htmlTag);
      warmIRImageDim(resolvedSrc);
      foundOnLine.push({
        lineNumber: ln,
        spanFrom: lineFrom + s,
        spanTo: lineFrom + e,
        imgFrom: lineFrom + imgFrom,
        imgTo: lineFrom + imgTo,
        resolvedSrc,
        rawTarget,
        alt,
        href,
      });
    };
    const linkedImgRe =
      /\[!\[([^[\]\n]*)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)/g;
    while ((im = linkedImgRe.exec(text)) !== null) {
      const alt = (im[1] || "").trim();
      const rawTarget = parseMarkdownImageTarget(im[2] || "");
      const resolvedSrc = resolveIRImageSrc(rawTarget);
      if (!resolvedSrc) continue;
      const href = cleanClipboardUrl(parseMarkdownImageTarget(im[3] || ""), "href");
      const s = im.index;
      const imgFrom = s + 1;
      pushIRImgSpan(s, s + im[0].length, imgFrom, imgFrom + 2 + im[1].length + 2 + im[2].length + 1, rawTarget, resolvedSrc, alt, href);
    }

    const mdImgRe = /!\[([^[\]\n]*)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)/g;
    while ((im = mdImgRe.exec(text)) !== null) {
      const alt = (im[1] || "").trim();
      const rawTarget = parseMarkdownImageTarget(im[2] || "");
      const resolvedSrc = resolveIRImageSrc(rawTarget);
      if (!resolvedSrc) continue;
      const s = im.index;
      const e = s + im[0].length;
      pushIRImgSpan(s, e, s, e, rawTarget, resolvedSrc, alt);
    }

    const htmlImgRe = /<img\b(?:"[^"<>]*"|'[^'<>]*'|[^"'<>])*>/gi;
    while ((im = htmlImgRe.exec(text)) !== null) {
      const rawTarget = readHtmlAttr(im[0], "src");
      const resolvedSrc = resolveIRImageSrc(rawTarget);
      if (!resolvedSrc) continue;
      const alt = readHtmlAttr(im[0], "alt");
      const s = im.index;
      const e = s + im[0].length;
      pushIRImgSpan(s, e, s, e, rawTarget, resolvedSrc, alt, "", im[0]);
    }

    if (foundOnLine.length > 1) {
      foundOnLine.sort((a, b) => a.spanFrom - b.spanFrom || a.spanTo - b.spanTo);
    }
    for (const imgSpan of foundOnLine) images.push(imgSpan);
  }

  const res: IRDocScan = {
    tables,
    mermaids,
    mathBlocks,
    tableLines,
    multiLinks,
    images,
    fenceState,
    fenceNoHash,
    fenceFam,
    fenceBlockCmt,
    fenceEndLine,
    fenceAutoLang,
    calloutKind,
    quoteDepth,
    quoteEdge,
    alignKind,
    alignTagLine,
    lockState,
  };
  irDocScanCache.set(doc, res);
  return res;
}

const CSS_AT_RE = /@[A-Za-z-]+/g;
const CSS_IMP_RE = /!\s*important\b/gi;
const CSS_VAR_RE = /--[A-Za-z0-9_-]+|\$[A-Za-z_-][\w-]*/g;
const CSS_FN_RE = /(?<![A-Za-z0-9_-])([A-Za-z_-][\w-]*)(?=\()/g;
const CSS_VAL_KW_RE =
  /\b(?:none|auto|inherit|initial|unset|solid|dashed|dotted|flex|grid|block|inline|inline-flex|inline-block|relative|absolute|fixed|sticky|hidden|clip|visible|pointer|center|left|right|top|bottom|space-between|transparent|currentColor|border-box|ease|linear|infinite|both|row|column|wrap|nowrap|bold|normal|italic)\b/g;

function highlightIRCodeLine(
  text: string,
  fam: number,
  noHash: number,
  enteredInBlockCmt: boolean,
  claim: (s: number, e: number, cls: string) => void
): void {
  let cm: RegExpExecArray | null;
  const hl = (re: RegExp, cls: string, grp = 0) => {
    while ((cm = re.exec(text)) !== null) {
      const s = grp ? cm.index + (cm[0].length - cm[grp].length) : cm.index;
      claim(s, s + cm[grp].length, cls);
    }
  };

  // Family 11: Markdown
  if (fam === 11) {
    if (/^\s*#{1,6}\s+/.test(text)) {
      claim(0, text.length, "gh");
      return;
    }
    if (/^\s*(?:`{3,}|~{3,})/.test(text)) {
      claim(0, text.length, "cp");
      return;
    }
    const bqM = /^\s*>\s?/.exec(text);
    if (bqM) claim(0, bqM[0].length, "gu");
    hl(/`[^`\n]+`/g, "s2");
    hl(/!?\[[^[\]\n]*\]\([^()\n]+\)/g, "na");
    return;
  }

  // Family 6: Unified Diff / Patch
  if (fam === 6) {
    const hunkM = /^(@@[^@\n]*@@)(.*)$/.exec(text);
    if (hunkM) {
      claim(0, hunkM[1].length, "gu");
      return;
    }
    if (/^(?:diff --git\b|index\s|---\s|\+\+\+\s|\*\*\*\s)/.test(text)) {
      claim(0, text.length, "gh");
    } else if (text.startsWith("+")) {
      claim(0, text.length, "gi");
    } else if (text.startsWith("-")) {
      claim(0, text.length, "gd");
    }
    return;
  }

  // Family 4: HTML / XML / SVG / Vue
  if (fam === 4) {
    let startIdx = 0;
    if (enteredInBlockCmt) {
      const closeIdx = text.indexOf("-->");
      if (closeIdx < 0) {
        claim(0, text.length, "cm");
        return;
      }
      claim(0, closeIdx + 3, "cm");
      startIdx = closeIdx + 3;
    }
    while (startIdx < text.length) {
      const openIdx = text.indexOf("<!--", startIdx);
      if (openIdx < 0) break;
      const closeIdx = text.indexOf("-->", openIdx + 4);
      if (closeIdx < 0) {
        claim(openIdx, text.length, "cm");
        break;
      }
      claim(openIdx, closeIdx + 3, "cm");
      startIdx = closeIdx + 3;
    }
    hl(/<!DOCTYPE[^<>]*>|<\?xml[^<>?]*\?>/gi, "cp");
    hl(/"[^"]*"|'[^']*'/g, "s2");
    const tagRe = /(<\/?)([A-Za-z][A-Za-z0-9:._-]*)/g;
    while ((cm = tagRe.exec(text)) !== null) {
      claim(cm.index, cm.index + cm[1].length, "p");
      claim(cm.index + cm[1].length, cm.index + cm[0].length, "nt");
    }
    hl(/\/?>/g, "p");
    hl(/(\s)([A-Za-z_:][A-Za-z0-9_.:-]*)(?=\s*=)/g, "na", 2);
    hl(/&[A-Za-z0-9#]+;/g, "kc");
    return;
  }

  // Family 8: Lua (--[[ ... ]] block comments, -- line comments)
  if (fam === 8) {
    let idx8 = 0;
    if (enteredInBlockCmt) {
      const closeIdx = text.indexOf("]]");
      if (closeIdx < 0) {
        claim(0, text.length, "cm");
        return;
      }
      claim(0, closeIdx + 2, "cm");
      idx8 = closeIdx + 2;
    }
    while (idx8 < text.length) {
      if (text[idx8] === "-" && text[idx8 + 1] === "-") {
        if (text[idx8 + 2] === "[" && text[idx8 + 3] === "[") {
          const end = text.indexOf("]]", idx8 + 4);
          if (end < 0) {
            claim(idx8, text.length, "cm");
            break;
          }
          claim(idx8, end + 2, "cm");
          idx8 = end + 2;
          continue;
        }
        claim(idx8, text.length, "c1");
        break;
      }
      if (text[idx8] === '"' || text[idx8] === "'") {
        const q = text[idx8];
        const endPos = Math.min(scanQuotedEnd(text, idx8, q), text.length);
        claim(idx8, endPos, q === "'" ? "s1" : "s2");
        idx8 = endPos;
        continue;
      }
      idx8++;
    }
    hl(/\b(?:true|false|nil|self)\b/g, "kc");
    hl(/\b(?:local|function|end|if|then|else|elseif|for|in|while|do|repeat|until|return|break|goto|and|or|not)\b/g, "k");
    hl(/\b([A-Za-z_]\w*)(?=\s*[({'"\[])/g, "nf");
    hl(/\b(?:0[xX][0-9a-fA-F]+|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)\b/g, "mi");
    return;
  }

  // Lexical left-to-right pass for multi-line block comments, C preprocessor, line comments & string literals
  let idx = 0;
  const supportsBlockCmt = fam === 0 || fam === 1 || fam === 3 || fam === 5 || fam === 7 || fam === 10;
  if (enteredInBlockCmt && supportsBlockCmt) {
    const closeIdx = text.indexOf("*/");
    if (closeIdx < 0) {
      claim(0, text.length, "cm");
      return;
    }
    claim(0, closeIdx + 2, "cm");
    idx = closeIdx + 2;
  }

  if (idx === 0 && (fam === 0 || fam === 1)) {
    const ppM = /^(\s*)(#\s*(?:include|define|undef|ifdef|ifndef|if|elif|else|endif|pragma|error|region|endregion)\b)(\s*<[^>\n]+>)?/.exec(
      text
    );
    if (ppM) {
      const dFrom = ppM[1].length;
      const dTo = dFrom + ppM[2].length;
      claim(dFrom, dTo, "cp");
      if (ppM[3]) claim(dTo, dTo + ppM[3].length, "cpf");
      idx = ppM[0].length;
    }
  }

  while (idx < text.length) {
    const ch = text[idx];
    const nx = text[idx + 1];
    if (supportsBlockCmt && ch === "/" && nx === "*") {
      const end = text.indexOf("*/", idx + 2);
      if (end < 0) {
        claim(idx, text.length, "cm");
        break;
      }
      claim(idx, end + 2, "cm");
      idx = end + 2;
      continue;
    }
    if (fam !== 12 && ch === "/" && nx === "/" && (idx === 0 || text[idx - 1] !== ":")) {
      claim(idx, text.length, "c1");
      break;
    }
    if ((fam === 7 || fam === 12) && ch === "-" && nx === "-") {
      claim(idx, text.length, "c1");
      break;
    }
    if (!noHash && ch === "#" && nx !== "[" && (idx === 0 || /\s/.test(text[idx - 1]))) {
      claim(idx, text.length, "c1");
      break;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      if (ch === "'" && (fam === 0 || fam === 1)) {
        const lifeM = /^'[A-Za-z_][A-Za-z0-9_]*(?!')/.exec(text.slice(idx));
        if (lifeM && (idx === 0 || !/[A-Za-z0-9_]/.test(text[idx - 1]))) {
          claim(idx, idx + lifeM[0].length, "kt");
          idx += lifeM[0].length;
          continue;
        }
      }
      const endPos = Math.min(scanQuotedEnd(text, idx, ch), text.length);
      const isJsonKey = fam === 3 && ch === '"' && /^\s*:/.test(text.slice(endPos));
      claim(idx, endPos, isJsonKey ? "nt" : ch === "'" ? "s1" : "s2");
      idx = endPos;
      continue;
    }
    idx++;
  }

  // Family 3: JSON / JSONC / JSON5
  if (fam === 3) {
    hl(/(?:^|[{,\s])([A-Za-z_$][\w$]*)(?=\s*:)/g, "nt", 1);
    hl(/\b(?:true|false|null|Infinity|NaN)\b/g, "kc");
    hl(/[+-]?\b(?:0[xX][0-9a-fA-F]+|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)\b/g, "mi");
    hl(/[{}\[\]:,]/g, "p");
    return;
  }

  // Family 5: CSS / SCSS / Less
  if (fam === 5) {
    hl(CSS_AT_RE, "k");
    hl(CSS_IMP_RE, "kc");
    hl(CSS_VAR_RE, "nv");
    const cssCtx = computeCssContext(text);
    const pseudoRe = /(::?[A-Za-z-]+)/g;
    while ((cm = pseudoRe.exec(text)) !== null) {
      if (cssCtx[cm.index] === 1 || cssCtx[cm.index] === 3) claim(cm.index, cm.index + cm[0].length, "nd");
    }
    const selRe = /[.#][A-Za-z_-][A-Za-z0-9_-]*/g;
    while ((cm = selRe.exec(text)) !== null) {
      if (cssCtx[cm.index] === 1) claim(cm.index, cm.index + cm[0].length, "nc");
    }
    const propRe = /(?:^|[;{\s(])([A-Za-z-]+)\s*:(?!:)/g;
    while ((cm = propRe.exec(text)) !== null) {
      const lead = /^[;{\s(]/.test(cm[0]) ? 1 : 0;
      const s = cm.index + lead;
      if (cssCtx[s] === 2 || cssCtx[s] === 3) claim(s, s + cm[1].length, "na");
    }
    hl(CSS_FN_RE, "nf");
    hl(/#[0-9a-fA-F]{3,8}\b|-?\b\d+(?:\.\d+)?(?:px|rem|em|vh|vw|dvh|svh|dvw|svw|%|s|ms|deg|fr|ch)?\b/g, "mi");
    hl(CSS_VAL_KW_RE, "kc");
    return;
  }

  // Family 7: SQL
  if (fam === 7) {
    hl(/\b(?:NULL|TRUE|FALSE)\b/gi, "kc");
    hl(
      /\b(?:INT|INTEGER|BIGINT|SMALLINT|SERIAL|BIGSERIAL|TEXT|VARCHAR|CHAR|BOOLEAN|BOOL|DATE|TIME|TIMESTAMP|TIMESTAMPTZ|NUMERIC|DECIMAL|FLOAT|DOUBLE|REAL|JSON|JSONB|UUID|BLOB|BYTEA)\b/gi,
      "kt"
    );
    hl(
      /\b(?:SELECT|FROM|WHERE|INSERT|INTO|VALUES|UPDATE|SET|DELETE|JOIN|LEFT|RIGHT|INNER|OUTER|FULL|CROSS|ON|AS|AND|OR|NOT|IN|IS|LIKE|ILIKE|BETWEEN|EXISTS|GROUP|BY|ORDER|HAVING|LIMIT|OFFSET|UNION|ALL|DISTINCT|CREATE|ALTER|DROP|TRUNCATE|TABLE|INDEX|VIEW|DATABASE|SCHEMA|PRIMARY|KEY|FOREIGN|REFERENCES|UNIQUE|DEFAULT|CHECK|CONSTRAINT|CASCADE|BEGIN|COMMIT|ROLLBACK|TRANSACTION|WITH|CASE|WHEN|THEN|ELSE|END|RETURNING|ASC|DESC)\b/gi,
      "k"
    );
    hl(/\b([A-Za-z_]\w*)(?=\s*\()/g, "nf");
    hl(/-?\b\d+(?:\.\d+)?\b/g, "mi");
    return;
  }

  // Family 9: GraphQL
  if (fam === 9) {
    hl(/@[A-Za-z_]\w*/g, "nd");
    hl(/\$[A-Za-z_]\w*/g, "nv");
    hl(/\b(?:true|false|null)\b/g, "kc");
    hl(/\b(?:Int|Float|String|Boolean|ID)\b/g, "kt");
    hl(
      /\b(?:query|mutation|subscription|fragment|on|type|interface|union|enum|input|scalar|schema|directive|extend|implements|repeatable)\b/g,
      "k"
    );
    hl(/\b([A-Za-z_]\w*)(?=\s*:)/g, "na");
    hl(/\b[A-Z][A-Za-z0-9_]*\b/g, "nc");
    hl(/-?\b\d+(?:\.\d+)?\b/g, "mi");
    return;
  }

  // Family 10: Protobuf
  if (fam === 10) {
    hl(/\b(?:true|false|max)\b/g, "kc");
    hl(
      /\b(?:double|float|int32|int64|uint32|uint64|sint32|sint64|fixed32|fixed64|sfixed32|sfixed64|bool|string|bytes)\b/g,
      "kt"
    );
    hl(
      /\b(?:syntax|edition|package|import|public|weak|option|message|enum|service|rpc|returns|stream|repeated|optional|required|oneof|map|reserved|extensions|to)\b/g,
      "k"
    );
    hl(/\b[A-Z][A-Za-z0-9_]*\b/g, "nc");
    hl(/\b(?:0[xX][0-9a-fA-F]+|\d+)\b/g, "mi");
    return;
  }

  // Families 0, 1, 2: Programming, PHP, Ruby, Elixir, HCL/Terraform, Zig, Scala, Dart, C#, F#, VB, Shell, Nginx, YAML, TOML
  hl(/<\?(?:php|=)?|\?>/gi, "cp");
  hl(/^\s*(#!?\[[^\]]*\])/g, "nd", 1);
  hl(/(?:^|[\s(])(@[A-Za-z_][\w.]*)/g, "nd", 1);
  hl(/(?:^|[\s([,=])(:[A-Za-z_]\w*[?!]?)\b/g, "kc", 1);
  hl(/\b(?:https?|wss?|grpcs?|fastcgi|uwsgi|scgi):\/\/[^\s;'"]+/g, "s2");
  hl(/\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|\$[@*#?$!0-9-]/g, "nv");
  hl(/(?:^|\s)(--?[A-Za-z][\w-]*)/g, "na", 1);
  if (fam !== 1) hl(/^\s*(?:-\s+)?([A-Za-z_][A-Za-z0-9_.-]*)(?=\s*(?::(?:\s|$)|=))/g, "nt", 1);
  hl(/\b(?:true|false|null|nil|undefined|None|True|False|Nothing|on|off|yes|no|NaN|Infinity|iota|self|Self|this|super|base)\b/g, "kc");
  hl(
    /\b(?:string|int|int8|int16|int32|int64|uint|uint8|uint16|uint32|uint64|uintptr|float32|float64|bool|byte|sbyte|decimal|object|dynamic|rune|error|any|void|char|short|ushort|long|ulong|double|float|size_t|usize|isize|i8|i16|i32|i64|i128|u8|u16|u32|u64|u128|f16|f32|f64|f128|anyerror|comptime_int|comptime_float|str|number|boolean|unknown|never|symbol|bigint|Unit|Any|AnyRef|AnyVal|Option|Result|Vec|Box|Rc|Arc|String|Map|Set|List|Future|Stream|Iterable|Promise|Array|Record)\b/g,
    "kt"
  );
  hl(
    /\b(?:sudo|apt|apt-get|yum|dnf|apk|cd|mkdir|tar|wget|curl|make|docker|kubectl|systemctl|git|npm|pnpm|terraform|export|return|func|go|defer|chan|select|fallthrough|range|map|function|fn|mut|ref|move|unsafe|where|trait|dyn|crate|use|extern|def|defp|defmodule|defmacro|defprotocol|defimpl|defstruct|alias|require|quote|unquote|cond|receive|after|lambda|yield|pass|raise|except|global|nonlocal|assert|del|elif|elsif|unless|until|begin|rescue|ensure|module|end|when|then|attr_accessor|attr_reader|attr_writer|resource|variable|output|provider|locals|data|echo|foreach|endforeach|endif|callable|clone|class|interface|implements|extends|with|mixin|extension|factory|late|covariant|required|sealed|internal|record|init|delegate|event|operator|out|params|checked|unchecked|fixed|lock|stackalloc|using|get|set|let|member|open|rec|mutable|comptime|errdefer|orelse|unreachable|nosuspend|suspend|resume|object|implicit|lazy|abstract|readonly|keyof|typeof|instanceof|infer|declare|namespace|enum|impl|pub|mod|match|async|await|try|catch|finally|throw|new|delete|const|var|val|if|else|for|while|do|loop|switch|case|default|break|continue|import|from|package|type|struct|union|static|final|private|protected|public|override|virtual|inline|constexpr|template|typename|in|of|as|is|not|and|or|Sub|Function|Dim|As|If|Then|Else|ElseIf|End|For|To|Next|While|Return|Imports|Module|Class|Public|Private|server|location|upstream|events|stream|http|listen|proxy_pass|fastcgi_pass|proxy_set_header|rewrite|root|index|error_page|access_log|error_log|include|ssl_certificate|ssl_certificate_key|client_max_body_size|keepalive_timeout|gzip|absolute_redirect|server_name|try_files|add_header)\b/g,
    "k"
  );
  hl(/\b([a-z_][a-z0-9_]*)!(?=\s*[(\[{])/g, "nd");
  hl(/\b([A-Za-z_]\w*[?!]?)(?=\s*\()/g, "nf");
  hl(/\b[A-Z][a-zA-Z0-9_]*\b/g, "nc");
  if (fam !== 1) hl(/^(\s*)([a-z][a-z0-9_]{2,})(?=\s+[^\s=:{])/g, "na", 2);
  hl(/\b(?:0[xX][0-9a-fA-F_]+|0[bB][01_]+|0[oO][0-7_]+|\d[\d_]*(?:\.[\d_]+)?(?:[eE][+-]?\d+)?(?:ms|s|m|h|d|k|K|M|G)?)\b/g, "mi");
}

interface IRBlockDecoState {
  decos: DecorationSet;
  focused: boolean;
  selFromLine: number;
  selToLine: number;
  actFromLine: number;
  actToLine: number;
}

function buildIRBlockDecos(
  state: EditorState,
  focused: boolean,
  selFromLine: number,
  selToLine: number,
  actFromLine: number,
  actToLine: number
): DecorationSet {
  const builder = new RangeSetBuilder<Decoration>();
  const scan = getIRDocScan(state.doc);
  if (
    scan.tables.length === 0 &&
    scan.mermaids.length === 0 &&
    scan.mathBlocks.length === 0 &&
    scan.multiLinks.length === 0 &&
    scan.images.length === 0
  ) {
    return builder.finish();
  }
  const hideDeco = Decoration.replace({});

  type BlockOp =
    | { pos: number; endPos: number; kind: "table"; item: IRTableBlock }
    | { pos: number; endPos: number; kind: "mermaid"; item: IRMermaidBlock }
    | { pos: number; endPos: number; kind: "math"; item: IRMathBlock }
    | { pos: number; endPos: number; kind: "mlink"; item: IRMultiLinkSpan }
    | { pos: number; endPos: number; kind: "image"; item: IRImageSpan };
  const merged: BlockOp[] = [
    ...scan.tables.map((item) => ({ pos: item.from, endPos: item.to, kind: "table" as const, item })),
    ...scan.mermaids.map((item) => ({ pos: item.from, endPos: item.to, kind: "mermaid" as const, item })),
    ...scan.mathBlocks.map((item) => ({ pos: item.from, endPos: item.to, kind: "math" as const, item })),
    ...scan.multiLinks.map((item) => ({ pos: item.openFrom, endPos: item.closeTo, kind: "mlink" as const, item })),
    ...scan.images.map((item) => ({ pos: item.spanFrom, endPos: item.spanTo, kind: "image" as const, item })),
  ].sort((a, b) => a.pos - b.pos || a.endPos - b.endPos);

  let lastEmittedTo = 0;
  for (const entry of merged) {
    if (entry.kind === "image") {
      if (
        (focused && selFromLine <= entry.item.lineNumber && selToLine >= entry.item.lineNumber) ||
        (actFromLine > 0 && actFromLine <= entry.item.lineNumber && actToLine >= entry.item.lineNumber)
      ) {
        continue;
      }
    } else if (
      (focused && selFromLine <= entry.item.endLine && selToLine >= entry.item.startLine) ||
      (actFromLine > 0 && actFromLine <= entry.item.endLine && actToLine >= entry.item.startLine)
    ) {
      continue;
    }
    if (entry.pos < lastEmittedTo) {
      continue;
    }
    if (entry.kind === "table") {
      const { item } = entry;
      builder.add(
        item.from,
        item.to,
        Decoration.replace({
          widget: new TableIRWidget(item.raw, item.kind, item.from, item.to),
          block: true,
        })
      );
      lastEmittedTo = item.to;
    } else if (entry.kind === "mermaid") {
      const { item } = entry;
      builder.add(
        item.from,
        item.to,
        Decoration.replace({
          widget: new MermaidIRWidget(item.code, item.from),
          block: true,
        })
      );
      lastEmittedTo = item.to;
    } else if (entry.kind === "math") {
      const { item } = entry;
      builder.add(
        item.from,
        item.to,
        Decoration.replace({
          widget: new MathBlockIRWidget(item.expr, item.from),
          block: true,
        })
      );
      lastEmittedTo = item.to;
    } else if (entry.kind === "mlink") {
      const { item } = entry;
      if (item.openFrom < item.openTo) builder.add(item.openFrom, item.openTo, hideDeco);
      if (item.openTo < item.closeFrom) {
        builder.add(item.openTo, item.closeFrom, Decoration.mark({ class: "cm-ir-link" }));
      }
      if (item.closeFrom < item.closeTo) builder.add(item.closeFrom, item.closeTo, hideDeco);
      lastEmittedTo = item.closeTo;
    } else {
      const { item } = entry;
      if (item.spanFrom < item.imgFrom) builder.add(item.spanFrom, item.imgFrom, hideDeco);
      builder.add(
        item.imgFrom,
        item.imgTo,
        Decoration.replace({
          widget: new ImageIRWidget(item.resolvedSrc, item.rawTarget, item.alt, item.imgFrom, item.spanTo),
        })
      );
      if (item.imgTo < item.spanTo) {
        builder.add(
          item.imgTo,
          item.spanTo,
          Decoration.replace({ widget: new LinkedImageBadgeIRWidget(item.href, item.spanTo) })
        );
      }
      lastEmittedTo = item.spanTo;
    }
  }
  return builder.finish();
}

const irBlockDecoField = StateField.define<IRBlockDecoState>({
  create(state) {
    const focused = state.field(irFocusedField, false) ?? false;
    const selFromLine = focused ? state.doc.lineAt(state.selection.main.from).number : -1;
    const selToLine = focused ? state.doc.lineAt(state.selection.main.to).number : -1;
    const act = getActiveSearchLineRange(state);
    const actFromLine = act ? act.startLine : -1;
    const actToLine = act ? act.endLine : -1;
    return {
      decos: buildIRBlockDecos(state, focused, selFromLine, selToLine, actFromLine, actToLine),
      focused,
      selFromLine,
      selToLine,
      actFromLine,
      actToLine,
    };
  },
  update(prev, tr) {
    const focused = tr.state.field(irFocusedField, false) ?? false;
    const selFromLine = focused ? tr.state.doc.lineAt(tr.state.selection.main.from).number : -1;
    const selToLine = focused ? tr.state.doc.lineAt(tr.state.selection.main.to).number : -1;
    const act = getActiveSearchLineRange(tr.state);
    const actFromLine = act ? act.startLine : -1;
    const actToLine = act ? act.endLine : -1;
    const dimUpdated = tr.effects.some((e) => e.is(irImgDimReadyEffect));
    if (
      tr.docChanged ||
      dimUpdated ||
      focused !== prev.focused ||
      selFromLine !== prev.selFromLine ||
      selToLine !== prev.selToLine ||
      actFromLine !== prev.actFromLine ||
      actToLine !== prev.actToLine
    ) {
      return {
        decos: buildIRBlockDecos(tr.state, focused, selFromLine, selToLine, actFromLine, actToLine),
        focused,
        selFromLine,
        selToLine,
        actFromLine,
        actToLine,
      };
    }
    return prev;
  },
  provide: (f) => EditorView.decorations.from(f, (v) => v.decos),
});

function buildIRQuoteLineCls(depth: number, edge: number, alertKind: string): string {
  let quoteCls = "cm-ir-quote";
  if (depth === 2) quoteCls += " cm-ir-quote-d2";
  else if (depth >= 3) quoteCls += " cm-ir-quote-d3";
  if (edge & 1) quoteCls += " cm-ir-quote-first";
  if (edge & 2) quoteCls += " cm-ir-quote-last";
  if (alertKind) quoteCls += ` cm-ir-callout cm-ir-callout-${alertKind}`;
  return quoteCls;
}

const IR_HTML_TECH_RE = /(<(code|kbd|sup|sub)\b[^<>]*>)([^<\n]+)(<\/\2>)/gi;
const IR_BR_RE = /<br\s*\/?>/gi;
const IR_FONT_RE = /(<font\b[^<>]*>)([^<\n]+)(<\/font>)/gi;
const IR_SPAN_RE = /(<span\b[^<>]*>)([^<\n]+)(<\/span>)/gi;
const IR_MARK_RE = /(<mark>)([^<\n]+)(<\/mark>)/gi;
const IR_U_RE = /(<u>)([^<\n]+)(<\/u>)/gi;
const IR_HTML_LINK_RE = /(<a\b(?:"[^"<>]*"|'[^'<>]*'|[^"'<>])*?>)([^<\n]+)(<\/a>)/gi;
const IR_HTML_FMT_RE = /(<(strong|b|em|i|s|del|strike)>)([^<\n]+)(<\/\2>)/gi;
const IR_AUDIO_RE = /(<audio\b[^<>]*>)\s*(?:<\/audio>)?/gi;
const IR_VIDEO_RE = /(<video\b[^<>]*>)\s*(?:<\/video>)?/gi;
const IR_WIKI_RE = /\[\[([^[\]\n|\x01\x02]+?)(?:\|([^[\]\n]+))?\]\]/g;
const IR_LINK_RE = /(?<!!)\[([^[\]\n]+)\]\((?!\))([^()\n\x01\x02]*(?:\([^()\n\x01\x02]*\)[^()\n\x01\x02]*)*)\)/g;
const IR_BARE_URL_RE = /\bhttps?:\/\/[^\s<>()[\]"'\x01\x02]+/gi;
const IR_EQ_MARK_RE = /(?:^|[^=])==(?!=|\s)([^=\n]*?[^\s=])==(?!=)/g;
const IR_BOLD_RE = /\*\*(?!\s)([^\n*]+(?:\*(?!\*)[^\n*]+)*)\*\*/g;
const IR_STRIKE_RE = /~~([^~\n]+)~~/g;
const IR_ITALIC_RE = /(?:^|[^*])\*([^*\s][^*\n]*?)\*(?!\*)/g;
const IR_SUP_RE = /(?:^|[^^\\[])\^([^\s^\n[\]]+)\^(?!\^)/g;
const IR_SUB_RE = /(?:^|[^~\\/])~([^\s~\n/]+)~(?!~)/g;

function buildInstantRenderPlugin(): Extension {
  const hideDeco = Decoration.replace({});
  const buildDecos = (view: EditorView): DecorationSet => {
    const state = view.state;
    if (view.visibleRanges.length === 0) return Decoration.none;
    const allRanges: ReturnType<Decoration["range"]>[] = [];
    const addLine = (pos: number, cls: string) => {
      allRanges.push(Decoration.line({ class: cls }).range(pos));
    };
    const addRange = (from: number, to: number, deco: Decoration) => {
      if (from < to) allRanges.push(deco.range(from, to));
    };
    const addPoint = (pos: number, deco: Decoration) => {
      allRanges.push(deco.range(pos));
    };

    const focused = view.hasFocus;
    const composing = view.composing;
    const trackSel = focused || composing;
    const selFromLine = trackSel ? state.doc.lineAt(state.selection.main.from).number : -1;
    const selToLine = trackSel ? state.doc.lineAt(state.selection.main.to).number : -1;
    const actSearch = getActiveSearchLineRange(state);
    const actFromLine = actSearch ? actSearch.startLine : -1;
    const actToLine = actSearch ? actSearch.endLine : -1;
    const scan = getIRDocScan(state.doc);

    const collapsedBlockLines = new Set<number>();
    for (const blk of [...scan.tables, ...scan.mermaids, ...scan.mathBlocks]) {
      const expanded =
        (focused && selFromLine <= blk.endLine && selToLine >= blk.startLine) ||
        (actFromLine > 0 && actFromLine <= blk.endLine && actToLine >= blk.startLine);
      if (!expanded) {
        for (let k = blk.startLine; k <= blk.endLine; k++) collapsedBlockLines.add(k);
      }
    }
    const activeMultiLinks = scan.multiLinks.filter(
      (ml) =>
        !(
          (focused && selFromLine <= ml.endLine && selToLine >= ml.startLine) ||
          (actFromLine > 0 && actFromLine <= ml.endLine && actToLine >= ml.startLine)
        )
    );
    const irBlockState = state.field(irBlockDecoField, false);
    const imagesByLine = new Map<number, IRImageSpan[]>();
    if (scan.images.length > 0) {
      const bf = irBlockState?.focused ?? focused;
      const bsFrom = irBlockState?.selFromLine ?? selFromLine;
      const bsTo = irBlockState?.selToLine ?? selToLine;
      const bActFrom = irBlockState?.actFromLine ?? actFromLine;
      const bActTo = irBlockState?.actToLine ?? actToLine;
      for (const imgSpan of scan.images) {
        if (
          (bf && bsFrom <= imgSpan.lineNumber && bsTo >= imgSpan.lineNumber) ||
          (bActFrom > 0 && bActFrom <= imgSpan.lineNumber && bActTo >= imgSpan.lineNumber)
        ) {
          continue;
        }
        let list = imagesByLine.get(imgSpan.lineNumber);
        if (!list) {
          list = [];
          imagesByLine.set(imgSpan.lineNumber, list);
        }
        list.push(imgSpan);
      }
    }

    let lastEmittedLine = 0;

    for (const { from, to } of view.visibleRanges) {
      let pos = from;
      while (pos <= to) {
        const line = state.doc.lineAt(pos);
        if (line.number <= lastEmittedLine) {
          pos = line.to + 1;
          continue;
        }
        lastEmittedLine = line.number;
        if (collapsedBlockLines.has(line.number)) {
          pos = line.to + 1;
          continue;
        }

        const text = line.text;
        const isSearchActiveLine = actFromLine > 0 && line.number >= actFromLine && line.number <= actToLine;
        const isCursorLine = (trackSel && line.number >= selFromLine && line.number <= selToLine) || isSearchActiveLine;
        const fState = scan.fenceState[line.number] || 0;
        const lockSt = scan.lockState[line.number] || 0;
        if (lockSt > 0) {
          addLine(
            line.from,
            lockSt === 1 ? "cm-ir-lock cm-ir-lock-open" : lockSt === 3 ? "cm-ir-lock cm-ir-lock-close" : "cm-ir-lock cm-ir-lock-body"
          );
        }

        const fQDepth = scan.quoteDepth[line.number] || 0;
        if (fState === 1 || fState === 3) {
          const opening = fState === 1;
          let fenceLineCls = opening ? "cm-ir-fence cm-ir-fence-open" : "cm-ir-fence cm-ir-fence-close";
          if (fQDepth > 0) {
            const scanCallout = CALLOUT_NAMES[scan.calloutKind[line.number] || 0] || "";
            fenceLineCls += " " + buildIRQuoteLineCls(fQDepth, scan.quoteEdge[line.number] || 3, scanCallout);
          }
          addLine(line.from, fenceLineCls);
          if (!isCursorLine && text.length > 0) {
            if (opening) {
              const openInfo = matchFenceOpen(text);
              const explicitLang = (openInfo?.lang || "").toLowerCase();
              if (explicitLang === "mermaid") {
                if (openInfo && openInfo.langRawLen > 0) {
                  addRange(line.from, line.from + openInfo.langOffset, hideDeco);
                  addRange(
                    line.from + openInfo.langOffset,
                    line.from + openInfo.langOffset + openInfo.langRawLen,
                    Decoration.mark({ class: "cm-ir-fence-lang" })
                  );
                } else {
                  addRange(line.from, line.to, Decoration.mark({ class: "cm-ir-fence-muted" }));
                }
              } else {
                const autoLang = scan.fenceAutoLang.get(line.number) || "";
                const closeLn = scan.fenceEndLine[line.number] || 0;
                addRange(
                  line.from,
                  line.to,
                  Decoration.replace({ widget: new FenceLangIRWidget(line.number, explicitLang, autoLang) })
                );
                if (closeLn > line.number) {
                  addPoint(line.to, Decoration.widget({ widget: new FenceCopyIRWidget(line.number, closeLn), side: 1 }));
                }
              }
            } else {
              if (fQDepth > 0) {
                const qp = stripQuotePrefix(text, fQDepth);
                if (qp.len > 0) addRange(line.from, line.from + qp.len, hideDeco);
                addRange(line.from + qp.len, line.to, Decoration.mark({ class: "cm-ir-fence-muted" }));
              } else {
                addRange(line.from, line.to, Decoration.mark({ class: "cm-ir-fence-muted" }));
              }
            }
          }
          pos = line.to + 1;
          continue;
        }

        if (fState === 2) {
          let codeLineCls = "cm-ir-codeblock-line chroma";
          if (fQDepth > 0) {
            const scanCallout = CALLOUT_NAMES[scan.calloutKind[line.number] || 0] || "";
            codeLineCls += " " + buildIRQuoteLineCls(fQDepth, scan.quoteEdge[line.number] || 3, scanCallout);
          }
          addLine(line.from, codeLineCls);
          const qp = fQDepth > 0 ? stripQuotePrefix(text, fQDepth) : { stripped: text, len: 0 };
          if (!isCursorLine && qp.len > 0) {
            addRange(line.from, line.from + qp.len, hideDeco);
          }
          const codeSlice = qp.stripped;
          const baseOffset = line.from + qp.len;
          if (!(composing && isCursorLine) && codeSlice.length > 0 && codeSlice.length < 1600) {
            interface CodeTok {
              s: number;
              e: number;
              cls: string;
            }
            const toks: CodeTok[] = [];
            const used = new Uint8Array(codeSlice.length);
            const claim = (s: number, e: number, cls: string) => {
              if (s >= e || s < 0 || e > codeSlice.length) return;
              for (let i = s; i < e; i++) if (used[i]) return;
              for (let i = s; i < e; i++) used[i] = 1;
              toks.push({ s, e, cls });
            };
            highlightIRCodeLine(
              codeSlice,
              scan.fenceFam[line.number] || 0,
              scan.fenceNoHash[line.number] || 0,
              scan.fenceBlockCmt[line.number] === 1,
              claim
            );
            toks.sort((a, b) => a.s - b.s || a.e - b.e);
            for (const tk of toks) {
              addRange(baseOffset + tk.s, baseOffset + tk.e, Decoration.mark({ class: tk.cls }));
            }
          }
          pos = line.to + 1;
          continue;
        }

        const capLen = Math.min(text.length, 2400);
        const mChars = text.slice(0, capLen).split("");
        let maskDirty = false;
        const claimLiteral = (s: number, e: number): boolean => {
          if (s < 0 || e > capLen || s >= e) return false;
          for (let i = s; i < e; i++) {
            if (mChars[i] === "\x01" || mChars[i] === "\x02") return false;
          }
          for (let i = s; i < e; i++) mChars[i] = "\x02";
          maskDirty = true;
          return true;
        };
        const claimDelims = (openFrom: number, openTo: number, closeFrom: number, closeTo: number): boolean => {
          if (openFrom < 0 || closeTo > capLen || openFrom > openTo || openTo > closeFrom || closeFrom > closeTo) {
            return false;
          }
          if (openFrom === openTo && closeFrom === closeTo) return false;
          for (let i = openFrom; i < openTo; i++) {
            if (mChars[i] === "\x01" || mChars[i] === "\x02") return false;
          }
          for (let i = closeFrom; i < closeTo; i++) {
            if (mChars[i] === "\x01" || mChars[i] === "\x02") return false;
          }
          for (let i = openFrom; i < openTo; i++) mChars[i] = "\x01";
          for (let i = closeFrom; i < closeTo; i++) mChars[i] = "\x01";
          maskDirty = true;
          return true;
        };
        const applyDelimMark = (s: number, openEnd: number, closeStart: number, e: number, cls: string) => {
          if (!claimDelims(s, openEnd, closeStart, e)) return;
          addRange(line.from + s, line.from + openEnd, hideDeco);
          addRange(line.from + openEnd, line.from + closeStart, Decoration.mark({ class: cls }));
          addRange(line.from + closeStart, line.from + e, hideDeco);
        };
        const hideLineEdges = (openLen: number, closeTailLen: number) => {
          const closeStart = text.length - closeTailLen;
          if (!isCursorLine && claimDelims(0, openLen, closeStart, text.length)) {
            addRange(line.from, line.from + openLen, hideDeco);
            addRange(line.from + closeStart, line.to, hideDeco);
          }
        };

        // Reserve segments already handled by multi-line <a> and pre-scanned image decorations in irBlockDecoField
        for (const ml of activeMultiLinks) {
          if (line.number >= ml.startLine && line.number <= ml.endLine) {
            const rs = Math.max(0, ml.openFrom - line.from);
            const re = Math.min(capLen, ml.closeTo - line.from);
            for (let i = rs; i < re; i++) mChars[i] = "\x02";
          }
        }
        const lineImages = imagesByLine.get(line.number);
        if (lineImages) {
          for (const imgSpan of lineImages) {
            const rs = Math.max(0, imgSpan.spanFrom - line.from);
            const re = Math.min(capLen, imgSpan.spanTo - line.from);
            for (let i = rs; i < re; i++) mChars[i] = "\x02";
          }
        }

        const hm = /^(#{1,6})\s+/.exec(text);
        const htmlHm = !hm ? /^(\s*<h([1-6])\b[^<>]*>)(.*)(<\/h\2>\s*)$/i.exec(text) : null;
        const qm = /^([ \t]*(?:>[ \t]?)+)(?:\[!(NOTE|TIP|IMPORTANT|WARNING|CAUTION)\][ \t]*)?/i.exec(text);
        const htmlQm = !qm ? /^(\s*<blockquote\b[^<>]*>)(.*)(<\/blockquote>\s*)$/i.exec(text) : null;
        const soloToc = /^\s*\[toc\]\s*$/i.exec(text);
        const dm = !soloToc ? /^\s*:::(?:bookshelf|book|lock)?.*$/i.exec(text) : null;
        const hrm = /^\s*(?:---|\*\*\*|___|<hr\b[^<>]*\/?>)\s*$/i.exec(text);
        const soloBr = /^\s*<br\s*\/?>\s*$/i.exec(text);
        const centerLine = /^(\s*<center>)(.*)(<\/center>\s*)$/i.exec(text);
        const pLine = /^(\s*<p\b[^<>]*>)(.*)(<\/p>\s*)$/i.exec(text);
        const liLine = /^(\s*<li\b[^<>]*>)(.*)(<\/li>\s*)$/i.exec(text);
        const listWrapLine = /^\s*<\/?(?:ul|ol)\b[^<>]*>\s*$/i.exec(text);
        const alignTagLine = scan.alignTagLine[line.number] === 1;
        const alignKind = scan.alignKind[line.number] || 0;
        if (alignKind) {
          addLine(
            line.from,
            alignKind === 1 ? "cm-ir-center" : alignKind === 2 ? "cm-ir-align-right" : "cm-ir-align-left"
          );
        }

        if (alignTagLine) {
          addLine(line.from, "cm-ir-align-tag");
          claimLiteral(0, capLen);
        } else if (hm) {
          const lvl = hm[1].length;
          addLine(line.from, `cm-ir-h cm-ir-h${lvl}`);
          if (!isCursorLine && claimLiteral(0, hm[0].length)) {
            addRange(line.from, line.from + hm[0].length, hideDeco);
          }
        } else if (htmlHm) {
          addLine(line.from, `cm-ir-h cm-ir-h${Number(htmlHm[2])}`);
          hideLineEdges(htmlHm[1].length, htmlHm[4].length);
        } else if (qm) {
          const scanCallout = CALLOUT_NAMES[scan.calloutKind[line.number] || 0] || "";
          const alertKind = qm[2] ? qm[2].toLowerCase() : scanCallout;
          const depth = scan.quoteDepth[line.number] || Math.min(3, (qm[1].match(/>/g) || []).length);
          const edge = scan.quoteEdge[line.number] || 3;
          addLine(line.from, buildIRQuoteLineCls(depth, edge, alertKind));
          if (!isCursorLine && claimLiteral(0, qm[1].length)) {
            addRange(line.from, line.from + qm[1].length, hideDeco);
            if (qm[2] && claimLiteral(qm[1].length, qm[0].length)) {
              addRange(
                line.from + qm[1].length,
                line.from + qm[0].length,
                Decoration.replace({ widget: new CalloutPillIRWidget(alertKind, line.from + qm[0].length) })
              );
            }
          }
        } else if (htmlQm) {
          addLine(line.from, "cm-ir-quote cm-ir-quote-first cm-ir-quote-last");
          hideLineEdges(htmlQm[1].length, htmlQm[3].length);
        } else if (soloToc) {
          if (!isCursorLine && claimLiteral(0, text.length)) {
            addRange(
              line.from,
              line.to,
              Decoration.replace({ widget: new TocIRWidget(line.from, line.to) })
            );
          } else {
            addLine(line.from, "cm-ir-directive");
          }
        } else if (dm) {
          addLine(line.from, "cm-ir-directive");
        } else if (hrm) {
          addLine(line.from, "cm-ir-hr");
          if (!isCursorLine && claimLiteral(0, text.length)) {
            addRange(line.from, line.to, hideDeco);
          }
        } else if (soloBr || listWrapLine) {
          if (!isCursorLine && claimLiteral(0, text.length)) {
            addRange(line.from, line.to, hideDeco);
          }
        } else if (centerLine) {
          addLine(line.from, "cm-ir-center");
          hideLineEdges(centerLine[1].length, centerLine[3].length);
        } else if (pLine && !isCursorLine) {
          hideLineEdges(pLine[1].length, pLine[3].length);
        } else if (liLine && !isCursorLine) {
          const openLen = liLine[1].length;
          const closeStart = text.length - liLine[3].length;
          if (claimDelims(0, openLen, closeStart, text.length)) {
            addRange(
              line.from,
              line.from + openLen,
              Decoration.replace({ widget: new BulletIRWidget() })
            );
            addRange(line.from + closeStart, line.to, hideDeco);
          }
        }

        // Inline decorations on non-cursor / non-active-search lines (never applied when IME is composing on cursor line)
        if (!isCursorLine && text.length > 0 && text.length <= 2400) {
          // Interactive task checkbox (- [ ] / - [x] and blockquote > - [ ] / > - [x])
          const qp = parseQuotePrefix(text);
          const tm = /^(\s*)([-*+]\s+)(\[[ xX]\])(\s+)/.exec(text.slice(qp.prefixLen));
          if (tm) {
            const bulletStart = qp.prefixLen + tm[1].length;
            const boxStart = bulletStart + tm[2].length;
            const boxEnd = boxStart + tm[3].length;
            if (claimLiteral(bulletStart, boxEnd)) {
              const checked = /x/i.test(tm[3]);
              addRange(line.from + bulletStart, line.from + boxStart, hideDeco);
              addRange(
                line.from + boxStart,
                line.from + boxEnd,
                Decoration.replace({
                  widget: new TaskCheckboxIRWidget(checked, line.from + boxStart, line.from + boxEnd),
                })
              );
            }
          }

          // Inline code `...` / ``a ` b`` (atomic literal: claim entire span as \x02)
          let m: RegExpExecArray | null;
          let curMask = mChars.join("");
          maskDirty = false;
          const refreshMask = () => {
            if (maskDirty) {
              curMask = mChars.join("");
              maskDirty = false;
            }
          };
          const runPairPass = (re: RegExp, openLen: number, closeLen: number, cls: string, leadChar?: string) => {
            refreshMask();
            re.lastIndex = 0;
            while ((m = re.exec(curMask)) !== null) {
              const lead = leadChar && !m[0].startsWith(leadChar) ? 1 : 0;
              const s = m.index + lead;
              const e = m.index + m[0].length;
              applyDelimMark(s, s + openLen, e - closeLen, e, cls);
            }
          };
          IR_CODE_RE.lastIndex = 0;
          while ((m = IR_CODE_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const tLen = m[1].length;
            if (e - s > tLen * 2 && claimLiteral(s, e)) {
              addRange(line.from + s, line.from + s + tLen, hideDeco);
              addRange(line.from + s + tLen, line.from + e - tLen, Decoration.mark({ class: "cm-ir-inline-code" }));
              addRange(line.from + e - tLen, line.from + e, hideDeco);
            }
          }

          // Standalone Bilibili / YouTube embed preview widget
          const soloEmbed = matchStandaloneEmbedVideo(text);
          if (soloEmbed && claimLiteral(0, text.length)) {
            addRange(
              line.from,
              line.to,
              Decoration.replace({ widget: new EmbedVideoIRWidget(soloEmbed, line.from, line.to) })
            );
          }

          // Standalone Markdown video/audio link ([title](/uploads/demo.mp4))
          const soloMediaLink = !soloEmbed ? /^\s*\[([^\]\n]+)\]\(([^)\n]+)\)\s*$/.exec(text) : null;
          if (soloMediaLink) {
            const target = safeMediaSrc(parseMarkdownImageTarget(soloMediaLink[2] || ""), { allowRelative: true });
            if (target) {
              const mKind = isVideoMedia("", target) ? "video" : isAudioMedia("", target) ? "audio" : null;
              if (mKind && claimLiteral(0, text.length)) {
                addRange(
                  line.from,
                  line.to,
                  Decoration.replace({ widget: new MediaIRWidget(mKind, target, line.from, line.to) })
                );
              }
            }
          }

          // Footnote references [^id] (claim before ^sup^ and [label](url))
          refreshMask();
          IR_FN_REF_RE.lastIndex = 0;
          while ((m = IR_FN_REF_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (claimLiteral(s, e)) {
              addRange(line.from + s, line.from + e, Decoration.mark({ class: "cm-ir-sup cm-ir-link" }));
            }
          }

          // HTML inline <code>...</code>, <kbd>...</kbd>, <sup>...</sup>, <sub>...</sub>
          refreshMask();
          IR_HTML_TECH_RE.lastIndex = 0;
          while ((m = IR_HTML_TECH_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const tName = m[2].toLowerCase();
            const openEnd = s + m[1].length;
            const closeStart = e - m[4].length;
            const ok = tName === "code" ? claimLiteral(s, e) : claimDelims(s, openEnd, closeStart, e);
            if (ok) {
              const cls =
                tName === "code"
                  ? "cm-ir-inline-code"
                  : tName === "kbd"
                  ? "cm-ir-kbd"
                  : tName === "sup"
                  ? "cm-ir-sup"
                  : "cm-ir-sub";
              addRange(line.from + s, line.from + openEnd, hideDeco);
              addRange(line.from + openEnd, line.from + closeStart, Decoration.mark({ class: cls }));
              addRange(line.from + closeStart, line.from + e, hideDeco);
            }
          }

          // Inline <br> / <br/> / <br /> inside prose lines
          refreshMask();
          IR_BR_RE.lastIndex = 0;
          while ((m = IR_BR_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            if (claimLiteral(s, e)) {
              const trailingOnly = /^\s*$/.test(text.slice(e));
              addRange(
                line.from + s,
                line.from + e,
                trailingOnly ? hideDeco : Decoration.replace({ widget: new BrIRWidget() })
              );
            }
          }

          // Linear-time <font color="...">...</font> and <span style="color: ...">...</span>
          const runColorTagPass = (re: RegExp, fromStyle: boolean) => {
            refreshMask();
            re.lastIndex = 0;
            while ((m = re.exec(curMask)) !== null) {
              const s = m.index;
              const e = s + m[0].length;
              const rawAttr = readHtmlAttr(text.slice(s, s + m[1].length), fromStyle ? "style" : "color");
              const col = fromStyle ? (/^\s*color\s*:\s*([^;]+)\s*;?\s*$/i.exec(rawAttr)?.[1].trim() || "") : rawAttr;
              const openEnd = s + m[1].length;
              const closeStart = e - m[3].length;
              if (col && SAFE_IR_COLOR_RE.test(col) && claimDelims(s, openEnd, closeStart, e)) {
                addRange(line.from + s, line.from + openEnd, hideDeco);
                addRange(
                  line.from + openEnd,
                  line.from + closeStart,
                  Decoration.mark({
                    attributes: { style: `color: ${col};`, "data-volo-color": col.trim().toLowerCase() },
                  })
                );
                addRange(line.from + closeStart, line.from + e, hideDeco);
              }
            }
          };
          runColorTagPass(IR_FONT_RE, false);
          runColorTagPass(IR_SPAN_RE, true);

          // Highlight <mark>...</mark> & Underline <u>...</u>
          runPairPass(IR_MARK_RE, 6, 7, "cm-ir-mark");
          runPairPass(IR_U_RE, 3, 4, "cm-ir-underline");

          // Single-line HTML <a ...>...</a> links
          refreshMask();
          IR_HTML_LINK_RE.lastIndex = 0;
          while ((m = IR_HTML_LINK_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            applyDelimMark(s, s + m[1].length, e - m[3].length, e, "cm-ir-link");
          }

          // HTML inline formatting tags (<strong>/<b>, <em>/<i>, <s>/<del>/<strike>)
          refreshMask();
          IR_HTML_FMT_RE.lastIndex = 0;
          while ((m = IR_HTML_FMT_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const tagLow = m[2].toLowerCase();
            const cls =
              tagLow === "strong" || tagLow === "b"
                ? "cm-ir-bold"
                : tagLow === "em" || tagLow === "i"
                ? "cm-ir-italic"
                : "cm-ir-strike";
            applyDelimMark(s, s + m[1].length, e - m[4].length, e, cls);
          }

          // Linear-time <audio ...></audio> and <video ...></video> players on non-cursor lines
          const runMediaTagPass = (re: RegExp, kind: "audio" | "video") => {
            refreshMask();
            re.lastIndex = 0;
            while ((m = re.exec(curMask)) !== null) {
              const s = m.index;
              const e = s + m[0].length;
              const rawSrc = safeMediaSrc(readHtmlAttr(text.slice(s, s + m[1].length), "src"), { allowRelative: true });
              if (rawSrc && claimLiteral(s, e)) {
                addRange(line.from + s, line.from + e, Decoration.replace({ widget: new MediaIRWidget(kind, rawSrc, line.from + s, line.from + e) }));
              }
            }
          };
          runMediaTagPass(IR_AUDIO_RE, "audio");
          runMediaTagPass(IR_VIDEO_RE, "video");

          // Volo Wiki Links [[slug]] / [[slug|label]]
          refreshMask();
          IR_WIKI_RE.lastIndex = 0;
          while ((m = IR_WIKI_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const slug = text.slice(s + 2, s + 2 + m[1].length).trim();
            if (!slug) continue;
            const hasAlias = m[2] !== undefined;
            const openEnd = hasAlias ? s + 2 + m[1].length + 1 : s + 2;
            const closeStart = e - 2;
            if (openEnd < closeStart && claimDelims(s, openEnd, closeStart, e)) {
              addRange(line.from + s, line.from + openEnd, hideDeco);
              addRange(
                line.from + openEnd,
                line.from + closeStart,
                Decoration.mark({
                  class: "cm-ir-link cm-ir-wikilink",
                  attributes: { "data-wiki-slug": slug },
                })
              );
              addRange(line.from + closeStart, line.from + e, hideDeco);
            }
          }

          // Markdown Links [label](url) — delimiter-only occupied so [`code` link](url) and [Click **here**](url) compose!
          refreshMask();
          IR_LINK_RE.lastIndex = 0;
          while ((m = IR_LINK_RE.exec(curMask)) !== null) {
            const s = m.index;
            const e = s + m[0].length;
            const labelLen = m[1].length;
            const openEnd = s + 1;
            const closeStart = s + 1 + labelLen;
            if (claimDelims(s, openEnd, closeStart, e)) {
              for (let i = closeStart; i < e; i++) mChars[i] = "\x02";
              maskDirty = true;
              addRange(line.from + s, line.from + openEnd, hideDeco);
              addRange(line.from + openEnd, line.from + closeStart, Decoration.mark({ class: "cm-ir-link" }));
              addRange(line.from + closeStart, line.from + e, hideDeco);
            }
          }

          // Bare URL autolinks in prose (claim literal BEFORE math/strike/italic/sup/sub)
          refreshMask();
          IR_BARE_URL_RE.lastIndex = 0;
          while ((m = IR_BARE_URL_RE.exec(curMask)) !== null) {
            const s = m.index;
            let e = s + m[0].length;
            while (e > s && /[.,;:!?]$/.test(text[e - 1])) e--;
            if (e > s && claimLiteral(s, e)) {
              addRange(line.from + s, line.from + e, Decoration.mark({ class: "cm-ir-link" }));
            }
          }

          // Inline LaTeX math $...$ & \(...\) (atomic literal span)
          refreshMask();
          IR_INLINE_MATH_RE.lastIndex = 0;
          while ((m = IR_INLINE_MATH_RE.exec(curMask)) !== null) {
            const isParen = m[2] !== undefined;
            const lead = isParen || m[0].startsWith("$") ? 0 : 1;
            const s = m.index + lead;
            const e = m.index + m[0].length;
            const rawInner = isParen ? text.slice(s + 2, e - 2) : text.slice(s + 1, e - 1);
            const expr = rawInner.trim();
            if (expr && (isParen || isLikelyInlineMath(expr)) && claimLiteral(s, e)) {
              addRange(
                line.from + s,
                line.from + e,
                Decoration.replace({
                  widget: new MathInlineIRWidget(expr, line.from + s, line.from + e),
                })
              );
            }
          }

          // Extended Highlight ==...==, Bold **...**, Strike ~~...~~, Italic *...*, Sup ^...^, Sub ~...~
          runPairPass(IR_EQ_MARK_RE, 2, 2, "cm-ir-mark", "=");
          runPairPass(IR_BOLD_RE, 2, 2, "cm-ir-bold");
          runPairPass(IR_STRIKE_RE, 2, 2, "cm-ir-strike");
          runPairPass(IR_ITALIC_RE, 1, 1, "cm-ir-italic", "*");
          runPairPass(IR_SUP_RE, 1, 1, "cm-ir-sup", "^");
          runPairPass(IR_SUB_RE, 1, 1, "cm-ir-sub", "~");
        }

        pos = line.to + 1;
      }
    }
    return Decoration.set(allRanges, true);
  };

  const irPlugin = ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      activeLine: number;
      activeLineHasImg: boolean;
      selFromLine: number;
      selToLine: number;
      wasComposing: boolean;
      headOffsetY: number | null = null;
      measuredScrollTop = 0;
      currentScrollTop = 0;
      lastPointerClientY: number | null = null;
      lastPointerTime = 0;
      destroyed = false;
      readonly view: EditorView;
      readonly onMouseDown: (e: MouseEvent) => void;
      readonly onScroll: () => void;
      readonly onWheel: () => void;

      constructor(view: EditorView) {
        this.view = view;
        activeIRViews.add(view);
        this.wasComposing = view.composing;
        this.decorations = buildDecos(view);
        const ln = view.state.doc.lineAt(view.state.selection.main.head);
        const trackInit = view.hasFocus || view.composing;
        this.selFromLine = trackInit ? view.state.doc.lineAt(view.state.selection.main.from).number : -1;
        this.selToLine = trackInit ? view.state.doc.lineAt(view.state.selection.main.to).number : -1;
        this.activeLine = view.hasFocus ? ln.number : -1;
        this.activeLineHasImg = view.hasFocus ? lineHasIRTallWidget(view.state, ln.number, ln.text) : false;
        this.currentScrollTop = view.scrollDOM.scrollTop;
        this.measuredScrollTop = this.currentScrollTop;

        this.onMouseDown = (e: MouseEvent) => {
          if (e.button === 0) {
            this.lastPointerClientY = e.clientY;
            this.lastPointerTime = performance.now();
          }
        };
        this.onScroll = () => {
          this.currentScrollTop = view.scrollDOM.scrollTop;
          lastIRScrollActivityAt = performance.now();
        };
        this.onWheel = () => {
          lastIRScrollActivityAt = performance.now();
        };
        view.dom.addEventListener("mousedown", this.onMouseDown, true);
        view.scrollDOM.addEventListener("scroll", this.onScroll, { passive: true });
        view.scrollDOM.addEventListener("wheel", this.onWheel, { passive: true });
        this.scheduleHeadSample(view);
        if (view.hasFocus !== (view.state.field(irFocusedField, false) ?? false)) {
          const nextFoc = view.hasFocus;
          queueMicrotask(() => {
            if (!this.destroyed && !view.composing) {
              view.dispatch({ effects: irFocusEffect.of(nextFoc) });
            }
          });
        }
      }

      scheduleHeadSample(view: EditorView) {
        view.requestMeasure({
          read: (v) => {
            const scroller = v.scrollDOM;
            if (scroller.clientHeight <= 0) return null;
            const coords = v.coordsAtPos(v.state.selection.main.head);
            if (!coords) return null;
            const rect = scroller.getBoundingClientRect();
            return {
              offsetY: (coords.top + coords.bottom) / 2 - rect.top,
              scrollTop: scroller.scrollTop,
            };
          },
          write: (m) => {
            if (!m) return;
            this.headOffsetY = m.offsetY;
            this.measuredScrollTop = m.scrollTop;
            this.currentScrollTop = m.scrollTop;
          },
        });
      }

      update(update: ViewUpdate) {
        const state = update.state;
        const focused = update.view.hasFocus;
        const composingChanged = update.view.composing !== this.wasComposing;
        this.wasComposing = update.view.composing;
        if (update.focusChanged) {
          queueMicrotask(() => {
            if (
              !this.destroyed &&
              !update.view.composing &&
              (update.view.state.field(irFocusedField, false) ?? false) !== update.view.hasFocus
            ) {
              update.view.dispatch({ effects: irFocusEffect.of(update.view.hasFocus) });
            }
          });
        }

        const head = state.selection.main.head;
        const nextLine = state.doc.lineAt(head);
        const trackSel = focused || update.view.composing;
        const nextSelFromLine = trackSel ? state.doc.lineAt(state.selection.main.from).number : -1;
        const nextSelToLine = trackSel ? state.doc.lineAt(state.selection.main.to).number : -1;
        const selLinesChanged = nextSelFromLine !== this.selFromLine || nextSelToLine !== this.selToLine;
        const nextLineNo = focused ? nextLine.number : -1;
        const prevLineNo = this.activeLine;
        const prevHadImg = this.activeLineHasImg;
        const nextHasImg = focused ? lineHasIRTallWidget(state, nextLine.number, nextLine.text) : false;
        const lineChanged = nextLineNo !== prevLineNo;
        const searchChanged = update.transactions.some((tr) => tr.effects.some((e) => e.is(setSearchStateEffect)));

        if (
          update.docChanged ||
          update.viewportChanged ||
          (update.selectionSet && selLinesChanged) ||
          update.focusChanged ||
          composingChanged ||
          searchChanged
        ) {
          this.decorations = buildDecos(update.view);
        }

        this.selFromLine = nextSelFromLine;
        this.selToLine = nextSelToLine;
        this.activeLine = nextLineNo;
        this.activeLineHasImg = nextHasImg;

        const clickedWidgetY = pendingIRClickClientY;
        pendingIRClickClientY = null;

        const transitionedImg = !update.docChanged
          ? lineChanged && (prevHadImg || nextHasImg)
          : lineChanged &&
            prevHadImg &&
            !nextHasImg &&
            update.transactions.some((tr) => tr.isUserEvent("input"));

        if (transitionedImg && focused && !update.view.composing) {
          const isPointer =
            clickedWidgetY !== null ||
            update.transactions.some((tr) => tr.isUserEvent("select.pointer")) ||
            (this.lastPointerClientY !== null && performance.now() - this.lastPointerTime < 180);
          const pointerY = clickedWidgetY ?? (isPointer ? this.lastPointerClientY : null);
          const prevOffsetY =
            this.headOffsetY !== null
              ? this.headOffsetY - (this.currentScrollTop - this.measuredScrollTop)
              : null;

          update.view.requestMeasure({
            read: () => true,
            write: (_, view) => {
              queueMicrotask(() => {
                if (this.destroyed || view.composing) return;
                const scroller = view.scrollDOM;
                const h = scroller.clientHeight;
                if (h <= 0) return;
                const rect = scroller.getBoundingClientRect();
                const coords = view.coordsAtPos(view.state.selection.main.head);
                if (!coords) return;

                const curOffsetY = (coords.top + coords.bottom) / 2 - rect.top;
                const padTop = Math.min(44, h * 0.2);
                const padBot = Math.min(52, h * 0.2);
                let desiredOffsetY = curOffsetY;

                if (pointerY !== null) {
                  desiredOffsetY = Math.max(padTop, Math.min(h - padBot, pointerY - rect.top));
                } else if (prevLineNo > 0 && nextLine.number < prevLineNo && nextHasImg) {
                  desiredOffsetY =
                    prevOffsetY !== null
                      ? Math.max(padTop, Math.min(h - padBot, prevOffsetY - 26))
                      : Math.max(padTop, Math.min(h - padBot, curOffsetY));
                } else if (prevLineNo > 0 && nextLine.number > prevLineNo && prevHadImg) {
                  const maxComfortY = Math.max(padTop, Math.min(h - padBot, h * 0.62));
                  if (curOffsetY > maxComfortY) {
                    desiredOffsetY =
                      prevOffsetY !== null
                        ? Math.max(padTop, Math.min(maxComfortY, prevOffsetY + 28))
                        : maxComfortY;
                  } else if (curOffsetY < padTop) {
                    desiredOffsetY = padTop;
                  }
                } else {
                  desiredOffsetY = Math.max(padTop, Math.min(h - padBot, curOffsetY));
                }

                const prevScroll = scroller.scrollTop;
                const maxScroll = Math.max(0, scroller.scrollHeight - h);
                const targetScroll = Math.max(
                  0,
                  Math.min(maxScroll, Math.round(prevScroll + (curOffsetY - desiredOffsetY)))
                );
                if (Math.abs(targetScroll - prevScroll) >= 2) {
                  scroller.scrollTop = targetScroll;
                }
                this.measuredScrollTop = scroller.scrollTop;
                this.currentScrollTop = this.measuredScrollTop;
                this.headOffsetY = curOffsetY - (this.measuredScrollTop - prevScroll);
              });
            },
          });
        } else if (update.selectionSet || update.docChanged || update.viewportChanged) {
          this.scheduleHeadSample(update.view);
        }
      }

      destroy() {
        this.destroyed = true;
        activeIRViews.delete(this.view);
        this.view.dom.removeEventListener("mousedown", this.onMouseDown, true);
        this.view.scrollDOM.removeEventListener("scroll", this.onScroll);
        this.view.scrollDOM.removeEventListener("wheel", this.onWheel);
      }
    },
    { decorations: (v) => v.decorations }
  );

  return [irFocusedField, irBlockDecoField, irPlugin];
}

const MAX_IR_IMG_W = 560;
const MAX_IR_IMG_H = 260;
const DEFAULT_IR_IMG_PENDING_W = 320;
const DEFAULT_IR_IMG_PENDING_H = 180;
const irImageDims = new Map<string, { w: number; h: number }>();
const irImageBroken = new Set<string>();
const irImageProbing = new Set<string>();
const activeIRViews = new Set<EditorView>();
let lastIRScrollActivityAt = 0;
let pendingWarmFlushTimer: number | null = null;
let lastWarmReadySrc = "";
let pendingIRClickClientY: number | null = null;

function setIRImageDim(src: string, w: number, h: number): void {
  if (!src || !(w > 0) || !(h > 0)) return;
  irImageBroken.delete(src);
  if (!irImageDims.has(src) && irImageDims.size >= 256) {
    const oldest = irImageDims.keys().next().value;
    if (oldest !== undefined) irImageDims.delete(oldest);
  }
  irImageDims.set(src, { w, h });
}

function scheduleIdleIRImgFlush(src: string): void {
  lastWarmReadySrc = src;
  if (pendingWarmFlushTimer !== null) return;
  const run = () => {
    pendingWarmFlushTimer = null;
    if (activeIRViews.size === 0) return;
    const idleFor = performance.now() - lastIRScrollActivityAt;
    if (idleFor < 220) {
      pendingWarmFlushTimer = window.setTimeout(run, Math.ceil(230 - idleFor));
      return;
    }
    const readySrc = lastWarmReadySrc;
    for (const v of activeIRViews) {
      if (v.dom.isConnected && !v.composing) {
        v.dispatch({ effects: irImgDimReadyEffect.of(readySrc) });
      }
    }
  };
  pendingWarmFlushTimer = window.setTimeout(run, 230);
}

function seedIRImageHintDims(src: string, htmlTag?: string): void {
  if (!src || irImageDims.has(src)) return;
  if (htmlTag) {
    const w = Number(readHtmlAttr(htmlTag, "width"));
    const h = Number(readHtmlAttr(htmlTag, "height"));
    if (Number.isFinite(w) && Number.isFinite(h) && w > 0 && h > 0) {
      setIRImageDim(src, w, h);
      return;
    }
  }
  const qiniuM = /imageView2\/\d+\/w\/(\d+)\/h\/(\d+)/i.exec(src);
  if (qiniuM) {
    const w = Number(qiniuM[1]);
    const h = Number(qiniuM[2]);
    if (w > 0 && h > 0) {
      setIRImageDim(src, w, h);
      return;
    }
  }
  const kvW = /[?&](?:w|width)=(\d+)/i.exec(src);
  const kvH = /[?&](?:h|height)=(\d+)/i.exec(src);
  if (kvW && kvH) {
    const w = Number(kvW[1]);
    const h = Number(kvH[1]);
    if (w > 0 && h > 0) {
      setIRImageDim(src, w, h);
    }
  }
}

function warmIRImageDim(src: string): void {
  if (
    !src ||
    irImageDims.has(src) ||
    irImageBroken.has(src) ||
    irImageProbing.has(src) ||
    irImageProbing.size >= 48 ||
    src.startsWith("data:")
  ) {
    return;
  }
  irImageProbing.add(src);
  const probe = new Image();
  if (!/^(?:\/(?!\/)|data:|blob:)/i.test(src)) {
    probe.referrerPolicy = "no-referrer";
  }
  probe.decoding = "async";
  probe.onload = () => {
    irImageProbing.delete(src);
    if (probe.naturalWidth > 0 && probe.naturalHeight > 0) {
      setIRImageDim(src, probe.naturalWidth, probe.naturalHeight);
      scheduleIdleIRImgFlush(src);
    }
  };
  probe.onerror = () => {
    irImageProbing.delete(src);
    if (irImageBroken.size >= 256) {
      const oldest = irImageBroken.values().next().value;
      if (oldest !== undefined) irImageBroken.delete(oldest);
    }
    irImageBroken.add(src);
    scheduleIdleIRImgFlush(src);
  };
  probe.src = src;
}

function computeIRImgBox(src: string): { w: number; h: number } | null {
  const nat = irImageDims.get(src);
  if (!nat || nat.w <= 0 || nat.h <= 0) return null;
  const scale = Math.min(1, MAX_IR_IMG_W / nat.w, MAX_IR_IMG_H / nat.h);
  return {
    w: Math.max(1, Math.round(nat.w * scale)),
    h: Math.max(1, Math.round(nat.h * scale)),
  };
}

function lineHasIRTallWidget(state: EditorState, lineNumber: number, text: string): boolean {
  if (getIRDocScan(state.doc).tableLines.has(lineNumber)) return true;
  if (text.length === 0 || text.length > 2400) return false;
  if (/!\[[^[\]\n]*\]\((?!\))[^()\n]*(?:\([^()\n]*\)[^()\n]*)*\)/.test(text) || /<(?:img|video|audio)\b[^<>]*?\bsrc\s*=/i.test(text)) {
    return true;
  }
  if (/^\s*\[toc\]\s*$/i.test(text) || matchStandaloneEmbedVideo(text) !== null) {
    return true;
  }
  const soloLink = /^\s*\[([^[\]\n]+)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)\s*$/.exec(text);
  if (soloLink) {
    const target = parseMarkdownImageTarget(soloLink[2] || "");
    if (isVideoMedia("", target) || isAudioMedia("", target)) return true;
  }
  return false;
}

class BrIRWidget extends WidgetType {
  eq(): boolean {
    return true;
  }
  toDOM(): HTMLElement {
    return mkEl("br");
  }
}

class BulletIRWidget extends WidgetType {
  eq(): boolean {
    return true;
  }
  toDOM(): HTMLElement {
    const span = mkEl("span", undefined, "• ");
    span.style.color = "var(--volo-accent)";
    span.style.fontWeight = "700";
    return span;
  }
}

function appendSafeCellMarkdown(host: HTMLElement, raw: string): void {
  const parts = raw.split(/<br\s*\/?>/gi);
  parts.forEach((seg, idx) => {
    if (idx > 0) host.appendChild(mkEl("br"));
    const tokenRe =
      /!\[([^[\]\n]*)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)|\[([^[\]\n]+)\]\((?!\))([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)|(?<!`)(`+)(?!`)([^\n]+?)(?<!`)\5(?!`)|(\bhttps?:\/\/[^\s<>()[\]"']+)|\$(?!\s|\$)([^\n$]*?[^\s\\$])\$(?![0-9a-zA-Z\\$])|\*\*([^*\n]+)\*\*|==([^=\n]+)==|~~([^~\n]+)~~|\*([^*\s][^*\n]*?)\*|\^([^\s^\n[\]]+)\^|~([^\s~\n/]+)~|<(u|mark|kbd|sup|sub|strong|b|em|i|s|del)>([^<\n]+)<\/\15>|(<a\b(?:"[^"<>]*"|'[^'<>]*'|[^"'<>])*?>)([^<\n]+)<\/a>/gi;
    let last = 0;
    let m: RegExpExecArray | null;
    while ((m = tokenRe.exec(seg)) !== null) {
      if (m.index > last) {
        host.appendChild(document.createTextNode(seg.slice(last, m.index)));
      }
      if (m[2] !== undefined) {
        const src = resolveIRImageSrc(parseMarkdownImageTarget(m[2]));
        if (src) {
          const img = mkEl("img");
          img.src = src;
          img.alt = m[1] || "";
          img.style.maxHeight = "64px";
          host.appendChild(img);
        }
      } else if (m[3] !== undefined && m[4] !== undefined) {
        const a = mkEl("a");
        const href = safeHref(parseMarkdownImageTarget(m[4]), { allowMailto: true });
        if (href) a.href = href;
        appendSafeCellMarkdown(a, m[3]);
        host.appendChild(a);
      } else if (m[6] !== undefined) {
        const codeTxt = m[6];
        const c = mkEl(
          "code",
          "cm-ir-inline-code",
          codeTxt.length >= 2 && codeTxt.startsWith(" ") && codeTxt.endsWith(" ") && codeTxt.trim()
            ? codeTxt.slice(1, -1)
            : codeTxt
        );
        host.appendChild(c);
      } else if (m[7] !== undefined) {
        let urlStr = m[7];
        let trail = "";
        while (urlStr.length > 0 && /[.,;:!?]$/.test(urlStr[urlStr.length - 1])) {
          trail = urlStr[urlStr.length - 1] + trail;
          urlStr = urlStr.slice(0, -1);
        }
        const safeAuto = safeHref(urlStr, { allowRelative: false });
        if (safeAuto) {
          const a = mkEl("a", undefined, urlStr);
          a.href = safeAuto;
          host.appendChild(a);
        } else {
          host.appendChild(document.createTextNode(urlStr));
        }
        if (trail) host.appendChild(document.createTextNode(trail));
      } else if (m[8] !== undefined) {
        const expr = m[8].trim();
        if (isLikelyInlineMath(expr)) {
          const mathSpan = mkEl("span", "cm-ir-math-inline", expr);
          void ensureEditorKaTeX()
            .then((katex) => {
              if (katex && mathSpan.isConnected) {
                katex.render(expr, mathSpan, { displayMode: false, throwOnError: false });
              }
            })
            .catch(() => {});
          host.appendChild(mathSpan);
        } else {
          host.appendChild(document.createTextNode(m[0]));
        }
      } else if (m[9] !== undefined) {
        const b = mkEl("strong");
        appendSafeCellMarkdown(b, m[9]);
        host.appendChild(b);
      } else if (m[10] !== undefined) {
        const mk = mkEl("mark");
        appendSafeCellMarkdown(mk, m[10]);
        host.appendChild(mk);
      } else if (m[11] !== undefined) {
        host.appendChild(mkEl("del", undefined, m[11]));
      } else if (m[12] !== undefined) {
        host.appendChild(mkEl("em", undefined, m[12]));
      } else if (m[13] !== undefined) {
        host.appendChild(mkEl("sup", undefined, m[13]));
      } else if (m[14] !== undefined) {
        host.appendChild(mkEl("sub", undefined, m[14]));
      } else if (m[15] !== undefined && m[16] !== undefined) {
        const tag = m[15].toLowerCase();
        host.appendChild(
          mkEl((tag === "b" ? "strong" : tag === "i" ? "em" : tag === "s" ? "del" : tag) as keyof HTMLElementTagNameMap, undefined, m[16])
        );
      } else if (m[18] !== undefined) {
        const href = safeHref(readHtmlAttr(m[17], "href"), { allowMailto: true });
        const a = mkEl("a", undefined, m[18]);
        if (href) a.href = href;
        host.appendChild(a);
      }
      last = m.index + m[0].length;
    }
    if (last < seg.length) {
      host.appendChild(document.createTextNode(seg.slice(last)));
    }
  });
}

const SAFE_IR_TABLE_TAGS = new Set([
  "table",
  "thead",
  "tbody",
  "tfoot",
  "tr",
  "th",
  "td",
  "caption",
  "a",
  "strong",
  "b",
  "em",
  "i",
  "u",
  "s",
  "del",
  "strike",
  "code",
  "kbd",
  "mark",
  "sup",
  "sub",
  "br",
  "span",
  "font",
  "img",
  "p",
  "div",
]);

function cloneSanitizedTableNode(node: Node, rowCounter: { idx: number }): Node | null {
  if (node.nodeType === Node.TEXT_NODE) {
    return document.createTextNode(node.textContent || "");
  }
  if (node.nodeType !== Node.ELEMENT_NODE) return null;
  const srcEl = node as HTMLElement;
  const tag = srcEl.tagName.toLowerCase();
  if (tag === "script" || tag === "style" || tag === "iframe" || tag === "object") return null;

  if (!SAFE_IR_TABLE_TAGS.has(tag)) {
    const frag = document.createDocumentFragment();
    srcEl.childNodes.forEach((ch) => {
      const c = cloneSanitizedTableNode(ch, rowCounter);
      if (c) frag.appendChild(c);
    });
    return frag;
  }

  const outTag = tag === "p" || tag === "div" ? "span" : tag;
  const out = document.createElement(outTag);
  if (tag === "tr") {
    out.setAttribute("data-row-idx", String(rowCounter.idx++));
  } else if (tag === "th" || tag === "td") {
    const cs = srcEl.getAttribute("colspan");
    const rs = srcEl.getAttribute("rowspan");
    const al = (srcEl.style?.textAlign || srcEl.getAttribute("align") || "").toLowerCase().trim();
    if (cs && /^\d{1,2}$/.test(cs)) out.setAttribute("colspan", cs);
    if (rs && /^\d{1,2}$/.test(rs)) out.setAttribute("rowspan", rs);
    if (al === "left" || al === "center" || al === "right") out.style.textAlign = al;
  } else if (tag === "a") {
    const href = safeHref(srcEl.getAttribute("href") || "", { allowMailto: true });
    if (href) {
      out.setAttribute("href", href);
    }
  } else if (tag === "img") {
    const safeSrc = resolveIRImageSrc(srcEl.getAttribute("src") || "");
    if (!safeSrc) return null;
    out.setAttribute("src", safeSrc);
    out.setAttribute("alt", srcEl.getAttribute("alt") || "");
    out.style.maxHeight = "72px";
  } else if (tag === "font" || tag === "span") {
    const col = (srcEl.getAttribute("color") || srcEl.style?.color || "").trim();
    if (col && SAFE_IR_COLOR_RE.test(col)) out.style.color = col;
  }

  srcEl.childNodes.forEach((ch) => {
    const c = cloneSanitizedTableNode(ch, rowCounter);
    if (c) out.appendChild(c);
  });
  return out;
}

abstract class BaseIRWidget extends WidgetType {
  ignoreEvent(): boolean {
    return true;
  }
}

class TableIRWidget extends BaseIRWidget {
  constructor(
    readonly raw: string,
    readonly kind: "html" | "md",
    readonly pos: number,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: TableIRWidget): boolean {
    return (
      this.raw === other.raw &&
      this.kind === other.kind &&
      this.pos === other.pos &&
      this.endPos === other.endPos
    );
  }
  get estimatedHeight(): number {
    const rows =
      this.kind === "md"
        ? Math.max(1, this.raw.split("\n").length - 1)
        : Math.max(1, (this.raw.match(/<tr\b/gi) || []).length);
    return Math.min(520, rows * 38 + 16);
  }
  toDOM(view: EditorView): HTMLElement {
    const host = mkEl("div", "cm-ir-table-host");
    const scroller = mkEl("div", "volo-table-scroll");

    if (this.kind === "html") {
      const parsed = new DOMParser().parseFromString(this.raw, "text/html");
      const srcTbl = parsed.querySelector("table");
      if (srcTbl) {
        const cleanTbl = cloneSanitizedTableNode(srcTbl, { idx: 0 });
        if (cleanTbl) scroller.appendChild(cleanTbl);
      }
    } else {
      const lines = this.raw.split("\n");
      const tbl = mkEl("table");
      const headCells = splitGfmTableRow(lines[0] || "");
      const sepCells = splitGfmTableRow(lines[1] || "");
      const aligns = sepCells.map((c) => {
        const tStr = c.trim();
        return tStr.startsWith(":") && tStr.endsWith(":") ? "center" : tStr.endsWith(":") ? "right" : "left";
      });
      const cols = Math.max(1, headCells.length, aligns.length);
      const buildRow = (tag: "th" | "td", cells: string[], rowIdx: number) => {
        const tr = mkEl("tr");
        tr.setAttribute("data-row-idx", String(rowIdx));
        for (let ci = 0; ci < cols; ci++) {
          const cell = mkEl(tag);
          if (aligns[ci]) cell.style.textAlign = aligns[ci];
          appendSafeCellMarkdown(cell, cells[ci] || "");
          tr.appendChild(cell);
        }
        return tr;
      };
      const thead = mkEl("thead");
      thead.appendChild(buildRow("th", headCells, 0));
      tbl.appendChild(thead);

      if (lines.length > 2) {
        const tbody = mkEl("tbody");
        for (let ri = 2; ri < lines.length; ri++) {
          tbody.appendChild(buildRow("td", splitGfmTableRow(lines[ri]), ri - 1));
        }
        tbl.appendChild(tbody);
      }
      scroller.appendChild(tbl);
    }

    host.appendChild(scroller);

    host.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      const targetEl = e.target as HTMLElement | null;
      const linkEl = targetEl?.closest("a[href]") as HTMLAnchorElement | null;
      if (linkEl && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        const href = safeHref(linkEl.getAttribute("href") || "");
        if (href) {
          window.open(href, "_blank", "noopener,noreferrer");
        }
        return;
      }
      e.preventDefault();
      const trEl = targetEl?.closest("tr[data-row-idx]") as HTMLElement | null;
      const rowIdx = trEl ? Math.max(0, Number(trEl.getAttribute("data-row-idx") || 0)) : 0;

      let targetPos = Math.min(this.pos, view.state.doc.length);
      if (this.kind === "md") {
        const startLn = view.state.doc.lineAt(targetPos).number;
        const lineOffset = rowIdx === 0 ? 0 : rowIdx + 1;
        const targetLn = Math.min(view.state.doc.lines, startLn + lineOffset);
        targetPos = view.state.doc.line(targetLn).to;
      } else {
        const trRe = /<tr\b/gi;
        let seen = 0;
        let m: RegExpExecArray | null;
        while ((m = trRe.exec(this.raw)) !== null) {
          if (seen === rowIdx) {
            targetPos = Math.min(this.pos + m.index, view.state.doc.length);
            break;
          }
          seen++;
        }
      }

      focusIRWidgetPos(view, targetPos, e.clientY);
    });

    return host;
  }
}

class ImageIRWidget extends BaseIRWidget {
  readonly boxKey: string;
  constructor(
    readonly src: string,
    readonly rawSrc: string,
    readonly alt: string,
    readonly pos: number,
    readonly endPos: number
  ) {
    super();
    const box = computeIRImgBox(src);
    this.boxKey = irImageBroken.has(src) ? "broken" : box ? `${box.w}x${box.h}` : "pending";
  }
  eq(other: ImageIRWidget): boolean {
    return (
      this.src === other.src &&
      this.rawSrc === other.rawSrc &&
      this.alt === other.alt &&
      this.pos === other.pos &&
      this.endPos === other.endPos &&
      this.boxKey === other.boxKey
    );
  }
  get estimatedHeight(): number {
    if (irImageBroken.has(this.src)) return 34;
    const box = computeIRImgBox(this.src);
    return (box ? box.h : DEFAULT_IR_IMG_PENDING_H) + 10;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("span", "cm-ir-img-wrap");
    wrap.title = this.alt ? `${this.alt} (${this.rawSrc})` : this.rawSrc;

    const fb = mkEl("span", "cm-ir-img-fallback", this.alt ? `🖼 ${this.alt} · ${this.rawSrc}` : `🖼 ${this.rawSrc}`);

    if (irImageBroken.has(this.src)) {
      fb.hidden = false;
      wrap.appendChild(fb);
      bindIRFocusMouseDown(wrap, view, this.endPos);
      return wrap;
    }

    fb.hidden = true;
    const img = mkEl("img", "cm-ir-img");
    if (this.src && !/^(?:\/(?!\/)|data:|blob:)/i.test(this.src)) {
      img.referrerPolicy = "no-referrer";
    }
    const cached = computeIRImgBox(this.src);
    if (cached) {
      wrap.classList.add("is-sized");
      wrap.style.width = `${cached.w}px`;
      wrap.style.aspectRatio = `${cached.w} / ${cached.h}`;
      img.width = cached.w;
      img.height = cached.h;
      img.decoding = "sync";
    } else {
      wrap.classList.add("is-loading");
      wrap.style.width = `${DEFAULT_IR_IMG_PENDING_W}px`;
      wrap.style.aspectRatio = `${DEFAULT_IR_IMG_PENDING_W} / ${DEFAULT_IR_IMG_PENDING_H}`;
      img.decoding = "async";
    }
    img.src = this.src;
    img.alt = this.alt;

    img.onload = () => {
      if (img.naturalWidth > 0 && img.naturalHeight > 0) {
        setIRImageDim(this.src, img.naturalWidth, img.naturalHeight);
      }
      if (cached) return;
      const box = computeIRImgBox(this.src);
      const commitLoadedSize = () => {
        if (!view.dom.isConnected) return;
        const idleFor = performance.now() - lastIRScrollActivityAt;
        if (idleFor < 220) {
          window.setTimeout(commitLoadedSize, Math.ceil(230 - idleFor));
          return;
        }
        if (!wrap.isConnected) {
          if (!view.composing) {
            view.dispatch({ effects: irImgDimReadyEffect.of(this.src) });
          }
          return;
        }
        view.requestMeasure({
          read: (v) => {
            const wrapRect = wrap.getBoundingClientRect();
            const scrollerRect = v.scrollDOM.getBoundingClientRect();
            return {
              oldH: wrapRect.height,
              aboveViewport: wrapRect.bottom <= scrollerRect.top + 48,
            };
          },
          write: (m, v) => {
            wrap.classList.remove("is-loading");
            if (box) {
              wrap.classList.add("is-sized");
              wrap.style.width = `${box.w}px`;
              wrap.style.aspectRatio = `${box.w} / ${box.h}`;
              img.width = box.w;
              img.height = box.h;
            } else {
              wrap.style.width = "";
              wrap.style.aspectRatio = "";
            }
            const newH = wrap.getBoundingClientRect().height;
            if (m.aboveViewport && Math.abs(newH - m.oldH) >= 1) {
              v.scrollDOM.scrollTop += Math.round(newH - m.oldH);
            }
            queueMicrotask(() => {
              if (v.dom.isConnected && !v.composing) {
                v.dispatch({ effects: irImgDimReadyEffect.of(this.src) });
              }
            });
          },
        });
      };
      commitLoadedSize();
    };

    img.onerror = () => {
      if (irImageBroken.size >= 256) {
        const oldest = irImageBroken.values().next().value;
        if (oldest !== undefined) irImageBroken.delete(oldest);
      }
      irImageBroken.add(this.src);
      wrap.classList.remove("is-loading", "is-sized");
      wrap.style.width = "";
      wrap.style.height = "";
      wrap.style.aspectRatio = "";
      img.hidden = true;
      fb.hidden = false;
      if (view.dom.isConnected) {
        view.requestMeasure();
        queueMicrotask(() => {
          if (view.dom.isConnected && !view.composing) {
            view.dispatch({ effects: irImgDimReadyEffect.of(this.src) });
          }
        });
      }
    };

    bindIRFocusMouseDown(wrap, view, this.endPos);

    wrap.appendChild(img);
    wrap.appendChild(fb);
    return wrap;
  }
}

class FenceLangIRWidget extends BaseIRWidget {
  constructor(
    readonly openLineNo: number,
    readonly explicitLang: string,
    readonly autoLang: string
  ) {
    super();
  }
  eq(other: FenceLangIRWidget): boolean {
    return (
      this.openLineNo === other.openLineNo &&
      this.explicitLang === other.explicitLang &&
      this.autoLang === other.autoLang
    );
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("span");
    const select = mkEl("select", "cm-ir-fence-lang-select");
    select.setAttribute("aria-label", t("ed.code_lang_ph"));
    populateCodeLangSelect(select, this.explicitLang, this.autoLang, t("ed.code_plain"));
    select.addEventListener("mousedown", (e) => {
      e.stopPropagation();
    });
    select.addEventListener("change", (e) => {
      e.stopPropagation();
      if (this.openLineNo < 1 || this.openLineNo > view.state.doc.lines) return;
      const ln = view.state.doc.line(this.openLineNo);
      const info = matchFenceOpen(ln.text);
      if (!info) return;
      const lead = ln.text.slice(0, info.leadSpaces);
      const fence = info.fenceChar.repeat(info.fenceLen);
      const nextLang = select.value.trim().toLowerCase();
      const nextLineText = `${lead}${fence}${nextLang}`;
      view.dispatch({
        changes: { from: ln.from, to: ln.to, insert: nextLineText },
        userEvent: "input",
      });
    });
    wrap.appendChild(select);
    return wrap;
  }
}

class FenceCopyIRWidget extends BaseIRWidget {
  constructor(
    readonly openLineNo: number,
    readonly closeLineNo: number
  ) {
    super();
  }
  eq(other: FenceCopyIRWidget): boolean {
    return this.openLineNo === other.openLineNo && this.closeLineNo === other.closeLineNo;
  }
  toDOM(view: EditorView): HTMLElement {
    const bar = mkEl("span", "cm-ir-fence-bar");
    const copyTxt = t("ed.copy_code");
    const copiedTxt = t("ed.copied");
    const btn = mkEl("button", "cm-ir-fence-btn", copyTxt);
    btn.type = "button";
    btn.addEventListener("mousedown", (e) => {
      e.preventDefault();
      e.stopPropagation();
    });
    btn.addEventListener("click", (e) => {
      e.preventDefault();
      e.stopPropagation();
      const doc = view.state.doc;
      const startLn = this.openLineNo + 1;
      const endLn = Math.min(doc.lines, this.closeLineNo - 1);
      const qDepth = getIRDocScan(doc).quoteDepth[this.openLineNo] || 0;
      let code = "";
      if (startLn <= endLn && startLn >= 1) {
        if (qDepth > 0) {
          const outLines: string[] = [];
          for (let ln = startLn; ln <= endLn; ln++) {
            outLines.push(stripQuotePrefix(doc.line(ln).text, qDepth).stripped);
          }
          code = outLines.join("\n");
        } else {
          code = doc.sliceString(doc.line(startLn).from, doc.line(endLn).to);
        }
      }
      triggerCopyBtnFeedback(btn, code, copyTxt, copiedTxt);
    });
    bar.appendChild(btn);
    return bar;
  }
}

const CALLOUT_PILL_ICONS: Record<string, string> = {
  note: "ℹ",
  tip: "💡",
  important: "✦",
  warning: "⚠",
  caution: "⛔",
};

class CalloutPillIRWidget extends BaseIRWidget {
  constructor(
    readonly kind: string,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: CalloutPillIRWidget): boolean {
    return this.kind === other.kind && this.endPos === other.endPos;
  }
  toDOM(view: EditorView): HTMLElement {
    const icon = CALLOUT_PILL_ICONS[this.kind] || "ℹ";
    const pill = mkEl("span", "cm-ir-callout-pill", `${icon} ${t(`ed.callout_${this.kind}`)}`);
    bindIRFocusMouseDown(pill, view, this.endPos, false);
    return pill;
  }
}

/** Compact "↗ host" chip that stands in for the hidden `](href)` tail of a linked image `[![alt](src)](href)`. */
class LinkedImageBadgeIRWidget extends BaseIRWidget {
  constructor(
    readonly href: string,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: LinkedImageBadgeIRWidget): boolean {
    return this.href === other.href && this.endPos === other.endPos;
  }
  toDOM(view: EditorView): HTMLElement {
    let label = this.href;
    const hm = /^(?:https?:)?\/\/([^/?#]+)(.*)$/i.exec(this.href);
    if (hm) {
      const rest = trimEndChars(hm[2], "/");
      label = hm[1].replace(/^www\./i, "") + rest;
    }
    if (label.length > 42) label = label.slice(0, 41) + "…";
    const chip = mkEl("span", "cm-ir-img-link-badge", `↗ ${label}`);
    chip.title = this.href;
    bindIRFocusMouseDown(chip, view, this.endPos, false);
    return chip;
  }
}

interface StandaloneEmbedInfo {
  provider: "Bilibili" | "YouTube";
  videoId: string;
  url: string;
  label: string;
}

function matchStandaloneEmbedVideo(text: string): StandaloneEmbedInfo | null {
  const trimmed = text.trim();
  if (!trimmed || !trimmed.includes("://")) {
    return null;
  }
  let label = "";
  let rawUrl = "";
  const mdLink = /^\[([^\]\n]+)\]\(([^)\n]+)\)$/.exec(trimmed);
  if (mdLink) {
    label = mdLink[1].trim();
    rawUrl = parseMarkdownImageTarget(mdLink[2]);
  } else {
    const bare = /^<?(https?:\/\/[^\s<>()[\]"']+)>?$/i.exec(trimmed);
    if (bare) {
      rawUrl = bare[1];
      label = rawUrl;
    }
  }
  if (!rawUrl) return null;
  try {
    const u = new URL(rawUrl);
    if (u.protocol !== "https:" && u.protocol !== "http:") return null;
    const host = u.hostname.toLowerCase();
    if (host === "bilibili.com" || host === "www.bilibili.com" || host === "m.bilibili.com") {
      const bm = /\/video\/(BV[0-9A-Za-z]{10})\b/.exec(u.pathname);
      if (bm) {
        return { provider: "Bilibili", videoId: bm[1], url: rawUrl, label: label || bm[1] };
      }
    }
    if (host === "youtube.com" || host === "www.youtube.com" || host === "m.youtube.com" || host === "youtu.be") {
      let vid = "";
      if (host === "youtu.be") {
        const cand = u.pathname.replace(/^\/+/, "").split("/")[0] || "";
        if (/^[A-Za-z0-9_-]{11}$/.test(cand)) vid = cand;
      } else if (u.pathname === "/watch") {
        const cand = u.searchParams.get("v") || "";
        if (/^[A-Za-z0-9_-]{11}$/.test(cand)) vid = cand;
      } else {
        const ym = /^\/(?:embed|shorts|live)\/([A-Za-z0-9_-]{11})(?:\/|$)/.exec(u.pathname);
        if (ym) vid = ym[1];
      }
      if (vid) {
        return { provider: "YouTube", videoId: vid, url: rawUrl, label: label || vid };
      }
    }
  } catch {
    return null;
  }
  return null;
}

function focusIRWidgetPos(view: EditorView, pos: number, clientY: number): void {
  pendingIRClickClientY = clientY;
  view.dispatch({
    selection: { anchor: Math.min(pos, view.state.doc.length) },
    effects: irFocusEffect.of(true),
    userEvent: "select.pointer",
  });
  view.focus();
}

function bindIRFocusMouseDown(el: HTMLElement, view: EditorView, pos: number, useClickY = true): void {
  el.addEventListener("mousedown", (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    if (useClickY) {
      focusIRWidgetPos(view, pos, e.clientY);
    } else {
      view.dispatch({
        selection: { anchor: Math.min(pos, view.state.doc.length) },
        userEvent: "select.pointer",
      });
      view.focus();
    }
  });
}

function renderKatexIRWidget(el: HTMLElement, expr: string, displayMode: boolean, view: EditorView): void {
  void ensureEditorKaTeX()
    .then((katex) => {
      if (!katex || !el.isConnected) return;
      try {
        katex.render(expr, el, { displayMode, throwOnError: false });
        view.requestMeasure();
      } catch {
        /* keep raw formula text */
      }
    })
    .catch(() => {});
}

class MediaIRWidget extends BaseIRWidget {
  constructor(
    readonly kind: "audio" | "video",
    readonly src: string,
    readonly pos: number,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: MediaIRWidget): boolean {
    return this.kind === other.kind && this.src === other.src && this.pos === other.pos && this.endPos === other.endPos;
  }
  get estimatedHeight(): number {
    return this.kind === "video" ? 220 : 52;
  }
  toDOM(view: EditorView): HTMLElement {
    const isVideo = this.kind === "video";
    const wrap = mkEl("span", `cm-ir-media-wrap cm-ir-media-wrap--${this.kind}`);
    const el = mkEl(this.kind, `cm-ir-${this.kind}`);
    el.controls = true;
    el.preload = "metadata";
    if (isVideo) (el as HTMLVideoElement).playsInline = true;
    el.src = this.src;
    const editChip = mkEl("button", "cm-ir-media-edit", "✎");
    editChip.type = "button";
    editChip.title = this.src;
    wrap.append(el, editChip);
    wrap.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      if (e.target === el && !e.altKey && !e.metaKey && !e.ctrlKey) {
        if (!isVideo) return;
        const rect = el.getBoundingClientRect();
        if (rect.height > 60 && e.clientY >= rect.bottom - 44) return;
      }
      e.preventDefault();
      focusIRWidgetPos(view, this.endPos, e.clientY);
    });
    return wrap;
  }
}

class TocIRWidget extends BaseIRWidget {
  constructor(
    readonly pos: number,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: TocIRWidget): boolean {
    return this.pos === other.pos && this.endPos === other.endPos;
  }
  get estimatedHeight(): number {
    return 44;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("span", "cm-ir-toc-widget");
    wrap.appendChild(mkEl("span", "cm-ir-toc-widget__badge", "TOC"));
    wrap.appendChild(mkEl("span", "cm-ir-toc-widget__title", t("ed.insert_toc")));
    bindIRFocusMouseDown(wrap, view, this.endPos);
    return wrap;
  }
}

class EmbedVideoIRWidget extends BaseIRWidget {
  constructor(
    readonly info: StandaloneEmbedInfo,
    readonly pos: number,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: EmbedVideoIRWidget): boolean {
    return (
      this.info.provider === other.info.provider &&
      this.info.videoId === other.info.videoId &&
      this.info.label === other.info.label &&
      this.pos === other.pos &&
      this.endPos === other.endPos
    );
  }
  get estimatedHeight(): number {
    return 48;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("span", `cm-ir-embed-widget cm-ir-embed-widget--${this.info.provider.toLowerCase()}`);
    const badge = mkEl("span", "cm-ir-embed-widget__badge", `▶ ${this.info.provider}`);
    const title = mkEl(
      "span",
      "cm-ir-embed-widget__title",
      this.info.label && this.info.label !== this.info.url && this.info.label !== this.info.videoId
        ? `${this.info.label} · ${this.info.videoId}`
        : this.info.videoId
    );
    wrap.append(badge, title);
    wrap.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      if (e.metaKey || e.ctrlKey) {
        const safeUrl = safeHref(this.info.url, { allowRelative: false });
        if (safeUrl) {
          window.open(safeUrl, "_blank", "noopener,noreferrer");
        }
        return;
      }
      focusIRWidgetPos(view, this.endPos, e.clientY);
    });
    return wrap;
  }
}

class TaskCheckboxIRWidget extends BaseIRWidget {
  constructor(
    readonly checked: boolean,
    readonly from: number,
    readonly to: number
  ) {
    super();
  }
  eq(other: TaskCheckboxIRWidget): boolean {
    return this.checked === other.checked && this.from === other.from && this.to === other.to;
  }
  toDOM(view: EditorView): HTMLElement {
    const cb = mkEl("input", "cm-ir-task-cb");
    cb.type = "checkbox";
    cb.checked = this.checked;
    cb.setAttribute("aria-label", "Toggle task");
    cb.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      e.stopPropagation();
      if (this.to <= view.state.doc.length) {
        view.dispatch({
          changes: { from: this.from, to: this.to, insert: this.checked ? "[ ]" : "[x]" },
          userEvent: "input",
        });
      }
    });
    return cb;
  }
}

class MermaidIRWidget extends BaseIRWidget {
  constructor(
    readonly code: string,
    readonly startPos: number
  ) {
    super();
  }
  eq(other: MermaidIRWidget): boolean {
    return this.code === other.code && this.startPos === other.startPos;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("div", "cm-ir-mermaid");
    renderMermaidSvg(this.code, wrap, () => view.requestMeasure());
    bindIRFocusMouseDown(wrap, view, this.startPos);
    return wrap;
  }
}

class MathBlockIRWidget extends BaseIRWidget {
  constructor(
    readonly expr: string,
    readonly startPos: number
  ) {
    super();
  }
  eq(other: MathBlockIRWidget): boolean {
    return this.expr === other.expr && this.startPos === other.startPos;
  }
  toDOM(view: EditorView): HTMLElement {
    const wrap = mkEl("div", "cm-ir-math-block", this.expr);
    renderKatexIRWidget(wrap, this.expr, true, view);
    bindIRFocusMouseDown(wrap, view, this.startPos);
    return wrap;
  }
}

class MathInlineIRWidget extends BaseIRWidget {
  constructor(
    readonly expr: string,
    readonly startPos: number,
    readonly endPos: number
  ) {
    super();
  }
  eq(other: MathInlineIRWidget): boolean {
    return this.expr === other.expr && this.startPos === other.startPos && this.endPos === other.endPos;
  }
  toDOM(view: EditorView): HTMLElement {
    const span = mkEl("span", "cm-ir-math-inline", this.expr);
    renderKatexIRWidget(span, this.expr, false, view);
    bindIRFocusMouseDown(span, view, this.endPos);
    return span;
  }
}

const SELECTABLE_WYSIWYG_BLOCK_SEL = ".volo-wysiwyg-audio-block, [data-volo-md-b64]";

function isSelectableWysiwygBlock(el: Element | null | undefined): el is HTMLElement {
  return Boolean(
    el instanceof HTMLElement &&
      (el.classList.contains("volo-wysiwyg-audio-block") || el.hasAttribute("data-volo-md-b64"))
  );
}

function isEmptyWysiwygBlock(el: Element): boolean {
  return (
    (el.textContent || "").trim() === "" &&
    !el.querySelector("img, audio, video, hr, table, pre, input, iframe, [data-volo-splice], [data-volo-md-b64]")
  );
}

function isNonEditableWysiwygBlock(el: Element): boolean {
  const tag = el.tagName.toLowerCase();
  return (
    (el as HTMLElement).contentEditable === "false" ||
    el.hasAttribute("data-volo-md-b64") ||
    el.classList.contains("volo-wysiwyg-audio-block") ||
    el.classList.contains("volo-math-block") ||
    el.classList.contains("volo-inline-toc") ||
    el.classList.contains("volo-media-figure") ||
    (tag === "pre" && el.classList.contains("mermaid")) ||
    tag === "audio" ||
    tag === "video" ||
    tag === "hr" ||
    tag === "figure" ||
    el.hasAttribute("data-raw-md")
  );
}

function createWysiwygPad(): HTMLParagraphElement {
  const p = mkEl("p");
  p.setAttribute("data-volo-pad", "1");
  p.appendChild(mkEl("br"));
  return p;
}

function createWysiwygMediaBlock(mediaEl: HTMLAudioElement | HTMLVideoElement, delLabel: string): HTMLDivElement {
  const isVideo = mediaEl.tagName.toLowerCase() === "video";
  const src = (mediaEl.getAttribute("src") || mediaEl.querySelector("source")?.getAttribute("src") || "").trim();
  const wrap = mkEl("div", "volo-wysiwyg-audio-block");
  wrap.contentEditable = "false";
  if (src) {
    wrap.setAttribute(isVideo ? "data-video-src" : "data-audio-src", src);
  }

  const clone = mediaEl.cloneNode(true) as HTMLAudioElement | HTMLVideoElement;
  clone.controls = true;
  if (!clone.getAttribute("preload")) clone.preload = "metadata";
  wrap.appendChild(clone);

  const delBtn = mkEl("button", "volo-wysiwyg-audio-del", "×");
  delBtn.type = "button";
  delBtn.title = delLabel;
  delBtn.setAttribute("aria-label", delLabel);
  wrap.appendChild(delBtn);

  return wrap;
}

function normalizeWysiwygDOM(root: HTMLElement, delLabel: string): void {
  // 0. Enable Goldmark disabled task-list checkboxes for interactive toggling in WYSIWYG
  for (const cb of Array.from(root.querySelectorAll<HTMLInputElement>('input[type="checkbox"][disabled]'))) {
    if (!cb.closest("[data-volo-md-b64]")) {
      cb.disabled = false;
    }
  }

  // 1. Strip whitespace-only text nodes directly under root
  for (const child of Array.from(root.childNodes)) {
    if (child.nodeType === Node.TEXT_NODE && !(child.textContent || "").trim()) {
      child.remove();
    }
  }

  // 2. Wrap bare <audio> and <video> elements into selectable/deletable non-editable blocks
  for (const mediaEl of Array.from(root.querySelectorAll<HTMLAudioElement | HTMLVideoElement>("audio, video"))) {
    if (
      mediaEl.closest(".volo-wysiwyg-audio-block") ||
      mediaEl.closest("[data-md-media-href]") ||
      mediaEl.closest("[data-volo-md-b64]")
    ) {
      continue;
    }
    const parent = mediaEl.parentElement;
    if (!parent) continue;
    if (parent === root) {
      root.replaceChild(createWysiwygMediaBlock(mediaEl, delLabel), mediaEl);
    } else if (
      parent.parentElement === root &&
      (parent.tagName === "P" || parent.tagName === "DIV") &&
      Array.from(parent.childNodes).every(
        (n) =>
          n === mediaEl ||
          (n.nodeType === Node.TEXT_NODE && !(n.textContent || "").trim()) ||
          (n instanceof HTMLElement && n.tagName === "BR")
      )
    ) {
      root.replaceChild(createWysiwygMediaBlock(mediaEl, delLabel), parent);
    }
  }

  // 3. Promote pad paragraphs that now contain user content, and deduplicate adjacent pads
  for (const pad of Array.from(root.querySelectorAll("p[data-volo-pad]"))) {
    if (!isEmptyWysiwygBlock(pad)) {
      pad.removeAttribute("data-volo-pad");
    } else if (pad.previousElementSibling?.hasAttribute("data-volo-pad")) {
      pad.remove();
    }
  }

  // 4. Ensure at least one editable paragraph when document is empty or has only pads
  const hasNonEditable = Array.from(root.children).some(isNonEditableWysiwygBlock);
  if (root.children.length === 0) {
    const p = mkEl("p");
    p.appendChild(mkEl("br"));
    root.appendChild(p);
    return;
  }
  if (!hasNonEditable && root.children.length === 1 && root.firstElementChild?.hasAttribute("data-volo-pad")) {
    root.firstElementChild.removeAttribute("data-volo-pad");
    return;
  }

  // 5. Guarantee editable <p> landing pads before leading, between adjacent, and after trailing non-editable blocks
  if (root.firstElementChild && isNonEditableWysiwygBlock(root.firstElementChild)) {
    root.insertBefore(createWysiwygPad(), root.firstElementChild);
  }
  const kids = Array.from(root.children);
  for (let i = 0; i < kids.length - 1; i++) {
    if (isNonEditableWysiwygBlock(kids[i]) && isNonEditableWysiwygBlock(kids[i + 1])) {
      root.insertBefore(createWysiwygPad(), kids[i + 1]);
    }
  }
  if (root.lastElementChild && isNonEditableWysiwygBlock(root.lastElementChild)) {
    root.appendChild(createWysiwygPad());
  }
}

function placeCaretInBlock(block: HTMLElement, atStart: boolean): void {
  const sel = window.getSelection();
  if (!sel) return;
  const range = document.createRange();
  if (isEmptyWysiwygBlock(block)) {
    if (!block.firstChild) block.appendChild(mkEl("br"));
    range.setStart(block, 0);
    range.collapse(true);
  } else {
    range.selectNodeContents(block);
    range.collapse(atStart);
  }
  sel.removeAllRanges();
  sel.addRange(range);
}

function isCaretAtBlockStart(block: HTMLElement, range: Range): boolean {
  if (isEmptyWysiwygBlock(block)) return true;
  const r = document.createRange();
  r.selectNodeContents(block);
  r.setEnd(range.startContainer, range.startOffset);
  return r.toString().length === 0 && !r.cloneContents().querySelector("img, audio, video, br");
}

function isCaretAtBlockEnd(block: HTMLElement, range: Range): boolean {
  if (isEmptyWysiwygBlock(block)) return true;
  const r = document.createRange();
  r.selectNodeContents(block);
  r.setStart(range.endContainer, range.endOffset);
  return r.toString().length === 0 && !r.cloneContents().querySelector("img, audio, video");
}

function wrapInlineMarker(raw: string, marker: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return raw;
  const start = raw.indexOf(trimmed);
  return `${raw.slice(0, start)}${marker}${trimmed}${marker}${raw.slice(start + trimmed.length)}`;
}

function escapeTableCellPipes(s: string): string {
  let out = "";
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "\\" && i + 1 < s.length) {
      out += ch + s[i + 1];
      i++;
    } else if (ch === "|") {
      out += "\\|";
    } else {
      out += ch;
    }
  }
  return out;
}

function formatMdLinkDest(url: string, rawTitle?: string | null): string {
  const u = url.trim();
  const safeU = /[\s()]/.test(u) ? `<${u.replace(/</g, "%3C").replace(/>/g, "%3E")}>` : u;
  const t = (rawTitle || "").replace(/[\r\n]+/g, " ").trim();
  return t ? `${safeU} "${t.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"` : safeU;
}

function parseCssRgbChannels(raw: string): [number, number, number, number] | null {
  const c = raw.trim().toLowerCase();
  if (!c) return null;
  if (c.startsWith("#")) {
    const hex = c.slice(1);
    if (hex.length === 3 || hex.length === 4) {
      const r = parseInt(hex[0] + hex[0], 16);
      const g = parseInt(hex[1] + hex[1], 16);
      const b = parseInt(hex[2] + hex[2], 16);
      const a = hex.length === 4 ? parseInt(hex[3] + hex[3], 16) / 255 : 1;
      if (!Number.isNaN(r) && !Number.isNaN(g) && !Number.isNaN(b) && !Number.isNaN(a)) return [r, g, b, a];
    } else if (hex.length === 6 || hex.length === 8) {
      const r = parseInt(hex.slice(0, 2), 16);
      const g = parseInt(hex.slice(2, 4), 16);
      const b = parseInt(hex.slice(4, 6), 16);
      const a = hex.length === 8 ? parseInt(hex.slice(6, 8), 16) / 255 : 1;
      if (!Number.isNaN(r) && !Number.isNaN(g) && !Number.isNaN(b) && !Number.isNaN(a)) return [r, g, b, a];
    }
    return null;
  }
  const m = /^rgba?\(\s*(\d{1,3})\s*(?:,\s*|\s+)(\d{1,3})\s*(?:,\s*|\s+)(\d{1,3})(?:\s*(?:,|\/)\s*([0-9.]+%?))?\s*\)$/.exec(c);
  if (!m) return null;
  const r = Math.min(255, parseInt(m[1], 10));
  const g = Math.min(255, parseInt(m[2], 10));
  const b = Math.min(255, parseInt(m[3], 10));
  let a = 1;
  if (m[4] !== undefined) {
    a = m[4].endsWith("%") ? parseFloat(m[4]) / 100 : parseFloat(m[4]);
    if (Number.isNaN(a)) a = 1;
  }
  return [r, g, b, a];
}

function isDefaultBodyTextColor(raw: string): boolean {
  const c = raw.trim().toLowerCase();
  if (!c) return true;
  if (
    c === "black" ||
    c === "windowtext" ||
    c === "canvastext" ||
    c === "buttontext" ||
    c === "inherit" ||
    c === "initial" ||
    c === "unset" ||
    c === "revert" ||
    c === "currentcolor" ||
    c === "transparent" ||
    c === "#000" ||
    c === "#000000" ||
    c === "#111" ||
    c === "#111111" ||
    c === "#222" ||
    c === "#222222" ||
    c === "#333" ||
    c === "#333333"
  ) {
    return true;
  }
  const rgba = parseCssRgbChannels(c);
  if (rgba) {
    const [r, g, b, a] = rgba;
    if (a <= 0.05) return true;
    const mx = Math.max(r, g, b);
    const mn = Math.min(r, g, b);
    if (mx - mn <= 18 && mx <= 75) return true;
  }
  return false;
}

function isHighlightBackgroundColor(raw: string): boolean {
  const bg = raw.trim().toLowerCase();
  if (
    !bg ||
    bg === "transparent" ||
    bg === "none" ||
    bg === "inherit" ||
    bg === "initial" ||
    bg === "unset" ||
    bg === "revert" ||
    bg === "currentcolor" ||
    bg === "white" ||
    bg === "window" ||
    bg === "canvas" ||
    bg === "#fff" ||
    bg === "#ffffff"
  ) {
    return false;
  }
  if (
    /^(?:yellow|gold|khaki|lemonchiffon|lightyellow|palegoldenrod|moccasin|peachpuff|pink|lightpink|lightgreen|palegreen|lightblue|lightcyan|lavender|plum|highlight|mark)$/.test(
      bg
    )
  ) {
    return true;
  }
  const rgba = parseCssRgbChannels(bg);
  if (rgba) {
    const [r, g, b, a] = rgba;
    if (a <= 0.06) return false;
    const mx = Math.max(r, g, b);
    const mn = Math.min(r, g, b);
    // Reject near-white page/card container backgrounds and dark neutral code backgrounds
    if (mn >= 236 && mx - mn <= 14) return false;
    if (mx <= 70 && mx - mn <= 18) return false;
    return true;
  }
  return false;
}

function decodeUtf8Base64(b64: string): string | null {
  try {
    const bin = atob(b64.replace(/\s+/g, ""));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) {
      bytes[i] = bin.charCodeAt(i);
    }
    return new TextDecoder().decode(bytes);
  } catch {
    return null;
  }
}

function escapeClipboardHtml(s: string): string {
  return s
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function nodeNbspText(n: Node | null | undefined): string {
  return (n?.textContent || "").replace(/\u00a0/g, " ");
}

function serializeFencedCodeHost(host: Element, lang: string): string {
  const srcNode = host.cloneNode(true) as HTMLElement;
  srcNode.querySelectorAll(".ln, .lnt, .volo-code-head, button, select").forEach((n) => n.remove());
  const raw = nodeNbspText(srcNode).replace(/\n$/, "");
  const fence = computeFenceDelim(raw);
  return `\n${fence}${lang}\n${raw}\n${fence}\n\n`;
}

function serializeNodesToMarkdown(nodes: Node[], listDepth: number): string {
  let out = "";
  for (let i = 0; i < nodes.length; i++) {
    const child = nodes[i];
    if (child.nodeType === Node.ELEMENT_NODE && (child as HTMLElement).tagName.toLowerCase() === "br") {
      out += "\n";
      // Goldmark WithHardWraps() emits `<br>\n`; strip single leading newline on adjacent text node
      const next = nodes[i + 1];
      if (next && next.nodeType === Node.TEXT_NODE) {
        const raw = nodeNbspText(next).replace(/^\r?\n/, "");
        const inRaw = !!next.parentElement?.closest(
          "pre, code, kbd, .mermaid, .volo-math-inline, .volo-math-block, .katex, math, [data-math-src], [data-volo-md-b64], [data-raw-md]"
        );
        out += inRaw ? raw : escapeMarkdownProseText(raw, true);
        i++;
      }
      continue;
    }
    out += serializeNodeToMarkdown(child, listDepth);
  }
  return out;
}

function serializeChildrenToMarkdown(parent: Node, listDepth: number): string {
  return serializeNodesToMarkdown(Array.from(parent.childNodes), listDepth);
}

const LI_BLOCK_CHILD_TAGS = new Set([
  "p",
  "div",
  "section",
  "article",
  "aside",
  "pre",
  "figure",
  "blockquote",
  "ul",
  "ol",
  "table",
  "details",
  "dl",
  "h1",
  "h2",
  "h3",
  "h4",
  "h5",
  "h6",
]);

function serializeListItemChildren(
  li: Element,
  pCb: HTMLInputElement | null
): Array<{ kind: "list" | "block" | "inline"; text: string }> {
  const chunks: Array<{ kind: "list" | "block" | "inline"; text: string }> = [];
  let inlineBuf: Node[] = [];

  const flushInline = () => {
    if (inlineBuf.length === 0) return;
    const txt = stripHorizWsBeforeNewline(serializeNodesToMarkdown(inlineBuf, 0))
      .replace(/\n{2,}/g, "\n")
      .trim();
    inlineBuf = [];
    if (txt) chunks.push({ kind: "inline", text: txt });
  };

  for (const child of Array.from(li.childNodes)) {
    if (child.nodeType === 1) {
      const cTag = (child as Element).tagName.toLowerCase();
      if (cTag === "input" && (child as HTMLInputElement).type === "checkbox") continue;
      if (LI_BLOCK_CHILD_TAGS.has(cTag)) {
        flushInline();
        if (cTag === "ul" || cTag === "ol") {
          const sub = trimEndChars(serializeNodeToMarkdown(child, 0), "\n", true);
          if (sub) chunks.push({ kind: "list", text: sub });
          continue;
        }
        if (cTag === "p" && pCb && (child as Element).contains?.(pCb)) {
          const cloneP = child.cloneNode(true) as HTMLElement;
          cloneP.querySelector('input[type="checkbox"]')?.remove();
          const pTxt = serializeChildrenToMarkdown(cloneP, 0).trim();
          if (pTxt) chunks.push({ kind: "inline", text: pTxt });
          continue;
        }
        const isInlineDiv =
          cTag === "div" &&
          !(child as Element).querySelector("p,div,section,article,aside,pre,figure,blockquote,ul,ol,table,details,dl,h1,h2,h3,h4,h5,h6,hr");
        const blk = trimEndChars(serializeNodeToMarkdown(child, 0), "\n", true);
        if (blk) chunks.push({ kind: cTag === "p" || isInlineDiv ? "inline" : "block", text: blk });
        continue;
      }
    }
    inlineBuf.push(child);
  }
  flushInline();
  return chunks;
}

let activeWysiwygSourceMD = "";

function serializeNodeToMarkdown(node: Node, listDepth = 0): string {
  if (node.nodeType === Node.TEXT_NODE) {
    const raw = nodeNbspText(node);
    const pEl = node.parentElement;
    if (
      pEl?.closest(
        "pre, code, kbd, .mermaid, .volo-math-inline, .volo-math-block, .katex, math, [data-math-src], [data-volo-md-b64], [data-raw-md]"
      )
    ) {
      return raw;
    }
    const prev = node.previousSibling;
    const isBlockParent =
      !!pEl && /^(?:p|div|li|blockquote|td|th|h[1-6]|section|article|aside|figcaption|summary|dd|dt)$/i.test(pEl.tagName);
    const atLineStart =
      (prev === null && isBlockParent) ||
      (prev?.nodeType === 1 && (prev as Element).tagName.toLowerCase() === "br") ||
      (prev?.nodeType === 3 && /\n$/.test(prev.textContent || ""));
    return escapeMarkdownProseText(raw, atLineStart);
  }
  if (node.nodeType !== Node.ELEMENT_NODE) return "";
  const el = node as HTMLElement;
  const tag = el.tagName.toLowerCase();

  // Ephemeral caret-splice marker used for exact WYSIWYG snippet/paste insertion
  if (el.hasAttribute("data-volo-splice")) {
    return el.getAttribute("data-volo-splice") || "";
  }

  // FIRST check: Lossless protected directive blocks (:::lock, :::book, :::bookshelf, :::plugin, [toc])
  const mdB64 = el.getAttribute("data-volo-md-b64");
  if (mdB64) {
    const decoded = (decodeUtf8Base64(mdB64) || "").trim();
    if (decoded) return `\n\n${decoded}\n\n`;
  }

  // Synthetic empty caret-pad paragraph around non-editable media/blocks
  if (el.hasAttribute("data-volo-pad") && isEmptyWysiwygBlock(el)) {
    return "";
  }

  // Lossless inline TOC & standalone media link round-tripping
  if (el.hasAttribute("data-md-toc")) {
    return `\n[toc]\n\n`;
  }
  const mdMediaHref = el.getAttribute("data-md-media-href");
  if (mdMediaHref) {
    const mdMediaLabel = el.getAttribute("data-md-media-label") || mdMediaHref;
    return `\n[${mdMediaLabel}](${mdMediaHref})\n\n`;
  }

  // Goldmark footnotes section (<div class="footnotes"> or <section class="footnotes">)
  if ((tag === "div" || tag === "section") && el.classList.contains("footnotes")) {
    const items = Array.from(el.querySelectorAll("ol > li"));
    if (items.length > 0) {
      const defs = items
        .map((li, idx) => {
          const rawId = (li.id || "").replace(/^fn:?/, "") || String(idx + 1);
          const body = serializeChildrenToMarkdown(li, 0).trim();
          const lines = body.split("\n");
          const first = `[^${rawId}]: ${lines[0] || ""}`;
          const rest = lines.slice(1).map((l) => (l.trim() ? `    ${l}` : ""));
          return [first, ...rest].join("\n").trimEnd();
        })
        .join("\n\n");
      return `\n\n${defs}\n\n`;
    }
  }

  // Lossless KaTeX / MathML round-tripping (internal data-math-src + external webpage .katex / <math>)
  if (el.hasAttribute("data-math-src")) {
    const expr = (el.getAttribute("data-math-src") || "").trim();
    const isBlock = el.getAttribute("data-math-display") === "1" || el.classList.contains("volo-math-block");
    return isBlock ? `\n$$\n${expr}\n$$\n\n` : `$${expr}$`;
  }
  if (el.classList.contains("katex-display") || (tag === "math" && el.getAttribute("display") === "block")) {
    const tex = (
      el.querySelector('annotation[encoding="application/x-tex"]')?.textContent ||
      el.getAttribute("alttext") ||
      el.getAttribute("data-tex") ||
      ""
    ).trim();
    if (tex) return `\n$$\n${tex}\n$$\n\n`;
  }
  if (el.classList.contains("katex") || tag === "math") {
    const tex = (
      el.querySelector('annotation[encoding="application/x-tex"]')?.textContent ||
      el.getAttribute("alttext") ||
      el.getAttribute("data-tex") ||
      ""
    ).trim();
    if (tex) return `$${tex}$`;
  }

  const serializeMediaEl = (kind: "audio" | "video", rawSrc: string): string => {
    const safe = safeMediaSrc(rawSrc);
    return safe ? `\n<${kind} controls preload="metadata" src="${escapeClipboardHtml(safe)}"></${kind}>\n\n` : "";
  };

  // Wrapped WYSIWYG audio/video card
  if (el.classList.contains("volo-wysiwyg-audio-block")) {
    const readMedia = (kind: "audio" | "video") =>
      (
        el.getAttribute(`data-${kind}-src`) ||
        el.querySelector(kind)?.getAttribute("src") ||
        el.querySelector(`${kind} source`)?.getAttribute("src") ||
        ""
      ).trim();
    return serializeMediaEl("video", readMedia("video")) || serializeMediaEl("audio", readMedia("audio"));
  }

  // Preserve custom directive blocks if present
  const rawMd = el.getAttribute("data-raw-md");
  if (rawMd) return `\n${rawMd}\n`;

  // GitHub-style Callout (<aside class="volo-callout volo-callout--note|tip|important|warning|caution">)
  if (el.classList.contains("volo-callout")) {
    const kindMatch = /volo-callout--(note|tip|important|warning|caution)/i.exec(el.className);
    const kind = (el.getAttribute("data-callout") || (kindMatch ? kindMatch[1] : "note")).toUpperCase();
    const bodyEl = el.querySelector(".volo-callout__body");
    const bodyNodes = bodyEl
      ? Array.from(bodyEl.childNodes)
      : Array.from(el.childNodes).filter(
          (n) => !(n.nodeType === 1 && (n as Element).classList.contains("volo-callout__title"))
        );
    const bodyMd = serializeNodesToMarkdown(bodyNodes, 0).trim().replace(/\n{3,}/g, "\n\n");
    if (!bodyMd) return `\n> [!${kind}]\n\n`;
    const quoted = bodyMd
      .split("\n")
      .map((l) => (l.trim() ? `> ${l}` : ">"))
      .join("\n");
    return `\n> [!${kind}]\n${quoted}\n\n`;
  }

  // Mermaid diagram blocks (<pre class="mermaid"> or <div class="mermaid">)
  if (el.classList.contains("mermaid") && (tag === "pre" || tag === "div")) {
    const raw = (el.getAttribute("data-mermaid-src") || el.textContent || "").replace(/\n$/, "");
    const fence = computeFenceDelim(raw);
    return `\n${fence}mermaid\n${raw}\n${fence}\n\n`;
  }

  // Unwrap .volo-table-scroll wrapper directly to its inner <table>
  if (tag === "div" && el.classList.contains("volo-table-scroll")) {
    const tbl = el.querySelector("table");
    if (tbl) return serializeNodeToMarkdown(tbl, listDepth);
  }

  const inner = () => serializeChildrenToMarkdown(el, listDepth);

  if (/^h([1-6])$/.test(tag)) {
    return `\n${"#".repeat(Number(tag[1]))} ${collapseNewlines(inner(), " ").trim()}\n\n`;
  }

  switch (tag) {
    case "center":
    case "p":
    case "div":
    case "section":
    case "article":
    case "aside":
    case "figcaption": {
      const txt = inner().trim();
      // Preserve presentational alignment wrappers (`<div align=center>`, `<center>`, `style="text-align:right"`)
      const alignVal =
        tag === "center"
          ? "center"
          : (el.getAttribute("align") || el.style?.textAlign || "").toLowerCase().trim();
      if (txt && (alignVal === "center" || alignVal === "right")) {
        return `\n<div align="${alignVal}">\n\n${txt}\n\n</div>\n\n`;
      }
      return txt ? `\n${txt}\n\n` : "\n";
    }
    case "br":
      return "\n";
    case "strong":
    case "b":
      return wrapInlineMarker(inner(), "**");
    case "em":
    case "i":
      return wrapInlineMarker(inner(), "*");
    case "del":
    case "s":
    case "strike":
      return wrapInlineMarker(inner(), "~~");
    case "u":
      return `<u>${inner()}</u>`;
    case "mark":
      return wrapInlineMarker(inner(), "==");
    case "kbd":
      return `<kbd>${escapeClipboardHtml(nodeNbspText(el))}</kbd>`;
    case "sup": {
      if (el.id.startsWith("fnref") || el.classList.contains("footnote-ref")) {
        const fnA = el.querySelector('a[href^="#fn"]');
        const href = fnA?.getAttribute("href") || "";
        const fnId = href.replace(/^#fn:?/, "") || (el.textContent || "").replace(/[\[\]]/g, "").trim();
        if (fnId) return `[^${fnId}]`;
      }
      const tSup = inner().trim();
      return tSup && !/[\s^[\]]/.test(tSup) ? `^${tSup}^` : `<sup>${inner()}</sup>`;
    }
    case "sub": {
      const tSub = inner().trim();
      return tSub && !/[\s~/]/.test(tSub) ? `~${tSub}~` : `<sub>${inner()}</sub>`;
    }
    case "font": {
      const col = (el.getAttribute("color") || el.style?.color || "").trim();
      if (col && SAFE_IR_COLOR_RE.test(col) && !isDefaultBodyTextColor(col)) {
        return `<font color="${col}">${inner()}</font>`;
      }
      return inner();
    }
    case "span": {
      let content = inner();
      const bg = (el.style?.backgroundColor || "").trim();
      if (bg && isHighlightBackgroundColor(bg) && !el.querySelector("mark")) {
        content = wrapInlineMarker(content, "==");
      }
      const col = (el.style?.color || "").trim();
      if (col && SAFE_IR_COLOR_RE.test(col) && !isDefaultBodyTextColor(col)) {
        return `<font color="${col}">${content}</font>`;
      }
      return content;
    }
    case "details": {
      const sumEl = Array.from(el.children).find((c) => c.tagName.toLowerCase() === "summary") as HTMLElement | undefined;
      const sumTxt = sumEl ? serializeChildrenToMarkdown(sumEl, 0).trim() : "";
      const bodyTxt = serializeNodesToMarkdown(
        Array.from(el.childNodes).filter((n) => n !== sumEl),
        0
      ).trim();
      const openAttr = el.hasAttribute("open") ? " open" : "";
      return `\n<details${openAttr}>\n<summary>${sumTxt}</summary>\n\n${bodyTxt}\n</details>\n\n`;
    }
    case "summary":
      return inner().trim();
    case "dl": {
      const parts: string[] = [];
      for (const child of Array.from(el.children)) {
        const cTag = child.tagName.toLowerCase();
        if (cTag === "dt") {
          const dtTxt = collapseNewlines(serializeChildrenToMarkdown(child, 0), " ").trim();
          if (dtTxt) parts.push(dtTxt);
        } else if (cTag === "dd") {
          const ddTxt = serializeChildrenToMarkdown(child, 0).trim();
          const lines = ddTxt.split("\n");
          const first = `: ${lines[0] || ""}`;
          const rest = lines.slice(1).map((l) => (l.trim() ? `  ${l}` : ""));
          parts.push([first, ...rest].join("\n").trimEnd());
        }
      }
      return parts.length > 0 ? `\n${parts.join("\n")}\n\n` : "";
    }
    case "dt":
      return `${collapseNewlines(inner(), " ").trim()}\n`;
    case "dd":
      return `: ${inner().trim()}\n`;
    case "figure": {
      if (el.classList.contains("volo-codeblock") || el.hasAttribute("data-lang") || el.hasAttribute("data-detected-lang")) {
        const codeEl = el.querySelector("code");
        const preEl = el.querySelector("pre");
        const cls = codeEl?.className || "";
        const langMatch = /(?:language|lang)-([a-zA-Z0-9_+#.-]+)/.exec(cls);
        const explicitSpan = el.querySelector(".volo-code-lang:not([data-auto-lang])");
        const lang =
          el.getAttribute("data-lang") ||
          preEl?.getAttribute("data-lang") ||
          codeEl?.getAttribute("data-lang") ||
          (langMatch ? langMatch[1] : "") ||
          (explicitSpan?.textContent || "").trim().toLowerCase();
        const srcNode = codeEl || preEl || el;
        return serializeFencedCodeHost(srcNode, lang);
      }
      return inner();
    }
    case "code": {
      if (el.parentElement?.tagName.toLowerCase() === "pre") {
        const clone = el.cloneNode(true) as HTMLElement;
        clone.querySelectorAll(".ln, .lnt").forEach((n) => n.remove());
        return nodeNbspText(clone);
      }
      const codeTxt = nodeNbspText(el).replace(/\r?\n/g, " ");
      if (!codeTxt) return "";
      let maxTicks = 0;
      for (const m of codeTxt.matchAll(/`+/g)) {
        if (m[0].length > maxTicks) maxTicks = m[0].length;
      }
      const fence = "`".repeat(maxTicks + 1);
      const needPad =
        codeTxt.startsWith("`") ||
        codeTxt.endsWith("`") ||
        (codeTxt.startsWith(" ") && codeTxt.endsWith(" ") && codeTxt.trim().length > 0);
      return needPad ? `${fence} ${codeTxt} ${fence}` : `${fence}${codeTxt}${fence}`;
    }
    case "pre": {
      const codeEl = el.querySelector("code");
      const cls = `${el.className || ""} ${codeEl?.className || ""}`;
      const langMatch = /(?:language|lang)-([a-zA-Z0-9_+#.-]+)/.exec(cls);
      const attrLang =
        el.getAttribute("data-lang") ||
        el.getAttribute("data-language") ||
        el.getAttribute("data-extracted-lang") ||
        codeEl?.getAttribute("data-lang") ||
        codeEl?.getAttribute("data-language") ||
        el.closest("figure[data-lang]")?.getAttribute("data-lang") ||
        "";
      const lang = attrLang || (langMatch ? langMatch[1] : /\bmermaid\b/i.test(cls) ? "mermaid" : "");
      return serializeFencedCodeHost(codeEl || el, lang);
    }
    case "blockquote": {
      const bqBody = inner().trim().replace(/\n{3,}/g, "\n\n");
      if (!bqBody) return `\n>\n\n`;
      const lines = bqBody
        .split("\n")
        .map((l) => (l.trim() ? `> ${l}` : ">"))
        .join("\n");
      return `\n${lines}\n\n`;
    }
    case "ul":
    case "ol": {
      const isOrdered = tag === "ol";
      const startAttr = isOrdered ? parseInt(el.getAttribute("start") || "1", 10) : 1;
      const startNum = Number.isFinite(startAttr) && startAttr > 0 ? startAttr : 1;
      const baseIndent = "  ".repeat(listDepth);
      const items = Array.from(el.children)
        .filter((c) => c.tagName.toLowerCase() === "li")
        .map((li, idx) => {
          const directCb = Array.from(li.children).find(
            (c) => c.tagName.toLowerCase() === "input" && (c as HTMLInputElement).type === "checkbox"
          ) as HTMLInputElement | undefined;
          const pCb = !directCb
            ? (li.querySelector(':scope > p > input[type="checkbox"]') as HTMLInputElement | null)
            : null;
          const cb = directCb || pCb;
          const taskPrefix = cb ? `[${cb.checked ? "x" : " "}] ` : "";
          const marker = isOrdered ? `${startNum + idx}. ${taskPrefix}` : `- ${taskPrefix}`;
          const contIndent = baseIndent + " ".repeat(isOrdered ? `${startNum + idx}. `.length : 2);

          const chunks = serializeListItemChildren(li, pCb);
          if (chunks.length === 0) {
            return `${baseIndent}${marker}`.trimEnd();
          }

          let out = "";
          chunks.forEach((ch, ci) => {
            const lines = ch.text.split("\n");
            if (ci === 0 && ch.kind === "inline") {
              const first = `${baseIndent}${marker}${lines[0] || ""}`;
              const rest = lines.slice(1).map((l) => (l ? `${contIndent}${l}` : ""));
              out = [first, ...rest].join("\n");
            } else if (ci === 0) {
              const indented = lines.map((l, liIdx) =>
                liIdx === 0 ? `${baseIndent}${marker}${l}` : l ? `${contIndent}${l}` : ""
              );
              out = indented.join("\n");
            } else {
              const sep = ch.kind === "list" && chunks[ci - 1]?.kind === "inline" && chunks.length <= 2 ? "\n" : "\n\n";
              const indented = lines.map((l) => (l ? `${contIndent}${l}` : "")).join("\n");
              out += `${sep}${indented}`;
            }
          });
          return out;
        });
      const hasMultiBlockItem = items.some((it) => it.includes("\n\n"));
      const joined = items.join(hasMultiBlockItem ? "\n\n" : "\n");
      return listDepth === 0 ? `\n${joined}\n\n` : joined;
    }
    case "a": {
      if (el.classList.contains("footnote-backref") || el.getAttribute("role") === "doc-backlink") {
        return "";
      }
      const rawHref = el.getAttribute("href") || "";
      const href = cleanClipboardUrl(rawHref, "href");
      const explicitWiki = (el.getAttribute("data-wiki-link") || el.getAttribute("data-wiki-slug") || "").trim();
      if (explicitWiki) {
        const label = collapseNewlines(inner(), " ").trim();
        return label && label !== explicitWiki ? `[[${explicitWiki}|${label}]]` : `[[${explicitWiki}]]`;
      }
      const postSlugM = /^\/posts\/([a-zA-Z0-9._%-]+)$/.exec(href);
      if (postSlugM) {
        try {
          const slug = decodeURIComponent(postSlugM[1]);
          if (
            /^[a-zA-Z0-9._-]+$/.test(slug) &&
            (activeWysiwygSourceMD.includes(`[[${slug}]]`) || activeWysiwygSourceMD.includes(`[[${slug}|`))
          ) {
            const label = collapseNewlines(inner(), " ").trim();
            return label && label !== slug ? `[[${slug}|${label}]]` : `[[${slug}]]`;
          }
        } catch {
          /* ignore malformed percent escape */
        }
      }
      if (!href) {
        return collapseNewlines(inner(), " ").trim();
      }

      const aTitle = el.getAttribute("title");
      const dest = formatMdLinkDest(href, aTitle);

      // Link wrapping image(s) (e.g. Baidu/WeChat/card anchor wrapping badge overlay + <img>): emit valid linked image(s) without newlines inside [...]
      if (el.querySelector("img")) {
        const imgMds: string[] = [];
        for (const im of Array.from(el.querySelectorAll("img"))) {
          const imSrc = cleanClipboardUrl(
            im.getAttribute("data-volo-img-src") ||
              im.getAttribute("data-actualsrc") ||
              im.getAttribute("data-original") ||
              im.getAttribute("data-lazy-src") ||
              im.getAttribute("data-src") ||
              im.getAttribute("src") ||
              "",
            "src"
          );
          if (!imSrc) continue;
          const alt = escapeMdLinkLabel((im.getAttribute("alt") || "").replace(/[\r\n]+/g, " ").trim());
          imgMds.push(`[![${alt}](${formatMdLinkDest(imSrc, im.getAttribute("title"))})](${dest})`);
        }
        const clone = el.cloneNode(true) as HTMLElement;
        clone
          .querySelectorAll("img, picture, source, svg, button, [class*='tag'], [class*='badge'], [class*='count'], [class*='time'], [class*='date']")
          .forEach((n) => n.remove());
        const caption = (clone.textContent || "").replace(/\s+/g, " ").trim();
        const isOverlayBadge =
          !caption ||
          /^\d+$/.test(caption) ||
          /^(?:昨天|今天|前天|\d+\s*(?:分钟|小时|天|周|月|年)前)$/i.test(caption) ||
          (caption.length <= 6 && !/[。！？.!?]/.test(caption));
        if (imgMds.length > 0) {
          return isOverlayBadge
            ? imgMds.join("\n\n")
            : `${imgMds.join("\n\n")}\n\n[${escapeMdLinkLabel(caption)}](${dest})`;
        }
      }

      // Link wrapping a single heading (<a href="..."><h3>Title</h3></a>): lift heading syntax outside the brackets
      const headingChild = el.querySelector("h1, h2, h3, h4, h5, h6");
      if (headingChild) {
        const clone = el.cloneNode(true) as HTMLElement;
        clone.querySelector("h1, h2, h3, h4, h5, h6")?.remove();
        if (!(clone.textContent || "").trim()) {
          const lvl = Math.min(6, Math.max(1, Number(headingChild.tagName.slice(1)) || 2));
          const hText = (headingChild.textContent || "").replace(/\s+/g, " ").trim();
          if (hText) {
            return `\n\n${"#".repeat(lvl)} [${escapeMdLinkLabel(hText)}](${dest})\n\n`;
          }
        }
      }

      const label = collapseNewlines(inner(), " ").trim();
      if (!label || label === href) {
        return `[${escapeMdLinkLabel(href)}](${dest})`;
      }
      return `[${escapeMdLinkLabel(label)}](${dest})`;
    }
    case "img": {
      const rawSrc = el.getAttribute("data-volo-img-src") || el.getAttribute("src") || "";
      const src = cleanClipboardUrl(rawSrc, "src");
      if (!src) return "";
      const alt = escapeMdLinkLabel((el.getAttribute("alt") || "").replace(/[\r\n]+/g, " ").trim());
      const wAttr = (el.getAttribute("width") || el.style?.width || "").trim();
      const hAttr = (el.getAttribute("height") || el.style?.height || "").trim();
      const wasExplicitHtmlImg =
        Boolean(wAttr || hAttr) &&
        (/[%a-z]/i.test(wAttr + hAttr) || (activeWysiwygSourceMD.includes("<img") && activeWysiwygSourceMD.includes(src)));
      if (wasExplicitHtmlImg) {
        const escSrc = escapeClipboardHtml(src);
        const escAlt = escapeClipboardHtml(alt);
        const escW = escapeClipboardHtml(wAttr);
        const escH = escapeClipboardHtml(hAttr);
        return `<img src="${escSrc}"${escAlt ? ` alt="${escAlt}"` : ""}${escW ? ` width="${escW}"` : ""}${escH ? ` height="${escH}"` : ""} />`;
      }
      return `![${alt}](${formatMdLinkDest(src, el.getAttribute("title"))})`;
    }
    case "audio":
    case "video":
      return serializeMediaEl(tag, el.getAttribute("src") || el.querySelector("source")?.getAttribute("src") || "");
    case "hr":
      return `\n---\n\n`;
    case "table": {
      const rows = Array.from(
        el.querySelectorAll(":scope > thead > tr, :scope > tbody > tr, :scope > tfoot > tr, :scope > tr")
      );
      if (rows.length === 0) return "";
      const headerCells = Array.from(rows[0].querySelectorAll(":scope > th, :scope > td"));
      const colAligns = headerCells.map((cell) => {
        const raw = ((cell as HTMLElement).style?.textAlign || cell.getAttribute("align") || "").toLowerCase().trim();
        if (raw === "center") return ":---:";
        if (raw === "right") return "---:";
        if (raw === "left") return ":---";
        return "---";
      });
      const matrix = rows.map((tr) =>
        Array.from(tr.querySelectorAll(":scope > th, :scope > td")).map((cell) =>
          escapeTableCellPipes(
            collapseNewlines(serializeChildrenToMarkdown(cell, 0).trim(), "<br>").trim()
          )
        )
      );
      const cols = Math.max(1, ...matrix.map((r) => r.length));
      const padRow = (r: string[]) => {
        const copy = [...r];
        while (copy.length < cols) copy.push("");
        return `| ${copy.join(" | ")} |`;
      };
      const head = padRow(matrix[0]);
      const sep = `| ${Array.from({ length: cols }, (_, ci) => colAligns[ci] || "---").join(" | ")} |`;
      const body = matrix.slice(1).map(padRow).join("\n");
      return `\n${head}\n${sep}${body ? `\n${body}` : ""}\n\n`;
    }
    default:
      return inner();
  }
}

function htmlToMarkdown(root: HTMLElement): string {
  return serializeChildrenToMarkdown(root, 0)
    .replace(/\n{3,}/g, "\n\n")
    .trim();
}

function parseBlockquoteLine(line: string): { depth: number; body: string } {
  let s = line;
  let depth = 0;
  while (true) {
    const m = /^ {0,3}> ?(.*)$/.exec(s);
    if (!m) break;
    depth++;
    s = m[1];
  }
  return { depth, body: s };
}

function isCursorInFencedCode(state: EditorState): boolean {
  const cursorLine = state.doc.lineAt(state.selection.main.from).number;
  let fenceChar = "";
  let fenceLen = 0;
  let fenceQuoteDepth = 0;
  for (let ln = 1; ln <= cursorLine; ln++) {
    const { depth, body } = parseBlockquoteLine(state.doc.line(ln).text);
    if (fenceChar && depth < fenceQuoteDepth) {
      fenceChar = "";
      fenceLen = 0;
      fenceQuoteDepth = 0;
    }
    if (!fenceChar) {
      const open = matchFenceOpen(body);
      if (open) {
        if (ln === cursorLine) return false;
        fenceChar = open.fenceChar;
        fenceLen = open.fenceLen;
        fenceQuoteDepth = depth;
      }
    } else if (depth === fenceQuoteDepth && matchFenceClose(body, fenceChar, fenceLen)) {
      fenceChar = "";
      fenceLen = 0;
      fenceQuoteDepth = 0;
    }
  }
  return fenceChar !== "";
}

function dataUrlToFile(dataUrl: string, idx: number): File | null {
  const m = /^data:(image\/(png|jpe?g|gif|webp|avif));base64,([A-Za-z0-9+/=\s]+)$/i.exec(dataUrl.trim());
  if (!m) return null;
  try {
    const mime = m[1].toLowerCase();
    const rawExt = m[2].toLowerCase();
    const ext = rawExt === "jpeg" ? "jpg" : rawExt;
    const bin = atob(m[3].replace(/\s+/g, ""));
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) {
      bytes[i] = bin.charCodeAt(i);
    }
    return new File([bytes], `pasted-image-${idx}.${ext}`, { type: mime });
  } catch {
    return null;
  }
}

function extractPreCodeLines(container: HTMLElement): string {
  const clone = container.cloneNode(true) as HTMLElement;
  clone
    .querySelectorAll(
      ".ln, .lnt, .lineno, .line-numbers-rows, .hljs-ln-numbers, .code-snippet__line-index, .hljs-button, .copy-code-btn, [class*='copy-btn'], [class*='copy_btn'], [data-clipboard-text], button, select, .volo-code-head"
    )
    .forEach((n) => n.remove());
  clone.querySelectorAll("br").forEach((br) => br.replaceWith(clone.ownerDocument.createTextNode("\n")));

  // WeChat (.code-snippet_outer / .code-snippet__line), Shiki/Prism (.line), and block-wrapped lines
  const wechatLines = Array.from(clone.querySelectorAll(".code-snippet_outer, .code-snippet__line"));
  if (wechatLines.length > 0 && !(clone.textContent || "").includes("\n")) {
    return wechatLines.map(nodeNbspText).join("\n");
  }

  const lineEls = Array.from(
    clone.querySelectorAll(":scope > .line, :scope > code > .line, :scope > div, :scope > code > div, :scope > p, :scope > code > p")
  );
  if (lineEls.length > 1 && !(clone.textContent || "").includes("\n")) {
    return lineEls.map(nodeNbspText).join("\n");
  }
  return nodeNbspText(clone).replace(/\n$/, "");
}

function isTerminalOrIdeClipboardDOM(doc: Document, body: HTMLElement): boolean {
  for (const meta of Array.from(doc.querySelectorAll("meta"))) {
    const sig = `${meta.getAttribute("name") || ""} ${meta.getAttribute("content") || ""}`.toLowerCase();
    if (
      /\b(?:vscode|visual studio code|jetbrains|intellij|pycharm|goland|webstorm|clion|iterm|windows terminal|xterm|alacritty|kitty|wezterm|putty|konsole|gnome-terminal)\b/.test(
        sig
      )
    ) {
      return true;
    }
  }
  if (body.children.length === 1) {
    const root = body.firstElementChild as HTMLElement | null;
    if (root && (root.tagName === "DIV" || root.tagName === "PRE")) {
      const ff = (root.style?.fontFamily || "").toLowerCase();
      const bg = (root.style?.backgroundColor || "").trim();
      const isMonoRoot = /\b(?:monospace|consolas|menlo|monaco|courier|sfmono|fira\s*code|jetbrains\s*mono|cascadia|source\s*code)\b/.test(
        ff
      );
      if (isMonoRoot && bg) {
        const hasProseSemantics = Boolean(
          root.querySelector(
            "h1, h2, h3, h4, h5, h6, p, ul, ol, li, table, blockquote, a[href], img, strong, b, em, i, mark, figure, article, section, code[class]"
          )
        );
        if (!hasProseSemantics) return true;
      }
    }
  }
  return false;
}

function extractToolbarCodeLang(el: HTMLElement): string {
  const clone = el.cloneNode(true) as HTMLElement;
  clone
    .querySelectorAll("button, svg, a, .hljs-button, .copy-code-btn, [class*='copy'], [data-clipboard-text]")
    .forEach((n) => n.remove());
  const cleaned = (clone.textContent || "")
    .replace(/\b(?:copy\s*code|copied|copy)\b/gi, "")
    .replace(/(?:复制代码|已复制|复制)/g, "")
    .trim();
  if (/^[a-zA-Z][a-zA-Z0-9_+#.-]{0,22}$/.test(cleaned)) {
    return cleaned.toLowerCase();
  }
  return "";
}

function encodeVoloClipboardSource(md: string): string {
  try {
    const bytes = new TextEncoder().encode(md);
    let bin = "";
    const chunk = 8192;
    for (let i = 0; i < bytes.length; i += chunk) {
      bin += String.fromCharCode(...bytes.subarray(i, i + chunk));
    }
    return btoa(bin);
  } catch {
    return "";
  }
}

function formatClipboardInlineMd(text: string): string {
  let out = escapeClipboardHtml(text);
  out = out.replace(/!\[([^[\]\n]*)\]\(([^()\s]+)\)/g, (m, alt, url) => {
    const safe = cleanClipboardUrl(url, "src");
    return safe ? `<img src="${escapeClipboardHtml(safe)}" alt="${alt}" />` : m;
  });
  out = out.replace(/\[([^[\]\n]+)\]\(([^()\s]+)\)/g, (_m, label, url) => {
    const safe = cleanClipboardUrl(url, "href");
    return safe ? `<a href="${escapeClipboardHtml(safe)}">${label}</a>` : label;
  });
  out = out.replace(/`([^`\n]+)`/g, "<code>$1</code>");
  out = out.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
  out = out.replace(/==([^=\n]+)==/g, "<mark>$1</mark>");
  out = out.replace(/~~([^~\n]+)~~/g, "<del>$1</del>");
  out = out.replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<em>$2</em>");
  return out;
}

function markdownSelectionToClipboardHTML(md: string): string {
  const b64 = encodeVoloClipboardSource(md);
  const lines = md.replace(/\r\n?/g, "\n").split("\n");
  const blocks: string[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const fenceM = /^(`{3,}|~{3,})([^\n\r\u2028\u2029]*)/.exec(line);
    if (fenceM && fenceM[0].length === line.length) {
      const ch = fenceM[1][0];
      const fLen = fenceM[1].length;
      const lang = fenceM[2].trim().split(/\s+/)[0] || "";
      const codeLines: string[] = [];
      i++;
      while (i < lines.length) {
        const cl = lines[i];
        if (new RegExp(`^${ch}{${fLen},}\\s*$`).test(cl)) {
          i++;
          break;
        }
        codeLines.push(cl);
        i++;
      }
      const cls = lang ? ` class="language-${escapeClipboardHtml(lang)}"` : "";
      blocks.push(`<pre><code${cls}>${escapeClipboardHtml(codeLines.join("\n"))}</code></pre>`);
      continue;
    }
    const hm = /^(#{1,6})[ \t]+(\S[^\n\r\u2028\u2029]*)$/.exec(line);
    if (hm) {
      const lvl = hm[1].length;
      blocks.push(`<h${lvl}>${formatClipboardInlineMd(hm[2])}</h${lvl}>`);
      i++;
      continue;
    }
    if (/^\s*(?:---|\*\*\*|___)\s*$/.test(line)) {
      blocks.push("<hr />");
      i++;
      continue;
    }
    if (/^\s*>\s?/.test(line)) {
      const qLines: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        qLines.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      blocks.push(`<blockquote><p>${formatClipboardInlineMd(qLines.join(" "))}</p></blockquote>`);
      continue;
    }
    if (/^\s*[-*+]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*+]\s+/.test(lines[i])) {
        items.push(`<li>${formatClipboardInlineMd(lines[i].replace(/^\s*[-*+]\s+/, ""))}</li>`);
        i++;
      }
      blocks.push(`<ul>${items.join("")}</ul>`);
      continue;
    }
    if (/^\s*\d+\.\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
        items.push(`<li>${formatClipboardInlineMd(lines[i].replace(/^\s*\d+\.\s+/, ""))}</li>`);
        i++;
      }
      blocks.push(`<ol>${items.join("")}</ol>`);
      continue;
    }
    if (!line.trim()) {
      i++;
      continue;
    }
    const paraLines: string[] = [line];
    i++;
    while (
      i < lines.length &&
      lines[i].trim() &&
      !/^(?:#{1,6}\s|```|~~~|\s*>\s?|\s*[-*+]\s+|\s*\d+\.\s+|\s*(?:---|\*\*\*|___)\s*$)/.test(lines[i])
    ) {
      paraLines.push(lines[i]);
      i++;
    }
    blocks.push(`<p>${paraLines.map((pl) => formatClipboardInlineMd(pl)).join("<br />")}</p>`);
  }
  const attr = b64 ? ` data-volo-source-md="${escapeClipboardHtml(b64)}"` : "";
  return `<div${attr}>${blocks.join("") || formatClipboardInlineMd(md)}</div>`;
}

function tsvToMarkdownTable(rawPlain: string): string | null {
  const norm = rawPlain.replace(/\r\n?/g, "\n").trimEnd();
  if (!norm.includes("\t")) return null;
  const lines = norm.split("\n").filter((l) => l.length > 0);
  if (lines.length < 2 || lines.length > 500) return null;
  if (lines.some((l) => l.startsWith("\t") || /^\s*(?:func |const |let |var |package |import |def |class |SELECT |```)/i.test(l))) {
    return null;
  }
  const rows = lines.map((l) => l.split("\t").map((c) => c.trim().replace(/\\/g, "\\\\").replace(/\|/g, "\\|")));
  const colCount = rows[0].length;
  if (colCount < 2 || colCount > 32) return null;
  let mismatches = 0;
  for (const r of rows) {
    if (r.length !== colCount) mismatches++;
  }
  if (mismatches > Math.floor(rows.length * 0.3)) return null;
  const pad = (r: string[]) => Array.from({ length: colCount }, (_, idx) => r[idx] || "");
  const header = `| ${pad(rows[0]).join(" | ")} |`;
  const sep = `| ${Array.from({ length: colCount }, () => "---").join(" | ")} |`;
  const body = rows.slice(1).map((r) => `| ${pad(r).join(" | ")} |`);
  return [header, sep, ...body].join("\n");
}

function isMediaClipboardFile(f: File): boolean {
  const mt = (f.type || "").toLowerCase();
  if (mt.startsWith("image/") || mt.startsWith("audio/") || mt.startsWith("video/")) return true;
  return /\.(?:png|jpe?g|gif|webp|avif|bmp|ico|svg|mp3|wav|ogg|m4a|flac|aac|opus|mp4|webm|mov|m4v|ogv)$/i.test(f.name || "");
}

function extractClipboardMediaFiles(dt: DataTransfer): File[] {
  const found: File[] = [];
  const seen = new Set<string>();
  const add = (f: File | null) => {
    if (!f || !isMediaClipboardFile(f)) return;
    const key = `${f.name}:${f.size}:${f.type}`;
    if (!seen.has(key)) {
      seen.add(key);
      found.push(f);
    }
  };
  if (dt.files) {
    for (let i = 0; i < dt.files.length; i++) add(dt.files[i]);
  }
  if (dt.items) {
    for (let i = 0; i < dt.items.length; i++) {
      if (dt.items[i].kind === "file") add(dt.items[i].getAsFile());
    }
  }
  return found;
}

function shouldHandlePasteAsBinaryMedia(dt: DataTransfer, files: File[], rawPlain: string, rawHTML: string): boolean {
  if (files.length === 0) return false;
  const plainTrim = rawPlain.trim();
  if (!plainTrim && !rawHTML.trim()) return true;
  if (plainTrim) {
    const fileNames = new Set(files.map((f) => f.name.toLowerCase()));
    const plainLines = plainTrim
      .split(/\r?\n/)
      .map((l) => l.trim().replace(/^file:\/\//i, "").split(/[\\/]/).pop()?.toLowerCase() || "")
      .filter(Boolean);
    const plainMatchesFiles = plainLines.length > 0 && plainLines.every((n) => fileNames.has(n));
    if (!plainMatchesFiles) return false;
  }
  if (!rawHTML.trim()) return true;
  try {
    const d = parseClipboardHtml(rawHTML);
    const imgs = d.body.querySelectorAll("img");
    const onlyLocalImgs =
      imgs.length > 0 &&
      imgs.every((im) => /^(?:file:|webkit-fake-url:)/i.test((im.getAttribute("src") || "").trim()));
    const textLeft = (d.body.textContent || "").trim();
    return onlyLocalImgs || !textLeft;
  } catch {
    return false;
  }
}

function extractWysiwygSelectionClipboard(
  root: HTMLElement
): { plain: string; html: string; range: Range } | null {
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return null;
  const range = sel.getRangeAt(0);
  if (!root.contains(range.commonAncestorContainer)) return null;

  const startEl =
    range.startContainer.nodeType === Node.ELEMENT_NODE
      ? (range.startContainer as Element)
      : range.startContainer.parentElement;
  const endEl =
    range.endContainer.nodeType === Node.ELEMENT_NODE
      ? (range.endContainer as Element)
      : range.endContainer.parentElement;
  const startPre = startEl?.closest("pre");
  const endPre = endEl?.closest("pre");
  if (startPre && startPre === endPre) {
    const wrap = mkEl("div");
    wrap.appendChild(range.cloneContents());
    if (!wrap.querySelector(".volo-code-head")) {
      const codeText = extractPreCodeLines(wrap);
      if (!codeText) return null;
      return {
        plain: codeText,
        html: `<pre><code>${escapeClipboardHtml(codeText)}</code></pre>`,
        range,
      };
    }
  }

  const wrap = mkEl("div");
  wrap.appendChild(range.cloneContents());
  wrap
    .querySelectorAll(".ln, .lnt, .volo-code-head, .volo-ed-code-copy, button, select, .volo-ed-empty-hint")
    .forEach((n) => n.remove());
  wrap.querySelectorAll("pre").forEach((pre) => {
    const codeText = extractPreCodeLines(pre as HTMLElement);
    const replacement = mkEl("pre");
    const lang = pre.closest("[data-lang]")?.getAttribute("data-lang") || "";
    const code = mkEl("code", lang ? `language-${lang}` : undefined, codeText);
    replacement.appendChild(code);
    pre.replaceWith(replacement);
  });
  const md = htmlToMarkdown(wrap).trim();
  const plain = md || nodeNbspText(wrap);
  if (!plain) return null;
  const b64 = encodeVoloClipboardSource(plain);
  const attr = b64 ? ` data-volo-source-md="${escapeClipboardHtml(b64)}"` : "";
  return {
    plain,
    html: `<div${attr}>${wrap.innerHTML}</div>`,
    range,
  };
}

function unwrapSearchHighlightEm(body: HTMLElement, doc: Document): void {
  const isCJK = (ch: string) => /[\u3400-\u9fff\uf900-\ufaff]/.test(ch);
  const ems = Array.from(body.querySelectorAll<HTMLElement>("em"));
  for (const em of ems) {
    if (em.closest("pre, code")) continue;
    const cls = (em.className || "").toLowerCase();
    const fs = (em.style?.fontStyle || "").toLowerCase().trim();
    const col = (em.style?.color || "").trim();
    const insideHeadingOrLink = Boolean(em.closest("h1, h2, h3, h4, h5, h6, a[href], .c-abstract, .c-container, [data-click], [tpl]"));
    const styledAsHighlight = fs === "normal" || Boolean(col) || /(?:highlight|hl|keyword|search|c-color)/.test(cls);

    let tightCjkSearch = false;
    const prev = em.previousSibling;
    const next = em.nextSibling;
    const prevTxt = prev?.nodeType === 3 ? prev.textContent || "" : "";
    const nextTxt = next?.nodeType === 3 ? next.textContent || "" : "";
    const emTxt = (em.textContent || "").trim();
    const touchesAnotherEm =
      (prev?.nodeType === 1 && (prev as Element).tagName === "EM") ||
      (next?.nodeType === 1 && (next as Element).tagName === "EM");
    if (touchesAnotherEm) {
      tightCjkSearch = true;
    } else if (emTxt && /[\u3400-\u9fff]/.test(emTxt)) {
      const tailChar = prevTxt.slice(-1);
      const headChar = nextTxt.slice(0, 1);
      if ((tailChar && isCJK(tailChar)) || (headChar && isCJK(headChar))) {
        tightCjkSearch = true;
      }
    }

    if (insideHeadingOrLink || styledAsHighlight || tightCjkSearch) {
      const frag = doc.createDocumentFragment();
      while (em.firstChild) frag.appendChild(em.firstChild);
      em.replaceWith(frag);
    }
  }
}

function clipboardHtmlToMarkdown(
  rawHTML: string
): { markdown: string; pendingUploads: Array<{ placeholder: string; file: File }> } | null {
  const trimmed = rawHTML.trim();
  if (!trimmed) return null;

  const vDoc = parseClipboardHtml(trimmed);
  const doc = vDoc as unknown as Document;
  const body = vDoc.body as unknown as HTMLElement;
  if (!body) return null;

  // 0a. Exact lossless round-trip when pasting selection copied inside Volo Studio
  const voloCarrier = body.querySelector<HTMLElement>("[data-volo-source-md]");
  if (voloCarrier) {
    const check = body.cloneNode(true) as HTMLElement;
    check.querySelectorAll("[data-volo-source-md]").forEach((n) => n.remove());
    if (!(check.textContent || "").trim()) {
      const restored = decodeUtf8Base64(voloCarrier.getAttribute("data-volo-source-md") || "");
      if (restored !== null) {
        return { markdown: restored, pendingUploads: [] };
      }
    }
  }

  // 0b. Detect terminal / IDE syntax-highlighted HTML clipboards BEFORE stripping <meta>, falling back to clean text/plain
  if (isTerminalOrIdeClipboardDOM(doc, body)) {
    return null;
  }

  // 1. Strip scripts, styles, icons, hidden noise, copy buttons, and unsafe embeds
  body
    .querySelectorAll(
      "script, style, meta, link, title, noscript, iframe, object, embed, svg, canvas, button, select, textarea, [hidden], .headerlink, a.anchor, a.header-anchor, .volo-code-head, .volo-ed-code-copy, .copy-code-button, .copy-code-btn, .hljs-button, [class*='copy-btn'], [class*='copy_btn'], [data-clipboard-text], .line-numbers-rows, .code-snippet__line-index, [class*='tts'], [class*='audio-play'], [class*='corner-tag'], [class*='img-count'], [class*='pic-num']"
    )
    .forEach((n) => n.remove());
  for (const el of Array.from(body.querySelectorAll<HTMLElement>("[aria-hidden='true'], [style]"))) {
    if (el.closest(".katex, math, pre, code")) continue;
    if (el.getAttribute("aria-hidden") === "true") {
      el.remove();
      continue;
    }
    const st = (el.getAttribute("style") || "").toLowerCase();
    if (/(?:display\s*:\s*none|visibility\s*:\s*hidden|opacity\s*:\s*0(?:\s*;|\s*$)|font-size\s*:\s*0(?:px|em|rem)?(?:\s*;|\s*$))/i.test(st)) {
      el.remove();
    }
  }
  for (const sup of Array.from(body.querySelectorAll<HTMLElement>("sup"))) {
    if (sup.closest("pre, code, .katex, math")) continue;
    const cls = (sup.className || "").toLowerCase();
    const txt = (sup.textContent || "").trim();
    if (/(?:cite|ref|source|index)/.test(cls) || /^\[\d{1,3}\]$/.test(txt) || (sup.hasAttribute("data-url") && /^\d{1,3}$/.test(txt))) {
      sup.remove();
    }
  }

  // 1b. Normalize Yuque (<ne-h1>..<ne-h6>, <ne-p>, <ne-code>, data-lake-card="codeblock") & Feishu (.ace-code-block)
  for (let lvl = 1; lvl <= 6; lvl++) {
    for (const neH of Array.from(body.querySelectorAll(`ne-h${lvl}`))) {
      const h = doc.createElement(`h${lvl}`);
      while (neH.firstChild) h.appendChild(neH.firstChild);
      neH.replaceWith(h);
    }
  }
  for (const neP of Array.from(body.querySelectorAll("ne-p"))) {
    const p = doc.createElement("p");
    while (neP.firstChild) p.appendChild(neP.firstChild);
    neP.replaceWith(p);
  }
  for (const neC of Array.from(body.querySelectorAll("ne-code"))) {
    const c = doc.createElement("code");
    while (neC.firstChild) c.appendChild(neC.firstChild);
    neC.replaceWith(c);
  }
  const replaceWithPre = (target: Element, codeText: string, lang = "") => {
    const pre = doc.createElement("pre");
    const code = doc.createElement("code");
    if (lang && lang !== "plain" && lang !== "plaintext") code.className = `language-${lang}`;
    code.textContent = codeText;
    pre.appendChild(code);
    target.replaceWith(pre);
  };
  for (const card of Array.from(body.querySelectorAll<HTMLElement>('div[data-lake-card="codeblock"], [ne-card="codeblock"]'))) {
    let lang = card.getAttribute("data-language") || card.getAttribute("data-lang") || "";
    let codeTxt = "";
    const rawVal = card.getAttribute("data-card-value") || "";
    if (rawVal.startsWith("data:")) {
      try {
        const parsed = JSON.parse(decodeURIComponent(rawVal.slice(5)));
        if (typeof parsed?.code === "string") codeTxt = parsed.code;
        if (!lang && typeof parsed?.mode === "string") lang = parsed.mode;
      } catch {
        /* fallback to DOM lines */
      }
    }
    if (!codeTxt) {
      const cmLines = Array.from(card.querySelectorAll(".cm-line, .CodeMirror-line"));
      codeTxt =
        cmLines.length > 0
          ? cmLines.map(nodeNbspText).join("\n")
          : extractPreCodeLines(card);
    }
    replaceWithPre(card, codeTxt, lang);
  }

  // Feishu / Lark code blocks (.ace-code-block or containers of div[data-ace-line])
  for (const aceBlock of Array.from(body.querySelectorAll<HTMLElement>('.ace-code-block, [data-record-type="code"]'))) {
    const aceLines = Array.from(aceBlock.querySelectorAll("div[data-ace-line], .ace-line, .code-line"));
    const lang =
      aceBlock.getAttribute("data-language") ||
      aceBlock.getAttribute("data-lang") ||
      extractToolbarCodeLang((aceBlock.querySelector("[class*='header'], [class*='lang']") as HTMLElement) || doc.createElement("span"));
    const codeTxt =
      aceLines.length > 0
        ? aceLines.map((l) => nodeNbspText(l).replace(/\r?\n$/, "")).join("\n")
        : extractPreCodeLines(aceBlock);
    replaceWithPre(aceBlock, codeTxt, lang);
  }
  const strayAceLines = Array.from(body.querySelectorAll<HTMLElement>("div[data-ace-line]"));
  if (strayAceLines.length > 0) {
    const parent = strayAceLines[0].parentElement;
    if (parent && parent !== body && !parent.closest("pre")) {
      const lines = Array.from(parent.querySelectorAll("div[data-ace-line]")).map((l) =>
        nodeNbspText(l).replace(/\r?\n$/, "")
      );
      replaceWithPre(parent, lines.join("\n"));
    }
  }

  // GitHub modern React code view (.react-code-text)
  const reactCodeLines = Array.from(body.querySelectorAll<HTMLElement>(".react-code-text"));
  if (reactCodeLines.length > 0) {
    const container =
      (reactCodeLines[0].closest(".react-code-lines, [data-hpc], section, table") as HTMLElement | null) ||
      reactCodeLines[0].parentElement;
    if (container && container !== body && !container.closest("pre")) {
      const lines = Array.from(container.querySelectorAll<HTMLElement>(".react-code-text")).map((el) =>
        (el.getAttribute("data-code-text") ?? el.textContent ?? "").replace(/\u00a0/g, " ").replace(/\r?\n/g, "")
      );
      replaceWithPre(container, lines.join("\n"));
    }
  }

  // 2. Unwrap Google Docs' outer <b style="font-weight:normal" id="docs-internal-guid-..."> wrapper
  for (const bEl of Array.from(body.querySelectorAll<HTMLElement>("b[style], strong[style]"))) {
    const fw = (bEl.style?.fontWeight || "").toLowerCase().trim();
    if (fw === "normal" || fw === "400" || bEl.id.startsWith("docs-internal-guid")) {
      const span = doc.createElement("span");
      while (bEl.firstChild) span.appendChild(bEl.firstChild);
      bEl.replaceWith(span);
    }
  }

  // 2b. Unwrap search-engine keyword highlight <em> tags before styling conversion
  unwrapSearchHighlightEm(body, doc);

  // 3. Strip permalink '#' / '¶' anchors inside headings
  for (const a of Array.from(body.querySelectorAll<HTMLAnchorElement>("h1 a, h2 a, h3 a, h4 a, h5 a, h6 a"))) {
    const href = (a.getAttribute("href") || "").trim();
    if (href.startsWith("#") && /^[#¶§\s]*$/.test(a.textContent || "")) {
      a.remove();
    }
  }

  // 4. Unwrap Hexo / Rouge / Hugo / GitHub 2-column code-highlight tables into clean <pre><code>
  for (const hl of Array.from(body.querySelectorAll<HTMLElement>("figure.highlight, div.highlight"))) {
    if (!hl.querySelector("table, pre")) continue;
    const cls = `${hl.className || ""} ${hl.querySelector("code, pre")?.className || ""}`;
    const langM =
      /(?:language|lang)-([a-zA-Z0-9_+#.-]+)/.exec(cls) ||
      /\bhighlight-source-([a-zA-Z0-9_+#.-]+)/.exec(cls) ||
      /\bhighlight\s+([a-zA-Z0-9_+#.-]+)/.exec(hl.className || "");
    const lang = hl.getAttribute("data-lang") || (langM ? langM[1] : "");
    hl.querySelectorAll(".gutter, td.blob-num, td.rouge-gutter, pre.lineno, figcaption").forEach((n) => n.remove());
    const lntds = hl.querySelectorAll(":scope table td.lntd");
    if (lntds.length >= 2) lntds[0].remove();
    const codeHost = (hl.querySelector(".code pre, td.rouge-code pre, td.lntd pre, pre, code") || hl) as HTMLElement;
    replaceWithPre(hl, extractPreCodeLines(codeHost), lang);
  }

  for (const tbl of Array.from(body.querySelectorAll<HTMLTableElement>("table"))) {
    let codeCells = Array.from(tbl.querySelectorAll<HTMLElement>("td.blob-code"));
    if (codeCells.length === 0) {
      codeCells = Array.from(tbl.querySelectorAll<HTMLElement>("td.blob-code-inner"));
    }
    if (codeCells.length > 0) {
      const lines = codeCells.map((c) => nodeNbspText(c).replace(/\r?\n/g, ""));
      replaceWithPre(tbl, lines.join("\n"));
    }
  }

  // 4b. Strip ChatGPT / Claude / DeepSeek / CSDN / Juejin / Zhihu sibling code-toolbar headers preceding <pre>
  for (const pre of Array.from(body.querySelectorAll<HTMLElement>("pre"))) {
    const prev = pre.previousElementSibling as HTMLElement | null;
    const parent = pre.parentElement;
    if (
      prev &&
      parent &&
      parent !== body &&
      parent.children.length <= 3 &&
      (prev.tagName === "DIV" || prev.tagName === "HEADER" || prev.tagName === "SPAN") &&
      !prev.querySelector("p, pre, table, ul, ol, blockquote, img, h1, h2, h3, h4, h5, h6")
    ) {
      const headerLang = extractToolbarCodeLang(prev);
      const rawHeaderTxt = (prev.textContent || "").trim();
      if (headerLang || rawHeaderTxt === "" || /^(?:copy\s*code|复制代码|复制)$/i.test(rawHeaderTxt)) {
        if (headerLang && !pre.getAttribute("data-lang")) {
          pre.setAttribute("data-extracted-lang", headerLang);
        }
        prev.remove();
      }
    }
  }

  // 5. Normalize <br>, WeChat .code-snippet_outer, and .line spans inside remaining <pre> blocks
  for (const pre of Array.from(body.querySelectorAll<HTMLElement>("pre"))) {
    if (pre.classList.contains("mermaid")) continue;
    const codeEl = pre.querySelector("code");
    const langCls = `${pre.className || ""} ${codeEl?.className || ""}`;
    const langM = /(?:language|lang)-([a-zA-Z0-9_+#.-]+)/.exec(langCls);
    const attrLang = (
      pre.getAttribute("data-lang") ||
      pre.getAttribute("data-language") ||
      codeEl?.getAttribute("data-lang") ||
      codeEl?.getAttribute("data-language") ||
      pre.getAttribute("data-extracted-lang") ||
      (langM ? langM[1] : "")
    )
      .trim()
      .toLowerCase();
    const txt = extractPreCodeLines(codeEl || pre);
    const cleanCode = doc.createElement("code");
    if (attrLang && attrLang !== "plain" && attrLang !== "plaintext") {
      cleanCode.className = `language-${attrLang}`;
      cleanCode.setAttribute("data-lang", attrLang);
    }
    cleanCode.textContent = txt;
    pre.replaceChildren(cleanCode);
  }

  // 6. Resolve lazy-loaded <img> sources (WeChat data-src, Zhihu data-actualsrc/data-original, srcset) & decode &amp;
  for (const img of Array.from(body.querySelectorAll<HTMLImageElement>("img"))) {
    let realSrc = (
      img.getAttribute("data-actualsrc") ||
      img.getAttribute("data-original") ||
      img.getAttribute("data-lazy-src") ||
      img.getAttribute("data-src") ||
      img.getAttribute("src") ||
      ""
    ).trim();
    if ((!realSrc || realSrc.startsWith("data:image/svg+xml")) && img.getAttribute("srcset")) {
      const firstSet = (img.getAttribute("srcset") || "").split(",")[0]?.trim().split(/\s+/)[0] || "";
      if (firstSet) realSrc = firstSet;
    }
    realSrc = cleanClipboardUrl(realSrc, "src");
    if (!realSrc) {
      img.remove();
      continue;
    }
    img.setAttribute("data-volo-img-src", realSrc);
  }

  // 7. Convert semantic inline CSS spans (Word / Google Docs / Notion / Feishu / WeChat)
  for (const span of Array.from(body.querySelectorAll<HTMLElement>("span[style]"))) {
    if (span.closest("pre, code, .katex, math")) continue;
    const st = span.style;
    if (!st) continue;
    const hasBlockChild = Boolean(
      span.querySelector("p, div, section, article, ul, ol, table, pre, blockquote, h1, h2, h3, h4, h5, h6")
    );
    if (hasBlockChild) continue;
    const fw = (st.fontWeight || "").toLowerCase().trim();
    const isBold = fw === "bold" || fw === "bolder" || (Number(fw) >= 600 && !Number.isNaN(Number(fw)));
    const isItalic = st.fontStyle === "italic" || st.fontStyle === "oblique";
    const td = `${st.textDecoration || ""} ${st.textDecorationLine || ""}`.toLowerCase();
    const isStrike = td.includes("line-through");
    const ff = (st.fontFamily || "").toLowerCase();
    const isMono = /\b(?:monospace|consolas|menlo|monaco|courier|sfmono-regular|fira\s*code|jetbrains\s*mono)\b/.test(ff);
    const bgCol = (st.backgroundColor || "").trim();
    const isHighlight = !isMono && isHighlightBackgroundColor(bgCol);

    const wrapInner = (tagName: string) => {
      const w = doc.createElement(tagName);
      while (span.firstChild) w.appendChild(span.firstChild);
      span.appendChild(w);
    };
    if (isMono && !span.querySelector("code")) {
      wrapInner("code");
    } else {
      if (isHighlight && !span.querySelector("mark")) {
        wrapInner("mark");
        st.backgroundColor = "";
      }
      if (isStrike) wrapInner("del");
      if (isItalic) wrapInner("em");
      if (isBold) wrapInner("strong");
    }
  }

  // 8. Only hijack paste when the clipboard HTML genuinely carries rich structure/formatting
  const hasRichSemantics = Boolean(
    body.querySelector(
      "h1, h2, h3, h4, h5, h6, strong, b, em, i, del, s, strike, u, mark, code, pre, kbd, sup, sub, a[href], img[src], img[data-volo-img-src], table, ul, ol, dl, blockquote, hr, details, figure, audio, video, math, .katex"
    )
  );
  const hasFontColor = Array.from(body.querySelectorAll<HTMLElement>("font[color]")).some((f) => {
    const c = (f.getAttribute("color") || "").trim();
    return Boolean(c && SAFE_IR_COLOR_RE.test(c) && !isDefaultBodyTextColor(c));
  });
  const hasColorSpan = Array.from(body.querySelectorAll<HTMLElement>("span[style]")).some((s) => {
    const c = (s.style?.color || "").trim();
    return Boolean(c && SAFE_IR_COLOR_RE.test(c) && !isDefaultBodyTextColor(c));
  });
  if (!hasRichSemantics && !hasFontColor && !hasColorSpan) {
    return null;
  }

  // 9. Queue embedded data:image/*;base64 images for automatic local upload
  const pendingUploads: Array<{ placeholder: string; file: File }> = [];
  const randWords = new Uint32Array(1);
  crypto.getRandomValues(randWords);
  const batchTag = `${Date.now().toString(36)}${randWords[0].toString(36)}`;
  let imgIdx = 0;
  for (const img of Array.from(body.querySelectorAll<HTMLImageElement>("img[data-volo-img-src^='data:image/'], img[src^='data:image/']"))) {
    const src = img.getAttribute("data-volo-img-src") || img.getAttribute("src") || "";
    const file = dataUrlToFile(src, ++imgIdx);
    if (file && file.size > 0 && file.size <= 15 * 1024 * 1024) {
      const placeholder = `/__volo_paste_img_${batchTag}_${imgIdx}__`;
      img.setAttribute("data-volo-img-src", placeholder);
      pendingUploads.push({ placeholder, file });
    } else {
      img.remove();
    }
  }

  const markdown = htmlToMarkdown(body);
  if (!markdown) return null;
  return { markdown, pendingUploads };
}

function injectSplitPreviewMarkers(md: string): string {
  if (!md.includes("<!--")) return md;
  const lines = md.split("\n");
  let fenceChar = "";
  let fenceLen = 0;
  for (let i = 0; i < lines.length; i++) {
    const ln = lines[i];
    if (!fenceChar) {
      const open = matchFenceOpen(ln);
      if (open) {
        fenceChar = open.fenceChar;
        fenceLen = open.fenceLen;
        continue;
      }
      if (/^\s*<!--\s*more\s*-->\s*$/i.test(ln)) {
        lines[i] = "\n§VOLO_MORE_MARKER§\n";
      }
    } else if (matchFenceClose(ln, fenceChar, fenceLen)) {
      fenceChar = "";
      fenceLen = 0;
    }
  }
  return lines.join("\n");
}

async function fetchPreviewHTML(
  contentMd: string,
  locale: string,
  preserveDirectives: boolean,
  hasAudio: boolean,
  signal?: AbortSignal,
  forExport?: boolean
): Promise<string> {
  const effectiveMd = preserveDirectives || forExport ? contentMd : injectSplitPreviewMarkers(contentMd);
  const payload: Record<string, unknown> = { content_md: effectiveMd, locale, has_audio: hasAudio };
  if (preserveDirectives) {
    payload.preserve_directives = true;
  }
  try {
    const res = await apiFetch<{ html: string }>("/api/admin/render/preview", {
      method: "POST",
      body: JSON.stringify(payload),
      signal,
    });
    return res.html || "";
  } catch (err: any) {
    if (err?.status === 404) {
      const fallback = await apiFetch<{ html: string }>("/api/admin/articles/preview", {
        method: "POST",
        body: JSON.stringify(payload),
        signal,
      });
      return fallback.html || "";
    }
    throw err;
  }
}

interface OutlineItem {
  level: number;
  text: string;
  line: number;
}

const EDITOR_SWATCHES = ["#ff0000", "#ea580c", "#ca8a04", "#16a34a", "#2563eb", "#7c3aed", "#64748b"] as const;

const EDITOR_CALLOUTS = [
  { kind: "NOTE", color: "#2563eb" },
  { kind: "TIP", color: "#16a34a" },
  { kind: "IMPORTANT", color: "#7c3aed" },
  { kind: "WARNING", color: "#ca8a04" },
  { kind: "CAUTION", color: "#dc2626" },
] as const;

type ToolbarPopKind = "" | "heading" | "color" | "callout" | "media" | "codeblock" | "book" | "math" | "export";

interface SlashCmdItem {
  id: string;
  keywords: string;
  label: string;
  kbd: string;
  action: "snippet" | "code" | "math" | "lock" | "media";
  payload?: string;
}

export function CM6EditorCore({ value, onChange, quickInserts = [], onSave, hasAudio }: CM6EditorProps) {
  activeWysiwygSourceMD = value;
  const editorRootRef = useRef<HTMLDivElement | null>(null);
  const hostRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const wysiwygRef = useRef<HTMLDivElement | null>(null);
  const previewRef = useRef<HTMLDivElement | null>(null);
  const phCompartment = useRef(new Compartment());
  const modeCompartment = useRef(new Compartment());
  const valueRef = useRef(value);
  valueRef.current = value;
  const composingRef = useRef<boolean>(false);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onSaveRef = useRef(onSave);
  onSaveRef.current = onSave;

  const curLocale = localeSig.value;
  const [mode, setMode] = useState<EditorMode>(() => {
    const saved = localStorage.getItem("volo_editor_mode");
    return saved === "wysiwyg" || saved === "sv" || saved === "ir" ? saved : "ir";
  });
  const modeRef = useRef<EditorMode>(mode);
  modeRef.current = mode;
  const [syncScroll, setSyncScroll] = useState<boolean>(() => {
    return localStorage.getItem("volo_editor_sync_scroll") !== "0";
  });
  const syncScrollRef = useRef(syncScroll);
  syncScrollRef.current = syncScroll;
  const scrollDriverRef = useRef<{ source: "cm" | "pv" | null; timer: number }>({ source: null, timer: 0 });

  const [previewHTML, setPreviewHTML] = useState<string>("");
  const [uploading, setUploading] = useState<boolean>(false);
  const [fullscreen, setFullscreen] = useState<boolean>(false);
  /** Soft keyboard is open while focus is inside the editor (mobile; derived from visualViewport). */
  const [kbOpen, setKbOpen] = useState<boolean>(false);
  /** Focus snapshot taken right before a fullscreen toggle so the caret survives the relayout. */
  const fsFocusRef = useRef<{ kind: "cm" | "wysiwyg"; range: Range | null } | null>(null);
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
  const [qiMediaMime, setQiMediaMime] = useState<"" | "image" | "video" | "audio">("");
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

  // Native Find & Replace Bar State
  const [findOpen, setFindOpen] = useState<boolean>(false);
  const [findShowReplace, setFindShowReplace] = useState<boolean>(false);
  const [findQuery, setFindQuery] = useState<string>("");
  const [replaceQuery, setReplaceQuery] = useState<string>("");
  const [findCase, setFindCase] = useState<boolean>(false);
  const [findRegex, setFindRegex] = useState<boolean>(false);
  const [findIdx, setFindIdx] = useState<number>(0);
  const findInputRef = useRef<HTMLInputElement | null>(null);
  const openFindBarRef = useRef<(withReplace: boolean) => void>(() => {});

  // Slash `/` Command Palette State
  const [slashMenu, setSlashMenu] = useState<{
    query: string;
    from: number;
    to: number;
    x: number;
    y: number;
    idx: number;
  } | null>(null);
  const slashMenuRef = useRef(slashMenu);
  slashMenuRef.current = slashMenu;
  const slashPopupRef = useRef<HTMLDivElement | null>(null);
  const execSlashItemRef = useRef<(idxOffset?: number) => boolean>(() => false);

  const togglePop = (kind: Exclude<ToolbarPopKind, "">) => {
    setEdTip(null);
    setActivePop((prev) => (prev === kind ? "" : kind));
  };

  const switchMode = (next: EditorMode) => {
    if (next === "wysiwyg") {
      setFindOpen(false);
    } else {
      activeWysiwygCellRef.current = null;
      setWysiwygTblActive(false);
    }
    setMode(next);
    localStorage.setItem("volo_editor_mode", next);
  };

  // Word, char, line stats + live outline (debounced to avoid re-scanning large docs every keystroke)
  const debouncedStatsValue = useDebouncedValue(value, 200);
  const { stats, outline } = useMemo(() => {
    const raw = debouncedStatsValue || "";
    const linesArr = raw.split("\n");
    const cjk = (raw.match(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g) || []).length;
    const latin = (
      raw
        .replace(/[\u4e00-\u9fff\u3040-\u30ff\uac00-\ud7af]/g, " ")
        .match(/[A-Za-z0-9_'-]+/g) || []
    ).length;
    const toc: OutlineItem[] = [];
    let fenceChar = "";
    let fenceLen = 0;
    let fenceQDepth = 0;
    linesArr.forEach((ln, idx) => {
      if (!fenceChar) {
        const open = matchFenceOpen(ln);
        if (open) {
          fenceChar = open.fenceChar;
          fenceLen = open.fenceLen;
          fenceQDepth = open.quoteDepth;
          return;
        }
      } else if (matchFenceClose(ln, fenceChar, fenceLen, fenceQDepth)) {
        fenceChar = "";
        fenceLen = 0;
        fenceQDepth = 0;
        return;
      }
      if (fenceChar) return;
      const m = /^(#{1,4})[ \t]+(\S[^\n\r\u2028\u2029]*)$/.exec(ln);
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
  }, [debouncedStatsValue]);

  const wysiwygForceSyncRef = useRef<boolean>(false);
  const wysiwygAwaitingInitialSyncRef = useRef<boolean>(false);
  const [wysiwygHydrating, setWysiwygHydrating] = useState<boolean>(false);
  const wysiwygHydratingRef = useRef<boolean>(false);
  wysiwygHydratingRef.current = wysiwygHydrating;
  const previewSourceRef = useRef<string | null>(null);
  const wysiwygDirtyRef = useRef<boolean>(false);
  const wysiwygComposingRef = useRef<boolean>(false);
  const wysiwygEmitQueuedRef = useRef<boolean>(false);
  const wysiwygPlaceCaretRef = useRef<boolean>(false);
  const wysiwygEditSeqRef = useRef<number>(0);
  const propSyncRef = useRef(createPropSyncTracker());
  const wysiwygHistoryRef = useRef<{
    stack: Array<{ frag: DocumentFragment; sig: string; ts: number }>;
    idx: number;
  }>({ stack: [], idx: -1 });
  const wysiwygApplyingHistoryRef = useRef<boolean>(false);
  const lastWysiwygRangeRef = useRef<Range | null>(null);
  const activeWysiwygCellRef = useRef<HTMLTableCellElement | null>(null);
  const [wysiwygTblActive, setWysiwygTblActive] = useState<boolean>(false);

  const markWysiwygDirty = (dirty = true) => {
    if (dirty) wysiwygDirtyRef.current = true;
    wysiwygEditSeqRef.current += 1;
    wysiwygForceSyncRef.current = false;
    wysiwygAwaitingInitialSyncRef.current = false;
  };

  const commitWysiwygEdit = (root: HTMLElement, saveSel = true) => {
    if (saveSel) saveWysiwygSelection(root);
    pushWysiwygHistory(root, true);
    onChangeRef.current(htmlToMarkdown(root));
  };

  const dispatchRange = (
    from: number,
    to: number,
    insert: string,
    anchor: number,
    head?: number,
    extra?: { userEvent?: string; scrollIntoView?: boolean },
    focus = true
  ) => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      changes: { from, to, insert },
      selection: head !== undefined ? { anchor, head } : { anchor },
      ...extra,
    });
    if (focus) view.focus();
  };

  const pushWysiwygHistory = (root: HTMLElement | null, force = false, reset = false) => {
    if (!root || wysiwygApplyingHistoryRef.current) return;
    const h = wysiwygHistoryRef.current;
    const sig = root.innerHTML;
    const now = Date.now();
    const frag = document.createDocumentFragment();
    for (const child of Array.from(root.childNodes)) {
      frag.appendChild(child.cloneNode(true));
    }
    if (reset) {
      h.stack = [{ frag, sig, ts: now }];
      h.idx = 0;
      return;
    }
    if (h.idx < h.stack.length - 1) {
      h.stack = h.stack.slice(0, h.idx + 1);
    }
    const top = h.stack[h.idx];
    if (top && top.sig === sig) return;
    if (!force && top && h.idx > 0 && now - top.ts < 400) {
      h.stack[h.idx] = { frag, sig, ts: now };
      return;
    }
    h.stack.push({ frag, sig, ts: now });
    if (h.stack.length > 100) {
      h.stack.shift();
    }
    h.idx = h.stack.length - 1;
  };

  const applyWysiwygHistoryStep = (delta: -1 | 1): boolean => {
    if (wysiwygHydratingRef.current) return false;
    const root = wysiwygRef.current;
    if (!root) return false;
    const h = wysiwygHistoryRef.current;
    const nextIdx = h.idx + delta;
    if (nextIdx < 0 || nextIdx >= h.stack.length) return false;
    const entry = h.stack[nextIdx];
    if (!entry) return false;
    h.idx = nextIdx;
    wysiwygApplyingHistoryRef.current = true;
    try {
      markWysiwygDirty();
      root.replaceChildren(entry.frag.cloneNode(true));
      normalizeWysiwygDOM(root, t("common.delete"));
      root.querySelectorAll<HTMLButtonElement>(".volo-copy-btn[data-copied]").forEach((btn) => {
        btn.removeAttribute("data-copied");
        btn.textContent = t("ed.copy_code");
      });
      root.querySelectorAll<HTMLSelectElement>(".volo-code-lang-select").forEach((sel) => {
        sel.value = sel.closest("figure.volo-codeblock, .volo-code-block")?.getAttribute("data-lang") || "";
      });
      root.focus();
      placeCaretInBlock(root, false);
      commitWysiwygEdit(root);
    } finally {
      wysiwygApplyingHistoryRef.current = false;
    }
    return true;
  };

  const saveWysiwygSelection = (root?: HTMLElement | null) => {
    const host = root || wysiwygRef.current;
    if (!host) return;
    const sel = window.getSelection();
    if (!sel || sel.rangeCount === 0) return;
    const r = sel.getRangeAt(0);
    if (host.contains(r.commonAncestorContainer)) {
      lastWysiwygRangeRef.current = r.cloneRange();
      const baseEl = r.startContainer instanceof Element ? r.startContainer : r.startContainer.parentElement;
      const cell = baseEl?.closest("td, th") as HTMLTableCellElement | null;
      const validCell = cell && host.contains(cell) ? cell : null;
      activeWysiwygCellRef.current = validCell;
      setWysiwygTblActive(Boolean(validCell));
    }
  };

  const mutateWysiwygTable = (
    op: "row_above" | "row_below" | "del_row" | "col_left" | "col_right" | "del_col" | "del_table"
  ) => {
    const root = wysiwygRef.current;
    const cell = activeWysiwygCellRef.current;
    if (!root || !cell || !root.contains(cell)) return;
    const tr = cell.closest("tr") as HTMLTableRowElement | null;
    const table = cell.closest("table") as HTMLTableElement | null;
    if (!tr || !table || !root.contains(table)) return;
    markWysiwygDirty();
    const allRows = Array.from(table.querySelectorAll("tr"));
    const colCount = Math.max(1, tr.children.length);
    const colIdx = Math.max(0, Array.from(tr.children).indexOf(cell));
    const removeWholeTable = () => {
      const wrap = table.closest(".volo-table-scroll");
      (wrap && root.contains(wrap) ? wrap : table).remove();
      activeWysiwygCellRef.current = null;
      setWysiwygTblActive(false);
      normalizeWysiwygDOM(root, t("common.delete"));
      commitWysiwygEdit(root, false);
    };
    if (op === "del_table") {
      removeWholeTable();
      return;
    }
    let focusTarget: HTMLElement | null = cell;
    if (op === "row_above" || op === "row_below") {
      const inHead = tr.parentElement?.tagName === "THEAD";
      let tbody = table.querySelector("tbody");
      if (!tbody) {
        tbody = mkEl("tbody");
        table.appendChild(tbody);
      }
      if (inHead && op === "row_above") {
        const demoted = mkEl("tr");
        for (const ch of Array.from(tr.children)) {
          const td = mkEl("td");
          if (ch.hasChildNodes()) {
            td.replaceChildren(...Array.from(ch.childNodes));
          } else {
            td.appendChild(mkEl("br"));
          }
          demoted.appendChild(td);
          ch.replaceChildren(mkEl("br"));
        }
        tbody.insertBefore(demoted, tbody.firstChild);
        focusTarget = (tr.children[colIdx] as HTMLElement) || cell;
      } else {
        const newTr = mkEl("tr");
        for (let i = 0; i < colCount; i++) {
          const td = mkEl("td");
          td.appendChild(mkEl("br"));
          newTr.appendChild(td);
        }
        if (inHead) {
          tbody.insertBefore(newTr, tbody.firstChild);
        } else if (op === "row_above") {
          tr.before(newTr);
        } else {
          tr.after(newTr);
        }
        focusTarget = (newTr.children[colIdx] as HTMLElement) || null;
      }
    } else if (op === "del_row") {
      if (allRows.length <= 1) {
        removeWholeTable();
        return;
      }
      const inHead = tr.parentElement?.tagName === "THEAD";
      const tbody = table.querySelector("tbody");
      if (inHead && tbody && tbody.rows.length > 0) {
        const nextRow = tbody.rows[0];
        Array.from(tr.children).forEach((th, idx) => {
          const srcCell = nextRow.children[idx];
          if (srcCell && srcCell.hasChildNodes()) {
            th.replaceChildren(...Array.from(srcCell.childNodes));
          } else {
            th.replaceChildren(mkEl("br"));
          }
        });
        nextRow.remove();
        focusTarget = (tr.children[colIdx] as HTMLElement) || null;
      } else {
        const neighbor = (tr.nextElementSibling || tr.previousElementSibling) as HTMLTableRowElement | null;
        tr.remove();
        if (table.querySelectorAll("tr").length === 0) {
          removeWholeTable();
          return;
        }
        focusTarget = (neighbor?.children[Math.min(colIdx, Math.max(0, (neighbor?.children.length || 1) - 1))] as HTMLElement) || null;
      }
    } else if (op === "col_left" || op === "col_right") {
      for (const r of allRows) {
        const refC = r.children[Math.min(colIdx, Math.max(0, r.children.length - 1))];
        const tag = r.parentElement?.tagName === "THEAD" || refC?.tagName === "TH" ? "th" : "td";
        const nc = mkEl(tag);
        nc.appendChild(mkEl("br"));
        if (!refC) r.appendChild(nc);
        else if (op === "col_left") refC.before(nc);
        else refC.after(nc);
        if (r === tr) focusTarget = nc;
      }
    } else if (op === "del_col") {
      if (colCount <= 1) {
        removeWholeTable();
        return;
      }
      for (const r of allRows) {
        r.children[colIdx]?.remove();
      }
      focusTarget = (tr.children[Math.min(colIdx, tr.children.length - 1)] as HTMLElement) || null;
    }
    root.focus();
    if (focusTarget && root.contains(focusTarget)) {
      placeCaretInBlock(focusTarget, true);
      saveWysiwygSelection(root);
    }
    commitWysiwygEdit(root, false);
  };

  const restoreWysiwygSelection = (root: HTMLElement): Range | null => {
    const sel = window.getSelection();
    if (sel && sel.rangeCount > 0) {
      const cur = sel.getRangeAt(0);
      if (root.contains(cur.commonAncestorContainer)) {
        lastWysiwygRangeRef.current = cur.cloneRange();
        return cur;
      }
    }
    const saved = lastWysiwygRangeRef.current;
    if (saved && root.contains(saved.commonAncestorContainer)) {
      root.focus();
      const s = window.getSelection();
      if (s) {
        s.removeAllRanges();
        const cloned = saved.cloneRange();
        s.addRange(cloned);
        return cloned;
      }
    }
    return null;
  };

  useEffect(() => {
    if (mode !== "wysiwyg") return;
    const onSelChange = () => {
      if (wysiwygComposingRef.current) return;
      saveWysiwygSelection(wysiwygRef.current);
    };
    document.addEventListener("selectionchange", onSelChange);
    return () => document.removeEventListener("selectionchange", onSelChange);
  }, [mode]);

  const scheduleWysiwygEmit = (root: HTMLElement, placeEmptyCaret = false) => {
    markWysiwygDirty();
    if (placeEmptyCaret) wysiwygPlaceCaretRef.current = true;
    if (wysiwygEmitQueuedRef.current) return;
    wysiwygEmitQueuedRef.current = true;
    queueMicrotask(() => {
      wysiwygEmitQueuedRef.current = false;
      const shouldPlace = wysiwygPlaceCaretRef.current;
      wysiwygPlaceCaretRef.current = false;
      if (wysiwygComposingRef.current || !root.isConnected) return;
      const wasEmpty = root.children.length === 0;
      normalizeWysiwygDOM(root, t("common.delete"));
      if (shouldPlace && wasEmpty && root.firstElementChild instanceof HTMLElement) {
        placeCaretInBlock(root.firstElementChild, true);
      }
      pushWysiwygHistory(root);
      const md = htmlToMarkdown(root);
      if (md !== (valueRef.current || "")) {
        onChangeRef.current(md);
      }
    });
  };

  const spliceMarkdownAtWysiwygCaret = (root: HTMLElement, mdSnippet: string) => {
    wysiwygEditSeqRef.current += 1;
    wysiwygAwaitingInitialSyncRef.current = false;
    const range = restoreWysiwygSelection(root);
    if (!range) {
      wysiwygForceSyncRef.current = true;
      const base = valueRef.current || "";
      const next = `${base}${base.endsWith("\n") || !base ? "" : "\n\n"}${mdSnippet.replace(/^\n+/, "")}`;
      onChangeRef.current(next);
      return;
    }

    const randBuf = new Uint32Array(1);
    crypto.getRandomValues(randBuf);
    const spliceMarker = `__VOLO_SPLICE_${Date.now().toString(36)}_${randBuf[0].toString(36)}__`;
    const marker = document.createElement("span");
    marker.setAttribute("data-volo-splice", spliceMarker);

    const anchorEl =
      range.startContainer instanceof Element ? range.startContainer : range.startContainer.parentElement;
    const nonEditCard = anchorEl?.closest('[contenteditable="false"]') as HTMLElement | null;
    if (nonEditCard && nonEditCard !== root && root.contains(nonEditCard)) {
      let topCard: HTMLElement = nonEditCard;
      while (topCard.parentElement && topCard.parentElement !== root) {
        topCard = topCard.parentElement;
      }
      topCard.after(marker);
    } else {
      if (!range.collapsed) {
        range.deleteContents();
      }
      range.insertNode(marker);
    }

    const serialized = htmlToMarkdown(root);
    marker.remove();

    const idx = serialized.indexOf(spliceMarker);
    if (idx < 0) {
      wysiwygForceSyncRef.current = true;
      const base = serialized || valueRef.current || "";
      onChangeRef.current(`${base}${base ? "\n\n" : ""}${mdSnippet.trim()}`);
      return;
    }

    const before = serialized.slice(0, idx);
    const after = serialized.slice(idx + spliceMarker.length);
    const isBlockSnippet = mdSnippet.startsWith("\n") || /^(?::::|```|\$\$|\| |> |#{1,6}\s|-\s|\d+\.\s|---)/.test(mdSnippet);
    let nextMd: string;
    if (isBlockSnippet) {
      const cleanBlock = mdSnippet.trim();
      const lead = !before ? "" : before.endsWith("\n\n") ? "" : before.endsWith("\n") ? "\n" : "\n\n";
      const trail = !after ? "" : after.startsWith("\n\n") ? "" : after.startsWith("\n") ? "\n" : "\n\n";
      nextMd = `${before}${lead}${cleanBlock}${trail}${after}`;
    } else {
      nextMd = `${before}${mdSnippet}${after}`;
    }
    wysiwygDirtyRef.current = false;
    wysiwygForceSyncRef.current = true;
    onChangeRef.current(nextMd.replace(/\n{3,}/g, "\n\n").trim());
  };

  const getSelectedText = (): string => {
    if (mode === "wysiwyg") {
      const cur = window.getSelection();
      if (cur && cur.rangeCount > 0 && wysiwygRef.current?.contains(cur.getRangeAt(0).commonAncestorContainer)) {
        const txt = cur.toString();
        if (txt) return txt;
      }
      if (lastWysiwygRangeRef.current && wysiwygRef.current?.contains(lastWysiwygRangeRef.current.commonAncestorContainer)) {
        return lastWysiwygRangeRef.current.toString();
      }
      return "";
    }
    const view = viewRef.current;
    if (!view) return "";
    const sel = view.state.selection.main;
    return sel.empty ? "" : view.state.sliceDoc(sel.from, sel.to);
  };

  const removeWysiwygAudioBlock = (root: HTMLElement, block: HTMLElement, preferBlock?: HTMLElement | null) => {
    markWysiwygDirty();
    const fallback = (preferBlock || block.nextElementSibling || block.previousElementSibling) as HTMLElement | null;
    block.remove();
    normalizeWysiwygDOM(root, t("common.delete"));
    const target =
      fallback && root.contains(fallback) && !isNonEditableWysiwygBlock(fallback)
        ? fallback
        : (Array.from(root.children).find((c) => !isNonEditableWysiwygBlock(c)) as HTMLElement | undefined);
    root.focus();
    if (target) {
      placeCaretInBlock(target, false);
      saveWysiwygSelection(root);
    }
    commitWysiwygEdit(root, false);
  };

  const insertSnippet = (snippet: string) => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (mode === "wysiwyg" && wysiwygRef.current) {
      spliceMarkdownAtWysiwygCaret(wysiwygRef.current, snippet);
      return;
    }
    if (mode === "wysiwyg") {
      wysiwygEditSeqRef.current += 1;
      wysiwygAwaitingInitialSyncRef.current = false;
      wysiwygForceSyncRef.current = true;
      const next = `${value}${value.endsWith("\n") || !value ? "" : "\n"}${snippet}`;
      onChangeRef.current(next);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    dispatchRange(sel.from, sel.to, snippet, sel.from + snippet.length);
  };

  const wrapSelection = (before: string, after: string, fallback: string) => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (mode === "wysiwyg" && wysiwygRef.current) {
      const root = wysiwygRef.current;
      const rawSelected = getSelectedText() || fallback;
      if (before === "$") {
        spliceMarkdownAtWysiwygCaret(root, `$${rawSelected}$`);
        return;
      }
      if (before.includes(":::")) {
        spliceMarkdownAtWysiwygCaret(root, `${before}${rawSelected}${after}`);
        return;
      }
      markWysiwygDirty();
      restoreWysiwygSelection(root);
      root.focus();
      const selTxt = escapeClipboardHtml(rawSelected);
      if (before === "**") document.execCommand("bold");
      else if (before === "*") document.execCommand("italic");
      else if (before === "~~") document.execCommand("strikeThrough");
      else if (before === "<u>") document.execCommand("underline");
      else if (before === "<mark>" || before === "==") document.execCommand("insertHTML", false, `<mark>${selTxt}</mark>`);
      else if (before === "^") document.execCommand("insertHTML", false, `<sup>${selTxt}</sup>`);
      else if (before === "~") document.execCommand("insertHTML", false, `<sub>${selTxt}</sub>`);
      else if (before === "[" && after.startsWith("](") && after.endsWith(")")) {
        const rawHref = after.slice(2, -1) || "https://";
        const url = escapeClipboardHtml(safeHref(rawHref) || "https://");
        document.execCommand("insertHTML", false, `<a href="${url}">${selTxt}</a>`);
      } else {
        document.execCommand("insertHTML", false, `<code>${selTxt}</code>`);
      }
      commitWysiwygEdit(root);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const rawSel = view.state.sliceDoc(sel.from, sel.to);
    const isSingleStar = before === "*" && after === "*";

    // Toggle-unwrap when selection itself already starts & ends with matching markers
    if (
      rawSel.length > before.length + after.length &&
      rawSel.startsWith(before) &&
      rawSel.endsWith(after) &&
      !(isSingleStar && rawSel.startsWith("**") && rawSel.endsWith("**") && !rawSel.startsWith("***"))
    ) {
      const inner = rawSel.slice(before.length, rawSel.length - after.length);
      dispatchRange(sel.from, sel.to, inner, sel.from, sel.from + inner.length, { userEvent: "input" });
      return;
    }

    // Toggle-unwrap when surrounding document text immediately wraps the current selection
    if (
      sel.from >= before.length &&
      sel.to + after.length <= view.state.doc.length &&
      view.state.sliceDoc(sel.from - before.length, sel.from) === before &&
      view.state.sliceDoc(sel.to, sel.to + after.length) === after &&
      !(
        isSingleStar &&
        sel.from >= 2 &&
        view.state.sliceDoc(sel.from - 2, sel.from) === "**" &&
        !(sel.from >= 3 && view.state.sliceDoc(sel.from - 3, sel.from) === "***")
      )
    ) {
      dispatchRange(
        sel.from - before.length,
        sel.to + after.length,
        rawSel,
        sel.from - before.length,
        sel.from - before.length + rawSel.length,
        { userEvent: "input" }
      );
      return;
    }

    const selected = rawSel || fallback;
    dispatchRange(
      sel.from,
      sel.to,
      `${before}${selected}${after}`,
      sel.from + before.length,
      sel.from + before.length + selected.length
    );
  };

  const insertCodeBlock = (lang: string) => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (modeRef.current === "wysiwyg" && wysiwygRef.current) {
      const selTxt = trimEndChars(getSelectedText(), "\n", true);
      const fence = computeFenceDelim(selTxt);
      spliceMarkdownAtWysiwygCaret(wysiwygRef.current, `\n${fence}${lang}\n${selTxt}\n${fence}\n`);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const rawSel = trimEndChars(view.state.sliceDoc(sel.from, sel.to), "\n", true);
    const startLine = view.state.doc.lineAt(sel.from);
    const endLine = view.state.doc.lineAt(sel.to);
    const needLeadNl = sel.from > startLine.from ? "\n" : "";
    const needTrailNl = sel.to < endLine.to ? "\n" : "";
    const fence = computeFenceDelim(rawSel);
    const openFence = `${needLeadNl}${fence}${lang}\n`;
    const closeFence = `\n${fence}${needTrailNl}`;
    const insert = `${openFence}${rawSel}${closeFence}`;
    const cursorStart = sel.from + openFence.length;
    dispatchRange(sel.from, sel.to, insert, cursorStart, cursorStart + rawSel.length, {
      scrollIntoView: true,
      userEvent: "input",
    });
  };
  const insertCodeBlockRef = useRef(insertCodeBlock);
  insertCodeBlockRef.current = insertCodeBlock;

  const applyColorStyle = (color: string) => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (mode === "wysiwyg" && wysiwygRef.current) {
      markWysiwygDirty();
      restoreWysiwygSelection(wysiwygRef.current);
      wysiwygRef.current.focus();
      document.execCommand("foreColor", false, color);
      commitWysiwygEdit(wysiwygRef.current);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const rawSel = view.state.sliceDoc(sel.from, sel.to);
    let unwrapped = rawSel || "text";
    let prevUnwrapped: string;
    do {
      prevUnwrapped = unwrapped;
      unwrapped = unwrapped.replace(/^<(?:font|span)\b[^<>]*>([\s\S]*?)<\/(?:font|span)\b[^<>]*>$/i, "$1");
    } while (unwrapped !== prevUnwrapped);
    const before = `<font color="${color}">`;
    dispatchRange(sel.from, sel.to, `${before}${unwrapped}</font>`, sel.from + before.length, sel.from + before.length + unwrapped.length);
  };

  const stripStyledSpans = (s: string): string => {
    const re = /<(span\b[^<>]*|\/span\b[^<>]*)>/gi;
    const stack: Array<{ os: number; oe: number; styled: boolean }> = [];
    const drop: Array<[number, number]> = [];
    let m: RegExpExecArray | null;
    while ((m = re.exec(s)) !== null) {
      if (m[1][0] === "/") {
        const top = stack.pop();
        if (top && top.styled) {
          drop.push([top.os, top.oe], [m.index, m.index + m[0].length]);
        }
      } else {
        stack.push({
          os: m.index,
          oe: m.index + m[0].length,
          styled: /^<span\s+style\s*=\s*(?:"[^"<>]*"|'[^'<>]*')\s*>$/i.test(m[0]),
        });
      }
    }
    if (drop.length === 0) return s;
    drop.sort((a, b) => a[0] - b[0]);
    let out = "";
    let pos = 0;
    for (const [a, b] of drop) {
      if (a > pos) out += s.slice(pos, a);
      if (b > pos) pos = b;
    }
    return out + s.slice(pos);
  };

  const clearInlineStyle = () => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (mode === "wysiwyg" && wysiwygRef.current) {
      markWysiwygDirty();
      restoreWysiwygSelection(wysiwygRef.current);
      wysiwygRef.current.focus();
      document.execCommand("removeFormat");
      commitWysiwygEdit(wysiwygRef.current);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const sel = view.state.selection.main;
    const stripTags = (s: string) =>
      stripStyledSpans(s.replace(/<\/?(?:font|mark|u|sup|sub)\b[^<>]*>/gi, ""))
        .replace(/==([^=\n]+)==/g, "$1");
    if (!sel.empty) {
      const cleaned = stripTags(view.state.sliceDoc(sel.from, sel.to));
      dispatchRange(sel.from, sel.to, cleaned, sel.from, sel.from + cleaned.length);
    } else {
      const line = view.state.doc.lineAt(sel.head);
      const cleaned = stripTags(line.text);
      if (cleaned !== line.text) {
        dispatchRange(line.from, line.to, cleaned, Math.min(line.from + cleaned.length, sel.head));
      } else {
        view.focus();
      }
    }
  };

  const prefixCurrentLine = (prefix: string) => {
    if (modeRef.current === "wysiwyg" && wysiwygHydratingRef.current) return;
    if (mode === "wysiwyg" && wysiwygRef.current) {
      const root = wysiwygRef.current;
      markWysiwygDirty();
      restoreWysiwygSelection(root);
      root.focus();
      if (prefix.startsWith("#")) {
        const lvl = prefix.trim().length;
        document.execCommand("formatBlock", false, `H${lvl}`);
      } else if (prefix === "- ") {
        document.execCommand("insertUnorderedList");
      } else if (prefix === "1. ") {
        document.execCommand("insertOrderedList");
      } else if (prefix === "- [ ] ") {
        const findActiveLi = (): HTMLLIElement | null => {
          const s = window.getSelection();
          if (!s || s.rangeCount === 0) return null;
          const node = s.getRangeAt(0).startContainer;
          const el = node instanceof Element ? node : node.parentElement;
          const li = el?.closest("li") as HTMLLIElement | null;
          return li && root.contains(li) ? li : null;
        };
        let li = findActiveLi();
        if (!li) {
          document.execCommand("insertUnorderedList");
          li = findActiveLi();
        }
        if (li) {
          const existingCb = li.querySelector(':scope > input[type="checkbox"], :scope > p > input[type="checkbox"]');
          if (existingCb) {
            existingCb.remove();
          } else {
            const cb = mkEl("input");
            cb.type = "checkbox";
            li.insertBefore(document.createTextNode(" "), li.firstChild);
            li.insertBefore(cb, li.firstChild);
          }
        } else {
          spliceMarkdownAtWysiwygCaret(root, "\n- [ ] ");
          return;
        }
      } else if (prefix === "> ") {
        document.execCommand("formatBlock", false, "BLOCKQUOTE");
      }
      commitWysiwygEdit(root);
      return;
    }
    const view = viewRef.current;
    if (!view) return;
    const line = view.state.doc.lineAt(view.state.selection.main.from);
    const cleaned = line.text.replace(/^(?:#{1,6}\s+|>\s+|-\s+\[[ xX]\]\s+|-\s+|\d+\.\s+)/, "");
    const nextText = `${prefix}${cleaned}`;
    dispatchRange(line.from, line.to, nextText, line.from + nextText.length);
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
      const url = safeMediaSrc(res.asset.public_url);
      if (!url) {
        showToast(t("ed.upload_failed"));
        return;
      }
      const name = res.asset.orig_filename || "asset";
      const mime = res.asset.mime_type || file.type || "";
      if (isVideoMedia(mime, url)) {
        insertSnippet(`\n<video controls preload="metadata" src="${url}"></video>\n`);
      } else if (isAudioMedia(mime, url)) {
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

  const uploadMediaFiles = async (files: Iterable<File>, sequential = false) => {
    for (const f of files) {
      if (sequential) await uploadMediaFile(f);
      else void uploadMediaFile(f);
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
        let ext: string, blobType: string;
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

  const uploadPendingPasteImages = async (pending: Array<{ placeholder: string; file: File }>) => {
    if (pending.length === 0) return;
    setUploading(true);
    try {
      for (const item of pending) {
        try {
          const fd = new FormData();
          fd.append("file", item.file);
          const res = await apiFetch<{ asset: { public_url: string } }>("/api/admin/media/upload", {
            method: "POST",
            body: fd,
          });
          const uploadedUrl = safeMediaSrc(res?.asset?.public_url, { allowDataImage: false });
          if (!uploadedUrl) continue;

          // Update live WYSIWYG DOM <img> elements first so subsequent keystrokes never revert to placeholder
          const wRoot = wysiwygRef.current;
          let updatedWysiwygDom = false;
          if (wRoot) {
            for (const img of Array.from(wRoot.querySelectorAll<HTMLImageElement>("img"))) {
              const curSrc = img.getAttribute("src") || "";
              if (curSrc === item.placeholder || curSrc.endsWith(item.placeholder)) {
                img.setAttribute("src", uploadedUrl);
                updatedWysiwygDom = true;
              }
            }
          }
          if (modeRef.current === "wysiwyg" && wRoot) {
            if (updatedWysiwygDom) {
              markWysiwygDirty(false);
              commitWysiwygEdit(wRoot, false);
            } else {
              const curVal = valueRef.current || "";
              if (curVal.includes(item.placeholder)) {
                wysiwygEditSeqRef.current += 1;
                wysiwygAwaitingInitialSyncRef.current = false;
                wysiwygForceSyncRef.current = true;
                onChangeRef.current(curVal.split(item.placeholder).join(uploadedUrl));
              }
            }
            continue;
          }

          const view = viewRef.current;
          if (view && !view.composing && !composingRef.current) {
            const curDoc = view.state.doc.toString();
            const idx = curDoc.indexOf(item.placeholder);
            if (idx >= 0) {
              view.dispatch({
                changes: { from: idx, to: idx + item.placeholder.length, insert: uploadedUrl },
              });
              continue;
            }
          }
          const curVal = valueRef.current || "";
          if (curVal.includes(item.placeholder)) {
            onChangeRef.current(curVal.split(item.placeholder).join(uploadedUrl));
          }
        } catch {
          /* keep placeholder or let user retry */
        }
      }
    } finally {
      setUploading(false);
    }
  };

  const handleRichPaste = (
    event: ClipboardEvent,
    inCode: () => boolean,
    insertMd: (md: string, forceBlock: boolean) => void,
    onBeforeConvert?: (rawPlain: string, rawHTML: string) => boolean
  ): boolean => {
    const dt = event.clipboardData;
    if (!dt) return false;
    const types = Array.from(dt.types || []);
    const rawPlain = dt.getData("text/plain") || "";
    const rawHTML = dt.getData("text/html") || "";
    const mediaFiles = extractClipboardMediaFiles(dt);

    if (shouldHandlePasteAsBinaryMedia(dt, mediaFiles, rawPlain, rawHTML)) {
      event.preventDefault();
      void uploadMediaFiles(mediaFiles);
      return true;
    }
    if (types.includes("vscode-editor-data") || types.includes("application/x-jetbrains-editor") || inCode()) {
      return false;
    }
    if (onBeforeConvert?.(rawPlain, rawHTML)) {
      return true;
    }
    if (rawHTML) {
      const converted = clipboardHtmlToMarkdown(rawHTML);
      if (converted && converted.markdown) {
        event.preventDefault();
        insertMd(converted.markdown, false);
        if (converted.pendingUploads.length > 0) {
          void uploadPendingPasteImages(converted.pendingUploads);
        }
        return true;
      }
    }
    const tableMd = tsvToMarkdownTable(rawPlain);
    if (tableMd) {
      event.preventDefault();
      insertMd(tableMd, true);
      return true;
    }
    if (mediaFiles.length > 0 && !rawPlain.trim()) {
      event.preventDefault();
      void uploadMediaFiles(mediaFiles);
      return true;
    }
    return false;
  };

  const exportEditorDocument = async (kind: "md" | "html" | "rich_copy") => {
    const md = valueRef.current || "";
    const stamp = new Date().toISOString().slice(0, 10);
    const triggerDownload = (blob: Blob, filename: string) => {
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = filename;
      document.body.appendChild(a);
      a.click();
      a.remove();
      window.setTimeout(() => URL.revokeObjectURL(url), 1500);
    };

    if (kind === "md") {
      triggerDownload(new Blob([md], { type: "text/markdown;charset=utf-8" }), `article-${stamp}.md`);
      return;
    }

    let rendered = previewHTML;
    try {
      const html = await fetchPreviewHTML(md, curLocale, false, Boolean(hasAudio), undefined, true);
      rendered = html || rendered;
    } catch {
      /* fallback to cached previewHTML */
    }

    if (kind === "html") {
      const fullDoc = `<!doctype html><html lang="${curLocale || "zh-CN"}"><head><meta charset="utf-8" /><meta name="viewport" content="width=device-width, initial-scale=1" /><meta name="color-scheme" content="light dark" /><title>Exported Article</title><style>:root{--bg:#fff;--fg:#1c1917;--surf:#f5f5f4;--quote-bg:#fafaf9;--bd:#e7e5e4;--acc:#d97706;--mark-bg:#fef08a;--mark-fg:#1c1917;--link:#2563eb}@media(prefers-color-scheme:dark){:root{--bg:#121211;--fg:#eceae5;--surf:#1e1d1b;--quote-bg:#181716;--bd:#2e2c29;--acc:#f59e0b;--mark-bg:rgba(250,204,21,.28);--mark-fg:#fef9c3;--link:#60a5fa}[style*="color: #000"],[style*="color:#000"],[style*="color: #111"],[style*="color: #222"],[style*="color: #333"],[style*="color: black"],font[color="black"],font[color="#000"],font[color="#000000"],font[color="#111111"],font[color="#222222"],font[color="#333333"]{color:var(--fg)!important}}body{max-width:780px;margin:40px auto;padding:0 20px;font:16px/1.75 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif;color:var(--fg);background:var(--bg)}a{color:var(--link)}pre{background:var(--surf);padding:14px 16px;border-radius:10px;overflow-x:auto}code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:.9em}blockquote{margin:1em 0;padding:.5em 1em;border-left:3px solid var(--acc);background:var(--quote-bg)}table{border-collapse:collapse;width:100%;margin:1em 0}th,td{border:1px solid var(--bd);padding:8px 12px;text-align:left}th{background:var(--surf)}img,video,audio{max-width:100%;border-radius:8px}mark{background:var(--mark-bg);color:var(--mark-fg);padding:.1em .28em;border-radius:4px}</style></head><body>${rendered}</body></html>`;
      triggerDownload(new Blob([fullDoc], { type: "text/html;charset=utf-8" }), `article-${stamp}.html`);
      return;
    }

    try {
      if (typeof ClipboardItem !== "undefined" && navigator.clipboard?.write) {
        await navigator.clipboard.write([
          new ClipboardItem({
            "text/html": new Blob([rendered], { type: "text/html" }),
            "text/plain": new Blob([md], { type: "text/plain" }),
          }),
        ]);
      } else {
        await navigator.clipboard?.writeText(rendered);
      }
      showToast(t("ed.copied_rich_html"), "ok");
    } catch {
      showToast(t("ed.copied"), "ok");
    }
  };

  const findOpenRef = useRef(findOpen);
  findOpenRef.current = findOpen;

  const findResult = useMemo<{ matches: SearchMatchRange[]; invalid: boolean }>(() => {
    if (!findOpen || !findQuery) return { matches: [], invalid: false };
    let re: RegExp;
    try {
      const pattern = findRegex ? findQuery : findQuery.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      re = new RegExp(pattern, findCase ? "g" : "gi");
    } catch {
      return { matches: [], invalid: true };
    }
    const matches: SearchMatchRange[] = [];
    let m: RegExpExecArray | null;
    while ((m = re.exec(value)) !== null) {
      if (m[0].length === 0) {
        re.lastIndex = m.index + 1;
        if (re.lastIndex > value.length) break;
        continue;
      }
      matches.push({ from: m.index, to: m.index + m[0].length });
      if (matches.length >= 2000) break;
    }
    return { matches, invalid: false };
  }, [findOpen, findQuery, findCase, findRegex, value]);

  const findMatches = findResult.matches;
  const findInvalid = findResult.invalid;
  const safeFindIdx = findMatches.length === 0 ? 0 : Math.min(findIdx, findMatches.length - 1);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      effects: setSearchStateEffect.of({
        matches: findOpen && mode !== "wysiwyg" ? findMatches : [],
        activeIdx: safeFindIdx,
      }),
    });
  }, [findOpen, mode, findMatches, safeFindIdx]);

  const openFindBar = (withReplace: boolean) => {
    let selTxt = "";
    if (modeRef.current === "wysiwyg") {
      selTxt = window.getSelection()?.toString() || "";
    } else if (viewRef.current) {
      const sel = viewRef.current.state.selection.main;
      if (!sel.empty) selTxt = viewRef.current.state.sliceDoc(sel.from, sel.to);
    }
    if (selTxt && !selTxt.includes("\n") && selTxt.length <= 120) {
      setFindQuery(selTxt);
      setFindIdx(0);
    }
    if (modeRef.current === "wysiwyg") {
      switchMode("ir");
    }
    setMobilePane("write");
    setFindOpen(true);
    if (withReplace) setFindShowReplace(true);
    requestAnimationFrame(() => {
      findInputRef.current?.focus();
      findInputRef.current?.select();
    });
  };
  openFindBarRef.current = openFindBar;

  const closeFindBar = () => {
    setFindOpen(false);
    viewRef.current?.focus();
  };

  const stepFindMatch = (dir: 1 | -1) => {
    if (findMatches.length === 0) return;
    setMobilePane("write");
    const nextIdx = (safeFindIdx + dir + findMatches.length) % findMatches.length;
    setFindIdx(nextIdx);
    const target = findMatches[nextIdx];
    const view = viewRef.current;
    if (view && target) {
      view.dispatch({
        selection: { anchor: target.from, head: target.to },
        effects: EditorView.scrollIntoView(target.from, { y: "center" }),
      });
    }
  };

  const computeMatchReplacement = (matchedText: string): string => {
    if (!findRegex) return replaceQuery;
    try {
      return matchedText.replace(new RegExp(findQuery, findCase ? "" : "i"), replaceQuery);
    } catch {
      return replaceQuery;
    }
  };

  const replaceCurrentMatch = () => {
    const view = viewRef.current;
    if (!view || findMatches.length === 0) return;
    const target = findMatches[safeFindIdx];
    if (!target) return;
    const rep = computeMatchReplacement(view.state.sliceDoc(target.from, target.to));
    dispatchRange(target.from, target.to, rep, target.from + rep.length, undefined, { scrollIntoView: true, userEvent: "input" }, false);
  };

  const replaceAllMatches = () => {
    const view = viewRef.current;
    if (!view || findMatches.length === 0) return;
    const changes = findMatches.map((m) => ({
      from: m.from,
      to: m.to,
      insert: computeMatchReplacement(view.state.sliceDoc(m.from, m.to)),
    }));
    view.dispatch({ changes, userEvent: "input" });
    setFindIdx(0);
  };

  const slashItems = useMemo<SlashCmdItem[]>(
    () => [
      { id: "h1", keywords: "h1 heading1 title 一级标题", label: `${t("ed.heading")} 1`, kbd: "# ", action: "snippet", payload: "# " },
      { id: "h2", keywords: "h2 heading2 subtitle 二级标题", label: `${t("ed.heading")} 2`, kbd: "## ", action: "snippet", payload: "## " },
      { id: "h3", keywords: "h3 heading3 三级标题", label: `${t("ed.heading")} 3`, kbd: "### ", action: "snippet", payload: "### " },
      { id: "code", keywords: "code fence block 代码块", label: t("ed.code_block"), kbd: "```", action: "code" },
      { id: "quote", keywords: "quote blockquote 引用", label: t("ed.quote"), kbd: "> ", action: "snippet", payload: "> " },
      { id: "note", keywords: "note info callout 提示", label: t("ed.callout_note"), kbd: "[!NOTE]", action: "snippet", payload: "> [!NOTE]\n> " },
      { id: "tip", keywords: "tip callout 技巧", label: t("ed.callout_tip"), kbd: "[!TIP]", action: "snippet", payload: "> [!TIP]\n> " },
      { id: "warn", keywords: "warn warning caution callout 警告", label: t("ed.callout_warning"), kbd: "[!WARNING]", action: "snippet", payload: "> [!WARNING]\n> " },
      { id: "task", keywords: "todo task check 待办 任务", label: t("ed.task"), kbd: "- [ ]", action: "snippet", payload: "- [ ] " },
      { id: "ul", keywords: "list bullet ul 列表", label: t("ed.ul"), kbd: "- ", action: "snippet", payload: "- " },
      { id: "table", keywords: "table grid 表格", label: t("ed.table"), kbd: "| |", action: "snippet", payload: `${t("ed.table_tpl")}\n` },
      { id: "math", keywords: "math latex katex 公式", label: t("ed.math_block"), kbd: "$$", action: "math" },
      {
        id: "mermaid",
        keywords: "mermaid diagram chart 流程图",
        label: t("ed.mermaid_diagram"),
        kbd: "mermaid",
        action: "snippet",
        payload: "```mermaid\nflowchart LR\n  A[Start] --> B[Done]\n```\n",
      },
      { id: "media", keywords: "image media photo audio video 图片 媒体", label: t("ed.insert_from_library"), kbd: "![](...)", action: "media" },
      { id: "lock", keywords: "lock password secret 加密 密码", label: t("ed.sc_lock_pwd"), kbd: ":::lock", action: "lock" },
      { id: "hr", keywords: "hr divider line 分割线", label: t("ed.hr"), kbd: "---", action: "snippet", payload: "---\n" },
      { id: "toc", keywords: "toc outline 目录", label: t("ed.insert_toc"), kbd: "[toc]", action: "snippet", payload: "[toc]\n" },
    ],
    [curLocale]
  );

  const slashItemsRef = useRef<SlashCmdItem[]>([]);
  slashItemsRef.current = slashItems;

  const filteredSlashItems = useMemo(() => {
    if (!slashMenu) return [];
    const q = slashMenu.query.trim().toLowerCase();
    if (!q) return slashItems;
    return slashItems.filter(
      (it) => it.id.includes(q) || it.keywords.toLowerCase().includes(q) || it.label.toLowerCase().includes(q)
    );
  }, [slashMenu, slashItems]);

  const filteredSlashItemsRef = useRef<SlashCmdItem[]>([]);
  filteredSlashItemsRef.current = filteredSlashItems;

  const computeSlashMenuXY = (coords: { left: number; top: number; bottom: number }, query: string) => {
    const q = query.trim().toLowerCase();
    const count = !q
      ? slashItemsRef.current.length
      : slashItemsRef.current.filter(
          (it) => it.id.includes(q) || it.keywords.toLowerCase().includes(q) || it.label.toLowerCase().includes(q)
        ).length;
    const vv = window.visualViewport;
    const vpTop = vv ? vv.offsetTop : 0;
    const vpLeft = vv ? vv.offsetLeft : 0;
    const vpW = vv ? vv.width : window.innerWidth;
    const vpH = vv ? vv.height : window.innerHeight;
    const estH = Math.min(260, Math.max(1, count) * 40 + 16) + 6;
    const x = Math.max(vpLeft + 12, Math.min(vpLeft + vpW - 260, Math.round(coords.left)));
    const y =
      coords.bottom + estH > vpTop + vpH
        ? Math.max(vpTop + 12, Math.round(coords.top - estH))
        : Math.round(coords.bottom + 6);
    return { x, y };
  };
  const computeSlashMenuXYRef = useRef(computeSlashMenuXY);
  computeSlashMenuXYRef.current = computeSlashMenuXY;

  const executeSlashItem = (item: SlashCmdItem): boolean => {
    const cur = slashMenuRef.current;
    const view = viewRef.current;
    if (!cur || !view) return false;
    setSlashMenu(null);
    const doc = view.state.doc;
    const fromPos = Math.max(0, Math.min(doc.length, cur.from));
    const toPos = Math.max(fromPos, Math.min(doc.length, cur.to));
    const line = doc.lineAt(fromPos);
    const leadText = doc.sliceString(line.from, fromPos);

    if (item.action === "code") {
      dispatchRange(fromPos, toPos, "```go\n\n```", fromPos + 6, undefined, { scrollIntoView: true, userEvent: "input" });
      return true;
    }
    if (item.action === "math") {
      dispatchRange(fromPos, toPos, "$$\n\n$$", fromPos + 3, undefined, { scrollIntoView: true, userEvent: "input" });
      return true;
    }
    if (item.action === "media") {
      dispatchRange(fromPos, toPos, "", fromPos, undefined, { userEvent: "input" }, false);
      setMediaModalOpen(true);
      return true;
    }
    if (item.action === "lock") {
      dispatchRange(fromPos, toPos, "", fromPos, undefined, { userEvent: "input" }, false);
      setLockMode("pwd");
      setQiTab("lock");
      setQiOpen(true);
      return true;
    }
    const payload = item.payload || "";
    const replaceStart = payload.startsWith("> ") && /^\s*>\s*$/.test(leadText) ? line.from : fromPos;
    dispatchRange(replaceStart, toPos, payload, replaceStart + payload.length, undefined, { scrollIntoView: true, userEvent: "input" });
    return true;
  };

  execSlashItemRef.current = (idxOverride?: number) => {
    const list = filteredSlashItemsRef.current;
    const cur = slashMenuRef.current;
    if (!cur || list.length === 0) return false;
    const idx = Math.max(0, Math.min(list.length - 1, idxOverride ?? cur.idx));
    return executeSlashItem(list[idx]);
  };

  useEffect(() => {
    if (!slashMenu) return;
    const onDown = (e: MouseEvent) => {
      if (slashPopupRef.current?.contains(e.target as Node)) return;
      setSlashMenu(null);
    };
    const onScroll = (e: Event) => {
      if (slashPopupRef.current && e.target instanceof Node && slashPopupRef.current.contains(e.target)) return;
      setSlashMenu(null);
    };
    const onVpChange = () => {
      const cur = slashMenuRef.current;
      const view = viewRef.current;
      if (!cur || !view || !view.dom.isConnected) return;
      const pos = Math.max(0, Math.min(view.state.doc.length, cur.to));
      const coords = view.coordsAtPos(pos);
      if (!coords) return;
      const { x, y } = computeSlashMenuXYRef.current(coords, cur.query);
      setSlashMenu((prev) => (prev ? { ...prev, x, y } : null));
    };
    const vv = window.visualViewport;
    document.addEventListener("mousedown", onDown, true);
    window.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onVpChange);
    vv?.addEventListener("resize", onVpChange);
    vv?.addEventListener("scroll", onVpChange);
    return () => {
      document.removeEventListener("mousedown", onDown, true);
      window.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onVpChange);
      vv?.removeEventListener("resize", onVpChange);
      vv?.removeEventListener("scroll", onVpChange);
    };
  }, [slashMenu !== null]);

  const modeExtensions = (m: EditorMode) =>
    m === "ir" ? [buildInstantRenderPlugin()] : [lineNumbers()];

  useEffect(() => {
    if (!hostRef.current) return;

    const writeMdSelectionClipboard = (event: ClipboardEvent, view: EditorView) => {
      const sel = view.state.selection.main;
      if (sel.empty || isCursorInFencedCode(view.state)) return null;
      const selectedMd = view.state.sliceDoc(sel.from, sel.to);
      const dt = event.clipboardData;
      if (!selectedMd || !dt) return null;
      event.preventDefault();
      dt.setData("text/plain", selectedMd);
      dt.setData("text/html", markdownSelectionToClipboardHTML(selectedMd));
      return sel;
    };

    const domHandlers = EditorView.domEventHandlers({
      ...buildCMImeHandlers(composingRef, valueRef, onChangeRef, propSyncRef.current),
      copy(event, view) {
        return Boolean(writeMdSelectionClipboard(event, view));
      },
      cut(event, view) {
        const sel = writeMdSelectionClipboard(event, view);
        if (!sel) return false;
        if (!view.state.readOnly) {
          view.dispatch({
            changes: { from: sel.from, to: sel.to, insert: "" },
            selection: { anchor: sel.from },
            userEvent: "delete.cut",
          });
        }
        return true;
      },
      paste(event, view) {
        return handleRichPaste(
          event,
          () => isCursorInFencedCode(view.state),
          (md, forceBlock) => {
            const sel = view.state.selection.main;
            let toInsert = md;
            if (forceBlock || toInsert.includes("\n")) {
              const charBefore = sel.from > 0 ? view.state.sliceDoc(sel.from - 1, sel.from) : "\n";
              const charAfter = sel.to < view.state.doc.length ? view.state.sliceDoc(sel.to, sel.to + 1) : "\n";
              if (charBefore !== "\n" && (forceBlock || !toInsert.startsWith("\n"))) toInsert = "\n\n" + toInsert;
              if (charAfter !== "\n" && (forceBlock || !toInsert.endsWith("\n"))) toInsert = toInsert + "\n\n";
            }
            view.dispatch({
              changes: { from: sel.from, to: sel.to, insert: toInsert },
              selection: { anchor: sel.from + toInsert.length },
              userEvent: "input.paste",
            });
          }
        );
      },
      drop(event, view) {
        const files = event.dataTransfer?.files;
        if (files && files.length > 0) {
          event.preventDefault();
          const dropPos = view.posAtCoords({ x: event.clientX, y: event.clientY });
          if (dropPos !== null) {
            view.dispatch({ selection: { anchor: dropPos } });
          }
          void uploadMediaFiles(Array.from(files), true);
          return true;
        }
        return false;
      },
    });

    const slashKeymap = Prec.highest(
      keymap.of([
        {
          key: "ArrowDown",
          run: () => {
            if (!slashMenuRef.current || filteredSlashItemsRef.current.length === 0) return false;
            const len = filteredSlashItemsRef.current.length;
            setSlashMenu((prev) => (prev ? { ...prev, idx: (prev.idx + 1) % len } : null));
            return true;
          },
        },
        {
          key: "ArrowUp",
          run: () => {
            if (!slashMenuRef.current || filteredSlashItemsRef.current.length === 0) return false;
            const len = filteredSlashItemsRef.current.length;
            setSlashMenu((prev) => (prev ? { ...prev, idx: (prev.idx - 1 + len) % len } : null));
            return true;
          },
        },
        {
          key: "Enter",
          run: () => {
            if (!slashMenuRef.current || filteredSlashItemsRef.current.length === 0) return false;
            return execSlashItemRef.current();
          },
        },
        {
          key: "Tab",
          run: () => {
            if (!slashMenuRef.current || filteredSlashItemsRef.current.length === 0) return false;
            return execSlashItemRef.current();
          },
        },
        {
          key: "Escape",
          run: () => {
            if (slashMenuRef.current) {
              setSlashMenu(null);
              return true;
            }
            if (findOpenRef.current) {
              setFindOpen(false);
              return true;
            }
            return false;
          },
        },
      ])
    );

    const cmd = (key: string, fn: () => void) => ({
      key,
      run: () => {
        fn();
        return true;
      },
    });
    const wrapKeys: Array<[string, string, string, string]> = [
      ["Mod-b", "**", "**", "bold"],
      ["Mod-i", "*", "*", "italic"],
      ["Mod-u", "<u>", "</u>", "text"],
      ["Mod-Shift-s", "~~", "~~", "text"],
      ["Mod-e", "`", "`", "code"],
      ["Mod-Shift-h", "==", "==", "text"],
    ];
    const shortcutKeymap = keymap.of([
      cmd("Mod-s", () => (onSaveRef.current ? onSaveRef.current() : window.dispatchEvent(new CustomEvent("volo:save")))),
      cmd("Mod-f", () => openFindBarRef.current(false)),
      cmd("Mod-h", () => openFindBarRef.current(true)),
      { key: "Mod-/", run: (v) => toggleEditorComment(v, false) },
      cmd("Mod-Alt-c", () => insertCodeBlockRef.current("bash")),
      ...wrapKeys.map(([key, b, a, fb]) => cmd(key, () => wrapSelection(b, a, fb))),
      cmd("Mod-Shift-m", () => insertSnippet("\n$$\nE = mc^2\n$$\n")),
      cmd("Mod-k", () => wrapSelection("[", "](https://)", t("ed.link_text_ph"))),
      cmd("Mod-Shift-f", () => setFullscreen((f) => !f)),
    ]);

    const state = EditorState.create({
      doc: value,
      extensions: [
        modeCompartment.current.of(modeExtensions(mode)),
        cmSearchHighlightField,
        slashKeymap,
        history(),
        Prec.high(keymap.of(markdownEditingKeymap)),
        buildSmartTypingExt(false),
        pasteURLAsLinkExt,
        EditorView.lineWrapping,
        phCompartment.current.of(placeholder(t("ed.placeholder"))),
        shortcutKeymap,
        keymap.of([...voloBaseKeymap, ...historyKeymap]),
        domHandlers,
        EditorView.updateListener.of((update) => {
          if (update.docChanged && !isPropSyncUpdate(update) && !update.view.composing && !composingRef.current) {
            const docStr = update.state.doc.toString();
            recordEditorEmit(propSyncRef.current, docStr);
            onChangeRef.current(docStr);
          }
          if (
            !update.view.composing &&
            !composingRef.current &&
            modeRef.current !== "wysiwyg" &&
            (update.docChanged || (slashMenuRef.current !== null && update.selectionSet))
          ) {
            const sel = update.state.selection.main;
            if (!sel.empty) {
              if (slashMenuRef.current) setSlashMenu(null);
              return;
            }
            const headPos = sel.head;
            const line = update.state.doc.lineAt(headPos);
            if (isInsideFencedCode(update.state, line.number) || isInsideMathBlock(update.state, line.number)) {
              if (slashMenuRef.current) setSlashMenu(null);
              return;
            }
            const before = line.text.slice(0, headPos - line.from);
            const after = line.text.slice(headPos - line.from);
            const sm = after.trim() === "" ? /^(\s*(?:>\s*)?)\/([a-zA-Z0-9_-]{0,16})$/.exec(before) : null;
            if (!sm) {
              if (slashMenuRef.current) setSlashMenu(null);
              return;
            }
            const slashFrom = line.from + sm[1].length;
            const query = sm[2];
            const viewInstance = update.view;
            requestAnimationFrame(() => {
              if (!viewInstance.dom.isConnected || viewInstance.state.selection.main.head !== headPos) return;
              const coords = viewInstance.coordsAtPos(headPos);
              if (!coords) return;
              const { x, y } = computeSlashMenuXYRef.current(coords, query);
              setSlashMenu((prev) => ({
                query,
                from: slashFrom,
                to: headPos,
                x,
                y,
                idx: prev && prev.from === slashFrom ? prev.idx : 0,
              }));
            });
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

  const handleWysiwygLangChange = () => {
    const root = wysiwygRef.current;
    if (!root) return;
    markWysiwygDirty(false);
    commitWysiwygEdit(root, false);
  };

  const enhanceDom = (el: HTMLElement, isWysiwyg: boolean) =>
    enhanceRenderedPreviewDOM(el, {
      isWysiwyg,
      copyLabel: t("ed.copy_code"),
      copiedLabel: t("ed.copied"),
      plainLabel: t("ed.code_plain"),
      ...(isWysiwyg ? { onLangChange: handleWysiwygLangChange } : {}),
    });

  useEffect(() => {
    const view = viewRef.current;
    if (view) {
      view.dispatch({
        effects: modeCompartment.current.reconfigure(modeExtensions(mode)),
      });
    }
    if (mode === "wysiwyg" && wysiwygRef.current) {
      wysiwygDirtyRef.current = false;
      const curKey = `${curLocale}:${hasAudio ? 1 : 0}:${valueRef.current}`;
      const cacheMatches = previewSourceRef.current === curKey && Boolean(previewHTML.trim());
      if (cacheMatches) {
        wysiwygForceSyncRef.current = false;
        wysiwygAwaitingInitialSyncRef.current = false;
        setWysiwygHydrating(false);
        if (!wysiwygRef.current.innerHTML.trim()) {
          wysiwygRef.current.innerHTML = previewHTML || `<p><br></p>`;
        }
        normalizeWysiwygDOM(wysiwygRef.current, t("common.delete"));
        enhanceDom(wysiwygRef.current, true);
        pushWysiwygHistory(wysiwygRef.current, true, true);
      } else {
        wysiwygForceSyncRef.current = true;
        wysiwygAwaitingInitialSyncRef.current = true;
        wysiwygRef.current.innerHTML = `<p><br></p>`;
        wysiwygHistoryRef.current = { stack: [], idx: -1 };
        setWysiwygHydrating(true);
      }
    } else {
      wysiwygAwaitingInitialSyncRef.current = false;
      setWysiwygHydrating(false);
    }
  }, [mode]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    view.dispatch({
      effects: phCompartment.current.reconfigure(placeholder(t("ed.placeholder"))),
    });
  }, [curLocale]);

  useEffect(() => {
    syncCMViewValue(viewRef.current, composingRef.current, propSyncRef.current, value);
  }, [value]);

  // Fetch server-rendered HTML for Split Preview (`sv`) and Visual (`wysiwyg`) modes
  useEffect(() => {
    if (mode === "ir") return;
    const reqKey = mode === "wysiwyg" ? `${curLocale}:${hasAudio ? 1 : 0}:${value}` : null;
    if (
      mode === "wysiwyg" &&
      previewSourceRef.current === reqKey &&
      !wysiwygForceSyncRef.current &&
      Boolean(previewHTML.trim())
    ) {
      return;
    }
    if (
      mode === "wysiwyg" &&
      wysiwygRef.current &&
      !wysiwygForceSyncRef.current &&
      Boolean(wysiwygRef.current.contains(document.activeElement))
    ) {
      return;
    }
    const ctrl = new AbortController();
    const delay = wysiwygAwaitingInitialSyncRef.current ? 0 : value.length > 20000 ? 450 : 180;
    const timer = window.setTimeout(async () => {
      const reqSeq = wysiwygEditSeqRef.current;
      try {
        const html = await fetchPreviewHTML(value, curLocale, mode === "wysiwyg", Boolean(hasAudio), ctrl.signal);
        if (ctrl.signal.aborted) return;
        if (mode === "wysiwyg" && reqSeq !== wysiwygEditSeqRef.current) {
          return;
        }
        previewSourceRef.current = reqKey;
        setPreviewHTML(html);
        if (
          mode === "wysiwyg" &&
          wysiwygRef.current &&
          !wysiwygComposingRef.current &&
          reqSeq === wysiwygEditSeqRef.current &&
          (wysiwygForceSyncRef.current || !wysiwygRef.current.contains(document.activeElement))
        ) {
          const isInitialHydrate = wysiwygAwaitingInitialSyncRef.current;
          wysiwygAwaitingInitialSyncRef.current = false;
          wysiwygForceSyncRef.current = false;
          wysiwygDirtyRef.current = false;
          wysiwygRef.current.innerHTML = html || `<p><br></p>`;
          normalizeWysiwygDOM(wysiwygRef.current, t("common.delete"));
          enhanceDom(wysiwygRef.current, true);
          pushWysiwygHistory(wysiwygRef.current, true, isInitialHydrate);
          setWysiwygHydrating(false);
        }
      } catch {
        if (!ctrl.signal.aborted && mode === "wysiwyg") {
          wysiwygAwaitingInitialSyncRef.current = false;
          wysiwygForceSyncRef.current = false;
          setWysiwygHydrating(false);
        }
      }
    }, delay);
    return () => {
      window.clearTimeout(timer);
      ctrl.abort();
    };
  }, [value, mode, curLocale, hasAudio]);

  // Enhance Split Preview (`sv`) DOM with KaTeX math, Mermaid diagrams, and code copy buttons
  useEffect(() => {
    if (mode !== "sv" || !previewRef.current) return;
    enhanceDom(previewRef.current, false);
  }, [mode, previewHTML, curLocale]);

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

    let anchorsCache: ScrollAnchor[] | null = null;
    let cachedDocRef: EditorState["doc"] | null = null;
    let cachedCmH = -1;
    let cachedPvH = -1;

    const normHeading = (s: string): string =>
      s
        .replace(/!\[([^[\]]*)\]\(([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)/g, "$1")
        .replace(/\[([^[\]]+)\]\(([^()\n]*(?:\([^()\n]*\)[^()\n]*)*)\)/g, "$1")
        .replace(/\[\[([^[\]|]+)(?:\|([^[\]]+))?\]\]/g, (_, a, b) => b || a)
        .replace(/<[^<>]+>/g, " ")
        .replace(/[`*_~=^#<>|\\]/g, " ")
        .replace(/\s+/g, " ")
        .trim()
        .toLowerCase();

    const computeAnchors = (): ScrollAnchor[] | null => {
      const cmMax = Math.max(0, cmScroller.scrollHeight - cmScroller.clientHeight);
      const pvMax = Math.max(0, pvPane.scrollHeight - pvPane.clientHeight);
      if (cmMax <= 0 && pvMax <= 0) return null;
      if (
        anchorsCache &&
        cachedDocRef === view.state.doc &&
        cachedCmH === cmScroller.scrollHeight &&
        cachedPvH === pvPane.scrollHeight
      ) {
        return anchorsCache;
      }

      const anchors: ScrollAnchor[] = [{ cm: 0, pv: 0 }];
      const mdHeads: Array<{ offset: number; level: number; norm: string }> = [];
      let fenceChar = "";
      let fenceLen = 0;
      let fenceQDepth = 0;
      for (let ln = 1; ln <= view.state.doc.lines; ln++) {
        const lineObj = view.state.doc.line(ln);
        if (!fenceChar) {
          const open = matchFenceOpen(lineObj.text);
          if (open) {
            fenceChar = open.fenceChar;
            fenceLen = open.fenceLen;
            fenceQDepth = open.quoteDepth;
            continue;
          }
        } else if (matchFenceClose(lineObj.text, fenceChar, fenceLen, fenceQDepth)) {
          fenceChar = "";
          fenceLen = 0;
          fenceQDepth = 0;
          continue;
        }
        if (!fenceChar) {
          const hm = /^ {0,3}(#{1,6})[ \t]+(\S[^\n\r\u2028\u2029]*)$/.exec(lineObj.text);
          if (hm) {
            const rawHeading = trimEndChars(hm[2].trimEnd(), "#").trimEnd();
            mdHeads.push({ offset: lineObj.from, level: hm[1].length, norm: normHeading(rawHeading) });
          }
        }
      }

      const pvHeads = Array.from(pvPane.querySelectorAll<HTMLElement>("h1, h2, h3, h4, h5, h6")).filter(
        (el) => !el.closest(".volo-callout, .volo-embed-book, .volo-bookshelf, blockquote, details, td, th")
      );

      if (mdHeads.length > 0 && pvHeads.length > 0 && cmMax > 0 && pvMax > 0) {
        const pvMeta = pvHeads.map((el) => ({
          el,
          level: Number(el.tagName.slice(1)) || 1,
          norm: normHeading(el.textContent || ""),
        }));
        const paneTop = pvPane.getBoundingClientRect().top;
        let pvCursor = 0;
        for (let i = 0; i < mdHeads.length && pvCursor < pvMeta.length; i++) {
          const mh = mdHeads[i];
          let matchedIdx = -1;
          const lookLimit = Math.min(pvMeta.length, pvCursor + 8);
          for (let j = pvCursor; j < lookLimit; j++) {
            const pm = pvMeta[j];
            if (pm.level !== mh.level) continue;
            if (
              pm.norm === mh.norm ||
              (mh.norm.length >= 2 && (pm.norm.includes(mh.norm) || mh.norm.includes(pm.norm)))
            ) {
              matchedIdx = j;
              break;
            }
          }
          if (matchedIdx < 0 && mdHeads.length === pvMeta.length && pvMeta[i]?.level === mh.level) {
            matchedIdx = i;
          }
          if (matchedIdx < 0) continue;
          pvCursor = matchedIdx + 1;
          const cmY = Math.min(cmMax, Math.max(0, view.lineBlockAt(mh.offset).top - 8));
          const pvY = Math.min(
            pvMax,
            Math.max(0, pvMeta[matchedIdx].el.getBoundingClientRect().top - paneTop + pvPane.scrollTop - 8)
          );
          const prev = anchors[anchors.length - 1];
          if (cmY >= prev.cm && pvY >= prev.pv && (cmY > prev.cm || pvY > prev.pv)) {
            anchors.push({ cm: cmY, pv: pvY });
          }
        }
      }

      const last = anchors[anchors.length - 1];
      anchors.push({ cm: Math.max(last.cm, cmMax), pv: Math.max(last.pv, pvMax) });
      anchorsCache = anchors;
      cachedDocRef = view.state.doc;
      cachedCmH = cmScroller.scrollHeight;
      cachedPvH = pvPane.scrollHeight;
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

    const invalidateAnchors = () => {
      anchorsCache = null;
      if (syncScrollRef.current && scrollDriverRef.current.source !== "pv") {
        syncCmToPv();
      }
    };

    let ro: ResizeObserver | null = null;
    if (typeof ResizeObserver !== "undefined") {
      ro = new ResizeObserver(invalidateAnchors);
      ro.observe(pvPane);
      ro.observe(cmScroller);
      for (const ch of Array.from(pvPane.children)) ro.observe(ch);
    }
    let mo: MutationObserver | null = null;
    if (typeof MutationObserver !== "undefined") {
      mo = new MutationObserver(() => {
        anchorsCache = null;
      });
      mo.observe(pvPane, { childList: true, subtree: true });
    }
    pvPane.addEventListener("load", invalidateAnchors, true);

    cmScroller.addEventListener("scroll", syncCmToPv, { passive: true });
    pvPane.addEventListener("scroll", syncPvToCm, { passive: true });
    const rafId = window.requestAnimationFrame(syncCmToPv);

    return () => {
      window.cancelAnimationFrame(rafId);
      ro?.disconnect();
      mo?.disconnect();
      pvPane.removeEventListener("load", invalidateAnchors, true);
      cmScroller.removeEventListener("scroll", syncCmToPv);
      pvPane.removeEventListener("scroll", syncPvToCm);
    };
  }, [mode, previewHTML, syncScroll]);

  useEffect(() => {
    const onGlobalShortcut = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.shiftKey && (e.key === "f" || e.key === "F")) {
        const root = editorRootRef.current;
        if (!root) return;
        if (fullscreen || root.contains(document.activeElement)) {
          e.preventDefault();
          setFullscreen((f) => !f);
        }
      }
    };
    window.addEventListener("keydown", onGlobalShortcut);
    return () => window.removeEventListener("keydown", onGlobalShortcut);
  }, [fullscreen]);

  // visualViewport tracking (rAF-throttled, always on): exposes --volo-vv-h / --volo-vv-top so editor heights
  // can be clamped to the visible area (and fullscreen pinned to it), and derives `kbOpen` — soft keyboard up
  // while an editable inside the editor is focused — so secondary chrome can fold away on mobile.
  useEffect(() => {
    const vv = window.visualViewport;
    const coarse = window.matchMedia?.("(pointer: coarse)");
    let raf = 0;
    let lastH = -1;
    let lastTop = -1;
    let baseW = -1;
    let baseH = 0;
    let kb = false;
    let pending: boolean | null = null;
    const isEditable = (n: Element | null): boolean =>
      !!n &&
      (n instanceof HTMLTextAreaElement ||
        (n instanceof HTMLInputElement &&
          !/^(?:button|checkbox|radio|range|color|file|submit|reset|image)$/i.test(n.type)) ||
        (n as HTMLElement).isContentEditable);
    const sync = () => {
      raf = 0;
      const el = editorRootRef.current;
      if (!el) return;
      const ih = window.innerHeight;
      const h = Math.round(vv ? vv.height : ih);
      const top = vv ? Math.max(0, Math.round(vv.offsetTop)) : 0;
      if (h !== lastH) {
        el.style.setProperty("--volo-vv-h", `${h}px`);
        lastH = h;
      }
      if (top !== lastTop) {
        el.style.setProperty("--volo-vv-top", `${top}px`);
        lastTop = top;
      }
      // Baseline = tallest visual viewport seen at the current width (reset on rotation). Covers browsers that
      // shrink the layout viewport with the keyboard, where innerHeight - vv.height stays ~0.
      if (window.innerWidth !== baseW) {
        baseW = window.innerWidth;
        baseH = 0;
      }
      baseH = Math.max(baseH, h);
      const kbPx = Math.max(ih - h - top, baseH - h);
      const ae = document.activeElement;
      const next =
        !!vv &&
        !!coarse?.matches &&
        (vv.scale || 1) < 1.05 &&
        kbPx > 120 &&
        !!ae &&
        el.contains(ae) &&
        isEditable(ae);
      // Commit only once the value is stable for two frames (iOS animates keyboard / IME candidate bar).
      if (next === kb) {
        pending = null;
      } else if (pending === next) {
        pending = null;
        kb = next;
        setKbOpen(next);
      } else {
        pending = next;
        raf = window.requestAnimationFrame(sync);
      }
    };
    const schedule = () => {
      if (!raf) raf = window.requestAnimationFrame(sync);
    };
    sync();
    window.addEventListener("resize", schedule);
    vv?.addEventListener("resize", schedule);
    vv?.addEventListener("scroll", schedule);
    document.addEventListener("focusin", schedule);
    document.addEventListener("focusout", schedule);
    return () => {
      if (raf) window.cancelAnimationFrame(raf);
      window.removeEventListener("resize", schedule);
      vv?.removeEventListener("resize", schedule);
      vv?.removeEventListener("scroll", schedule);
      document.removeEventListener("focusin", schedule);
      document.removeEventListener("focusout", schedule);
      editorRootRef.current?.style.removeProperty("--volo-vv-h");
      editorRootRef.current?.style.removeProperty("--volo-vv-top");
    };
  }, []);

  // Keyboard just opened: the editor height is clamped to the visual viewport, keep the caret in view.
  useEffect(() => {
    if (!kbOpen) return;
    const raf = window.requestAnimationFrame(() => {
      const view = viewRef.current;
      if (!view || !view.hasFocus) return;
      view.dispatch({ effects: EditorView.scrollIntoView(view.state.selection.main.head, { y: "nearest" }) });
    });
    return () => window.cancelAnimationFrame(raf);
  }, [kbOpen]);

  useEffect(() => {
    if (!fullscreen) return;
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";

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
    return () => {
      document.body.style.overflow = prevOverflow;
      document.removeEventListener("keydown", onKey);
    };
  }, [fullscreen]);

  // After a toolbar fullscreen toggle: put focus / caret back where it was (the relayout can drop focus on
  // mobile, which would also collapse the soft keyboard) and keep the caret visible in the new geometry.
  useEffect(() => {
    const snap = fsFocusRef.current;
    fsFocusRef.current = null;
    if (!snap) return;
    const raf = window.requestAnimationFrame(() => {
      if (snap.kind === "cm") {
        const view = viewRef.current;
        if (!view) return;
        if (!view.hasFocus) view.focus();
        view.dispatch({ effects: EditorView.scrollIntoView(view.state.selection.main.head, { y: "nearest" }) });
        return;
      }
      const surf = wysiwygRef.current;
      if (!surf || surf.contains(document.activeElement)) return;
      try {
        surf.focus({ preventScroll: true });
      } catch {
        surf.focus();
      }
      const sel = window.getSelection();
      if (snap.range && sel && surf.contains(snap.range.startContainer)) {
        sel.removeAllRanges();
        sel.addRange(snap.range);
      }
    });
    return () => window.cancelAnimationFrame(raf);
  }, [fullscreen]);

  const toggleFullscreen = () => {
    const ae = document.activeElement;
    const view = viewRef.current;
    const surf = wysiwygRef.current;
    if (view && (view.hasFocus || (ae && view.dom.contains(ae)))) {
      fsFocusRef.current = { kind: "cm", range: null };
    } else if (surf && ae && surf.contains(ae)) {
      const sel = window.getSelection();
      fsFocusRef.current = {
        kind: "wysiwyg",
        range: sel && sel.rangeCount > 0 ? sel.getRangeAt(0).cloneRange() : null,
      };
    } else {
      fsFocusRef.current = null;
    }
    setFullscreen((v) => !v);
  };

  // Toolbar buttons must not steal focus from the editor: a blur collapses the mobile keyboard (and loses the
  // WYSIWYG selection) only for it to re-open right after. Cancelling mousedown keeps focus; click still fires.
  const handleToolbarMouseDown = (e: MouseEvent) => {
    if (e.button !== 0) return;
    const target = e.target as HTMLElement | null;
    if (!target || target.closest("input, select, textarea, [contenteditable]")) return;
    if (target.closest("button")) e.preventDefault();
  };

  // Unified portal popover positioning for Heading / Color / Callout / Media / Bookshelf / Math / Export menus
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
      const dims = POP_DIMS[activePop] || { minW: 184, maxW: 260, maxH: 200, estH: 120 };
      const estH = popMenuRef.current?.offsetHeight || dims.estH;
      setPopStyle(
        computePortalMenuStyle(rect, {
          ...dims,
          estH,
          align: activePop === "export" ? "right" : "left",
        }).style
      );
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
    if (!qiOpen) return;
    if (qiTab !== "media" && qiTab !== "books" && qiTab !== "cite") return;
    const isMedia = qiTab === "media";
    const isBooks = qiTab === "books";
    const per = isBooks ? "10" : isMedia ? "18" : "20";
    const page = isMedia ? qiMediaPage : isBooks ? qiBooksPage : qiArtPage;
    const q = (isMedia ? debouncedQiMediaQ : isBooks ? debouncedQiBookQ : debouncedQiArtQ).trim();
    const params = new URLSearchParams({ page: String(page), per_page: per, page_size: per });
    if (isMedia) params.set("refs", "0");
    if (q) params.set("q", q);
    if (isMedia && qiMediaMime) params.set("mime", qiMediaMime);
    const ep = isMedia ? "media" : isBooks ? "books" : "articles";
    apiFetch<any>(`/api/admin/${ep}?${params.toString()}`)
      .then((r) => {
        const rawList = r.items || r[ep] || [];
        const list = qiTab === "cite" ? rawList.map((it: any) => it.article || it) : rawList;
        const total = typeof r.total === "number" ? r.total : list.length;
        if (isMedia) {
          setQiMedia(list);
          setQiMediaTotal(total);
        } else if (isBooks) {
          setQiBooks(list);
          setQiBooksTotal(total);
        } else {
          setQiArticles(list);
          setQiArtTotal(total);
        }
      })
      .catch(() => {});
  }, [qiOpen, qiTab, qiMediaPage, debouncedQiMediaQ, qiMediaMime, qiBooksPage, debouncedQiBookQ, qiArtPage, debouncedQiArtQ]);

  const jumpToLine = (lineNo: number) => {
    setMobilePane("write");
    if (mode === "wysiwyg") {
      const root = wysiwygRef.current;
      if (!root) return;
      const targetIdx = outline.findIndex((it) => it.line === lineNo);
      const headings = Array.from(root.querySelectorAll<HTMLElement>("h1, h2, h3, h4"));
      const target =
        (targetIdx >= 0 ? headings[targetIdx] : undefined) ||
        (() => {
          const item = outline.find((it) => it.line === lineNo);
          if (!item) return undefined;
          const want = item.text.replace(/\s+/g, " ").trim().toLowerCase();
          return headings.find((h) => (h.textContent || "").replace(/\s+/g, " ").trim().toLowerCase() === want);
        })();
      if (target) {
        target.scrollIntoView({ behavior: "smooth", block: "start" });
        placeCaretInBlock(target, true);
        root.focus();
      }
      return;
    }
    const view = viewRef.current;
    if (!view) return;
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
  const edIco = (d: string, sw?: number) => (
    <SvgIcon sw={sw}>
      <path d={d} />
    </SvgIcon>
  );
  const insertMediaAssetSnippet = (asset: {
    id: string;
    original_name?: string;
    orig_filename?: string;
    mime_type?: string;
    public_url: string;
  }) => {
    const name = asset.orig_filename || asset.original_name || asset.id;
    const mime = String(asset.mime_type || "");
    const isImg = mime.startsWith("image/");
    const safeMedia = safeMediaSrc(asset.public_url, { allowDataImage: isImg });
    const safeLink = safeHref(asset.public_url);
    if (isImg) {
      if (safeMedia) insertSnippet(`![${name}](${safeMedia})`);
    } else if (isVideoMedia(mime, asset.public_url)) {
      if (safeMedia) insertSnippet(`\n<video controls preload="metadata" src="${safeMedia}"></video>\n`);
    } else if (isAudioMedia(mime, asset.public_url)) {
      if (safeMedia) insertSnippet(`\n<audio controls preload="metadata" src="${safeMedia}"></audio>\n`);
    } else if (safeLink) {
      insertSnippet(`[${name}](${safeLink})`);
    }
  };
  const edBtn = (
    tip: string,
    onClick: () => void,
    children: preact.ComponentChildren,
    extraCls = "",
    btnRef?: (el: HTMLButtonElement | null) => void
  ) => (
    <button
      ref={btnRef}
      type="button"
      class={`volo-ed-btn${extraCls ? " " + extraCls : ""}`}
      data-tip={tip}
      aria-label={tip}
      onClick={onClick}
    >
      {children}
    </button>
  );
  const edPopBtn = (id: Exclude<ToolbarPopKind, "">, tip: string, children: preact.ComponentChildren) => (
    <div class="volo-pop-wrap">
      {edBtn(tip, () => togglePop(id), children, activePop === id ? "active" : "", (el) => {
        popBtnRefs.current[id] = el;
      })}
    </div>
  );

  return (
    <div
      ref={editorRootRef}
      class={`volo-studio-editor ${fullscreen ? "is-fullscreen" : ""} ${kbOpen ? "is-kb-open" : ""}`}
    >
      {/* Top Sticky Rich Icon Toolbar */}
      <div
        class="volo-ed-toolbar"
        onPointerOver={handleToolbarPointerOver as any}
        onPointerLeave={() => setEdTip(null)}
        onPointerDown={() => setEdTip(null)}
        onMouseDown={handleToolbarMouseDown as any}
      >
        <div class="volo-ed-tools">
          {/* Group 1: Undo / Redo */}
          {edBtn(
            t("ed.undo"),
            () => {
              if (mode === "wysiwyg") applyWysiwygHistoryStep(-1);
              else if (viewRef.current) undo(viewRef.current);
            },
            edIco("M9 14 4 9l5-5M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5v0a5.5 5.5 0 0 1-5.5 5.5H11")
          )}
          {edBtn(
            t("ed.redo"),
            () => {
              if (mode === "wysiwyg") applyWysiwygHistoryStep(1);
              else if (viewRef.current) redo(viewRef.current);
            },
            edIco("m15 14 5-5-5-5M20 9H9.5A5.5 5.5 0 0 0 4 14.5v0A5.5 5.5 0 0 0 9.5 20H13")
          )}

          <span class="volo-ed-sep" />

          {/* Group 2: Heading, Typography & Color/Highlight */}
          {edPopBtn("heading", t("ed.heading"), <span style={{ fontWeight: 750, fontSize: "12.5px" }}>H▾</span>)}

          {edBtn(
            t("ed.bold"),
            () => wrapSelection("**", "**", "bold"),
            edIco("M6 4h8a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6zM6 12h9a4 4 0 0 1 4 4 4 4 0 0 1-4 4H6z", 2.1)
          )}
          {edBtn(
            t("ed.italic"),
            () => wrapSelection("*", "*", "italic"),
            edIco("M19 4H10M14 20H5M15 4 9 20", 2)
          )}
          {edBtn(
            t("ed.strike"),
            () => wrapSelection("~~", "~~", "text"),
            edIco("M16 4H9a3 3 0 0 0-2.83 4M14 12a4 4 0 0 1 0 8H6M4 12h16")
          )}

          {/* Text Color, Highlight (<mark>) & Underline (<u>) Popover */}
          {edPopBtn(
            "color",
            t("ed.text_color"),
            <span class="volo-ed-color-glyph">
              <span>A</span>
              <i />
            </span>
          )}

          {edBtn(t("ed.code"), () => wrapSelection("`", "`", "code"), edIco("M16 18 22 12 16 6M8 6 2 12 8 18"))}

          <span class="volo-ed-sep" />

          {/* Group 3: Quote, Callout, Bullet / Ordered / Task Lists */}
          {edBtn(
            t("ed.quote"),
            () => prefixCurrentLine("> "),
            edIco(
              "M3 21c3 0 7-1 7-8V5c0-1.25-.756-2.017-2-2H4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2 1 0 1 0 1 1v1c0 1-1 2-2 2s-1 .008-1 1.031V20c0 1 0 1 1 1zM15 21c3 0 7-1 7-8V5c0-1.25-.757-2.017-2-2h-4c-1.25 0-2 .75-2 1.972V11c0 1.25.75 2 2 2h.75c0 2.25.25 4-2.75 4v3c0 1 0 1 1 1z"
            )
          )}

          {edPopBtn(
            "callout",
            t("ed.callout"),
            <SvgIcon>
              <circle cx="12" cy="12" r="9" />
              <path d="M12 8v4M12 16h.01" />
            </SvgIcon>
          )}

          {edBtn(
            t("ed.ul"),
            () => prefixCurrentLine("- "),
            <SvgIcon>
              <path d="M8 6h13M8 12h13M8 18h13" />
              <circle cx="4" cy="6" r="1" fill="currentColor" />
              <circle cx="4" cy="12" r="1" fill="currentColor" />
              <circle cx="4" cy="18" r="1" fill="currentColor" />
            </SvgIcon>
          )}
          {edBtn(
            t("ed.ol"),
            () => prefixCurrentLine("1. "),
            edIco("M10 6h11M10 12h11M10 18h11M4 6h1v4M4 10h2M6 18H4c0-1 2-2 2-3s-1-1.5-2-1")
          )}
          {edBtn(
            t("ed.task"),
            () => prefixCurrentLine("- [ ] "),
            <SvgIcon>
              <rect x="3" y="5" width="6" height="6" rx="1.2" />
              <path d="m3 17 2 2 4-4M13 8h8M13 17h8" />
            </SvgIcon>
          )}

          <span class="volo-ed-sep" />

          {/* Group 4: Link, Media, Table, Code Block, Divider */}
          {edBtn(
            t("ed.link"),
            () => wrapSelection("[", "](https://)", t("ed.link_text_ph")),
            edIco(
              "M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"
            )
          )}

          {edPopBtn(
            "media",
            uploading ? t("ed.uploading") : t("ed.upload_media"),
            <SvgIcon>
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <circle cx="8.5" cy="8.5" r="1.5" />
              <path d="M21 15 16 10 5 21" />
            </SvgIcon>
          )}
          <input
            ref={fileInputRef}
            type="file"
            multiple
            accept="image/*,audio/*,video/*"
            style={{ display: "none" }}
            onChange={(e) => {
              const files = Array.from((e.currentTarget as HTMLInputElement).files || []);
              (e.currentTarget as HTMLInputElement).value = "";
              void uploadMediaFiles(files, true);
            }}
          />

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
              <SvgIcon>
                <rect x="9" y="2" width="6" height="12" rx="3" />
                <path d="M5 10a7 7 0 0 0 14 0M12 17v4M8 21h8" />
              </SvgIcon>
            )}
          </button>

          {edBtn(
            t("ed.table"),
            () => insertSnippet(`\n${t("ed.table_tpl")}\n`),
            <SvgIcon>
              <rect x="3" y="3" width="18" height="18" rx="2" />
              <path d="M3 9h18M3 15h18M9 3v18M15 3v18" />
            </SvgIcon>
          )}
          {edPopBtn(
            "codeblock",
            t("ed.code_block"),
            <SvgIcon>
              <rect x="3" y="4" width="18" height="16" rx="2" />
              <path d="M8 10 11 12.5 8 15M13 15h3" />
            </SvgIcon>
          )}

          {edPopBtn(
            "math",
            t("ed.math_mermaid"),
            <span style={{ fontWeight: 720, fontSize: "13px", fontFamily: "serif" }}>∑</span>
          )}

          {edBtn(t("ed.hr"), () => insertSnippet("\n---\n"), edIco("M4 12h16"))}

          <span class="volo-ed-sep" />

          {/* Group 5: Volo Extension Shortcuts (Reader Lock, Password Lock, Embedded Bookshelf, Quick Insert Hub) */}
          {edBtn(
            t("ed.sc_lock_user"),
            () => {
              const sel = getSelectedText().trim();
              if (sel) {
                wrapSelection(`\n:::lock type="user"\n`, `\n:::\n`, sel);
              } else {
                insertSnippet(`\n:::lock type="user"\n${t("ed.lock_user_ph")}\n:::\n`);
              }
            },
            <SvgIcon sw={1.85}>
              <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2M17 13v-1.5a1.5 1.5 0 0 1 3 0V13" />
              <circle cx="9" cy="7" r="4" />
              <rect x="15" y="13" width="7" height="6" rx="1.2" />
            </SvgIcon>
          )}

          {edBtn(
            t("ed.sc_lock_pwd"),
            () => {
              const sel = getSelectedText().trim();
              if (sel) setLockBody(sel);
              setLockMode("pwd");
              setQiTab("lock");
              setQiOpen(true);
            },
            <SvgIcon sw={1.85}>
              <rect x="5" y="11" width="14" height="10" rx="2" />
              <path d="M8 11V7a4 4 0 0 1 8 0v4" />
              <circle cx="12" cy="16" r="1" fill="currentColor" />
            </SvgIcon>
          )}

          {edPopBtn(
            "book",
            t("ed.sc_bookshelf"),
            edIco("M2 4h6a4 4 0 0 1 4 4v13a3 3 0 0 0-3-3H2zM22 4h-6a4 4 0 0 0-4 4v13a3 3 0 0 1 3-3h7z", 1.85)
          )}

          {/* Quick Insert + Modal Trigger */}
          {edBtn(
            t("ed.quick_insert"),
            () => setQiOpen(true),
            edIco("M12 5v14M5 12h14", 2.1),
            "volo-ed-btn-accent"
          )}

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

        {/* Right Controls: 3-Mode Switcher + Sync Scroll (in sv) + Find + Outline + Export + Fullscreen */}
        <div class="volo-ed-right">
          <div class="volo-ed-modes" role="tablist">
            {(["ir", "wysiwyg", "sv"] as const).map((mKey) => (
              <button
                key={mKey}
                type="button"
                role="tab"
                aria-selected={mode === mKey}
                class={`volo-ed-mode-btn ${mode === mKey ? "active" : ""}`}
                onClick={() => {
                  switchMode(mKey);
                  if (mKey !== "sv") setMobilePane("write");
                }}
              >
                {t(`ed.mode_${mKey}`)}
              </button>
            ))}
          </div>

          {mode === "sv" &&
            edBtn(
              t("ed.sync_scroll"),
              () =>
                setSyncScroll((prev) => {
                  const next = !prev;
                  localStorage.setItem("volo_editor_sync_scroll", next ? "1" : "0");
                  return next;
                }),
              edIco("m7 7 3-3 3 3M10 4v11m7 2-3 3-3-3m3 3V9"),
              syncScroll ? "active" : ""
            )}

          {edBtn(
            t("ed.find_replace"),
            () => (findOpen ? closeFindBar() : openFindBar(false)),
            edIco("M18 11a7 7 0 1 1-14 0 7 7 0 0 1 14 0Zm2 9-3.9-3.9"),
            findOpen ? "active" : ""
          )}

          {edBtn(
            t("ed.outline"),
            () =>
              setShowOutline((v) => {
                const next = !v;
                if (next) setMobilePane("outline");
                else if (mobilePane === "outline") setMobilePane("write");
                return next;
              }),
            edIco("M3 6h18M7 12h14M11 18h10"),
            showOutline ? "active" : ""
          )}

          {edPopBtn(
            "export",
            t("ed.export"),
            edIco("M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3")
          )}

          {fullscreen && onSave && (
            <button type="button" class="volo-btn volo-btn-primary volo-btn-sm" onClick={() => onSave()}>
              {t("common.save")}
            </button>
          )}

          {edBtn(
            fullscreen ? t("ed.exit_fullscreen") : t("ed.fullscreen"),
            toggleFullscreen,
            edIco(
              fullscreen
                ? "M8 3v3a2 2 0 0 1-2 2H3M21 8h-3a2 2 0 0 1-2-2V3M3 16h3a2 2 0 0 1 2 2v3M16 21v-3a2 2 0 0 1 2-2h3"
                : "M15 3h6v6M9 21H3v-6M21 3l-7 7M3 21l7-7"
            ),
            fullscreen ? "active" : ""
          )}
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
                  {EDITOR_SWATCHES.map((color) => (
                    <button
                      key={color}
                      type="button"
                      class="volo-color-swatch"
                      title={color.toUpperCase()}
                      style={{ background: color }}
                      onClick={() => {
                        applyColorStyle(color);
                        setActivePop("");
                      }}
                    />
                  ))}
                </div>
                {[
                  [t("ed.color_mark"), () => wrapSelection("==", "==", "text")],
                  [t("ed.underline"), () => wrapSelection("<u>", "</u>", "text")],
                  [t("ed.sup"), () => wrapSelection("^", "^", "2")],
                  [t("ed.sub"), () => wrapSelection("~", "~", "2")],
                  [t("ed.color_clear"), () => clearInlineStyle()],
                ].map(([lbl, fn], idx) => (
                  <button
                    key={idx}
                    type="button"
                    class="volo-cselect-item"
                    onClick={() => {
                      (fn as () => void)();
                      setActivePop("");
                    }}
                  >
                    <span class="volo-cselect-item-text">{lbl as string}</span>
                  </button>
                ))}
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

            {(activePop === "book" || activePop === "math" || activePop === "export") &&
              (activePop === "book"
                ? [
                    [t("ed.sc_bookshelf_all"), () => insertSnippet("\n:::bookshelf\n:::\n")],
                    [t("ed.sc_book_pick"), () => { setQiTab("books"); setQiOpen(true); }],
                  ]
                : activePop === "math"
                ? [
                    [t("ed.math_inline"), () => wrapSelection("$", "$", "E = mc^2")],
                    [t("ed.math_block"), () => insertSnippet("\n$$\n\\int_0^\\infty e^{-x^2}\\,dx = \\frac{\\sqrt{\\pi}}{2}\n$$\n")],
                    [t("ed.mermaid_diagram"), () => insertSnippet("\n```mermaid\nflowchart LR\n  A[Start] --> B[Done]\n```\n")],
                    [t("ed.insert_toc"), () => insertSnippet("\n[toc]\n")],
                  ]
                : [
                    [t("ed.export_md"), () => void exportEditorDocument("md")],
                    [t("ed.export_html"), () => void exportEditorDocument("html")],
                    [t("ed.copy_rich_html"), () => void exportEditorDocument("rich_copy")],
                  ]
              ).map(([lbl, fn], idx) => (
                <button
                  key={idx}
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    (fn as () => void)();
                  }}
                >
                  <span class="volo-cselect-item-text">{lbl as string}</span>
                </button>
              ))}

            {activePop === "codeblock" &&
              CODE_BLOCK_LANGS.map((item) => (
                <button
                  key={item.value || "plain"}
                  type="button"
                  class="volo-cselect-item"
                  onClick={() => {
                    setActivePop("");
                    insertCodeBlock(item.value);
                  }}
                >
                  <span class="volo-cselect-item-text">{item.value ? item.label : t("ed.code_plain")}</span>
                </button>
              ))}
          </div>,
          document.body
        )}

      {/* Slash `/` Block Command Menu Portal */}
      {slashMenu &&
        filteredSlashItems.length > 0 &&
        typeof document !== "undefined" &&
        createPortal(
          <div
            ref={slashPopupRef}
            class="volo-slash-menu"
            style={{ left: `${slashMenu.x}px`, top: `${slashMenu.y}px` }}
            role="listbox"
          >
            <div class="volo-slash-head">{t("ed.slash_hint")}</div>
            {filteredSlashItems.map((it, idx) => {
              const active = idx === Math.min(slashMenu.idx, filteredSlashItems.length - 1);
              return (
                <button
                  key={it.id}
                  type="button"
                  role="option"
                  aria-selected={active}
                  class={`volo-slash-item ${active ? "is-active" : ""}`}
                  onMouseEnter={() => setSlashMenu((prev) => (prev ? { ...prev, idx } : null))}
                  onMouseDown={(e) => {
                    e.preventDefault();
                    executeSlashItem(it);
                  }}
                >
                  <span>{it.label}</span>
                  <span class="volo-slash-kbd">{it.kbd}</span>
                </button>
              );
            })}
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
              onClick={() => {
                const active = document.activeElement as HTMLElement | null;
                if (active && editorRootRef.current?.contains(active)) active.blur();
                setMobilePane("preview");
              }}
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
        {/* Native Floating Find & Replace Bar */}
        {findOpen && mode !== "wysiwyg" && (
          <div class="volo-ed-findbar" role="search">
            <div class="volo-ed-findbar-row">
              <input
                ref={findInputRef}
                type="text"
                class={`volo-ed-find-input ${findInvalid ? "is-invalid" : ""}`}
                placeholder={t("ed.find_ph")}
                value={findQuery}
                onInput={(e) => {
                  setFindQuery((e.currentTarget as HTMLInputElement).value);
                  setFindIdx(0);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    stepFindMatch(e.shiftKey ? -1 : 1);
                  } else if (e.key === "Escape") {
                    e.preventDefault();
                    closeFindBar();
                  }
                }}
              />
              {findQuery ? (
                <span class="volo-ed-find-count">
                  {findMatches.length > 0 ? `${safeFindIdx + 1}/${findMatches.length}` : t("ed.no_matches")}
                </span>
              ) : null}
              <button
                type="button"
                class={`volo-ed-find-btn ${findCase ? "is-on" : ""}`}
                title={t("ed.match_case")}
                aria-label={t("ed.match_case")}
                aria-pressed={findCase}
                onClick={() => setFindCase((v) => !v)}
              >
                Aa
              </button>
              <button
                type="button"
                class={`volo-ed-find-btn ${findRegex ? "is-on" : ""}`}
                title={t("ed.match_regex")}
                aria-label={t("ed.match_regex")}
                aria-pressed={findRegex}
                onClick={() => setFindRegex((v) => !v)}
              >
                .*
              </button>
              <button
                type="button"
                class="volo-ed-find-btn"
                aria-label="Previous"
                onClick={() => stepFindMatch(-1)}
              >
                ↑
              </button>
              <button
                type="button"
                class="volo-ed-find-btn"
                aria-label="Next"
                onClick={() => stepFindMatch(1)}
              >
                ↓
              </button>
              <button
                type="button"
                class={`volo-ed-find-btn ${findShowReplace ? "is-on" : ""}`}
                title={t("ed.replace_ph")}
                aria-label={t("ed.replace_ph")}
                aria-pressed={findShowReplace}
                onClick={() => setFindShowReplace((v) => !v)}
              >
                ⇄
              </button>
              <button
                type="button"
                class="volo-ed-find-btn"
                aria-label={t("common.close")}
                onClick={closeFindBar}
              >
                ×
              </button>
            </div>
            {findShowReplace && (
              <div class="volo-ed-findbar-row">
                <input
                  type="text"
                  class="volo-ed-find-input"
                  placeholder={t("ed.replace_ph")}
                  value={replaceQuery}
                  onInput={(e) => setReplaceQuery((e.currentTarget as HTMLInputElement).value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") {
                      e.preventDefault();
                      replaceCurrentMatch();
                    } else if (e.key === "Escape") {
                      e.preventDefault();
                      closeFindBar();
                    }
                  }}
                />
                <button type="button" class="volo-ed-find-btn" onClick={replaceCurrentMatch}>
                  {t("ed.replace_one")}
                </button>
                <button type="button" class="volo-ed-find-btn" onClick={replaceAllMatches}>
                  {t("ed.replace_all")}
                </button>
              </div>
            )}
          </div>
        )}

        {/* CM6 Host stays mounted for undo history; hidden only in pure visual mode */}
        <div
          class={`volo-cm-host ${mode === "ir" ? "is-ir-mode" : ""}`}
          ref={hostRef}
          style={{ display: mode === "wysiwyg" ? "none" : undefined }}
        />

        {/* Visual WYSIWYG Table Action Bar + Editable Surface */}
        {mode === "wysiwyg" && wysiwygTblActive && (
          <div
            class="volo-wysiwyg-tbl-bar"
            role="toolbar"
            aria-label={t("ed.table")}
            onMouseDown={(e) => e.preventDefault()}
          >
            {(
              [
                ["row_above", "+Row ↑"],
                ["row_below", "+Row ↓"],
                ["del_row", "-Row"],
                null,
                ["col_left", "+Col ←"],
                ["col_right", "+Col →"],
                ["del_col", "-Col"],
                null,
                ["del_table", `× ${t("ed.table")}`],
              ] as const
            ).map((it, idx) =>
              it ? (
                <button key={it[0]} type="button" class="volo-ed-find-btn" onClick={() => mutateWysiwygTable(it[0])}>
                  {it[1]}
                </button>
              ) : (
                <span key={`s${idx}`} class="volo-ed-sep" />
              )
            )}
          </div>
        )}
        {mode === "wysiwyg" && (
          <div
            ref={wysiwygRef}
            class="volo-wysiwyg-surface"
            contentEditable={!wysiwygHydrating}
            aria-busy={wysiwygHydrating || undefined}
            onDragOver={(e) => {
              e.preventDefault();
            }}
            onDrop={(e) => {
              if (wysiwygHydratingRef.current) {
                e.preventDefault();
                return;
              }
              const files = e.dataTransfer?.files;
              if (!files || files.length === 0) return;
              e.preventDefault();
              const root = e.currentTarget as HTMLElement;
              const docAny = document as any;
              let dropRange: Range | null = null;
              if (typeof docAny.caretRangeFromPoint === "function") {
                dropRange = docAny.caretRangeFromPoint(e.clientX, e.clientY);
              } else if (typeof docAny.caretPositionFromPoint === "function") {
                const cp = docAny.caretPositionFromPoint(e.clientX, e.clientY);
                if (cp?.offsetNode) {
                  dropRange = document.createRange();
                  dropRange.setStart(cp.offsetNode, cp.offset);
                  dropRange.collapse(true);
                }
              }
              if (dropRange && root.contains(dropRange.commonAncestorContainer)) {
                const sel = window.getSelection();
                if (sel) {
                  sel.removeAllRanges();
                  sel.addRange(dropRange);
                }
                lastWysiwygRangeRef.current = dropRange.cloneRange();
              }
              void uploadMediaFiles(Array.from(files), true);
            }}
            onMouseDown={(e) => {
              const root = e.currentTarget as HTMLElement;
              const target = e.target as HTMLElement | null;
              if (!target) return;

              if (target.closest(".volo-code-head")) {
                return;
              }

              const delBtn = target.closest(".volo-wysiwyg-audio-del");
              if (delBtn) {
                e.preventDefault();
                e.stopPropagation();
                const block = delBtn.closest(SELECTABLE_WYSIWYG_BLOCK_SEL) as HTMLElement | null;
                if (block) removeWysiwygAudioBlock(root, block);
                return;
              }

              const selBlock = target.closest(SELECTABLE_WYSIWYG_BLOCK_SEL) as HTMLElement | null;
              root.querySelectorAll(`${SELECTABLE_WYSIWYG_BLOCK_SEL}`).forEach((el) => {
                if (el !== selBlock) el.classList.remove("is-selected");
              });
              if (selBlock) {
                selBlock.classList.add("is-selected");
                const tag = target.tagName.toLowerCase();
                if (tag !== "audio" && tag !== "video" && tag !== "button" && tag !== "input" && tag !== "select") {
                  e.preventDefault();
                  root.focus();
                }
                return;
              }

              if (target === root) {
                const rootRect = root.getBoundingClientRect();
                if (e.clientX >= rootRect.left + root.clientWidth || e.clientY >= rootRect.top + root.clientHeight) {
                  return;
                }
                normalizeWysiwygDOM(root, t("common.delete"));
                const editables = Array.from(root.children).filter(
                  (c): c is HTMLElement => c instanceof HTMLElement && !isNonEditableWysiwygBlock(c)
                );
                if (editables.length === 0) return;
                e.preventDefault();
                root.focus();
                const firstR = editables[0].getBoundingClientRect();
                if (e.clientY <= firstR.bottom) {
                  placeCaretInBlock(editables[0], e.clientY < firstR.top);
                  saveWysiwygSelection(root);
                  return;
                }
                let chosen = editables[editables.length - 1];
                for (const blk of editables) {
                  const r = blk.getBoundingClientRect();
                  if (e.clientY <= r.bottom + 6) {
                    chosen = blk;
                    break;
                  }
                }
                placeCaretInBlock(chosen, false);
                saveWysiwygSelection(root);
              }
            }}
            onMouseUp={(e) => {
              saveWysiwygSelection(e.currentTarget as HTMLElement);
            }}
            onKeyUp={(e) => {
              if (e.isComposing || (e as any).keyCode === 229 || wysiwygComposingRef.current) return;
              saveWysiwygSelection(e.currentTarget as HTMLElement);
            }}
            onChange={(e) => {
              const target = e.target as HTMLElement | null;
              if (target instanceof HTMLInputElement && target.type === "checkbox") {
                if (target.checked) target.setAttribute("checked", "");
                else target.removeAttribute("checked");
                scheduleWysiwygEmit(e.currentTarget as HTMLElement, false);
              }
            }}
            onKeyDownCapture={(e) => {
              if (e.isComposing || (e as any).keyCode === 229 || wysiwygComposingRef.current) return;
              const root = e.currentTarget as HTMLElement;
              const selectedBlock = root.querySelector(
                ".volo-wysiwyg-audio-block.is-selected, [data-volo-md-b64].is-selected"
              ) as HTMLElement | null;
              if (selectedBlock && (e.key === "Backspace" || e.key === "Delete")) {
                e.preventDefault();
                e.stopPropagation();
                removeWysiwygAudioBlock(root, selectedBlock);
              }
            }}
            onBeforeInput={(e) => {
              if (wysiwygHydratingRef.current) {
                e.preventDefault();
                return;
              }
              const ie = e as unknown as InputEvent;
              if (ie.inputType === "historyUndo") {
                e.preventDefault();
                applyWysiwygHistoryStep(-1);
              } else if (ie.inputType === "historyRedo") {
                e.preventDefault();
                applyWysiwygHistoryStep(1);
              }
            }}
            onKeyDown={(e) => {
              if (e.isComposing || (e as any).keyCode === 229 || wysiwygComposingRef.current) return;
              if ((e.metaKey || e.ctrlKey) && !e.altKey) {
                const k = e.key.toLowerCase();
                if (k === "z" && !e.shiftKey) {
                  e.preventDefault();
                  applyWysiwygHistoryStep(-1);
                  return;
                }
                if ((k === "z" && e.shiftKey) || (k === "y" && !e.shiftKey && !e.metaKey)) {
                  e.preventDefault();
                  applyWysiwygHistoryStep(1);
                  return;
                }
              }
              if ((e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey && (e.key === "f" || e.key === "F" || e.key === "h" || e.key === "H")) {
                e.preventDefault();
                openFindBar(e.key.toLowerCase() === "h");
                return;
              }
              const root = e.currentTarget as HTMLElement;
              const selectedBlock = root.querySelector(
                ".volo-wysiwyg-audio-block.is-selected, [data-volo-md-b64].is-selected"
              ) as HTMLElement | null;
              if (selectedBlock) {
                if (e.key === "ArrowUp") {
                  e.preventDefault();
                  selectedBlock.classList.remove("is-selected");
                  const prev = selectedBlock.previousElementSibling as HTMLElement | null;
                  if (prev && !isNonEditableWysiwygBlock(prev)) {
                    placeCaretInBlock(prev, false);
                    saveWysiwygSelection(root);
                  }
                  return;
                }
                if (e.key === "ArrowDown" || e.key === "Enter") {
                  e.preventDefault();
                  selectedBlock.classList.remove("is-selected");
                  const next = selectedBlock.nextElementSibling as HTMLElement | null;
                  if (next && !isNonEditableWysiwygBlock(next)) {
                    placeCaretInBlock(next, true);
                    saveWysiwygSelection(root);
                  }
                  return;
                }
                selectedBlock.classList.remove("is-selected");
              }

              if (e.key !== "Backspace" && e.key !== "Delete") return;
              const sel = window.getSelection();
              if (!sel || sel.rangeCount === 0 || !sel.isCollapsed) return;
              const range = sel.getRangeAt(0);

              if (range.startContainer === root) {
                const idx = range.startOffset;
                if (e.key === "Backspace" && idx > 0) {
                  const prevNode = root.childNodes[idx - 1];
                  if (prevNode instanceof Element && isSelectableWysiwygBlock(prevNode)) {
                    e.preventDefault();
                    removeWysiwygAudioBlock(root, prevNode);
                    return;
                  }
                }
                if (e.key === "Delete" && idx < root.childNodes.length) {
                  const nextNode = root.childNodes[idx];
                  if (nextNode instanceof Element && isSelectableWysiwygBlock(nextNode)) {
                    e.preventDefault();
                    removeWysiwygAudioBlock(root, nextNode);
                    return;
                  }
                }
              }

              let cur: Node | null = range.startContainer;
              while (cur && cur.parentNode !== root) {
                cur = cur.parentNode;
              }
              if (!(cur instanceof HTMLElement)) return;

              if (e.key === "Backspace" && isCaretAtBlockStart(cur, range)) {
                const prev = cur.previousElementSibling;
                if (isSelectableWysiwygBlock(prev)) {
                  e.preventDefault();
                  removeWysiwygAudioBlock(root, prev, cur);
                }
              } else if (e.key === "Delete" && isCaretAtBlockEnd(cur, range)) {
                const next = cur.nextElementSibling;
                if (isSelectableWysiwygBlock(next)) {
                  e.preventDefault();
                  removeWysiwygAudioBlock(root, next, cur);
                }
              }
            }}
            onCompositionStart={() => {
              wysiwygComposingRef.current = true;
            }}
            onCompositionEnd={(e) => {
              wysiwygComposingRef.current = false;
              const root = e.currentTarget as HTMLElement;
              saveWysiwygSelection(root);
              scheduleWysiwygEmit(root, false);
            }}
            onCopy={(e) => {
              const root = e.currentTarget as HTMLElement;
              const dt = e.clipboardData;
              if (!dt) return;
              const payload = extractWysiwygSelectionClipboard(root);
              if (!payload) return;
              e.preventDefault();
              dt.setData("text/plain", payload.plain);
              dt.setData("text/html", payload.html);
            }}
            onCut={(e) => {
              const root = e.currentTarget as HTMLElement;
              const dt = e.clipboardData;
              if (!dt) return;
              const payload = extractWysiwygSelectionClipboard(root);
              if (!payload) return;
              e.preventDefault();
              dt.setData("text/plain", payload.plain);
              dt.setData("text/html", payload.html);
              if (root.isContentEditable) {
                payload.range.deleteContents();
                const sel = window.getSelection();
                if (sel) {
                  sel.removeAllRanges();
                  sel.addRange(payload.range);
                }
                saveWysiwygSelection(root);
                scheduleWysiwygEmit(root, true);
              }
            }}
            onPaste={(e) => {
              if (wysiwygHydratingRef.current) {
                e.preventDefault();
                return;
              }
              const root = e.currentTarget as HTMLElement;
              saveWysiwygSelection(root);
              const sel = window.getSelection();
              const anchorEl =
                sel && sel.rangeCount > 0
                  ? sel.getRangeAt(0).startContainer.nodeType === Node.ELEMENT_NODE
                    ? (sel.getRangeAt(0).startContainer as Element)
                    : sel.getRangeAt(0).startContainer.parentElement
                  : null;
              handleRichPaste(
                e as unknown as ClipboardEvent,
                () => Boolean(anchorEl?.closest("pre")),
                (md) => spliceMarkdownAtWysiwygCaret(root, md),
                (rawPlain, rawHTML) => {
                  const trimmedPlain = rawPlain.trim();
                  const cleanHref =
                    !rawHTML.trim() && trimmedPlain && !/\s/.test(trimmedPlain)
                      ? safeHref(cleanClipboardUrl(trimmedPlain, "href"), {
                          allowRelative: false,
                          allowHash: false,
                          allowMailto: false,
                        })
                      : "";
                  if (cleanHref && sel && !sel.isCollapsed && !anchorEl?.closest("a, code")) {
                    const selStr = sel.toString().trim();
                    if (selStr && !selStr.includes("\n")) {
                      e.preventDefault();
                      document.execCommand("createLink", false, cleanHref);
                      saveWysiwygSelection(root);
                      pushWysiwygHistory(root, true);
                      scheduleWysiwygEmit(root, false);
                      return true;
                    }
                  }
                  return false;
                }
              );
            }}
            onInput={(e) => {
              if (wysiwygHydratingRef.current || (e as unknown as InputEvent).isComposing || wysiwygComposingRef.current) return;
              const root = e.currentTarget as HTMLElement;
              saveWysiwygSelection(root);
              scheduleWysiwygEmit(root, true);
            }}
            onBlur={(e) => {
              const root = e.currentTarget as HTMLElement;
              saveWysiwygSelection(root);
              root
                .querySelectorAll(".volo-wysiwyg-audio-block.is-selected, [data-volo-md-b64].is-selected")
                .forEach((el) => el.classList.remove("is-selected"));
              if (!wysiwygDirtyRef.current || wysiwygComposingRef.current) return;
              wysiwygDirtyRef.current = false;
              const md = htmlToMarkdown(root);
              onChangeRef.current(md);
            }}
          />
        )}

        {/* Split Live Preview Pane */}
        {mode === "sv" &&
          (previewHTML ? (
            <div
              ref={previewRef}
              class="volo-preview-pane"
              onCopy={(e) => {
                const sel = window.getSelection();
                const dt = e.clipboardData;
                if (!sel || sel.rangeCount === 0 || sel.isCollapsed || !dt) return;
                const range = sel.getRangeAt(0);
                const host = e.currentTarget as HTMLElement;
                const startEl = range.startContainer instanceof Element ? range.startContainer : range.startContainer.parentElement;
                const endEl = range.endContainer instanceof Element ? range.endContainer : range.endContainer.parentElement;
                const hasCode =
                  startEl?.closest("pre, .chroma, code") ||
                  endEl?.closest("pre, .chroma, code") ||
                  host.querySelector(".ln, .lnt") !== null;
                if (!hasCode) return;
                const frag = range.cloneContents();
                if (!frag.querySelector(".ln, .lnt, .volo-code-head, button")) return;
                frag.querySelectorAll(".ln, .lnt, .volo-code-head, button, select").forEach((n) => n.remove());
                const tmp = document.createElement("div");
                tmp.appendChild(frag);
                const plain = nodeNbspText(tmp);
                e.preventDefault();
                dt.setData("text/plain", plain);
                dt.setData("text/html", tmp.innerHTML);
              }}
              dangerouslySetInnerHTML={{ __html: previewHTML }}
            />
          ) : (
            <div ref={previewRef} class="volo-preview-pane">
              <p style={{ color: "var(--volo-muted)" }}>{t("ed.live_preview_empty")}</p>
            </div>
          ))}

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
        onSelect={insertMediaAssetSnippet}
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
                      ? `type="user"${lockHint.trim() ? ` ${formatDirectiveAttr("hint", lockHint)}` : ""}`
                      : `${formatDirectiveAttr("pwd", lockPwd || "123456")}${lockHint.trim() ? ` ${formatDirectiveAttr("hint", lockHint)}` : ""}`;
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
                  ["video", t("cnt.media_type_video")],
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
                  const mime = String(m.mime_type || "");
                  const isImg = mime.startsWith("image/");
                  const isVid = !isImg && isVideoMedia(mime, m.public_url);
                  const safeMedia = safeMediaSrc(m.public_url, { allowDataImage: isImg });
                  return (
                    <button
                      key={m.id}
                      type="button"
                      class="volo-qi-media-card"
                      onClick={() => {
                        insertMediaAssetSnippet(m);
                        setQiOpen(false);
                      }}
                    >
                      {isImg && safeMedia ? (
                        <img src={safeMedia} alt={name} loading="lazy" />
                      ) : isVid && safeMedia ? (
                        <video src={safeMedia} preload="metadata" muted playsInline />
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

        {(() => {
          const renderQiPickList = <T extends { id: string }>(
            items: T[],
            search: string,
            page: number,
            pageSize: number,
            maxH: string,
            gap: string,
            pad: string,
            matchFn: (it: T, q: string) => boolean,
            renderBody: (it: T) => preact.ComponentChildren,
            onPick: (it: T) => void
          ) => {
            const q = search.trim().toLowerCase();
            const filtered = items.length > pageSize && q ? items.filter((it) => matchFn(it, q)) : items;
            const rows = filtered.length > pageSize ? paginateItems(filtered, page, pageSize).slice : filtered;
            if (rows.length === 0) return <ConsoleEmpty text={t("tbl.empty")} />;
            return (
              <div style={{ display: "flex", flexDirection: "column", gap, maxHeight: maxH, overflowY: "auto" }}>
                {rows.map((it) => (
                  <div
                    key={it.id}
                    class="volo-subcard"
                    style={{
                      padding: pad,
                      margin: 0,
                      display: "flex",
                      justifyContent: "space-between",
                      alignItems: "center",
                      gap: "10px",
                    }}
                  >
                    <div style={{ minWidth: 0, flex: 1 }}>{renderBody(it)}</div>
                    <button
                      type="button"
                      class="volo-btn volo-btn-sm"
                      style={{ flexShrink: 0 }}
                      onClick={() => {
                        onPick(it);
                        setQiOpen(false);
                      }}
                    >
                      {t("ed.qi_insert_btn")}
                    </button>
                  </div>
                ))}
              </div>
            );
          };

          if (qiTab === "books") {
            return (
              <div>
                <div style={{ marginBottom: "10px", display: "flex", gap: "8px", alignItems: "center", flexWrap: "wrap" }}>
                  <input
                    class="volo-input"
                    style={{ flex: "1 1 200px", minWidth: "150px", height: "34px" }}
                    placeholder={t("ed.qi_search_books")}
                    value={qiBookSearch}
                    onInput={(e) => {
                      setQiBookSearch(e.currentTarget.value);
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
                {renderQiPickList(
                  qiBooks,
                  qiBookSearch,
                  qiBooksPage,
                  10,
                  "260px",
                  "8px",
                  "10px 12px",
                  (b, q) =>
                    String(b.title || "").toLowerCase().includes(q) ||
                    String(b.author || "").toLowerCase().includes(q) ||
                    String(b.slug || "").toLowerCase().includes(q),
                  (b) => (
                    <>
                      <div style={{ fontWeight: 650, fontSize: "13px", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                        {b.title}
                      </div>
                      <div class="volo-help" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                        {b.description || `/books/${b.slug}`}
                      </div>
                    </>
                  ),
                  (b) =>
                    insertSnippet(
                      b.slug
                        ? `\n:::book ${formatDirectiveAttr("slug", b.slug)}\n:::\n`
                        : `\n:::book ${formatDirectiveAttr("title", b.title)} ${formatDirectiveAttr("status", b.reading_status || "reading")}\n:::\n`
                    )
                )}
                <PaginationBar total={qiBooksTotal} page={qiBooksPage} pageSize={10} onChange={(p) => setQiBooksPage(p)} />
              </div>
            );
          }
          if (qiTab === "cite") {
            return (
              <div>
                <div class="volo-field" style={{ marginBottom: "10px" }}>
                  <label>{t("ed.qi_cite")}</label>
                  <input
                    class="volo-input"
                    placeholder={t("ed.qi_search_art")}
                    value={qiArtSearch}
                    onInput={(e) => {
                      setQiArtSearch(e.currentTarget.value);
                      setQiArtPage(1);
                    }}
                  />
                </div>
                {renderQiPickList(
                  qiArticles,
                  qiArtSearch,
                  qiArtPage,
                  20,
                  "240px",
                  "6px",
                  "8px 12px",
                  (a, q) => String(a.title || "").toLowerCase().includes(q) || String(a.slug || "").toLowerCase().includes(q),
                  (a) => {
                    const st = String(a.status || "published");
                    const stClass = st === "published" ? "published" : st === "scheduled" ? "scheduled" : "draft";
                    return (
                      <>
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
                      </>
                    );
                  },
                  (a) => insertSnippet(`[${a.title || a.slug}](/posts/${a.slug})`)
                )}
                <PaginationBar total={qiArtTotal} page={qiArtPage} pageSize={20} onChange={(p) => setQiArtPage(p)} />
              </div>
            );
          }
          return null;
        })()}

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
      </AnimatedModal>
    </div>
  );
}

function buildCssSyntaxPlugin() {
  const buildDecos = (view: EditorView): DecorationSet => {
    const builder = new RangeSetBuilder<Decoration>();
    const doc = view.state.doc;
    const composing = view.composing;
    const cursorLine = composing ? doc.lineAt(view.state.selection.main.head) : null;
    let lastAddedTo = 0;
    for (const { from, to } of view.visibleRanges) {
      const scanFrom = Math.max(0, from - 4096);
      const scanTo = Math.min(doc.length, to + 512);
      const slice = doc.sliceString(scanFrom, scanTo);
      interface Tok {
        s: number;
        e: number;
        cls: string;
      }
      const toks: Tok[] = [];
      const used = new Uint8Array(slice.length);
      const claim = (s: number, e: number, cls: string) => {
        if (s >= e || s < 0 || e > slice.length) return;
        for (let i = s; i < e; i++) if (used[i]) return;
        for (let i = s; i < e; i++) used[i] = 1;
        const absS = Math.max(from, scanFrom + s);
        const absE = Math.min(to, scanFrom + e);
        if (absS < absE) toks.push({ s: absS, e: absE, cls });
      };

      const cssCtx = computeCssContext(slice);
      let cPos = 0;
      while (cPos < slice.length) {
        const cs = slice.indexOf("/*", cPos);
        if (cs < 0) break;
        const ce = slice.indexOf("*/", cs + 2);
        const end = ce < 0 ? slice.length : ce + 2;
        claim(cs, end, "cm");
        cPos = end;
      }
      let m: RegExpExecArray | null;
      const scanCss = (re: RegExp, cls: string, grp = 0, ctxFilter = 0) => {
        re.lastIndex = 0;
        while ((m = re.exec(slice)) !== null) {
          if (!ctxFilter || cssCtx[m.index] === ctxFilter) claim(m.index, m.index + m[grp].length, cls);
        }
      };
      for (const [s, e] of findCssStringRanges(slice, used)) {
        claim(s, e, "s2");
      }
      scanCss(CSS_AT_RE, "k");
      scanCss(CSS_IMP_RE, "kc");
      scanCss(/--[a-zA-Z0-9_-]+/g, "nv");
      scanCss(/(::?[a-zA-Z-][a-zA-Z0-9_-]*)/g, "nd", 0, 1);
      scanCss(/[.#][A-Za-z_-][A-Za-z0-9_-]*/g, "nc", 0, 1);
      const propRe = /(?:^|[;{\s(])([a-zA-Z-]+)\s*:(?!:)/g;
      while ((m = propRe.exec(slice)) !== null) {
        const s = m.index + (m[0][0] === m[1][0] ? 0 : 1);
        if (cssCtx[s] === 2) claim(s, s + m[1].length, "na");
      }
      scanCss(CSS_FN_RE, "nf", 1);
      scanCss(/#[0-9a-fA-F]{3,8}\b/g, "mi", 0, 2);
      scanCss(/-?\b\d+(?:\.\d+)?(?:px|rem|em|vh|vw|dvw|svw|dvh|svh|%|s|ms|deg|fr|ch)?\b/g, "mi");
      scanCss(CSS_VAL_KW_RE, "kc");

      toks.sort((a, b) => a.s - b.s || a.e - b.e);
      for (const tk of toks) {
        if (tk.s < lastAddedTo) continue;
        if (cursorLine && tk.s < cursorLine.to && tk.e > cursorLine.from) {
          continue;
        }
        builder.add(tk.s, tk.e, Decoration.mark({ class: tk.cls }));
        lastAddedTo = tk.e;
      }
    }
    return builder.finish();
  };

  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      wasComposing: boolean;
      constructor(view: EditorView) {
        this.wasComposing = view.composing;
        this.decorations = buildDecos(view);
      }
      update(u: ViewUpdate) {
        const compChanged = u.view.composing !== this.wasComposing;
        this.wasComposing = u.view.composing;
        if (u.docChanged || u.viewportChanged || compChanged || (u.view.composing && u.selectionSet)) {
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
  const phCompartment = useRef(new Compartment());
  const valueRef = useRef(value);
  valueRef.current = value;
  const composingRef = useRef(false);
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const onSaveRef = useRef(onSave);
  onSaveRef.current = onSave;
  const propSyncRef = useRef(createPropSyncTracker());

  useEffect(() => {
    if (!hostRef.current) return;
    const imeHandlers = EditorView.domEventHandlers(
      buildCMImeHandlers(composingRef, valueRef, onChangeRef, propSyncRef.current)
    );
    const state = EditorState.create({
      doc: value || "",
      extensions: [
        lineNumbers(),
        history(),
        EditorView.lineWrapping,
        buildCssSyntaxPlugin(),
        buildSmartTypingExt(true),
        phCompartment.current.of(placeholder(phText || "")),
        Prec.high(
          keymap.of([
            {
              key: "Enter",
              run: expandIndentedBracesOnEnter,
            },
            {
              key: "Backspace",
              run: (v) => {
                const { state: st } = v;
                if (st.readOnly || st.selection.ranges.length !== 1 || !st.selection.main.empty) return false;
                const pos = st.selection.main.head;
                const ln = st.doc.lineAt(pos);
                return pos > ln.from ? deleteAutoPair(v, pos, ln.to) : false;
              },
            },
            {
              key: "Mod-/",
              run: (v) => toggleEditorComment(v, true),
            },
          ])
        ),
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
        imeHandlers,
        EditorView.updateListener.of((u) => {
          if (u.docChanged && !isPropSyncUpdate(u) && !u.view.composing && !composingRef.current) {
            const docStr = u.state.doc.toString();
            recordEditorEmit(propSyncRef.current, docStr);
            onChangeRef.current(docStr);
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
    syncCMViewValue(viewRef.current, composingRef.current, propSyncRef.current, value || "");
  }, [value]);

  useEffect(() => {
    viewRef.current?.dispatch({
      effects: phCompartment.current.reconfigure(placeholder(phText || "")),
    });
  }, [phText]);

  return <div ref={hostRef} class="volo-cm-host volo-css-cm-host chroma" />;
}
