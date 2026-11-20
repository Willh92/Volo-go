import assert from "node:assert/strict";
import path from "node:path";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertRedirect,
  assertStatus,
} from "../lib/assertions.mjs";
import { launchEdge, withPage } from "../lib/browser.mjs";
import { createClient } from "../lib/client.mjs";
import { runVoloCommand, startVoloServer } from "../lib/server.mjs";
import {
  buildStoreZip,
  createTempArticle,
  createTempUser,
  deleteArticleSafe,
  deleteUserSafe,
  extractBootConfig,
  loginConsoleViaUI,
  waitForEditorReady,
  withTempSettings,
} from "./_helpers.mjs";

describe("Tier 2 — Markdown, Settings & Plugin/Theme Corners", () => {
  let srv;
  let admin;
  let anon;

  before(async () => {
    srv = await startVoloServer({
      setupPayload: {
        site_title: "Tier2 Corners E2E",
        default_locale: "en-US",
      },
    });
    admin = createClient(srv.baseUrl);
    anon = createClient(srv.baseUrl);
    assertStatus(await admin.login(srv.adminCreds.username, srv.adminCreds.password), 200);
  });

  after(async () => {
    if (srv) {
      await srv.stop();
    }
  });

  describe("F1 & F2: CLI, BootConfig, /healthz & Setup Corners", () => {
    it("F1: /healthz rejects PUT, DELETE, and PATCH with 405 and Allow: GET, HEAD", async () => {
      for (const method of ["PUT", "DELETE", "PATCH"]) {
        const res = await anon.request(method, "/healthz", {});
        assertStatus(res, 405, `${method} /healthz`);
        assertHeaderIncludes(res, "allow", "GET", `${method} /healthz`);
        assertHeaderIncludes(res, "allow", "HEAD", `${method} /healthz`);
      }
    });

    it("F1: BootConfig JSON is valid on /setup (pre-init) and /console, and doctor validates secret key and uninit DB", async () => {
      const preSrv = await startVoloServer({ skipSetup: true });
      try {
        const setupHtml = await createClient(preSrv.baseUrl).get("/setup");
        assertStatus(setupHtml, 200);
        const preBoot = extractBootConfig(setupHtml.text);
        assert.equal(preBoot.mode, "setup");
        assert.ok(preBoot.version);

        const doc = await runVoloCommand(
          ["doctor", "--json", "--data-dir", preSrv.dataDir, "--db-dsn", path.join(preSrv.workDir, "never-created.db")],
          { env: { VOLO_SECRET_KEY: "bad-short-key" } },
        );
        assert.notEqual(doc.code, 0, "doctor with invalid VOLO_SECRET_KEY must exit non-zero");
      } finally {
        await preSrv.stop();
      }

      const conHtml = await anon.get("/console");
      assertStatus(conHtml, 200);
      const conBoot = extractBootConfig(conHtml.text);
      assert.equal(conBoot.mode, "console");
      assert.ok(conBoot.version);
    });

    it("F1: GET /api/admin/about/project-info?offline=1 returns cached offline telemetry and rejects POST", async () => {
      const res = await admin.get("/api/admin/about/project-info?offline=1");
      assertStatus(res, 200);
      assert.equal(res.data.online, false);
      assert.equal(res.data.live_fetched, false);
      assert.ok(res.data.admin_gzip_kb > 0);
      assert.equal(res.data.repo.full_name, "Willh92/Volo-go");

      const postRes = await admin.post("/api/admin/about/project-info", {});
      assertStatus(postRes, [404, 405], "POST /api/admin/about/project-info");
    });

    it("F2: pre-init setup handles en-US locale and invalid timezone safely without breaking boot", async () => {
      const preSrv = await startVoloServer({ skipSetup: true });
      try {
        const c = createClient(preSrv.baseUrl);
        const res = await c.setupComplete({
          admin_username: "tzadmin",
          admin_email: "tz@e2e.local",
          admin_password: "StrongPassword!123",
          site_title: "TZ Corner Blog",
          default_locale: "en-US",
          timezone: "Invalid/Timezone_XYZ",
          public_url: preSrv.baseUrl,
        });
        assertStatus(res, 200, "Invalid timezone handling");
        assertStatus(await createClient(preSrv.baseUrl).get("/"), 200);
      } finally {
        await preSrv.stop();
      }
    });

    it("F2: pre-init test-legacy-db rejects path traversal and admin routes deny unauthenticated access", async () => {
      const preSrv = await startVoloServer({ skipSetup: true });
      try {
        const c = createClient(preSrv.baseUrl);
        const trav = await c.post("/api/setup/test-legacy-db", { dsn: "../../etc/passwd" });
        assertStatus(trav, [400, 404], "test-legacy-db traversal");
        assertStatus(await c.get("/api/admin/settings"), 401, "Admin API pre-init denies anon");
      } finally {
        await preSrv.stop();
      }
    });
  });

  describe("F6, F7 & F8: Settings Clamping, Optimistic Lock, Secrets & Permalinks", () => {
    it("F6: reading.pow_difficulty_bits resets out-of-range (0/99 -> 16) and accepts 8..24", async () => {
      await withTempSettings(
        admin,
        "reading",
        { pow_difficulty_bits: 0 },
        { pow_difficulty_bits: 10 },
        async (r0) => {
          assert.equal(r0.value.pow_difficulty_bits, 16, "pow_difficulty_bits=0 resets to 16");
          const r99 = await admin.patch("/api/admin/settings/reading", { pow_difficulty_bits: 99 });
          assert.equal(r99.data.value.pow_difficulty_bits, 16, "pow_difficulty_bits=99 resets to 16");
          const r8 = await admin.patch("/api/admin/settings/reading", { pow_difficulty_bits: 8 });
          assert.equal(r8.data.value.pow_difficulty_bits, 8);
        },
      );
    });

    it("F6: site.footer_text sanitizes dangerous HTML when enable_footer_html=true and hides when false", async () => {
      await withTempSettings(
        admin,
        "site",
        {
          enable_footer_html: true,
          footer_text: '<p>SafeFooterText</p><svg onload="alert(1)"></svg><base href="https://evil.example">',
        },
        { enable_footer_html: false, footer_text: "" },
        async (res) => {
          const ft = res.value.footer_text || "";
          assert.ok(ft.includes("SafeFooterText"));
          assert.ok(!/<svg\b|<base\b/i.test(ft));

          const homeOn = await anon.get("/");
          assertStatus(homeOn, 200);
          assert.ok(homeOn.text.includes("SafeFooterText"));

          await admin.patch("/api/admin/settings/site", { enable_footer_html: false });
          const homeOff = await anon.get("/");
          assert.equal(homeOff.text.includes("SafeFooterText"), false);
        },
      );
    });

    it("F7: optimistic lock 409 Conflict across reading, seo, and appearance (expected_version, W/ ETag, bad type -> 400)", async () => {
      for (const domain of ["reading", "seo", "appearance"]) {
        const g = await admin.get(`/api/admin/settings/${domain}`);
        assertStatus(g, 200);
        const ver = g.data.version;

        const staleBody = await admin.patch(`/api/admin/settings/${domain}`, {
          expected_version: ver + 99,
        });
        assertStatus(staleBody, 409, `${domain} stale expected_version -> 409`);

        const staleIfMatch = await admin.patch(
          `/api/admin/settings/${domain}`,
          {},
          { headers: { "If-Match": `W/"${ver + 99}"` } },
        );
        assertStatus(staleIfMatch, 409, `${domain} stale weak If-Match -> 409`);
      }

      const badVerType = await admin.patch("/api/admin/settings/site", {
        expected_version: "not-an-int",
      });
      assertStatus(badVerType, 400, "Non-integer expected_version returns 400");
    });

    it("F7: seo.meta_keywords/description escape quotes/tags and /robots.txt protects /api/ and /console/", async () => {
      await withTempSettings(
        admin,
        "seo",
        {
          meta_keywords: 'go,"xss"<script>',
          meta_description: 'Desc "quote" <svg>',
          indexnow_key: "t2-seo-corner-key",
        },
        { meta_keywords: "", meta_description: "", indexnow_key: "" },
        async () => {
          const home = await anon.get("/");
          assertStatus(home, 200);
          assert.ok(!home.text.includes('content="go,"xss"<script>"'));
          assert.ok(!home.text.includes("<svg>"));

          const rob = await anon.get("/robots.txt");
          assertStatus(rob, 200);
          assert.ok(rob.text.includes("Disallow: /api/"));
          assert.ok(rob.text.includes("Disallow: /console/"));
        },
      );
    });

    it("F7: seo.post_permalink_style switches canonical URLs across all 4 styles and 301-redirects legacy URLs", async () => {
      const art = await createTempArticle(admin, {
        title: "Permalink Corner Post",
        slug: "t2-permalink-corner",
      });
      try {
        for (const style of ["/archives/{slug}", "/p/{slug}", "/{year}/{month}/{slug}", "/posts/{slug}"]) {
          await withTempSettings(
            admin,
            "seo",
            { post_permalink_style: style },
            { post_permalink_style: "/posts/{slug}" },
            async () => {
              const home = await anon.get("/");
              assertStatus(home, 200);
              if (style === "/p/{slug}") {
                assert.ok(home.text.includes("/p/t2-permalink-corner"));
                assertRedirect(await anon.get("/posts/t2-permalink-corner"), 301, "/p/t2-permalink-corner");
              } else if (style === "/archives/{slug}") {
                assert.ok(home.text.includes("/archives/t2-permalink-corner"));
                assertRedirect(await anon.get("/posts/t2-permalink-corner"), 301, "/archives/t2-permalink-corner");
              } else if (style === "/{year}/{month}/{slug}") {
                assert.match(home.text, /\/\d{4}\/\d{2}\/t2-permalink-corner/);
                assertRedirect(
                  await anon.get("/posts/t2-permalink-corner"),
                  301,
                  /\/\d{4}\/\d{2}\/t2-permalink-corner/,
                );
              } else if (style === "/posts/{slug}") {
                assert.ok(home.text.includes("/posts/t2-permalink-corner"));
                assertRedirect(await anon.get("/p/t2-permalink-corner"), 301, "/posts/t2-permalink-corner");
              }
            },
          );
        }
      } finally {
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F7: appearance.custom_css strips mixed-case javascript:/expression( and breaks out </StYlE>", async () => {
      await withTempSettings(
        admin,
        "appearance",
        {
          custom_css: "h1 { color: red; } </StYlE><script>alert(1)</script> p { background: url(JaVaScRiPt:1); width: eXpReSsIoN(1); }",
        },
        { custom_css: "" },
        async (res) => {
          const css = res.value.custom_css || "";
          assert.equal(/<\/style>/i.test(css), false);
          assert.ok(css.includes("<\\/StYlE>"));
          assert.equal(/javascript\s*:/i.test(css), false);
          assert.equal(/expression\s*\(/i.test(css), false);
        },
      );
    });

    it("F7: site.custom_head_meta strips svg/math/base/meta-refresh/xlink:href and NUL bytes", async () => {
      await withTempSettings(
        admin,
        "site",
        {
          custom_head_meta:
            '<meta name="corner-ok" content="1">\u0000<svg onload=1></svg><math><mi>a</mi></math><base href="//evil"><meta http-equiv="refresh" content="0;url=x"><a xlink:href="javascript:1"></a>',
        },
        { custom_head_meta: "" },
        async (res) => {
          const v = res.value.custom_head_meta || "";
          assert.ok(v.includes('<meta name="corner-ok" content="1">'));
          assert.ok(!v.includes("\u0000"));
          assert.ok(!/<svg\b|<math\b|<base\b|xlink:href|http-equiv\s*=\s*["']?refresh/i.test(v));
        },
      );
    });

    it("F7: feed.podcast persists cover_image_url, explicit, language, and category and returns 404 when disabled", async () => {
      await withTempSettings(
        admin,
        "feed",
        {
          podcast: {
            enabled: true,
            title: "Corner Pod",
            author: "Corner Author",
            category: "Science",
            language: "en-us",
            cover_image_url: "https://example.org/t2-pod-cover.png",
            explicit: true,
          },
        },
        { podcast: { enabled: false, cover_image_url: "" } },
        async () => {
          const pod = await anon.get("/podcast.xml");
          assertStatus(pod, 200);
          assert.ok(pod.text.includes("Corner Pod"));
          assert.ok(pod.text.includes("Corner Author"));
          assert.ok(pod.text.includes("<itunes:explicit>true</itunes:explicit>"));
          assert.ok(pod.text.includes("<language>en-us</language>"));
          assert.ok(pod.text.includes("Science"));
          assert.ok(pod.text.includes("https://example.org/t2-pod-cover.png"));
        },
      );
      assertStatus(await anon.get("/podcast.xml"), 404);
    });

    it("F5 & F6: disabled toggles (enable_instant_search, enable_raw_markdown, enable_llms_txt) return 404 and restore to 200", async () => {
      const art = await createTempArticle(admin, {
        title: "Disabled Toggle Probe Post",
        slug: "t2-disabled-toggle-post",
        content_md: "DisabledToggleProbeToken body.",
      });
      try {
        assertStatus(await anon.get("/api/v1/search?q=DisabledToggleProbeToken"), 200);
        assertStatus(await anon.get(`/posts/${art.slug}.md`), 200);
        assertStatus(await anon.get("/llms.txt"), 200);
        assertStatus(await anon.get("/llms-full.txt"), 200);

        await withTempSettings(
          admin,
          "reading",
          { enable_instant_search: false },
          { enable_instant_search: true },
          async () => {
            assertStatus(
              await anon.get("/api/v1/search?q=DisabledToggleProbeToken"),
              404,
              "enable_instant_search=false must return 404",
            );
          },
        );
        assertStatus(await anon.get("/api/v1/search?q=DisabledToggleProbeToken"), 200);

        await withTempSettings(
          admin,
          "feed",
          { enable_raw_markdown: false, enable_llms_txt: false },
          { enable_raw_markdown: true, enable_llms_txt: true },
          async () => {
            assertStatus(
              await anon.get(`/posts/${art.slug}.md`),
              404,
              "enable_raw_markdown=false must return 404",
            );
            assertStatus(await anon.get("/llms.txt"), 404, "enable_llms_txt=false /llms.txt must return 404");
            assertStatus(
              await anon.get("/llms-full.txt"),
              404,
              "enable_llms_txt=false /llms-full.txt must return 404",
            );
          },
        );
        assertStatus(await anon.get(`/posts/${art.slug}.md`), 200);
        assertStatus(await anon.get("/llms.txt"), 200);
        assertStatus(await anon.get("/llms-full.txt"), 200);
      } finally {
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F8: settings secret clearing requires clear_secrets:[...] and ignores __VOLO_CLEAR_SECRET__", async () => {
      try {
        await admin.patch("/api/admin/settings/storage", {
          s3_secret_key: "initial-s3-secret",
        });

        const sentinelAttempt = await admin.patch("/api/admin/settings/storage", {
          s3_secret_key: "__VOLO_CLEAR_SECRET__",
        });
        assertStatus(sentinelAttempt, 200);
        assert.equal(
          sentinelAttempt.data.value.s3_secret_key_set,
          true,
          "Settings PATCH must NOT clear secret via __VOLO_CLEAR_SECRET__ string",
        );

        const realClear = await admin.patch("/api/admin/settings/storage", {
          clear_secrets: ["s3_secret_key"],
        });
        assertStatus(realClear, 200);
        assert.equal(realClear.data.value.s3_secret_key_set, false);
      } finally {
        await admin.patch("/api/admin/settings/storage", { clear_secrets: ["s3_secret_key"] });
      }
    });

    it("F8: backup clamps local_interval_hours/local_keep_count and rejects invalid restore archive", async () => {
      await withTempSettings(
        admin,
        "backup",
        { local_interval_hours: 9999, local_keep_count: 0 },
        { local_interval_hours: 24, local_keep_count: 7 },
        async (res) => {
          assert.equal(res.value.local_interval_hours, 720, "local_interval_hours > 720 clamps to 720");
          assert.equal(res.value.local_keep_count, 1, "local_keep_count < 1 clamps to 1");
        },
      );

      const badRestore = new FormData();
      badRestore.append("file", new Blob(["corrupted-not-a-zip"], { type: "application/zip" }), "bad.zip");
      const restoreRes = await admin.post("/api/admin/backup/restore", badRestore);
      assertStatus(restoreRes, 400, "Corrupted backup restore rejected");
    });
  });

  describe("F13 & F14: Plugin Circuit Breaker, Rate Limits, Permissions & Theme Corners", () => {
    it("F13: nonexistent plugin ID returns 400/404 across lifecycle and public mount endpoints", async () => {
      assertStatus(await admin.post("/api/admin/plugins/no-such-plugin/enable"), [400, 404]);
      assertStatus(await admin.post("/api/admin/plugins/no-such-plugin/disable"), [400, 404]);
      assertStatus(await admin.patch("/api/admin/plugins/no-such-plugin/config", { a: 1 }), [400, 404]);
      assertStatus(await admin.get("/api/admin/plugins/no-such-plugin/export"), [400, 404]);
      assertStatus(await admin.post("/api/admin/plugins/no-such-plugin/reset-circuit"), [400, 404]);
      assertStatus(await anon.get("/plugins/no-such-plugin/api/ping"), 404);
    });

    it("F13: plugin circuit breaker opens after 5 consecutive failures, blocks /enable (400), and recovers via reset-circuit", async () => {
      const zipBuf = buildStoreZip([
        {
          name: "manifest.json",
          data: JSON.stringify({
            id: "t2-breaker-plugin",
            version: "1.0.0",
            min_host_version: "0.1.0",
            runtime: "goja",
            entry: "main.js",
            name: { "en-US": "Breaker Probe" },
            permissions: ["http:routes"],
          }),
        },
        {
          name: "main.js",
          data: 'function onRequest() { throw new Error("intentional boom"); }\nmodule.exports = { onRequest: onRequest };\n',
        },
      ]);

      const form = new FormData();
      form.append("file", new Blob([zipBuf], { type: "application/zip" }), "breaker.zip");
      assertStatus(await admin.post("/api/admin/plugins/install?enable=1", form), 201);

      try {
        for (let i = 1; i <= 5; i += 1) {
          const r = await anon.get("/plugins/t2-breaker-plugin/api/fail");
          assertStatus(r, 503, `Failure ${i}/5 returns 503`);
        }

        const list = await admin.get("/api/admin/plugins");
        const p = list.data.items.find((x) => x.id === "t2-breaker-plugin");
        assert.equal(p.circuit_state, "open");
        assert.equal(p.is_enabled, false);
        assert.equal(p.failure_count, 5);

        assertStatus(await anon.get("/plugins/t2-breaker-plugin/api/fail"), 404, "Open circuit hides plugin API");
        assertStatus(
          await admin.post("/api/admin/plugins/t2-breaker-plugin/enable"),
          400,
          "Cannot enable plugin while circuit is OPEN",
        );

        const rst = await admin.post("/api/admin/plugins/t2-breaker-plugin/reset-circuit");
        assertStatus(rst, 200);
        assert.equal(rst.data.plugin.circuit_state, "closed");
        assert.equal(rst.data.plugin.failure_count, 0);

        const reEnable = await admin.post("/api/admin/plugins/t2-breaker-plugin/enable");
        assertStatus(reEnable, 200);
        assert.equal(reEnable.data.plugin.is_enabled, true);
      } finally {
        await admin.del("/api/admin/plugins/t2-breaker-plugin").catch(() => {});
      }
    });

    it("F13: plugin config secret clearing supports both __VOLO_CLEAR_SECRET__ and __clear_secrets", async () => {
      const zipBuf = buildStoreZip([
        {
          name: "manifest.json",
          data: JSON.stringify({
            id: "t2-secret-plugin",
            version: "1.0.0",
            min_host_version: "0.1.0",
            runtime: "goja",
            entry: "main.js",
            name: { "en-US": "Secret Probe" },
            config_schema: [
              { key: "api_key", type: "secret", label: { "en-US": "API Key" } },
              { key: "token_b", type: "secret", label: { "en-US": "Token B" } },
            ],
          }),
        },
        { name: "main.js", data: "module.exports = {};\n" },
      ]);
      const form = new FormData();
      form.append("file", new Blob([zipBuf], { type: "application/zip" }), "secret.zip");
      assertStatus(await admin.post("/api/admin/plugins/install", form), 201);

      try {
        const setRes = await admin.patch("/api/admin/plugins/t2-secret-plugin/config", {
          api_key: "sec-1",
          token_b: "sec-2",
        });
        assertStatus(setRes, 200);
        assert.equal(setRes.data.plugin.secret_keys_set.api_key, true);
        assert.equal(setRes.data.plugin.secret_keys_set.token_b, true);

        const clr1 = await admin.patch("/api/admin/plugins/t2-secret-plugin/config", {
          api_key: "__VOLO_CLEAR_SECRET__",
        });
        assertStatus(clr1, 200);
        assert.equal(clr1.data.plugin.secret_keys_set.api_key, false);
        assert.equal(clr1.data.plugin.secret_keys_set.token_b, true);

        const clr2 = await admin.patch("/api/admin/plugins/t2-secret-plugin/config", {
          __clear_secrets: ["token_b"],
        });
        assertStatus(clr2, 200);
        assert.equal(clr2.data.plugin.secret_keys_set.token_b, false);
      } finally {
        await admin.del("/api/admin/plugins/t2-secret-plugin").catch(() => {});
      }
    });

    it("F13: kanbanniang /pat enforces 20/min rate limit (429) and /list returns 401 on non-admin", async () => {
      assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
      try {
        assertStatus(await anon.get("/plugins/kanbanniang/api/list"), 401, "Anon /list returns 401");

        const { user, password } = await createTempUser(admin, {
          role: "editor",
          current_password: srv.adminCreds.password,
        });
        try {
          const ed = createClient(srv.baseUrl);
          assertStatus(await ed.login(user.username, password), 200);
          assertStatus(await ed.get("/plugins/kanbanniang/api/list"), 401, "Editor non-admin /list returns 401");
        } finally {
          await deleteUserSafe(admin, user.id);
        }

        const patClient = createClient(srv.baseUrl, { headers: { "X-Forwarded-For": "203.0.113.55" } });
        for (let i = 1; i <= 20; i += 1) {
          const r = await patClient.post("/plugins/kanbanniang/api/pat");
          assertStatus(r, 200, `kanbanniang pat call ${i}/20`);
        }
        const limited = await patClient.post("/plugins/kanbanniang/api/pat");
        assertStatus(limited, 429, "kanbanniang pat 21st call within 60s returns 429");
        assertHeaderIncludes(limited, "cache-control", "no-store");
      } finally {
        await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
      }
    });

    it("F13: sensitive-words /api/* returns 403 on anon & editor non-admin and allows admin DELETE remote-cache", async () => {
      assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);
      const { user, password } = await createTempUser(admin, {
        role: "editor",
        current_password: srv.adminCreds.password,
      });
      try {
        assertStatus(await anon.get("/plugins/sensitive-words/api/status"), 403);
        assertStatus(await anon.post("/plugins/sensitive-words/api/sync"), 403);

        const ed = createClient(srv.baseUrl);
        assertStatus(await ed.login(user.username, password), 200);
        assertStatus(await ed.get("/plugins/sensitive-words/api/status"), 403, "Editor denied sensitive-words /status");

        const clr = await admin.del("/plugins/sensitive-words/api/remote-cache");
        assertStatus(clr, 200, "Admin DELETE sensitive-words remote-cache");
        assert.equal(clr.data.ok, true);
      } finally {
        await deleteUserSafe(admin, user.id);
        await admin.post("/api/admin/plugins/sensitive-words/disable").catch(() => {});
      }
    });

    it("F14: theme activate rejects unknown theme (400/404) and config endpoint persists partial updates", async () => {
      assertStatus(
        await admin.post("/api/admin/themes/activate", { theme_id: "nonexistent-theme-xyz", target: "desktop" }),
        [400, 404],
      );
      const seedCfg = await admin.patch("/api/admin/themes/volo-studio/config", {
        header_nav_primary_count: 4,
      });
      assertStatus(seedCfg, 200, "Seed partial theme config");
      const emptyCfg = await admin.put("/api/admin/themes/volo-studio/config", {});
      assertStatus(emptyCfg, 200, "Empty partial PUT theme config succeeds");
      const appAfter = await admin.get("/api/admin/settings/appearance");
      assertStatus(appAfter, 200);
      assert.equal(
        appAfter.data.value?.theme_configs?.["volo-studio"]?.header_nav_primary_count,
        4,
        "Empty PUT theme config must preserve existing theme_configs keys",
      );
    });

    it("F14: theme secret config clearing supports __VOLO_CLEAR_SECRET__ and empty string", async () => {
      const zipBuf = buildStoreZip([
        {
          name: "theme.json",
          data: JSON.stringify({
            id: "t2-secret-theme",
            name: { "en-US": "Secret Theme" },
            version: "1.0.0",
            min_host_version: "0.1.0",
            config_schema: [
              { key: "analytics_secret", type: "secret", label: { "en-US": "Secret A" } },
              { key: "webhook_secret", type: "secret", label: { "en-US": "Secret B" } },
            ],
          }),
        },
        { name: "templates/index.html", data: "<html><body>T2 Theme</body></html>" },
      ]);

      const form = new FormData();
      form.append("file", new Blob([zipBuf], { type: "application/zip" }), "t2-secret-theme.zip");
      const inst = await admin.post("/api/admin/themes/install", form);
      assertStatus(inst, 201, "Install synthetic theme");

      try {
        const setRes = await admin.put("/api/admin/themes/t2-secret-theme/config", {
          analytics_secret: "theme-sec-1",
          webhook_secret: "theme-sec-2",
        });
        assertStatus(setRes, 200);

        const clr1 = await admin.put("/api/admin/themes/t2-secret-theme/config", {
          analytics_secret: "__VOLO_CLEAR_SECRET__",
        });
        assertStatus(clr1, 200);
        assert.equal(clr1.data.theme?.config_secrets_set?.analytics_secret ?? false, false);

        const clr2 = await admin.put("/api/admin/themes/t2-secret-theme/config", {
          webhook_secret: "",
        });
        assertStatus(clr2, 200);
        assert.equal(clr2.data.theme?.config_secrets_set?.webhook_secret ?? false, false);
      } finally {
        await admin.del("/api/admin/themes/t2-secret-theme").catch(() => {});
      }
    });

    it("F14: all 6 accents x both themes (volo-studio, volo-journal) emit matching data-volo-skin & data-accent", async () => {
      const accents = ["forest", "ocean", "teal", "amber", "plum", "rose"];
      try {
        for (const theme of ["volo-studio", "volo-journal"]) {
          const expectedSkin = theme === "volo-studio" ? "studio" : "volo-journal";
          assertStatus(await admin.post("/api/admin/themes/activate", { theme_id: theme, target: "both" }), 200);
          for (const accent of accents) {
            assertStatus(await admin.patch("/api/admin/settings/appearance", { accent }), 200);
            const html = await anon.get("/");
            assertStatus(html, 200);
            assert.ok(html.text.includes(`data-volo-skin="${expectedSkin}"`));
            assert.ok(html.text.includes(`data-accent="${accent}"`));
          }
        }
      } finally {
        await admin.post("/api/admin/themes/activate", { theme_id: "volo-studio", target: "both" }).catch(() => {});
        await admin.patch("/api/admin/settings/appearance", { accent: "forest" }).catch(() => {});
      }
    });

    it("F14: volo-journal theme_configs toggle journal_motto, hero_badge, show_colophon, and show_recent_posts", async () => {
      const art = await createTempArticle(admin, {
        title: "Journal Recent Corner Post",
        slug: "t2-journal-recent-post",
        content_md: "Journal recent post body.",
      });
      try {
        assertStatus(await admin.post("/api/admin/themes/activate", { theme_id: "volo-journal", target: "both" }), 200);
        assertStatus(
          await admin.put("/api/admin/themes/volo-journal/config", {
            journal_motto: "CORNER_MOTTO_ON",
            hero_badge: "CORNER_BADGE_ON",
            show_colophon: true,
            show_recent_posts: true,
          }),
          200,
        );
        const home1 = await anon.get("/");
        assertStatus(home1, 200);
        assert.ok(home1.text.includes("CORNER_MOTTO_ON"));
        assert.ok(home1.text.includes("CORNER_BADGE_ON"));
        assert.ok(home1.text.includes("journal-colophon"));

        const arch1 = await anon.get("/archives");
        assertStatus(arch1, 200);
        assert.ok(
          arch1.text.includes("studio-side-articles") && arch1.text.includes("Journal Recent Corner Post"),
          "show_recent_posts=true must render recent-post sidebar card on non-home pages",
        );

        assertStatus(
          await admin.put("/api/admin/themes/volo-journal/config", {
            show_colophon: false,
            show_recent_posts: false,
          }),
          200,
        );
        const home2 = await anon.get("/");
        assert.equal(home2.text.includes('class="journal-colophon"'), false);

        const arch2 = await anon.get("/archives");
        assertStatus(arch2, 200);
        assert.equal(
          arch2.text.includes("studio-side-articles"),
          false,
          "show_recent_posts=false must hide recent-post sidebar cards",
        );
      } finally {
        await admin.put("/api/admin/themes/volo-journal/config", {
          show_colophon: true,
          show_recent_posts: true,
        }).catch(() => {});
        await admin.post("/api/admin/themes/activate", { theme_id: "volo-studio", target: "both" }).catch(() => {});
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F14: kanbanniang rejects unknown config keys and safely escapes greeting_en on public SSR", async () => {
      assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
      try {
        assertStatus(
          await admin.patch("/api/admin/plugins/kanbanniang/config", { unknown_bad_key: "x" }),
          400,
          "kanbanniang rejects unknown config keys",
        );
        assertStatus(
          await admin.patch("/api/admin/plugins/kanbanniang/config", {
            position: "left",
            hide_on_mobile: false,
            greeting_en: 'Hello <script>alert(1)</script> Reader',
          }),
          200,
        );
        const home = await anon.get("/");
        assertStatus(home, 200);
        assert.ok(home.text.includes("volo-kbn-left"));
        assert.ok(home.text.includes("Hello &lt;script&gt;alert(1)&lt;/script&gt; Reader"));
        assert.equal(home.text.includes("<script>alert(1)</script>"), false);
      } finally {
        await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
      }
    });
  });

  describe("F15 & F16: Markdown Corners, Render Preview & Edge SPA Corners", () => {
    it("F15: long unbroken 250-char code lines, unknown fences, and {hl_lines=...} render cleanly without attribute leaks", async () => {
      const longToken = "x".repeat(260);
      const md = [
        "```go {hl_lines=[1,2]}",
        `var longLine = "${longToken}"`,
        "```",
        "",
        "```go{hl_lines=[1]}",
        "func ok() {}",
        "```",
        "",
        "```unknownlang999",
        "plain fallback line",
        "```",
      ].join("\n");

      const prev = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(prev, 200);
      assert.ok(prev.data.html.includes('data-lang="go"'));
      assert.ok(prev.data.html.includes(longToken));
      assert.ok(!prev.data.html.includes('hl_lines=[1,2]'));
      assert.ok(prev.data.html.includes("plain fallback line"));
    });

    it("F15: CJK/Unicode heading IDs, duplicate -2 suffixes, literal --flag, ==mark==, ^sup^, and ~sub~ corners", async () => {
      const md = [
        "## 架构设计与概览",
        "",
        "## 架构设计与概览",
        "",
        "CLI `--with-libzip` and `--prefix` stay literal.",
        "",
        "Format ==highlighted== and ==**bold mark**== while `a == b == c` stays plain.",
        "",
        "Formula H~2~O and X^2^ while `/~user/file~` and `2^3 + 2^4` stay literal.",
      ].join("\n");

      const prev = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(prev, 200);
      assert.ok(prev.data.html.includes('id="架构设计与概览"'));
      assert.ok(prev.data.html.includes('id="架构设计与概览-2"'));
      assert.ok(prev.data.html.includes("--with-libzip"));
      assert.ok(prev.data.html.includes("--prefix"));
      assert.ok(prev.data.html.includes("<mark>highlighted</mark>"));
      assert.ok(prev.data.html.includes("<sub>2</sub>"));
      assert.ok(prev.data.html.includes("<sup>2</sup>"));
      assert.ok(prev.data.html.includes("/~user/file~"));
    });

    it("F15: nested blockquotes, GitHub alerts, .volo-table-scroll, and list-to-blockquote indented code", async () => {
      const md = [
        "- list item",
        "",
        "> quote interruption",
        "",
        "    indented code block must remain code",
        "",
        "> [!CAUTION]",
        "> Nested **caution** alert.",
        "",
        "| H1 | H2 |",
        "| --- | --- |",
        "| C1 | C2 |",
      ].join("\n");

      const prev = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(prev, 200);
      assert.ok(prev.data.html.includes("indented code block must remain code"));
      assert.ok(prev.data.html.includes('data-callout="caution"'));
      assert.ok(prev.data.html.includes('<div class="volo-table-scroll"><table>'));
    });

    it("F15: unclosed :::lock fails closed across HTML/search/feeds/llms/.md, currency vs math holds, and malformed [toc]/:::book degrade safely", async () => {
      const md = [
        "UNCLOSED_PUBLIC_ANCHOR_OK",
        "",
        "    [toc]",
        "",
        "> [toc]",
        "",
        ":::book",
        ":::",
        "",
        ':::book slug="unknown-missing-book"',
        ":::",
        "",
        "Budget $100 and $200 today, formula $\\alpha$ and 预算$100，公式$\\beta$ here.",
        "",
        ':::lock pwd="unclosed-secret"',
        "UNCLOSED_SECRET_SHOULD_FAIL_CLOSED",
      ].join("\n");

      const bqMd = [
        '> :::lock pwd="bq-unclosed"',
        "> UNCLOSED_BQ_SECRET_SHOULD_FAIL_CLOSED",
        "",
        "BQ_OUTSIDE_PUBLIC_OK",
      ].join("\n");

      const art = await createTempArticle(admin, {
        title: "Unclosed Lock Top Post",
        slug: "t2-unclosed-lock-post",
        content_md: md,
      });
      const bqArt = await createTempArticle(admin, {
        title: "Unclosed Lock Blockquote Post",
        slug: "t2-unclosed-lock-bq-post",
        content_md: bqMd,
      });
      try {
        const pub = await anon.get(`/posts/${art.slug}`);
        assertStatus(pub, 200);
        assert.ok(pub.text.includes('class="volo-lock-card"'));
        assert.equal(pub.text.includes("UNCLOSED_SECRET_SHOULD_FAIL_CLOSED"), false);
        assert.ok(pub.text.includes('data-book-slug="unknown-missing-book"'));
        assert.ok(pub.text.includes("volo-math-inline"));
        assert.equal(pub.text.includes('<span class="volo-math-inline">$100'), false);

        const bqPub = await anon.get(`/posts/${bqArt.slug}`);
        assertStatus(bqPub, 200);
        assert.ok(bqPub.text.includes("BQ_OUTSIDE_PUBLIC_OK"));
        assert.equal(bqPub.text.includes("UNCLOSED_BQ_SECRET_SHOULD_FAIL_CLOSED"), false);

        for (const secret of ["UNCLOSED_SECRET_SHOULD_FAIL_CLOSED", "UNCLOSED_BQ_SECRET_SHOULD_FAIL_CLOSED"]) {
          const searchRes = await anon.get(`/api/v1/search?q=${encodeURIComponent(secret)}`);
          assertStatus(searchRes, 200);
          assert.equal(searchRes.data.hits.length, 0, `Search must not leak ${secret}`);
        }

        const nonHtmlPaths = [
          "/rss.xml",
          "/atom.xml",
          "/feed.json",
          "/llms.txt",
          "/llms-full.txt",
          `/posts/${art.slug}.md`,
          `/posts/${bqArt.slug}.md`,
        ];
        for (const p of nonHtmlPaths) {
          const r = await anon.get(p);
          assertStatus(r, 200, `GET ${p}`);
          assert.equal(
            r.text.includes("UNCLOSED_SECRET_SHOULD_FAIL_CLOSED"),
            false,
            `${p} must not leak top-level unclosed :::lock`,
          );
          assert.equal(
            r.text.includes("UNCLOSED_BQ_SECRET_SHOULD_FAIL_CLOSED"),
            false,
            `${p} must not leak blockquote-wrapped unclosed :::lock`,
          );
        }
      } finally {
        await deleteArticleSafe(admin, art.id);
        await deleteArticleSafe(admin, bqArt.id);
      }
    });

    it("F16: POST /api/admin/render/preview handles preserve_directives=true vs false and rejects malformed JSON", async () => {
      assertStatus(
        await admin.post("/api/admin/render/preview", "{bad", {
          headers: { "Content-Type": "application/json" },
        }),
        400,
      );

      const md = '[toc]\n\n# H1\n\n:::lock pwd="k"\nSecret\n:::\n';
      const pTrue = await admin.post("/api/admin/render/preview", {
        content_md: md,
        preserve_directives: true,
      });
      assertStatus(pTrue, 200);
      assert.ok(pTrue.data.html.includes("data-volo-md-b64="));

      const pFalse = await admin.post("/api/admin/render/preview", {
        content_md: md,
        preserve_directives: false,
      });
      assertStatus(pFalse, 200);
      assert.ok(pFalse.data.html.includes("Secret"));
    });

    it("F16: headless Edge #about error banner + Retry recovery and telemetry verification", async () => {
      await launchEdge().then(async (browser) => {
        try {
          await withPage(
            browser,
            {
              width: 1280,
              height: 800,
              ignoreConsolePatterns: [/500/, /Internal Server Error/, /Failed to load resource/],
            },
            async (page, diag) => {
              let failFirst = true;
              await page.route("**/api/admin/about/project-info*", async (route) => {
                if (failFirst) {
                  failFirst = false;
                  await route.fulfill({
                    status: 500,
                    contentType: "application/json",
                    body: JSON.stringify({ error: "simulated outage" }),
                  });
                  return;
                }
                await route.continue();
              });

              await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#about");
              const banner = page.locator('[role="alert"].volo-draft-banner');
              await banner.waitFor({ state: "visible", timeout: 10000 });

              const retryOk = page.waitForResponse(
                (r) => r.url().includes("/api/admin/about/project-info") && r.status() === 200,
              );
              await banner.locator("button.volo-btn.volo-btn-sm").click();
              await retryOk;
              await banner.waitFor({ state: "hidden", timeout: 10000 });
              diag.assertClean();
            },
          );
        } finally {
          await browser.close();
        }
      });
    });

    it("F16: headless Edge CM6 editor preserves --flag, CJK, math vs currency, and ==mark== across IR/SV/WYSIWYG", async () => {
      await launchEdge().then(async (browser) => {
        try {
          await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
            await waitForEditorReady(page, () =>
              loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor"),
            );

            const cmContent = page.locator(".volo-cm-host .cm-editor .cm-content");
            await cmContent.click();
            await page.keyboard.insertText(
              "## 边界测试\n\nUse `--flag` and ==mark== with $100 and $\\alpha$.\n",
            );

            const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
            const svPromise = page.waitForResponse(
              (r) => r.url().includes("/api/admin/render/preview") && r.status() === 200,
            );
            await modeTabs.nth(2).click();
            await svPromise;

            const previewPane = page.locator(".volo-ed-body.mode-sv .volo-preview-pane");
            const previewText = (await previewPane.textContent()) || "";
            const previewHtml = (await previewPane.innerHTML()) || "";
            assert.ok(previewText.includes("--flag"));
            assert.ok(previewText.includes("边界测试"));
            assert.ok(previewText.includes("$100"));
            assert.ok(previewHtml.includes("volo-math-inline"));
            assert.ok(previewHtml.includes("<mark>mark</mark>"));

            await modeTabs.nth(1).click();
            const wysiwyg = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
            await wysiwyg.waitFor({ state: "visible" });
            const wysiwygText = (await wysiwyg.textContent()) || "";
            const wysiwygHtml = (await wysiwyg.innerHTML()) || "";
            assert.ok(wysiwygText.includes("--flag"));
            assert.ok(wysiwygText.includes("边界测试"));
            assert.ok(wysiwygText.includes("$100"));
            assert.ok(wysiwygHtml.includes("<mark>mark</mark>"));
            assert.ok(wysiwygHtml.includes("volo-math-inline") || wysiwygHtml.includes("data-math-src"));

            await modeTabs.nth(0).click();
            await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
            await page.locator(".volo-cm-host .cm-editor .cm-line", { hasText: "--flag" }).click();
            const roundTripText = (await page.locator(".volo-cm-host .cm-editor .cm-content").textContent()) || "";
            assert.ok(roundTripText.includes("--flag"), `Round-trip lost --flag: ${roundTripText}`);
            assert.ok(roundTripText.includes("边界测试"), `Round-trip lost CJK heading: ${roundTripText}`);
            assert.ok(roundTripText.includes("$100"), `Round-trip lost $100: ${roundTripText}`);
            assert.ok(roundTripText.includes("$\\alpha$"), `Round-trip lost $\\alpha$: ${roundTripText}`);
            assert.ok(roundTripText.includes("==mark=="), `Round-trip lost ==mark==: ${roundTripText}`);
            diag.assertClean();
          });
        } finally {
          await browser.close();
        }
      });
    });
  });
});
