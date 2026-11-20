import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertRedirect,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient, solvePoW } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

describe("F4 & F5: Public HTML Routes, Search & Syndication Feeds", () => {
  let server;
  let adminClient;
  let anonClient;
  let seeded = {};

  before(async () => {
    server = await startVoloServer({
      setupPayload: {
        site_title: "Volo Public Feeds E2E",
        site_subtitle: "Testing public HTML and feeds",
        default_locale: "en-US",
      },
    });
    adminClient = createClient(server.baseUrl);
    anonClient = createClient(server.baseUrl);

    const loginRes = await adminClient.login(
      server.adminCreds.username,
      server.adminCreds.password,
    );
    assertStatus(loginRes, 200, "Admin login");

    const feedPatch = await adminClient.patch("/api/admin/settings/feed", {
      output_mode: "full",
      item_count: 20,
      enable_raw_markdown: true,
      enable_llms_txt: true,
      podcast: {
        enabled: true,
        title: "Volo E2E Podcast",
        subtitle: "Audio feed",
        summary: "Podcast summary",
        author: "Volo Host",
        owner_name: "Volo Owner",
        owner_email: "podcast@e2e.local",
        language: "en-us",
        category: "Technology",
        explicit: false,
      },
    });
    assertStatus(feedPatch, 200, "Enable feed endpoints");

    const catRes = await adminClient.post("/api/admin/categories", {
      name: "Engineering",
      slug: "engineering",
      description: "Systems and architecture",
    });
    assertStatus(catRes, 201, "Seed category");
    seeded.category = catRes.data.category;

    const pubArtRes = await adminClient.post("/api/admin/articles", {
      title: "Public Guide",
      slug: "public-guide",
      status: "published",
      category_id: seeded.category.id,
      tags: ["go", "e2e"],
      allow_comment: true,
      audio_url: "/uploads/episode-1.mp3",
      audio_mime: "audio/mpeg",
      audio_size_bytes: 4096,
      audio_duration_sec: 180,
      summary_md: "Public summary for guide",
      content_md:
        "# Intro\n\nPublic guide body marker.\n\n:::lock pwd=\"sec\"\nHIDDEN_LOCK_BODY\n:::\n",
    });
    assertStatus(pubArtRes, 201, "Seed public article");
    seeded.publicArticle = pubArtRes.data.article;

    const secArtRes = await adminClient.post("/api/admin/articles", {
      title: "Secret Post",
      slug: "secret-post",
      status: "published",
      password: "topsecret",
      summary_md: "Secret summary",
      content_md: "TOP_SECRET_BODY_SHOULD_NOT_LEAK",
    });
    assertStatus(secArtRes, 201, "Seed password-protected article");
    seeded.secretArticle = secArtRes.data.article;

    const pageRes = await adminClient.post("/api/admin/pages", {
      title: "Colophon",
      slug: "colophon",
      is_published: true,
      content_md: "Colophon standalone page marker.",
    });
    assertStatus(pageRes, 201, "Seed standalone page");
    seeded.page = pageRes.data.page;

    const seriesRes = await adminClient.post("/api/admin/series", {
      title: "Deep Dive Series",
      slug: "deep-dive",
      description: "Series description",
      article_ids: [seeded.publicArticle.id],
    });
    assertStatus(seriesRes, 201, "Seed series");
    seeded.series = seriesRes.data.series;

    const bookRes = await adminClient.post("/api/admin/books", {
      title: "E2E Handbook",
      slug: "handbook",
      description: "Handbook description",
      source_type: "manual",
      embed_mode: "inline",
      chapters: [
        {
          title: "Chapter One",
          url: "/posts/public-guide",
          depth: 0,
        },
      ],
    });
    assertStatus(bookRes, 201, "Seed book");
    seeded.book = bookRes.data.book;

    const linkRes = await adminClient.post("/api/admin/links", {
      title: "Friend Blog",
      url: "https://example.org",
      description: "Friendly engineering site",
      category_name: "Friends",
    });
    assertStatus(linkRes, 201, "Seed link");
    seeded.link = linkRes.data.link;

    const followRes = await adminClient.post("/api/admin/follows", {
      title: "Upstream RSS",
      feed_url: "https://example.org/rss.xml",
      site_url: "https://example.org",
      description: "Upstream feed",
    });
    assertStatus(followRes, 201, "Seed follow");
    seeded.follow = followRes.data.follow;

    const powRes = await anonClient.get("/api/v1/pow/challenge");
    assertStatus(powRes, 200, "Fetch PoW challenge");
    const powSol = solvePoW(powRes.data.challenge, powRes.data.difficulty_bits);
    const cmtRes = await anonClient.post("/api/v1/comments", {
      target_type: "article",
      target_id: seeded.publicArticle.id,
      author_name: "Feed Visitor",
      content_md: "Dynamic timeline comment marker.",
      pow_challenge: powRes.data.challenge,
      pow_solution: powSol,
      website_hp: "",
      env_fp: "e2e-public-feeds",
    });
    assertStatus(cmtRes, 201, "Seed guest comment");
    seeded.comment = cmtRes.data.comment;
  });

  after(async () => {
    if (server) {
      await server.stop();
    }
  });

  describe("F4: Public HTML Routes & /api/v1/search", () => {
    it("GET / renders home page with published article and site branding", async () => {
      const fresh = createClient(server.baseUrl);
      const res = await fresh.get("/");
      assertStatus(res, 200, "GET /");
      assertHeaderIncludes(res, "content-type", "text/html; charset=utf-8", "GET /");
      assert.ok(res.text.includes("Volo Public Feeds E2E"));
      assert.ok(res.text.includes("Public Guide"));
      assert.ok(res.text.includes("/posts/public-guide"));
      assert.equal(res.text.includes("TOP_SECRET_BODY_SHOULD_NOT_LEAK"), false);
      assert.equal(res.text.includes("HIDDEN_LOCK_BODY"), false);
    });

    it("GET /posts/{slug} renders article, hides :::lock secret, and /p/{slug} redirects 301", async () => {
      const fresh = createClient(server.baseUrl);
      const postRes = await fresh.get("/posts/public-guide");
      assertStatus(postRes, 200, "GET /posts/public-guide");
      assertHeaderIncludes(postRes, "content-type", "text/html", "GET /posts/public-guide");
      assert.ok(postRes.text.includes("Public Guide"));
      assert.ok(postRes.text.includes("Public guide body marker."));
      assert.ok(postRes.text.includes("volo-lock-card"));
      assert.equal(
        postRes.text.includes("HIDDEN_LOCK_BODY"),
        false,
        "Locked block content must not leak to anonymous visitor",
      );

      const aliasRes = await fresh.get("/p/public-guide");
      assertRedirect(aliasRes, 301, "/posts/public-guide", "GET /p/public-guide");
    });

    it("Standalone pages /about and /colophon return 200 and unknown slug returns 404", async () => {
      const fresh = createClient(server.baseUrl);
      const aboutRes = await fresh.get("/about");
      assertStatus(aboutRes, 200, "GET /about");
      assertHeaderIncludes(aboutRes, "content-type", "text/html");

      const colophonRes = await fresh.get("/colophon");
      assertStatus(colophonRes, 200, "GET /colophon");
      assert.ok(colophonRes.text.includes("Colophon standalone page marker."));

      const missingRes = await fresh.get("/nonexistent-page-e2e-xyz");
      assertStatus(missingRes, 404, "GET unknown slug");
    });

    it("Taxonomy routes /categories, /categories/{slug}, /tags, and /tags/{slug} render expected content", async () => {
      const fresh = createClient(server.baseUrl);

      const catsRes = await fresh.get("/categories");
      assertStatus(catsRes, 200, "GET /categories");
      assert.ok(catsRes.text.includes('data-collection-root="categories"'));
      assert.ok(catsRes.text.includes("Engineering"));

      const catDetailRes = await fresh.get("/categories/engineering");
      assertStatus(catDetailRes, 200, "GET /categories/engineering");
      assert.ok(catDetailRes.text.includes("Public Guide"));

      const tagsRes = await fresh.get("/tags");
      assertStatus(tagsRes, 200, "GET /tags");
      assert.ok(tagsRes.text.includes("js-tag-filter-input"));
      assert.ok(tagsRes.text.includes("#go"));

      const tagDetailRes = await fresh.get("/tags/go");
      assertStatus(tagDetailRes, 200, "GET /tags/go");
      assert.ok(tagDetailRes.text.includes("Public Guide"));
    });

    it("Archive routes /archives and /archives/{YYYY-MM} render timeline and monthly posts", async () => {
      const fresh = createClient(server.baseUrl);

      const archRes = await fresh.get("/archives");
      assertStatus(archRes, 200, "GET /archives");
      assert.ok(archRes.text.includes("studio-archive-years"));
      assert.ok(archRes.text.includes("Public Guide"));

      const monthMatch = archRes.text.match(/\/archives\/(\d{4}-\d{2})/);
      assert.ok(monthMatch, "Expected /archives/YYYY-MM link in /archives page");

      const monthRes = await fresh.get(`/archives/${monthMatch[1]}`);
      assertStatus(monthRes, 200, `GET /archives/${monthMatch[1]}`);
      assert.ok(monthRes.text.includes("Public Guide"));

      const missingMonthRes = await fresh.get("/archives/1999-01");
      assertStatus(missingMonthRes, 404, "GET empty archive month");
    });

    it("Collections and community routes /series, /books, /links, /follows, and /dynamic render 200 with seeded items", async () => {
      const fresh = createClient(server.baseUrl);

      const seriesList = await fresh.get("/series");
      assertStatus(seriesList, 200, "GET /series");
      assert.ok(seriesList.text.includes("Deep Dive Series"));

      const seriesDetail = await fresh.get("/series/deep-dive");
      assertStatus(seriesDetail, 200, "GET /series/deep-dive");
      assert.ok(seriesDetail.text.includes("Public Guide"));

      const booksList = await fresh.get("/books");
      assertStatus(booksList, 200, "GET /books");
      assert.ok(booksList.text.includes("E2E Handbook"));

      const bookReader = await fresh.get("/books/handbook");
      assertStatus(bookReader, 200, "GET /books/handbook");
      assert.ok(bookReader.text.includes("Chapter One"));

      const linksRes = await fresh.get("/links");
      assertStatus(linksRes, 200, "GET /links");
      assert.ok(linksRes.text.includes('data-collection-root="links"'));
      assert.ok(linksRes.text.includes("Friend Blog"));

      const followsRes = await fresh.get("/follows");
      assertStatus(followsRes, 200, "GET /follows");
      assert.ok(followsRes.text.includes("Upstream RSS"));

      const dynRes = await fresh.get("/dynamic");
      assertStatus(dynRes, 200, "GET /dynamic");
      assert.ok(dynRes.text.includes("studio-layout--dynamic"));
      assert.ok(dynRes.text.includes("Dynamic timeline comment marker."));
    });

    it("GET /api/v1/search returns matching published hits, hides protected secrets, and respects enable_instant_search", async () => {
      const fresh = createClient(server.baseUrl);
      const searchRes = await fresh.get("/api/v1/search?q=Public&page=1&per_page=10");
      assertStatus(searchRes, 200, "GET /api/v1/search");
      assertJsonHasKeys(searchRes.data, ["hits", "total", "page", "per_page", "has_more"]);
      assert.equal(searchRes.data.page, 1);
      assert.equal(searchRes.data.per_page, 10);
      assert.ok(searchRes.data.total >= 1);
      assert.ok(
        searchRes.data.hits.some((h) => String(h.slug || h.url || "").includes("public-guide")),
        "Expected public-guide in search hits",
      );

      const secretSearch = await fresh.get("/api/v1/search?q=TOP_SECRET_BODY_SHOULD_NOT_LEAK");
      assertStatus(secretSearch, 200, "Search for protected secret body");
      assert.equal(secretSearch.data.hits.length, 0, "Protected article body must not leak in search");

      try {
        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            enable_instant_search: false,
          }),
          200,
        );
        const disabledRes = await fresh.get("/api/v1/search?q=Public");
        assertStatus(disabledRes, 404, "Search disabled returns 404");
      } finally {
        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            enable_instant_search: true,
          }),
          200,
        );
      }
    });
  });

  describe("F5: Syndication, Feeds & Machine Endpoints", () => {
    it("GET /rss.xml returns valid RSS 2.0 XML and excludes protected posts and :::lock secrets", async () => {
      const fresh = createClient(server.baseUrl);
      const res = await fresh.get("/rss.xml");
      assertStatus(res, 200, "GET /rss.xml");
      assertHeaderIncludes(res, "content-type", "application/rss+xml", "GET /rss.xml");
      assert.ok(res.text.includes('<rss version="2.0"'));
      assert.ok(res.text.includes("<channel>"));
      assert.ok(res.text.includes("<item>"));
      assert.ok(res.text.includes("<content:encoded>"));
      assert.ok(res.text.includes("Public Guide"));
      assert.equal(res.text.includes("TOP_SECRET_BODY_SHOULD_NOT_LEAK"), false);
      assert.equal(res.text.includes("HIDDEN_LOCK_BODY"), false);
    });

    it("GET /atom.xml returns valid Atom 1.0 XML with entry elements", async () => {
      const fresh = createClient(server.baseUrl);
      const res = await fresh.get("/atom.xml");
      assertStatus(res, 200, "GET /atom.xml");
      assertHeaderIncludes(res, "content-type", "application/atom+xml", "GET /atom.xml");
      assert.ok(res.text.includes('<feed xmlns="http://www.w3.org/2005/Atom">'));
      assert.ok(res.text.includes("<entry>"));
      assert.ok(res.text.includes("Public Guide"));
      assert.equal(res.text.includes("TOP_SECRET_BODY_SHOULD_NOT_LEAK"), false);
      assert.equal(res.text.includes("HIDDEN_LOCK_BODY"), false);
    });

    it("GET /feed.json returns JSON Feed 1.1 with public items and excludes protected posts", async () => {
      const fresh = createClient(server.baseUrl);
      const res = await fresh.get("/feed.json");
      assertStatus(res, 200, "GET /feed.json");
      assertHeaderIncludes(res, "content-type", "application/feed+json", "GET /feed.json");
      assert.equal(res.data?.version, "https://jsonfeed.org/version/1.1");
      assert.ok(Array.isArray(res.data?.items) && res.data.items.length >= 1);
      assert.ok(
        res.data.items.some((item) => item.title === "Public Guide"),
        "Expected Public Guide in feed.json",
      );
      assert.equal(
        res.data.items.some((item) => item.title === "Secret Post"),
        false,
        "Protected post must be excluded from feed.json",
      );
    });

    it("GET /podcast.xml returns iTunes RSS with audio enclosure and returns 404 when disabled", async () => {
      const fresh = createClient(server.baseUrl);
      const res = await fresh.get("/podcast.xml");
      assertStatus(res, 200, "GET /podcast.xml");
      assertHeaderIncludes(res, "content-type", "application/rss+xml", "GET /podcast.xml");
      assert.ok(res.text.includes("<itunes:author>Volo Host</itunes:author>"));
      assert.ok(res.text.includes("<enclosure"));
      assert.ok(res.text.includes("episode-1.mp3"));

      try {
        assertStatus(
          await adminClient.patch("/api/admin/settings/feed", {
            podcast: { enabled: false },
          }),
          200,
        );
        assertStatus(await fresh.get("/podcast.xml"), 404, "Disabled /podcast.xml");
      } finally {
        assertStatus(
          await adminClient.patch("/api/admin/settings/feed", {
            podcast: { enabled: true },
          }),
          200,
        );
      }
    });

    it("GET /sitemap.xml and GET /robots.txt expose public URLs and disallow private routes", async () => {
      const fresh = createClient(server.baseUrl);
      const smRes = await fresh.get("/sitemap.xml");
      assertStatus(smRes, 200, "GET /sitemap.xml");
      assertHeaderIncludes(smRes, "content-type", "application/xml", "GET /sitemap.xml");
      assert.ok(smRes.text.includes('<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"'));
      assert.ok(smRes.text.includes("/posts/public-guide"));
      assert.ok(smRes.text.includes("/colophon"));
      assert.equal(
        smRes.text.includes("/posts/secret-post"),
        false,
        "Protected post must not appear in sitemap.xml",
      );

      const robotsRes = await fresh.get("/robots.txt");
      assertStatus(robotsRes, 200, "GET /robots.txt");
      assertHeaderIncludes(robotsRes, "content-type", "text/plain", "GET /robots.txt");
      assert.ok(robotsRes.text.includes("User-agent: *"));
      assert.ok(robotsRes.text.includes("Disallow: /api/"));
      assert.ok(robotsRes.text.includes("Disallow: /console/"));
      assert.ok(robotsRes.text.includes(`Sitemap: ${server.baseUrl}/sitemap.xml`));
    });

    it("GET /llms.txt, /llms-full.txt, and /posts/{slug}.md strip lock blocks, block protected posts, and honor toggles", async () => {
      const fresh = createClient(server.baseUrl);

      const llmsRes = await fresh.get("/llms.txt");
      assertStatus(llmsRes, 200, "GET /llms.txt");
      assertHeaderIncludes(llmsRes, "content-type", "text/plain");
      assert.ok(llmsRes.text.includes("Public Guide"));
      assert.equal(llmsRes.text.includes("Secret Post"), false);
      assert.equal(llmsRes.text.includes("HIDDEN_LOCK_BODY"), false);

      const llmsFullRes = await fresh.get("/llms-full.txt");
      assertStatus(llmsFullRes, 200, "GET /llms-full.txt");
      assertHeaderIncludes(llmsFullRes, "content-type", "text/plain");
      assert.ok(llmsFullRes.text.includes("Public guide body marker."));
      assert.equal(llmsFullRes.text.includes("HIDDEN_LOCK_BODY"), false);

      const rawMdRes = await fresh.get("/posts/public-guide.md");
      assertStatus(rawMdRes, 200, "GET /posts/public-guide.md");
      assertHeaderIncludes(rawMdRes, "content-type", "text/markdown");
      assert.ok(rawMdRes.text.includes("Public guide body marker."));
      assert.equal(
        rawMdRes.text.includes("HIDDEN_LOCK_BODY"),
        false,
        "Raw markdown must strip :::lock blocks",
      );

      const secretMdRes = await fresh.get("/posts/secret-post.md");
      assertStatus(secretMdRes, 403, "Protected article .md returns 403");

      try {
        assertStatus(
          await adminClient.patch("/api/admin/settings/feed", {
            enable_llms_txt: false,
            enable_raw_markdown: false,
          }),
          200,
        );
        assertStatus(await fresh.get("/llms.txt"), 404, "Disabled /llms.txt");
        assertStatus(await fresh.get("/llms-full.txt"), 404, "Disabled /llms-full.txt");
        assertStatus(await fresh.get("/posts/public-guide.md"), 404, "Disabled .md endpoint");
      } finally {
        assertStatus(
          await adminClient.patch("/api/admin/settings/feed", {
            enable_llms_txt: true,
            enable_raw_markdown: true,
          }),
          200,
        );
      }
    });
  });
});
