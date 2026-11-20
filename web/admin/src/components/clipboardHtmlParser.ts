// Pure-TS non-DOM HTML tokenizer and tree-node parser for clipboard HTML paste inspection.
// Avoids DOMParser / innerHTML / createContextualFragment so untrusted clipboard HTML never hits a DOM parser sink.

const VOID_RE = /^(?:area|base|br|col|embed|hr|img|input|link|meta|param|source|track|wbr)$/;
const RAW_TEXT_RE = /^(?:script|style|textarea|title)$/;
const HEAD_ONLY_RE = /^(?:meta|link|base|title)$/;
const P_CLOSE_RE =
  /^(?:address|article|aside|blockquote|details|div|dl|figure|figcaption|footer|h[1-6]|header|hr|li|main|nav|ol|p|pre|section|table|ul)$/;
const ENTITIES: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: "\u00a0" };

function decodeEnt(s: string): string {
  return s.includes("&")
    ? s.replace(/&(#x[0-9a-fA-F]+|#\d+|[a-zA-Z]+);/g, (m, e: string) => {
        if (e[0] === "#") {
          const c = e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10);
          return c > 0 && c <= 0x10ffff ? String.fromCodePoint(c) : c === 0 ? "\ufffd" : m;
        }
        return ENTITIES[e.toLowerCase()] ?? m;
      })
    : s;
}

function parseStyle(raw: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const decl of raw.split(";")) {
    const col = decl.indexOf(":");
    if (col <= 0) continue;
    const p = decl.slice(0, col).trim().toLowerCase().replace(/-([a-z])/g, (_, c: string) => c.toUpperCase());
    if (p) out[p] = decl.slice(col + 1).replace(/\s*!important\s*$/i, "").trim();
  }
  return out;
}

type SimpleSel = { tag: string; id: string; cls: string[]; attrs: Array<[string, string, string]>; scope: boolean };
type SelPart = { comb: string; s: SimpleSel };

function splitCommas(sel: string): string[] {
  const out: string[] = [];
  let start = 0, br = 0, q = "";
  for (let i = 0; i < sel.length; i++) {
    const ch = sel[i];
    if (q) { if (ch === q) q = ""; }
    else if (ch === '"' || ch === "'") q = ch;
    else if (ch === "[") br++;
    else if (ch === "]" && br > 0) br--;
    else if (ch === "," && br === 0) {
      const t = sel.slice(start, i).trim();
      if (t) out.push(t);
      start = i + 1;
    }
  }
  const tail = sel.slice(start).trim();
  if (tail) out.push(tail);
  return out;
}

