import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { gunzipSync } from "node:zlib";
import {
  CODE_BLOCK_LANGS,
  HASH_COMMENT_LANGS,
  DASH_COMMENT_LANGS,
  NO_HASH_COMMENT_LANGS,
  IR_CODE_RE,
  IR_FN_REF_RE,
  IR_INLINE_MATH_RE,
  IR_ML_ANCHOR_RE,
  resolveFenceLangFamily,
  computeFenceDelim,
  hasUnescapedTrailingPipe,
  splitGfmTableRow,
  escapeMarkdownProseText,
  parseQuotePrefix,
  collapseNewlines,
  computeCssContext,
  findCssStringRanges,
  detectIRCodeLang,
} from "../src/components/markdownHelpers.ts";

test("resolveFenceLangFamily classifies Markdown (11), Ada/Haskell/VHDL (12), and core server lexers", () => {
  assert.equal(resolveFenceLangFamily("go"), 1);
  assert.equal(resolveFenceLangFamily("typescript"), 1);
  assert.equal(resolveFenceLangFamily("bash"), 2);
  assert.equal(resolveFenceLangFamily("caddyfile"), 2);
  assert.equal(resolveFenceLangFamily("json"), 3);
  assert.equal(resolveFenceLangFamily("html"), 4);
  assert.equal(resolveFenceLangFamily("scss"), 5);
  assert.equal(resolveFenceLangFamily("diff"), 6);
  assert.equal(resolveFenceLangFamily("sql"), 7);
  assert.equal(resolveFenceLangFamily("lua"), 8);
  assert.equal(resolveFenceLangFamily("graphql"), 9);
  assert.equal(resolveFenceLangFamily("proto"), 10);
  assert.equal(resolveFenceLangFamily("md"), 11);
  assert.equal(resolveFenceLangFamily("markdown"), 11);
  assert.equal(resolveFenceLangFamily("haskell"), 12);
  assert.equal(resolveFenceLangFamily("hs"), 12);
  assert.equal(resolveFenceLangFamily("ada"), 12);
  assert.equal(resolveFenceLangFamily("vhdl"), 12);
  assert.equal(resolveFenceLangFamily("plaintext"), 0);

  // Lexer list parity checks
  const langs = new Set(CODE_BLOCK_LANGS.map((l) => l.value));
  for (const expected of ["go", "markdown", "haskell", "ada", "vhdl", "prisma", "caddy", "nginx", "powershell"]) {
    assert.ok(langs.has(expected), `missing ${expected} in CODE_BLOCK_LANGS`);
  }
  assert.ok(HASH_COMMENT_LANGS.has("bash"));
  assert.ok(DASH_COMMENT_LANGS.has("haskell"));
  assert.ok(NO_HASH_COMMENT_LANGS.has("markdown"));
});

test("computeFenceDelim chooses delimiter longer than any backtick run inside body", () => {
  assert.equal(computeFenceDelim("const x = 1;"), "```");
  assert.equal(computeFenceDelim("inline `code` only"), "```");
  assert.equal(computeFenceDelim("```md\nnested\n```"), "````");
  assert.equal(computeFenceDelim("`````\n5 ticks\n`````"), "``````");
});

test("splitGfmTableRow and hasUnescapedTrailingPipe handle inline code pipes and escaped pipes", () => {
  assert.equal(hasUnescapedTrailingPipe("| a | b |"), true);
  assert.equal(hasUnescapedTrailingPipe("| a | b \\|"), false);
  assert.equal(hasUnescapedTrailingPipe("| a | b \\\\|"), true);

  assert.deepEqual(splitGfmTableRow("| Col A | `a | b` | c \\| d |"), ["Col A", "`a | b`", "c | d"]);
  assert.deepEqual(splitGfmTableRow("left | ``x || y`` | right"), ["left", "``x || y``", "right"]);
  assert.deepEqual(splitGfmTableRow("| unclosed `tick | next |"), ["unclosed `tick", "next"]);
});

