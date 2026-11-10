import { readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig, Plugin } from "vite";

const __dirname = dirname(fileURLToPath(import.meta.url));

function compactLocalesPlugin(): Plugin {
  const virtualId = "virtual:volo-locales";
  const resolvedId = "\0" + virtualId;
  const localesDir = join(__dirname, "src", "locales");

  function readLocaleCatalogs() {
    const files = readdirSync(localesDir)
      .filter((f) => f.endsWith(".json"))
      .sort();
    const keySet = new Set<string>();
    const items = files.map((f) => {
      const raw = JSON.parse(readFileSync(join(localesDir, f), "utf8")) as Record<string, string>;
      for (const k of Object.keys(raw)) keySet.add(k);
      const code = raw["meta.code"] || f.replace(/\.json$/i, "");
      const name = raw["meta.name"] || code;
      const dir = raw["meta.dir"] || "ltr";
      return { code, name, dir, raw };
    });
    const keys = Array.from(keySet).sort();
    return { items, keys };
  }

  let cachedKeyMap: Map<string, number> | null = null;
  function getKeyIndexMap() {
    if (!cachedKeyMap) {
      const { keys } = readLocaleCatalogs();
      cachedKeyMap = new Map<string, number>();
      keys.forEach((k, idx) => cachedKeyMap!.set(k, idx));
    }
    return cachedKeyMap;
  }

  return {
    name: "volo-compact-locales",
    buildStart() {
      cachedKeyMap = null;
    },
    resolveId(id) {
      if (id === virtualId) return resolvedId;
      return null;
    },
    load(id) {
      if (id !== resolvedId) return null;
      const { items, keys } = readLocaleCatalogs();
      const metas = items.map(({ code, name, dir }) => ({ code, name, dir }));
      let prevKey = "";
      const packedKeys = keys.map((k) => {
        if (k.includes("~")) throw new Error(`Locale key must not contain ~: ${k}`);
        let shared = 0;
        while (shared < prevKey.length && shared < k.length && prevKey[shared] === k[shared] && shared < 35) {
          shared++;
        }
        prevKey = k;
        return shared.toString(36) + k.slice(shared);
      });
      return (
        `export const builtinLocales = ${JSON.stringify(metas)};\n` +
        `let _p = "";\n` +
        `export const keys = ${JSON.stringify(packedKeys.join("~"))}.split("~").map((s) => (_p = _p.slice(0, parseInt(s[0], 36)) + s.slice(1)));\n`
      );
    },
    transform(code, id) {
      const cleanId = id.split("?")[0].replaceAll("\\", "/");
      if (!cleanId.includes("/src/") || (!cleanId.endsWith(".ts") && !cleanId.endsWith(".tsx")) || cleanId.endsWith("/src/i18n.ts")) {
        return null;
      }
      const keyMap = getKeyIndexMap();
      let changed = false;
      const out = code.replace(/(?<![A-Za-z0-9_$?.])t\(\s*(["'])([A-Za-z0-9_.-]+)\1(?=\s*[,)])/g, (full, _q, key: string) => {
        const idx = keyMap.get(key);
        if (idx === undefined) return full;
        changed = true;
        return `t(${idx}`;
      });
      return changed ? { code: out, map: null } : null;
    },
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = (req.url || "").split("?")[0];
        const m = /\/i18n\/([a-zA-Z0-9_-]+)\.json$/.exec(url);
        if (!m) return next();
        const { items, keys } = readLocaleCatalogs();
        const found = items.find((it) => it.code === m[1]);
        if (!found) return next();
        const col = keys.map((k) => found.raw[k] ?? "");
        res.setHeader("Content-Type", "application/json; charset=utf-8");
        res.end(JSON.stringify(col));
      });
    },
    generateBundle() {
      const { items, keys } = readLocaleCatalogs();
      for (const { code, raw } of items) {
        const col = keys.map((k) => raw[k] ?? "");
        this.emitFile({
          type: "asset",
          fileName: `i18n/${code}.json`,
          source: JSON.stringify(col),
        });
      }
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
  css: {
    transformer: "lightningcss",
    lightningcss: {
      targets: {
        safari: (17 << 16) | (5 << 8),
        chrome: 124 << 16,
        firefox: 125 << 16,
      },
    },
  },
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
    rolldownOptions: {
      output: {
        entryFileNames: "assets/index-[hash].js",
        chunkFileNames: "assets/editor-[hash].js",
      },
    },
  },
});
