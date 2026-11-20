export { cleanClipboardUrl } from "../urlSanitize.ts";

export const IR_CODE_RE = /(?<!`)(`+)(?!`)([^\n]+?)(?<!`)\1(?!`)/g;
export const IR_FN_REF_RE = /\[\^([^\s[\]\n\x01\x02]+)\]/g;
export const IR_INLINE_MATH_RE =
  /(?:^|[^\\$])\$(?!\s|\$)([^\n$\x01\x02]*?[^\s\\$\x01\x02])\$(?![0-9a-zA-Z\\$])|(?<!\\)\\\(((?:[^\n\x01\x02\\]|\\(?:\\|(?![()\\])))+?)\\\)/g;
export const IR_ML_ANCHOR_RE =
  /(<a\b(?:"[^"<>]*"|'[^'<>]*'|[^"'<>])*?>)([^<]*(?:<(?!\/?a\b)[^<]*)*)(<\/a>)/gi;

export const CODE_BLOCK_LANGS: ReadonlyArray<{ value: string; label: string }> = (
  [
    ["go", "Go"],
    ["bash", "Bash / Shell"],
    ["bat", "Batch / CMD"],
    ["powershell", "PowerShell"],
    ["dockerfile", "Dockerfile"],
    ["makefile", "Makefile"],
    ["javascript", "JavaScript"],
    ["jsx", "JSX"],
    ["typescript", "TypeScript"],
    ["tsx", "TSX"],
    ["python", "Python"],
    ["rust", "Rust"],
    ["c", "C"],
    ["cpp", "C++"],
    ["csharp", "C#"],
    ["objective-c", "Objective-C"],
    ["java", "Java"],
    ["kotlin", "Kotlin"],
    ["swift", "Swift"],
    ["prisma", "Prisma"],
    ["php", "PHP"],
    ["ruby", "Ruby"],
    ["lua", "Lua"],
    ["dart", "Dart"],
    ["scala", "Scala"],
    ["zig", "Zig"],
    ["elixir", "Elixir"],
    ["r", "R"],
    ["perl", "Perl"],
    ["sql", "SQL"],
    ["graphql", "GraphQL"],
    ["proto", "Protobuf"],
    ["hcl", "HCL / Terraform"],
    ["json", "JSON"],
    ["yaml", "YAML"],
    ["toml", "TOML"],
    ["ini", "INI"],
    ["env", ".env"],
    ["properties", "Properties"],
    ["systemd", "Systemd"],
    ["nginx", "Nginx"],
    ["caddy", "Caddyfile"],
    ["apache", "Apache"],
    ["html", "HTML"],
    ["xml", "XML"],
    ["svg", "SVG"],
    ["vue", "Vue"],
    ["svelte", "Svelte"],
    ["css", "CSS"],
    ["scss", "SCSS"],
    ["less", "Less"],
    ["diff", "Diff"],
    ["markdown", "Markdown"],
    ["mermaid", "Mermaid"],
    ["haskell", "Haskell"],
    ["ada", "Ada"],
    ["vhdl", "VHDL"],
    ["", ""],
  ] as const
).map(([value, label]) => ({ value, label }));

const FENCE_FAM_GROUPS: ReadonlyArray<readonly [number, string]> = [
  [
    1,
    "c cpp c++ cc cxx h hpp cs c# csharp objective-c objc fs f# fsharp vb vb.net vbnet go golang js jsx javascript mjs cjs ts tsx typescript mts cts java kotlin kt swift dart rust rs zig scala prisma php php8 phtml",
  ],
  [
    2,
    "sh bash zsh fish ash shell console shellsession sh-session terminal bat cmd powershell ps1 pwsh py python python3 rb ruby elixir ex exs hcl terraform tf yaml yml toml ini conf nginx caddy caddyfile apache apacheconf haproxy vhost env dotenv properties systemd service editorconfig dockerfile docker make makefile perl pl r",
  ],
  [3, "json jsonc json5 geojson"],
  [4, "html htm xml svg vue svelte"],
  [5, "css scss sass less"],
  [6, "diff patch"],
  [7, "sql mysql postgres postgresql sqlite pgsql"],
  [8, "lua"],
  [9, "graphql gql"],
  [10, "proto protobuf"],
  [11, "md markdown"],
  [12, "haskell hs ada vhdl"],
];

const FENCE_FAM_MAP: Record<string, number> = {};
export const HASH_COMMENT_LANGS = new Set<string>();
export const DASH_COMMENT_LANGS = new Set<string>();
export const NO_HASH_COMMENT_LANGS = new Set<string>(["glsl", "hlsl", "wgsl", "groovy", "objc", "objective-c", "bat", "cmd"]);

for (const [fam, list] of FENCE_FAM_GROUPS) {
  for (const k of list.split(" ")) {
    FENCE_FAM_MAP[k] = fam;
    if ((fam === 2 || fam === 9) && !NO_HASH_COMMENT_LANGS.has(k)) HASH_COMMENT_LANGS.add(k);
    if (fam === 7 || fam === 8 || fam === 12) DASH_COMMENT_LANGS.add(k);
    if (fam !== 2 && fam !== 9 && k !== "php") NO_HASH_COMMENT_LANGS.add(k);
  }
}

export function resolveFenceLangFamily(lang: string): number {
  return FENCE_FAM_MAP[lang] || 0;
}

export function computeFenceDelim(code: string): string {
  let maxRun = 0;
  for (const m of code.matchAll(/`+/g)) {
    if (m[0].length > maxRun) maxRun = m[0].length;
  }
  return "`".repeat(Math.max(3, maxRun + 1));
}

