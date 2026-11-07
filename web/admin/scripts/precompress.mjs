import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, unlinkSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { gzipSync } from "node:zlib";

const distDir = new URL("../../../internal/adminui/dist", import.meta.url).pathname;
const themeAssetsDir = new URL("../../../internal/theme/builtin/volo-studio/assets", import.meta.url).pathname;
const themeAssetsGzDir = new URL("../../../internal/theme/builtin/volo-studio/assets_gz", import.meta.url).pathname;

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

function walkAdminDist(dir) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      walkAdminDist(full);
      continue;
    }
    if (full.endsWith(".js") || full.endsWith(".css")) {
      const raw = readFileSync(full);
      writeFileSync(`${full}.gz`, deterministicGzip(raw));
      unlinkSync(full);
    }
  }
}

function precompressBuiltinThemeAssets(srcDir, outDir) {
  if (!existsSync(srcDir)) {
    return;
  }
  rmSync(outDir, { recursive: true, force: true });
  mkdirSync(outDir, { recursive: true });
  for (const name of readdirSync(srcDir)) {
    if (name.startsWith(".")) continue;
    const full = join(srcDir, name);
    const st = statSync(full);
    if (!st.isFile()) continue;
    const raw = readFileSync(full);
    writeFileSync(join(outDir, `${name}.gz`), deterministicGzip(raw));
  }
}

walkAdminDist(distDir);
precompressBuiltinThemeAssets(themeAssetsDir, themeAssetsGzDir);

