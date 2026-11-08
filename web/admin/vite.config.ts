import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig, Plugin } from "vite";

const __dirname = dirname(fileURLToPath(import.meta.url));

function compactLocalesPlugin(): Plugin {
  const virtualId = "virtual:volo-locales";
  const resolvedId = "\0" + virtualId;
  const localesDir = join(__dirname, "src", "locales");

  return {
    name: "volo-compact-locales",
    resolveId(id) {
      if (id === virtualId) return resolvedId;
      return null;
    },
    load(id) {
      if (id !== resolvedId) return null;
      const files = readdirSync(localesDir)
        .filter((f) => f.endsWith(".json"))
        .sort();
      const tables: Array<{ code: string; dict: Record<string, string> }> = [];
      const keySet = new Set<string>();

      for (const f of files) {
        const raw = JSON.parse(readFileSync(join(localesDir, f), "utf8")) as Record<string, string>;
        const code = raw["meta.code"] || f.replace(/\.json$/i, "");
        tables.push({ code, dict: raw });
        for (const k of Object.keys(raw)) keySet.add(k);
      }

      const keys = Array.from(keySet).sort();
      const codes = tables.map((t) => t.code);
      let prevKey = "";
      const packedKeys = keys.map((k) => {
        if (k.includes("\t")) throw new Error(`Locale key must not contain TAB: ${k}`);
        let shared = 0;
        while (shared < prevKey.length && shared < k.length && prevKey[shared] === k[shared] && shared < 35) {
          shared++;
        }
        prevKey = k;
        return shared.toString(36) + k.slice(shared);
      });
      const baseCol = tables[0] ? keys.map((k) => tables[0].dict[k] ?? "") : [];
      const columns = tables.map((t, ti) =>
        keys.map((k, ki) => {
          const v = t.dict[k] ?? "";
          if (v.includes("\t") || v === "\x01") {
            throw new Error(`Locale value must not contain TAB or SOH: ${k}`);
          }
          if (ti > 0 && v !== "" && v === baseCol[ki]) return "\x01";
          return v;
        }),
      );

      return (
        `export const codes = ${JSON.stringify(codes)};\n` +
        `let _p = "";\n` +
        `export const keys = ${JSON.stringify(packedKeys.join("\t"))}.split("\\t").map((s) => (_p = _p.slice(0, parseInt(s[0], 36)) + s.slice(1)));\n` +
        `const _c0 = ${JSON.stringify((columns[0] || []).join("\t"))}.split("\\t");\n` +
        `export const columns = [_c0,${columns
          .slice(1)
          .map((c) => `${JSON.stringify(c.join("\t"))}.split("\\t").map((v,i)=>v==="\\x01"?_c0[i]:v)`)
          .join(",")}];\n`
      );
    },
  };
}

// @codemirror/commands imports indentation/tree helpers from @codemirror/language and @lezer/common,
// dragging in the full Lezer parser runtime even when no Language extension is registered.
// Provide exact pure-JS equivalents for plain/custom-decorated CM6 editors.
function lightweightCM6LanguageShimPlugin(): Plugin {
  const langId = "\0virtual:cm6-lang-shim";
  const lezerId = "\0virtual:lezer-common-shim";
  return {
    name: "volo-cm6-lang-shim",
    enforce: "pre",
    resolveId(id) {
      if (id === "@codemirror/language") return langId;
      if (id === "@lezer/common") return lezerId;
      return null;
    },
    load(id) {
      if (id === lezerId) {
        return `export const NodeProp = { closedBy: {}, openedBy: {} };\n`;
      }
      if (id === langId) {
        return `
import { Facet, countColumn } from "@codemirror/state";
export const indentUnit = Facet.define({ combine: (vals) => vals[0] ?? "  " });
export function getIndentUnit(state) {
  const unit = state.facet(indentUnit);
  return unit.charCodeAt(0) === 9 ? state.tabSize * unit.length : unit.length;
}
export function indentString(state, cols) {
  const unit = state.facet(indentUnit);
  let result = "";
  if (unit.charCodeAt(0) === 9) {
    while (cols >= state.tabSize) { result += "\\t"; cols -= state.tabSize; }
  }
  for (let i = 0; i < cols; i++) result += " ";
  return result;
}
export class IndentContext {
  constructor(state) { this.state = state; this.unit = getIndentUnit(state); }
  lineIndent(pos) {
    const line = this.state.doc.lineAt(pos);
    return countColumn(/^\\s*/.exec(line.text)[0], this.state.tabSize);
  }
}
export function getIndentation() { return null; }
export function matchBrackets() { return null; }
const emptyNode = {
  name: "", from: 0, to: 0, parent: null, firstChild: null, nextSibling: null, prevSibling: null,
  childBefore: () => null, childAfter: () => null,
  type: { prop: () => undefined },
};
const emptyStack = { node: emptyNode, next: null };
export function syntaxTree() {
  return { resolveInner: () => emptyNode, resolveStack: () => emptyStack, iterate: () => {} };
}
`;
      }
      return null;
    },
  };
}

export default defineConfig({
  base: "./",
  plugins: [compactLocalesPlugin(), lightweightCM6LanguageShimPlugin()],
  oxc: {
    jsx: {
      runtime: "classic",
      pragma: "__P.h",
      pragmaFrag: "__P.Fragment",
    },
    jsxInject: `import * as __P from "preact";`,
  },
  build: {
    outDir: "../../internal/adminui/dist",
    emptyOutDir: true,
    sourcemap: false,
    target: "es2022",
    cssCodeSplit: false,
    modulePreload: false,
    chunkSizeWarningLimit: 1000,
  },
});