test("escapeMarkdownProseText escapes syntax contextually while preserving URLs", () => {
  // Literal Markdown delimiters inside normal prose
  assert.equal(escapeMarkdownProseText("a * b * c [note]", false), "a \\* b \\* c \\[note]");
  assert.equal(escapeMarkdownProseText("price $100 and ~~old~~", false), "price \\$100 and \\~\\~old\\~\\~");
  assert.equal(escapeMarkdownProseText("H~2~O and x^2^", false), "H\\~2\\~O and x\\^2\\^");

  // Block-start markers only escaped when isAtBlockStart = true
  assert.equal(escapeMarkdownProseText("# Not a heading", true), "\\# Not a heading");
  assert.equal(escapeMarkdownProseText("# mid-paragraph", false), "# mid-paragraph");
  assert.equal(escapeMarkdownProseText("1. Not a list", true), "1\\. Not a list");
  assert.equal(escapeMarkdownProseText("> Not a quote", true), "\\> Not a quote");
  assert.equal(escapeMarkdownProseText("---", true), "\\---");

  // URLs must NOT have query/tilde/asterisk/brackets corrupted inside URL token
  assert.equal(
    escapeMarkdownProseText("visit https://example.com/~user/*path* and *bold*", false),
    "visit https://example.com/~user/*path* and \\*bold\\*"
  );
});

test("IR_CODE_RE matches inline code spans accurately and avoids quadratic ReDoS on long backtick runs", () => {
  const matchSpans = (s: string) => {
    IR_CODE_RE.lastIndex = 0;
    const out: string[] = [];
    let m: RegExpExecArray | null;
    while ((m = IR_CODE_RE.exec(s)) !== null) out.push(m[2]);
    return out;
  };
  assert.deepEqual(matchSpans("use `foo` and ``bar `baz` qux`` here"), ["foo", "bar `baz` qux"]);

  const pathological = "`".repeat(10_000);
  const t0 = performance.now();
  const hits = matchSpans(pathological);
  const elapsedMs = performance.now() - t0;
  assert.deepEqual(hits, []);
  assert.ok(elapsedMs < 50, `IR_CODE_RE took ${elapsedMs.toFixed(2)}ms on 10k backticks (limit 50ms)`);
});

test("markdownHelpers maintains full lexer and alias parity with internal/render/lexers_spec.gz", () => {
  const here = dirname(fileURLToPath(import.meta.url));
  const specPath = resolve(here, "../../../internal/render/lexers_spec.gz");
  const raw = gunzipSync(readFileSync(specPath)).toString("utf8");
  const records = raw.split("\x1e").filter(Boolean);
  assert.ok(records.length >= 20, "expected >= 20 lexer records in lexers_spec.gz");

  const codeBlockLangSet = new Set(CODE_BLOCK_LANGS.map((l) => l.value));
  for (const rec of records) {
    const fields = rec.split("\x1f");
    const aliases = (fields[13] || "").split(",").filter(Boolean);
    assert.ok(aliases.length > 0);
    assert.ok(
      aliases.some((a) => codeBlockLangSet.has(a)),
      `expected at least one of [${aliases.join(", ")}] in CODE_BLOCK_LANGS`
    );
    for (const alias of aliases) {
      assert.ok(resolveFenceLangFamily(alias) > 0, `alias "${alias}" missing from FENCE_FAM_GROUPS`);
    }
  }

  // Contract #3 + alias comment-style parity
  for (const lang of [
    "tsx",
    "jsx",
    "objective-c",
    "bat",
    "mermaid",
    "kotlin",
    "swift",
    "prisma",
    "r",
    "perl",
    "dockerfile",
    "powershell",
    "makefile",
    "ini",
    "caddy",
    "xml",
    "vue",
    "svelte",
    "less",
    "markdown",
    "haskell",
  ]) {
    assert.ok(codeBlockLangSet.has(lang), `Contract #3 lang "${lang}" missing from CODE_BLOCK_LANGS`);
  }
  assert.equal(resolveFenceLangFamily("c#"), 1);
  assert.equal(resolveFenceLangFamily("objective-c"), 1);
  assert.equal(resolveFenceLangFamily("objc"), 1);
  assert.equal(resolveFenceLangFamily("bat"), 2);
  assert.equal(resolveFenceLangFamily("cmd"), 2);
  assert.ok(NO_HASH_COMMENT_LANGS.has("objective-c"));
  assert.ok(NO_HASH_COMMENT_LANGS.has("objc"));
  assert.ok(NO_HASH_COMMENT_LANGS.has("bat"));
  assert.ok(NO_HASH_COMMENT_LANGS.has("cmd"));
  assert.ok(!HASH_COMMENT_LANGS.has("bat"));
  assert.ok(!HASH_COMMENT_LANGS.has("cmd"));
});