function parseSimple(raw: string): SimpleSel {
  const res: SimpleSel = { tag: "", id: "", cls: [], attrs: [], scope: raw.startsWith(":scope") };
  let i = res.scope ? 6 : 0;
  const rd = () => {
    const st = i;
    while (i < raw.length && !/[.#[:\s]/.test(raw[i])) i++;
    return raw.slice(st, i);
  };
  while (i < raw.length) {
    const ch = raw[i];
    if (ch === "#") { i++; res.id = rd(); }
    else if (ch === ".") { i++; const c = rd(); if (c) res.cls.push(c); }
    else if (ch === "[") {
      i++;
      let q = "", end = -1;
      for (let j = i; j < raw.length; j++) {
        if (q) { if (raw[j] === q) q = ""; }
        else if (raw[j] === '"' || raw[j] === "'") q = raw[j];
        else if (raw[j] === "]") { end = j; break; }
      }
      if (end < 0) break;
      const m = /^([a-zA-Z0-9_:-]+)\s*(?:(\^=|\*=|=)\s*(?:"([^"]*)"|'([^']*)'|([^\s]*)))?$/.exec(raw.slice(i, end).trim());
      i = end + 1;
      if (m) res.attrs.push([m[1].toLowerCase(), m[2] || "", m[3] ?? m[4] ?? m[5] ?? ""]);
    } else if (ch === ":") {
      i++; rd();
      res.attrs.push(["__u__", "=", "1"]);
    } else {
      res.tag = rd().toLowerCase();
    }
  }
  return res;
}

function parseComplex(raw: string): SelPart[] {
  const parts: SelPart[] = [];
  let i = 0, comb = "", n = raw.length;
  while (i < n) {
    while (i < n && /\s/.test(raw[i])) { if (parts.length && comb !== ">") comb = " "; i++; }
    if (i >= n) break;
    if (raw[i] === ">") { comb = ">"; i++; while (i < n && /\s/.test(raw[i])) i++; continue; }
    let st = i, br = 0, q = "";
    while (i < n) {
      const ch = raw[i];
      if (q) { if (ch === q) q = ""; }
      else if (ch === '"' || ch === "'") q = ch;
      else if (ch === "[") br++;
      else if (ch === "]" && br > 0) br--;
      else if (br === 0 && (/\s/.test(ch) || ch === ">")) break;
      i++;
    }
    const tok = raw.slice(st, i);
    if (tok) { parts.push({ comb: parts.length ? comb || " " : "", s: parseSimple(tok) }); comb = ""; }
  }
  return parts;
}

const selCache = new Map<string, SelPart[][]>();
function compileSel(sel: string): SelPart[][] {
  let c = selCache.get(sel);
  if (!c) {
    c = splitCommas(sel).map(parseComplex).filter((p) => p.length > 0);
    if (selCache.size < 256) selCache.set(sel, c);
  }
  return c;
}

export class VNode {
  nodeType: number;
  tagName: string;
  _t: string;
  parentNode: VNode | null = null;
  ownerDocument: VDocument;
  childNodes: VNode[] = [];
  _txt: string;
  private _a: Record<string, string>;
  private _st: Record<string, string> | null = null;
  private _cl: string[] | null = null;
  readonly classList: { contains: (cls: string) => boolean };

  constructor(nodeType: number, tagOrText: string, owner: VDocument, attrs: Record<string, string> = {}) {
    this.nodeType = nodeType;
    this.ownerDocument = owner;
    this._a = attrs;
    if (nodeType === 3) {
      this._t = "";
      this.tagName = "";
      this._txt = tagOrText;
    } else {
      this._t = tagOrText.toLowerCase();
      this.tagName = tagOrText.toUpperCase();
      this._txt = "";
    }
    this.classList = {
      contains: (cls: string) => {
        if (!this._cl) {
          const r = this._a.class || "";
          this._cl = r ? r.trim().split(/\s+/) : [];
        }
        return this._cl.includes(cls);
      },
    };
  }

  get parentElement(): VNode | null { return this.parentNode?.nodeType === 1 ? this.parentNode : null; }
  private sib(d: -1 | 1): VNode | null {
    const c = this.parentNode?.childNodes;
    if (!c) return null;
    const idx = c.indexOf(this);
    return idx >= 0 ? c[idx + d] || null : null;
  }
  get previousSibling(): VNode | null { return this.sib(-1); }
  get nextSibling(): VNode | null { return this.sib(1); }
  get previousElementSibling(): VNode | null {
    let cur = this.previousSibling;
    while (cur && cur.nodeType !== 1) cur = cur.previousSibling;
    return cur;
  }
  get nextElementSibling(): VNode | null {
    let cur = this.nextSibling;
    while (cur && cur.nodeType !== 1) cur = cur.nextSibling;
    return cur;
  }
  get firstChild(): VNode | null { return this.childNodes[0] || null; }
  get firstElementChild(): VNode | null { return this.childNodes.find((c) => c.nodeType === 1) || null; }
  get children(): VNode[] { return this.childNodes.filter((c) => c.nodeType === 1); }
  get id(): string { return this._a.id || ""; }
  set id(v: string) { this.setAttribute("id", v); }
  get className(): string { return this._a.class || ""; }
  set className(v: string) { this.setAttribute("class", v); }
  get type(): string { return this._a.type || ""; }
  get checked(): boolean { return Object.prototype.hasOwnProperty.call(this._a, "checked"); }
  get style(): Record<string, string> { return (this._st ??= parseStyle(this._a.style || "")); }

  getAttribute(name: string): string | null {
    const k = name.toLowerCase();
    return Object.prototype.hasOwnProperty.call(this._a, k) ? this._a[k] : null;
  }
  setAttribute(name: string, val: string): void {
    const k = name.toLowerCase();
    this._a[k] = String(val);
    if (k === "class") this._cl = null;
    if (k === "style") this._st = null;
  }
  hasAttribute(name: string): boolean { return Object.prototype.hasOwnProperty.call(this._a, name.toLowerCase()); }

  get textContent(): string {
    return this.nodeType === 3 ? this._txt : this.childNodes.map((c) => c.textContent).join("");
  }
  set textContent(v: string) {
    if (this.nodeType === 3) this._txt = v;
    else v ? this.replaceChildren(new VNode(3, v, this.ownerDocument)) : this.replaceChildren();
  }

  appendChild(node: VNode): VNode {
    if (node.nodeType === 11) {
      for (const it of [...node.childNodes]) this.appendChild(it);
      return node;
    }
    node.parentNode?._rm(node);
    node.parentNode = this;
    this.childNodes.push(node);
    return node;
  }
  replaceChildren(...nodes: Array<VNode | string>): void {
    for (const c of this.childNodes) c.parentNode = null;
    this.childNodes = [];
    for (const n of nodes) this.appendChild(typeof n === "string" ? new VNode(3, n, this.ownerDocument) : n);
  }
  _rm(node: VNode): void {
    const idx = this.childNodes.indexOf(node);
    if (idx >= 0) { this.childNodes.splice(idx, 1); node.parentNode = null; }
  }
  remove(): void { this.parentNode?._rm(this); }
  replaceWith(...nodes: Array<VNode | string>): void {
    const p = this.parentNode;
    if (!p) return;
    const idx = p.childNodes.indexOf(this);
    if (idx < 0) return;
    this.parentNode = null;
    const exp: VNode[] = [];
    for (const it of nodes) {
      if (typeof it === "string") exp.push(new VNode(3, it, p.ownerDocument));
      else if (it.nodeType === 11) {
        for (const s of [...it.childNodes]) { it._rm(s); s.parentNode = p; exp.push(s); }
      } else { it.parentNode?._rm(it); it.parentNode = p; exp.push(it); }
    }
    p.childNodes.splice(idx, 1, ...exp);
  }
  cloneNode(deep = false): VNode {
    const cl = new VNode(this.nodeType, this.nodeType === 3 ? this._txt : this._t, this.ownerDocument, { ...this._a });
    if (this._st) cl._st = { ...this._st };
    if (deep) for (const c of this.childNodes) cl.appendChild(c.cloneNode(true));
    return cl;
  }

  private matchS(s: SimpleSel, sc: VNode | null): boolean {
    if (s.scope && this !== sc) return false;
    if (s.tag && s.tag !== "*" && this._t !== s.tag) return false;
    if (s.id && this.id !== s.id) return false;
    for (const c of s.cls) if (!this.classList.contains(c)) return false;
    for (const [n, op, val] of s.attrs) {
      if (!this.hasAttribute(n)) return false;
      if (op) {
        const v = this._a[n] || "";
        if ((op === "=" && v !== val) || (op === "^=" && !v.startsWith(val)) || (op === "*=" && !v.includes(val))) return false;
      }
    }
    return true;
  }
  private matchC(p: SelPart[], idx: number, sc: VNode | null): boolean {
    if (!this.matchS(p[idx].s, sc)) return false;
    if (idx === 0) return true;
    if (p[idx].comb === ">") return this.parentElement ? this.parentElement.matchC(p, idx - 1, sc) : false;
    for (let anc = this.parentElement; anc; anc = anc.parentElement) if (anc.matchC(p, idx - 1, sc)) return true;
    return false;
  }
  closest(sel: string): VNode | null {
    const gs = compileSel(sel);
    for (let cur: VNode | null = this; cur; cur = cur.parentElement) {
      if (gs.some((c) => cur!.matchC(c, c.length - 1, null))) return cur;
    }
    return null;
  }
  querySelectorAll(sel: string): VNode[] {
    const out: VNode[] = [], gs = compileSel(sel);
    const walk = (n: VNode) => {
      for (const ch of n.childNodes) {
        if (ch.nodeType === 1) {
          if (gs.some((c) => ch.matchC(c, c.length - 1, this))) out.push(ch);
          walk(ch);
        }
      }
    };
    walk(this);
    return out;
  }
  querySelector(sel: string): VNode | null {
    const gs = compileSel(sel);
    const walk = (n: VNode): VNode | null => {
      for (const ch of n.childNodes) {
        if (ch.nodeType === 1) {
          if (gs.some((c) => ch.matchC(c, c.length - 1, this))) return ch;
          const sub = walk(ch);
          if (sub) return sub;
        }
      }
      return null;
    };
    return walk(this);
  }
}

export interface VDocument {
  head: VNode;
  body: VNode;
  createElement(tag: string): VNode;
  createTextNode(text: string): VNode;
  createDocumentFragment(): VNode;
  querySelectorAll(sel: string): VNode[];
  querySelector(sel: string): VNode | null;
}

export function parseClipboardHtml(html: string): VDocument {
  const doc: VDocument = {
    head: null as unknown as VNode,
    body: null as unknown as VNode,
    createElement: (tag) => new VNode(1, tag, doc),
    createTextNode: (text) => new VNode(3, text, doc),
    createDocumentFragment: () => new VNode(11, "#frag", doc),
    querySelectorAll: (sel) => [...doc.head.querySelectorAll(sel), ...doc.body.querySelectorAll(sel)],
    querySelector: (sel) => doc.head.querySelector(sel) || doc.body.querySelector(sel),
  };
  doc.head = new VNode(1, "head", doc);
  doc.body = new VNode(1, "body", doc);

  const stack: VNode[] = [doc.body], n = html.length;
  let seenBody = false, i = 0;
  const addTxt = (t: string) => {
    if (!t) return;
    const dec = decodeEnt(t), p = stack[stack.length - 1] || doc.body, prev = p.childNodes[p.childNodes.length - 1];
    if (prev?.nodeType === 3) prev._txt += dec; else p.appendChild(new VNode(3, dec, doc));
  };
  while (i < n) {
    const lt = html.indexOf("<", i);
    if (lt < 0) { addTxt(html.slice(i)); break; }
    if (lt > i) { addTxt(html.slice(i, lt)); i = lt; }
    if (html.startsWith("<!--", i)) { const e = html.indexOf("-->", i + 4); i = e < 0 ? n : e + 3; continue; }
    if (i + 1 < n && (html[i + 1] === "!" || html[i + 1] === "?")) { const g = html.indexOf(">", i + 2); i = g < 0 ? n : g + 1; continue; }
    if (i + 1 < n && html[i + 1] === "/") {
      const g = html.indexOf(">", i + 2);
      if (g < 0) { addTxt(html.slice(i)); break; }
      const cn = html.slice(i + 2, g).trim().toLowerCase();
      i = g + 1;
      if (cn === "head") { stack.length = 1; stack[0] = doc.body; continue; }
      if (cn === "body" || cn === "html") continue;
      for (let k = stack.length - 1; k >= 1; k--) if (stack[k]._t === cn) { stack.length = k; break; }
      continue;
    }
    if (i + 1 >= n || !/[a-zA-Z]/.test(html[i + 1])) { addTxt("<"); i++; continue; }
    let j = i + 1;
    while (j < n && /[a-zA-Z0-9:-]/.test(html[j])) j++;
    const tag = html.slice(i + 1, j).toLowerCase(), attrs: Record<string, string> = {};
    let selfClose = false, closed = false;
    while (j < n) {
      while (j < n && /\s/.test(html[j])) j++;
      if (j >= n) break;
      if (html[j] === "/" && html[j + 1] === ">") { selfClose = closed = true; j += 2; break; }
      if (html[j] === ">") { closed = true; j++; break; }
      const aS = j;
      while (j < n && !/[\s=/>]/.test(html[j])) j++;
      const aN = html.slice(aS, j).toLowerCase();
      if (!aN) { j++; continue; }
      while (j < n && /\s/.test(html[j])) j++;
      let aV = "";
      if (html[j] === "=") {
        j++;
        while (j < n && /\s/.test(html[j])) j++;
        if (html[j] === '"' || html[j] === "'") {
          const q = html[j++], vS = j, vE = html.indexOf(q, j);
          if (vE < 0) { aV = html.slice(vS); j = n; } else { aV = html.slice(vS, vE); j = vE + 1; }
        } else {
          const vS = j;
          while (j < n && !/[\s>]/.test(html[j])) j++;
          aV = html.slice(vS, j);
        }
      }
      if (!Object.prototype.hasOwnProperty.call(attrs, aN)) attrs[aN] = decodeEnt(aV);
    }
    if (!closed) { addTxt(html.slice(i)); break; }
    i = j;
    if (tag === "html") continue;
    if (tag === "head") { if (!seenBody) { stack.length = 1; stack[0] = doc.head; } continue; }
    if (tag === "body") { seenBody = true; stack.length = 1; stack[0] = doc.body; continue; }
    const toHead = !seenBody && HEAD_ONLY_RE.test(tag);
    if (!toHead) {
      const top = stack[stack.length - 1]._t;
      if ((P_CLOSE_RE.test(tag) && top === "p") || (tag === "li" && top === "li") || ((tag === "td" || tag === "th") && (top === "td" || top === "th"))) {
        stack.pop();
      } else if (tag === "tr") {
        while (stack.length > 1 && /^(?:td|th|tr)$/.test(stack[stack.length - 1]._t)) stack.pop();
      }
    }
    const el = new VNode(1, tag, doc, attrs);
    (toHead ? doc.head : stack[stack.length - 1]).appendChild(el);
    if (RAW_TEXT_RE.test(tag) && !selfClose) {
      const tail = html.slice(i), m = new RegExp(`<\\/${tag}\\s*>`, "i").exec(tail);
      if (m) { if (m.index > 0) el.appendChild(new VNode(3, tail.slice(0, m.index), doc)); i += m.index + m[0].length; }
      else { if (tail) el.appendChild(new VNode(3, tail, doc)); i = n; }
      continue;
    }
    if (!selfClose && !VOID_RE.test(tag) && !toHead) stack.push(el);
  }
  return doc;
}
