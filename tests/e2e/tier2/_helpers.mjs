import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { assertStatus } from "../lib/assertions.mjs";
import { solvePoW } from "../lib/client.mjs";
import { runVoloCommand } from "../lib/server.mjs";

export const MINIMAL_PNG_BUFFER = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
  "base64",
);

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let i = 0; i < 256; i += 1) {
    let c = i;
    for (let k = 0; k < 8; k += 1) {
      c = (c & 1) !== 0 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
    table[i] = c >>> 0;
  }
  return table;
})();

export function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i += 1) {
    c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  }
  return (c ^ 0xffffffff) >>> 0;
}

/**
 * Build a minimal uncompressed (STORE) ZIP archive in memory.
 */
export function buildStoreZip(entries) {
  const localParts = [];
  const centralParts = [];
  let offset = 0;

  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const dataBuf = Buffer.isBuffer(entry.data) ? entry.data : Buffer.from(entry.data, "utf8");
    const crc = crc32(dataBuf);

    const localHdr = Buffer.alloc(30);
    localHdr.writeUInt32LE(0x04034b50, 0);
    localHdr.writeUInt16LE(20, 4);
    localHdr.writeUInt16LE(0x0800, 6);
    localHdr.writeUInt16LE(0, 8);
    localHdr.writeUInt16LE(0, 10);
    localHdr.writeUInt16LE(0, 12);
    localHdr.writeUInt32LE(crc, 14);
    localHdr.writeUInt32LE(dataBuf.length, 18);
    localHdr.writeUInt32LE(dataBuf.length, 22);
    localHdr.writeUInt16LE(nameBuf.length, 26);
    localHdr.writeUInt16LE(0, 28);

    localParts.push(localHdr, nameBuf, dataBuf);

    const centralHdr = Buffer.alloc(46);
    centralHdr.writeUInt32LE(0x02014b50, 0);
    centralHdr.writeUInt16LE(20, 4);
    centralHdr.writeUInt16LE(20, 6);
    centralHdr.writeUInt16LE(0x0800, 8);
    centralHdr.writeUInt16LE(0, 10);
    centralHdr.writeUInt16LE(0, 12);
    centralHdr.writeUInt16LE(0, 14);
    centralHdr.writeUInt32LE(crc, 16);
    centralHdr.writeUInt32LE(dataBuf.length, 20);
    centralHdr.writeUInt32LE(dataBuf.length, 24);
    centralHdr.writeUInt16LE(nameBuf.length, 28);
    centralHdr.writeUInt16LE(0, 30);
    centralHdr.writeUInt16LE(0, 32);
    centralHdr.writeUInt16LE(0, 34);
    centralHdr.writeUInt16LE(0, 36);
    centralHdr.writeUInt32LE(0, 38);
    centralHdr.writeUInt32LE(offset, 42);

    centralParts.push(centralHdr, nameBuf);
    offset += localHdr.length + nameBuf.length + dataBuf.length;
  }

  const centralDir = Buffer.concat(centralParts);
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0);
  eocd.writeUInt16LE(0, 4);
  eocd.writeUInt16LE(0, 6);
  eocd.writeUInt16LE(entries.length, 8);
  eocd.writeUInt16LE(entries.length, 10);
  eocd.writeUInt32LE(centralDir.length, 12);
  eocd.writeUInt32LE(offset, 16);
  eocd.writeUInt16LE(0, 20);

  return Buffer.concat([...localParts, centralDir, eocd]);
}

/**
 * Fetch PoW challenge, solve it, and submit a public comment.
 */
export async function submitGuestComment(guestClient, targetId, opts = {}) {
  const chRes = await guestClient.get("/api/v1/pow/challenge");
  assertStatus(chRes, 200, "GET /api/v1/pow/challenge");
  const { challenge, difficulty_bits: bits } = chRes.data;
  const sol = solvePoW(challenge, bits);

  return guestClient.post("/api/v1/comments", {
    target_type: opts.target_type || "article",
    target_id: targetId,
    author_name: opts.author_name || "E2E Guest",
    author_email: opts.author_email || "guest@e2e.local",
    author_url: opts.author_url ?? "",
    content_md: opts.content_md || "E2E guest comment body",
    pow_challenge: challenge,
    pow_solution: sol,
    website_hp: opts.website_hp ?? "",
    env_fp: opts.env_fp || "e2e-fp-shared",
    ...(opts.parent_id ? { parent_id: opts.parent_id } : {}),
  });
}