export function hasUnescapedTrailingPipe(s: string): boolean {
  if (!s.endsWith("|")) return false;
  let bs = 0;
  for (let i = s.length - 2; i >= 0 && s[i] === "\\"; i--) bs++;
  return bs % 2 === 0;
}

export function splitGfmTableRow(line: string): string[] {
  let s = line.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (hasUnescapedTrailingPipe(s)) s = s.slice(0, -1);
  const cells: string[] = [];
  let cur = "";
  let codeTicks = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "\\" && i + 1 < s.length) {
      const next = s[i + 1];
      if (next === "|") {
        // GFM §4.10: `\|` unescapes to literal `|` before inline code spans are parsed
        cur += "|";
        i++;
        continue;
      }
      if (next === "\\") {
        cur += codeTicks > 0 ? "\\\\" : "\\";
        i++;
        continue;
      }
    }
    if (ch === "`") {
      let run = 1;
      while (i + run < s.length && s[i + run] === "`") run++;
      if (codeTicks === 0) {
        // Only treat backtick run as an inline code span if a matching closing run exists on the row
        const closer = "`".repeat(run);
        let j = i + run;
        let hasCloser = false;
        while (j < s.length) {
          const hit = s.indexOf(closer, j);
          if (hit < 0) break;
          const prevTick = hit > 0 && s[hit - 1] === "`";
          const nextTick = hit + run < s.length && s[hit + run] === "`";
          if (!prevTick && !nextTick) {
            hasCloser = true;
            break;
          }
          j = hit + 1;
        }
        if (hasCloser) codeTicks = run;
      } else if (run === codeTicks) {
        codeTicks = 0;
      }
      cur += s.slice(i, i + run);
      i += run - 1;
    } else if (ch === "|" && codeTicks === 0) {
      cells.push(cur.trim());
      cur = "";
    } else {
      cur += ch;
    }
  }
  cells.push(cur.trim());
  return cells;
}

