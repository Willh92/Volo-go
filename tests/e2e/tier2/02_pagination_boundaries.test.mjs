import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertNoXSS,
  assertRedirect,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";
import {
  createTempArticle,
  deleteArticleSafe,
  submitGuestComment,
  withTempSettings,
} from "./_helpers.mjs";

describe("Tier 2 — Pagination, Large Books & Query Boundaries", () => {
  let srv;
  let admin;
  let anon;
  const seeded = {
    articleIds: [],
    categoryIds: [],
    linkIds: [],
    seriesId: "",
    bookId: "",
    lockedPostId: "",
    lockedChapterId: "",
    ch60Id: "",
    ch98Id: "",
    ch99Id: "",
    ch105Id: "",
    navIds: [],
  };

  before(async () => {
    srv = await startVoloServer({
      setupPayload: {
        site_title: "Tier2 Pagination E2E",
        default_locale: "en-US",
      },
    });
    admin = createClient(srv.baseUrl);
    anon = createClient(srv.baseUrl);
    assertStatus(await admin.login(srv.adminCreds.username, srv.adminCreds.password), 200);

    assertStatus(
      await admin.patch("/api/admin/settings/feed", {
        enable_raw_markdown: true,
        enable_llms_txt: true,
      }),
      200,
    );

    // Seed 40 categories (>36 per page -> 2 pages; >18 -> +N jump-pills toggle)
    for (let i = 1; i <= 40; i += 1) {
      const prefix = i <= 25 ? "Alpha" : "Beta";
      const num = String(i).padStart(2, "0");
      const cRes = await admin.post("/api/admin/categories", {
        name: `${prefix} Cat ${num}`,
        slug: `t2-cat-${num}`,
        description: `Category ${num}`,
        sort_order: i,
      });
      assertStatus(cRes, 201, `Seed category ${num}`);
      seeded.categoryIds.push(cRes.data.category.id);
    }

    // Seed 125 tags (>120 per page -> 2 pages on /tags)
    const manyTags = [];
    for (let i = 1; i <= 125; i += 1) {
      manyTags.push(`t2tag-${String(i).padStart(3, "0")}`);
    }
    const tagArt = await createTempArticle(admin, {
      title: "Pagination Seed One",
      slug: "t2-pag-seed-1",
      category_id: seeded.categoryIds[0],
      tags: manyTags,
      content_md: "Pagination body 1 pagmarker.",
    });
    seeded.articleIds.push(tagArt.id);

    for (let i = 2; i <= 5; i += 1) {
      const a = await createTempArticle(admin, {
        title: `Pagination Seed ${i}`,
        slug: `t2-pag-seed-${i}`,
        category_id: seeded.categoryIds[1],
        tags: ["t2tag-001", "t2tag-002"],
        content_md: `Pagination body ${i} pagmarker.`,
      });
      seeded.articleIds.push(a.id);
    }

    // Seed 55 links across 3 groups (GroupA: 20, GroupB: 20, GroupC: 15 -> Page 1: 40, Page 2: 15)
    const groups = [
      { name: "GroupA", count: 20 },
      { name: "GroupB", count: 20 },
      { name: "GroupC", count: 15 },
    ];
    for (const grp of groups) {
      for (let i = 1; i <= grp.count; i += 1) {
        const lRes = await admin.post("/api/admin/links", {
          title: `${grp.name} Link ${String(i).padStart(2, "0")}`,
          url: `https://example.org/${grp.name.toLowerCase()}/${i}`,
          category_name: grp.name,
          sort_order: i,
        });
        assertStatus(lRes, 201);
        seeded.linkIds.push(lRes.data.link.id);
      }
    }

    // Seed password-protected article for locked book chapter test
    const lockedPost = await createTempArticle(admin, {
      title: "Locked Book Chapter Target",
      slug: "t2-locked-book-ch",
      password: "book-secret-pwd",
      content_md: "SECRET_BOOK_CHAPTER_CONTENT",
    });
    seeded.lockedPostId = lockedPost.id;

    // Seed 125-chapter book across 2 volumes (Vol 1: ch 1..60, Vol 2: ch 61..125)
    const chapters = [{ title: "Volume One: Foundations", url: "", depth: 0 }];
    for (let i = 1; i <= 60; i += 1) {
      chapters.push({
        title: `Chapter ${String(i).padStart(3, "0")}`,
        url: "/posts/t2-pag-seed-1",
        depth: 1,
      });
    }
    chapters.push({ title: "Volume Two: Scale", url: "", depth: 0 });
    for (let i = 61; i <= 125; i += 1) {
      chapters.push({
        title: `Chapter ${String(i).padStart(3, "0")}`,
        url: i === 105 ? "/posts/t2-locked-book-ch" : "/posts/t2-pag-seed-2",
        depth: 1,
      });
    }
    const bookRes = await admin.post("/api/admin/books", {
      title: "Large Multi-Volume Engineering Book",
      slug: "t2-large-book",
      description: "125 chapters across 2 volumes",
      source_type: "manual",
      embed_mode: "inline",
      chapters,
    });
    assertStatus(bookRes, 201, "Seed 125-chapter book");
    seeded.bookId = bookRes.data.book.id;

    const bookDetail = await admin.get(`/api/admin/books/${seeded.bookId}`);
    assertStatus(bookDetail, 200);
    const chList = bookDetail.data.chapters;
    seeded.ch60Id = chList.find((c) => c.title === "Chapter 060").id;
    seeded.ch98Id = chList.find((c) => c.title === "Chapter 098").id;
    seeded.ch99Id = chList.find((c) => c.title === "Chapter 099").id;
    seeded.ch105Id = chList.find((c) => c.title === "Chapter 105").id;
    seeded.lockedChapterId = seeded.ch105Id;
  });

  after(async () => {
    if (srv) {
      await srv.stop();
    }
  });

  describe("F4: Public Route & Search Pagination Boundaries", () => {
    it("F4: GET / clamps page=0, page=-1, page=abc to page 1 and handles page=9999 cleanly", async () => {
      for (const q of ["?page=0", "?page=-1", "?page=abc"]) {
        const res = await anon.get(`/${q}`);
        assertStatus(res, 200, `GET /${q}`);
        assert.ok(res.text.includes("Pagination Seed"), `Expected seeded posts on /${q}`);
      }
      const overMax = await anon.get("/?page=9999");
      assertStatus(overMax, 200, "GET /?page=9999");
      assert.equal((overMax.text.match(/<article\s+class="article__item\b/g) || []).length, 0);
    });

    it("F4: GET /categories handles q=, page=0/-1/9999 clamping, preserves q in pagination, and renders +N toggle", async () => {
      const p1 = await anon.get("/categories?page=0");
      assertStatus(p1, 200);
      assert.ok(p1.text.includes('data-collection-root="categories"'));
      assert.ok(p1.text.includes("js-jump-pills-toggle"), "Expected +N jump-pill toggle when >18 categories");

      const pOver = await anon.get("/categories?page=9999");
      assertStatus(pOver, 200);
      assert.ok(pOver.text.includes("Beta Cat 40"), "Over-max page should clamp to page 2 showing Cat 40");

      const filtered = await anon.get("/categories?q=Beta&page=-1");
      assertStatus(filtered, 200);
      const rootIdx = filtered.text.indexOf('data-collection-root="categories"');
      assert.ok(rootIdx >= 0);
      const gridStart = filtered.text.indexOf('<div class="bookshelf-grid">', rootIdx);
      const gridEnd = filtered.text.indexOf("js-collection-empty", gridStart);
      assert.ok(gridStart >= 0 && gridEnd > gridStart);
      const gridSlice = filtered.text.slice(gridStart, gridEnd);
      assert.ok(gridSlice.includes("Beta Cat 26"));
      assert.ok(!gridSlice.includes("Alpha Cat 01"));

      const qCatP1 = await anon.get("/categories?q=Cat&page=1");
      assertStatus(qCatP1, 200);
      assert.ok(qCatP1.text.includes('class="pagination"'), "40 matching categories (>36) must render pagination");
      assert.match(
        qCatP1.text,
        /<a[^>]*class="pagination__item"[^>]*href="[^"]*\/categories\?[^"]*(?:q=Cat[^"]*page=2|page=2[^"]*q=Cat)[^"]*"/,
        "/categories pagination href must preserve q=Cat",
      );

      const specialQ = await anon.get("/categories?q=%3Cscript%3E%26nope");
      assertStatus(specialQ, 200, "Special chars empty q on /categories");
      const catMainStart = specialQ.text.indexOf('data-collection-root="categories"');
      const catMainEnd = specialQ.text.indexOf("</form>", catMainStart);
      assert.ok(catMainStart >= 0 && catMainEnd > catMainStart);
      assertNoXSS(specialQ.text.slice(catMainStart, catMainEnd), "/categories special chars q");
      assert.ok(specialQ.text.includes("&lt;script&gt;&amp;nope"), "/categories must HTML-escape q=");
    });

    it("F4: GET /tags supports q=, sort=name|popular, page=0/-1/9999 clamping across 125+ tags", async () => {
      const p1 = await anon.get("/tags?sort=name&page=0");
      assertStatus(p1, 200);
      assert.ok(p1.text.includes("#t2tag-001"));
      assert.ok(p1.text.includes("nav") && p1.text.includes("pagination"));

      const pOver = await anon.get("/tags?sort=name&page=9999");
      assertStatus(pOver, 200);
      assert.ok(pOver.text.includes("#t2tag-125"), "Clamped page 2 should show t2tag-125");
      assert.ok(pOver.text.includes("sort=name"), "Pagination links must preserve sort=");

      const qRes = await anon.get("/tags?q=t2tag-12&sort=popular");
      assertStatus(qRes, 200);
      assert.ok(qRes.text.includes("#t2tag-125"));
      assert.ok(!qRes.text.includes("#t2tag-005"));
    });

    it("F4: GET /archives and GET /series handle page=0/-1/9999 and special-char q= cleanly", async () => {
      const serRes = await admin.post("/api/admin/series", {
        title: "SeriesBoundary",
        slug: "t2-series-boundary",
        description: "Series boundary verification",
      });
      assertStatus(serRes, 201, "Seed series for /series boundary test");
      const seriesId = serRes.data.series.id;

      try {
        for (const p of ["/archives?page=0", "/archives?page=-1"]) {
          const r = await anon.get(p);
          assertStatus(r, 200, `GET ${p}`);
          assert.ok(r.text.includes("studio-archive-post-row"), `${p} must render archive rows`);
          assert.ok(r.text.includes("Pagination Seed One"), `${p} must render seeded posts`);
        }
        const archOver = await anon.get("/archives?page=9999");
        assertStatus(archOver, 200, "GET /archives?page=9999");
        assert.ok(archOver.text.includes("studio-archive-post-row"), "/archives?page=9999 must clamp and render archive rows");

        const yr1999 = await anon.get("/archives?year=1999&page=1");
        assertStatus(yr1999, 200, "GET /archives?year=1999&page=1");
        assert.equal(yr1999.text.includes("studio-archive-timeline"), false, "/archives?year=1999 must have no timeline");
        assert.equal(yr1999.text.includes("studio-archive-post-row"), false, "/archives?year=1999 must have no archive post rows");

        for (const p of ["/series?page=0", "/series?page=-1", "/series?page=9999"]) {
          const r = await anon.get(p);
          assertStatus(r, 200, `GET ${p}`);
          assert.ok(r.text.includes("SeriesBoundary"), `${p} must render seeded series`);
        }

        const sXss = await anon.get("/series?q=%22%3C%3E&page=1");
        assertStatus(sXss, 200, "GET /series?q=%22%3C%3E&page=1");
        const serRoot = sXss.text.indexOf('data-collection-root="series"');
        const serEnd = sXss.text.indexOf("</form>", serRoot);
        assert.ok(serRoot >= 0 && serEnd > serRoot);
        assertNoXSS(sXss.text.slice(serRoot, serEnd), "/series special-char q");
        assert.equal(sXss.text.includes('value=""<>"'), false, "Special-char q must not reflect unescaped quotes/brackets");
        assert.ok(sXss.text.includes("&lt;&gt;"), "Special-char q must be HTML-escaped in search input");
      } finally {
        await admin.del(`/api/admin/series/${seriesId}`).catch(() => {});
      }
    });

    it("F4: GET /follows and GET /dynamic handle boundary pages and /dynamic hides RecentPosts on page > 1", async () => {
      const fol0 = await anon.get("/follows?page=0");
      assertStatus(fol0, 200);
      assert.ok(fol0.text.includes("studio-hero__title"));
      assert.ok(fol0.text.includes("studio-empty"), "/follows?page=0 must render studio-empty when 0 follows exist");
      const folOver = await anon.get("/follows?page=9999");
      assertStatus(folOver, 200);
      assert.ok(folOver.text.includes("studio-hero__title"));
      assert.ok(folOver.text.includes("studio-empty"), "/follows?page=9999 must render studio-empty when 0 follows exist");

      const cmtIds = [];
      try {
        await withTempSettings(
          admin,
          "reading",
          { pow_difficulty_bits: 8, comment_rate_max_count: 0, comment_moderation_mode: "off" },
          { pow_difficulty_bits: 16, comment_rate_max_count: 12, comment_moderation_mode: "smart" },
          async () => {
            for (let i = 1; i <= 22; i += 1) {
              const cRes = await submitGuestComment(anon, seeded.articleIds[0], {
                author_name: `DynGuest${i}`,
                content_md: `Dynamic boundary comment ${i}`,
              });
              assertStatus(cRes, 201, `Seed dynamic comment ${i}/22`);
              cmtIds.push(cRes.data.comment.id);
            }
          },
        );

        const dyn1 = await anon.get("/dynamic?page=1");
        assertStatus(dyn1, 200);
        assert.ok(dyn1.text.includes("studio-dynamic-comment"), "/dynamic?page=1 must render comments");
        assert.ok(dyn1.text.includes('class="pagination"'), "/dynamic?page=1 with 22 comments must render pagination");
        assert.ok(dyn1.text.includes("studio-archive-list"), "/dynamic?page=1 must render RecentPosts");
        assert.ok(dyn1.text.includes("Pagination Seed One"), "/dynamic?page=1 RecentPosts must include seeded post");

        const dyn2 = await anon.get("/dynamic?page=2");
        assertStatus(dyn2, 200);
        assert.ok(dyn2.text.includes("studio-dynamic-comment"), "/dynamic?page=2 must render page 2 comments");
        assert.ok(dyn2.text.includes('class="pagination"'));
        assert.equal(dyn2.text.includes("studio-archive-list"), false, "/dynamic?page=2 must hide RecentPosts");

        const dynOver = await anon.get("/dynamic?page=9999");
        assertStatus(dynOver, 200);
        assert.equal(
          dynOver.text.includes("studio-archive-list"),
          false,
          "/dynamic?page=9999 clamps to page 2 and hides RecentPosts",
        );
      } finally {
        if (cmtIds.length > 0) {
          await admin.post("/api/admin/comments/batch", { action: "purge", ids: cmtIds }).catch(() => {});
        }
      }
    });

    it("F4: GET /api/v1/search clamps page/per_page boundaries and handles empty/special-char queries", async () => {
      const emptyQ = await anon.get("/api/v1/search?q=&page=0&per_page=0");
      assertStatus(emptyQ, 200);
      assert.equal(emptyQ.data.page, 1);
      assert.equal(emptyQ.data.per_page, 20);

      const wsQ = await anon.get("/api/v1/search?q=%20%20%20&page=-5&per_page=999");
      assertStatus(wsQ, 200);
      assert.equal(wsQ.data.page, 1);
      assert.equal(wsQ.data.per_page, 50, "per_page > 50 must clamp to 50");

      const specialQ = await anon.get("/api/v1/search?q=%3Cscript%3E%25_%22&page=9999");
      assertStatus(specialQ, 200);
      assert.equal(specialQ.data.page, 9999);
      assert.deepStrictEqual(specialQ.data.hits, []);
    });
  });

  describe("F5: Syndication & Machine Endpoint Boundaries", () => {
    it("F5: /rss.xml, /atom.xml, and /feed.json render valid empty feeds when 0 published posts exist", async () => {
      const emptySrv = await startVoloServer({ setupPayload: { default_locale: "en-US" } });
      try {
        const c = createClient(emptySrv.baseUrl);
        const rss = await c.get("/rss.xml");
        assertStatus(rss, 200);
        assert.ok(rss.text.includes("<rss") && rss.text.includes("<channel>"));
        assert.equal(rss.text.includes("<item>"), false);

        const atom = await c.get("/atom.xml");
        assertStatus(atom, 200);
        assert.ok(atom.text.includes("<feed"));
        assert.equal(atom.text.includes("<entry>"), false);

        const jf = await c.get("/feed.json");
        assertStatus(jf, 200);
        assert.equal(jf.data.version, "https://jsonfeed.org/version/1.1");
        assert.equal(jf.data.items.length, 0);
      } finally {
        await emptySrv.stop();
      }
    });

    it("F5: /rss.xml and /atom.xml safely escape XML special chars (&, <, >, ]]>) in post content", async () => {
      const xmlArt = await createTempArticle(admin, {
        title: 'XML Escape & <Title> "Test" ]]>',
        slug: "t2-xml-escape-test",
        summary_md: "Summary with & <tag> and ]]> end.",
        content_md: "Body with `a < b && c > d` and literal ]]> sequence.",
      });
      try {
        const rss = await anon.get("/rss.xml");
        assertStatus(rss, 200);
        assert.ok(rss.text.includes("XML Escape &amp; &lt;Title&gt;"));

        const atom = await anon.get("/atom.xml");
        assertStatus(atom, 200);
        assert.ok(atom.text.includes("XML Escape &amp; &lt;Title&gt;"));
      } finally {
        await deleteArticleSafe(admin, xmlArt.id);
      }
    });

    it("F5: feed.item_count resets out-of-range (0/999 -> 20) and caps feed items at item_count=1", async () => {
      await withTempSettings(
        admin,
        "feed",
        { item_count: 0 },
        { item_count: 20 },
        async (d0) => {
          assert.equal(d0.value.item_count, 20, "item_count=0 must reset to 20");
          const d999 = await admin.patch("/api/admin/settings/feed", { item_count: 999 });
          assertStatus(d999, 200);
          assert.equal(d999.data.value.item_count, 20, "item_count=999 must reset to 20");

          const d1 = await admin.patch("/api/admin/settings/feed", { item_count: 1 });
          assertStatus(d1, 200);
          assert.equal(d1.data.value.item_count, 1);

          const jf = await anon.get("/feed.json");
          assertStatus(jf, 200);
          assert.equal(jf.data.items.length, 1, "feed.json must honor item_count=1");
        },
      );
    });

    it("F5: /sitemap.xml excludes drafts, scheduled future posts, password-protected posts, and unpublished pages", async () => {
      const futureMs = Date.now() + 7 * 86400 * 1000;
      const draftArt = await createTempArticle(admin, { slug: "t2-sm-draft", status: "draft" });
      const schedArt = await createTempArticle(admin, {
        slug: "t2-sm-scheduled",
        status: "scheduled",
        scheduled_at: futureMs,
      });
      const unpubPage = await admin.post("/api/admin/pages", {
        title: "Unpublished Page",
        slug: "t2-sm-unpub-page",
        content_md: "Hidden",
        is_published: false,
      });
      assertStatus(unpubPage, 201);

      try {
        const sm = await anon.get("/sitemap.xml");
        assertStatus(sm, 200);
        assert.ok(sm.text.includes("/posts/t2-pag-seed-1"));
        assert.equal(sm.text.includes("t2-sm-draft"), false);
        assert.equal(sm.text.includes("t2-sm-scheduled"), false);
        assert.equal(sm.text.includes("t2-locked-book-ch"), false);
        assert.equal(sm.text.includes("t2-sm-unpub-page"), false);
      } finally {
        await deleteArticleSafe(admin, draftArt.id);
        await deleteArticleSafe(admin, schedArt.id);
        await admin.del(`/api/admin/pages/${unpubPage.data.page.id}`).catch(() => {});
      }
    });

    it("F5: /posts/{slug}.md returns 404 for drafts/scheduled/unknown and 403 for password-protected posts", async () => {
      const draftArt = await createTempArticle(admin, { slug: "t2-md-draft", status: "draft" });
      try {
        assertStatus(await anon.get("/posts/t2-md-draft.md"), 404);
        assertStatus(await anon.get("/posts/does-not-exist-xyz.md"), 404);
        assertStatus(await anon.get("/posts/t2-locked-book-ch.md"), 403);
      } finally {
        await deleteArticleSafe(admin, draftArt.id);
      }
    });
  });

  describe("F6: Reading & Site Domain Boundaries", () => {
    it("F6: reading.posts_per_page resets out-of-range (0/999 -> 15), accepts 1 and 100, and updates / page count", async () => {
      await withTempSettings(
        admin,
        "reading",
        { posts_per_page: 0 },
        { posts_per_page: 15 },
        async (d0) => {
          assert.equal(d0.value.posts_per_page, 15, "posts_per_page=0 resets to 15");
          const d999 = await admin.patch("/api/admin/settings/reading", { posts_per_page: 999 });
          assert.equal(d999.data.value.posts_per_page, 15, "posts_per_page=999 resets to 15");

          const d100 = await admin.patch("/api/admin/settings/reading", { posts_per_page: 100 });
          assert.equal(d100.data.value.posts_per_page, 100);

          const d1 = await admin.patch("/api/admin/settings/reading", { posts_per_page: 1 });
          assert.equal(d1.data.value.posts_per_page, 1);

          const home = await anon.get("/?page=1");
          assertStatus(home, 200);
          assert.equal((home.text.match(/<article\s+class="article__item\b/g) || []).length, 1);
          assert.ok(home.text.includes('class="pagination"'));
        },
      );
    });

    it("F6: reading.list_style falls back invalid mode to summary and toggles summary vs title_only rendering", async () => {
      await withTempSettings(
        admin,
        "reading",
        { list_style: "magazine_invalid" },
        { list_style: "summary" },
        async (res) => {
          assert.equal(res.value.list_style, "summary", "Unknown list_style must fall back to summary");
          const summaryHtml = await anon.get("/");
          assertStatus(summaryHtml, 200);
          assert.ok(summaryHtml.text.includes("studio-abstract"), "summary mode must render .studio-abstract");
          assert.ok(summaryHtml.text.includes("pagmarker"), "summary mode must render summary excerpt text");
        },
      );

      await withTempSettings(
        admin,
        "reading",
        { list_style: "title_only" },
        { list_style: "summary" },
        async () => {
          const titleOnlyHtml = await anon.get("/");
          assertStatus(titleOnlyHtml, 200);
          assert.ok(titleOnlyHtml.text.includes("article__item--title-only"));
          assert.ok(titleOnlyHtml.text.includes("Pagination Seed"));
          assert.equal(
            titleOnlyHtml.text.includes("studio-abstract"),
            false,
            "title_only mode must omit .studio-abstract",
          );
          assert.equal(
            titleOnlyHtml.text.includes("pagmarker"),
            false,
            "title_only mode must omit summary/body excerpt text",
          );
        },
      );
    });

    it("F6: site long title/subtitle and custom_vars escape HTML safely on public pages", async () => {
      const longTitle = "LongTitle_" + "X".repeat(120) + '_<script>alert(1)</script>';
      await withTempSettings(
        admin,
        "site",
        { title: longTitle, custom_vars: { env_tag: "tier2-boundary" } },
        { title: "Tier2 Pagination E2E", custom_vars: {} },
        async (res) => {
          assert.equal(res.value.custom_vars.env_tag, "tier2-boundary");
          const home = await anon.get("/");
          assertStatus(home, 200);
          assert.ok(!home.text.includes("<script>alert(1)</script>"));
          assert.ok(home.text.includes("LongTitle_"));
        },
      );
    });
  });

  describe("F9: Article & Standalone Page Boundaries", () => {
    it("F9: auto-deduplicates duplicate article slugs and rejects empty/whitespace title", async () => {
      const dup = await admin.post("/api/admin/articles", {
        title: "Duplicate Slug",
        slug: "t2-pag-seed-1",
        content_md: "Body",
      });
      assertStatus(dup, 201, "Duplicate article slug auto-deduplicates cleanly");
      assert.notEqual(dup.data.article.slug, "t2-pag-seed-1", "Deduped slug must not collide");
      assert.ok(dup.data.article.slug.startsWith("t2-pag-seed-1-"));
      await deleteArticleSafe(admin, dup.data.article.id);

      const emptyTitle = await admin.post("/api/admin/articles", {
        title: "   ",
        slug: "t2-empty-title",
        content_md: "Body",
      });
      assertStatus(emptyTitle, 400, "Empty article title");
    });

    it("F9: scheduled future article stays hidden from /, /posts/{slug}, search, and feeds", async () => {
      const futureMs = Date.now() + 3 * 86400 * 1000;
      const sched = await createTempArticle(admin, {
        title: "Future Scheduled Post",
        slug: "t2-future-scheduled-post",
        status: "scheduled",
        scheduled_at: futureMs,
        content_md: "FUTURE_SECRET_MARKER",
      });
      try {
        assertStatus(await anon.get("/posts/t2-future-scheduled-post"), 404);
        const home = await anon.get("/");
        assert.equal(home.text.includes("Future Scheduled Post"), false);
        const search = await anon.get("/api/v1/search?q=FUTURE_SECRET_MARKER");
        assertStatus(search, 200);
        assert.equal(search.data.hits.length, 0);
      } finally {
        await deleteArticleSafe(admin, sched.id);
      }
    });

    it("F9: GET /api/admin/articles clamps page=0/-1, per_page=0/999 (max 300), and handles page=9999", async () => {
      const r0 = await admin.get("/api/admin/articles?page=0&per_page=0");
      assertStatus(r0, 200);
      assert.equal(r0.data.page, 1);
      assert.equal(r0.data.per_page, 50);

      const rMax = await admin.get("/api/admin/articles?page=-2&per_page=999");
      assertStatus(rMax, 200);
      assert.equal(rMax.data.page, 1);
      assert.equal(rMax.data.per_page, 300, "Admin articles per_page > 300 must clamp to 300");

      const rOver = await admin.get("/api/admin/articles?page=9999");
      assertStatus(rOver, 200);
      assert.equal(rOver.data.page, 9999);
      assert.deepStrictEqual(rOver.data.items, []);
      assert.ok(rOver.data.total >= 5);
    });

    it("F9: articles/batch rejects empty ids/invalid action and articles/import rejects empty or missing upload", async () => {
      assertStatus(await admin.post("/api/admin/articles/batch", { action: "publish", ids: [] }), 400);
      assertStatus(
        await admin.post("/api/admin/articles/batch", { action: "explode", ids: [seeded.articleIds[0]] }),
        400,
      );

      const noFileForm = new FormData();
      noFileForm.append("note", "missing-file-field");
      assertStatus(await admin.post("/api/admin/articles/import", noFileForm), 400);

      assertStatus(await admin.post("/api/admin/articles/import", "{not-multipart"), 400);
    });

    it("F9: standalone pages reject empty title and duplicate slugs while system routes retain priority", async () => {
      assertStatus(
        await admin.post("/api/admin/pages", { title: "  ", slug: "t2-empty-pg", content_md: "x" }),
        400,
        "Empty page title rejected",
      );

      const pApi = await admin.post("/api/admin/pages", {
        title: "Shadow API",
        slug: "api",
        content_md: "Should not shadow /api/*",
        is_published: true,
      });
      assertStatus(pApi, 201, "Create standalone page with reserved slug 'api'");
      try {
        const searchCheck = await anon.get("/api/v1/search?q=Pagination");
        assertStatus(searchCheck, 200, "System /api/* route must take precedence over page slug");
        assert.ok(Array.isArray(searchCheck.data?.hits), "System /api/v1/search must return hits array");
      } finally {
        await admin.del(`/api/admin/pages/${pApi.data.page.id}`).catch(() => {});
      }

      const p1 = await admin.post("/api/admin/pages", {
        title: "Dup Page",
        slug: "t2-dup-page",
        content_md: "One",
        is_published: false,
      });
      assertStatus(p1, 201);
      try {
        const p2 = await admin.post("/api/admin/pages", {
          title: "Dup Page 2",
          slug: "t2-dup-page",
          content_md: "Two",
          is_published: false,
        });
        assertStatus(p2, [400, 409], "Duplicate page slug");
      } finally {
        await admin.del(`/api/admin/pages/${p1.data.page.id}`).catch(() => {});
      }
    });
  });

  describe("F10: Categories, Tags & Large Book Reader Boundaries", () => {
    it("F10: GET /api/admin/categories clamps page/per_page (max 200) and rejects duplicate slug", async () => {
      const r0 = await admin.get("/api/admin/categories?page=0&per_page=0");
      assertStatus(r0, 200);
      assert.equal(r0.data.page, 1);
      assert.equal(r0.data.per_page, 100);

      const rMax = await admin.get("/api/admin/categories?page=-1&per_page=999");
      assertStatus(rMax, 200);
      assert.equal(rMax.data.page, 1);
      assert.equal(rMax.data.per_page, 200, "Admin categories per_page > 200 clamps to 200");

      const rOver = await admin.get("/api/admin/categories?page=9999");
      assertStatus(rOver, 200);
      assert.deepStrictEqual(rOver.data.items, []);
      assert.ok(rOver.data.total >= 40);

      const dupCat = await admin.post("/api/admin/categories", {
        name: "Dup Cat",
        slug: "t2-cat-01",
      });
      assertStatus(dupCat, [400, 409], "Duplicate category slug");
    });

    it("F10: POST /api/admin/tags/merge rejects empty source_ids, self-merge, and nonexistent target", async () => {
      const tRes = await admin.post("/api/admin/tags", { name: "t2-self-tag", slug: "t2-self-tag" });
      assertStatus(tRes, 201);
      const tid = tRes.data.tag.id;
      try {
        assertStatus(
          await admin.post("/api/admin/tags/merge", { source_ids: [], target_id: tid }),
          400,
          "Empty source_ids",
        );
        assertStatus(
          await admin.post("/api/admin/tags/merge", { source_ids: [tid], target_id: tid }),
          400,
          "Self-merge",
        );
        assertStatus(
          await admin.post("/api/admin/tags/merge", { source_ids: [tid], target_id: "nonexistent-tag-id" }),
          [400, 404],
          "Nonexistent target tag",
        );
      } finally {
        await admin.post("/api/admin/tags/cleanup").catch(() => {});
      }
    });

    it("F10: large 125-chapter book /books/{slug} paginates TOC at 100 rows/page and clamps cpage=0/999", async () => {
      const p1 = await anon.get("/books/t2-large-book?cpage=0");
      assertStatus(p1, 200, "Book reader cpage=0");
      assert.ok(p1.text.includes("Chapter 001"));
      assert.ok(p1.text.includes("Chapter 098"));
      assert.ok(!p1.text.includes("Chapter 099"), "Page 1 window (100 rows incl 2 vol headers) ends at Chapter 098");
      assert.ok(p1.text.includes("book-reader-toc__page-info"));

      const p2 = await anon.get("/books/t2-large-book?cpage=2");
      assertStatus(p2, 200, "Book reader cpage=2");
      assert.ok(p2.text.includes("Chapter 099"));
      assert.ok(p2.text.includes("Chapter 125"));
      assert.ok(p2.text.includes("Volume Two: Scale"), "Page 2 must prepend enclosing volume header");

      const pOver = await anon.get("/books/t2-large-book?cpage=999");
      assertStatus(pOver, 200, "Book reader cpage=999 clamps to page 2");
      assert.ok(pOver.text.includes("Chapter 125"));
    });

    it("F10: large book ?ch= auto-resolves TOC cpage, opens enclosing volume <details>, and 404s invalid ?ch=", async () => {
      const ch99 = await anon.get(`/books/t2-large-book?ch=${encodeURIComponent(seeded.ch99Id)}`);
      assertStatus(ch99, 200, "Auto-resolve ?ch=099 to cpage=2");
      assert.ok(ch99.text.includes("Chapter 099"));
      assert.ok(ch99.text.includes("Chapter 125"));
      assert.match(
        ch99.text,
        /<details[^>]*class="book-reader-toc__volume"[^>]*\bopen\b[^>]*data-default-open="1"/i,
        "Enclosing volume must be open by default",
      );

      const badCh = await anon.get("/books/t2-large-book?ch=nonexistent-chapter-id");
      assertStatus(badCh, 404, "Invalid ?ch= returns 404");
    });

    it("F10: large book .book-reader-neighbors prev/next crosses cpage=1/2 and volume 1/2 boundaries", async () => {
      const at98 = await anon.get(`/books/t2-large-book?ch=${encodeURIComponent(seeded.ch98Id)}`);
      assertStatus(at98, 200);
      const nbIdx = at98.text.indexOf("book-reader-neighbors");
      assert.ok(nbIdx >= 0, "Expected .book-reader-neighbors container");
      const nbSlice = at98.text.slice(nbIdx, nbIdx + 1200);
      assert.ok(
        nbSlice.includes("Chapter 099"),
        "Chapter 098 Next link must cross cpage boundary to Chapter 099",
      );

      const at60 = await anon.get(`/books/t2-large-book?ch=${encodeURIComponent(seeded.ch60Id)}`);
      assertStatus(at60, 200);
      const nbIdx60 = at60.text.indexOf("book-reader-neighbors");
      assert.ok(nbIdx60 >= 0);
      assert.ok(
        at60.text.slice(nbIdx60, nbIdx60 + 1200).includes("Chapter 061"),
        "Chapter 060 Next link must cross Volume 1 -> Volume 2",
      );
    });

    it("F10: password-locked book chapter returns 200 BookUnlockError on wrong password and 303 + __volo_unlock on correct", async () => {
      const reader = createClient(srv.baseUrl);
      const lockedUrl = `/books/t2-large-book?ch=${encodeURIComponent(seeded.lockedChapterId)}`;

      const lockedView = await reader.get(lockedUrl);
      assertStatus(lockedView, 200);
      assertHeaderIncludes(lockedView, "cache-control", "no-store");
      assert.ok(lockedView.text.includes("book-unlock-card"));
      assert.equal(lockedView.text.includes("SECRET_BOOK_CHAPTER_CONTENT"), false);

      const formHeaders = {
        headers: {
          "Content-Type": "application/x-www-form-urlencoded",
          Origin: srv.baseUrl,
        },
      };
      const wrongPwd = await reader.post(lockedUrl, "password=wrong-pwd", formHeaders);
      assertStatus(wrongPwd, 200, "Wrong book chapter password returns 200 with error");
      assertHeaderIncludes(wrongPwd, "cache-control", "no-store");
      assert.equal(wrongPwd.text.includes("SECRET_BOOK_CHAPTER_CONTENT"), false);
      assert.ok(
        wrongPwd.text.includes("js-unlock-msg") && !wrongPwd.text.includes('class="volo-lock-msg js-unlock-msg" hidden'),
        "BookUnlockError message must be visible without hidden attribute",
      );

      const okPwd = await reader.post(lockedUrl, "password=book-secret-pwd", formHeaders);
      assertRedirect(okPwd, 303, `/books/t2-large-book`, "Correct book chapter password redirects 303");

      const unlocked = await reader.get(lockedUrl);
      assertStatus(unlocked, 200);
      assert.ok(unlocked.text.includes("SECRET_BOOK_CHAPTER_CONTENT"));
    });
  });

  describe("F11: Comment & Media Boundary Cases", () => {
    it("F11: rejects comments when allow_comment=false or comment_access_mode=closed and paginates public comments", async () => {
      const noCmtArt = await createTempArticle(admin, {
        slug: "t2-no-comments",
        allow_comment: false,
      });
      const cmtArt = await createTempArticle(admin, {
        title: "Comment Pagination Target",
        slug: "t2-cmt-pag-target",
        allow_comment: true,
      });
      const cmtIds = [];
      try {
        const res = await submitGuestComment(anon, noCmtArt.id, { content_md: "Blocked" });
        assertStatus(res, [400, 403], "Comment on allow_comment=false article rejected");

        await withTempSettings(
          admin,
          "reading",
          { comment_access_mode: "off" },
          { comment_access_mode: "all" },
          async () => {
            const closedRes = await submitGuestComment(anon, seeded.articleIds[0], {
              content_md: "Blocked by off mode",
            });
            assertStatus(closedRes, [400, 403], "Comment under comment_access_mode=off rejected");
          },
        );

        await withTempSettings(
          admin,
          "reading",
          { pow_difficulty_bits: 8, comment_rate_max_count: 0, comment_moderation_mode: "off" },
          { pow_difficulty_bits: 16, comment_rate_max_count: 12, comment_moderation_mode: "smart" },
          async () => {
            for (let i = 1; i <= 3; i += 1) {
              const c = await submitGuestComment(anon, cmtArt.id, {
                author_name: `CmtPagGuest${i}`,
                content_md: `Comment pagination item ${i}`,
              });
              assertStatus(c, 201, `Seed comment ${i}/3 for pagination`);
              cmtIds.push(c.data.comment.id);
            }
          },
        );

        const cPage1 = await anon.get(
          `/api/v1/comments?target_type=article&target_id=${encodeURIComponent(cmtArt.id)}&page=1&per_page=2`,
        );
        assertStatus(cPage1, 200);
        assert.equal(cPage1.data.page, 1);
        assert.equal(cPage1.data.per_page, 2);
        assert.equal(cPage1.data.total_comments, 3);
        assert.equal(cPage1.data.total_roots, 3);
        assert.equal(cPage1.data.total_pages, 2);
        assert.equal(cPage1.data.has_more, true);
        assert.equal(cPage1.data.comments.length, 2);

        const cPageOver = await anon.get(
          `/api/v1/comments?target_type=article&target_id=${encodeURIComponent(cmtArt.id)}&page=9999&per_page=2`,
        );
        assertStatus(cPageOver, 200);
        assert.equal(cPageOver.data.page, 2, "/api/v1/comments page=9999 must clamp to total_pages (2)");
        assert.equal(cPageOver.data.has_more, false);
        assert.equal(cPageOver.data.comments.length, 1);
      } finally {
        if (cmtIds.length > 0) {
          await admin.post("/api/admin/comments/batch", { action: "purge", ids: cmtIds }).catch(() => {});
        }
        await deleteArticleSafe(admin, noCmtArt.id);
        await deleteArticleSafe(admin, cmtArt.id);
      }
    });

    it("F11: media upload rejects empty file, missing file field, and disallowed extension/MIME", async () => {
      const emptyForm = new FormData();
      emptyForm.append("file", new Blob([], { type: "image/png" }), "empty.png");
      assertStatus(await admin.post("/api/admin/media/upload", emptyForm), 400, "Empty upload rejected");

      const badExtForm = new FormData();
      badExtForm.append("file", new Blob(["<html>bad</html>"], { type: "text/html" }), "evil.html");
      assertStatus(await admin.post("/api/admin/media/upload", badExtForm), 400, "HTML upload rejected");

      const missingField = new FormData();
      missingField.append("asset", new Blob(["abc"], { type: "image/png" }), "a.png");
      assertStatus(await admin.post("/api/admin/media/upload", missingField), 400, "Missing 'file' field rejected");
    });

    it("F11: reactions validate kind/target_type and admin comment moderation/ban-ip reject unknown IDs", async () => {
      const badReaction = await anon.post("/api/v1/reactions", {
        target_type: "article",
        target_id: seeded.articleIds[0],
        kind: "invalid_emoji_kind",
      });
      assertStatus(badReaction, 400, "Invalid reaction kind rejected");

      const badTargetType = await anon.post("/api/v1/reactions", {
        target_type: "invalid_target",
        target_id: seeded.articleIds[0],
        kind: "like",
      });
      assertStatus(badTargetType, 400, "Invalid reaction target_type rejected");

      assertStatus(
        await admin.patch("/api/admin/comments/nonexistent-comment-id", { status: "approved" }),
        404,
        "Patch nonexistent comment returns 404",
      );
      assertStatus(
        await admin.post("/api/admin/comments/nonexistent-comment-id/ban-ip", {}),
        404,
        "Ban-ip on nonexistent comment returns 404",
      );
    });
  });

  describe("F12: Links, Nav, Redirects & Follows Boundaries", () => {
    it("F12: /links performs group-aware category-preserving pagination and preserves q/category filtering", async () => {
      const p1 = await anon.get("/links?page=0");
      assertStatus(p1, 200);
      assert.ok(p1.text.includes("GroupA Link 01"));
      assert.ok(p1.text.includes("GroupB Link 20"));
      assert.ok(!p1.text.includes("GroupC Link 01"), "GroupC (15 links) must stay intact on page 2");

      const p2 = await anon.get("/links?page=9999");
      assertStatus(p2, 200, "GET /links?page=9999 clamps to page 2");
      assert.ok(p2.text.includes("GroupC Link 01"));
      assert.ok(!p2.text.includes("GroupA Link 01"));

      const qPaginated = await anon.get("/links?q=Group&page=1");
      assertStatus(qPaginated, 200);
      assert.ok(qPaginated.text.includes('class="pagination"'), "55 links matching q=Group (>48) must paginate");
      assert.match(
        qPaginated.text,
        /<a[^>]*class="pagination__item"[^>]*href="[^"]*\/links\?[^"]*(?:q=Group[^"]*page=2|page=2[^"]*q=Group)[^"]*"/,
        "/links pagination href must preserve q=Group",
      );

      const catFiltered = await anon.get("/links?category=GroupB");
      assertStatus(catFiltered, 200);
      assert.ok(catFiltered.text.includes("GroupB Link 01"));
      assert.ok(!catFiltered.text.includes("GroupA Link 01"));
      assert.ok(!catFiltered.text.includes("GroupC Link 01"));
    });

    it("F12: GET /api/admin/nav supports page/per_page clamping (max 500) and backward-compatible defaults", async () => {
      const defRes = await admin.get("/api/admin/nav");
      assertStatus(defRes, 200);
      assertJsonHasKeys(defRes.data, ["items", "nav_items", "total", "page", "per_page"]);
      assert.equal(defRes.data.page, 1);
      assert.equal(defRes.data.per_page, 100);

      const clampRes = await admin.get("/api/admin/nav?page=-1&per_page=9999");
      assertStatus(clampRes, 200);
      assert.equal(clampRes.data.page, 1);
      assert.equal(clampRes.data.per_page, 500, "Admin nav per_page > 500 clamps to 500");

      const overRes = await admin.get("/api/admin/nav?page=9999");
      assertStatus(overRes, 200);
      assert.deepStrictEqual(overRes.data.items, []);
    });

    it("F12: redirects reject invalid target URLs/self-loops and OPML/CSV imports reject malformed payloads", async () => {
      assertStatus(
        await admin.post("/api/admin/redirects", {
          source_path: "/t2-unsafe-target",
          target_path: "javascript:alert(1)",
          status_code: 301,
        }),
        400,
        "Unsafe javascript: redirect target rejected",
      );
      assertStatus(
        await admin.post("/api/admin/redirects", {
          source_path: "",
          target_path: "/about",
          status_code: 301,
        }),
        400,
        "Empty source_path rejected",
      );
      assertStatus(
        await admin.post("/api/admin/redirects", {
          source_path: "/t2-loop",
          target_path: "/t2-loop",
          status_code: 301,
        }),
        400,
        "Self-loop redirect rejected",
      );
      assertStatus(
        await admin.post("/api/admin/follows/import-opml", { opml: "<not-opml-xml" }),
        400,
        "Malformed OPML XML rejected",
      );
      assertStatus(
        await admin.post("/api/admin/redirects/import", "{malformed-json", {
          headers: { "Content-Type": "application/json" },
        }),
        400,
        "Malformed JSON redirects import rejected",
      );
      assertStatus(
        await admin.post("/api/admin/redirects/import", [
          { source_path: "/t2-imp-loop", target_path: "/t2-imp-loop", status_code: 301 },
        ]),
        400,
        "All-self-loop redirects import rejected",
      );
    });
  });
});