/**
 * Patch a settings domain temporarily and guarantee restoration in finally.
 */
export async function withTempSettings(adminClient, domain, patch, restore, fn) {
  const patchRes = await adminClient.patch(`/api/admin/settings/${domain}`, patch);
  assertStatus(patchRes, 200, `PATCH /api/admin/settings/${domain}`);
  try {
    return await fn(patchRes.data);
  } finally {
    if (restore && Object.keys(restore).length > 0) {
      await adminClient.patch(`/api/admin/settings/${domain}`, restore).catch(() => {});
    }
  }
}

export function extractBootConfig(html) {
  const match = String(html || "").match(
    /<script\s+id=["']volo-boot-config["'][^>]*>([\s\S]*?)<\/script>/i,
  );
  assert.ok(match, 'Expected <script id="volo-boot-config"> tag in HTML');
  return JSON.parse(match[1].trim());
}

export async function readBootConfig(page) {
  const raw = await page.locator("#volo-boot-config").textContent();
  return JSON.parse(String(raw || "{}"));
}

export async function loginConsoleViaUI(page, diag, baseUrl, creds, hash = "") {
  await page.goto(`${baseUrl}/console${hash}`, { waitUntil: "domcontentloaded" });
  await page.locator(".volo-auth-version").waitFor({ state: "visible", timeout: 15000 });
  await page.locator('input[name="volo_staff_ident"]').fill(creds.username);
  await page.locator('input[name="volo_staff_secret"]').fill(creds.password);
  diag.clear();
  await page.locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit').click();
  await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
  await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
}

/**
 * Enter #editor and wait for loadTaxonomies().then(() => startNewArticle()) to settle
 * before interacting with title/slug/CM6 inputs.
 */
export async function waitForEditorReady(page, triggerFn) {
  const waitGet200 = (urlPart) =>
    page.waitForResponse(
      (r) =>
        r.url().includes(urlPart) &&
        r.request().method() === "GET" &&
        r.status() === 200,
      { timeout: 20000 },
    );

  const catsWait = waitGet200("/api/admin/categories");
  const seriesWait = waitGet200("/api/admin/series");
  const tagsWait = waitGet200("/api/admin/tags");

  await triggerFn();
  await Promise.all([catsWait, seriesWait, tagsWait]);
  await page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 });
  await page.locator(".volo-cm-host .cm-editor .cm-content").waitFor({ state: "visible", timeout: 15000 });
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
}

export async function createTempArticle(adminClient, overrides = {}) {
  const suffix = `${Date.now()}-${Math.floor(Math.random() * 10000)}`;
  const payload = {
    title: overrides.title || `Temp Article ${suffix}`,
    slug: overrides.slug || `temp-article-${suffix}`,
    content_md: overrides.content_md || "Temp article body.",
    status: overrides.status || "published",
    allow_comment: overrides.allow_comment ?? true,
    ...overrides,
  };
  const res = await adminClient.post("/api/admin/articles", payload);
  assertStatus(res, 201, `Create article ${payload.slug}`);
  return res.data.article;
}

export async function deleteArticleSafe(adminClient, id) {
  if (!id) return;
  await adminClient.del(`/api/admin/articles/${id}`).catch(() => {});
}

export async function createTempUser(adminClient, overrides = {}) {
  const suffix = `${Date.now()}_${randomBytes(4).toString("hex")}`;
  const username = overrides.username || `u_${suffix}`;
  const password = overrides.password || "TempUserPass!123";
  const payload = {
    username,
    email: overrides.email || `${username}@e2e.local`,
    display_name: overrides.display_name || `User ${suffix}`,
    role: overrides.role || "editor",
    password,
    ...(overrides.current_password ? { current_password: overrides.current_password } : {}),
  };
  const res = await adminClient.post("/api/admin/users", payload);
  assertStatus(res, 201, `Create user ${username}`);
  return { user: res.data.user, password };
}

export async function deleteUserSafe(adminClient, id) {
  if (!id) return;
  await adminClient.del(`/api/admin/users/${id}`).catch(() => {});
}

export async function resolveCliVersion() {
  const res = await runVoloCommand(["version"]);
  assert.equal(res.code, 0, `volo version failed: ${res.stderr}`);
  const m = res.stdout.trim().match(/^volo\s+(v[^\s]+)\s+\(/i);
  assert.ok(m, `Unexpected volo version output: ${res.stdout}`);
  return { version: m[1], stdout: res.stdout.trim() };
}
