import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, unlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { gunzipSync, gzipSync } from "node:zlib";
import { transform as transformCSS } from "lightningcss";
import { minifySync } from "vite";

const resolvePath = (rel) => fileURLToPath(new URL(rel, import.meta.url));
const defaultDistDir = resolvePath("../../../internal/adminui/dist");
const cssTargets = {
  safari: (17 << 16) | (5 << 8),
  chrome: 124 << 16,
  firefox: 125 << 16,
  edge: 124 << 16,
};

let sevenZipBin = "";
for (const candidate of [
  "7z",
  "C:\\Program Files\\7-Zip\\7z.exe",
  "C:\\Program Files (x86)\\7-Zip\\7z.exe",
]) {
  try {
    execFileSync(candidate, ["--help"], { stdio: "ignore" });
    sevenZipBin = candidate;
    break;
  } catch {}
}

function sha256Short(buf) {
  return createHash("sha256").update(buf).digest("hex").slice(0, 8);
}

function safeWriteFile(outPath, buf) {
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      if (existsSync(outPath)) {
        try {
          unlinkSync(outPath);
        } catch {}
      }
      writeFileSync(outPath, buf);
      return;
    } catch (err) {
      if (attempt === 2) throw err;
    }
  }
}

function deterministicGzip(raw) {
  let gz = null;
  if (sevenZipBin && raw.length > 256) {
    const tmp = mkdtempSync(join(tmpdir(), "volo-gz-"));
    try {
      const inPath = join(tmp, "payload");
      const outPath = join(tmp, "payload.gz");
      writeFileSync(inPath, raw);
      execFileSync(sevenZipBin, ["a", "-tgzip", "-mx=9", "-mfb=258", "-mpass=15", outPath, inPath], {
        stdio: "ignore",
      });
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
    const src = raw.toString("utf8").replace(/\r\n/g, "\n");
    const out = minifySync(fileName, src, { mangle: { toplevel: true } });
    if (out && typeof out.code === "string" && (!out.errors || out.errors.length === 0)) {
      return Buffer.from(out.code, "utf8");
    }
  } else if (fileName.endsWith(".css")) {
    const src = Buffer.from(raw.toString("utf8").replace(/\r\n/g, "\n"), "utf8");
    const out = transformCSS({
      filename: fileName,
      code: src,
      minify: true,
      targets: cssTargets,
    });
    if (out && out.code) {
      return Buffer.from(out.code);
    }
  } else if (fileName.endsWith(".html")) {
    let html = raw.toString("utf8").replace(/\r\n/g, "\n");
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
  } else if (fileName.endsWith(".svg")) {
    let svg = raw.toString("utf8").replace(/\r\n/g, "\n");
    let prevSvg;
    do {
      prevSvg = svg;
      svg = svg.replace(/<!--[\s\S]*?-->/g, "");
    } while (svg !== prevSvg);
    svg = svg
      .replace(/>\s+</g, "><")
      .replace(/[ \t\n]+/g, " ")
      .trim();
    return Buffer.from(svg, "utf8");
  } else if (fileName.endsWith(".json")) {
    return Buffer.from(JSON.stringify(JSON.parse(raw.toString("utf8"))), "utf8");
  }
  return raw;
}

async function processAdminDist(targetDistDir) {
  const assetsDir = join(targetDistDir, "assets");
  const minAssets = new Map();
  const verMap = new Map();

  if (existsSync(assetsDir)) {
    for (const ent of readdirSync(assetsDir, { withFileTypes: true })) {
      if (!ent.isFile()) continue;
      const full = join(assetsDir, ent.name);
      if (ent.name.endsWith(".gz")) {
        unlinkSync(full);
        continue;
      }
      if (ent.name.endsWith(".js") || ent.name.endsWith(".css")) {
        minAssets.set(ent.name, minifyEmbeddedPayload(ent.name, readFileSync(full)));
      }
    }

    const jsNames = Array.from(minAssets.keys())
      .filter((n) => n.endsWith(".js"))
      .sort();
    const jsGraphDigest = sha256Short(
      Buffer.from(jsNames.map((n) => `${n}:${sha256Short(minAssets.get(n))}`).join("|"), "utf8")
    );

    for (const [name, buf] of minAssets.entries()) {
      if (name.endsWith(".js")) {
        verMap.set(name, sha256Short(Buffer.concat([buf, Buffer.from(jsGraphDigest, "utf8")])));
      } else {
        verMap.set(name, sha256Short(buf));
      }
    }

    for (const name of jsNames) {
      const rewritten = minAssets
        .get(name)
        .toString("utf8")
        .replace(/(["'`])\.\/([A-Za-z0-9_.-]+\.js)\1/g, (full, q, depName) => {
          const v = verMap.get(depName);
          return v ? `${q}./${depName}?v=${v}${q}` : full;
        });
      minAssets.set(name, Buffer.from(rewritten, "utf8"));
    }

    for (const [name, buf] of minAssets.entries()) {
      const full = join(assetsDir, name);
      safeWriteFile(`${full}.gz`, deterministicGzip(buf));
      unlinkSync(full);
    }
  }

  function walkRest(dir) {
    for (const ent of readdirSync(dir, { withFileTypes: true })) {
      const full = join(dir, ent.name);
      if (ent.isDirectory()) {
        if (full === assetsDir) continue;
        walkRest(full);
        continue;
      }
      if (
        ent.isFile() &&
        (full.endsWith(".js") || full.endsWith(".css") || full.endsWith(".json") || full.endsWith(".html"))
      ) {
        let rawBuf = readFileSync(full);
        if (ent.name === "index.html" && verMap.size > 0) {
          const withVersions = rawBuf
            .toString("utf8")
            .replace(/(["'])\.\/assets\/([A-Za-z0-9_.-]+\.(?:js|css))\1/g, (m, q, assetName) => {
              const v = verMap.get(assetName);
              return v ? `${q}./assets/${assetName}?v=${v}${q}` : m;
            });
          rawBuf = Buffer.from(withVersions, "utf8");
        }
        const raw = minifyEmbeddedPayload(ent.name, rawBuf);
        safeWriteFile(`${full}.gz`, deterministicGzip(raw));
        unlinkSync(full);
      }
    }
  }

  walkRest(targetDistDir);
}

function writeGzipIfChanged(outPath, raw) {
  try {
    const prevGz = readFileSync(outPath);
    if (gunzipSync(prevGz).equals(raw)) {
      return;
    }
  } catch {}
  safeWriteFile(outPath, deterministicGzip(raw));
}

function syncBuiltinEmbeddedAssets() {
  const i18nDir = resolvePath("../../../internal/i18n/builtin");
  const i18nGzDir = resolvePath("../../../internal/i18n/builtin_gz");
  for (const name of readdirSync(i18nDir)) {
    if (!name.endsWith(".json")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(i18nDir, name)));
    writeGzipIfChanged(join(i18nGzDir, `${name}.gz`), raw);
  }

  const studioRootDir = resolvePath("../../../internal/theme/builtin/volo-studio");
  for (const name of ["theme.json", "preview.svg"]) {
    const srcPath = join(studioRootDir, name);
    if (!existsSync(srcPath)) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(srcPath));
    writeGzipIfChanged(join(studioRootDir, `${name}.gz`), raw);
  }

  const studioAssetsDir = join(studioRootDir, "assets");
  const studioAssetsGzDir = join(studioRootDir, "assets_gz");
  for (const name of readdirSync(studioAssetsDir)) {
    if (!name.endsWith(".js") && !name.endsWith(".css")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(studioAssetsDir, name)));
    writeGzipIfChanged(join(studioAssetsGzDir, `${name}.gz`), raw);
  }

  const studioTplDir = join(studioRootDir, "templates");
  const studioTplGzDir = join(studioRootDir, "templates_gz");
  for (const name of readdirSync(studioTplDir)) {
    if (!name.endsWith(".html")) continue;
    const raw = minifyEmbeddedPayload(name, readFileSync(join(studioTplDir, name)));
    writeGzipIfChanged(join(studioTplGzDir, `${name}.gz`), raw);
  }
}

const args = process.argv.slice(2);
const distFlagIdx = args.indexOf("--admin-dist");
const targetAdminDist = distFlagIdx >= 0 && args[distFlagIdx + 1] ? resolve(args[distFlagIdx + 1]) : defaultDistDir;

if (!args.includes("--embedded-only")) {
  processAdminDist(targetAdminDist);
}
if (!args.includes("--no-embedded")) {
  syncBuiltinEmbeddedAssets();
}
