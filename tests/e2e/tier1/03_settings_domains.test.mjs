import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  assertHeaderIncludes,
  assertJsonHasKeys,
  assertSettingPersisted,
  assertSSRFRejected,
  assertStatus,
} from "../lib/assertions.mjs";
import { createClient } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

const ALL_9_DOMAINS = [
  "site",
  "reading",
  "feed",
  "seo",
  "storage",
  "notifications",
  "security",
  "appearance",
  "backup",
];

describe("F6, F7 & F8: All 9 Settings Domains, Persistence, Validation, Optimistic Lock & SSRF", () => {
  let server;
  let adminClient;

  before(async () => {
    server = await startVoloServer({
      setupPayload: {
        site_title: "Volo Settings E2E",
        default_locale: "en-US",
      },
    });
    adminClient = createClient(server.baseUrl);
    const loginRes = await adminClient.login(
      server.adminCreds.username,
      server.adminCreds.password,
    );
    assertStatus(loginRes, 200, "Admin login");
  });

  after(async () => {
    if (server) {
      await server.stop();
    }
  });

  describe("F6: Settings Domain: site & reading", () => {
    it("GET /api/admin/settings lists all 9 domains and versions, and unknown domain returns 404", async () => {
      const allRes = await adminClient.get("/api/admin/settings");
      assertStatus(allRes, 200, "GET /api/admin/settings");
      assertJsonHasKeys(allRes.data, [
        "domains",
        "versions",
        "storage_drivers",
        "effective_upload_dir",
      ]);
      assertJsonHasKeys(allRes.data.domains, ALL_9_DOMAINS, "settings.domains");
      assertJsonHasKeys(allRes.data.versions, ALL_9_DOMAINS, "settings.versions");
      for (const dom of ALL_9_DOMAINS) {
        assert.ok(
          typeof allRes.data.versions[dom] === "number" && allRes.data.versions[dom] >= 1,
          `Expected positive numeric version for domain ${dom}`,
        );
      }

      const unknownRes = await adminClient.get("/api/admin/settings/nonexistent_domain");
      assertStatus(unknownRes, 404, "Unknown settings domain");
    });

    it("site domain persists updates, validates title and unsafe URLs, and reflects on public HTML", async () => {
      const emptyTitle = await adminClient.patch("/api/admin/settings/site", {
        title: "   ",
      });
      assertStatus(emptyTitle, 400, "Empty site.title rejected");

      const unsafeFavicon = await adminClient.patch("/api/admin/settings/site", {
        favicon_url: "javascript:alert(1)",
      });
      assertStatus(unsafeFavicon, 400, "Unsafe favicon_url rejected");

      const unsafeSocial = await adminClient.patch("/api/admin/settings/site", {
        social_links: [
          { id: "bad", label: "Evil", icon: "link", url: "javascript:alert(1)" },
        ],
      });
      assertStatus(unsafeSocial, 400, "Unsafe social_links URL rejected");

      const patchRes = await adminClient.patch("/api/admin/settings/site", {
        title: "Updated E2E Site",
        subtitle: "Refined Subtitle",
        notice_board: "<p>Maintenance Notice E2E</p>",
        footer_text: "Custom Footer E2E",
        enable_footer_html: true,
        social_links: [
          { id: "gh", label: "GitHub", icon: "github", url: "https://github.com/Willh92/Volo-go" },
        ],
        custom_vars: { badge: "e2e-ok" },
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/settings/site");

      await assertSettingPersisted(adminClient, "site", {
        title: "Updated E2E Site",
        subtitle: "Refined Subtitle",
        enable_footer_html: true,
        custom_vars: { badge: "e2e-ok" },
      });

      const homeRes = await createClient(server.baseUrl).get("/");
      assertStatus(homeRes, 200);
      assert.ok(homeRes.text.includes("Updated E2E Site"));
      assert.ok(homeRes.text.includes("Maintenance Notice E2E"));
      assert.ok(homeRes.text.includes("Custom Footer E2E"));

      const hideFooter = await adminClient.patch("/api/admin/settings/site", {
        enable_footer_html: false,
      });
      assertStatus(hideFooter, 200);
      const homeNoFooter = await createClient(server.baseUrl).get("/");
      assertStatus(homeNoFooter, 200);
      assert.equal(homeNoFooter.text.includes("Custom Footer E2E"), false);
    });

    it("reading domain persists configuration, clamps out-of-range posts_per_page, and rejects unsafe CDN prefixes", async () => {
      const badCdn = await adminClient.patch("/api/admin/settings/reading", {
        asset_cdn_prefix: "javascript:evil()",
      });
      assertStatus(badCdn, 400, "Unsafe asset_cdn_prefix rejected");

      const patchRes = await adminClient.patch("/api/admin/settings/reading", {
        list_style: "title_only",
        posts_per_page: 7,
        show_code_line_numbers: true,
        allow_reader_register: true,
        comment_access_mode: "registered",
        pow_difficulty_bits: 12,
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/settings/reading");

      await assertSettingPersisted(adminClient, "reading", {
        list_style: "title_only",
        posts_per_page: 7,
        show_code_line_numbers: true,
        allow_reader_register: true,
        comment_access_mode: "registered",
        pow_difficulty_bits: 12,
      });

      const outOfRange = await adminClient.patch("/api/admin/settings/reading", {
        posts_per_page: 999,
      });
      assertStatus(outOfRange, 200, "Out-of-range posts_per_page clamps safely");
      assert.equal(outOfRange.data.value?.posts_per_page, 15);

      await adminClient.patch("/api/admin/settings/reading", {
        list_style: "summary",
        comment_access_mode: "all",
      });
    });

    it("reading.posts_per_page and reading.enable_instant_search control live pagination and /api/v1/search", async () => {
      const createdIds = [];
      try {
        for (let i = 1; i <= 3; i += 1) {
          const res = await adminClient.post("/api/admin/articles", {
            title: `Pagination Probe Post ${i}`,
            slug: `pagination-probe-${i}`,
            status: "published",
            content_md: `Body for pagination probe ${i} searchmarker.`,
          });
          assertStatus(res, 201, `Seed pagination post ${i}`);
          createdIds.push(res.data.article.id);
        }

        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            posts_per_page: 2,
            enable_instant_search: true,
          }),
          200,
        );

        const anon = createClient(server.baseUrl);
        const extractFeedSlugs = (html) => {
          const feedSection = html.split('<aside class="studio-sidebar">')[0] || html;
          return new Set(feedSection.match(/\/posts\/pagination-probe-\d+/g) || []);
        };

        const page1 = await anon.get("/?page=1");
        assertStatus(page1, 200, "Home page 1");
        const page1Cards = (page1.text.match(/<article\s+class="article__item\b/g) || []).length;
        assert.equal(page1Cards, 2, "Expected 2 article cards on page 1 when posts_per_page=2");
        const page1Slugs = extractFeedSlugs(page1.text);
        assert.equal(page1Slugs.size, 2, "Expected 2 distinct seeded post slugs on page 1");

        const page2 = await anon.get("/?page=2");
        assertStatus(page2, 200, "Home page 2");
        const page2Cards = (page2.text.match(/<article\s+class="article__item\b/g) || []).length;
        assert.equal(page2Cards, 1, "Expected 1 article card on page 2 when posts_per_page=2");
        const page2Slugs = extractFeedSlugs(page2.text);
        assert.equal(page2Slugs.size, 1, "Expected 1 seeded post slug on page 2");

        const allSlugs = new Set([...page1Slugs, ...page2Slugs]);
        assert.equal(allSlugs.size, 3, "Page 1 and Page 2 should partition all 3 seeded posts");

        const searchOn = await anon.get("/api/v1/search?q=searchmarker");
        assertStatus(searchOn, 200, "Search enabled");
        assert.ok(searchOn.data.total >= 3);

        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            enable_instant_search: false,
          }),
          200,
        );
        assertStatus(
          await anon.get("/api/v1/search?q=searchmarker"),
          404,
          "Search disabled returns 404",
        );
      } finally {
        await adminClient.patch("/api/admin/settings/reading", {
          posts_per_page: 15,
          enable_instant_search: true,
        });
        if (createdIds.length > 0) {
          await adminClient.post("/api/admin/articles/batch", {
            action: "delete",
            ids: createdIds,
          });
        }
      }
    });

    it("reading.show_code_line_numbers toggles Chroma line number spans in rendered HTML", async () => {
      try {
        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            show_code_line_numbers: true,
          }),
          200,
        );
        const withLn = await adminClient.post("/api/admin/render/preview", {
          content_md: "```go\npackage main\nfunc main() {}\n```",
        });
        assertStatus(withLn, 200, "Render preview with line numbers");
        assert.ok(withLn.data.html.includes('<span class="ln">'));

        assertStatus(
          await adminClient.patch("/api/admin/settings/reading", {
            show_code_line_numbers: false,
          }),
          200,
        );
        const withoutLn = await adminClient.post("/api/admin/render/preview", {
          content_md: "```go\npackage main\nfunc main() {}\n```",
        });
        assertStatus(withoutLn, 200, "Render preview without line numbers");
        assert.equal(withoutLn.data.html.includes('<span class="ln">'), false);
      } finally {
        await adminClient.patch("/api/admin/settings/reading", {
          show_code_line_numbers: true,
        });
      }
    });
  });

  describe("F7: Settings Domain: feed, seo & appearance", () => {
    it("feed domain persists output_mode/item_count and controls /llms.txt, .md, and /podcast.xml", async () => {
      const artRes = await adminClient.post("/api/admin/articles", {
        title: "Feed Toggle Article",
        slug: "feed-toggle-article",
        status: "published",
        content_md: "Feed toggle body content.",
      });
      assertStatus(artRes, 201);
      const artId = artRes.data.article.id;

      try {
        const offRes = await adminClient.patch("/api/admin/settings/feed", {
          output_mode: "summary",
          item_count: 5,
          enable_llms_txt: false,
          enable_raw_markdown: false,
          podcast: { enabled: false, title: "Disabled Pod" },
        });
        assertStatus(offRes, 200);
        await assertSettingPersisted(adminClient, "feed", {
          output_mode: "summary",
          item_count: 5,
          enable_llms_txt: false,
          enable_raw_markdown: false,
        });

        const anon = createClient(server.baseUrl);
        assertStatus(await anon.get("/llms.txt"), 404);
        assertStatus(await anon.get("/posts/feed-toggle-article.md"), 404);
        assertStatus(await anon.get("/podcast.xml"), 404);

        const onRes = await adminClient.patch("/api/admin/settings/feed", {
          output_mode: "full",
          enable_llms_txt: true,
          enable_raw_markdown: true,
          podcast: { enabled: true, title: "Enabled Pod", author: "Host" },
        });
        assertStatus(onRes, 200);

        const llmsOn = await anon.get("/llms.txt");
        assertStatus(llmsOn, 200);
        assertHeaderIncludes(llmsOn, "content-type", "text/plain");
        const mdOn = await anon.get("/posts/feed-toggle-article.md");
        assertStatus(mdOn, 200);
        assertHeaderIncludes(mdOn, "content-type", "text/markdown");
        const podOn = await anon.get("/podcast.xml");
        assertStatus(podOn, 200);
        assertHeaderIncludes(podOn, "content-type", "application/rss+xml");
      } finally {
        await adminClient.del(`/api/admin/articles/${artId}`);
      }
    });

    it("seo domain validates permalink style and indexnow_key, serves /{key}.txt, and renders meta tags", async () => {
      const badPermalink = await adminClient.patch("/api/admin/settings/seo", {
        post_permalink_style: "/invalid/{slug}",
      });
      assertStatus(badPermalink, 400, "Invalid post_permalink_style rejected");

      const badKey = await adminClient.patch("/api/admin/settings/seo", {
        indexnow_key: "short",
      });
      assertStatus(badKey, 400, "Short indexnow_key rejected");

      try {
        const okRes = await adminClient.patch("/api/admin/settings/seo", {
          meta_keywords: "volo,go,cms",
          meta_description: "E2E SEO description marker",
          post_permalink_style: "/p/{slug}",
          indexnow_enabled: true,
          indexnow_key: "volo-e2e-indexnow-key-1234",
        });
        assertStatus(okRes, 200, "PATCH /api/admin/settings/seo");

        await assertSettingPersisted(adminClient, "seo", {
          meta_keywords: "volo,go,cms",
          meta_description: "E2E SEO description marker",
          post_permalink_style: "/p/{slug}",
          indexnow_enabled: true,
          indexnow_key: "volo-e2e-indexnow-key-1234",
        });

        const anon = createClient(server.baseUrl);
        const keyTxt = await anon.get("/volo-e2e-indexnow-key-1234.txt");
        assertStatus(keyTxt, 200, "GET /{indexnow_key}.txt");
        assert.equal(keyTxt.text.trim(), "volo-e2e-indexnow-key-1234");

        const homeRes = await anon.get("/");
        assertStatus(homeRes, 200);
        assert.ok(homeRes.text.includes("volo,go,cms"));
        assert.ok(homeRes.text.includes("E2E SEO description marker"));
      } finally {
        await adminClient.patch("/api/admin/settings/seo", {
          post_permalink_style: "/posts/{slug}",
          indexnow_enabled: false,
        });
      }
    });

    it("appearance domain validates accents and modes, persists updates, and reflects HTML data attributes", async () => {
      assertStatus(
        await adminClient.patch("/api/admin/settings/appearance", { accent: "neon" }),
        400,
        "Invalid accent rejected",
      );
      assertStatus(
        await adminClient.patch("/api/admin/settings/appearance", { default_mode: "sepia" }),
        400,
        "Invalid default_mode rejected",
      );

      try {
        const res = await adminClient.patch("/api/admin/settings/appearance", {
          accent: "plum",
          default_mode: "dark",
        });
        assertStatus(res, 200);
        await assertSettingPersisted(adminClient, "appearance", {
          accent: "plum",
          default_mode: "dark",
        });

        const homeRes = await createClient(server.baseUrl).get("/");
        assertStatus(homeRes, 200);
        assert.ok(homeRes.text.includes('data-accent="plum"'));
        assert.ok(homeRes.text.includes('data-default-mode="dark"'));
      } finally {
        await adminClient.patch("/api/admin/settings/appearance", {
          accent: "forest",
          default_mode: "auto",
        });
      }
    });

    it("appearance.active_theme switches between volo-studio and volo-journal cleanly", async () => {
      try {
        const switchJournal = await adminClient.patch("/api/admin/settings/appearance", {
          active_theme: "volo-journal",
        });
        assertStatus(switchJournal, 200, "Switch active_theme to volo-journal");
        assert.equal(switchJournal.data.value?.active_theme, "volo-journal");

        const journalHome = await createClient(server.baseUrl).get("/");
        assertStatus(journalHome, 200);
        assert.ok(journalHome.text.includes('data-volo-skin="volo-journal"'));
        assert.ok(journalHome.text.includes("/themes/volo-journal/assets/volo-journal.css"));

        const switchStudio = await adminClient.patch("/api/admin/settings/appearance", {
          active_theme: "volo-studio",
        });
        assertStatus(switchStudio, 200, "Switch active_theme back to volo-studio");

        const studioHome = await createClient(server.baseUrl).get("/");
        assertStatus(studioHome, 200);
        assert.ok(studioHome.text.includes('data-volo-skin="studio"'));
        assert.ok(studioHome.text.includes("/themes/volo-studio/assets/volo-studio.css"));
      } finally {
        await adminClient.patch("/api/admin/settings/appearance", {
          active_theme: "volo-studio",
        });
      }
    });

    it("appearance.custom_css sanitizes dangerous CSS vectors and renders #volo-custom-css on public pages", async () => {
      try {
        const res = await adminClient.patch("/api/admin/settings/appearance", {
          custom_css:
            "body { --e2e-css: 1; } /* </style><script>alert(1)</script> */ background: url(javascript:alert(1)); width: expression(1);",
        });
        assertStatus(res, 200, "PATCH appearance.custom_css");
        const storedCss = res.data.value?.custom_css || "";
        assert.ok(storedCss.includes("--e2e-css: 1"));
        assert.equal(/javascript\s*:/i.test(storedCss), false);
        assert.equal(/expression\s*\(/i.test(storedCss), false);
        assert.equal(storedCss.includes("</style>"), false);
        assert.ok(storedCss.includes("<\\/style>"));

        const homeRes = await createClient(server.baseUrl).get("/");
        assertStatus(homeRes, 200);
        const styleMatch = homeRes.text.match(
          /<style\s+id=["']volo-custom-css["'][^>]*>([\s\S]*?)<\/style>/i,
        );
        assert.ok(styleMatch, "Expected <style id=\"volo-custom-css\"> in HTML");
        assert.ok(styleMatch[1].includes("--e2e-css: 1"));
        assert.equal(/javascript\s*:/i.test(styleMatch[1]), false);
        assert.equal(/expression\s*\(/i.test(styleMatch[1]), false);
        assert.equal(styleMatch[1].includes("</style>"), false);
      } finally {
        await adminClient.patch("/api/admin/settings/appearance", {
          custom_css: "",
        });
      }
    });

    it("site.custom_head_meta sanitizes dangerous tags while preserving safe meta tags in <head>", async () => {
      try {
        const res = await adminClient.patch("/api/admin/settings/site", {
          custom_head_meta:
            '<meta name="e2e-verify" content="safe-1"><svg onload="alert(1)"></svg><base href="https://evil.example">',
        });
        assertStatus(res, 200, "PATCH site.custom_head_meta");
        const storedMeta = res.data.value?.custom_head_meta || "";
        assert.ok(storedMeta.includes('<meta name="e2e-verify" content="safe-1">'));
        assert.equal(storedMeta.includes("<svg"), false);
        assert.equal(/<base\b/i.test(storedMeta), false);
        assert.equal(/evil\.example/i.test(storedMeta), false);
        assert.equal(/onload\s*=/i.test(storedMeta), false);

        const homeRes = await createClient(server.baseUrl).get("/");
        assertStatus(homeRes, 200);
        const headMatch = homeRes.text.match(/<head[^>]*>([\s\S]*?)<\/head>/i);
        assert.ok(headMatch, "Expected <head> in public HTML");
        assert.ok(headMatch[1].includes('<meta name="e2e-verify" content="safe-1">'));
        assert.equal(/<base\b/i.test(headMatch[1]), false);
        assert.equal(/evil\.example/i.test(headMatch[1]), false);
        assert.equal(headMatch[1].includes("alert(1)"), false);
        assert.equal(/<svg\b/i.test(headMatch[1]), false);
      } finally {
        await adminClient.patch("/api/admin/settings/site", {
          custom_head_meta: "",
        });
      }
    });
  });

  describe("F8: Settings Domains: storage, notifications, security, backup + Optimistic Lock & SSRF", () => {
    it("PATCH /api/admin/settings/{domain} enforces optimistic locking via expected_version and If-Match (409 Conflict)", async () => {
      const getRes = await adminClient.get("/api/admin/settings/site");
      assertStatus(getRes, 200);
      const v1 = getRes.data.version;
      assertHeaderIncludes(getRes, "etag", `"${v1}"`);

      const patchBody = await adminClient.patch("/api/admin/settings/site", {
        expected_version: v1,
        subtitle: "Versioned Subtitle 1",
      });
      assertStatus(patchBody, 200, "PATCH with matching expected_version");
      const v2 = patchBody.data.version;
      assert.equal(v2, v1 + 1);
      assertHeaderIncludes(patchBody, "etag", `"${v2}"`);

      const staleBody = await adminClient.patch("/api/admin/settings/site", {
        expected_version: v1,
        subtitle: "Stale Body Update",
      });
      assertStatus(staleBody, 409, "PATCH with stale expected_version returns 409");

      const patchHeader = await adminClient.patch(
        "/api/admin/settings/site",
        { subtitle: "Versioned Subtitle 2" },
        { headers: { "If-Match": `"${v2}"` } },
      );
      assertStatus(patchHeader, 200, "PATCH with matching If-Match");
      assert.equal(patchHeader.data.version, v2 + 1);

      const staleHeader = await adminClient.patch(
        "/api/admin/settings/site",
        { subtitle: "Stale Header Update" },
        { headers: { "If-Match": `"${v2}"` } },
      );
      assertStatus(staleHeader, 409, "PATCH with stale If-Match returns 409");
    });

    it("storage domain manages s3_secret_key masking/clearing, local probe, and SSRF rejection", async () => {
      const patchRes = await adminClient.patch("/api/admin/settings/storage", {
        active_driver: "local",
        max_upload_mb: 128,
        image_quality: 88,
        strip_exif: true,
        s3_endpoint: "https://s3.example.com",
        s3_region: "us-east-1",
        s3_bucket: "e2e-bucket",
        s3_access_key: "AKIAE2E",
        s3_secret_key: "super-secret-s3-key",
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/settings/storage");
      assert.equal(patchRes.data.value?.s3_secret_key_set, true);
      assert.equal(patchRes.data.value?.s3_secret_key, undefined);

      await assertSettingPersisted(adminClient, "storage", {
        active_driver: "local",
        max_upload_mb: 128,
        image_quality: 88,
        s3_secret_key_set: true,
      });

      const localTest = await adminClient.post("/api/admin/settings/storage/test", {
        settings: { active_driver: "local" },
      });
      assertStatus(localTest, 200, "Local storage probe");
      assert.equal(localTest.data.ok, true);
      assert.equal(localTest.data.driver, "local");
      assert.equal(localTest.data.writable, true);

      const ssrfMeta = await adminClient.post("/api/admin/settings/storage/test", {
        settings: {
          active_driver: "s3",
          s3_endpoint: "http://169.254.169.254",
          s3_region: "us-east-1",
          s3_bucket: "e2e",
          s3_access_key: "AKIA",
        },
        s3_secret_key: "secret",
      });
      assertSSRFRejected(ssrfMeta, "S3 storage/test metadata IP");

      const ssrfLoop = await adminClient.post("/api/admin/settings/storage/test", {
        settings: {
          active_driver: "s3",
          s3_endpoint: "http://127.0.0.1:9000",
          s3_region: "us-east-1",
          s3_bucket: "e2e",
          s3_access_key: "AKIA",
        },
        s3_secret_key: "secret",
      });
      assertSSRFRejected(ssrfLoop, "S3 storage/test loopback IP");

      const clearRes = await adminClient.patch("/api/admin/settings/storage", {
        clear_secrets: ["s3_secret_key"],
      });
      assertStatus(clearRes, 200, "Clear s3_secret_key");
      assert.equal(clearRes.data.value?.s3_secret_key_set, false);
    });

    it("notifications domain manages smtp_password masking/clearing and rejects SSRF SMTP targets", async () => {
      const patchRes = await adminClient.patch("/api/admin/settings/notifications", {
        smtp_enabled: true,
        smtp_host: "smtp.example.com",
        smtp_port: 587,
        smtp_security: "starttls",
        smtp_auth: true,
        smtp_username: "mailer@e2e.local",
        smtp_from_email: "noreply@e2e.local",
        smtp_from_name: "Volo E2E",
        smtp_password: "smtp-secret-value",
        notify_admin_on_comment: true,
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/settings/notifications");
      assert.equal(patchRes.data.value?.smtp_password_set, true);
      assert.equal(patchRes.data.value?.smtp_password, undefined);

      await assertSettingPersisted(adminClient, "notifications", {
        smtp_host: "smtp.example.com",
        smtp_port: 587,
        smtp_security: "starttls",
        smtp_password_set: true,
        notify_admin_on_comment: true,
      });

      const ssrfLoop = await adminClient.post("/api/admin/settings/notifications/test-smtp", {
        to_email: "admin@e2e.local",
        settings: {
          smtp_enabled: true,
          smtp_host: "127.0.0.1",
          smtp_port: 587,
          smtp_security: "starttls",
        },
      });
      assertSSRFRejected(ssrfLoop, "test-smtp loopback IP");

      const ssrfMeta = await adminClient.post("/api/admin/settings/notifications/test-smtp", {
        to_email: "admin@e2e.local",
        settings: {
          smtp_enabled: true,
          smtp_host: "169.254.169.254",
          smtp_port: 25,
          smtp_security: "plain_internal",
        },
      });
      assertSSRFRejected(ssrfMeta, "test-smtp metadata IP");

      const clearRes = await adminClient.patch("/api/admin/settings/notifications", {
        smtp_enabled: false,
        clear_secrets: ["smtp_password"],
      });
      assertStatus(clearRes, 200, "Clear smtp_password");
      assert.equal(clearRes.data.value?.smtp_password_set, false);
    });

    it("security domain normalizes CIDRs, validates dynamic_login_path, switches mount, and rejects invalid inputs", async () => {
      assertStatus(
        await adminClient.patch("/api/admin/settings/security", {
          risk_whitelist_cidrs: ["not-an-ip"],
        }),
        400,
        "Invalid CIDR rejected",
      );

      assertStatus(
        await adminClient.patch("/api/admin/settings/security", {
          dynamic_login_path: "/api/reserved",
        }),
        400,
        "Reserved dynamic_login_path rejected",
      );

      try {
        const res = await adminClient.patch("/api/admin/settings/security", {
          risk_control_enabled: true,
          risk_strike_threshold: 7,
          risk_whitelist_cidrs: ["10.0.0.5"],
          waf_enabled: true,
          waf_max_requests: 500,
          totp_remember_days: 14,
          dynamic_login_path: "/stealth-e2e",
        });
        assertStatus(res, 200, "PATCH /api/admin/settings/security");
        assert.deepStrictEqual(res.data.value?.risk_whitelist_cidrs, ["10.0.0.5/32"]);
        assert.equal(res.data.value?.dynamic_login_path, "/stealth-e2e");

        const anon = createClient(server.baseUrl);
        assertStatus(await anon.get("/stealth-e2e"), 200, "Custom dynamic_login_path active");
        assertStatus(await anon.get("/console"), 404, "Default /console hidden when dynamic_login_path set");
      } finally {
        assertStatus(
          await adminClient.patch("/api/admin/settings/security", {
            dynamic_login_path: "",
          }),
          200,
          "Reset dynamic_login_path",
        );
      }

      assertStatus(
        await createClient(server.baseUrl).get("/console"),
        200,
        "Default /console restored after resetting dynamic_login_path",
      );
    });

    it("security domain self-lockout guard rejects geo_policy_active deny policy when current public IP would be blocked", async () => {
      const lockoutRes = await adminClient.patch(
        "/api/admin/settings/security",
        {
          geo_policy_active: true,
          geo_default_action: "deny",
          geo_unknown_action: "deny",
        },
        { headers: { "X-Real-IP": "8.8.8.8" } },
      );
      assertStatus(lockoutRes, 400, "GeoIP self-lockout policy rejected");

      const currentSec = await adminClient.get("/api/admin/settings/security");
      assertStatus(currentSec, 200);
      assert.equal(
        currentSec.data.value?.geo_policy_active,
        false,
        "Self-lockout guard must roll back geo_policy_active",
      );
    });

    it("backup domain persists settings and masks/clears dual secrets, while PAT tokens are denied settings access", async () => {
      const patchRes = await adminClient.patch("/api/admin/settings/backup", {
        local_auto_enabled: true,
        local_interval_hours: 12,
        local_keep_count: 5,
        include_media_in_auto: false,
        remote_protocol: "webdav",
        webdav_url: "https://dav.example.com/volo",
        webdav_user: "volobackup",
        webdav_dir: "/backups",
        webdav_password: "webdav-secret-value",
        s3_endpoint: "https://s3.example.com",
        s3_bucket: "backup-bucket",
        s3_secret_key: "backup-s3-secret",
      });
      assertStatus(patchRes, 200, "PATCH /api/admin/settings/backup");
      assert.equal(patchRes.data.value?.webdav_password_set, true);
      assert.equal(patchRes.data.value?.s3_secret_key_set, true);
      assert.equal(patchRes.data.value?.webdav_password, undefined);
      assert.equal(patchRes.data.value?.s3_secret_key, undefined);

      await assertSettingPersisted(adminClient, "backup", {
        local_auto_enabled: true,
        local_interval_hours: 12,
        local_keep_count: 5,
        include_media_in_auto: false,
        remote_protocol: "webdav",
        webdav_password_set: true,
        s3_secret_key_set: true,
      });

      const clearRes = await adminClient.patch("/api/admin/settings/backup", {
        local_auto_enabled: false,
        clear_secrets: ["webdav_password", "s3_secret_key"],
      });
      assertStatus(clearRes, 200, "Clear dual backup secrets");
      assert.equal(clearRes.data.value?.webdav_password_set, false);
      assert.equal(clearRes.data.value?.s3_secret_key_set, false);

      const { res: patRes, plaintextToken, token } = await adminClient.createPAT(
        "e2e-settings-denied",
        ["articles:write"],
        server.adminCreds.password,
      );
      assertStatus(patRes, 201);
      try {
        const patClient = adminClient.withBearer(plaintextToken);
        assertStatus(await patClient.get("/api/admin/settings"), 403, "PAT denied GET settings");
        assertStatus(
          await patClient.patch("/api/admin/settings/site", { title: "PAT Hack" }),
          403,
          "PAT denied PATCH settings",
        );
      } finally {
        await adminClient.del(`/api/admin/security/tokens/${token.id}`);
      }
    });
  });
});
