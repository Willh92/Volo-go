import { mkdirSync, readdirSync, readFileSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { gzipSync } from "node:zlib";
import { transform as transformCSS } from "lightningcss";
import { minifySync } from "vite";

const distDir = new URL("../../../internal/adminui/dist", import.meta.url).pathname;
const themeAssetsDir = new URL("../../../internal/theme/builtin/volo-studio/assets", import.meta.url).pathname;
const themeAssetsGzDir = new URL("../../../internal/theme/builtin/volo-studio/assets_gz", import.meta.url).pathname;
const themeTemplatesDir = new URL("../../../internal/theme/builtin/volo-studio/templates", import.meta.url).pathname;
const themeTemplatesGzDir = new URL("../../../internal/theme/builtin/volo-studio/templates_gz", import.meta.url).pathname;
const i18nBuiltinDir = new URL("../../../internal/i18n/builtin", import.meta.url).pathname;
const i18nBuiltinGzDir = new URL("../../../internal/i18n/builtin_gz", import.meta.url).pathname;

function deterministicGzip(raw) {
  const gz = gzipSync(raw, { level: 9 });
  if (gz.length >= 10 && gz[0] === 0x1f && gz[1] === 0x8b && (gz[3] & 0x02) === 0) {
    gz[4] = 0;
    gz[5] = 0;
    gz[6] = 0;
    gz[7] = 0;
    gz[9] = 0x03;
  }
  return gz;
}

function minifyEmbeddedPayload(fileName, raw) {
  if (fileName.endsWith(".js")) {
    const out = minifySync(fileName, raw.toString("utf8"));
    if (out && typeof out.code === "string" && (!out.errors || out.errors.length === 0)) {
      return Buffer.from(out.code, "utf8");
    }
  } else if (fileName.endsWith(".css")) {
    const out = transformCSS({ filename: fileName, code: raw, minify: true });
    if (out && out.code) {
      return Buffer.from(out.code);
    }
  } else if (fileName.endsWith(".json")) {
    return Buffer.from(JSON.stringify(JSON.parse(raw.toString("utf8"))), "utf8");
  }
  return raw;
}

function walkAdminDist(dir) {
  for (const ent of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, ent.name);
    if (ent.isDirectory()) {
      walkAdminDist(full);
      continue;
    }
    if (ent.isFile() && (full.endsWith(".js") || full.endsWith(".css"))) {
      const raw = readFileSync(full);
      writeFileSync(`${full}.gz`, deterministicGzip(raw));
      unlinkSync(full);
    }
  }
}

function precompressFlatDir(srcDir, outDir, suffix = "") {
  let entries;
  try {
    entries = readdirSync(srcDir, { withFileTypes: true });
  } catch (err) {
    if (err && err.code === "ENOENT") {
      return;
    }
    throw err;
  }
  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(outDir, { recursive: true });
  for (const ent of entries) {
    if (ent.name.startsWith(".")) continue;
    if (suffix && !ent.name.endsWith(suffix)) continue;
    if (!ent.isFile()) continue;
    const full = join(srcDir, ent.name);
    const raw = minifyEmbeddedPayload(ent.name, readFileSync(full));
    writeFileSync(join(outDir, `${ent.name}.gz`), deterministicGzip(raw));
  }
}

walkAdminDist(distDir);
precompressFlatDir(themeAssetsDir, themeAssetsGzDir);
precompressFlatDir(themeTemplatesDir, themeTemplatesGzDir, ".html");
precompressFlatDir(i18nBuiltinDir, i18nBuiltinGzDir, ".json");
