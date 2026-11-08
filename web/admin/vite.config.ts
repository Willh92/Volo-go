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
      const columns = tables.map((t) => keys.map((k) => t.dict[k] ?? ""));

      return (
        `export const codes = ${JSON.stringify(codes)};\n` +
        `export const keys = ${JSON.stringify(keys)};\n` +
        `export const columns = ${JSON.stringify(columns)};\n`
      );
    },
  };
}

export default defineConfig({
  base: "./",
  plugins: [compactLocalesPlugin()],
  esbuild: {
    jsx: "automatic",
    jsxImportSource: "preact",
    legalComments: "none",
  },
  build: {
    outDir: "../../internal/adminui/dist",
    emptyOutDir: true,
    sourcemap: false,
    target: "es2022",
    cssCodeSplit: false,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes("@lezer")) return "cm6-lezer";
          if (id.includes("@codemirror") || id.includes("CM6EditorCore")) return "cm6-editor";
          return undefined;
        },
      },
    },
  },
});
