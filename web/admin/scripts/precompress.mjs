import { readdirSync, readFileSync, writeFileSync, unlinkSync, statSync } from "node:fs";
import { join } from "node:path";
import { gzipSync } from "node:zlib";

const distDir = new URL("../../../internal/adminui/dist", import.meta.url).pathname;

function walk(dir) {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      walk(full);
      continue;
    }
    if (full.endsWith(".js") || full.endsWith(".css")) {
      const raw = readFileSync(full);
      const gz = gzipSync(raw, { level: 9 });
      writeFileSync(`${full}.gz`, gz);
      unlinkSync(full);
    }
  }
}

walk(distDir);
