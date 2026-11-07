import { defineConfig } from "vite";

export default defineConfig({
  base: "./",
  esbuild: {
    jsx: "automatic",
    jsxImportSource: "preact",
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
          if (id.includes("@lezer")) {
            return "cm6-lezer";
          }
          if (id.includes("@codemirror")) {
            return "cm6-editor";
          }
          return undefined;
        },
      },
    },
  },
});
