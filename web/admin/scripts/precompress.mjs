import { execFileSync } from "node:child_process";
import { mkdtempSync, readdirSync, readFileSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { gzipSync } from "node:zlib";
import { transform as transformCSS } from "lightningcss";
import { minifySync } from "vite";

const distDir = new URL("../../../internal/adminui/dist", import.meta.url).pathname;
const cssTargets = {
  safari: (17 << 16) | (5 << 8),
  chrome: 124 << 16,
  firefox: 125 << 16,
};

let has7z = false;
try {
  execFileSync("7z", ["--help"], { stdio: "ignore" });
  has7z = true;
} catch {}

function deterministicGzip(raw) {
  let gz = null;
  if (has7z && raw.length > 256) {
    const tmp = mkdtempSync(join(tmpdir(), "volo-gz-"));
    try {
      const inPath = join(tmp, "payload");
      const outPath = join(tmp, "payload.gz");
      writeFileSync(inPath, raw);
      execFileSync("7z", ["a", "-tgzip", "-mx=9", "-mfb=258", "-mpass=15", outPath, inPath], { stdio: "ignore" });
      gz = readFileSync(outPath);
    } catch {
      gz = null;
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  }
  if (!gz) {
    gz = gzipSync(raw, { level: 9 });
  }
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
    const out = minifySync(fileName, raw.toString("utf8"), { mangle: { toplevel: true } });
    if (out && typeof out.code === "string" && (!out.errors || out.errors.length === 0)) {
      return Buffer.from(out.code, "utf8");
    }
  } else if (fileName.endsWith(".css")) {
    const out = transformCSS({
      filename: fileName,
      code: raw,
      minify: true,
      targets: cssTargets,
    });
    if (out && out.code) {
      return Buffer.from(out.code);
    }
  } else if (fileName.endsWith(".html")) {
    let html = raw.toString("utf8");
    let prevHtml;
    do {
      prevHtml = html;
      html = html.replace(/<!--(?!VOLO_BOOT-->)[\s\S]*?-->/g, "");
    } while (html !== prevHtml);
    html = html.replace(/<style\b[^>]*>([\s\S]*?)<\/style\b[^>]*>/gi, (full, css) => {
      if (!/^<style>/i.test(full) || css.includes("{{")) return full;
      try {
        const out = transformCSS({
          filename: `${fileName}.css`,
          code: Buffer.from(css, "utf8"),
          minify: true,
          targets: cssTargets,
        });
        if (out && out.code) {
          return `<style>${Buffer.from(out.code).toString("utf8")}</style>`;
        }
      } catch {}
      return full;
    });
    html = html.replace(/<script\b[^>]*>([\s\S]*?)<\/script\b[^>]*>/gi, (full, js) => {
      if (!/^<script>/i.test(full) || js.includes("{{") || !js.trim()) return full;
      try {
        const out = minifySync(`${fileName}.js`, js);
        if (out && typeof out.code === "string" && (!out.errors || out.errors.length === 0)) {
          return `<script>${out.code.trim()}</script>`;
        }
      } catch {}
      return full;
    });
    html = html.replace(/\n[ \t]+/g, "\n").replace(/\n{2,}/g, "\n").trim();
    return Buffer.from(html, "utf8");
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
    if (
      ent.isFile() &&
      (full.endsWith(".js") || full.endsWith(".css") || full.endsWith(".json") || full.endsWith(".html"))
    ) {
      const raw = minifyEmbeddedPayload(ent.name, readFileSync(full));
      writeFileSync(`${full}.gz`, deterministicGzip(raw));
      unlinkSync(full);
    }
  }
}

function syncBuiltinEmbeddedAssets() {
  const i18nDir = new URL("../../../internal/i18n/builtin", import.meta.url).pathname;
  const i18nGzDir = new URL("../../../internal/i18n/builtin_gz", import.meta.url).pathname;
  for (const name of readdirSync(i18nDir)) {
    if (!name.endsWith(".json")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(i18nDir, name)));
    writeFileSync(join(i18nGzDir, `${name}.gz`), deterministicGzip(raw));
  }

  const studioAssetsDir = new URL("../../../internal/theme/builtin/volo-studio/assets", import.meta.url).pathname;
  const studioAssetsGzDir = new URL("../../../internal/theme/builtin/volo-studio/assets_gz", import.meta.url).pathname;
  for (const name of readdirSync(studioAssetsDir)) {
    if (!name.endsWith(".js") && !name.endsWith(".css")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(studioAssetsDir, name)));
    writeFileSync(join(studioAssetsGzDir, `${name}.gz`), deterministicGzip(raw));
  }

  const studioTplDir = new URL("../../../internal/theme/builtin/volo-studio/templates", import.meta.url).pathname;
  const studioTplGzDir = new URL("../../../internal/theme/builtin/volo-studio/templates_gz", import.meta.url).pathname;
  for (const name of readdirSync(studioTplDir)) {
    if (!name.endsWith(".html")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(studioTplDir, name)));
    writeFileSync(join(studioTplGzDir, `${name}.gz`), deterministicGzip(raw));
  }
}

if (!process.argv.includes("--embedded-only")) {
  walkAdminDist(distDir);
}
syncBuiltinEmbeddedAssets();
