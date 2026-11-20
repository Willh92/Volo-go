import assert from "node:assert/strict";
import path from "node:path";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertNoXSS,
  assertSSRFRejected,
  assertStatus,
} from "../lib/assertions.mjs";
import { launchEdge, withPage } from "../lib/browser.mjs";
import { createClient, generateTOTP, solvePoW } from "../lib/client.mjs";
import { runVoloCommand, startVoloServer } from "../lib/server.mjs";
import {
  MINIMAL_PNG_BUFFER,
  buildStoreZip,
  createTempArticle,
  createTempUser,
  deleteArticleSafe,
  deleteUserSafe,
  extractBootConfig,
  loginConsoleViaUI,
  submitGuestComment,
  waitForEditorReady,
} from "./_helpers.mjs";

describe("Tier 2 — Security, SSRF, XSS & Auth Boundaries", () => {
  let srv;
  let admin;

  before(async () => {
    srv = await startVoloServer({
      setupPayload: {
        site_title: "Tier2 Security E2E",
        default_locale: "en-US",
      },
    });
    admin = createClient(srv.baseUrl);
    const loginRes = await admin.login(srv.adminCreds.username, srv.adminCreds.password);
    assertStatus(loginRes, 200, "Admin login");
  });

  after(async () => {
    if (srv) {
      await srv.stop();
    }
  });

  describe("F1 & F2: CLI, Boot, Setup Lockout & Pre-Init Validation", () => {
    it("F1: invalid CLI subcommand/flags return non-zero and doctor fails on non-directory data-dir", async () => {
      const badFlag = await runVoloCommand(["serve", "--nonexistent-flag-xyz"]);
      assert.notEqual(badFlag.code, 0, "Invalid CLI flag must exit non-zero");

      const fileAsDataDir = path.join(srv.dataDir, "volo.db");
      const docBadDir = await runVoloCommand([
        "doctor",
        "--json",
        "--data-dir",
        fileAsDataDir,
      ]);
      assert.notEqual(docBadDir.code, 0, "doctor with regular file as --data-dir must exit non-zero");

      const missingDir = path.join(srv.workDir, "does-not-exist-dir");
      const docMissing = await runVoloCommand([
        "doctor",
        "--json",
        "--data-dir",
        missingDir,
        "--db-dsn",
        path.join(missingDir, "volo.db"),
      ]);
      assert.equal(docMissing.code, 0);
      const rep = JSON.parse(docMissing.stdout);
      assert.ok(
        rep.checks.some((c) => c.status === "warn"),
        "doctor on uninitialized missing db/dir should report warning checks",
      );
    });

    it("F1: /console BootConfig never leaks secrets and about/project-info enforces staff admin auth", async () => {
      const anon = createClient(srv.baseUrl);
      const consoleHtml = await anon.get("/console");
      assertStatus(consoleHtml, 200);
      const boot = extractBootConfig(consoleHtml.text);
      assert.equal(boot.mode, "console");
      assert.equal(boot.secret_key, undefined);
      assert.equal(boot.password, undefined);

      assertStatus(await anon.get("/api/admin/about/project-info?offline=1"), 401);

      const { user, password } = await createTempUser(admin, {
        role: "reader",
        current_password: srv.adminCreds.password,
      });
      try {
        const reader = createClient(srv.baseUrl);
        const rLogin = await reader.post("/api/v1/reader/login", {
          ident: user.username,
          password,
        });
        assertStatus(rLogin, 200, "Reader login");
        assertStatus(await reader.get("/api/admin/about/project-info?offline=1"), 403);
      } finally {
        await deleteUserSafe(admin, user.id);
      }
    });

    it("F2: post-init lockout returns 404 across GET /setup and all /api/setup/* routes", async () => {
      const anon = createClient(srv.baseUrl);
      const endpoints = [
        ["GET", "/setup"],
        ["GET", "/setup/"],
        ["GET", "/api/setup/status"],
        ["POST", "/api/setup/test-db", { driver: "sqlite", dsn: "x.db" }],
        ["POST", "/api/setup/configure-db", { driver: "sqlite", dsn: "x.db" }],
        ["POST", "/api/setup/test-legacy-db", { dsn: "legacy.db" }],
        ["POST", "/api/setup/import-legacy", { dsn: "legacy.db" }],
        ["POST", "/api/setup/migrate-db", { driver: "sqlite", dsn: "x.db" }],
        [
          "POST",
          "/api/setup/complete",
          { admin_username: "hacker", admin_email: "h@e2e.local", admin_password: "Password!123" },
        ],
      ];

      for (const [method, url, body] of endpoints) {
        const res = await anon.request(method, url, body);
        assertStatus(res, 404, `Post-init ${method} ${url}`);
      }
    });

    it("F2: pre-init setup/complete rejects malformed JSON, invalid fields, and cross-origin", async () => {
      const preSrv = await startVoloServer({ skipSetup: true });
      try {
        const client = createClient(preSrv.baseUrl);

        const badJson = await client.post("/api/setup/complete", "{bad-json", {
          headers: { "Content-Type": "application/json" },
        });
        assertStatus(badJson, 400, "Malformed JSON on /api/setup/complete");

        const shortUser = await client.post("/api/setup/complete", {
          admin_username: "a",
          admin_email: "admin@e2e.local",
          admin_password: "StrongPassword!123",
        });
        assertStatus(shortUser, 400, "Too short admin_username");

        const weakPwd = await client.post("/api/setup/complete", {
          admin_username: "validadmin",
          admin_email: "admin@e2e.local",
          admin_password: "123",
        });
        assertStatus(weakPwd, 400, "Weak admin_password");

        const badEmail = await client.post("/api/setup/complete", {
          admin_username: "validadmin",
          admin_email: "invalid-email",
          admin_password: "StrongPassword!123",
        });
        assertStatus(badEmail, 400, "Invalid admin_email");

        const crossOrigin = await client.post(
          "/api/setup/complete",
          {
            admin_username: "validadmin",
            admin_email: "admin@e2e.local",
            admin_password: "StrongPassword!123",
          },
          { headers: { Origin: "https://attacker.example" } },
        );
        assertStatus(crossOrigin, 403, "Cross-origin setup/complete");
      } finally {
        await preSrv.stop();
      }
    });

    it("F2: pre-init setup/test-db rejects SQLite traversal, unsupported driver, and DB SSRF targets", async () => {
      const preSrv = await startVoloServer({ skipSetup: true });
      try {
        const client = createClient(preSrv.baseUrl);

        const badDrv = await client.post("/api/setup/test-db", {
          driver: "oracle",
          dsn: "oracle://127.0.0.1/db",
        });
        assertStatus(badDrv, 400, "Unsupported DB driver");

        const trav = await client.post("/api/setup/test-db", {
          driver: "sqlite",
          dsn: "../../outside.db",
        });
        assertStatus(trav, 400, "Path traversal sqlite DSN");

        for (const dsn of [
          "postgres://u:p@127.0.0.1:5432/volo?sslmode=disable",
          "postgres://u:p@169.254.169.254:5432/volo?sslmode=disable",
          "mysql://u:p@tcp(10.0.0.1:3306)/volo",
        ]) {
          const driver = dsn.startsWith("mysql") ? "mysql" : "postgres";
          const res = await client.post("/api/setup/test-db", { driver, dsn });
          assertSSRFRejected(res, `Pre-init test-db SSRF (${dsn})`);
        }
      } finally {
        await preSrv.stop();
      }
    });
  });

  describe("F3: Auth, CSRF, Cross-Login, PAT Boundaries & Malformed JSON", () => {
    it("F3: missing, malformed, or invalid session cookies and Bearer tokens return 401", async () => {
      const anon = createClient(srv.baseUrl);
      assertStatus(await anon.get("/api/admin/overview"), 401, "No auth");
      assertStatus(await anon.get("/api/admin/settings"), 401, "No auth settings");

      const badSid = createClient(srv.baseUrl, {
        headers: { Cookie: "__volo_sid=invalid-session-token-xyz" },
      });
      assertStatus(await badSid.get("/api/admin/me"), 401, "Invalid __volo_sid");

      const badPat = admin.withBearer("volo_pat_nonexistent_token_123456");
      assertStatus(await badPat.get("/api/admin/articles"), 401, "Invalid PAT bearer");
    });

    it("F3: reader role with valid session receives 403 on all /api/admin/* endpoints", async () => {
      const { user, password } = await createTempUser(admin, {
        role: "reader",
        current_password: srv.adminCreds.password,
      });
      try {
        const reader = createClient(srv.baseUrl);
        const loginRes = await reader.post("/api/v1/reader/login", {
          ident: user.username,
          password,
        });
        assertStatus(loginRes, 200, "Reader login");

        assertStatus(await reader.get("/api/admin/overview"), 403, "Reader -> overview");
        assertStatus(await reader.get("/api/admin/settings"), 403, "Reader -> settings");
        assertStatus(await reader.get("/api/admin/articles"), 403, "Reader -> articles");
        assertStatus(
          await reader.post("/api/admin/articles", { title: "X", content_md: "Y" }),
          403,
          "Reader -> POST articles",
        );
      } finally {
        await deleteUserSafe(admin, user.id);
      }
    });

    it("F3: reader/staff cross-login is strictly rejected with 401 on opposite login endpoints", async () => {
      const { user: readerUser, password: readerPwd } = await createTempUser(admin, {
        role: "reader",
        current_password: srv.adminCreds.password,
      });
      const { user: editorUser, password: editorPwd } = await createTempUser(admin, {
        role: "editor",
        current_password: srv.adminCreds.password,
      });

      try {
        const c1 = createClient(srv.baseUrl);
        const readerToStaff = await c1.login(readerUser.username, readerPwd);
        assertStatus(readerToStaff, 401, "Reader rejected on /api/admin/auth/login");

        const c2 = createClient(srv.baseUrl);
        const adminToReader = await c2.post("/api/v1/reader/login", {
          ident: srv.adminCreds.username,
          password: srv.adminCreds.password,
        });
        assertStatus(adminToReader, 401, "Admin rejected on /api/v1/reader/login");

        const editorToReader = await c2.post("/api/v1/reader/login", {
          ident: editorUser.username,
          password: editorPwd,
        });
        assertStatus(editorToReader, 401, "Editor rejected on /api/v1/reader/login");
      } finally {
        await deleteUserSafe(admin, readerUser.id);
        await deleteUserSafe(admin, editorUser.id);
      }
    });

    it("F3: missing X-Volo-Console omits csrf_token and missing/bad X-CSRF-Token returns 403 across POST/PUT/PATCH/DELETE", async () => {
      const client = createClient(srv.baseUrl);
      assertStatus(await client.login(srv.adminCreds.username, srv.adminCreds.password), 200);

      try {
        const meNoHdr = await client.me(false);
        assertStatus(meNoHdr, 200);
        assert.equal(meNoHdr.data.csrf_token, "", "csrf_token must be empty without X-Volo-Console: 1");

        const meWithHdr = await client.me(true);
        assertStatus(meWithHdr, 200);
        assert.ok(meWithHdr.data.csrf_token.length > 0);

        const postNoCsrf = await client.post(
          "/api/admin/articles",
          { title: "No CSRF", content_md: "body" },
          { omitCsrf: true },
        );
        assertStatus(postNoCsrf, 403, "POST missing CSRF");

        const patchBadCsrf = await client.patch(
          "/api/admin/settings/site",
          { subtitle: "bad" },
          { headers: { "X-CSRF-Token": "wrong-token" } },
        );
        assertStatus(patchBadCsrf, 403, "PATCH bad CSRF");

        const putNoCsrf = await client.put(
          "/api/admin/themes/volo-studio/config",
          { show_recent_posts: true },
          { omitCsrf: true },
        );
        assertStatus(putNoCsrf, 403, "PUT missing CSRF");

        const delNoCsrf = await client.del("/api/admin/plugins/sensitive-words/logs", {
          omitCsrf: true,
        });
        assertStatus(delNoCsrf, 403, "DELETE missing CSRF");
      } finally {
        await client.logout();
      }
    });

    it("F3: malformed JSON returns 400 across hardened admin endpoints", async () => {
      const art = await createTempArticle(admin, { slug: "t2-malformed-json-art" });
      const guest = createClient(srv.baseUrl);
      const cmt = await submitGuestComment(guest, art.id, { content_md: "Malformed JSON test target" });
      assertStatus(cmt, 201);
      const cid = cmt.data.comment.id;

      try {
        const broken = '{"broken": ';
        const jsonHeaders = { headers: { "Content-Type": "application/json" } };
        const endpoints = [
          ["POST", `/api/admin/articles/${art.id}/share-token`],
          ["POST", "/api/admin/seo/github-sync"],
          ["POST", "/api/admin/settings/storage/test"],
          ["POST", "/api/admin/plugins/sensitive-words/cron"],
          ["POST", `/api/admin/comments/${cid}/reply`],
          ["POST", `/api/admin/comments/${cid}/ban-ip`],
          ["POST", "/api/admin/articles"],
          ["PATCH", "/api/admin/settings/site"],
          ["POST", "/api/admin/settings/notifications/test-smtp"],
          ["POST", "/api/admin/webhooks"],
        ];

        for (const [method, url] of endpoints) {
          const res = await admin.request(method, url, broken, jsonHeaders);
          assertStatus(res, 400, `Malformed JSON on ${method} ${url}`);
          assert.equal(res.data?.error, "invalid json", `Expected invalid json error on ${url}`);
        }
      } finally {
        await admin.del(`/api/admin/comments/${cid}?purge=1`).catch(() => {});
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F3: PAT scopes enforce strict read/write isolation across articles, pages, and media", async () => {
      const artRead = await admin.createPAT("t2-read", ["articles:read"], srv.adminCreds.password);
      const artWrite = await admin.createPAT("t2-awrite", ["articles:write"], srv.adminCreds.password);
      const pageWrite = await admin.createPAT("t2-pwrite", ["pages:write"], srv.adminCreds.password);
      const mediaWrite = await admin.createPAT("t2-mwrite", ["media:write"], srv.adminCreds.password);
      assertStatus(artRead.res, 201);
      assertStatus(artWrite.res, 201);
      assertStatus(pageWrite.res, 201);
      assertStatus(mediaWrite.res, 201);

      let createdArtId = "";
      let createdPageId = "";
      let createdMediaId = "";
      try {
        const cRead = admin.withBearer(artRead.plaintextToken);
        assertStatus(await cRead.get("/api/admin/articles"), 200);
        assertStatus(await cRead.get("/api/admin/pages"), 200);
        assertStatus(await cRead.post("/api/admin/articles", { title: "X", content_md: "Y" }), 403);

        const cWrite = admin.withBearer(artWrite.plaintextToken);
        const aRes = await cWrite.post("/api/admin/articles", {
          title: "PAT Scoped Post",
          slug: "t2-pat-scoped-post",
          content_md: "Body",
          status: "draft",
        });
        assertStatus(aRes, 201);
        createdArtId = aRes.data.article.id;
        assertStatus(await cWrite.get("/api/admin/categories"), 200);
        assertStatus(await cWrite.post("/api/admin/pages", { title: "Denied", slug: "t2-denied-p", content_md: "x" }), 403);

        const cPage = admin.withBearer(pageWrite.plaintextToken);
        const pRes = await cPage.post("/api/admin/pages", {
          title: "PAT Page",
          slug: "t2-pat-page",
          content_md: "Page body",
          is_published: false,
        });
        assertStatus(pRes, 201);
        createdPageId = pRes.data.page.id;
        assertStatus(await cPage.post("/api/admin/articles", { title: "Denied", content_md: "x" }), 403);

        const cMedia = admin.withBearer(mediaWrite.plaintextToken);
        const form = new FormData();
        form.append("file", new Blob([MINIMAL_PNG_BUFFER], { type: "image/png" }), "pat.png");
        const mRes = await cMedia.post("/api/admin/media/upload", form);
        assertStatus(mRes, 201);
        createdMediaId = mRes.data.asset.id;
        assertStatus(await cMedia.post("/api/admin/pages", { title: "Denied", slug: "t2-denied-p2", content_md: "x" }), 403);
      } finally {
        await deleteArticleSafe(admin, createdArtId);
        if (createdPageId) await admin.del(`/api/admin/pages/${createdPageId}`).catch(() => {});
        if (createdMediaId) await admin.del(`/api/admin/media/${createdMediaId}`).catch(() => {});
        for (const t of [artRead, artWrite, pageWrite, mediaWrite]) {
          if (t.token?.id) await admin.del(`/api/admin/security/tokens/${t.token.id}`).catch(() => {});
        }
      }
    });

    it("F3: backup:read PAT is admin-only and all PATs receive 403 on interactive-only routes", async () => {
      const { user: editorUser, password: editorPwd } = await createTempUser(admin, {
        role: "editor",
        current_password: srv.adminCreds.password,
      });
      const adminBackupPat = await admin.createPAT("t2-bk-pat", ["backup:read", "articles:read"], srv.adminCreds.password);
      assertStatus(adminBackupPat.res, 201);

      try {
        const editorClient = createClient(srv.baseUrl);
        assertStatus(await editorClient.login(editorUser.username, editorPwd), 200);
        const editorBackup = await editorClient.createPAT("ed-bk", ["backup:read"], editorPwd);
        assertStatus(editorBackup.res, 403, "Editor cannot mint backup:read PAT");

        const pat = admin.withBearer(adminBackupPat.plaintextToken);
        const expRes = await pat.get("/api/admin/backup/export");
        assertStatus(expRes, 200, "Admin backup:read PAT can GET /api/admin/backup/export");

        const forbiddenInteractive = [
          ["GET", "/api/admin/settings"],
          ["PATCH", "/api/admin/settings/site", { title: "No" }],
          ["GET", "/api/admin/plugins"],
          ["POST", "/api/admin/plugins/sensitive-words/enable"],
          ["GET", "/api/admin/themes"],
          ["GET", "/api/admin/users"],
          ["GET", "/api/admin/comments"],
          ["GET", "/api/admin/security/tokens"],
          ["GET", "/api/admin/nav"],
          ["GET", "/api/admin/links"],
          ["GET", "/api/admin/follows"],
          ["GET", "/api/admin/redirects"],
          ["GET", "/api/admin/webhooks"],
          ["POST", "/api/admin/auth/logout"],
          ["PATCH", "/api/admin/me", { display_name: "Blocked" }],
        ];
        for (const [method, url, body] of forbiddenInteractive) {
          const res = await pat.request(method, url, body);
          assertStatus(res, 403, `PAT must get 403 on ${method} ${url}`);
        }
      } finally {
        if (adminBackupPat.token?.id) {
          await admin.del(`/api/admin/security/tokens/${adminBackupPat.token.id}`).catch(() => {});
        }
        await deleteUserSafe(admin, editorUser.id);
      }
    });

    it("F3: 2FA rejects invalid TOTP codes, bogus challenge tokens, and exhausted recovery codes", async () => {
      const { user, password } = await createTempUser(admin, {
        role: "editor",
        current_password: srv.adminCreds.password,
      });
      try {
        const ed = createClient(srv.baseUrl);
        assertStatus(await ed.login(user.username, password), 200);

        const setup = await ed.post("/api/admin/security/totp/setup", {});
        assertStatus(setup, 200);
        const secret = setup.data.secret;
        const enroll = await ed.post("/api/admin/security/totp/enroll", {
          secret,
          code: generateTOTP(secret, -1),
          current_password: password,
        });
        assertStatus(enroll, 200);
        const recCode = enroll.data.recovery_codes[0];
        await ed.logout();

        const bogusChallenge = await ed.verify2FA("bogus-challenge-token", generateTOTP(secret, 0));
        assertStatus(bogusChallenge, 401, "Bogus 2FA challenge_token rejected");

        const ch1 = await ed.login(user.username, password);
        assertStatus(ch1, 200);
        assert.equal(ch1.data.requires_2fa, true);

        const wrongCode = await ed.verify2FA(ch1.data.challenge_token, "000000");
        assertStatus(wrongCode, 401, "Wrong TOTP code rejected");

        const recOk = await ed.verify2FA(ch1.data.challenge_token, recCode);
        assertStatus(recOk, 200, "First use of recovery code succeeds");
        await ed.logout();

        const ch2 = await ed.login(user.username, password);
        assertStatus(ch2, 200);
        const recReuse = await ed.verify2FA(ch2.data.challenge_token, recCode);
        assertStatus(recReuse, 401, "Exhausted recovery code cannot be reused");
      } finally {
        await deleteUserSafe(admin, user.id);
      }
    });
  });

  describe("F7, F9, F11, F15 & F16: XSS Sanitization Matrix", () => {
    it("F15: article Markdown & preview strip script, event handlers, svg/math, and entity/control-obfuscated URIs", async () => {
      const xssMd = [
        "Safe intro.",
        "",
        '<script>alert("xss")</script>',
        '<img src="x" onerror="alert(1)">',
        '<svg onload="alert(2)"><circle r="10"/></svg>',
        '<math><mi>x</mi></math>',
        '<form action="https://evil.example"><input type="text"></form>',
        '<a href="javascript:alert(3)">js</a>',
        '<a href="vbscript:msgbox(1)">vbs</a>',
        '<a href="data:text/html,<script>alert(4)</script>">data</a>',
        '<a href="j&#x61;vascript:alert(5)">entity</a>',
        '<a href="&#x09;java&#x0a;script:alert(6)">ctrl</a>',
        "<!-- secret html comment -->",
      ].join("\n");

      const prev = await admin.post("/api/admin/render/preview", { content_md: xssMd });
      assertStatus(prev, 200, "Preview XSS payload");
      assertNoXSS(prev.data.html, "Preview XSS");
      assert.ok(!/<svg\b|<math\b|<form\b/i.test(prev.data.html), "Dangerous subtrees must be stripped");
      assert.ok(!prev.data.html.includes("secret html comment"), "HTML comments must be stripped");

      const art = await createTempArticle(admin, {
        slug: "t2-xss-article-matrix",
        content_md: xssMd,
      });
      try {
        const pub = await createClient(srv.baseUrl).get(`/posts/${art.slug}`);
        assertStatus(pub, 200);
        const start = pub.text.indexOf('class="volo-prose"');
        assert.ok(start >= 0);
        const end = pub.text.indexOf("</article>", start);
        const prose = pub.text.slice(start, end > start ? end : undefined);
        assertNoXSS(prose, "Published article prose XSS");
      } finally {
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F15: iframe allowlist drops evil embeds and enforces sandbox on allowlisted YouTube/Bilibili embeds", async () => {
      const md = [
        '<iframe src="https://evil.example/phish"></iframe>',
        '<iframe src="https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"></iframe>',
        '<iframe src="https://player.bilibili.com/player.html?bvid=BV1xx411c7mD"></iframe>',
      ].join("\n\n");

      const prev = await admin.post("/api/admin/render/preview", { content_md: md });
      assertStatus(prev, 200);
      assert.ok(!prev.data.html.includes("evil.example"), "Disallowed iframe must be dropped");
      assert.ok(prev.data.html.includes("youtube-nocookie.com/embed/dQw4w9WgXcQ"));
      assert.ok(prev.data.html.includes("player.bilibili.com/player.html?bvid=BV1xx411c7mD"));
      assert.ok(
        prev.data.html.includes('sandbox="allow-scripts allow-same-origin allow-presentation"'),
        "Allowlisted iframes must force strict sandbox attribute",
      );
    });

    it("F11: public guest comment sanitization strips scripts, iframes, style/id/data-*, and obfuscated schemes", async () => {
      const art = await createTempArticle(admin, { slug: "t2-comment-xss-target" });
      try {
        const guest = createClient(srv.baseUrl);
        const cmtRes = await submitGuestComment(guest, art.id, {
          author_name: "XSSTester",
          content_md: [
            "# HeadingOne",
            "",
            '<script>alert(1)</script>',
            "",
            '<img src="x" onerror="alert(2)">',
            "",
            '<iframe src="https://www.youtube.com/embed/dQw4w9WgXcQ"></iframe>',
            "",
            '<p id="evil-id" style="position:fixed" data-x="1">CleanText</p>',
            "",
            "[Evil](j&#x61;vascript:alert(3))",
            "",
            "[Ext](https://example.org)",
          ].join("\n"),
        });
        assertStatus(cmtRes, 201, "Guest comment with XSS payloads");
        const rendered = cmtRes.data.comment.content_html || "";
        assertNoXSS(rendered, "Comment rendered HTML");
        assert.ok(!rendered.toLowerCase().includes("<iframe"), "Comments must drop all iframes");
        assert.ok(!rendered.includes("evil-id"), "Comments must strip id attribute");
        assert.ok(!rendered.includes("position:fixed"), "Comments must strip style attribute");
        assert.ok(!rendered.includes("data-x"), "Comments must strip data-* attributes");
        assert.ok(rendered.includes("<h4>"), "Comments must demote h1 to h4");
        assert.ok(rendered.includes("nofollow"), "External comment links must carry rel=nofollow");
      } finally {
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F7: site.custom_head_meta strips svg, math, base, meta refresh, and xlink:href while preserving safe meta", async () => {
      const payload = [
        '<meta name="t2-head-ok" content="verified">',
        '<svg onload="alert(1)"><use xlink:href="javascript:alert(2)"></use></svg>',
        '<math><mi>x</mi></math>',
        '<base href="https://evil.example/">',
        '<meta http-equiv="refresh" content="0;url=https://evil.example/">',
      ].join("");

      try {
        const patch = await admin.patch("/api/admin/settings/site", {
          custom_head_meta: payload,
        });
        assertStatus(patch, 200);
        const stored = patch.data.value?.custom_head_meta || "";
        assert.ok(stored.includes('<meta name="t2-head-ok" content="verified">'));
        assert.ok(!/<svg\b/i.test(stored), "<svg> must be stripped from custom_head_meta");
        assert.ok(!/<math\b/i.test(stored), "<math> must be stripped from custom_head_meta");
        assert.ok(!/<base\b/i.test(stored), "<base> must be stripped from custom_head_meta");
        assert.ok(!/http-equiv\s*=\s*["']?refresh/i.test(stored), "meta refresh must be stripped");
        assert.ok(!/xlink:href/i.test(stored), "xlink:href must be stripped");

        const home = await createClient(srv.baseUrl).get("/");
        assertStatus(home, 200);
        const headMatch = home.text.match(/<head[^>]*>([\s\S]*?)<\/head>/i);
        assert.ok(headMatch);
        assert.ok(headMatch[1].includes('<meta name="t2-head-ok" content="verified">'));
        assert.ok(!headMatch[1].includes("evil.example"));
      } finally {
        await admin.patch("/api/admin/settings/site", { custom_head_meta: "" });
      }
    });

    it("F7: appearance.custom_css strips javascript:/expression( and escapes </style> breakout", async () => {
      try {
        const patch = await admin.patch("/api/admin/settings/appearance", {
          custom_css:
            "body { --t2-safe: 1; } /* </style><script>alert(9)</script> */ a { background: url(JaVaScRiPt:alert(1)); width: eXpReSsIoN(alert(2)); }",
        });
        assertStatus(patch, 200);
        const css = patch.data.value?.custom_css || "";
        assert.ok(css.includes("--t2-safe: 1"));
        assert.equal(/javascript\s*:/i.test(css), false);
        assert.equal(/expression\s*\(/i.test(css), false);
        assert.equal(css.includes("</style>"), false);
        assert.ok(css.includes("<\\/style>"));
      } finally {
        await admin.patch("/api/admin/settings/appearance", { custom_css: "" });
      }
    });

    it("F9: article and link creation reject javascript:/vbscript:/data:text/html URLs", async () => {
      for (const badUrl of ["javascript:alert(1)", "vbscript:msgbox(1)", "data:text/html,<script>1</script>"]) {
        const artBad = await admin.post("/api/admin/articles", {
          title: "Bad Cover",
          slug: `t2-bad-cover-${Math.random().toString(36).slice(2, 7)}`,
          content_md: "body",
          cover_url: badUrl,
        });
        assertStatus(artBad, 400, `Article cover_url rejected for ${badUrl}`);

        const linkBad = await admin.post("/api/admin/links", {
          title: "Bad Link",
          url: badUrl,
        });
        assertStatus(linkBad, 400, `Friend link url rejected for ${badUrl}`);
      }
    });

    it("F16: headless Edge CM6 editor sanitizes vbscript:/data:text/html/onerror= on WYSIWYG paste", async () => {
      await launchEdge().then(async (browser) => {
        try {
          await withPage(
            browser,
            {
              width: 1280,
              height: 800,
              ignoreConsolePatterns: [/Executing inline event handler violates the following Content Security Policy directive/],
            },
            async (page, diag) => {
              await waitForEditorReady(page, () =>
                loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor"),
              );

              const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
              await modeTabs.nth(1).click();
              const wysiwyg = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
              await wysiwyg.waitFor({ state: "visible", timeout: 10000 });
              await wysiwyg.click();

              await page.evaluate(() => {
                const el = document.querySelector('.volo-wysiwyg-surface[contenteditable="true"]');
                const dt = new DataTransfer();
                dt.setData(
                  "text/html",
                  '<p>PasteGuard <a href="vbscript:msgbox(1)">Vbs</a> <a href="data:text/html,<script>alert(1)</script>">DataLink</a> <img src="https://example.com/a.png" onerror="alert(2)"></p>',
                );
                dt.setData("text/plain", "PasteGuard Vbs DataLink");
                el.dispatchEvent(new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: dt }));
              });

              await page.locator(".volo-wysiwyg-surface", { hasText: "PasteGuard" }).waitFor({
                state: "visible",
                timeout: 5000,
              });

              await modeTabs.nth(0).click();
              await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
              const cmText = (await page.locator(".volo-cm-host .cm-editor .cm-content").textContent()) || "";
              assert.ok(cmText.includes("PasteGuard"));
              assert.equal(/vbscript\s*:/i.test(cmText), false, `vbscript: leaked: ${cmText}`);
              assert.equal(/data\s*:\s*text\/html/i.test(cmText), false, `data:text/html leaked: ${cmText}`);
              assert.equal(/\bonerror\s*=/i.test(cmText), false, `onerror= leaked: ${cmText}`);
              diag.assertClean();
            },
          );
        } finally {
          await browser.close();
        }
      });
    });
  });

  describe("F8, F11, F12, F13 & F16: SSRF, Input & Plugin Security Boundaries", () => {
    it("F12: POST /api/admin/links/{id}/check returns 200 health_status='blocked' on loopback, metadata, and RFC1918", async () => {
      const targets = [
        "http://127.0.0.1:8080/healthz",
        "http://169.254.169.254/latest/meta-data/",
        "http://10.0.0.5/internal",
      ];
      for (const url of targets) {
        const lRes = await admin.post("/api/admin/links", {
          title: `SSRF Link ${url}`,
          url,
          category_name: "SSRF",
        });
        assertStatus(lRes, 201, `Create link for check (${url})`);
        const linkId = lRes.data.link.id;
        try {
          const chk = await admin.post(`/api/admin/links/${linkId}/check`);
          assertStatus(chk, 200, `Check link SSRF (${url})`);
          assert.equal(chk.data.ok, true);
          assert.equal(chk.data.link?.health_status, "blocked", `Expected health_status=blocked for ${url}`);
          assert.equal(chk.data.link?.http_code, 0);
        } finally {
          await admin.del(`/api/admin/links/${linkId}`).catch(() => {});
        }
      }
    });

    it("F12: POST /api/admin/follows/{id}/sync rejects private/loopback/metadata feed URLs with 400", async () => {
      for (const feedUrl of [
        "http://127.0.0.1:8080/rss.xml",
        "http://169.254.169.254/latest/meta-data/",
        "http://192.168.1.10/feed.xml",
      ]) {
        const fRes = await admin.post("/api/admin/follows", {
          title: `SSRF Follow ${feedUrl}`,
          feed_url: feedUrl,
        });
        assertStatus(fRes, 201, `Create follow (${feedUrl})`);
        const fid = fRes.data.follow.id;
        try {
          const syncRes = await admin.post(`/api/admin/follows/${fid}/sync`);
          assertSSRFRejected(syncRes, `Follow sync SSRF (${feedUrl})`);
          assert.ok(String(syncRes.data?.follow?.sync_error || syncRes.data?.error || "").length > 0);
        } finally {
          await admin.del(`/api/admin/follows/${fid}`).catch(() => {});
        }
      }
    });

    it("F12: webhooks create & update reject SSRF targets across custom and bark providers", async () => {
      const ssrfTargets = [
        { provider: "custom", url: "http://127.0.0.1:9999/hook" },
        { provider: "bark", url: "http://169.254.169.254/key/msg" },
        { provider: "custom", url: "http://10.0.0.1/hook" },
        { provider: "custom", url: "http://[::1]:8080/hook" },
        { provider: "custom", url: "http://localhost:8080/hook" },
        { provider: "custom", url: "file:///etc/passwd" },
      ];
      for (const t of ssrfTargets) {
        const res = await admin.post("/api/admin/webhooks", {
          name: `SSRF ${t.provider}`,
          provider: t.provider,
          url: t.url,
          events: ["article.published"],
          is_enabled: true,
        });
        assertSSRFRejected(res, `POST webhook SSRF (${t.provider}: ${t.url})`);
      }

      const valid = await admin.post("/api/admin/webhooks", {
        name: "Valid Before Update",
        provider: "bark",
        url: "https://api.day.app/validtoken/hi",
        events: ["article.published"],
        is_enabled: true,
      });
      assertStatus(valid, 201);
      const whId = valid.data.webhook.id;
      try {
        const updSsrf = await admin.put(`/api/admin/webhooks/${whId}`, {
          name: "Mutated SSRF",
          provider: "bark",
          url: "http://127.0.0.1:8080/ssrf",
          events: ["article.published"],
          is_enabled: true,
        });
        assertSSRFRejected(updSsrf, "PUT webhook SSRF");
      } finally {
        await admin.del(`/api/admin/webhooks/${whId}`).catch(() => {});
      }
    });

    it("F12: POST /api/admin/webhooks/{id}/test rejects non-routable/blocked delivery with 400", async () => {
      const wh = await admin.post("/api/admin/webhooks", {
        name: "Test Delivery Target",
        provider: "custom",
        url: "https://ssrf-probe.invalid/hook",
        events: ["article.published"],
        is_enabled: true,
      });
      assertStatus(wh, 201);
      const whId = wh.data.webhook.id;
      try {
        const testRes = await admin.post(`/api/admin/webhooks/${whId}/test`);
        assertStatus(testRes, 400, "POST /api/admin/webhooks/{id}/test on unreachable/invalid host");
        assert.equal(testRes.data?.ok, false);
        assert.ok(String(testRes.data?.error || "").length > 0);
      } finally {
        await admin.del(`/api/admin/webhooks/${whId}`).catch(() => {});
      }
    });

    it("F8: POST /api/admin/settings/storage/test rejects S3 SSRF targets", async () => {
      for (const endpoint of [
        "http://127.0.0.1:9000",
        "http://169.254.169.254",
        "http://10.0.0.1:9000",
        "http://[::1]:9000",
        "http://localhost:9000",
      ]) {
        const res = await admin.post("/api/admin/settings/storage/test", {
          settings: {
            active_driver: "s3",
            s3_endpoint: endpoint,
            s3_region: "us-east-1",
            s3_bucket: "t2-bucket",
            s3_access_key: "AKIA",
          },
          s3_secret_key: "secret",
        });
        assertSSRFRejected(res, `S3 storage/test SSRF (${endpoint})`);
      }
    });

    it("F8: POST /api/admin/settings/notifications/test-smtp rejects loopback, metadata, and RFC1918 under TLS", async () => {
      const targets = [
        { smtp_host: "127.0.0.1", smtp_port: 587, smtp_security: "starttls" },
        { smtp_host: "169.254.169.254", smtp_port: 25, smtp_security: "plain_internal" },
        { smtp_host: "10.0.0.5", smtp_port: 465, smtp_security: "tls" },
        { smtp_host: "localhost", smtp_port: 587, smtp_security: "starttls" },
      ];
      for (const s of targets) {
        const res = await admin.post("/api/admin/settings/notifications/test-smtp", {
          to_email: "admin@e2e.local",
          settings: { smtp_enabled: true, ...s },
        });
        assertSSRFRejected(res, `SMTP SSRF (${s.smtp_host})`);
      }
    });

    it("F8: backup remote S3 SSRF succeeds locally with remote_uploaded=false and remote_warning", async () => {
      try {
        const patch = await admin.patch("/api/admin/settings/backup", {
          remote_enabled: true,
          remote_protocol: "s3",
          s3_endpoint: "http://169.254.169.254",
          s3_region: "us-east-1",
          s3_bucket: "ssrf-backup",
          s3_access_key: "AKIA",
          s3_secret_key: "super-secret-backup-key",
        });
        assertStatus(patch, 200);

        const runBk = await admin.post("/api/admin/backup/local", {});
        assertStatus(runBk, 200, "Local backup with SSRF remote S3");
        assert.equal(runBk.data.ok, true);
        assert.equal(runBk.data.remote_uploaded, false);
        assert.ok(String(runBk.data.remote_warning || "").length > 0, "Expected remote_warning on SSRF remote S3");
        assert.ok(!String(runBk.data.remote_warning).includes("super-secret-backup-key"));

        if (runBk.data.file) {
          await admin.del(`/api/admin/backup/local/${encodeURIComponent(runBk.data.file)}`).catch(() => {});
        }
      } finally {
        await admin.patch("/api/admin/settings/backup", {
          remote_enabled: false,
          remote_protocol: "none",
          clear_secrets: ["s3_secret_key"],
        });
      }
    });

    it("F11: public comments reject invalid PoW solution, non-existent target_id, and filled honeypot", async () => {
      const art = await createTempArticle(admin, { slug: "t2-pow-comment-target" });
      try {
        const guest = createClient(srv.baseUrl);
        const ch = await guest.get("/api/v1/pow/challenge");
        assertStatus(ch, 200);

        const badPow = await guest.post("/api/v1/comments", {
          target_type: "article",
          target_id: art.id,
          author_name: "BadPow",
          content_md: "Should fail",
          pow_challenge: ch.data.challenge,
          pow_solution: "definitely-wrong-nonce",
          website_hp: "",
        });
        assertStatus(badPow, 400, "Wrong PoW solution");

        const sol = solvePoW(ch.data.challenge, ch.data.difficulty_bits);
        const badTarget = await guest.post("/api/v1/comments", {
          target_type: "article",
          target_id: "nonexistent-article-id-xyz",
          author_name: "BadTarget",
          content_md: "Should fail",
          pow_challenge: ch.data.challenge,
          pow_solution: sol,
          website_hp: "",
        });
        assertStatus(badTarget, [400, 404], "Non-existent target_id");

        const ch2 = await guest.get("/api/v1/pow/challenge");
        const sol2 = solvePoW(ch2.data.challenge, ch2.data.difficulty_bits);
        const hp = await guest.post("/api/v1/comments", {
          target_type: "article",
          target_id: art.id,
          author_name: "Bot",
          content_md: "Spam",
          pow_challenge: ch2.data.challenge,
          pow_solution: sol2,
          website_hp: "https://spam.example",
        });
        assertStatus(hp, 400, "Honeypot filled");
      } finally {
        await deleteArticleSafe(admin, art.id);
      }
    });

    it("F13: plugin install rejects malformed ZIP, missing manifest.json, and path traversal entries", async () => {
      const postZip = async (buf, name) => {
        const form = new FormData();
        form.append("file", new Blob([buf], { type: "application/zip" }), name);
        return admin.post("/api/admin/plugins/install", form);
      };

      const badZip = await postZip(Buffer.from("not-a-zip-archive"), "bad.zip");
      assertStatus(badZip, 400, "Non-ZIP bytes rejected");

      const noManifestZip = buildStoreZip([{ name: "main.js", data: "module.exports = {};" }]);
      assertStatus(await postZip(noManifestZip, "nomanifest.zip"), 400, "Missing manifest.json rejected");

      const traversalZip = buildStoreZip([
        {
          name: "manifest.json",
          data: JSON.stringify({
            id: "t2-trav-plugin",
            version: "1.0.0",
            min_host_version: "0.1.0",
            runtime: "goja",
            entry: "main.js",
            name: { "en-US": "Traversal" },
          }),
        },
        { name: "../escape.js", data: "evil" },
        { name: "main.js", data: "module.exports = {};" },
      ]);
      assertStatus(await postZip(traversalZip, "traversal.zip"), 400, "Path traversal ZIP entry rejected");
    });

    it("F16: headless Edge LoginView displays error alert on bad credentials without uncaught page errors", async () => {
      await launchEdge().then(async (browser) => {
        try {
          await withPage(
            browser,
            {
              width: 1280,
              height: 800,
              ignoreConsolePatterns: [/401/, /Unauthorized/, /Failed to load resource/],
            },
            async (page, diag) => {
              await page.goto(`${srv.baseUrl}/console`, { waitUntil: "domcontentloaded" });
              await page.locator(".volo-auth-version").waitFor({ state: "visible", timeout: 15000 });
              await page.locator('input[name="volo_staff_ident"]').fill(srv.adminCreds.username);
              await page.locator('input[name="volo_staff_secret"]').fill("WrongPassword!999");
              diag.clear();
              await page.locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit').click();

              const errEl = page.locator('[role="alert"], .volo-alert-err, .volo-auth-error');
              await errEl.first().waitFor({ state: "visible", timeout: 10000 });
              assert.equal(diag.pageErrors.length, 0);
              diag.assertClean();
            },
          );
        } finally {
          await browser.close();
        }
      });
    });
  });
});