test("parseQuotePrefix, collapseNewlines, computeCssContext, and escapeMarkdownProseText run in linear time", () => {
  assert.deepEqual(parseQuotePrefix("hello"), { prefixLen: 0, markerCount: 0 });
  assert.deepEqual(parseQuotePrefix("  >   > hi"), { prefixLen: 8, markerCount: 2 });
  assert.deepEqual(parseQuotePrefix("    > not quote"), { prefixLen: 0, markerCount: 0 });

  assert.equal(collapseNewlines("a \t\r\n\n  b\n c"), "a b c");
  assert.equal(collapseNewlines("a \n  b", "<br>"), "a<br>b");

  const css = "/* .x */ .cls:hover { color: rgb(1, 2); } .unclosed";
  const ctx = computeCssContext(css);
  assert.equal(ctx[3], 0); // inside comment
  assert.equal(ctx[css.indexOf(".cls")], 1); // selector before '{'
  assert.equal(ctx[css.indexOf("color")], 2); // declaration before ';'
  assert.equal(ctx[css.indexOf("rgb")], 2); // before ';'
  assert.equal(ctx[css.indexOf(".unclosed")], 3); // EOF

  const brackets = "[".repeat(10_000);
  const t0 = performance.now();
  assert.equal(escapeMarkdownProseText(brackets, false), brackets);
  assert.equal(escapeMarkdownProseText(brackets + "]", false), "\\[".repeat(10_000) + "]");
  const tEsc = performance.now() - t0;
  assert.ok(tEsc < 25, `escapeMarkdownProseText took ${tEsc.toFixed(2)}ms (limit 25ms)`);

  const quotes = "> ".repeat(10_000) + "x";
  const t1 = performance.now();
  const qp = parseQuotePrefix(quotes);
  const tQp = performance.now() - t1;
  assert.equal(qp.markerCount, 10_000);
  assert.ok(tQp < 25, `parseQuotePrefix took ${tQp.toFixed(2)}ms (limit 25ms)`);

  const nls = " \t\n ".repeat(5_000);
  const t2 = performance.now();
  collapseNewlines(nls, " ");
  const tNl = performance.now() - t2;
  assert.ok(tNl < 25, `collapseNewlines took ${tNl.toFixed(2)}ms (limit 25ms)`);

  const badCss = "a-".repeat(5_000) + "/*".repeat(2_000);
  const t3 = performance.now();
  computeCssContext(badCss);
  const tCss = performance.now() - t3;
  assert.ok(tCss < 25, `computeCssContext took ${tCss.toFixed(2)}ms (limit 25ms)`);

  const allOpenComments = "/*".repeat(5_000);
  const t4 = performance.now();
  const cmtCtx = computeCssContext(allOpenComments);
  const tCssCmt = performance.now() - t4;
  assert.equal(cmtCtx[0], 0);
  assert.equal(cmtCtx[cmtCtx.length - 1], 0);
  assert.ok(tCssCmt < 25, `computeCssContext('/*'.repeat(5000)) took ${tCssCmt.toFixed(2)}ms (limit 25ms)`);
});

