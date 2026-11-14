#!/usr/bin/env node
// Deterministic AST-grade minifier for Volo-go plugin packages (.js, .css, .json).
//
// Reuses `vite` (`minifySync`) and `lightningcss` (`transform`) installed in `web/admin`
// with zero extra npm dependencies:
// - Browser scripts (`assets/**/*.js`): ES2018 AST minification + local identifier mangling
//   (preserving concise MIT attribution banner when present).
// - Host JSVM entry scripts (`main.js` / `manifest.entry`): ES2015 AST minification with
//   `mangle.toplevel = false` so global hook functions and JSVM syntax boundaries stay 100% intact.
// - Stylesheets (`**/*.css`): LightningCSS AST minification.
// - JSON descriptors (`**/*.json`): Compact single-line UTF-8 serialization.
//
// Modes:
//   1) Batch manifest mode (invoked once by `.github/scripts/package-plugin.py`):
//      node .github/scripts/minify-plugin-assets.mjs --batch <batch.json>
//   2) Directory mode (for release staging, e.g. `plugins/sensitive-words`):
//      node .github/scripts/minify-plugin-assets.mjs --dir <srcDir> [--out-dir <dstDir>]

import { copyFileSync, existsSync, lstatSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, extname, join, posix, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const repoRoot = resolve(fileURLToPath(new URL("../..", import.meta.url)));
const adminPkgPath = join(repoRoot, "web", "admin", "package.json");
if (!existsSync(adminPkgPath)) {
  throw new Error(`Missing web/admin/package.json at ${adminPkgPath}`);
}

const adminRequire = createRequire(pathToFileURL(adminPkgPath).href);
const { minifySync } = adminRequire("vite");
const { transform: transformCSS } = adminRequire("lightningcss");

const CSS_TARGETS = {
  safari: (17 << 16) | (5 << 8),
  chrome: 124 << 16,
  firefox: 125 << 16,
  edge: 124 << 16,
};

const EXCLUDED_NAMES = new Set([".ds_store", "thumbs.db", "desktop.ini"]);

function normalizeRel(relPath) {
  return String(relPath || "")
    .replace(/\\/g, "/")
    .replace(/^\/+/, "");
}

function minifyPluginBuffer(relPosix, rawBuf, entryRelPosix) {
  const rel = normalizeRel(relPosix);
  const ext = posix.extname(rel).toLowerCase();

  if (ext === ".js") {
    const src = rawBuf.toString("utf8").replace(/\r\n/g, "\n");
    const isEntry = rel === normalizeRel(entryRelPosix || "main.js");
    const hasMitNotice = !isEntry && /MIT License:\s*see LICENSE/i.test(src.slice(0, 400));
    const out = minifySync(rel, src, {
      target: isEntry ? "es2015" : "es2018",
      mangle: { toplevel: false },
    });
    if (!out || typeof out.code !== "string" || (out.errors && out.errors.length > 0)) {
      const msg = out?.errors?.map((e) => e.message || String(e)).join("; ") || "unknown minifySync error";
      throw new Error(`JS minification failed for ${rel}: ${msg}`);
    }
    const prefix = hasMitNotice ? "/*! MIT License: see LICENSE */\n" : "";
    return Buffer.from(prefix + out.code.trim(), "utf8");
  }

  if (ext === ".css") {
    const src = Buffer.from(rawBuf.toString("utf8").replace(/\r\n/g, "\n"), "utf8");
    const out = transformCSS({
      filename: rel,
      code: src,
      minify: true,
      targets: CSS_TARGETS,
    });
    if (!out || !out.code) {
      throw new Error(`CSS minification failed for ${rel}`);
    }
    return Buffer.from(out.code);
  }

  if (ext === ".json") {
    const parsed = JSON.parse(rawBuf.toString("utf8"));
    return Buffer.from(JSON.stringify(parsed), "utf8");
  }

  return rawBuf;
}

function runBatchMode(batchJsonPath) {
  const spec = JSON.parse(readFileSync(resolve(batchJsonPath), "utf8"));
  const entryRel = normalizeRel(spec.entry || "main.js");
  const items = Array.isArray(spec.files) ? spec.files : [];
  let rawBytes = 0;
  let minBytes = 0;
  let count = 0;

  for (const item of items) {
    const rel = normalizeRel(item.rel);
    const srcBuf = readFileSync(item.src);
    const outBuf = minifyPluginBuffer(rel, srcBuf, entryRel);
    mkdirSync(dirname(item.out), { recursive: true });
    writeFileSync(item.out, outBuf);
    rawBytes += srcBuf.length;
    minBytes += outBuf.length;
    count += 1;
  }

  process.stdout.write(JSON.stringify({ ok: true, count, rawBytes, minBytes }) + "\n");
}

function runDirectoryMode(srcDirRaw, outDirRaw) {
  const srcDir = resolve(srcDirRaw);
  const outDir = outDirRaw ? resolve(outDirRaw) : srcDir;
  const inPlace = srcDir === outDir;

  const manifestPath = join(srcDir, "manifest.json");
  let entryRel = "main.js";
  if (existsSync(manifestPath)) {
    const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
    if (manifest && typeof manifest.entry === "string" && manifest.entry.trim()) {
      entryRel = normalizeRel(manifest.entry.trim());
    }
  }

  let rawTextBytes = 0;
  let minTextBytes = 0;
  let minifiedFiles = 0;

  function walk(curSrc) {
    for (const ent of readdirSync(curSrc, { withFileTypes: true })) {
      if (ent.name.startsWith(".") || EXCLUDED_NAMES.has(ent.name.toLowerCase())) {
        continue;
      }
      const fullSrc = join(curSrc, ent.name);
      const st = lstatSync(fullSrc);
      if (st.isSymbolicLink()) {
        throw new Error(`Symlink forbidden in plugin directory: ${fullSrc}`);
      }
      const relPosix = relative(srcDir, fullSrc).replace(/\\/g, "/");
      const fullOut = join(outDir, relPosix);

      if (st.isDirectory()) {
        mkdirSync(fullOut, { recursive: true });
        walk(fullSrc);
        continue;
      }
      if (!st.isFile()) {
        continue;
      }

      mkdirSync(dirname(fullOut), { recursive: true });
      const ext = extname(ent.name).toLowerCase();
      if (ext === ".js" || ext === ".css" || ext === ".json") {
        const rawBuf = readFileSync(fullSrc);
        const minBuf = minifyPluginBuffer(relPosix, rawBuf, entryRel);
        writeFileSync(fullOut, minBuf);
        rawTextBytes += rawBuf.length;
        minTextBytes += minBuf.length;
        minifiedFiles += 1;
      } else if (!inPlace) {
        copyFileSync(fullSrc, fullOut);
      }
    }
  }

  walk(srcDir);
  process.stdout.write(
    `Minified plugin tree ${srcDir} -> ${outDir}: ${minifiedFiles} text assets (${rawTextBytes}B -> ${minTextBytes}B)\n`
  );
}

const argv = process.argv.slice(2);
const batchIdx = argv.indexOf("--batch");
const dirIdx = argv.indexOf("--dir");
const outDirIdx = argv.indexOf("--out-dir");

if (batchIdx >= 0 && argv[batchIdx + 1]) {
  runBatchMode(argv[batchIdx + 1]);
} else if (dirIdx >= 0 && argv[dirIdx + 1]) {
  const outDirArg = outDirIdx >= 0 && argv[outDirIdx + 1] ? argv[outDirIdx + 1] : "";
  runDirectoryMode(argv[dirIdx + 1], outDirArg);
} else {
  console.error("Usage: minify-plugin-assets.mjs --batch <batch.json> | --dir <srcDir> [--out-dir <dstDir>]");
  process.exit(1);
}
