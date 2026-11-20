import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertJsonHasKeys,
  assertRedirect,
  assertSSRFRejected,
  assertStatus,
} from "../lib/assertions.mjs";
import { checkNoHorizontalOverflow, launchEdge, withPage } from "../lib/browser.mjs";
import { createClient, generateTOTP } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";
import {
  MINIMAL_PNG_BUFFER,
  buildStoreZip,
  createTempArticle,
  createTempUser,
  deleteArticleSafe,
  deleteUserSafe,
  extractBootConfig,
  loginConsoleViaUI,
  resolveCliVersion,
  submitGuestComment,
  waitForEditorReady,
  withTempSettings,
} from "../tier2/_helpers.mjs";

describe("Tier 3 — Cross-Feature Pairwise & Combinatorial Scenarios", () => {
  let srv;
  let admin;
  let anon;

  before(async () => {
    srv = await startVoloServer({
      setupPayload: {
        site_title: "Tier3 Cross-Feature Blog",
        default_locale: "en-US",
      },
    });
    admin = createClient(srv.baseUrl);
    anon = createClient(srv.baseUrl);
    assertStatus(await admin.login(srv.adminCreds.username, srv.adminCreds.password), 200);
    assertStatus(
      await admin.patch("/api/admin/settings/feed", {
        output_mode: "full",
        item_count: 20,
        enable_raw_markdown: true,
        enable_llms_txt: true,
      }),
      200,
    );
  });

  after(async () => {
    if (srv) {
      await srv.stop();
    }
  });

  it("S01 (F1+F2+F16): SetupWizard -> BootConfig.version parity with CLI & about/project-info + accent SSR", async () => {
    const cli = await resolveCliVersion();
    const fresh = await startVoloServer({ skipSetup: true });
    try {
      const setupPage = await createClient(fresh.baseUrl).get("/setup");
      assertStatus(setupPage, 200);
      const setupBoot = extractBootConfig(setupPage.text);
      assert.equal(setupBoot.mode, "setup");
      assert.equal(setupBoot.version, cli.version);

      const c = createClient(fresh.baseUrl);
      const comp = await c.setupComplete({
        admin_username: "t3admin",
        admin_email: "t3@e2e.local",
        admin_password: "Tier3Password!123",
        site_title: "T3 Fresh Journal",
        default_locale: "en-US",
        accent: "teal",
        public_url: fresh.baseUrl,
      });
      assertStatus(comp, 200);

      const home = await createClient(fresh.baseUrl).get("/");
      assertStatus(home, 200);
      assert.ok(home.text.includes("T3 Fresh Journal"));
      assert.ok(home.text.includes('data-accent="teal"'));

      const info = await c.get("/api/admin/about/project-info?offline=1");
      assertStatus(info, 200);
      assert.equal(info.data.version, cli.version);
    } finally {
      await fresh.stop();
    }
  });

  it("S02 (F4+F6): changing reading.posts_per_page immediately updates / pagination links and page counts", async () => {
    const isoSrv = await startVoloServer({ setupPayload: { default_locale: "en-US" } });
    try {
      const isoAdmin = createClient(isoSrv.baseUrl);
      assertStatus(await isoAdmin.login(isoSrv.adminCreds.username, isoSrv.adminCreds.password), 200);
      const isoAnon = createClient(isoSrv.baseUrl);

      for (let i = 1; i <= 5; i += 1) {
        await createTempArticle(isoAdmin, {
          title: `S02 Paged Post ${i}`,
          slug: `t3-s02-post-${i}`,
          content_md: `S02 body ${i}`,
        });
      }

      assertStatus(await isoAdmin.patch("/api/admin/settings/reading", { posts_per_page: 2 }), 200);
      const p1 = await isoAnon.get("/?page=1");
      assertStatus(p1, 200);
      assert.equal((p1.text.match(/<article\s+class="article__item\b/g) || []).length, 2);
      assert.ok(p1.text.includes("?page=2") && p1.text.includes("?page=3"));

      const p3 = await isoAnon.get("/?page=3");
      assertStatus(p3, 200);
      assert.equal((p3.text.match(/<article\s+class="article__item\b/g) || []).length, 1);

      assertStatus(await isoAdmin.patch("/api/admin/settings/reading", { posts_per_page: 4 }), 200);
      const p1Updated = await isoAnon.get("/?page=1");
      assertStatus(p1Updated, 200);
      assert.equal((p1Updated.text.match(/<article\s+class="article__item\b/g) || []).length, 4);
      assert.equal(p1Updated.text.includes("?page=3"), false, "5 posts at 4/page must only have 2 pages");
    } finally {
      await isoSrv.stop();
    }
  });

  it("S03 (F7+F14): appearance (volo-journal + rose + dark) + kanbanniang (right) reflect together on public HTML", async () => {
    try {
      assertStatus(
        await admin.patch("/api/admin/settings/appearance", {
          active_theme: "volo-journal",
          accent: "rose",
          default_mode: "dark",
        }),
        200,
      );
      assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
      assertStatus(
        await admin.patch("/api/admin/plugins/kanbanniang/config", {
          position: "right",
          hide_on_mobile: true,
        }),
        200,
      );

      const home = await anon.get("/");
      assertStatus(home, 200);
      assert.ok(home.text.includes('data-volo-skin="volo-journal"'));
      assert.ok(home.text.includes('data-accent="rose"'));
      assert.ok(home.text.includes('data-default-mode="dark"'));
      assert.ok(home.text.includes('id="volo-kanbanniang-root"'));
      assert.ok(home.text.includes("volo-kbn-right"));
      assert.ok(home.text.includes("volo-kbn-hide-mobile"));
    } finally {
      await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
      await admin.patch("/api/admin/settings/appearance", {
        active_theme: "volo-studio",
        accent: "forest",
        default_mode: "light",
      }).catch(() => {});
    }
  });

  it("S04 (F4+F5+F9+F15): rich Markdown + :::lock + :::bookshelf across HTML/search/feeds/llms/.md + protected post isolation", async () => {
    const bookRes = await admin.post("/api/admin/books", {
      title: "S04 Shelf Book",
      slug: "t3-s04-book",
      source_type: "manual",
    });
    assertStatus(bookRes, 201);
    const bookId = bookRes.data.book.id;

    const pubArt = await createTempArticle(admin, {
      title: "S04 Rich Public Post",
      slug: "t3-s04-rich-public",
      content_md: [
        "Rich public intro S04_PUBLIC_SEARCH_TOKEN.",
        "",
        "Budget $100 and formula $\\alpha + \\beta$.",
        "",
        "```go",
        "func s04() int { return 42 }",
        "```",
        "",
        "```mermaid",
        "graph LR",
        "  A --> B",
        "```",
        "",
        ":::bookshelf",
        ":::",
        "",
        ':::lock pwd="s04-lock-pass"',
        "S04_LOCKED_BLOCK_SECRET_XYZ",
        ":::",
      ].join("\n"),
    });

    const pwdArt = await createTempArticle(admin, {
      title: "S04 Protected Whole Post",
      slug: "t3-s04-protected-post",
      password: "whole-post-secret",
      content_md: "S04_WHOLE_POST_SECRET_XYZ",
    });

    try {
      const html = await anon.get(`/posts/${pubArt.slug}`);
      assertStatus(html, 200);
      assert.ok(html.text.includes('data-needs-math="1"'));
      assert.ok(html.text.includes('data-needs-mermaid="1"'));
      assert.ok(html.text.includes('class="volo-codeblock"'));
      assert.ok(html.text.includes("volo-embed-bookshelf"));
      assert.ok(html.text.includes("S04 Shelf Book"));
      assert.ok(html.text.includes('class="volo-lock-card"'));
      assert.equal(html.text.includes("S04_LOCKED_BLOCK_SECRET_XYZ"), false);

      const searchPub = await anon.get("/api/v1/search?q=S04_PUBLIC_SEARCH_TOKEN");
      assertStatus(searchPub, 200);
      assert.ok(searchPub.data.hits.some((h) => String(h.url || h.slug).includes(pubArt.slug)));

      const searchLock = await anon.get("/api/v1/search?q=S04_LOCKED_BLOCK_SECRET_XYZ");
      assertStatus(searchLock, 200);
      assert.equal(searchLock.data.hits.length, 0);

      const searchPwd = await anon.get("/api/v1/search?q=S04_WHOLE_POST_SECRET_XYZ");
      assertStatus(searchPwd, 200);
      assert.equal(searchPwd.data.hits.length, 0);

      for (const feedPath of ["/rss.xml", "/atom.xml", "/feed.json", "/llms.txt", "/llms-full.txt"]) {
        const fRes = await anon.get(feedPath);
        assertStatus(fRes, 200, `GET ${feedPath}`);
        assert.ok(fRes.text.includes("S04 Rich Public Post"), `${feedPath} must include public post`);
        assert.equal(fRes.text.includes("S04_LOCKED_BLOCK_SECRET_XYZ"), false, `${feedPath} must strip :::lock`);
        assert.equal(fRes.text.includes("S04 Protected Whole Post"), false, `${feedPath} must exclude protected post`);
        assert.equal(fRes.text.includes("S04_WHOLE_POST_SECRET_XYZ"), false);
      }

      const smRes = await anon.get("/sitemap.xml");
      assertStatus(smRes, 200);
      assert.ok(smRes.text.includes(pubArt.slug));
      assert.equal(smRes.text.includes(pwdArt.slug), false);

      const rawPub = await anon.get(`/posts/${pubArt.slug}.md`);
      assertStatus(rawPub, 200);
      assert.ok(rawPub.text.includes("S04_PUBLIC_SEARCH_TOKEN"));
      assert.equal(rawPub.text.includes("S04_LOCKED_BLOCK_SECRET_XYZ"), false);

      assertStatus(await anon.get(`/posts/${pwdArt.slug}.md`), 403, "Protected post .md returns 403");
    } finally {
      await deleteArticleSafe(admin, pubArt.id);
      await deleteArticleSafe(admin, pwdArt.id);
      await admin.del(`/api/admin/books/${bookId}`).catch(() => {});
    }
  });

  it("S05 (F11+F13): sensitive-words reject/moderate + reserved_nicknames filter comments and increment dashboard stats", async () => {
    const target = await createTempArticle(admin, {
      title: "S05 Moderation Target",
      slug: "t3-s05-target",
      allow_comment: true,
    });
    assertStatus(await admin.post("/api/admin/plugins/sensitive-words/enable"), 200);
    assertStatus(await admin.del("/api/admin/plugins/sensitive-words/kv"), 200);

    try {
      assertStatus(
        await admin.patch("/api/admin/plugins/sensitive-words/config", {
          action: "reject",
          check_nickname: true,
          custom_deny: ["t3_banned_phrase"],
          reserved_nicknames: ["official_staff_res"],
        }),
        200,
      );

      const st0 = await admin.get("/plugins/sensitive-words/api/status");
      assertStatus(st0, 200);
      const baseTotal = st0.data.blocked_total || 0;

      const rejContent = await submitGuestComment(anon, target.id, {
        author_name: "NormalUser",
        content_md: "Contains t3_banned_phrase inside",
      });
      assertStatus(rejContent, 403, "reject action returns 403");

      const rejNick = await submitGuestComment(anon, target.id, {
        author_name: "official_staff_res",
        content_md: "Normal clean text",
      });
      assertStatus(rejNick, 403, "reserved_nicknames returns 403");

      assertStatus(
        await admin.patch("/api/admin/plugins/sensitive-words/config", {
          action: "moderate",
          custom_deny: ["t3_moderate_phrase"],
        }),
        200,
      );

      const modRes = await submitGuestComment(anon, target.id, {
        author_name: "ModerationUser",
        content_md: "Contains t3_moderate_phrase inside",
      });
      assertStatus(modRes, 201, "moderate action holds comment as 201 pending");
      assert.equal(modRes.data.comment.status, "pending");

      const st1 = await admin.get("/plugins/sensitive-words/api/status");
      assertStatus(st1, 200);
      assert.ok(st1.data.blocked_total >= baseTotal + 3, "blocked_total must increment across reject + moderate");

      const ov = await admin.get("/api/admin/overview");
      assertStatus(ov, 200);
      assert.ok(JSON.stringify(ov.data).includes("sw-admin-stat"), "Overview must include sw-admin-stat card");
    } finally {
      await admin.post("/api/admin/plugins/sensitive-words/disable").catch(() => {});
      await deleteArticleSafe(admin, target.id);
    }
  });

  it("S06 (F4+F10+F12): pagination + q search combinations across /categories, /tags, and /links", async () => {
    const catIds = [];
    const linkIds = [];
    let artId = "";
    try {
      for (let i = 1; i <= 38; i += 1) {
        const grp = i <= 20 ? "S06GroupOne" : "S06GroupTwo";
        const c = await admin.post("/api/admin/categories", {
          name: `${grp} Cat ${i}`,
          slug: `t3-s06-cat-${i}`,
          sort_order: i,
        });
        assertStatus(c, 201);
        catIds.push(c.data.category.id);
      }

      const tags = [];
      for (let i = 1; i <= 125; i += 1) {
        tags.push(`t3s06-${i <= 5 ? "special" : "bulk"}-${String(i).padStart(3, "0")}`);
      }
      const a = await createTempArticle(admin, {
        slug: "t3-s06-tag-art",
        tags,
      });
      artId = a.id;

      for (let i = 1; i <= 52; i += 1) {
        const catName = i <= 26 ? "S06LinkGrp1" : "S06LinkGrp2";
        const l = await admin.post("/api/admin/links", {
          title: `${catName} Item ${i}`,
          url: `https://example.org/s06/${i}`,
          category_name: catName,
        });
        assertStatus(l, 201);
        linkIds.push(l.data.link.id);
      }

      const catP2 = await anon.get("/categories?page=2");
      assertStatus(catP2, 200);
      assert.ok(catP2.text.includes("S06GroupTwo Cat 38"));

      const catQ = await anon.get("/categories?q=S06GroupOne&page=1");
      assertStatus(catQ, 200);
      assert.ok(catQ.text.includes("S06GroupOne Cat 1"));
      assert.ok(catQ.text.includes("js-jump-pills-toggle"));
      assert.equal(catQ.text.includes("S06GroupTwo Cat 38"), false);

      const tagQ = await anon.get("/tags?q=t3s06-special&sort=name&page=1");
      assertStatus(tagQ, 200);
      assert.ok(tagQ.text.includes("#t3s06-special-001"));
      assert.equal(tagQ.text.includes("#t3s06-bulk-010"), false);

      const linksQ = await anon.get("/links?q=S06LinkGrp2&page=1");
      assertStatus(linksQ, 200);
      assert.ok(linksQ.text.includes("S06LinkGrp2 Item 52"));
      assert.equal(linksQ.text.includes("S06LinkGrp1 Item 1"), false);
    } finally {
      await deleteArticleSafe(admin, artId);
      for (const id of catIds) await admin.del(`/api/admin/categories/${id}`).catch(() => {});
      if (linkIds.length > 0) {
        await admin.post("/api/admin/links/batch", { action: "delete", ids: linkIds }).catch(() => {});
      }
    }
  });

  it("S07 (F4+F5+F9+F10): category/tag/series assignment + unpublish/draft/delete updates listings, feeds & search", async () => {
    const cat = await admin.post("/api/admin/categories", { name: "S07 Cat", slug: "t3-s07-cat" });
    assertStatus(cat, 201);
    const catId = cat.data.category.id;

    const art = await createTempArticle(admin, {
      title: "S07 Lifecycle Article",
      slug: "t3-s07-article",
      category_id: catId,
      tags: ["t3s07tag"],
      content_md: "S07_LIFECYCLE_SEARCH_MARKER",
    });

    const ser = await admin.post("/api/admin/series", {
      title: "S07 Series",
      slug: "t3-s07-series",
      article_ids: [art.id],
    });
    assertStatus(ser, 201);
    const serId = ser.data.series.id;

    try {
      assert.ok((await anon.get("/categories/t3-s07-cat")).text.includes("S07 Lifecycle Article"));
      assert.ok((await anon.get("/tags/t3s07tag")).text.includes("S07 Lifecycle Article"));
      assert.ok((await anon.get("/series/t3-s07-series")).text.includes("S07 Lifecycle Article"));
      assert.ok((await anon.get("/rss.xml")).text.includes("S07 Lifecycle Article"));
      assert.ok((await anon.get("/api/v1/search?q=S07_LIFECYCLE_SEARCH_MARKER")).data.hits.length >= 1);

      const toDraft = await admin.put(`/api/admin/articles/${art.id}`, {
        title: "S07 Lifecycle Article",
        slug: "t3-s07-article",
        category_id: catId,
        tags: ["t3s07tag"],
        content_md: "S07_LIFECYCLE_SEARCH_MARKER",
        status: "draft",
      });
      assertStatus(toDraft, 200);

      assertStatus(await anon.get("/posts/t3-s07-article"), 404);
      assert.equal((await anon.get("/categories/t3-s07-cat")).text.includes("S07 Lifecycle Article"), false);
      assert.equal((await anon.get("/rss.xml")).text.includes("S07 Lifecycle Article"), false);
      assert.equal((await anon.get("/api/v1/search?q=S07_LIFECYCLE_SEARCH_MARKER")).data.hits.length, 0);

      await deleteArticleSafe(admin, art.id);
      assert.equal((await anon.get("/sitemap.xml")).text.includes("t3-s07-article"), false);
    } finally {
      await admin.del(`/api/admin/series/${serId}`).catch(() => {});
      await admin.del(`/api/admin/categories/${catId}`).catch(() => {});
      await deleteArticleSafe(admin, art.id);
    }
  });

  it("S08 (F4+F6+F7+F12+F14): redirect + permalink + footer_html toggle + volo-journal theme_configs E2E", async () => {
    const art = await createTempArticle(admin, {
      title: "S08 Combo Post",
      slug: "t3-s08-combo",
      content_md: "S08 body content.",
    });
    const redir = await admin.post("/api/admin/redirects", {
      source_path: "/s08-legacy-entry",
      target_path: "/p/t3-s08-combo",
      status_code: 302,
    });
    assertStatus(redir, 201);
    const redirId = redir.data.redirect.id;

    try {
      assertStatus(
        await admin.patch("/api/admin/settings/seo", { post_permalink_style: "/p/{slug}" }),
        200,
      );
      assertStatus(
        await admin.patch("/api/admin/settings/site", {
          enable_footer_html: true,
          footer_text: "<strong>S08 Custom Footer</strong>",
        }),
        200,
      );
      assertStatus(
        await admin.post("/api/admin/themes/activate", { theme_id: "volo-journal", target: "both" }),
        200,
      );
      assertStatus(
        await admin.put("/api/admin/themes/volo-journal/config", {
          journal_motto: "S08 Journal Motto",
          hero_badge: "S08 Issue 01",
          show_colophon: true,
        }),
        200,
      );

      assertRedirect(await anon.get("/s08-legacy-entry"), 302, "/p/t3-s08-combo");
      assertRedirect(await anon.get("/posts/t3-s08-combo"), 301, "/p/t3-s08-combo");

      const home = await anon.get("/");
      assertStatus(home, 200);
      assert.ok(home.text.includes('href="/p/t3-s08-combo"'));
      assert.ok(home.text.includes("S08 Custom Footer"));
      assert.ok(home.text.includes("S08 Journal Motto"));
      assert.ok(home.text.includes("S08 Issue 01"));
      assert.ok(home.text.includes("journal-colophon"));

      assertStatus(await admin.patch("/api/admin/settings/site", { enable_footer_html: false }), 200);
      const homeNoFooter = await anon.get("/");
      assert.equal(homeNoFooter.text.includes("S08 Custom Footer"), false);
    } finally {
      await admin.del(`/api/admin/redirects/${redirId}`).catch(() => {});
      await deleteArticleSafe(admin, art.id);
      await admin.patch("/api/admin/settings/seo", { post_permalink_style: "/posts/{slug}" }).catch(() => {});
      await admin.patch("/api/admin/settings/site", { enable_footer_html: false, footer_text: "" }).catch(() => {});
      await admin.post("/api/admin/themes/activate", { theme_id: "volo-studio", target: "both" }).catch(() => {});
    }
  });

  it("S09 (F3+F8): dynamic_login_path hides /console, CSRF and 409 optimistic lock stay enforced, and reset restores /console", async () => {
    try {
      const sec = await admin.patch("/api/admin/settings/security", {
        dynamic_login_path: "/stealth-t3",
      });
      assertStatus(sec, 200);
      const v1 = sec.data.version;

      assertStatus(await anon.get("/console"), 404);
      assertStatus(await anon.get("/stealth-t3"), 200);

      assertStatus(
        await admin.patch("/api/admin/settings/security", { waf_enabled: true }, { omitCsrf: true }),
        403,
      );

      assertStatus(
        await admin.patch("/api/admin/settings/security", {
          expected_version: v1 + 99,
          waf_enabled: true,
        }),
        409,
      );
    } finally {
      await admin.patch("/api/admin/settings/security", { dynamic_login_path: "" }).catch(() => {});
    }
    assertStatus(await anon.get("/console"), 200);
  });

  it("S10 (F3+F9+F10): 2FA editor + scoped PAT (articles:write) creates tagged post in category, enforces RBAC/403, and revokes", async () => {
    const { user, password } = await createTempUser(admin, {
      role: "editor",
      current_password: srv.adminCreds.password,
    });
    const catRes = await admin.post("/api/admin/categories", { name: "S10 PAT Cat", slug: "t3-s10-cat" });
    assertStatus(catRes, 201);
    const catId = catRes.data.category.id;
    let artId = "";
    let patId = "";
    try {
      const ed = createClient(srv.baseUrl);
      assertStatus(await ed.login(user.username, password), 200);

      const totpSetup = await ed.post("/api/admin/security/totp/setup", {});
      assertStatus(totpSetup, 200);
      const secret = totpSetup.data.secret;
      assertStatus(
        await ed.post("/api/admin/security/totp/enroll", {
          secret,
          code: generateTOTP(secret, -1),
          current_password: password,
        }),
        200,
      );
      await ed.logout();

      const ch = await ed.login(user.username, password);
      assertStatus(ch, 200);
      assert.equal(ch.data.requires_2fa, true);
      assertStatus(await ed.verify2FA(ch.data.challenge_token, generateTOTP(secret, 0)), 200);

      const patRes = await ed.createPAT("t3-ed-write", ["articles:write"], password);
      assertStatus(patRes.res, 201);
      patId = patRes.token.id;

      const pat = ed.withBearer(patRes.plaintextToken);
      assertStatus(
        await pat.post("/api/admin/categories", { name: "Denied Editor Cat", slug: "t3-s10-denied-cat" }),
        403,
        "Editor PAT cannot create categories (admin-only)",
      );

      const art = await pat.post("/api/admin/articles", {
        title: "S10 PAT Post",
        slug: "t3-s10-post",
        category_id: catId,
        tags: ["t3s10"],
        status: "published",
        content_md: "Created by 2FA editor PAT",
      });
      assertStatus(art, 201);
      artId = art.data.article.id;

      assertStatus(await pat.get("/api/admin/settings"), 403);
      assertStatus(await pat.get("/api/admin/users"), 403);

      assertStatus(await ed.del(`/api/admin/security/tokens/${patId}`), 200);
      assertStatus(await pat.get("/api/admin/articles"), 401);
    } finally {
      await deleteArticleSafe(admin, artId);
      await admin.del(`/api/admin/categories/${catId}`).catch(() => {});
      await deleteUserSafe(admin, user.id);
    }
  });

  it("S11 (F9+F11+F12): media upload -> embed in post -> orphan check excludes -> delete post -> orphan check flags + webhook SSRF", async () => {
    const form = new FormData();
    form.append("file", new Blob([MINIMAL_PNG_BUFFER], { type: "image/png" }), "s11.png");
    const up = await admin.post("/api/admin/media/upload", form);
    assertStatus(up, 201);
    const assetId = up.data.asset.id;
    const assetUrl = up.data.url;

    const art = await createTempArticle(admin, {
      slug: "t3-s11-media-art",
      content_md: `![embedded](${assetUrl})`,
    });

    try {
      const scan1 = await admin.post("/api/admin/media/orphans");
      assertStatus(scan1, 200);
      assert.equal(scan1.data.orphans.some((o) => o.id === assetId), false, "Referenced media must not be orphan");

      await deleteArticleSafe(admin, art.id);

      const scan2 = await admin.post("/api/admin/media/orphans");
      assertStatus(scan2, 200);
      assert.equal(scan2.data.orphans.some((o) => o.id === assetId), true, "Unreferenced media must be flagged orphan");

      const ssrfHook = await admin.post("/api/admin/webhooks", {
        name: "S11 SSRF",
        provider: "bark",
        url: "http://169.254.169.254/hook",
        events: ["article.published"],
      });
      assertSSRFRejected(ssrfHook, "S11 webhook SSRF");
    } finally {
      await deleteArticleSafe(admin, art.id);
      await admin.del(`/api/admin/media/${assetId}`).catch(() => {});
    }
  });

  it("S12 (F6+F11): PoW + guest passport comment under moderation=all -> admin approve & reply -> /dynamic & public API", async () => {
    const art = await createTempArticle(admin, {
      slug: "t3-s12-cmt-art",
      allow_comment: true,
    });
    try {
      await withTempSettings(
        admin,
        "reading",
        { comment_moderation_mode: "all" },
        { comment_moderation_mode: "smart" },
        async () => {
          const guest = createClient(srv.baseUrl);
          const sub = await submitGuestComment(guest, art.id, {
            author_name: "S12PassportUser",
            content_md: "S12_COMMENT_MARKER",
          });
          assertStatus(sub, 201);
          assert.equal(sub.data.comment.status, "pending");
          assert.ok(sub.data.passport_token);
          const cid = sub.data.comment.id;

          assert.equal((await anon.get("/dynamic")).text.includes("S12_COMMENT_MARKER"), false);

          assertStatus(await admin.patch(`/api/admin/comments/${cid}`, { status: "approved" }), 200);
          const rep = await admin.post(`/api/admin/comments/${cid}/reply`, {
            content_md: "S12_ADMIN_REPLY_MARKER",
          });
          assertStatus(rep, 201);

          const dyn = await anon.get("/dynamic");
          assertStatus(dyn, 200);
          assert.ok(dyn.text.includes("S12_COMMENT_MARKER"));

          const apiCmt = await anon.get(`/api/v1/comments?target_type=article&target_id=${encodeURIComponent(art.id)}`);
          assertStatus(apiCmt, 200);
          assert.ok(apiCmt.text.includes("S12_ADMIN_REPLY_MARKER"));
        },
      );
    } finally {
      await deleteArticleSafe(admin, art.id);
    }
  });

  it("S13 (F4+F13): synthetic Goja ZIP plugin with http:routes + public.footer slot -> uninstall cleans up", async () => {
    const zipBuf = buildStoreZip([
      {
        name: "manifest.json",
        data: JSON.stringify({
          id: "t3-s13-slot-plugin",
          version: "1.0.0",
          min_host_version: "0.1.0",
          runtime: "goja",
          entry: "main.js",
          name: { "en-US": "S13 Slot Plugin" },
          permissions: ["http:routes", "slots:public.footer"],
          ui_slots: [{ slot: "public.footer", dynamic: true }],
        }),
      },
      {
        name: "main.js",
        data: [
          'function onRequest(req) { return { status: 200, headers: { "Content-Type": "application/json" }, body: JSON.stringify({ ok: true, s13: "active" }) }; }',
          'function renderSlot(slotName) { if (slotName === "public.footer") return "<div id=\\"t3-s13-slot\\">S13_FOOTER_SLOT</div>"; return ""; }',
          "module.exports = { onRequest: onRequest, renderSlot: renderSlot };",
        ].join("\n"),
      },
    ]);

    const form = new FormData();
    form.append("file", new Blob([zipBuf], { type: "application/zip" }), "s13.zip");
    assertStatus(await admin.post("/api/admin/plugins/install?enable=1", form), 201);

    try {
      const apiRes = await anon.get("/plugins/t3-s13-slot-plugin/api/ping");
      assertStatus(apiRes, 200);
      assert.equal(apiRes.data.s13, "active");

      const homeOn = await anon.get("/");
      assertStatus(homeOn, 200);
      assert.ok(homeOn.text.includes("S13_FOOTER_SLOT"));

      assertStatus(await admin.del("/api/admin/plugins/t3-s13-slot-plugin"), 200);
      assertStatus(await anon.get("/plugins/t3-s13-slot-plugin/api/ping"), [404, 410]);
      assert.equal((await anon.get("/")).text.includes("S13_FOOTER_SLOT"), false);
    } finally {
      await admin.del("/api/admin/plugins/t3-s13-slot-plugin").catch(() => {});
    }
  });

  it("S14 (F3+F8+F12): secrets across storage/notifications/backup + clear_secrets + PAT 403 + follows OPML intact", async () => {
    const patRes = await admin.createPAT("t3-s14-pat", ["articles:write"], srv.adminCreds.password);
    assertStatus(patRes.res, 201);
    const pat = admin.withBearer(patRes.plaintextToken);
    let followId = "";

    try {
      assertStatus(await admin.patch("/api/admin/settings/storage", { s3_secret_key: "s14-s3" }), 200);
      assertStatus(await admin.patch("/api/admin/settings/notifications", { smtp_password: "s14-smtp" }), 200);
      assertStatus(await admin.patch("/api/admin/settings/backup", { webdav_password: "s14-dav" }), 200);

      for (const dom of ["storage", "notifications", "backup"]) {
        assertStatus(await pat.get(`/api/admin/settings/${dom}`), 403, `PAT denied ${dom}`);
      }

      const fRes = await admin.post("/api/admin/follows", {
        title: "S14 OPML Feed",
        feed_url: "https://example.org/s14.xml",
      });
      assertStatus(fRes, 201);
      followId = fRes.data.follow.id;
      const opml = await admin.get("/api/admin/follows/export.opml");
      assertStatus(opml, 200);
      assert.ok(opml.text.includes("S14 OPML Feed"));

      const c1 = await admin.patch("/api/admin/settings/storage", { clear_secrets: ["s3_secret_key"] });
      assert.equal(c1.data.value.s3_secret_key_set, false);
      const c2 = await admin.patch("/api/admin/settings/notifications", { clear_secrets: ["smtp_password"] });
      assert.equal(c2.data.value.smtp_password_set, false);
      const c3 = await admin.patch("/api/admin/settings/backup", { clear_secrets: ["webdav_password"] });
      assert.equal(c3.data.value.webdav_password_set, false);
    } finally {
      if (followId) await admin.del(`/api/admin/follows/${followId}`).catch(() => {});
      if (patRes.token?.id) await admin.del(`/api/admin/security/tokens/${patRes.token.id}`).catch(() => {});
    }
  });

  it("S15 (F6+F10+F15): series-backed book sync + :::book + [toc] + show_code_line_numbers=false + :::lock unlock", async () => {
    const chPost = await createTempArticle(admin, {
      title: "S15 Book Chapter",
      slug: "t3-s15-ch",
      content_md: "Chapter body S15",
    });
    const ser = await admin.post("/api/admin/series", {
      title: "S15 Book Series",
      slug: "t3-s15-series",
      article_ids: [chPost.id],
    });
    assertStatus(ser, 201);
    const serId = ser.data.series.id;

    const book = await admin.post("/api/admin/books", {
      title: "S15 Synced Book",
      slug: "t3-s15-book",
      source_type: "series",
      series_id: serId,
      embed_mode: "inline",
    });
    assertStatus(book, 201);
    const bookId = book.data.book.id;

    let guideId = "";
    try {
      assertStatus(await admin.post(`/api/admin/books/${bookId}/sync`), 200);

      await withTempSettings(
        admin,
        "reading",
        { show_code_line_numbers: false },
        { show_code_line_numbers: true },
        async () => {
          const guide = await createTempArticle(admin, {
            title: "S15 Master Guide",
            slug: "t3-s15-guide",
            content_md: [
              "[toc]",
              "",
              "# Overview S15",
              "",
              "```go",
              "func s15() {}",
              "```",
              "",
              ':::book slug="t3-s15-book"',
              ":::",
              "",
              ':::lock pwd="s15-unlock-key"',
              "S15_UNLOCKED_PAYLOAD",
              ":::",
            ].join("\n"),
          });
          guideId = guide.id;

          const visitor = createClient(srv.baseUrl);
          const locked = await visitor.get(`/posts/${guide.slug}`);
          assertStatus(locked, 200);
          assert.ok(locked.text.includes('class="volo-inline-toc"'));
          assert.ok(locked.text.includes('data-book-slug="t3-s15-book"'));
          assert.equal(locked.text.includes('<span class="ln">'), false);
          assert.equal(locked.text.includes("S15_UNLOCKED_PAYLOAD"), false);

          assertStatus(
            await visitor.post(`/api/v1/articles/${guide.id}/unlock`, { password: "s15-unlock-key" }),
            200,
          );
          const unlocked = await visitor.get(`/posts/${guide.slug}`);
          assertStatus(unlocked, 200);
          assert.ok(unlocked.text.includes("S15_UNLOCKED_PAYLOAD"));
        },
      );
    } finally {
      await deleteArticleSafe(admin, guideId);
      await admin.del(`/api/admin/books/${bookId}`).catch(() => {});
      await admin.del(`/api/admin/series/${serId}`).catch(() => {});
      await deleteArticleSafe(admin, chPost.id);
    }
  });

  it("S16 (F4+F5+F12): standalone page + custom --admin-path + feed.output_mode='summary' in sitemap/robots/feed.json", async () => {
    const customSrv = await startVoloServer({
      adminPath: "/s16-panel",
      setupPayload: { default_locale: "en-US" },
    });
    try {
      const cAdmin = createClient(customSrv.baseUrl);
      const cAnon = createClient(customSrv.baseUrl);
      assertStatus(await cAdmin.login(customSrv.adminCreds.username, customSrv.adminCreds.password), 200);

      assertStatus(await cAdmin.patch("/api/admin/settings/feed", { output_mode: "summary" }), 200);
      await createTempArticle(cAdmin, {
        title: "S16 Summary Post",
        slug: "t3-s16-post",
        summary_md: "S16_SHORT_SUMMARY",
        content_md: "S16_FULL_ONLY_BODY_TOKEN",
      });
      assertStatus(
        await cAdmin.post("/api/admin/pages", {
          title: "S16 Standalone",
          slug: "t3-s16-page",
          content_md: "S16 page",
          is_published: true,
        }),
        201,
      );

      const sm = await cAnon.get("/sitemap.xml");
      assertStatus(sm, 200);
      assert.ok(sm.text.includes("/posts/t3-s16-post"));
      assert.ok(sm.text.includes("/t3-s16-page"));

      const rob = await cAnon.get("/robots.txt");
      assertStatus(rob, 200);
      assert.ok(rob.text.includes("Disallow: /api/"));
      assert.equal(rob.text.includes("/s16-panel"), false, "Custom cloaked --admin-path must not leak in robots.txt");
      assert.equal(rob.text.includes("/console/"), false, "Default /console/ must not appear when custom --admin-path is active");

      const jf = await cAnon.get("/feed.json");
      assertStatus(jf, 200);
      assert.ok(jf.text.includes("S16_SHORT_SUMMARY"));
      assert.equal(jf.text.includes("S16_FULL_ONLY_BODY_TOKEN"), false);
    } finally {
      await customSrv.stop();
    }
  });

  describe("Browser Cross-Feature Scenarios (Edge)", () => {
    let browser;

    before(async () => {
      browser = await launchEdge();
    });

    after(async () => {
      if (browser) {
        await browser.close();
      }
    });

    it("S17 (F7+F14+F15+F16): Edge #themes (volo-journal+ocean+dark) + kanbanniang + #editor publish -> public post", async () => {
      let createdArtId = "";
      try {
        assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
        assertStatus(await admin.patch("/api/admin/plugins/kanbanniang/config", { position: "right" }), 200);

        await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#themes");

          const journalCard = page.locator(".volo-theme-card", { hasText: /Volo Journal/i });
          await journalCard.waitFor({ state: "visible", timeout: 15000 });
          const activateWait = page.waitForResponse(
            (r) => r.url().includes("/api/admin/themes/activate") && r.request().method() === "POST" && r.status() === 200,
          );
          await journalCard.locator("button.volo-btn.volo-btn-sm.volo-btn-primary").click();
          await activateWait;

          const defaultModeField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Default.*Mode|默认.*模式|默认外观/i }) });
          await defaultModeField.locator("button.volo-cselect-btn").click();
          const modeMenu = page.locator('.volo-cselect-menu[role="listbox"]');
          await modeMenu.waitFor({ state: "visible", timeout: 5000 });
          await modeMenu.locator('button[role="option"]', { hasText: /Dark|深色/i }).click();

          await page.locator("button.volo-btn.volo-btn-sm", { hasText: /Ocean|海洋蓝/i }).click();

          const saveAppWait = page.waitForResponse(
            (r) => r.url().includes("/api/admin/settings/appearance") && r.request().method() === "PATCH" && r.status() === 200,
          );
          await page
            .locator(".volo-form-actions button.volo-btn.volo-btn-primary", { hasText: /Save|保存/ })
            .click();
          await saveAppWait;

          await waitForEditorReady(page, () =>
            page.goto(`${srv.baseUrl}/console#editor`, { waitUntil: "domcontentloaded" }),
          );

          await page.locator("input.volo-editor-title-input").fill("S17 Edge Combo Post");
          const slugInput = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Slug/i }) })
            .locator("input.volo-input");
          await slugInput.fill("t3-s17-edge-combo");

          const cm = page.locator(".volo-cm-host .cm-editor .cm-content");
          await cm.click();
          await page.keyboard.insertText(
            '## S17 Section\n\n[Safe](https://example.org) and [Bad](javascript:alert(1))\n\n```ts\nconst x: number = 7;\n```\n',
          );

          const savePromise = page.waitForResponse(
            (r) => r.url().includes("/api/admin/articles") && r.request().method() === "POST",
          );
          await page
            .locator(".volo-card-head button.volo-btn.volo-btn-sm.volo-btn-primary", {
              hasText: /Publish|发布文章/,
            })
            .click();
          const saveRes = await savePromise;
          const saved = await saveRes.json();
          createdArtId = saved.article.id;

          await page.goto(`${srv.baseUrl}/posts/t3-s17-edge-combo`, { waitUntil: "domcontentloaded" });
          const htmlAttrs = await page.evaluate(() => ({
            skin: document.documentElement.getAttribute("data-volo-skin"),
            accent: document.documentElement.getAttribute("data-accent"),
            defaultMode: document.documentElement.getAttribute("data-default-mode"),
            hasKbn: Boolean(document.querySelector("#volo-kanbanniang-root.volo-kbn-right")),
            hasJsHref: Boolean(document.querySelector('a[href^="javascript:"]')),
            bodyText: document.body.textContent || "",
          }));
          assert.equal(htmlAttrs.skin, "volo-journal");
          assert.equal(htmlAttrs.accent, "ocean");
          assert.equal(htmlAttrs.defaultMode, "dark");
          assert.equal(htmlAttrs.hasKbn, true);
          assert.equal(htmlAttrs.hasJsHref, false);
          assert.ok(htmlAttrs.bodyText.includes("S17 Section"));
          await checkNoHorizontalOverflow(page);
          diag.assertClean();
        });
      } finally {
        await deleteArticleSafe(admin, createdArtId);
        await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
        await admin.patch("/api/admin/settings/appearance", {
          active_theme: "volo-studio",
          accent: "forest",
          default_mode: "light",
        }).catch(() => {});
      }
    });

    it("S18 (F1+F6+F16): Edge #about BootConfig.version -> #settings_site title save -> public / header verification", async () => {
      try {
        await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
          const infoWait = page.waitForResponse(
            (r) => r.url().includes("/api/admin/about/project-info") && r.status() === 200,
          );
          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#about");
          const info = await (await infoWait).json();
          const badge = (await page.locator(".volo-about-hero .volo-badge").textContent()) || "";
          assert.ok(badge.includes(info.version));

          await page
            .locator("nav.volo-nav-scroll button.volo-nav-group-btn", { hasText: /General Settings|通用设置/ })
            .click();
          await page
            .locator("nav.volo-nav-scroll button.volo-nav-btn", { hasText: /Site & Reading|站点与阅读偏好/ })
            .click();
          const titleInput = page.locator('input[name="site_main_title"]');
          await titleInput.waitFor({ state: "visible", timeout: 10000 });
          await page.waitForFunction(() => {
            const el = document.querySelector('input[name="site_main_title"]');
            return el && el.value.length > 0;
          });
          await titleInput.fill("S18 Edge Verified Title");

          const siteForm = page.locator("form.volo-card").filter({ has: titleInput });
          const patchPromise = page.waitForResponse(
            (r) =>
              r.url().includes("/api/admin/settings/site") &&
              r.request().method() === "PATCH" &&
              r.status() === 200,
          );
          await siteForm.locator('button[type="submit"].volo-btn.volo-btn-primary').click();
          await patchPromise;
          await page.locator(".volo-toast.is-ok").waitFor({ state: "visible", timeout: 10000 });

          await page.goto(`${srv.baseUrl}/`, { waitUntil: "domcontentloaded" });
          const pubTitle = (await page.title()) || "";
          const pubBody = (await page.locator("body").textContent()) || "";
          assert.ok(pubTitle.includes("S18 Edge Verified Title") || pubBody.includes("S18 Edge Verified Title"));
          await checkNoHorizontalOverflow(page);
          diag.assertClean();
        });
      } finally {
        await admin.patch("/api/admin/settings/site", { title: "Tier3 Cross-Feature Blog" }).catch(() => {});
      }
    });
  });
});