test("IR_INLINE_MATH_RE and IR_FN_REF_RE match valid syntax and avoid ReDoS", () => {
  const collectMath = (s: string) => {
    IR_INLINE_MATH_RE.lastIndex = 0;
    const out: string[] = [];
    let m: RegExpExecArray | null;
    while ((m = IR_INLINE_MATH_RE.exec(s)) !== null) out.push(m[2] ?? m[1]);
    return out;
  };
  assert.deepEqual(collectMath(String.raw`\(a + b\) and \(a \\ b\) and \(a \\\b\) and $x + y$`), [
    "a + b",
    "a \\\\ b",
    "a \\\\\\b",
    "x + y",
  ]);
  assert.deepEqual(collectMath(String.raw`\\(not math\) and \(unclosed`), []);

  for (const [label, input] of [
    ["\\a", "\\(" + "\\a".repeat(10_000)],
    ["\\\\(", "\\(" + "\\\\(".repeat(10_000)],
    [" \\(", "\\(" + " \\(".repeat(10_000)],
  ] as const) {
    const t0 = performance.now();
    const hits = collectMath(input);
    const elapsedMs = performance.now() - t0;
    assert.deepEqual(hits, []);
    assert.ok(elapsedMs < 25, `IR_INLINE_MATH_RE (${label}) took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }

  const collectFnRefs = (s: string) => {
    IR_FN_REF_RE.lastIndex = 0;
    const out: string[] = [];
    let m: RegExpExecArray | null;
    while ((m = IR_FN_REF_RE.exec(s)) !== null) out.push(m[1]);
    return out;
  };
  assert.deepEqual(collectFnRefs("see[^1] and [^note-a]"), ["1", "note-a"]);
  const tFn = performance.now();
  assert.deepEqual(collectFnRefs("[^a".repeat(3334)), []);
  const fnMs = performance.now() - tFn;
  assert.ok(fnMs < 25, `IR_FN_REF_RE took ${fnMs.toFixed(2)}ms (limit 25ms)`);
});

test("detectIRCodeLang preserves language heuristics and runs in linear time", () => {
  assert.equal(detectIRCodeLang([""], 0, 0).lang, "");
  assert.equal(detectIRCodeLang(["----"], 0, 0).lang, "");
  assert.equal(detectIRCodeLang([".btn { color: red; }"], 0, 0).lang, "css");
  assert.equal(detectIRCodeLang([".btn {", "  color: red;", "}"], 0, 2).lang, "css");
  assert.equal(detectIRCodeLang(["SELECT id FROM posts"], 0, 0).lang, "sql");
  assert.equal(detectIRCodeLang(["import x from 'y';", "const a = 1;"], 0, 1).lang, "javascript");
  assert.equal(detectIRCodeLang(["const x = 1;", "interface Foo { a: number }"], 0, 1).lang, "typescript");
  assert.equal(detectIRCodeLang(["def foo():", "  return 1"], 0, 1).lang, "python");
  assert.equal(detectIRCodeLang(["package main", "func main() {}"], 0, 1).lang, "go");
  assert.equal(detectIRCodeLang(["diff --git a/x b/x", "@@ -1,1 +1,1 @@"], 0, 1).lang, "diff");
  assert.equal(detectIRCodeLang(["local x = 1", "elseif x then", "end"], 0, 2).lang, "lua");
  assert.equal(detectIRCodeLang(["function foo(x)", "  return x", "end"], 0, 2).lang, "lua");
  assert.equal(detectIRCodeLang(["function f() {}", "const n = 1;"], 0, 1).lang, "javascript");
  assert.equal(detectIRCodeLang(["import { a, b } from 'c';"], 0, 0).lang, "javascript");
  assert.equal(detectIRCodeLang(["elif x:", "  pass"], 0, 1).lang, "python");

  for (const [label, lines] of [
    ["css a:b", [".cls {", "a:b".repeat(3334), "}"]],
    ["css colon spaces", [".cls {", "a:" + " ".repeat(20000), "}"]],
    ["js import", ["import x ".repeat(1112)]],
    ["js import spaces 2k", ["import " + " ".repeat(2000)]],
    ["js import word spaces 1k", ["import a ".repeat(1000)]],
    ["py elif spaces 20k", ["elif " + " ".repeat(20000)]],
    ["lua elseif", ["elseif x ".repeat(1112)]],
    ["lua function", ["function a(".repeat(1112)]],
  ] as const) {
    const t0 = performance.now();
    const res = detectIRCodeLang([...lines], 0, lines.length - 1, 0);
    const elapsedMs = performance.now() - t0;
    assert.equal(res.lang, "");
    assert.ok(elapsedMs < 25, `detectIRCodeLang (${label}) took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }
});

test("findCssStringRanges matches CSS strings accurately and avoids ReDoS on escaped quotes", () => {
  const css = '/* "ignored" */ .a { content: "hi \\"there\\""; font: \'a\\\'b\'; } "unclosed\n"ok"';
  const used = new Uint8Array(css.length);
  for (let i = 0; i < css.indexOf("*/") + 2; i++) used[i] = 1;
  const ranges = findCssStringRanges(css, used).map(([s, e]) => css.slice(s, e));
  assert.deepEqual(ranges, ['"hi \\"there\\""', "'a\\'b'", '"ok"']);

  assert.deepEqual(findCssStringRanges('"" \'\' "a\\\nb"'), [
    [0, 2],
    [3, 5],
  ]);

  // Cross-quote unclosed-line recovery & backslash before \r/\u2028/\u2029
  const cross = '.broken { content: "unclosed; } .ok { font: \'Inter\'; }';
  const crossRanges = findCssStringRanges(cross);
  assert.deepEqual(crossRanges, [[44, 51]]);
  assert.deepEqual(
    crossRanges.map(([s, e]) => cross.slice(s, e)),
    ["'Inter'"]
  );
  const crossRev = ".broken { font: 'unclosed; } .ok { content: \"hi\"; }";
  assert.deepEqual(
    findCssStringRanges(crossRev).map(([s, e]) => crossRev.slice(s, e)),
    ['"hi"']
  );
  for (const sep of ["\n", "\r", "\u2028", "\u2029"]) {
    const s = `"a\\${sep}b; "ok"`;
    assert.deepEqual(
      findCssStringRanges(s).map(([a, b]) => s.slice(a, b)),
      ['"ok"']
    );
  }

  // Differential check against CSS_STR_RE on 1000 seeded CSS cases
  const CSS_STR_RE = /"(?:\\.|[^"\\\n])*"|'(?:\\.|[^'\\\n])*'/g;
  const toks = ["color:", " ", ";", "{", "}", "\n", "\r", '"foo"', "'bar'", '"open', "'open", '"a\\"b"', "'a\\'b'"];
  let seed = 1337;
  const rnd = (n: number) => {
    seed = (seed * 1664525 + 1013904223) >>> 0;
    return seed % n;
  };
  for (let c = 0; c < 1000; c++) {
    let s = "";
    for (let k = 0; k < 2 + (rnd(8)); k++) s += toks[rnd(toks.length)];
    CSS_STR_RE.lastIndex = 0;
    const expected: Array<[number, number]> = [];
    let m: RegExpExecArray | null;
    while ((m = CSS_STR_RE.exec(s)) !== null) expected.push([m.index, m.index + m[0].length]);
    assert.deepEqual(findCssStringRanges(s), expected);
  }

  for (const [label, input] of [
    ["dquotes_esc", '"a\\'.repeat(6000)],
    ["squotes_esc", "'a\\".repeat(6000)],
  ] as const) {
    const t0 = performance.now();
    const hits = findCssStringRanges(input);
    const elapsedMs = performance.now() - t0;
    assert.deepEqual(hits, []);
    assert.ok(elapsedMs < 25, `findCssStringRanges (${label}) took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }
});

test("IR_ML_ANCHOR_RE matches multi-line anchors cleanly and avoids ReDoS on unclosed <a> tags", () => {
  const collectAnchors = (s: string) => {
    IR_ML_ANCHOR_RE.lastIndex = 0;
    const out: Array<[string, string, string]> = [];
    let m: RegExpExecArray | null;
    while ((m = IR_ML_ANCHOR_RE.exec(s)) !== null) {
      out.push([m[1], m[2], m[3]]);
    }
    return out;
  };

  assert.deepEqual(collectAnchors('<a href="https://example.com">\n  <b>link</b>\n</a>'), [
    ['<a href="https://example.com">', "\n  <b>link</b>\n", "</a>"],
  ]);
  assert.deepEqual(collectAnchors('<a href="x">outer<a href="y">inner</a>'), [
    ['<a href="y">', "inner", "</a>"],
  ]);
  assert.deepEqual(collectAnchors('<A href=\'https://example.org\' title="ext">\n  <abbr>AI</abbr>\n</A>'), [
    ['<A href=\'https://example.org\' title="ext">', "\n  <abbr>AI</abbr>\n", "</A>"],
  ]);

  for (const [label, input, expectedCount] of [
    ["a_href_foo_6k", '<a href="x">foo'.repeat(6000), 0],
    ["a_open_20k", "<a>".repeat(20000), 0],
    ["a_closed_then_10k", "<a>x\n</a>" + "<a>".repeat(10000), 1],
  ] as const) {
    const t0 = performance.now();
    const hits = collectAnchors(input);
    const elapsedMs = performance.now() - t0;
    assert.equal(hits.length, expectedCount);
    assert.ok(elapsedMs < 25, `IR_ML_ANCHOR_RE (${label}) took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }
});
