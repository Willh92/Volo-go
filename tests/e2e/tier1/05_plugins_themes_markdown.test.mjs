import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertNoXSS,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient, solvePoW } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

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

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i += 1) {
    c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  }
  return (c ^ 0xffffffff) >>> 0;
}

function buildStoreZip(entries) {
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

async function submitGuestComment(guestClient, targetId, opts = {}) {
  const chRes = await guestClient.get("/api/v1/pow/challenge");
  assertStatus(chRes, 200);
  const sol = solvePoW(chRes.data.challenge, chRes.data.difficulty_bits);
  return guestClient.post("/api/v1/comments", {
    target_type: "article",
    target_id: targetId,
    author_name: opts.author_name || "Plugin Guest",
    author_email: opts.author_email || "pguest@e2e.local",
    content_md: opts.content_md || "Clean guest comment",
    pow_challenge: chRes.data.challenge,
    pow_solution: sol,
    website_hp: "",
    env_fp: opts.env_fp || "e2e-fp-05",
  });
}

describe("Tier 1 — Plugins, Themes & Markdown Rendering (F13, F14, F15)", () => {
  let srv;
  let admin;

  before(async () => {
    srv = await startVoloServer();
    admin = createClient(srv.baseUrl);
    const loginRes = await admin.login(srv.adminCreds.username, srv.adminCreds.password);
    assertStatus(loginRes, 200, "Admin login");
  });

  after(async () => {
    if (srv) {
      await srv.stop();
    }
  });

  describe("F13: Plugin Lifecycle & sensitive-words", () => {
    let targetArtId;

    before(async () => {
      const art = await admin.post("/api/admin/articles", {
        title: "F13 Antispam Target",
        slug: "f13-antispam-target",
        content_md: "Comment moderation plugin test target.",
        status: "published",
        allow_comment: true,
      });
      assertStatus(art, 201);
      targetArtId = art.data.article.id;
    });

    after(async () => {
      await admin.post("/api/admin/plugins/sensitive-words/disable").catch(() => {});
      if (targetArtId) {
        await admin.del(`/api/admin/articles/${targetArtId}`).catch(() => {});
      }
    });

    it("lists plugins disabled by default, enables/disables, updates priority, and resets circuit", async () => {
      const listRes = await admin.get("/api/admin/plugins");
      assertStatus(listRes, 200, "GET /api/admin/plugins");
      assertJsonHasKeys(listRes.data, ["items", "plugins", "cron_jobs"]);

      const sw = listRes.data.items.find((p) => p.id === "sensitive-words");
      const kbn = listRes.data.items.find((p) => p.id === "kanbanniang");
      assert.ok(sw && kbn, "Expected sensitive-words and kanbanniang plugins");
      assert.equal(sw.is_enabled, false);
      assert.equal(kbn.is_enabled, false);

      const enRes = await admin.post("/api/admin/plugins/sensitive-words/enable");
      assertStatus(enRes, 200, "Enable sensitive-words");
      assert.equal(enRes.data.plugin.is_enabled, true);

      const priRes = await admin.post("/api/admin/plugins/sensitive-words/priority", {
        priority: 15,
      });
      assertStatus(priRes, 200, "Set plugin priority");
      assert.equal(priRes.data.plugin.priority, 15);

      const rstRes = await admin.post("/api/admin/plugins/sensitive-words/reset-circuit");
      assertStatus(rstRes, 200, "Reset plugin circuit");
      assert.equal(rstRes.data.plugin.circuit_state, "closed");

      const disRes = await admin.post("/api/admin/plugins/sensitive-words/disable");
      assertStatus(disRes, 200, "Disable sensitive-words");
      assert.equal(disRes.data.plugin.is_enabled, false);
    });

    it("updates plugin config via PATCH, clears KV via DELETE, and tails/clears logs", async () => {
      assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);

      const patchRes = await admin.patch("/api/admin/plugins/sensitive-words/config", {
        action: "reject",
        custom_deny: ["forbiddenword_e2e"],
        auto_update: true,
      });
      assertStatus(patchRes, 200, "PATCH plugin config");
      assert.equal(patchRes.data.plugin.config.action, "reject");
      assert.ok(patchRes.data.plugin.config.custom_deny.includes("forbiddenword_e2e"));

      const kvDelRes = await admin.del("/api/admin/plugins/sensitive-words/kv");
      assertStatus(kvDelRes, 200, "DELETE plugin KV");
      assert.equal(kvDelRes.data.ok, true);
      assert.equal(typeof kvDelRes.data.cleared, "number");

      const logsRes = await admin.get("/api/admin/plugins/sensitive-words/logs?limit=20");
      assertStatus(logsRes, 200, "GET plugin logs");
      assert.ok(Array.isArray(logsRes.data.items));

      const clearLogsRes = await admin.del("/api/admin/plugins/sensitive-words/logs");
      assertStatus(clearLogsRes, 200, "DELETE plugin logs");
    });

    it("registers and manually triggers sensitive-words cron job (sync_dict)", async () => {
      assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);
      assertStatus(
        await admin.patch("/api/admin/plugins/sensitive-words/config", { auto_update: true }),
        200,
      );

      const cronList = await admin.get("/api/admin/plugins/sensitive-words/cron");
      assertStatus(cronList, 200, "GET plugin cron jobs");
      assert.ok(
        cronList.data.cron_jobs.some((job) => job.name === "sync_dict"),
        "Expected sync_dict cron job when auto_update is true",
      );

      const trigRes = await admin.post("/api/admin/plugins/sensitive-words/trigger-cron", {
        name: "sync_dict",
      });
      assertStatus(trigRes, 200, "POST trigger-cron");
      assert.equal(trigRes.data.ok, true);
      assert.equal(trigRes.data.triggered, 1);

      await admin.patch("/api/admin/plugins/sensitive-words/config", { auto_update: false });
    });

    it("exports plugin ZIP archive and installs/uninstalls a synthetic goja plugin ZIP", async () => {
      const expRes = await admin.get("/api/admin/plugins/sensitive-words/export");
      assertStatus(expRes, 200, "GET plugin export ZIP");
      assertHeaderIncludes(expRes, "content-type", "application/zip");
      assert.equal(expRes.buffer[0], 0x50);
      assert.equal(expRes.buffer[1], 0x4b);

      const zipBuf = buildStoreZip([
        {
          name: "manifest.json",
          data: JSON.stringify({
            id: "e2e-mini-plugin",
            version: "1.0.0",
            min_host_version: "0.1.0",
            runtime: "goja",
            entry: "main.js",
            name: { "en-US": "E2E Mini Plugin", "zh-CN": "E2E 迷你插件" },
            permissions: ["http:routes"],
          }),
        },
        {
          name: "main.js",
          data: 'function onRequest(req) { return { status: 200, headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ok: true, plugin: "e2e-mini-plugin" }) }; }\nmodule.exports = { onRequest: onRequest };\n',
        },
      ]);

      const form = new FormData();
      form.append("file", new Blob([zipBuf], { type: "application/zip" }), "e2e-mini-plugin.zip");
      const instRes = await admin.post("/api/admin/plugins/install?enable=1", form);
      assertStatus(instRes, 201, "POST /api/admin/plugins/install");
      assert.equal(instRes.data.plugin.id, "e2e-mini-plugin");
      assert.equal(instRes.data.plugin.is_enabled, true);

      const anon = createClient(srv.baseUrl);
      const routeRes = await anon.get("/plugins/e2e-mini-plugin/api/ping");
      assertStatus(routeRes, 200, "Synthetic plugin custom route");
      assert.equal(routeRes.data.plugin, "e2e-mini-plugin");

      const uninstRes = await admin.del("/api/admin/plugins/e2e-mini-plugin");
      assertStatus(uninstRes, 200, "DELETE synthetic plugin");
      assertStatus(await anon.get("/plugins/e2e-mini-plugin/api/ping"), [404, 410]);
    });

    it("enforces admin auth on sensitive-words API and rejects banned content & reserved nicknames", async () => {
      assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);
      assertStatus(
        await admin.patch("/api/admin/plugins/sensitive-words/config", {
          action: "reject",
          check_nickname: true,
          custom_deny: ["forbiddenword_e2e"],
        }),
        200,
      );

      const anon = createClient(srv.baseUrl);
      assertStatus(await anon.get("/plugins/sensitive-words/api/status"), 403, "Unauthenticated plugin status");

      const adminStatus = await admin.get("/plugins/sensitive-words/api/status");
      assertStatus(adminStatus, 200, "Admin sensitive-words status");
      assert.equal(adminStatus.data.ok, true);
      assert.equal(adminStatus.data.action, "reject");

      const badContent = await submitGuestComment(anon, targetArtId, {
        author_name: "NormalReader",
        content_md: "This includes forbiddenword_e2e in text",
      });
      assertStatus(badContent, 403, "Comment with custom_deny word must be rejected");

      const badNick = await submitGuestComment(anon, targetArtId, {
        author_name: "webmaster",
        content_md: "Innocent comment text",
      });
      assertStatus(badNick, 403, "Reserved nickname must be rejected");
    });

    it("switches sensitive-words action to moderate and holds matching guest comments as pending", async () => {
      assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);
      assertStatus(
        await admin.patch("/api/admin/plugins/sensitive-words/config", {
          action: "moderate",
          custom_deny: ["moderatedword_e2e"],
        }),
        200,
      );

      const guest = createClient(srv.baseUrl);
      const subRes = await submitGuestComment(guest, targetArtId, {
        author_name: "CarefulReader",
        content_md: "Comment containing moderatedword_e2e inside",
      });
      assertStatus(subRes, 201, "Moderated comment accepted into queue");
      assert.equal(subRes.data.comment.status, "pending");
    });
  });

  describe("F14: kanbanniang Plugin, Themes, 6 Accents & 4 Modes", () => {
    after(async () => {
      await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
      await admin.post("/api/admin/themes/activate", {
        theme_id: "volo-studio",
        target: "both",
      }).catch(() => {});
      await admin.patch("/api/admin/settings/appearance", {
        accent: "forest",
        default_mode: "light",
      }).catch(() => {});
    });

    it("enables kanbanniang, updates config, and injects public.footer & admin.dashboard_card slots", async () => {
      const enRes = await admin.post("/api/admin/plugins/kanbanniang/enable");
      assertStatus(enRes, 200, "Enable kanbanniang");
      assert.equal(enRes.data.plugin.is_enabled, true);

      const cfgRes = await admin.patch("/api/admin/plugins/kanbanniang/config", {
        position: "right",
        model: "Potion-Maker/Pio",
        hide_on_mobile: true,
      });
      assertStatus(cfgRes, 200, "Patch kanbanniang config");

      const anon = createClient(srv.baseUrl);
      const homeRes = await anon.get("/");
      assertStatus(homeRes, 200);
      assert.ok(homeRes.text.includes('id="volo-kanbanniang-root"'));
      assert.ok(homeRes.text.includes("volo-kbn-right"));
      assert.ok(homeRes.text.includes("volo-kbn-hide-mobile"));
      assert.ok(homeRes.text.includes('data-model="Potion-Maker/Pio"'));
      assert.ok(homeRes.text.includes('id="soloKanbanniang"'));

      const dashRes = await admin.get("/api/admin/overview");
      assertStatus(dashRes, 200);
      const pluginCards = JSON.stringify(dashRes.data);
      assert.ok(pluginCards.includes("kbn-admin-stat"));
    });

    it("serves kanbanniang HTTP endpoints (/state, /model, /random-model, /list, /pat)", async () => {
      assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
      assertStatus(await admin.del("/api/admin/plugins/kanbanniang/kv"), 200);

      const anon = createClient(srv.baseUrl);
      const stateRes = await anon.get("/plugins/kanbanniang/api/state");
      assertStatus(stateRes, 200, "GET /plugins/kanbanniang/api/state");
      assert.equal(stateRes.data.ok, true);
      assertJsonHasKeys(stateRes.data, ["model", "character", "position", "total_pats"]);

      const modelRes = await anon.get("/plugins/kanbanniang/api/model");
      assertStatus(modelRes, 200, "GET /plugins/kanbanniang/api/model");

      const randRes = await anon.get("/plugins/kanbanniang/api/random-model");
      assertStatus(randRes, 200, "GET /plugins/kanbanniang/api/random-model");

      assertStatus(await anon.get("/plugins/kanbanniang/api/list"), 401, "Unauthenticated /list returns 401");
      const listRes = await admin.get("/plugins/kanbanniang/api/list");
      assertStatus(listRes, 200, "Admin /list returns models");
      assert.equal(listRes.data.sc, true);
      assert.ok(Array.isArray(listRes.data.models) && listRes.data.models.length > 0);

      const patRes = await anon.post("/plugins/kanbanniang/api/pat");
      assertStatus(patRes, 200, "POST /plugins/kanbanniang/api/pat");
      assert.equal(patRes.data.ok, true);
      assert.ok(patRes.data.total_pats >= 1);
    });

    it("activates volo-journal and volo-studio themes and renders matching skin attributes & CSS", async () => {
      const actJournal = await admin.post("/api/admin/themes/activate", {
        theme_id: "volo-journal",
        target: "desktop",
      });
      assertStatus(actJournal, 200, "Activate volo-journal");
      assert.equal(actJournal.data.appearance.active_theme, "volo-journal");

      const anon = createClient(srv.baseUrl);
      const journalHome = await anon.get("/");
      assertStatus(journalHome, 200);
      assert.ok(journalHome.text.includes('data-volo-skin="volo-journal"'));
      assert.ok(journalHome.text.includes("/themes/volo-journal/assets/volo-journal.css"));

      const actStudio = await admin.post("/api/admin/themes/activate", {
        theme_id: "volo-studio",
        target: "both",
      });
      assertStatus(actStudio, 200, "Activate volo-studio");

      const studioHome = await anon.get("/");
      assertStatus(studioHome, 200);
      assert.ok(studioHome.text.includes('data-volo-skin="studio"'));
      assert.ok(studioHome.text.includes("/themes/volo-studio/assets/volo-studio.css"));
    });

    it("persists and renders all 6 appearance accents and rejects invalid accent", async () => {
      const accents = ["forest", "ocean", "teal", "amber", "plum", "rose"];
      const anon = createClient(srv.baseUrl);

      for (const accent of accents) {
        const res = await admin.patch("/api/admin/settings/appearance", { accent });
        assertStatus(res, 200, `Set accent=${accent}`);
        const htmlRes = await anon.get("/");
        assertStatus(htmlRes, 200);
        assert.ok(
          htmlRes.text.includes(`data-accent="${accent}"`),
          `Expected data-accent="${accent}" on public HTML`,
        );
      }

      const invalid = await admin.patch("/api/admin/settings/appearance", { accent: "neon" });
      assertStatus(invalid, 400, "Invalid accent must be rejected");
    });

    it("persists and renders all 4 appearance modes (light, dark, system, auto) and rejects invalid mode", async () => {
      const modes = ["light", "dark", "system", "auto"];
      const anon = createClient(srv.baseUrl);

      for (const mode of modes) {
        const res = await admin.patch("/api/admin/settings/appearance", { default_mode: mode });
        assertStatus(res, 200, `Set default_mode=${mode}`);
        const htmlRes = await anon.get("/");
        assertStatus(htmlRes, 200);
        assert.ok(
          htmlRes.text.includes(`data-default-mode="${mode}"`),
          `Expected data-default-mode="${mode}" on public HTML`,
        );
      }

      const invalid = await admin.patch("/api/admin/settings/appearance", { default_mode: "sepia" });
      assertStatus(invalid, 400, "Invalid default_mode must be rejected");
    });

    it("updates volo-journal theme config and reflects journal_motto on non-home pages", async () => {
      assertStatus(
        await admin.post("/api/admin/themes/activate", { theme_id: "volo-journal", target: "desktop" }),
        200,
      );
      const cfgRes = await admin.put("/api/admin/themes/volo-journal/config", {
        journal_motto: "E2E_JOURNAL_MOTTO",
      });
      assertStatus(cfgRes, 200, "PUT /api/admin/themes/volo-journal/config");

      const anon = createClient(srv.baseUrl);
      const aboutRes = await anon.get("/about");
      assertStatus(aboutRes, 200);
      assert.ok(aboutRes.text.includes("E2E_JOURNAL_MOTTO"));
    });
  });

  describe("F15: Markdown Rendering & XSS Sanitization", () => {
    it("renders Chroma syntax-highlighted code blocks with and without line numbers", async () => {
      assertStatus(
        await admin.patch("/api/admin/settings/reading", { show_code_line_numbers: false }),
        200,
      );
      const md = [
        "```go",
        "package main",
        "func main() { println(len(\"ok\")) }",
        "```",
        "",
        "```ts",
        "export const answer: number = 42;",
        "```",
      ].join("\n");

      const noLines = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(noLines, 200, "Render preview without line numbers");
      assert.ok(noLines.data.html.includes('<figure class="volo-codeblock" data-lang="go"><pre class="chroma"><code>'));
      assert.ok(noLines.data.html.includes('data-lang="ts"'));
      assert.ok(!noLines.data.html.includes('<span class="ln">'));

      assertStatus(
        await admin.patch("/api/admin/settings/reading", { show_code_line_numbers: true }),
        200,
      );
      const withLines = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(withLines, 200, "Render preview with line numbers");
      assert.ok(withLines.data.html.includes('<span class="line"><span class="ln">1</span><span class="cl">'));
    });

    it("renders block and inline math while avoiding false-positives on currency amounts", async () => {
      const md = [
        "$$E = mc^2$$",
        "",
        "Inline formula $\\alpha + \\beta$ and prices $100 and $200 today.",
      ].join("\n");

      const res = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(res, 200, "Render math preview");
      assert.equal(res.data.needs_math, true);
      assert.ok(res.data.html.includes('<div class="volo-math-block">$$E = mc^2$$</div>'));
      assert.ok(res.data.html.includes('<span class="volo-math-inline">$\\alpha + \\beta$</span>'));
      assert.ok(!res.data.html.includes('<span class="volo-math-inline">$100'));
    });

    it("renders Mermaid diagrams, GitHub callouts, and responsive .volo-table-scroll tables", async () => {
      const md = [
        "```mermaid",
        "graph LR",
        "  A[Client] --> B[Volo]",
        "```",
        "",
        "> [!NOTE]",
        "> Important callout note.",
        "",
        "> [!WARNING]",
        "> Caution warning text.",
        "",
        "| ColA | ColB |",
        "| --- | --- |",
        "| val1 | val2 |",
      ].join("\n");

      const res = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(res, 200, "Render mermaid/callout/table");
      assert.equal(res.data.needs_mermaid, true);
      assert.ok(res.data.html.includes('<pre class="mermaid">'));
      assert.ok(res.data.html.includes('class="volo-callout volo-callout--note" data-callout="note"'));
      assert.ok(res.data.html.includes('class="volo-callout volo-callout--warning" data-callout="warning"'));
      assert.ok(res.data.html.includes('<div class="volo-table-scroll"><table>'));
    });

    it("renders inline TOC ([toc]) and bookshelf embed (:::bookshelf) directives", async () => {
      const bookRes = await admin.post("/api/admin/books", {
        title: "Embedded Shelf Book",
        slug: "embedded-shelf-book",
        source_type: "manual",
      });
      assertStatus(bookRes, 201);
      const bookId = bookRes.data.book.id;

      try {
        const md = [
          "[toc]",
          "",
          "# Section Alpha",
          "",
          "## Subsection Beta",
          "",
          ":::bookshelf",
          ":::",
        ].join("\n");

        const res = await admin.post("/api/admin/render/preview", { content_md: md });
        assertStatus(res, 200, "Render [toc] and :::bookshelf");
        assert.ok(res.data.html.includes('<nav class="volo-inline-toc" data-md-toc="1"'));
        assert.ok(res.data.html.includes('<section class="volo-embed-bookshelf"'));
        assert.ok(res.data.html.includes("Embedded Shelf Book"));
      } finally {
        await admin.del(`/api/admin/books/${bookId}`);
      }
    });

    it("protects :::lock pwd blocks on public SSR until unlocked via /api/v1/articles/{id}/unlock", async () => {
      const artRes = await admin.post("/api/admin/articles", {
        title: "Locked Content Article",
        slug: "locked-content-article",
        content_md: [
          "Public intro paragraph.",
          "",
          ':::lock pwd="unlock123" hint="Enter password"',
          "TOP_SECRET_INLINE_BODY",
          ":::",
        ].join("\n"),
        status: "published",
      });
      assertStatus(artRes, 201);
      const artId = artRes.data.article.id;

      try {
        const visitor = createClient(srv.baseUrl);
        const lockedHtml = await visitor.get("/posts/locked-content-article");
        assertStatus(lockedHtml, 200);
        assert.ok(lockedHtml.text.includes("Public intro paragraph."));
        assert.ok(lockedHtml.text.includes('class="volo-lock-card"'));
        assert.ok(lockedHtml.text.includes('data-lock-type="pwd"'));
        assert.ok(!lockedHtml.text.includes("TOP_SECRET_INLINE_BODY"));

        const badUnlock = await visitor.post(`/api/v1/articles/${artId}/unlock`, {
          password: "wrong",
        });
        assertStatus(badUnlock, 403, "Wrong lock password returns 403");

        const okUnlock = await visitor.post(`/api/v1/articles/${artId}/unlock`, {
          password: "unlock123",
        });
        assertStatus(okUnlock, 200, "Correct lock password unlocks");

        const unlockedHtml = await visitor.get("/posts/locked-content-article");
        assertStatus(unlockedHtml, 200);
        assert.ok(unlockedHtml.text.includes("TOP_SECRET_INLINE_BODY"));
        assert.ok(unlockedHtml.text.includes("volo-lock-block--unlocked"));
      } finally {
        await admin.del(`/api/admin/articles/${artId}`);
      }
    });

    it("sanitizes dangerous XSS payloads in both render preview and published SSR output", async () => {
      const xssMd = [
        "Safe paragraph.",
        "",
        '<script>alert("xss1")</script>',
        "",
        '<img src="x" onerror="alert(2)">',
        "",
        "[Click Bad](javascript:alert(3))",
        "",
        '<iframe src="https://evil.example/embed"></iframe>',
      ].join("\n");

      const prevRes = await admin.post("/api/admin/render/preview", { content_md: xssMd });
      assertStatus(prevRes, 200, "XSS preview sanitization");
      assertNoXSS(prevRes.data.html, "Render preview XSS check");
      assert.ok(!prevRes.data.html.toLowerCase().includes("<iframe"));

      const artRes = await admin.post("/api/admin/articles", {
        title: "XSS Sanitization Test",
        slug: "xss-sanitization-test",
        content_md: xssMd,
        status: "published",
      });
      assertStatus(artRes, 201);

      try {
        const anon = createClient(srv.baseUrl);
        const pubRes = await anon.get("/posts/xss-sanitization-test");
        assertStatus(pubRes, 200);
        assert.ok(pubRes.text.includes("Safe paragraph."));
        const proseIdx = pubRes.text.indexOf('class="volo-prose"');
        assert.ok(proseIdx >= 0, "Expected .volo-prose container on article page");
        const proseEnd = pubRes.text.indexOf("</article>", proseIdx);
        const proseHtml = pubRes.text.slice(proseIdx, proseEnd > proseIdx ? proseEnd : undefined);
        assertNoXSS(proseHtml, "Public article prose XSS check");
        assert.ok(!pubRes.text.includes('alert("xss1")'));
        assert.ok(!/<img[^>]*\bonerror\s*=/i.test(pubRes.text));
        assert.ok(!pubRes.text.includes("javascript:alert"));
        assert.ok(!pubRes.text.toLowerCase().includes("<iframe"));
      } finally {
        await admin.del(`/api/admin/articles/${artRes.data.article.id}`);
      }
    });
  });
});