const URL_TOKEN_RE = /https?:\/\/[^\s<>()[\]"']+/gi;

function escapeProseSegment(seg: string): string {
  // Escape characters that Goldmark would otherwise re-parse as Markdown/HTML syntax on roundtrip.
  let out = seg
    .replace(/\\/g, "\\\\")
    .replace(/<(?=[A-Za-z!/?.])/g, "&lt;")
    .replace(/[*`]/g, "\\$&")
    .replace(/~~/g, "\\~\\~")
    .replace(/==/g, "\\=\\=")
    .replace(/(?<!\\)\^([^\s^\n[\]]+)\^/g, "\\^$1\\^")
    .replace(/(?<![~\\])~([^\s~\n/]+)~(?!~)/g, "\\~$1\\~");
  const lastClose = out.lastIndexOf("]");
  if (lastClose >= 0) {
    out = out.slice(0, lastClose).replace(/\[/g, "\\[") + out.slice(lastClose);
  }
  return out.replace(/\$(?=\S)/g, "\\$");
}

export function parseQuotePrefix(text: string): { prefixLen: number; markerCount: number } {
  let idx = 0;
  let markerCount = 0;
  while (idx < text.length) {
    let sp = 0;
    while (sp < 3 && idx + sp < text.length && text[idx + sp] === " ") sp++;
    if (idx + sp >= text.length || text[idx + sp] !== ">") break;
    idx += sp + 1;
    if (idx < text.length && /\s/.test(text[idx])) idx++;
    markerCount++;
  }
  return { prefixLen: idx, markerCount };
}

export function collapseNewlines(s: string, sep = " "): string {
  let nl = s.indexOf("\n");
  if (nl < 0) return s;
  let out = "";
  let pos = 0;
  while (nl >= 0) {
    let before = nl;
    while (before > pos) {
      const c = s.charCodeAt(before - 1);
      if (c === 32 || c === 9 || c === 13) before--;
      else break;
    }
    out += s.slice(pos, before) + sep;
    let after = nl + 1;
    while (after < s.length && /\s/.test(s[after])) after++;
    pos = after;
    nl = s.indexOf("\n", pos);
  }
  return out + s.slice(pos);
}

/**
 * Single-pass O(N) CSS context classifier:
 * 0 = inside comment/string, 1 = before '{', 2 = before ';'/'}'/')', 3 = EOF/unterminated
 */
export function computeCssContext(slice: string): Uint8Array {
  const ctx = new Uint8Array(slice.length);
  const stack: number[][] = [[]];
  let i = 0;
  while (i < slice.length) {
    const ch = slice[i];
    if (ch === "/" && slice[i + 1] === "*") {
      const end = slice.indexOf("*/", i + 2);
      if (end < 0) break;
      i = end + 2;
      continue;
    }
    if (ch === '"' || ch === "'") {
      let j = i + 1;
      while (j < slice.length) {
        if (slice[j] === "\\") {
          j += 2;
          continue;
        }
        if (slice[j] === ch || slice[j] === "\n") {
          j++;
          break;
        }
        j++;
      }
      i = j;
      continue;
    }
    if (ch === "(") {
      stack[stack.length - 1].push(i);
      stack.push([]);
    } else if (ch === ")") {
      if (stack.length > 1) {
        const inner = stack.pop()!;
        for (const idx of inner) ctx[idx] = 2;
      } else {
        stack[0].length = 0;
      }
    } else if (ch === "{") {
      const top = stack[stack.length - 1];
      for (const idx of top) ctx[idx] = 1;
      top.length = 0;
    } else if (ch === ";" || ch === "}") {
      while (stack.length > 1) {
        const inner = stack.pop()!;
        for (const idx of inner) ctx[idx] = 2;
      }
      for (const idx of stack[0]) ctx[idx] = 2;
      stack[0].length = 0;
    } else {
      stack[stack.length - 1].push(i);
    }
    i++;
  }
  while (stack.length > 0) {
    const rem = stack.pop()!;
    for (const idx of rem) ctx[idx] = 3;
  }
  return ctx;
}

/**
 * Single-pass O(N) CSS string scanner (single-line "..." and '...' with backslash escapes).
 */
export function findCssStringRanges(slice: string, used?: Uint8Array): Array<[number, number]> {
  const ranges: Array<[number, number]> = [];
  let i = 0;
  while (i < slice.length) {
    if (used && used[i]) {
      i++;
      continue;
    }
    const q = slice[i];
    if (q !== '"' && q !== "'") {
      i++;
      continue;
    }
    let altIdx = -1;
    let j = i + 1;
    let closed = false;
    while (j < slice.length) {
      const ch = slice[j];
      if (ch === "\n") break;
      if (ch === "\\") {
        if (j + 1 >= slice.length || "\n\r\u2028\u2029".includes(slice[j + 1])) {
          j++;
          break;
        }
        j += 2;
        continue;
      }
      if (ch === q) {
        j++;
        closed = true;
        break;
      }
      if (altIdx < 0 && (ch === '"' || ch === "'") && !used?.[j]) {
        altIdx = j;
      }
      j++;
    }
    if (closed) {
      ranges.push([i, j]);
      i = j;
    } else {
      i = altIdx >= 0 ? altIdx : j;
    }
  }
  return ranges;
}

export function escapeMarkdownProseText(text: string, isAtBlockStart = false): string {
  const clean = text.replace(/\u00a0/g, " ");
  if (!clean) return "";

  let out = "";
  let last = 0;
  URL_TOKEN_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = URL_TOKEN_RE.exec(clean)) !== null) {
    if (m.index > last) {
      out += escapeProseSegment(clean.slice(last, m.index));
    }
    out += m[0];
    last = m.index + m[0].length;
  }
  if (last < clean.length) {
    out += escapeProseSegment(clean.slice(last));
  }

  if (isAtBlockStart) {
    out = out
      .replace(/^(\s*)(#{1,6})(\s)/, "$1\\$2$3")
      .replace(/^(\s*)>/, "$1\\>")
      .replace(/^(\s*)([-+*])(\s)/, "$1\\$2$3")
      .replace(/^(\s*\d{1,9})([.)])(\s)/, "$1\\$2$3")
      .replace(/^(\s*)(:::)/, "$1\\$2")
      .replace(/^(\s*)(-{3,}|_{3,})(\s*)$/, "$1\\$2$3");
  }
  return out;
}

export function stripQuotePrefix(text: string, maxMarkers: number): { stripped: string; len: number } {
  if (maxMarkers <= 0) return { stripped: text, len: 0 };
  let idx = 0;
  let count = 0;
  while (count < maxMarkers && idx < text.length) {
    let sp = 0;
    while (sp < 3 && idx + sp < text.length && text[idx + sp] === " ") sp++;
    if (text[idx + sp] !== ">") break;
    idx += sp + 1;
    if (text[idx] === " ") idx++;
    count++;
  }
  return { stripped: text.slice(idx), len: idx };
}

export function detectIRCodeLang(
  linesText: string[],
  startLn: number,
  endLn: number,
  qDepth = 0
): { lang: string; fam: number; noHash: number } {
  if (startLn > endLn) return { lang: "", fam: 0, noHash: 0 };
  const sampleLines: string[] = [];
  let charBudget = 0;
  for (let k = startLn; k <= endLn && sampleLines.length < 32 && charBudget < 2400; k++) {
    const rawLine = linesText[k];
    const t = qDepth > 0 ? stripQuotePrefix(rawLine, qDepth).stripped : rawLine;
    if (t.trim()) {
      const capped = t.length > 400 ? t.slice(0, 400) : t;
      sampleLines.push(capped);
      charBudget += capped.length + 1;
    }
  }
  if (sampleLines.length === 0) return { lang: "", fam: 0, noHash: 0 };
  const sample = sampleLines.join("\n");
  const first = sampleLines[0].trim();

  if (/^(?:diff --git\b|---\s+\S|\+\+\+\s+\S|@@\s+-\d+)/m.test(sample)) {
    return { lang: "diff", fam: 6, noHash: 1 };
  }
  if (/^(?:<!DOCTYPE\s+html\b|<\?xml\b|<(?:html|head|body|div|section|template|svg|script|style|table|form|main|article)\b)/i.test(first)) {
    return { lang: first.startsWith("<?xml") ? "xml" : "html", fam: 4, noHash: 1 };
  }
  if ((first.startsWith("{") || first.startsWith("[")) && /"[^"\n]+"\s*:/.test(sample)) {
    return { lang: "json", fam: 3, noHash: 1 };
  }
  if (
    (/^[ \t]*SELECT\b/im.test(sample) && /\bFROM\b/i.test(sample)) ||
    /^[ \t]*(?:INSERT\s+INTO\b|UPDATE\s+\w+\s+SET\b|DELETE\s+FROM\b|CREATE\s+(?:TABLE|INDEX|VIEW|DATABASE)\b|ALTER\s+TABLE\b)/im.test(
      sample
    )
  ) {
    return { lang: "sql", fam: 7, noHash: 1 };
  }
  if (
    /\b(?:server\s*\{|location\s+[~^=*\s/]|proxy_pass\s+|fastcgi_pass\s+|upstream\s+\w+|listen\s+\d+|server_name\s+|absolute_redirect\s+(?:on|off)|proxy_set_header\s+)/.test(
      sample
    )
  ) {
    return { lang: "nginx", fam: 2, noHash: 0 };
  }
  if (
    /(?:^[ \t]*(?:@media|@keyframes|:root|\.[a-zA-Z_-][\w-]*[ \t]*[,{]|#[a-zA-Z_-][\w-]*[ \t]*[,{]))/m.test(sample) &&
    /(?:^[ \t]*|[;{][ \t]*)[a-z-]+[ \t]*:[ \t]*[^ \t;{}\n][^;{}\n]*;/m.test(sample)
  ) {
    return { lang: "css", fam: 5, noHash: 1 };
  }
  if (/\b(?:package\s+[a-z_]\w*|func\s+(?:\([^)]+\)\s*)?[A-Za-z_]\w*\s*\(|fmt\.Print|if\s+err\s*!=\s*nil\b)/.test(sample)) {
    return { lang: "go", fam: 1, noHash: 1 };
  }
  if (/\b(?:fn\s+[a-z_]\w*\s*[<(]|let\s+mut\s+|impl\b|pub\s+(?:fn|struct|enum|use)\b|println!\s*\()/.test(sample)) {
    return { lang: "rust", fam: 1, noHash: 1 };
  }
  if (/^[ \t]*syntax\s*=\s*["']proto[23]["']/m.test(sample) || /\b(?:message|service)\s+[A-Z]\w*\s*\{/.test(sample)) {
    return { lang: "proto", fam: 10, noHash: 1 };
  }
  if (/^<\?php\b/m.test(sample) || (/\$[A-Za-z_]\w*\s*=/.test(sample) && /->|::|\bfunction\b/.test(sample))) {
    return { lang: "php", fam: 1, noHash: 0 };
  }
  if (/^[ \t]*(?:query|mutation|subscription|fragment\s+\w+\s+on|type\s+\w+\s*(?:implements\s+\w+\s*)?\{)/m.test(sample)) {
    return { lang: "graphql", fam: 9, noHash: 0 };
  }
  if (/^[ \t]*#\s*(?:include|define|ifdef|ifndef|pragma)\b/m.test(sample) || /\b(?:std::|int\s+main\s*\()/.test(sample)) {
    return { lang: "cpp", fam: 1, noHash: 1 };
  }
  if (
    /\blocal\s+[A-Za-z_]\w*\s*=|^[ \t]*elseif\b[^\n]*\bthen\b/m.test(sample) ||
    (/^[ \t]*(?:local[ \t]+)?function\s+[A-Za-z_][\w.:]*\s*\(/m.test(sample) && /\bend\b/.test(sample))
  ) {
    return { lang: "lua", fam: 8, noHash: 1 };
  }
  if (/^[ \t]*(?:def\s+[A-Za-z_]\w*\s*\(|from\s+[A-Za-z_][\w.]*\s+import\s+|elif[ \t]+\S[^\n:]*:|if\s+__name__\s*==)/m.test(sample)) {
    return { lang: "python", fam: 2, noHash: 0 };
  }
  if (/\b(?:def\s+[a-z_]\w*[?!]?|attr_accessor\b|require\s+['"]|puts\s+|elsif\b)/.test(sample) && /\bend\b/.test(sample)) {
    return { lang: "ruby", fam: 2, noHash: 0 };
  }
  if (
    /\b(?:const\s+\w+\s*=|let\s+\w+\s*=|export\s+(?:default|function|const|interface|type|class)\b|console\.\w+\()|^[ \t]*import[ \t]+(?:[^\s;\n]+[ \t]+)+from[ \t]+['"]/m.test(
      sample
    )
  ) {
    return { lang: /\b(?:interface\s+\w+|:\s*(?:string|number|boolean|Record|ReadonlyArray)\b)/.test(sample) ? "typescript" : "javascript", fam: 1, noHash: 1 };
  }
  if (/^(?:#!\/(?:usr\/bin\/env\s+)?(?:ba|z)?sh\b|[ \t]*(?:sudo|apt|apt-get|yum|dnf|docker|kubectl|systemctl|git|npm|pnpm|curl|wget|chmod|chown|mkdir|export)[ \t]+)/m.test(sample)) {
    return { lang: "bash", fam: 2, noHash: 0 };
  }
  if (
    !sample.includes("{") &&
    !sample.includes(";") &&
    sampleLines.filter((l) => /^\s*(?:-\s+)?[A-Za-z0-9_.-]+:\s*(?:\S|$)/.test(l)).length >= 2
  ) {
    return { lang: "yaml", fam: 2, noHash: 0 };
  }
  return { lang: "", fam: 0, noHash: 0 };
}
