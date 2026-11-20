import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertRedirect,
  assertSSRFRejected,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient, solvePoW } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

const MINIMAL_PNG_BUFFER = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=",
  "base64",
);

async function submitGuestComment(guestClient, targetId, opts = {}) {
  const chRes = await guestClient.get("/api/v1/pow/challenge");
  assertStatus(chRes, 200, "GET /api/v1/pow/challenge");
  const { challenge, difficulty_bits: bits } = chRes.data;
  const powSolution = solvePoW(challenge, bits);

  const payload = {
    target_type: "article",
    target_id: targetId,
    author_name: opts.author_name || "E2E Guest",
    author_email: opts.author_email || "guest@e2e.local",
    content_md: opts.content_md || "Hello from E2E guest",
    pow_challenge: challenge,
    pow_solution: powSolution,
    website_hp: opts.website_hp ?? "",
    env_fp: opts.env_fp || "e2e-fp-04",
  };

  return guestClient.post("/api/v1/comments", payload);
}

describe("Tier 1 — Content CRUD (F9, F10, F11, F12)", () => {
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

  describe("F9: Articles & Standalone Pages CRUD", () => {
    it("creates draft article, fetches detail, lists by status, publishes via PUT, and serves /posts/{slug}", async () => {
      const createRes = await admin.post("/api/admin/articles", {
        title: "E2E CRUD Article",
        slug: "e2e-crud-article",
        summary_md: "Summary text",
        content_md: "# Article Heading\n\nInitial draft body.",
        status: "draft",
        allow_comment: true,
        tags: ["alpha", "e2e-crud"],
      });
      assertStatus(createRes, 201, "POST /api/admin/articles");
      assertJsonHasKeys(createRes.data, ["article", "is_password_protected", "has_password"]);
      const artId = createRes.data.article.id;
      assert.ok(artId);

      const getRes = await admin.get(`/api/admin/articles/${artId}`);
      assertStatus(getRes, 200, "GET /api/admin/articles/{id}");
      assert.equal(getRes.data.article.title, "E2E CRUD Article");
      assert.equal(getRes.data.article.status, "draft");
      assert.ok(Array.isArray(getRes.data.tags) && getRes.data.tags.includes("alpha"));

      const listRes = await admin.get("/api/admin/articles?status=draft");
      assertStatus(listRes, 200, "GET /api/admin/articles?status=draft");
      assert.ok(listRes.data.items.some((item) => item.article?.id === artId));

      const anon = createClient(srv.baseUrl);
      const draftPublic = await anon.get("/posts/e2e-crud-article");
      assertStatus(draftPublic, 404, "Draft article hidden from public");

      const updateRes = await admin.put(`/api/admin/articles/${artId}`, {
        title: "E2E CRUD Article Published",
        slug: "e2e-crud-article",
        summary_md: "Updated summary",
        content_md: "# Article Heading\n\nPublished body content.",
        status: "published",
        allow_comment: true,
        tags: ["alpha", "beta"],
      });
      assertStatus(updateRes, 200, "PUT /api/admin/articles/{id}");
      assert.equal(updateRes.data.article.status, "published");

      const pubRes = await anon.get("/posts/e2e-crud-article");
      assertStatus(pubRes, 200, "Published article accessible");
      assert.ok(pubRes.text.includes("E2E CRUD Article Published"));
      assert.ok(pubRes.text.includes("Published body content."));

      const delRes = await admin.del(`/api/admin/articles/${artId}`);
      assertStatus(delRes, 200, "DELETE /api/admin/articles/{id}");
    });

    it("pins and unpins an article via POST /api/admin/articles/{id}/pin", async () => {
      const createRes = await admin.post("/api/admin/articles", {
        title: "Pinnable Post",
        slug: "pinnable-post",
        content_md: "Pin test body",
        status: "published",
      });
      assertStatus(createRes, 201);
      const artId = createRes.data.article.id;
      assert.equal(createRes.data.article.is_pinned, false);

      const pin1 = await admin.post(`/api/admin/articles/${artId}/pin`);
      assertStatus(pin1, 200, "Pin article");
      assert.equal(pin1.data.ok, true);
      assert.equal(pin1.data.is_pinned, true);

      const pin2 = await admin.post(`/api/admin/articles/${artId}/pin`);
      assertStatus(pin2, 200, "Unpin article");
      assert.equal(pin2.data.is_pinned, false);

      await admin.del(`/api/admin/articles/${artId}`);
    });

    it("generates share-token for password-protected article and enables anonymous preview access", async () => {
      const createRes = await admin.post("/api/admin/articles", {
        title: "Protected Share Target",
        slug: "protected-share-target",
        content_md: "Secret protected preview body",
        status: "published",
        password: "share-pwd-123",
      });
      assertStatus(createRes, 201);
      const artId = createRes.data.article.id;

      const anon = createClient(srv.baseUrl);
      const lockedView = await anon.get("/posts/protected-share-target");
      assertStatus(lockedView, 200);
      assert.ok(!lockedView.text.includes("Secret protected preview body"));

      const shareRes = await admin.post(`/api/admin/articles/${artId}/share-token`, {
        ttl_hours: 24,
      });
      assertStatus(shareRes, 200, "POST share-token");
      assertJsonHasKeys(shareRes.data, ["token", "expires_at", "share_url", "ttl_hours"]);
      assert.equal(shareRes.data.ttl_hours, 24);

      const previewRes = await anon.get(
        `/posts/protected-share-target?preview=${encodeURIComponent(shareRes.data.token)}`,
      );
      assertStatus(previewRes, 200, "Preview protected post with valid share token");
      assert.ok(previewRes.text.includes("Secret protected preview body"));

      await admin.del(`/api/admin/articles/${artId}`);
    });

    it("records revisions on content updates and restores a prior revision", async () => {
      const createRes = await admin.post("/api/admin/articles", {
        title: "Revisioned Post",
        slug: "revisioned-post",
        content_md: "VERSION_ONE_BODY",
        status: "published",
        revision_note: "initial",
      });
      assertStatus(createRes, 201);
      const artId = createRes.data.article.id;

      const updRes = await admin.put(`/api/admin/articles/${artId}`, {
        title: "Revisioned Post",
        slug: "revisioned-post",
        content_md: "VERSION_TWO_BODY",
        status: "published",
        revision_note: "second edit",
      });
      assertStatus(updRes, 200);

      const revListRes = await admin.get(`/api/admin/articles/${artId}/revisions`);
      assertStatus(revListRes, 200, "GET revisions");
      assert.ok(Array.isArray(revListRes.data.items) && revListRes.data.items.length >= 2);

      const firstRev = revListRes.data.items.find((r) =>
        String(r.content_md || "").includes("VERSION_ONE_BODY") || r.revision_no === 1,
      ) || revListRes.data.items[revListRes.data.items.length - 1];
      assert.ok(firstRev?.id);

      const revDetailRes = await admin.get(`/api/admin/articles/${artId}/revisions/${firstRev.id}`);
      assertStatus(revDetailRes, 200, "GET revision detail");
      assert.ok(revDetailRes.data.revision.content_md.includes("VERSION_ONE_BODY"));

      const restoreRes = await admin.post(
        `/api/admin/articles/${artId}/revisions/${firstRev.id}/restore`,
      );
      assertStatus(restoreRes, 200, "POST revision restore");
      assert.ok(restoreRes.data.article.content_md.includes("VERSION_ONE_BODY"));

      await admin.del(`/api/admin/articles/${artId}`);
    });

    it("performs batch publish/draft/delete actions and imports Markdown via multipart form", async () => {
      const a1 = await admin.post("/api/admin/articles", {
        title: "Batch One",
        slug: "batch-one",
        content_md: "Batch 1",
        status: "draft",
      });
      const a2 = await admin.post("/api/admin/articles", {
        title: "Batch Two",
        slug: "batch-two",
        content_md: "Batch 2",
        status: "draft",
      });
      assertStatus(a1, 201);
      assertStatus(a2, 201);
      const ids = [a1.data.article.id, a2.data.article.id];

      const batchPub = await admin.post("/api/admin/articles/batch", {
        action: "publish",
        ids,
      });
      assertStatus(batchPub, 200, "Batch publish");

      const anon = createClient(srv.baseUrl);
      assertStatus(await anon.get("/posts/batch-one"), 200);
      assertStatus(await anon.get("/posts/batch-two"), 200);

      const batchDel = await admin.post("/api/admin/articles/batch", {
        action: "delete",
        ids,
      });
      assertStatus(batchDel, 200, "Batch delete");
      assertStatus(await anon.get("/posts/batch-one"), 404);

      const mdContent = [
        "---",
        "title: Imported Markdown Post",
        "slug: imported-markdown-post",
        "status: published",
        "tags:",
        "  - imported",
        "---",
        "",
        "Hello imported markdown body.",
      ].join("\n");
      const form = new FormData();
      form.append("file", new Blob([mdContent], { type: "text/markdown" }), "imported.md");

      const importRes = await admin.post("/api/admin/articles/import", form);
      assertStatus(importRes, 200, "POST /api/admin/articles/import");
      assert.equal(importRes.data.imported_count, 1);
      assert.ok(importRes.data.slugs.includes("imported-markdown-post"));

      const importedPage = await anon.get("/posts/imported-markdown-post");
      assertStatus(importedPage, 200);
      assert.ok(importedPage.text.includes("Hello imported markdown body."));

      const listImported = await admin.get("/api/admin/articles?q=Imported");
      assertStatus(listImported, 200, "GET /api/admin/articles?q=Imported");
      const matched = listImported.data.items.find(
        (x) => x.article?.slug === "imported-markdown-post",
      );
      assert.ok(matched?.article?.id, "Expected imported-markdown-post in admin article list");
      const delImportedRes = await admin.del(`/api/admin/articles/${matched.article.id}`);
      assertStatus(delImportedRes, 200, "DELETE imported-markdown-post");
    });

    it("creates, updates, publicly serves, and deletes a standalone page", async () => {
      const createRes = await admin.post("/api/admin/pages", {
        title: "E2E Standalone",
        slug: "e2e-standalone",
        content_md: "## Standalone Section\n\nStandalone body text.",
        is_published: true,
        show_toc: true,
      });
      assertStatus(createRes, 201, "POST /api/admin/pages");
      const pageId = createRes.data.page.id;

      const anon = createClient(srv.baseUrl);
      const pub1 = await anon.get("/e2e-standalone");
      assertStatus(pub1, 200, "GET /{slug} standalone page");
      assert.ok(pub1.text.includes("Standalone body text."));

      const updRes = await admin.put(`/api/admin/pages/${pageId}`, {
        title: "E2E Standalone Updated",
        slug: "e2e-standalone",
        content_md: "Updated standalone body.",
        is_published: true,
      });
      assertStatus(updRes, 200, "PUT /api/admin/pages/{id}");
      assert.equal(updRes.data.page.title, "E2E Standalone Updated");

      const pub2 = await anon.get("/e2e-standalone");
      assertStatus(pub2, 200);
      assert.ok(pub2.text.includes("Updated standalone body."));

      const delRes = await admin.del(`/api/admin/pages/${pageId}`);
      assertStatus(delRes, 200, "DELETE /api/admin/pages/{id}");
      assertStatus(await anon.get("/e2e-standalone"), 404);
    });
  });

  describe("F10: Categories, Tags, Series & Books CRUD", () => {
    it("creates, updates, reorders, and deletes categories", async () => {
      const c1 = await admin.post("/api/admin/categories", {
        name: "Cat Alpha",
        slug: "cat-alpha",
        description: "First category",
        sort_order: 1,
      });
      const c2 = await admin.post("/api/admin/categories", {
        name: "Cat Beta",
        slug: "cat-beta",
        description: "Second category",
        sort_order: 2,
      });
      assertStatus(c1, 201);
      assertStatus(c2, 201);
      const id1 = c1.data.category.id;
      const id2 = c2.data.category.id;

      const upd = await admin.put(`/api/admin/categories/${id1}`, {
        name: "Cat Alpha Updated",
        slug: "cat-alpha",
        description: "Updated description",
      });
      assertStatus(upd, 200);
      assert.equal(upd.data.ok, true);

      const reord = await admin.post("/api/admin/categories/reorder", {
        ordered_ids: [id2, id1],
      });
      assertStatus(reord, 200, "POST categories/reorder");
      assert.equal(reord.data.ok, true);

      const listRes = await admin.get("/api/admin/categories");
      assertStatus(listRes, 200);
      const updatedCat = listRes.data.items.find((c) => c.id === id1);
      assert.equal(updatedCat?.description, "Updated description");
      const idx2 = listRes.data.items.findIndex((c) => c.id === id2);
      const idx1 = listRes.data.items.findIndex((c) => c.id === id1);
      assert.ok(idx2 >= 0 && idx1 >= 0 && idx2 < idx1, "Reordered category should appear first");

      assertStatus(await admin.del(`/api/admin/categories/${id1}`), 200);
      assertStatus(await admin.del(`/api/admin/categories/${id2}`), 200);
    });

    it("creates tags, merges source tag into target tag, and cleans up unused tags", async () => {
      const tSrc = await admin.post("/api/admin/tags", { name: "merge-src", slug: "merge-src" });
      const tDst = await admin.post("/api/admin/tags", { name: "merge-dst", slug: "merge-dst" });
      const tUnused = await admin.post("/api/admin/tags", { name: "unused-e2e", slug: "unused-e2e" });
      assertStatus(tSrc, 201);
      assertStatus(tDst, 201);
      assertStatus(tUnused, 201);

      const artRes = await admin.post("/api/admin/articles", {
        title: "Tagged Article",
        slug: "tagged-article-f10",
        content_md: "Tag body",
        status: "published",
        tags: ["merge-src"],
      });
      assertStatus(artRes, 201);
      const artId = artRes.data.article.id;

      const mergeRes = await admin.post("/api/admin/tags/merge", {
        source_ids: [tSrc.data.tag.id],
        target_id: tDst.data.tag.id,
      });
      assertStatus(mergeRes, 200, "POST /api/admin/tags/merge");
      assert.equal(mergeRes.data.ok, true);
      assert.equal(mergeRes.data.merged, 1);
      assert.equal(mergeRes.data.rebound_articles, 1);

      const artCheck = await admin.get(`/api/admin/articles/${artId}`);
      assert.ok(artCheck.data.tags.includes("merge-dst"));
      assert.ok(!artCheck.data.tags.includes("merge-src"));

      const cleanupRes = await admin.post("/api/admin/tags/cleanup");
      assertStatus(cleanupRes, 200, "POST /api/admin/tags/cleanup");
      assert.ok(cleanupRes.data.deleted >= 1);

      await admin.del(`/api/admin/articles/${artId}`);
      await admin.post("/api/admin/tags/cleanup");
    });

    it("creates, updates, publicly renders on /series/{slug}, and deletes a series", async () => {
      const a1 = await admin.post("/api/admin/articles", {
        title: "Series Ep 1",
        slug: "series-ep-1",
        content_md: "Episode 1",
        status: "published",
      });
      assertStatus(a1, 201);
      const artId = a1.data.article.id;

      const sRes = await admin.post("/api/admin/series", {
        title: "Architecture Series",
        slug: "arch-series",
        description: "Deep dive series",
        article_ids: [artId],
      });
      assertStatus(sRes, 201, "POST /api/admin/series");
      const seriesId = sRes.data.series.id;

      const listRes = await admin.get("/api/admin/series");
      assertStatus(listRes, 200);
      const found = listRes.data.items.find((s) => s.id === seriesId);
      assert.ok(found);
      assert.equal(found.articles.length, 1);

      const updRes = await admin.put(`/api/admin/series/${seriesId}`, {
        title: "Architecture Series V2",
        slug: "arch-series",
        description: "Updated series desc",
        article_ids: [artId],
      });
      assertStatus(updRes, 200);

      const anon = createClient(srv.baseUrl);
      const pubRes = await anon.get("/series/arch-series");
      assertStatus(pubRes, 200, "GET /series/{slug}");
      assert.ok(pubRes.text.includes("Architecture Series V2"));
      assert.ok(pubRes.text.includes("Series Ep 1"));

      assertStatus(await admin.del(`/api/admin/series/${seriesId}`), 200);
      await admin.del(`/api/admin/articles/${artId}`);
    });

    it("creates manual book with nested chapters, fetches detail, and renders /books/{slug} reader", async () => {
      const chPost = await admin.post("/api/admin/articles", {
        title: "Book Chapter One Source",
        slug: "book-ch1-src",
        content_md: "Chapter one inline body text.",
        status: "published",
      });
      assertStatus(chPost, 201);

      const bookRes = await admin.post("/api/admin/books", {
        title: "Manual Engineering Book",
        slug: "manual-eng-book",
        description: "Manual book description",
        source_type: "manual",
        embed_mode: "inline",
        reading_status: "reading",
        chapters: [
          { title: "Part I", url: "", depth: 0 },
          { title: "Chapter 1", url: "/posts/book-ch1-src", depth: 1 },
        ],
      });
      assertStatus(bookRes, 201, "POST /api/admin/books");
      const bookId = bookRes.data.book.id;

      const detailRes = await admin.get(`/api/admin/books/${bookId}`);
      assertStatus(detailRes, 200, "GET /api/admin/books/{id}");
      assert.equal(detailRes.data.chapters.length, 2);
      const ch1 = detailRes.data.chapters.find((c) => c.title === "Chapter 1");
      assert.ok(ch1?.id);

      const anon = createClient(srv.baseUrl);
      const readerRes = await anon.get(`/books/manual-eng-book?ch=${encodeURIComponent(ch1.id)}`);
      assertStatus(readerRes, 200, "GET /books/{slug}?ch=...");
      assert.ok(readerRes.text.includes("Manual Engineering Book"));
      assert.ok(readerRes.text.includes("Chapter 1"));

      assertStatus(await admin.del(`/api/admin/books/${bookId}`), 200);
      await admin.del(`/api/admin/articles/${chPost.data.article.id}`);
    });

    it("creates series-backed book and syncs chapters offline via POST /api/admin/books/{id}/sync", async () => {
      const p1 = await admin.post("/api/admin/articles", {
        title: "Synced Chapter 1",
        slug: "synced-ch-1",
        content_md: "Sync body 1",
        status: "published",
      });
      const p2 = await admin.post("/api/admin/articles", {
        title: "Synced Chapter 2",
        slug: "synced-ch-2",
        content_md: "Sync body 2",
        status: "published",
      });
      assertStatus(p1, 201);
      assertStatus(p2, 201);

      const sRes = await admin.post("/api/admin/series", {
        title: "Book Sync Series",
        slug: "book-sync-series",
        article_ids: [p1.data.article.id, p2.data.article.id],
      });
      assertStatus(sRes, 201);
      const seriesId = sRes.data.series.id;

      const bRes = await admin.post("/api/admin/books", {
        title: "Synced Book",
        slug: "synced-book",
        source_type: "series",
        series_id: seriesId,
        embed_mode: "inline",
      });
      assertStatus(bRes, 201);
      const bookId = bRes.data.book.id;

      const syncRes = await admin.post(`/api/admin/books/${bookId}/sync`);
      assertStatus(syncRes, 200, "POST /api/admin/books/{id}/sync");
      assert.equal(syncRes.data.chapter_count, 2);

      const detailRes = await admin.get(`/api/admin/books/${bookId}`);
      assertStatus(detailRes, 200);
      assert.equal(detailRes.data.chapters.length, 2);

      await admin.del(`/api/admin/books/${bookId}`);
      await admin.del(`/api/admin/series/${seriesId}`);
      await admin.del(`/api/admin/articles/${p1.data.article.id}`);
      await admin.del(`/api/admin/articles/${p2.data.article.id}`);
    });

    it("supports server-side category search (?q=) and pagination on admin & public /categories", async () => {
      const catA = await admin.post("/api/admin/categories", { name: "Rust Systems", slug: "rust-sys" });
      const catB = await admin.post("/api/admin/categories", { name: "Cooking Notes", slug: "cooking-notes" });
      assertStatus(catA, 201);
      assertStatus(catB, 201);

      const adminPage = await admin.get("/api/admin/categories?q=Rust&page=1&per_page=10");
      assertStatus(adminPage, 200, "GET /api/admin/categories?q=Rust");
      assertJsonHasKeys(adminPage.data, ["items", "total", "page", "per_page"]);
      assert.ok(adminPage.data.items.some((c) => c.slug === "rust-sys"));
      assert.ok(!adminPage.data.items.some((c) => c.slug === "cooking-notes"));

      const anon = createClient(srv.baseUrl);
      const pubFiltered = await anon.get("/categories?q=Rust");
      assertStatus(pubFiltered, 200, "GET /categories?q=Rust");
      const rootIdx = pubFiltered.text.indexOf('data-collection-root="categories"');
      assert.ok(rootIdx >= 0, "Expected data-collection-root=categories");
      const gridStart = pubFiltered.text.indexOf('<div class="bookshelf-grid">', rootIdx);
      const gridEnd = pubFiltered.text.indexOf("js-collection-empty", gridStart);
      assert.ok(gridStart >= 0 && gridEnd > gridStart, "Expected category grid bounds");
      const collectionSlice = pubFiltered.text.slice(gridStart, gridEnd);
      assert.ok(collectionSlice.includes("Rust Systems"));
      assert.ok(!collectionSlice.includes("Cooking Notes"));

      await admin.del(`/api/admin/categories/${catA.data.category.id}`);
      await admin.del(`/api/admin/categories/${catB.data.category.id}`);
    });
  });

  describe("F11: Comments, Reactions & Media CRUD", () => {
    let commentArtId;

    before(async () => {
      const art = await admin.post("/api/admin/articles", {
        title: "F11 Target Article",
        slug: "f11-target-article",
        content_md: "Comments and reactions target.",
        status: "published",
        allow_comment: true,
      });
      assertStatus(art, 201);
      commentArtId = art.data.article.id;
    });

    after(async () => {
      if (commentArtId) {
        await admin.del(`/api/admin/articles/${commentArtId}`);
      }
    });

    it("submits guest comment with PoW + env_fp and rejects PoW replay and honeypot", async () => {
      const guest = createClient(srv.baseUrl);
      const chRes = await guest.get("/api/v1/pow/challenge");
      assertStatus(chRes, 200);
      const { challenge, difficulty_bits: bits } = chRes.data;
      const sol = solvePoW(challenge, bits);

      const okRes = await guest.post("/api/v1/comments", {
        target_type: "article",
        target_id: commentArtId,
        author_name: "Alice",
        author_email: "alice@e2e.local",
        content_md: "Valid PoW comment",
        pow_challenge: challenge,
        pow_solution: sol,
        website_hp: "",
        env_fp: "fp-valid",
      });
      assertStatus(okRes, 201, "Valid PoW comment submission");
      assertJsonHasKeys(okRes.data, ["comment", "passport_token"]);
      assert.equal(okRes.data.comment.status, "approved");

      const replayRes = await guest.post("/api/v1/comments", {
        target_type: "article",
        target_id: commentArtId,
        author_name: "Alice",
        content_md: "Replay attempt",
        pow_challenge: challenge,
        pow_solution: sol,
        website_hp: "",
      });
      assertStatus(replayRes, 400, "PoW replay must be rejected");

      const ch2 = await guest.get("/api/v1/pow/challenge");
      const sol2 = solvePoW(ch2.data.challenge, ch2.data.difficulty_bits);
      const hpRes = await guest.post("/api/v1/comments", {
        target_type: "article",
        target_id: commentArtId,
        author_name: "Bot",
        content_md: "Honeypot spam",
        pow_challenge: ch2.data.challenge,
        pow_solution: sol2,
        website_hp: "https://spam.invalid",
      });
      assertStatus(hpRes, 400, "Honeypot trigger must be rejected");
    });

    it("holds pending comments under moderation=all from anonymous visitors until admin approves", async () => {
      const patchRes = await admin.patch("/api/admin/settings/reading", {
        comment_moderation_mode: "all",
      });
      assertStatus(patchRes, 200, "Set comment_moderation_mode=all");

      try {
        const guest = createClient(srv.baseUrl);
        const subRes = await submitGuestComment(guest, commentArtId, {
          author_name: "Moderated User",
          content_md: "PENDING_UNIQUE_MARKER",
        });
        assertStatus(subRes, 201);
        assert.equal(subRes.data.comment.status, "pending");
        const cid = subRes.data.comment.id;

        const freshAnon = createClient(srv.baseUrl);
        const beforeApprove = await freshAnon.get(
          `/api/v1/comments?target_type=article&target_id=${encodeURIComponent(commentArtId)}`,
        );
        assertStatus(beforeApprove, 200);
        assert.ok(
          !JSON.stringify(beforeApprove.data).includes("PENDING_UNIQUE_MARKER"),
          "Pending comment must remain hidden from fresh anonymous visitors",
        );

        const approveRes = await admin.patch(`/api/admin/comments/${cid}`, {
          status: "approved",
        });
        assertStatus(approveRes, 200, "Admin approve comment");

        const afterApprove = await freshAnon.get(
          `/api/v1/comments?target_type=article&target_id=${encodeURIComponent(commentArtId)}`,
        );
        assertStatus(afterApprove, 200);
        assert.ok(
          JSON.stringify(afterApprove.data).includes("PENDING_UNIQUE_MARKER"),
          "Approved comment must appear to fresh anonymous visitors",
        );
      } finally {
        await admin.patch("/api/admin/settings/reading", {
          comment_moderation_mode: "smart",
        });
      }
    });

    it("supports admin comment reply, soft-delete to trash, restore, and permanent purge", async () => {
      const guest = createClient(srv.baseUrl);
      const sub = await submitGuestComment(guest, commentArtId, {
        author_name: "Parent Commenter",
        content_md: "Parent comment for reply test",
      });
      assertStatus(sub, 201);
      const parentId = sub.data.comment.id;

      const replyRes = await admin.post(`/api/admin/comments/${parentId}/reply`, {
        content_md: "Official admin reply",
      });
      assertStatus(replyRes, 201, "Admin reply");
      assertJsonHasKeys(replyRes.data, ["comment", "parent_auto_approved"]);
      const replyId = replyRes.data.comment.id;

      const trashRes = await admin.del(`/api/admin/comments/${replyId}`);
      assertStatus(trashRes, 200, "Soft-delete comment");

      const trashList = await admin.get("/api/admin/comments?status=trash");
      assertStatus(trashList, 200);
      assert.ok(trashList.data.items.some((c) => c.id === replyId));

      const restoreRes = await admin.post(`/api/admin/comments/${replyId}/restore`);
      assertStatus(restoreRes, 200, "Restore comment from trash");

      assertStatus(await admin.del(`/api/admin/comments/${replyId}`), 200);
      const purgeRes = await admin.del(`/api/admin/comments/${replyId}?purge=1`);
      assertStatus(purgeRes, 200, "Purge trashed comment");
      assert.equal(purgeRes.data.ok, true);
    });

    it("toggles public article reactions via POST /api/v1/reactions", async () => {
      const visitor = createClient(srv.baseUrl, {
        headers: { "X-Volo-Env-FP": "reaction-fp-1" },
      });
      const r1 = await visitor.post("/api/v1/reactions", {
        target_type: "article",
        target_id: commentArtId,
        kind: "like",
      });
      assertStatus(r1, 200, "Toggle reaction on");
      assert.equal(r1.data.added, true);
      assert.ok((r1.data.counts?.like || 0) >= 1);

      const r2 = await visitor.post("/api/v1/reactions", {
        target_type: "article",
        target_id: commentArtId,
        kind: "like",
      });
      assertStatus(r2, 200, "Toggle reaction off");
      assert.equal(r2.data.added, false);
    });

    it("uploads valid PNG via multipart field 'file', serves /uploads/*, and updates alt_text", async () => {
      const form = new FormData();
      form.append("file", new Blob([MINIMAL_PNG_BUFFER], { type: "image/png" }), "pixel.png");

      const upRes = await admin.post("/api/admin/media/upload", form);
      assertStatus(upRes, 201, "POST /api/admin/media/upload");
      assertJsonHasKeys(upRes.data, ["asset", "url"]);
      const assetId = upRes.data.asset.id;
      const assetUrl = upRes.data.url;

      const anon = createClient(srv.baseUrl);
      const imgRes = await anon.get(assetUrl);
      assertStatus(imgRes, 200, "GET uploaded media asset");
      assertHeaderIncludes(imgRes, "content-type", "image/");

      const patchRes = await admin.patch(`/api/admin/media/${assetId}`, {
        alt_text: "E2E pixel image",
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/media/{id}");
      assert.equal(patchRes.data.asset.alt_text, "E2E pixel image");

      assertStatus(await admin.del(`/api/admin/media/${assetId}`), 200);
    });

    it("detects unreferenced media via POST /api/admin/media/orphans and deletes asset cleanly", async () => {
      const form = new FormData();
      form.append("file", new Blob([MINIMAL_PNG_BUFFER], { type: "image/png" }), "orphan.png");
      const upRes = await admin.post("/api/admin/media/upload", form);
      assertStatus(upRes, 201);
      const assetId = upRes.data.asset.id;
      const assetUrl = upRes.data.url;

      const orphanScan = await admin.post("/api/admin/media/orphans");
      assertStatus(orphanScan, 200, "POST /api/admin/media/orphans");
      assert.ok(Array.isArray(orphanScan.data.orphans));
      assert.ok(orphanScan.data.orphans.some((o) => o.id === assetId));

      const delRes = await admin.del(`/api/admin/media/${assetId}`);
      assertStatus(delRes, 200, "DELETE /api/admin/media/{id}");

      const anon = createClient(srv.baseUrl);
      assertStatus(await anon.get(assetUrl), 404, "Deleted media URL must return 404");
    });
  });

  describe("F12: Nav, Links, Follows, Redirects & Webhooks CRUD", () => {
    it("creates, lists, updates, reflects on home HTML, and deletes a navigation item", async () => {
      const createRes = await admin.post("/api/admin/nav", {
        title: "E2E Docs Nav",
        url: "/e2e-docs-nav",
        is_enabled: true,
        sort_order: 99,
      });
      assertStatus(createRes, 201, "POST /api/admin/nav");
      const navId = createRes.data.item.id;

      try {
        const listRes = await admin.get("/api/admin/nav");
        assertStatus(listRes, 200);
        assert.ok(listRes.data.items.some((n) => n.id === navId));

        const updRes = await admin.put(`/api/admin/nav/${navId}`, {
          title: "E2E Docs Updated",
          url: "/e2e-docs-nav",
          is_enabled: true,
        });
        assertStatus(updRes, 200);

        const cfgRes = await admin.put("/api/admin/themes/volo-studio/config", {
          header_nav_items: ["home", `custom:${navId}`, "archives"],
          header_nav_primary_count: 5,
        });
        assertStatus(cfgRes, 200, "Bind custom nav item in theme config");

        const anon = createClient(srv.baseUrl);
        const homeRes = await anon.get("/");
        assertStatus(homeRes, 200);
        assert.ok(homeRes.text.includes("E2E Docs Updated"));
        assert.ok(homeRes.text.includes('href="/e2e-docs-nav"'));
      } finally {
        await admin.del(`/api/admin/nav/${navId}`);
      }
    });

    it("creates, filters, updates, and batch-deletes friend links", async () => {
      const lRes = await admin.post("/api/admin/links", {
        title: "Example Friend",
        url: "https://example.com/friend",
        description: "Test friend link",
        category_name: "Tools",
      });
      assertStatus(lRes, 201, "POST /api/admin/links");
      const linkId = lRes.data.link.id;

      const listRes = await admin.get("/api/admin/links?category=Tools");
      assertStatus(listRes, 200);
      assert.ok(listRes.data.items.some((l) => l.id === linkId));

      const updRes = await admin.put(`/api/admin/links/${linkId}`, {
        title: "Example Friend V2",
        url: "https://example.com/friend",
        description: "Updated link desc",
        category_name: "Tools",
      });
      assertStatus(updRes, 200);

      const batchDel = await admin.post("/api/admin/links/batch", {
        action: "delete",
        ids: [linkId],
      });
      assertStatus(batchDel, 200, "POST /api/admin/links/batch");
      assert.equal(batchDel.data.affected, 1);
    });

    it("creates follows, exports OPML XML, imports OPML XML, and deletes follow entries", async () => {
      const fRes = await admin.post("/api/admin/follows", {
        title: "Feed Alpha",
        feed_url: "https://example.com/alpha.xml",
        site_url: "https://example.com",
      });
      assertStatus(fRes, 201, "POST /api/admin/follows");
      const feedAId = fRes.data.follow.id;

      const opmlRes = await admin.get("/api/admin/follows/export.opml");
      assertStatus(opmlRes, 200, "GET /api/admin/follows/export.opml");
      assertHeaderIncludes(opmlRes, "content-type", "application/xml");
      assert.ok(opmlRes.text.includes("<opml"));
      assert.ok(opmlRes.text.includes("Feed Alpha"));

      const opmlXml = `<?xml version="1.0" encoding="UTF-8"?>
<opml version="2.0">
  <body>
    <outline text="Feed Beta" title="Feed Beta" type="rss" xmlUrl="https://example.org/beta.xml" htmlUrl="https://example.org"/>
  </body>
</opml>`;
      const impRes = await admin.post("/api/admin/follows/import-opml", { opml: opmlXml });
      assertStatus(impRes, 200, "POST /api/admin/follows/import-opml");
      assert.equal(impRes.data.imported, 1);

      const listRes = await admin.get("/api/admin/follows");
      const feedB = listRes.data.items.find((f) => f.title === "Feed Beta");
      assert.ok(feedB?.id);

      assertStatus(await admin.del(`/api/admin/follows/${feedAId}`), 200);
      assertStatus(await admin.del(`/api/admin/follows/${feedB.id}`), 200);
    });

    it("creates 301 redirect, verifies runtime redirect, rejects self-loop, and exports/imports CSV", async () => {
      const rRes = await admin.post("/api/admin/redirects", {
        source_path: "/e2e-old-route",
        target_path: "/about",
        status_code: 301,
      });
      assertStatus(rRes, 201, "POST /api/admin/redirects");
      const redirId = rRes.data.redirect.id;

      const anon = createClient(srv.baseUrl);
      const hitRes = await anon.get("/e2e-old-route", { redirect: "manual" });
      assertRedirect(hitRes, 301, "/about", "Runtime 301 redirect");

      const loopRes = await admin.post("/api/admin/redirects", {
        source_path: "/self-loop",
        target_path: "/self-loop",
        status_code: 301,
      });
      assertStatus(loopRes, 400, "Self-loop redirect must be rejected");

      const csvExport = await admin.get("/api/admin/redirects/export?format=csv");
      assertStatus(csvExport, 200);
      assertHeaderIncludes(csvExport, "content-type", "text/csv");
      assert.ok(csvExport.text.includes("/e2e-old-route,/about,301"));

      const csvImport = await admin.post("/api/admin/redirects/import", [
        { source_path: "/e2e-csv-imported", target_path: "/about", status_code: 302 },
      ]);
      assertStatus(csvImport, 200, "POST /api/admin/redirects/import");
      assert.equal(csvImport.data.imported, 1);

      const listRes = await admin.get("/api/admin/redirects");
      const importedRow = listRes.data.items.find((r) => r.source_path === "/e2e-csv-imported");
      assertStatus(await admin.del(`/api/admin/redirects/${redirId}`), 200);
      if (importedRow) {
        await admin.del(`/api/admin/redirects/${importedRow.id}`);
      }
    });

    it("creates, updates, and deletes webhook while blocking SSRF loopback/private URLs", async () => {
      const ssrfRes = await admin.post("/api/admin/webhooks", {
        name: "Local SSRF Hook",
        provider: "custom",
        url: "http://127.0.0.1:9999/hook",
        events: ["comment.created"],
        is_enabled: true,
      });
      assertSSRFRejected(ssrfRes, "Webhook loopback URL must be rejected");

      const okRes = await admin.post("/api/admin/webhooks", {
        name: "Public Hook",
        provider: "custom",
        url: "https://example.com/e2e-webhook",
        secret: "whsec-e2e-123",
        events: ["article.published"],
        is_enabled: true,
      });
      assertStatus(okRes, 201, "POST /api/admin/webhooks");
      assert.equal(okRes.data.secret_set, true);
      const whId = okRes.data.webhook.id;

      const updRes = await admin.put(`/api/admin/webhooks/${whId}`, {
        name: "Public Hook V2",
        provider: "custom",
        url: "https://example.com/e2e-webhook-v2",
        events: ["article.published", "comment.created"],
        is_enabled: false,
      });
      assertStatus(updRes, 200);
      assert.equal(updRes.data.ok, true);

      const listWh = await admin.get("/api/admin/webhooks");
      assertStatus(listWh, 200);
      const updatedWh = listWh.data.items.find((w) => w.webhook?.id === whId);
      assert.equal(updatedWh?.webhook?.name, "Public Hook V2");

      assertStatus(await admin.del(`/api/admin/webhooks/${whId}`), 200);
    });

    it("verifies public /links category and search query filtering", async () => {
      const l1 = await admin.post("/api/admin/links", {
        title: "Cloudflare Blog",
        url: "https://blog.cloudflare.com/",
        category_name: "Infra",
      });
      const l2 = await admin.post("/api/admin/links", {
        title: "Cooking Weekly",
        url: "https://example.org/cook",
        category_name: "Lifestyle",
      });
      assertStatus(l1, 201);
      assertStatus(l2, 201);

      const anon = createClient(srv.baseUrl);
      const qRes = await anon.get("/links?q=Cloudflare");
      assertStatus(qRes, 200, "GET /links?q=Cloudflare");
      assert.ok(qRes.text.includes("Cloudflare Blog"));
      assert.ok(!qRes.text.includes("Cooking Weekly"));

      const catRes = await anon.get("/links?category=Lifestyle");
      assertStatus(catRes, 200, "GET /links?category=Lifestyle");
      assert.ok(catRes.text.includes("Cooking Weekly"));
      assert.ok(!catRes.text.includes("Cloudflare Blog"));

      await admin.del(`/api/admin/links/${l1.data.link.id}`);
      await admin.del(`/api/admin/links/${l2.data.link.id}`);
    });
  });
});
