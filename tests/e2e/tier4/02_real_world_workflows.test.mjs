import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import { assertStatus } from "../lib/assertions.mjs";
import {
  checkElementsAlignedX,
  checkNoHorizontalOverflow,
  checkNoOverlap,
  checkNotClipped,
  launchEdge,
  withPage,
} from "../lib/browser.mjs";
import { createClient } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

function pageOpts(overrides = {}) {
  return {
    width: 1280,
    height: 800,
    locale: "en-US",
    failOnConsoleError: true,
    ...overrides,
  };
}

async function readBootConfig(page) {
  const raw = await page.locator("#volo-boot-config").textContent();
  return JSON.parse(String(raw || "{}"));
}

async function loginConsoleViaUI(page, diag, baseUrl, creds, hash = "") {
  await page.goto(`${baseUrl}/console${hash}`, { waitUntil: "domcontentloaded" });
  await page.locator(".volo-auth-version").waitFor({ state: "visible", timeout: 15000 });
  await page.locator('input[name="volo_staff_ident"]').fill(creds.username);
  await page.locator('input[name="volo_staff_secret"]').fill(creds.password);
  diag.clear();
  await page
    .locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit')
    .click();
  await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
  await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
}

async function assertPluginCircuitClosed(admin, pluginId, contextMsg) {
  const res = await admin.get("/api/admin/plugins");
  assertStatus(res, 200, `GET /api/admin/plugins (${contextMsg})`);
  const items = res.data?.items || res.data?.plugins || [];
  const p = items.find((item) => item.id === pluginId);
  assert.ok(p, `${contextMsg}: expected plugin "${pluginId}" in /api/admin/plugins`);
  assert.equal(
    p.is_enabled,
    true,
    `${contextMsg}: plugin "${pluginId}" disabled (circuit_state=${p.circuit_state}, failure_count=${p.failure_count}, last_error=${p.last_error})`,
  );
  assert.equal(
    p.circuit_state,
    "closed",
    `${contextMsg}: plugin "${pluginId}" circuit_state="${p.circuit_state}" (failure_count=${p.failure_count}, last_error=${p.last_error})`,
  );
}

async function waitForAnimations(page, selector) {
  await page.locator(selector).evaluate(async (el) => {
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    if (typeof el.getAnimations === "function") {
      await Promise.all(el.getAnimations().map((a) => a.finished.catch(() => {})));
    }
  });
}

async function waitForCondition(fn, contextMsg, timeoutMs = 10000, intervalMs = 40) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await fn()) return;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  throw new Error(`Timed out after ${timeoutMs}ms waiting for: ${contextMsg}`);
}

describe("Tier 4 — Real-World Application Workflows (S1–S8)", () => {
  let srv;
  let admin;
  let browser;

  before(async () => {
    srv = await startVoloServer({ defaultLocale: "en-US" });
    admin = createClient(srv.baseUrl);
    assertStatus(
      await admin.login(srv.adminCreds.username, srv.adminCreds.password),
      200,
      "Admin login",
    );
    browser = await launchEdge();
  });

  after(async () => {
    if (browser) {
      await browser.close();
    }
    if (srv) {
      await srv.stop();
    }
  });

  describe("S1: Cold-start Setup Wizard -> LoginView -> #about Telemetry & Retry", () => {
    it("completes SetupWizard on mobile/desktop, verifies LoginView & sidebar BootConfig.version, #about telemetry, and error/retry recovery", async () => {
      const freshSrv = await startVoloServer({
        skipSetup: true,
        defaultLocale: "en-US",
      });

      try {
        await withPage(
          browser,
          pageOpts({ width: 375, height: 812 }),
          async (page, diag) => {
            await page.goto(`${freshSrv.baseUrl}/setup`, {
              waitUntil: "domcontentloaded",
            });
            await page
              .locator(".volo-center-stage.is-setup .volo-wizard-box")
              .waitFor({ state: "visible", timeout: 15000 });
            await checkNoHorizontalOverflow(page);
            diag.assertClean();
          },
        );

        let bootVersion = "";
        await withPage(
          browser,
          pageOpts({ width: 1280, height: 800 }),
          async (page, diag) => {
            await page.goto(`${freshSrv.baseUrl}/setup`, {
              waitUntil: "domcontentloaded",
            });
            await page
              .locator(".volo-center-stage.is-setup .volo-wizard-box")
              .waitFor({ state: "visible", timeout: 15000 });
            await page
              .locator('input[name="setup_blog_title"]')
              .waitFor({ state: "visible", timeout: 15000 });

            const boot = await readBootConfig(page);
            assert.equal(boot.mode, "setup");
            assert.match(String(boot.version || ""), /^v?\d+\.\d+\.\d+/);
            bootVersion = boot.version;

            const freshTab = page.locator(".volo-subtabs > button.volo-tab").first();
            await freshTab.click();

            await page.locator('input[name="setup_blog_title"]').fill("S1 Cold Start Studio");
            await page.locator('input[name="setup_blog_subtitle"]').fill("Fresh S1 Subtitle");
            await page.locator('input[name="setup_public_url"]').fill(freshSrv.baseUrl);
            await page.locator(".volo-accent-dot-btn").nth(1).click();
            await page.locator('input[name="setup_owner_handle"]').fill("s1admin");
            await page.locator('input[name="setup_owner_nick"]').fill("S1 Owner");
            await page.locator('input[name="setup_owner_email"]').fill("s1@e2e.local");
            await page.locator('input[name="setup_owner_new_pwd"]').fill("S1Password!123");
            await page.locator('input[name="setup_owner_confirm_pwd"]').fill("S1Password!123");

            diag.clear();
            await page
              .locator('form button[type="submit"].volo-btn.volo-btn-primary')
              .click();
            await page.waitForURL(/\/console/, { timeout: 15000 });
            await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
            await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
            const railText = (await page.locator(".volo-rail-footer").textContent()) || "";
            assert.ok(railText.includes(`VOLO ${bootVersion}`));
            diag.assertClean();
          },
        );

        const s1Creds = { username: "s1admin", password: "S1Password!123" };

        await withPage(
          browser,
          pageOpts({ width: 1280, height: 800 }),
          async (page, diag) => {
            await page.goto(`${freshSrv.baseUrl}/console`, {
              waitUntil: "domcontentloaded",
            });
            await page.locator(".volo-auth-version").waitFor({ state: "visible", timeout: 15000 });
            const authBoot = await readBootConfig(page);
            assert.equal(authBoot.version, bootVersion);
            const authVerText = ((await page.locator(".volo-auth-version").textContent()) || "").trim();
            assert.equal(authVerText, `VOLO ${bootVersion}`);

            await page.locator('input[name="volo_staff_ident"]').fill(s1Creds.username);
            await page.locator('input[name="volo_staff_secret"]').fill(s1Creds.password);
            diag.clear();
            await page
              .locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit')
              .click();
            await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
            await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
            const railText = (await page.locator(".volo-rail-footer").textContent()) || "";
            assert.ok(railText.includes(`VOLO ${bootVersion}`));

            const infoPromise = page.waitForResponse(
              (res) => res.url().includes("/api/admin/about/project-info") && res.status() === 200,
            );
            await page
              .locator("nav.volo-nav-scroll button.volo-nav-btn", { hasText: /About/ })
              .click();
            await page.waitForFunction(() => location.hash === "#about");
            const infoRes = await infoPromise;
            const info = await infoRes.json();

            assert.equal(info.version, bootVersion);
            assert.ok(info.admin_gzip_kb > 0, "Expected admin_gzip_kb > 0");
            assert.equal(typeof info.online, "boolean");
            assert.ok(info.repo && info.repo.full_name, "Expected repo metadata");
            assert.ok(
              info.latest_release !== undefined,
              "Expected latest_release field in /api/admin/about/project-info",
            );

            await page.locator(".volo-about-hero .volo-badge").waitFor({ state: "visible" });
            const heroBadge = (await page.locator(".volo-about-hero .volo-badge").textContent()) || "";
            assert.ok(heroBadge.includes(info.version));
            const kvGrid = (await page.locator(".volo-about-kv-grid").textContent()) || "";
            assert.ok(kvGrid.includes(`${info.admin_gzip_kb} KB (gzip)`));
            const firstCard = (await page.locator(".volo-grid-2 .volo-card").first().textContent()) || "";
            assert.ok(firstCard.includes(info.repo.full_name));

            diag.assertClean();
          },
        );

        await withPage(
          browser,
          pageOpts({
            width: 1280,
            height: 800,
            ignoreConsolePatterns: [/500/, /Internal Server Error/, /Failed to load resource/],
          }),
          async (page, diag) => {
            let blockNext = true;
            await page.route("**/api/admin/about/project-info*", async (route) => {
              if (blockNext) {
                blockNext = false;
                await route.fulfill({
                  status: 500,
                  contentType: "application/json",
                  body: JSON.stringify({ error: "simulated telemetry failure" }),
                });
                return;
              }
              await route.continue();
            });

            await loginConsoleViaUI(page, diag, freshSrv.baseUrl, s1Creds, "#about");

            const alertBanner = page.locator('div[role="alert"].volo-draft-banner');
            await alertBanner.waitFor({ state: "visible", timeout: 10000 });
            const retryBtn = alertBanner.locator("button.volo-btn.volo-btn-sm");
            await retryBtn.waitFor({ state: "visible" });

            const recoverPromise = page.waitForResponse(
              (res) => res.url().includes("/api/admin/about/project-info") && res.status() === 200,
            );
            await retryBtn.click();
            const recRes = await recoverPromise;
            const info = await recRes.json();

            await alertBanner.waitFor({ state: "hidden", timeout: 10000 });
            const kvGrid = (await page.locator(".volo-about-kv-grid").textContent()) || "";
            assert.ok(kvGrid.includes(`${info.admin_gzip_kb} KB (gzip)`));

            diag.assertClean();
          },
        );
      } finally {
        await freshSrv.stop();
      }
    });
  });

  describe("S2 & S8: volo-journal Desktop TOC Drawer + Right-Docked kanbanniang & Wide Layout", () => {
    let s2ArtId;
    let s2DefaultPageId;
    let s2PageId;

    before(async () => {
      const art = await admin.post("/api/admin/articles", {
        title: "S8 Journal TOC Coexistence",
        slug: "s8-journal-toc-coexistence",
        content_md: "## Section One\n\nBody 1.\n\n### Subsection Two\n\nBody 2.\n\n## Section Three\n\nBody 3.",
        status: "published",
      });
      assertStatus(art, 201);
      s2ArtId = art.data.article.id;

      const defaultPg = await admin.post("/api/admin/pages", {
        title: "S2 Default Alignment Page",
        slug: "s2-default-alignment-page",
        template: "default",
        show_toc: true,
        is_published: true,
        content_md: "## Default Section\n\nDefault body text.",
      });
      assertStatus(defaultPg, 201);
      s2DefaultPageId = defaultPg.data.page.id;

      const pg = await admin.post("/api/admin/pages", {
        title: "S2 Wide Alignment Page",
        slug: "s2-wide-alignment-page",
        template: "wide",
        show_toc: true,
        is_published: true,
        content_md: "## Wide Section\n\nWide body text.",
      });
      assertStatus(pg, 201);
      s2PageId = pg.data.page.id;

      assertStatus(await admin.post("/api/admin/plugins/kanbanniang/enable"), 200);
      assertStatus(
        await admin.patch("/api/admin/plugins/kanbanniang/config", {
          position: "right",
          model: "Potion-Maker/Pio",
          hide_on_mobile: true,
        }),
        200,
      );
      await assertPluginCircuitClosed(admin, "kanbanniang", "S2/S8 setup");
      assertStatus(
        await admin.post("/api/admin/themes/activate", {
          theme_id: "volo-journal",
          target: "both",
        }),
        200,
      );
      assertStatus(
        await admin.patch("/api/admin/settings/appearance", {
          default_mode: "system",
          accent: "forest",
        }),
        200,
      );
    });

    after(async () => {
      await admin.post("/api/admin/plugins/kanbanniang/disable").catch(() => {});
      await admin
        .post("/api/admin/themes/activate", { theme_id: "volo-studio", target: "both" })
        .catch(() => {});
      if (s2ArtId) await admin.del(`/api/admin/articles/${s2ArtId}`).catch(() => {});
      if (s2DefaultPageId) await admin.del(`/api/admin/pages/${s2DefaultPageId}`).catch(() => {});
      if (s2PageId) await admin.del(`/api/admin/pages/${s2PageId}`).catch(() => {});
    });

    for (const mode of ["light", "dark"]) {
      it(`verifies wide alignment & volo-journal TOC drawer + right kanbanniang coexistence at 1440x900 (${mode})`, async () => {
        await withPage(
          browser,
          pageOpts({ width: 1440, height: 900, colorScheme: mode }),
          async (page, diag) => {
            await page.goto(`${srv.baseUrl}/`, { waitUntil: "domcontentloaded" });
            await page.waitForFunction(
              (m) =>
                document.documentElement.getAttribute("data-volo-skin") === "volo-journal" &&
                document.documentElement.getAttribute("data-theme") === m,
              mode,
            );
            await checkElementsAlignedX(
              page,
              [".studio-header__inner", "#studio-main", ".studio-footer"],
              { mode: "bounds", tolerance: 2 },
            );
            const normalWidth = await page.evaluate(
              () => document.querySelector("#studio-main").getBoundingClientRect().width,
            );

            await page.goto(`${srv.baseUrl}/s2-default-alignment-page`, {
              waitUntil: "domcontentloaded",
            });
            await page.locator('article[data-page-template="default"]').waitFor({ state: "visible" });
            const defaultWidths = await page.evaluate(() => ({
              layoutWidth: document.querySelector(".studio-layout--article").getBoundingClientRect().width,
              articleWidth: document.querySelector('article[data-page-template="default"]').getBoundingClientRect().width,
            }));

            await page.goto(`${srv.baseUrl}/s2-wide-alignment-page`, {
              waitUntil: "domcontentloaded",
            });
            await page.locator('article[data-page-template="wide"]').waitFor({ state: "visible" });
            await checkElementsAlignedX(
              page,
              [".studio-header__inner", "#studio-main", ".studio-footer"],
              { mode: "bounds", tolerance: 2 },
            );
            const wideWidths = await page.evaluate(() => ({
              mainWidth: document.querySelector("#studio-main").getBoundingClientRect().width,
              layoutWidth: document.querySelector(".studio-layout--article").getBoundingClientRect().width,
              articleWidth: document.querySelector('article[data-page-template="wide"]').getBoundingClientRect().width,
            }));
            assert.ok(wideWidths.mainWidth >= 1300 && wideWidths.mainWidth > normalWidth + 100);
            assert.ok(
              defaultWidths.articleWidth <= 855,
              `Expected default article width <= 855px, got ${defaultWidths.articleWidth}`,
            );
            assert.ok(
              wideWidths.layoutWidth >= 1300 && wideWidths.layoutWidth > defaultWidths.layoutWidth + 400,
              `Expected wide .studio-layout--article (${wideWidths.layoutWidth}) > default (${defaultWidths.layoutWidth})`,
            );
            assert.ok(
              wideWidths.articleWidth >= 1300 && wideWidths.articleWidth > defaultWidths.articleWidth + 400,
              `Expected article[data-page-template="wide"] (${wideWidths.articleWidth}) > default (${defaultWidths.articleWidth})`,
            );

            await page.goto(`${srv.baseUrl}/posts/s8-journal-toc-coexistence`, {
              waitUntil: "load",
            });
            await page.locator("#studio-main").waitFor({ state: "visible", timeout: 10000 });
            const kbn = page.locator("#volo-kanbanniang-root:not([hidden])");
            try {
              await kbn.waitFor({ state: "visible", timeout: 15000 });
            } catch (err) {
              await assertPluginCircuitClosed(admin, "kanbanniang", `S8 ${mode} timeout`);
              throw err;
            }
            const tocBtn = page.locator("#volo-mobile-toc-btn:not([hidden])");
            await tocBtn.waitFor({ state: "visible", timeout: 10000 });

            await checkNoOverlap(page, "#volo-mobile-toc-btn", "#volo-kanbanniang-root", {
              requireBothVisible: true,
            });
            await tocBtn.click();
            await page
              .locator("#volo-toc-drawer.is-open:not([hidden]) .volo-toc-drawer__panel")
              .waitFor({ state: "visible", timeout: 10000 });
            await waitForAnimations(page, ".volo-toc-drawer__panel");
            await checkNoOverlap(page, ".volo-toc-drawer__panel", ".solo-kanbanniang", {
              requireBothVisible: true,
            });
            await checkNoHorizontalOverflow(page);

            await page.keyboard.press("Escape");
            await page.locator("#volo-toc-drawer").waitFor({ state: "hidden", timeout: 10000 });

            // Closed-state summon pill coexistence with #volo-mobile-toc-btn & .volo-toc-drawer__panel
            await kbn.hover();
            const kbnCloseBtn = kbn.locator('[data-kbn-tool="close"]');
            await kbnCloseBtn.waitFor({ state: "visible", timeout: 5000 });
            await kbnCloseBtn.click();

            const summonPill = page.locator(
              "#volo-kanbanniang-summon.solo-kanbanniang-summon:not([hidden])",
            );
            await summonPill.waitFor({ state: "visible", timeout: 5000 });
            await checkNoOverlap(page, "#volo-mobile-toc-btn", "#volo-kanbanniang-summon", {
              requireBothVisible: true,
            });

            await tocBtn.click();
            await page
              .locator("#volo-toc-drawer.is-open:not([hidden]) .volo-toc-drawer__panel")
              .waitFor({ state: "visible", timeout: 10000 });
            await waitForAnimations(page, ".volo-toc-drawer__panel");
            await checkNoOverlap(page, ".volo-toc-drawer__panel", "#volo-kanbanniang-summon", {
              requireBothVisible: true,
            });
            await checkNoHorizontalOverflow(page);

            await page.keyboard.press("Escape");
            await page.locator("#volo-toc-drawer").waitFor({ state: "hidden", timeout: 10000 });

            await summonPill.click();
            await kbn.waitFor({ state: "visible", timeout: 10000 });
            await assertPluginCircuitClosed(admin, "kanbanniang", `S8 ${mode} end`);

            diag.assertClean();
          },
        );
      });
    }
  });

  describe("S3: Technical Author Journey in CM6 Editor -> Public Article & Comment Copy", () => {
    let s3ArtId;

    before(async () => {
      assertStatus(
        await admin.patch("/api/admin/settings/reading", {
          show_code_line_numbers: true,
          comment_access_mode: "all",
          pow_difficulty_bits: 8,
        }),
        200,
      );
      assertStatus(
        await admin.post("/api/admin/themes/activate", {
          theme_id: "volo-studio",
          target: "both",
        }),
        200,
      );
    });

    after(async () => {
      await admin
        .patch("/api/admin/settings/reading", {
          pow_difficulty_bits: 16,
        })
        .catch(() => {});
      if (s3ArtId) {
        await admin.del(`/api/admin/articles/${s3ArtId}`).catch(() => {});
      }
    });

    it("tests IR/WYSIWYG/SV modes, XSS javascript: sanitization, publishes rich article & verifies public .chroma .line width + copy stripping", async () => {
      const longToken = "VoloS3LongUnbrokenIdentifier_".repeat(7) + "Finish";
      const richBody = [
        "# S3 Architecture Overview",
        "",
        "## Multi-Language Code Blocks",
        "",
        "```go",
        "package s3demo",
        "",
        `const VeryLongLine = "${longToken}"`,
        "",
        "func ComputeValue(x int) int {",
        "\treturn x * 2",
        "}",
        "```",
        "",
        "```python",
        "def greet(name: str) -> str:",
        '    return f"hello {name}"',
        "```",
        "",
        "## Math & Mermaid",
        "",
        "Inline formula $\\alpha + \\beta$ and display math:",
        "",
        "$$E = mc^2$$",
        "",
        "```mermaid",
        "graph LR",
        "  Author[CM6 Editor] --> Published[Public Post]",
        "```",
        "",
        "> [!NOTE]",
        "> Technical callout block for reader guidance.",
        "",
        "## Wide Data Table",
        "",
        "| C1 | C2 | C3 | C4 | C5 | C6 | C7 | C8 |",
        "| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |",
        "| v1 | v2 | v3 | v4 | v5 | v6 | v7 | v8_long_value |",
      ].join("\n");

      await withPage(
        browser,
        pageOpts({ width: 1280, height: 800 }),
        async (page, diag) => {
          await page.route(/https:\/\/(cdn\.jsdelivr\.net|fastly\.jsdelivr\.net|unpkg\.com)\//, (route) =>
            route.fulfill({ status: 200, contentType: "application/javascript", body: "" }),
          );

          const categoriesReady = page.waitForResponse(
            (res) => res.url().includes("/api/admin/categories") && res.status() === 200,
          );
          const seriesReady = page.waitForResponse(
            (res) => res.url().includes("/api/admin/series") && res.status() === 200,
          );
          const tagsReady = page.waitForResponse(
            (res) => res.url().includes("/api/admin/tags") && res.status() === 200,
          );

          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor");
          await Promise.all([
            categoriesReady,
            seriesReady,
            tagsReady,
            page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 }),
          ]);

          const modeTabs = page.locator(
            '.volo-ed-modes[role="tablist"] > button.volo-ed-mode-btn[role="tab"]',
          );
          assert.equal(await modeTabs.count(), 3);

          // 1. Switch to WYSIWYG and test XSS javascript: paste sanitization
          await modeTabs.nth(1).click();
          const wysiwygSurface = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
          await wysiwygSurface.waitFor({ state: "visible", timeout: 10000 });
          await wysiwygSurface.click();

          await page.evaluate(() => {
            const el = document.querySelector('.volo-wysiwyg-surface[contenteditable="true"]');
            if (!el) throw new Error("Missing WYSIWYG surface");
            const dt = new DataTransfer();
            dt.setData(
              "text/html",
              '<p>AuthorSafeText <a href="javascript:alert(1)">EvilLink</a> <img src="javascript:alert(2)" alt="EvilImg"></p>',
            );
            dt.setData("text/plain", "AuthorSafeText EvilLink");
            el.dispatchEvent(
              new ClipboardEvent("paste", {
                bubbles: true,
                cancelable: true,
                clipboardData: dt,
              }),
            );
          });
          await page.waitForTimeout(250);

          await modeTabs.nth(0).click();
          await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
          const cmContent = page.locator(".volo-cm-host .cm-editor .cm-content");
          const afterPasteText = (await cmContent.textContent()) || "";
          assert.ok(afterPasteText.includes("AuthorSafeText"));
          assert.ok(
            afterPasteText.includes("EvilLink"),
            `Expected EvilLink anchor label preserved with href stripped, got: ${afterPasteText}`,
          );
          assert.ok(
            !afterPasteText.includes("EvilImg"),
            `Expected javascript: img stripped completely, got: ${afterPasteText}`,
          );
          assert.ok(
            !/javascript\s*:/i.test(afterPasteText),
            `Expected javascript: scheme stripped, got: ${afterPasteText}`,
          );
          assert.ok(
            !afterPasteText.includes("[EvilLink]("),
            `Expected markdown link syntax stripped for javascript: href, got: ${afterPasteText}`,
          );

          // 1b. Also verify HTML XSS paste sanitization directly in IR mode
          const isCmDocEmpty = () =>
            cmContent.evaluate((el) => {
              const clone = el.cloneNode(true);
              clone.querySelectorAll(".cm-placeholder").forEach((n) => n.remove());
              return (clone.textContent || "").trim() === "";
            });

          await cmContent.click();
          await page.keyboard.press("Control+a");
          await page.keyboard.press("Backspace");
          await page.waitForTimeout(120);
          await waitForCondition(isCmDocEmpty, "CM6 cleared before IR XSS paste");

          await page.evaluate(() => {
            const el = document.querySelector(".volo-cm-host .cm-editor .cm-content");
            if (!el) throw new Error("Missing IR cm-content");
            const dt = new DataTransfer();
            dt.setData(
              "text/html",
              '<p>IRSafeText <a href="javascript:alert(1)">EvilLink</a> <img src="javascript:alert(2)" alt="EvilImg"></p>',
            );
            dt.setData("text/plain", "IRSafeText EvilLink");
            el.dispatchEvent(
              new ClipboardEvent("paste", {
                bubbles: true,
                cancelable: true,
                clipboardData: dt,
              }),
            );
          });
          await page.waitForTimeout(150);
          const irPasteText = (await cmContent.textContent()) || "";
          assert.ok(irPasteText.includes("IRSafeText"));
          assert.ok(
            irPasteText.includes("EvilLink"),
            `Expected IR EvilLink label preserved with href stripped, got: ${irPasteText}`,
          );
          assert.ok(
            !irPasteText.includes("EvilImg"),
            `Expected IR javascript: img stripped, got: ${irPasteText}`,
          );
          assert.ok(
            !/javascript\s*:/i.test(irPasteText),
            `Expected IR javascript: scheme stripped, got: ${irPasteText}`,
          );
          assert.ok(
            !irPasteText.includes("[EvilLink]("),
            `Expected IR markdown link syntax stripped for javascript: href, got: ${irPasteText}`,
          );

          // 2. Replace with rich technical article (settling Preact after Backspace to avoid CM6 prop-sync race) & verify SV preview
          await cmContent.click();
          await page.keyboard.press("Control+a");
          await page.keyboard.press("Backspace");
          await page.waitForTimeout(120);
          await waitForCondition(isCmDocEmpty, "CM6 cleared before richBody insert");
          await page.keyboard.insertText(richBody);
          await waitForCondition(
            async () => ((await cmContent.textContent()) || "").includes("S3 Architecture Overview"),
            "CM6 contains S3 Architecture Overview",
          );
          await page.waitForTimeout(120);

          const previewPromise = page.waitForResponse(
            (res) => res.url().includes("/api/admin/render/preview") && res.status() === 200,
          );
          await modeTabs.nth(2).click();
          await page.locator(".volo-ed-body.mode-sv").waitFor({ state: "visible", timeout: 10000 });
          await previewPromise;
          const svPane = page.locator(".volo-ed-body.mode-sv .volo-preview-pane");
          await svPane.locator("h1", { hasText: "S3 Architecture Overview" }).waitFor({
            state: "visible",
            timeout: 10000,
          });
          await svPane.locator("pre.mermaid").waitFor({ state: "visible", timeout: 5000 });
          await svPane.locator(".volo-math-inline").waitFor({ state: "visible", timeout: 5000 });
          await svPane.locator(".volo-math-block").waitFor({ state: "visible", timeout: 5000 });
          await svPane.locator(".volo-callout--note").waitFor({ state: "visible" });
          await svPane.locator(".volo-table-scroll table").waitFor({ state: "visible" });

          // 3. Fill title/slug and publish
          await page.locator("input.volo-editor-title-input").fill("S3 Technical Author Journey");
          await page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Slug/i }) })
            .locator("input.volo-input")
            .fill("s3-tech-author-journey");

          const savePromise = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/articles") &&
              res.request().method() === "POST" &&
              (res.status() === 200 || res.status() === 201),
          );
          await page
            .locator(".volo-card-head button.volo-btn.volo-btn-sm.volo-btn-primary", {
              hasText: /Publish/,
            })
            .click();
          const saveRes = await savePromise;
          const saved = await saveRes.json();
          s3ArtId = saved.article.id;
          assert.equal(saved.article.status, "published");

          diag.assertClean();
        },
      );

      // 4. Verify public article rendering, .chroma .line scroll width, copy stripping & comment code copy
      const s3Views = [
        { width: 1280, height: 800, colorScheme: "light", testInteractiveCopy: true },
        { width: 1440, height: 900, colorScheme: "dark", testInteractiveCopy: false },
        { width: 375, height: 812, colorScheme: "light", testInteractiveCopy: false },
      ];

      for (const view of s3Views) {
        await withPage(
          browser,
          pageOpts({
            width: view.width,
            height: view.height,
            colorScheme: view.colorScheme,
            permissions: ["clipboard-read", "clipboard-write"],
          }),
          async (page, diag) => {
            await page.route(/https:\/\/(cdn\.jsdelivr\.net|fastly\.jsdelivr\.net|unpkg\.com)\//, (route) =>
              route.fulfill({ status: 200, contentType: "application/javascript", body: "" }),
            );

            await page.goto(`${srv.baseUrl}/posts/s3-tech-author-journey`, {
              waitUntil: "domcontentloaded",
            });
            // Wait for #volo-comments async mount + list hydration first so .vc-preview.volo-prose
            // and .vc-content.volo-prose are already in DOM before article.studio-article .volo-prose checks.
            await page
              .locator('#volo-comments[data-mounted="1"] .js-vc-submit')
              .waitFor({ state: "attached", timeout: 15000 });
            await page
              .locator("#volo-comments .js-vc-list > *")
              .first()
              .waitFor({ state: "attached", timeout: 15000 });
            await page
              .locator("article.studio-article .volo-prose")
              .waitFor({ state: "visible", timeout: 15000 });
            await checkNoHorizontalOverflow(page);

            // Go codeblock: .chroma .line width >= pre.scrollWidth & pre.clientWidth (note: .chroma .hl is CSS-only with no internal/render/ producer)
            const lineWidthReport = await page.evaluate(() => {
              const block = document.querySelector('.volo-codeblock[data-lang="go"]');
              if (!block) return null;
              const pre = block.querySelector("pre.chroma");
              const line = block.querySelector(".chroma .line");
              if (!pre || !line) return null;
              return {
                preScrollWidth: pre.scrollWidth,
                preClientWidth: pre.clientWidth,
                lineRectWidth: line.getBoundingClientRect().width,
              };
            });
            assert.ok(lineWidthReport, "Expected Go codeblock with .chroma .line");
            assert.ok(
              lineWidthReport.lineRectWidth >= lineWidthReport.preScrollWidth - 2 &&
                lineWidthReport.lineRectWidth >= lineWidthReport.preClientWidth - 2,
              `Expected .chroma .line width (${lineWidthReport.lineRectWidth}) >= pre.scrollWidth (${lineWidthReport.preScrollWidth}) and pre.clientWidth (${lineWidthReport.preClientWidth})`,
            );
            await checkNotClipped(page, '.volo-codeblock[data-lang="go"] .volo-copy-btn');

            // Python codeblock
            const pyBlock = page.locator('.volo-codeblock[data-lang="python"]');
            await pyBlock.waitFor({ state: "visible", timeout: 5000 });
            await pyBlock.locator(".volo-code-head .volo-copy-btn").waitFor({ state: "visible" });
            assert.ok((await pyBlock.locator(".chroma .line").count()) >= 2);
            await checkNotClipped(page, '.volo-codeblock[data-lang="python"] .volo-copy-btn');

            // Math SSR markers & containers
            const mathAttrs = await page.evaluate(() => ({
              htmlMath: document.documentElement.getAttribute("data-needs-math"),
              artMath: document.querySelector("article.studio-article")?.getAttribute("data-needs-math"),
              htmlMermaid: document.documentElement.getAttribute("data-needs-mermaid"),
              artMermaid: document.querySelector("article.studio-article")?.getAttribute("data-needs-mermaid"),
            }));
            assert.equal(mathAttrs.htmlMath, "1");
            assert.equal(mathAttrs.artMath, "1");
            assert.equal(mathAttrs.htmlMermaid, "1");
            assert.equal(mathAttrs.artMermaid, "1");

            const inlineMath = page.locator("article.studio-article .volo-prose .volo-math-inline");
            await inlineMath.waitFor({ state: "visible", timeout: 5000 });
            assert.ok(((await inlineMath.textContent()) || "").includes("\\alpha"));
            await checkNotClipped(page, "article.studio-article .volo-prose .volo-math-inline");

            const blockMath = page.locator("article.studio-article .volo-prose .volo-math-block");
            await blockMath.waitFor({ state: "visible", timeout: 5000 });
            assert.ok(((await blockMath.textContent()) || "").includes("E = mc^2"));
            await checkNotClipped(page, "article.studio-article .volo-prose .volo-math-block");

            // Mermaid container
            const mermaidEl = page.locator('article.studio-article .volo-prose pre.mermaid[data-mermaid-src]');
            await mermaidEl.waitFor({ state: "visible", timeout: 5000 });
            assert.ok(
              ((await mermaidEl.getAttribute("data-mermaid-src")) || "").includes("Author[CM6 Editor]"),
            );
            await checkNotClipped(page, "article.studio-article .volo-prose pre.mermaid[data-mermaid-src]");

            // Callout
            const calloutEl = page.locator("article.studio-article .volo-prose .volo-callout--note");
            await calloutEl.waitFor({ state: "visible", timeout: 5000 });
            assert.ok(
              ((await calloutEl.textContent()) || "").includes("Technical callout block for reader guidance."),
            );
            await checkNotClipped(page, "article.studio-article .volo-prose .volo-callout--note");

            // Wide table
            const tableWrap = page.locator("article.studio-article .volo-prose .volo-table-scroll");
            await tableWrap.waitFor({ state: "visible", timeout: 5000 });
            await tableWrap.locator("table").waitFor({ state: "visible", timeout: 5000 });
            await checkNotClipped(page, "article.studio-article .volo-prose .volo-table-scroll");
            if (view.width <= 375) {
              const tableScrollMetrics = await tableWrap.evaluate((el) => ({
                scrollWidth: el.scrollWidth,
                clientWidth: el.clientWidth,
              }));
              assert.ok(
                tableScrollMetrics.scrollWidth > tableScrollMetrics.clientWidth,
                `Expected .volo-table-scroll to scroll horizontally at 375px: ${JSON.stringify(tableScrollMetrics)}`,
              );
            }

            if (view.testInteractiveCopy) {
              const copyBtn = page
                .locator('.volo-codeblock[data-lang="go"] .volo-copy-btn')
                .first();
              await copyBtn.click();
              await page
                .locator('.volo-codeblock[data-lang="go"] .volo-copy-btn[data-copied="true"]')
                .waitFor({ state: "visible", timeout: 5000 });

              const copiedArticleCode = await page.evaluate(() =>
                navigator.clipboard.readText(),
              );
              assert.ok(copiedArticleCode.includes("package s3demo"));
              assert.ok(copiedArticleCode.includes("func ComputeValue(x int) int"));
              assert.ok(
                !/^1package\s+s3demo/m.test(copiedArticleCode) &&
                  !/^\s*1\s*package\s+s3demo/m.test(copiedArticleCode),
                `Expected .ln line numbers stripped from copied article code, got: ${JSON.stringify(copiedArticleCode)}`,
              );

              // Submit comment containing a fenced code block via #volo-comments UI
              const vcTrigger = page.locator("#volo-comments .js-vc-open");
              await vcTrigger.waitFor({ state: "visible", timeout: 10000 });
              await vcTrigger.click();
              await page
                .locator('#volo-comments input[name="author_name"]')
                .fill("S3 Code Reader");
              await page
                .locator("#volo-comments .js-vc-ta")
                .fill("Comment code check:\n\n```js\nconst alpha = 10;\nconst beta = 20;\n```");

              const postCommentPromise = page.waitForResponse(
                (res) =>
                  res.url().includes("/api/v1/comments") &&
                  res.request().method() === "POST" &&
                  res.status() === 201,
              );
              await page.locator("#volo-comments .js-vc-submit").click();
              await postCommentPromise;

              const commentCopyBtn = page.locator("#volo-comments .js-vc-copy").first();
              await commentCopyBtn.waitFor({ state: "visible", timeout: 10000 });
              await commentCopyBtn.click();
              await page
                .locator('#volo-comments .js-vc-copy[data-copied="true"]')
                .waitFor({ state: "visible", timeout: 5000 });

              // Note: highlightCommentCodeChroma intentionally emits no .ln/.lnt spans (article .volo-copy-btn covers .ln stripping)
              const copiedCommentCode = await page.evaluate(() =>
                navigator.clipboard.readText(),
              );
              assert.ok(copiedCommentCode.includes("const alpha = 10;"));
              assert.ok(copiedCommentCode.includes("const beta = 20;"));
              assert.ok(
                !/^\s*1\s*const\s+alpha/m.test(copiedCommentCode),
                `Expected clean copied comment code without line-number prefix, got: ${JSON.stringify(copiedCommentCode)}`,
              );
            }

            diag.assertClean();
          },
        );
      }
    });
  });

  describe("S4: Multi-Volume Book Authoring & Reader Navigation (>60 & >100 Chapters)", () => {
    let s4Post1Id;
    let s4Post2Id;
    let book65Id;
    let book115Id;
    let ch2Id;

    before(async () => {
      const p1 = await admin.post("/api/admin/articles", {
        title: "S4 Chapter One Body",
        slug: "s4-chapter-one-body",
        content_md: "# S4 Chapter One\n\nInline chapter body content for book reader.",
        status: "published",
      });
      const longChapterBody = [
        "# S4 Chapter Two",
        "",
        ...Array.from(
          { length: 16 },
          (_, idx) =>
            `Paragraph ${idx + 1}: Extended multi-volume book reader content for mobile scroll and floating TOC button verification.`,
        ),
      ].join("\n\n");
      const p2 = await admin.post("/api/admin/articles", {
        title: "S4 Chapter Two Body",
        slug: "s4-chapter-two-body",
        content_md: longChapterBody,
        status: "published",
      });
      assertStatus(p1, 201);
      assertStatus(p2, 201);
      s4Post1Id = p1.data.article.id;
      s4Post2Id = p2.data.article.id;

      const chapters65 = [];
      for (let v = 1; v <= 3; v += 1) {
        chapters65.push({ title: `Volume ${v}`, url: "", depth: 0 });
        for (let c = 1; c <= 21; c += 1) {
          const num = (v - 1) * 21 + c;
          chapters65.push({
            title: `Chapter ${String(num).padStart(3, "0")}`,
            url: num % 2 === 1 ? "/posts/s4-chapter-one-body" : "/posts/s4-chapter-two-body",
            depth: 1,
          });
        }
      }
      const b65 = await admin.post("/api/admin/books", {
        title: "S4 Medium Volume Book",
        slug: "s4-book-65",
        description: "Over 60 chapters across 3 volumes",
        source_type: "manual",
        embed_mode: "inline",
        chapters: chapters65,
      });
      assertStatus(b65, 201);
      book65Id = b65.data.book.id;

      const chapters115 = [];
      for (let v = 1; v <= 4; v += 1) {
        chapters115.push({ title: `Part ${v} — Volume ${v}`, url: "", depth: 0 });
        for (let c = 1; c <= 28; c += 1) {
          const num = (v - 1) * 28 + c;
          chapters115.push({
            title: `Chapter ${String(num).padStart(3, "0")}`,
            url: num % 2 === 1 ? "/posts/s4-chapter-one-body" : "/posts/s4-chapter-two-body",
            depth: 1,
          });
        }
      }
      const b115 = await admin.post("/api/admin/books", {
        title: "S4 Large Multi-Volume Book",
        slug: "s4-book-115",
        description: "Over 100 chapters across 4 volumes",
        source_type: "manual",
        embed_mode: "inline",
        chapters: chapters115,
      });
      assertStatus(b115, 201);
      book115Id = b115.data.book.id;

      const b115Detail = await admin.get(`/api/admin/books/${book115Id}`);
      assertStatus(b115Detail, 200);
      const ch2 = b115Detail.data.chapters.find((c) => c.title === "Chapter 002");
      assert.ok(ch2?.id, "Expected Chapter 002 in s4-book-115");
      ch2Id = ch2.id;
    });

    after(async () => {
      if (book65Id) await admin.del(`/api/admin/books/${book65Id}`).catch(() => {});
      if (book115Id) await admin.del(`/api/admin/books/${book115Id}`).catch(() => {});
      if (s4Post1Id) await admin.del(`/api/admin/articles/${s4Post1Id}`).catch(() => {});
      if (s4Post2Id) await admin.del(`/api/admin/articles/${s4Post2Id}`).catch(() => {});
    });

    it("renders <details> volumes, chevron styling, chapter filter, cpage=2 pagination, and prev/next neighbors across viewports & modes", async () => {
      const checks = [
        { width: 375, height: 812, colorScheme: "light", isMobile: true },
        { width: 1280, height: 800, colorScheme: "dark", isMobile: false },
        { width: 1440, height: 900, colorScheme: "light", isMobile: false },
      ];

      async function verifyVolumeAndFilter(page, isMobile) {
        const volumes = page.locator("details.book-reader-toc__volume");
        assert.ok((await volumes.count()) >= 2, "Expected >= 2 volume <details> groups");
        await checkNotClipped(page, ".book-reader-toc__volume-summary", {
          checkAncestorVertical: !isMobile,
        });
        await checkNotClipped(page, ".book-reader-toc__volume-chevron", {
          checkAncestorVertical: !isMobile,
        });

        const secondVol = volumes.nth(1);
        const wasOpen = await secondVol.evaluate((el) => el.open);
        await secondVol.locator("summary.book-reader-toc__volume-summary").click();
        const nowOpen = await secondVol.evaluate((el) => el.open);
        assert.equal(nowOpen, !wasOpen, "Expected volume <details> toggle to change open state");
        await checkNotClipped(
          page,
          "details.book-reader-toc__volume[open] .book-reader-toc__volume-items",
          { checkAncestorVertical: !isMobile },
        );

        const filterInput = page.locator("input.js-book-chap-filter");
        await filterInput.fill("Chapter 055");
        await page.waitForFunction(() => {
          const match = Array.from(document.querySelectorAll(".book-reader-toc__link")).find(
            (el) => el.textContent && el.textContent.includes("Chapter 055"),
          );
          const other = Array.from(document.querySelectorAll(".book-reader-toc__link")).find(
            (el) => el.textContent && el.textContent.includes("Chapter 001"),
          );
          return match && !match.hidden && other && other.hidden;
        });
        await filterInput.fill("");
      }

      for (const chk of checks) {
        await withPage(
          browser,
          pageOpts({
            width: chk.width,
            height: chk.height,
            colorScheme: chk.colorScheme,
          }),
          async (page, diag) => {
            // 1. s4-book-65 (>60, <=100 chapters): <details> volumes without .pagination--compact
            await page.goto(`${srv.baseUrl}/books/s4-book-65`, {
              waitUntil: "domcontentloaded",
            });
            await page.locator("[data-book-reader]").waitFor({ state: "visible", timeout: 10000 });
            await checkNoHorizontalOverflow(page);
            assert.equal(
              await page.locator(".pagination--compact").count(),
              0,
              "Expected 0 .pagination--compact elements on s4-book-65 (<=100 chapters)",
            );

            if (chk.isMobile) {
              await page.locator(".book-reader-toolbar__btn.js-book-toggle-toc").click();
              await page
                .locator(".js-book-toc-backdrop.is-open")
                .waitFor({ state: "visible", timeout: 5000 });
              await page
                .locator("[data-book-reader]:not(.is-toc-collapsed) aside.book-reader-toc")
                .waitFor({ state: "visible", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
            }
            await verifyVolumeAndFilter(page, chk.isMobile);
            if (chk.isMobile) {
              await page.locator(".book-reader-toc__collapse.js-book-toggle-toc").click();
              await page
                .locator(".js-book-toc-backdrop")
                .waitFor({ state: "hidden", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
            }

            // 2. s4-book-115 (>100 chapters): volumes, filter, cpage=2 & Prev/Next neighbors
            await page.goto(`${srv.baseUrl}/books/s4-book-115`, {
              waitUntil: "domcontentloaded",
            });
            await page.locator("[data-book-reader]").waitFor({ state: "visible", timeout: 10000 });
            await checkNoHorizontalOverflow(page);

            if (chk.isMobile) {
              await page.locator(".book-reader-toolbar__btn.js-book-toggle-toc").click();
              await page
                .locator(".js-book-toc-backdrop.is-open")
                .waitFor({ state: "visible", timeout: 5000 });
              await page
                .locator("[data-book-reader]:not(.is-toc-collapsed) aside.book-reader-toc")
                .waitFor({ state: "visible", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
            }

            assert.ok(
              (await page.locator("details.book-reader-toc__volume").count()) >= 3,
              "Expected >= 3 volume <details> groups on s4-book-115",
            );
            await verifyVolumeAndFilter(page, chk.isMobile);

            // Chapter pagination (?cpage=2)
            const cpage2Link = page
              .locator('.pagination--compact a.pagination__item[href*="cpage=2"]')
              .first();
            await cpage2Link.waitFor({ state: "visible", timeout: 5000 });
            await cpage2Link.click();
            await page.waitForURL(/cpage=2/, { timeout: 10000 });
            if (chk.isMobile) {
              await page.locator(".book-reader-toolbar__btn.js-book-toggle-toc").click();
              await page
                .locator(".js-book-toc-backdrop.is-open")
                .waitFor({ state: "visible", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
            }
            await page
              .locator("details.book-reader-toc__volume")
              .first()
              .waitFor({ state: "visible", timeout: 5000 });
            await checkNoHorizontalOverflow(page);

            // Both Prev and Next neighbor navigation from Chapter 002 (plus mobile scroll-to-reveal .js-book-mobile-toc)
            await page.goto(
              `${srv.baseUrl}/books/s4-book-115?ch=${encodeURIComponent(ch2Id)}`,
              { waitUntil: "domcontentloaded" },
            );
            if (chk.isMobile) {
              await page.locator("[data-book-reader].is-toc-collapsed").waitFor({
                state: "attached",
                timeout: 5000,
              });
              await page.evaluate(() => window.scrollTo(0, 260));
              const mobFab = page.locator(".js-book-mobile-toc:not([hidden])");
              await mobFab.waitFor({ state: "visible", timeout: 5000 });
              await mobFab.click();
              await page
                .locator(".js-book-toc-backdrop.is-open")
                .waitFor({ state: "visible", timeout: 5000 });
              await page
                .locator("[data-book-reader]:not(.is-toc-collapsed) aside.book-reader-toc")
                .waitFor({ state: "visible", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
              await page.locator(".book-reader-toc__collapse.js-book-toggle-toc").click();
              await page
                .locator(".js-book-toc-backdrop")
                .waitFor({ state: "hidden", timeout: 5000 });
              await waitForAnimations(page, "aside.book-reader-toc");
            }
            const neighborCards = page.locator(".book-reader-neighbors a.volo-neighbor-card");
            assert.equal(
              await neighborCards.count(),
              2,
              "Expected both Prev and Next neighbor cards on Chapter 002",
            );
            await checkNotClipped(page, ".book-reader-neighbors .volo-neighbor-card", {
              all: true,
            });
            const prevText = (await neighborCards.nth(0).textContent()) || "";
            const prevHref = await neighborCards.nth(0).getAttribute("href");
            assert.ok(prevText.includes("Chapter 001"), `Expected Prev=Chapter 001, got ${prevText}`);
            assert.ok(prevHref, "Expected href on Prev neighbor card");
            await neighborCards.nth(0).click();
            await page.waitForFunction(
              () => window.location.pathname.includes("/posts/s4-chapter-one-body"),
              null,
              { timeout: 10000 },
            );

            await page.goto(
              `${srv.baseUrl}/books/s4-book-115?ch=${encodeURIComponent(ch2Id)}`,
              { waitUntil: "domcontentloaded" },
            );
            const nextCards = page.locator(".book-reader-neighbors a.volo-neighbor-card");
            const nextText = (await nextCards.nth(1).textContent()) || "";
            const nextHref = await nextCards.nth(1).getAttribute("href");
            assert.ok(nextText.includes("Chapter 003"), `Expected Next=Chapter 003, got ${nextText}`);
            assert.ok(nextHref, "Expected href on Next neighbor card");
            await nextCards.nth(1).click();
            await page.waitForFunction(
              () => window.location.pathname.includes("/posts/s4-chapter-one-body"),
              null,
              { timeout: 10000 },
            );

            diag.assertClean();
          },
        );
      }
    });
  });

  describe("S5: Admin Settings UI Persistence -> Public Reflection", () => {
    const s5ArticleIds = [];

    before(async () => {
      for (let i = 1; i <= 5; i += 1) {
        const res = await admin.post("/api/admin/articles", {
          title: `S5 Paged Post ${i}`,
          slug: `s5-paged-post-${i}`,
          content_md: `S5 post ${i} content body.`,
          status: "published",
        });
        assertStatus(res, 201);
        s5ArticleIds.push(res.data.article.id);
      }
    });

    after(async () => {
      await admin
        .patch("/api/admin/settings/site", {
          title: "Volo E2E",
          subtitle: "",
        })
        .catch(() => {});
      await admin
        .patch("/api/admin/settings/reading", {
          posts_per_page: 10,
        })
        .catch(() => {});
      await admin
        .post("/api/admin/themes/activate", {
          theme_id: "volo-studio",
          target: "both",
        })
        .catch(() => {});
      await admin
        .patch("/api/admin/settings/appearance", {
          accent: "forest",
          default_mode: "light",
        })
        .catch(() => {});
      await admin
        .patch("/api/admin/settings/feed", {
          item_count: 20,
          enable_llms_txt: true,
        })
        .catch(() => {});
      await admin
        .patch("/api/admin/settings/seo", {
          meta_description: "",
          meta_keywords: "",
        })
        .catch(() => {});
      for (const id of s5ArticleIds) {
        await admin.del(`/api/admin/articles/${id}`).catch(() => {});
      }
    });

    it("persists #settings_site, #themes & #feed_seo updates across reload and reflects them on clean public /", async () => {
      await withPage(
        browser,
        pageOpts({ width: 1280, height: 800 }),
        async (page, diag) => {
          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#settings_site");

          const titleInput = page.locator('input[name="site_main_title"]');
          const subInput = page.locator('input[name="site_sub_tagline"]');
          await titleInput.waitFor({ state: "visible", timeout: 10000 });
          await page.waitForFunction(() => {
            const el = document.querySelector('input[name="site_main_title"]');
            return el && el.value.length > 0;
          });

          await titleInput.fill("S5 Persisted Studio");
          await subInput.fill("S5 Public Tagline");
          const siteForm = page.locator("form.volo-card").filter({ has: titleInput });
          const sitePatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/settings/site") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await siteForm.locator('button[type="submit"].volo-btn.volo-btn-primary').click();
          await sitePatch;

          const pppField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Posts Per Page$/ }) });
          const pppInput = pppField.locator('input[type="number"]');
          await pppInput.fill("2");
          const readingForm = page.locator("form.volo-card").filter({ has: pppInput });
          const readingPatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/settings/reading") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await readingForm.locator('button[type="submit"].volo-btn.volo-btn-primary').click();
          await readingPatch;

          // Configure Feed & SEO settings via #feed_seo UI
          await page.evaluate(() => {
            location.hash = "#feed_seo";
          });
          await page.waitForFunction(() => location.hash === "#feed_seo");
          const feedCountField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Feed Item Count$/ }) });
          const feedCountInput = feedCountField.locator('input[type="number"]');
          await feedCountInput.waitFor({ state: "visible", timeout: 10000 });
          await feedCountInput.fill("7");

          const feedPatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/settings/feed") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await page
            .locator("button.volo-btn.volo-btn-primary", { hasText: /Save Syndication Settings/ })
            .click();
          await feedPatch;

          const seoKwField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Default Meta Keywords$/ }) });
          const seoKwInput = seoKwField.locator("input.volo-tag-inline-input");
          await seoKwInput.fill("s5seokeyword");
          await seoKwInput.press("Enter");
          await seoKwField.locator(".volo-tag-pill", { hasText: "s5seokeyword" }).waitFor({
            state: "visible",
            timeout: 5000,
          });

          const seoDescField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Default Meta Description$/ }) });
          await seoDescField.locator("textarea.volo-textarea").fill("S5 Persisted SEO Description");

          const seoPatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/settings/seo") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await page
            .locator("button.volo-btn.volo-btn-primary", { hasText: /Save SEO Settings/ })
            .click();
          await seoPatch;

          const themesNavBtn = page.locator("nav.volo-nav-scroll button.volo-nav-btn", {
            hasText: /Themes & Plugins|Themes/,
          });
          if (!(await themesNavBtn.isVisible().catch(() => false))) {
            const appGroup = page.locator("nav.volo-nav-scroll button.volo-nav-group-btn", {
              hasText: /Appearance/i,
            });
            if ((await appGroup.count()) > 0) {
              await appGroup.first().click();
            }
          }
          if (await themesNavBtn.isVisible().catch(() => false)) {
            await themesNavBtn.click();
          } else {
            await page.evaluate(() => {
              location.hash = "#themes";
            });
          }
          await page.waitForFunction(() => location.hash === "#themes");

          const journalCard = page
            .locator(".volo-theme-card")
            .filter({ has: page.locator('a[href*="skin=volo-journal"]') });
          await journalCard.waitFor({ state: "visible", timeout: 10000 });
          const activateBtn = journalCard.locator("button.volo-btn.volo-btn-sm.volo-btn-primary");
          if ((await activateBtn.count()) > 0) {
            const actPromise = page.waitForResponse(
              (res) => res.url().includes("/api/admin/themes/activate") && res.status() === 200,
            );
            await activateBtn.click();
            await actPromise;
          }

          // Select ocean accent (2nd swatch) and Dark default color mode
          const oceanBtn = page.locator("button.volo-btn.volo-btn-sm:has(.volo-swatch-dot)", {
            hasText: /Ocean/,
          });
          await oceanBtn.click();

          const modeField = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Default Color Mode/ }) });
          await modeField.locator(".volo-cselect-btn").click();
          await page
            .locator(".volo-cselect-menu.is-portal .volo-cselect-item", { hasText: /Always Dark|Dark/i })
            .first()
            .click();

          const appPatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/settings/appearance") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await page
            .locator(".volo-card button.volo-btn.volo-btn-primary", { hasText: /Save Changes/ })
            .click();
          await appPatch;

          // Reload #themes and verify theme, accent, and mode persisted
          await page.reload({ waitUntil: "domcontentloaded" });
          await page
            .locator(".volo-theme-card.active")
            .filter({ has: page.locator('a[href*="skin=volo-journal"]') })
            .waitFor({ state: "visible", timeout: 10000 });
          const activeAccentBtn = page.locator(
            "button.volo-btn.volo-btn-sm.volo-btn-primary:has(.volo-swatch-dot)",
          );
          await activeAccentBtn.waitFor({ state: "visible", timeout: 5000 });
          assert.ok(
            ((await activeAccentBtn.textContent()) || "").includes("Ocean"),
            "Expected Ocean accent button to remain active after reload",
          );
          const modeLabel =
            (await page
              .locator(".volo-field")
              .filter({ has: page.locator("label", { hasText: /Default Color Mode/ }) })
              .locator(".volo-cselect-btn .volo-cselect-label")
              .textContent()) || "";
          assert.equal(modeLabel.trim(), "Always Dark");

          // Navigate to #settings_site after reload and verify persisted inputs
          await page.evaluate(() => {
            location.hash = "#settings_site";
          });
          await page.waitForFunction(() => location.hash === "#settings_site");
          await page.locator('input[name="site_main_title"]').waitFor({ state: "visible", timeout: 10000 });
          await page.waitForFunction(
            () =>
              document.querySelector('input[name="site_main_title"]')?.value ===
              "S5 Persisted Studio",
          );
          assert.equal(
            await page.locator('input[name="site_sub_tagline"]').inputValue(),
            "S5 Public Tagline",
          );
          assert.equal(
            await page
              .locator(".volo-field")
              .filter({ has: page.locator("label", { hasText: /^Posts Per Page$/ }) })
              .locator('input[type="number"]')
              .inputValue(),
            "2",
          );

          // Navigate to #feed_seo after reload and verify persisted inputs
          await page.evaluate(() => {
            location.hash = "#feed_seo";
          });
          await page.waitForFunction(() => location.hash === "#feed_seo");
          const reloadedFeedCount = page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Feed Item Count$/ }) })
            .locator('input[type="number"]');
          await reloadedFeedCount.waitFor({ state: "visible", timeout: 10000 });
          assert.equal(await reloadedFeedCount.inputValue(), "7");
          assert.equal(
            await page
              .locator(".volo-field")
              .filter({ has: page.locator("label", { hasText: /^Default Meta Description$/ }) })
              .locator("textarea.volo-textarea")
              .inputValue(),
            "S5 Persisted SEO Description",
          );
          await page
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /^Default Meta Keywords$/ }) })
            .locator(".volo-tag-pill", { hasText: "s5seokeyword" })
            .waitFor({ state: "visible", timeout: 5000 });

          diag.assertClean();
        },
      );

      // Fresh clean public browser context verifies every setting took effect
      await withPage(
        browser,
        pageOpts({ width: 1440, height: 900 }),
        async (page, diag) => {
          await page.goto(`${srv.baseUrl}/`, { waitUntil: "domcontentloaded" });
          await page.locator("#studio-main").waitFor({ state: "visible", timeout: 10000 });

          const htmlAttrs = await page.evaluate(() => ({
            skin: document.documentElement.getAttribute("data-volo-skin"),
            accent: document.documentElement.getAttribute("data-accent"),
            mode: document.documentElement.getAttribute("data-default-mode"),
            theme: document.documentElement.getAttribute("data-theme"),
            metaDesc: document.querySelector('meta[name="description"]')?.getAttribute("content") || "",
            metaKw: document.querySelector('meta[name="keywords"]')?.getAttribute("content") || "",
          }));
          assert.equal(htmlAttrs.skin, "volo-journal");
          assert.equal(htmlAttrs.accent, "ocean");
          assert.equal(htmlAttrs.mode, "dark");
          assert.equal(htmlAttrs.theme, "dark");
          assert.equal(htmlAttrs.metaDesc, "S5 Persisted SEO Description");
          assert.ok(htmlAttrs.metaKw.includes("s5seokeyword"));

          const bodyText = (await page.locator("body").textContent()) || "";
          assert.ok(bodyText.includes("S5 Persisted Studio"));

          const mottoText = (await page.locator("p.journal-masthead__motto").textContent()) || "";
          assert.ok(
            mottoText.includes("S5 Public Tagline"),
            `Expected p.journal-masthead__motto to contain "S5 Public Tagline", got: ${mottoText}`,
          );

          const postCards = await page.locator(".studio-posts > article.article__item").count();
          assert.ok(postCards <= 2 && postCards >= 1, `Expected <= 2 posts per page, got ${postCards}`);
          const page2Link = page.locator('nav.pagination a.pagination__item[href*="page=2"]').first();
          await page2Link.waitFor({ state: "visible", timeout: 5000 });
          await checkNoHorizontalOverflow(page);

          await page2Link.click();
          await page.waitForURL(/\?page=2/, { timeout: 10000 });
          await page.locator("#studio-main").waitFor({ state: "visible", timeout: 10000 });
          const page2Cards = await page.locator(".studio-posts > article.article__item").count();
          assert.ok(page2Cards <= 2 && page2Cards >= 1, `Expected 1..2 posts on page=2, got ${page2Cards}`);
          const brandSubText = (await page.locator(".studio-brand-subtitle").textContent()) || "";
          assert.ok(
            brandSubText.includes("S5 Public Tagline"),
            `Expected .studio-brand-subtitle on /?page=2 to contain "S5 Public Tagline", got: ${brandSubText}`,
          );
          await checkNoHorizontalOverflow(page);

          const rssRes = await page.request.get(`${srv.baseUrl}/rss.xml`);
          assert.equal(rssRes.status(), 200);
          const rssXml = await rssRes.text();
          assert.ok(rssXml.includes("S5 Persisted Studio"));
          const rssItemCount = (rssXml.match(/<item>/g) || []).length;
          assert.ok(rssItemCount >= 1 && rssItemCount <= 7, `Expected 1..7 RSS items, got ${rssItemCount}`);

          const atomRes = await page.request.get(`${srv.baseUrl}/atom.xml`);
          assert.equal(atomRes.status(), 200);
          const atomXml = await atomRes.text();
          assert.ok(atomXml.includes("S5 Persisted Studio"));
          assert.ok(atomXml.includes("S5 Public Tagline"));

          const llmsRes = await page.request.get(`${srv.baseUrl}/llms.txt`);
          assert.equal(llmsRes.status(), 200);
          assert.ok((await llmsRes.text()).includes("S5 Persisted Studio"));

          diag.assertClean();
        },
      );
    });
  });

  describe("S6: Large Taxonomy & Link Directory Exploration", () => {
    const catIds = [];
    const artIds = [];
    const linkIds = [];

    before(async () => {
      for (let i = 1; i <= 40; i += 1) {
        const pad = String(i).padStart(2, "0");
        const cRes = await admin.post("/api/admin/categories", {
          name: `S6 Category ${pad}`,
          slug: `s6-cat-${pad}`,
          description: `Taxonomy category ${pad}`,
          sort_order: i,
        });
        assertStatus(cRes, 201);
        catIds.push(cRes.data.category.id);
      }

      for (let batch = 0; batch < 5; batch += 1) {
        const tagNames = [];
        for (let j = 1; j <= 25; j += 1) {
          const num = batch * 25 + j;
          tagNames.push(`s6tag${String(num).padStart(3, "0")}`);
        }
        const artRes = await admin.post("/api/admin/articles", {
          title: `S6 Multi-Tag Anchor Article ${batch + 1}`,
          slug: `s6-multi-tag-anchor-${batch + 1}`,
          content_md: `Taxonomy tags verification batch ${batch + 1}.`,
          status: "published",
          category_id: catIds[0],
          tags: tagNames,
        });
        assertStatus(artRes, 201);
        artIds.push(artRes.data.article.id);
      }

      for (let g = 1; g <= 20; g += 1) {
        const gName = `S6LinkCat-${String(g).padStart(2, "0")}`;
        for (let k = 1; k <= 2; k += 1) {
          const lRes = await admin.post("/api/admin/links", {
            title: `S6 Friend ${g}-${k}`,
            url: `https://example.com/s6-${g}-${k}`,
            description: `Directory link ${g}-${k}`,
            category_name: gName,
          });
          assertStatus(lRes, 201);
          linkIds.push(lRes.data.link.id);
        }
      }
      for (let k = 1; k <= 12; k += 1) {
        const lRes = await admin.post("/api/admin/links", {
          title: `S6 Overflow Link ${k}`,
          url: `https://example.com/s6-ovf-${k}`,
          description: `Overflow directory item ${k}`,
          category_name: "S6LinkCat-Overflow",
        });
        assertStatus(lRes, 201);
        linkIds.push(lRes.data.link.id);
      }
    });

    after(async () => {
      if (linkIds.length > 0) {
        await admin
          .post("/api/admin/links/batch", { action: "delete", ids: linkIds })
          .catch(() => {});
      }
      for (const id of artIds) {
        await admin.del(`/api/admin/articles/${id}`).catch(() => {});
      }
      for (const id of catIds) {
        await admin.del(`/api/admin/categories/${id}`).catch(() => {});
      }
      await admin.post("/api/admin/tags/cleanup").catch(() => {});
    });

    for (const vp of [
      { width: 375, height: 812 },
      { width: 1440, height: 900 },
    ]) {
      it(`explores /categories, /tags, and /links (+N jump pills, server ?q= search, pagination) at ${vp.width}x${vp.height}`, async () => {
        await withPage(browser, pageOpts(vp), async (page, diag) => {
          // 1. /categories
          await page.goto(`${srv.baseUrl}/categories`, { waitUntil: "domcontentloaded" });
          await page.locator('[data-collection-root="categories"]').waitFor({ state: "visible" });
          await checkNoHorizontalOverflow(page);

          await checkNotClipped(page, '[data-collection-root="categories"] .studio-jump-pills');
          const catMoreBtn = page.locator('[data-collection-root="categories"] button.js-jump-pills-toggle');
          await catMoreBtn.waitFor({ state: "visible", timeout: 5000 });
          await catMoreBtn.scrollIntoViewIfNeeded();
          await checkNotClipped(page, '[data-collection-root="categories"] button.js-jump-pills-toggle');
          await catMoreBtn.click();
          assert.equal(await catMoreBtn.getAttribute("aria-expanded"), "true");
          assert.equal(
            await page.locator('[data-collection-root="categories"] [data-extra-pill]:not([hidden])').count() > 0,
            true,
          );

          await page.locator('nav.pagination a.pagination__item[href*="page=2"]').first().click();
          await page.waitForURL(/\/categories\?page=2/, { timeout: 10000 });
          await checkNoHorizontalOverflow(page);

          await page.locator('input.js-collection-filter[name="q"]').fill("Category 39");
          await page.locator(".studio-tax-search__btn").click();
          await page.waitForURL(/q=Category(\+|%20)39/, { timeout: 10000 });
          assert.ok(
            ((await page.locator(".bookshelf-grid").textContent()) || "").includes("S6 Category 39"),
          );

          // 2. /tags (>120 tags triggers pagination & server ?q= search via Enter)
          await page.goto(`${srv.baseUrl}/tags`, { waitUntil: "domcontentloaded" });
          await page.locator('input.js-tag-filter-input[name="q"]').waitFor({ state: "visible" });
          await checkNoHorizontalOverflow(page);

          const tagsPage2Link = page
            .locator('nav.pagination a.pagination__item[href*="page=2"]')
            .first();
          await tagsPage2Link.waitFor({ state: "visible", timeout: 5000 });
          await tagsPage2Link.click();
          await page.waitForURL(/\/tags\?.*page=2/, { timeout: 10000 });
          assert.ok(
            (await page.locator(".studio-tax-tag").count()) >= 1,
            "Expected tag chips on /tags?page=2",
          );
          await checkNoHorizontalOverflow(page);

          await page.locator('.studio-sort-pill[href*="sort=name"]').click();
          await page.waitForURL(/sort=name/, { timeout: 10000 });

          const tagInput = page.locator('input.js-tag-filter-input[name="q"]');
          await tagInput.fill("s6tag124");
          await tagInput.press("Enter");
          await page.waitForURL(/q=s6tag124/, { timeout: 10000 });
          await page.locator("a.js-tag-filter-clear").waitFor({ state: "visible", timeout: 5000 });
          assert.equal(await page.locator(".studio-tax-tag").count(), 1);
          assert.ok(
            ((await page.locator(".studio-tax-tag").first().textContent()) || "").includes("s6tag124"),
          );

          // 3. /links
          await page.goto(`${srv.baseUrl}/links`, { waitUntil: "domcontentloaded" });
          await page.locator('[data-collection-root="links"]').waitFor({ state: "visible" });
          await checkNoHorizontalOverflow(page);

          await checkNotClipped(page, '[data-collection-root="links"] .studio-jump-pills');
          const linkMoreBtn = page.locator('[data-collection-root="links"] button.js-jump-pills-toggle');
          await linkMoreBtn.waitFor({ state: "visible", timeout: 5000 });
          await linkMoreBtn.scrollIntoViewIfNeeded();
          await checkNotClipped(page, '[data-collection-root="links"] button.js-jump-pills-toggle');
          await linkMoreBtn.click();
          assert.equal(await linkMoreBtn.getAttribute("aria-expanded"), "true");
          assert.ok(
            (await page
              .locator('[data-collection-root="links"] [data-extra-pill]:not([hidden])')
              .count()) > 0,
            "Expected expanded [data-extra-pill]:not([hidden]) on /links",
          );

          await page.locator('nav.pagination a.pagination__item[href*="page=2"]').first().click();
          await page.waitForURL(/\/links\?page=2/, { timeout: 10000 });
          await checkNoHorizontalOverflow(page);

          await page.locator('input.js-collection-filter[name="q"]').fill("Overflow Link 7");
          await page.locator(".studio-tax-search__btn").click();
          await page.waitForURL(/q=Overflow/, { timeout: 10000 });
          assert.ok(
            ((await page.locator('[data-collection-root="links"]').textContent()) || "").includes(
              "S6 Overflow Link 7",
            ),
          );

          diag.assertClean();
        });
      });
    }
  });

  describe("S7: Anti-Spam & Moderation Workflow (sensitive-words -> Guest Reject -> Moderate & Reply)", () => {
    let s7ArtId;

    before(async () => {
      assertStatus(
        await admin.patch("/api/admin/settings/reading", {
          comment_access_mode: "all",
        }),
        200,
      );
      const art = await admin.post("/api/admin/articles", {
        title: "S7 Moderation Target",
        slug: "s7-moderation-target",
        content_md: "Target article for sensitive-words & admin reply workflow.",
        status: "published",
        allow_comment: true,
      });
      assertStatus(art, 201);
      s7ArtId = art.data.article.id;
    });

    after(async () => {
      await admin.post("/api/admin/plugins/sensitive-words/disable").catch(() => {});
      if (s7ArtId) {
        await admin.del(`/api/admin/articles/${s7ArtId}`).catch(() => {});
      }
    });

    it("configures sensitive-words in Admin #plugins, rejects banned guest comment, accepts clean comment, and publishes Admin reply", async () => {
      // 1. Configure sensitive-words via Admin UI (#plugins)
      await withPage(
        browser,
        pageOpts({ width: 1280, height: 800 }),
        async (page, diag) => {
          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#plugins");

          const swRow = page.locator("tr").filter({
            has: page.locator(".volo-code-chip", { hasText: /^sensitive-words$/ }),
          });
          await swRow.waitFor({ state: "visible", timeout: 10000 });

          const toggleBtn = swRow.locator('td.col-status button[role="switch"]');
          if ((await toggleBtn.getAttribute("aria-checked")) !== "true") {
            const enPromise = page.waitForResponse(
              (res) =>
                res.url().includes("/api/admin/plugins/sensitive-words/enable") &&
                res.status() === 200,
            );
            await toggleBtn.click();
            await enPromise;
          }

          await swRow.locator("td.col-act button.volo-act-btn", { hasText: /Configure/ }).click();
          const modal = page.locator(".volo-modal");
          await modal.waitFor({ state: "visible", timeout: 5000 });

          const actionField = modal
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Comment Match Action/ }) });
          await actionField.locator(".volo-cselect-btn").click();
          await page
            .locator(".volo-cselect-menu.is-portal .volo-cselect-item", {
              hasText: /Reject Immediately/,
            })
            .click();

          const denyField = modal
            .locator(".volo-field")
            .filter({ has: page.locator("label", { hasText: /Custom Blocked Words/ }) });
          const denyInput = denyField.locator("input.volo-tag-inline-input");
          await denyInput.fill("e2ebannedterm");
          await denyInput.press("Enter");
          await denyField.locator(".volo-tag-pill", { hasText: "e2ebannedterm" }).waitFor({
            state: "visible",
          });

          const cfgPatch = page.waitForResponse(
            (res) =>
              res.url().includes("/api/admin/plugins/sensitive-words/config") &&
              res.request().method() === "PATCH" &&
              res.status() === 200,
          );
          await modal
            .locator(".volo-form-actions button.volo-btn.volo-btn-primary")
            .click();
          await cfgPatch;
          await modal.waitFor({ state: "hidden", timeout: 5000 });

          diag.assertClean();
        },
      );

      // 2. Guest attempts banned comment (rejected 403) and then submits clean comment (201)
      await withPage(
        browser,
        pageOpts({
          width: 1280,
          height: 800,
          ignoreConsolePatterns: [/403/, /Forbidden/, /Failed to load resource/],
        }),
        async (page, diag) => {
          await page.goto(`${srv.baseUrl}/posts/s7-moderation-target`, {
            waitUntil: "domcontentloaded",
          });
          await page.locator("#volo-comments .js-vc-open").click();
          await page.locator('#volo-comments input[name="author_name"]').fill("S7 Guest");
          await page
            .locator("#volo-comments .js-vc-ta")
            .fill("This comment contains e2ebannedterm and must fail.");

          const rejectPromise = page.waitForResponse(
            (res) =>
              res.url().includes("/api/v1/comments") &&
              res.request().method() === "POST" &&
              res.status() === 403,
          );
          await page.locator("#volo-comments .js-vc-submit").click();
          await rejectPromise;
          await page.locator("#volo-comments .js-vc-msg.is-err").waitFor({
            state: "visible",
            timeout: 5000,
          });

          await page.reload({ waitUntil: "domcontentloaded" });
          const listAfterReject = (await page.locator("#volo-comments").textContent()) || "";
          assert.ok(!listAfterReject.includes("e2ebannedterm"));

          // Submit clean comment
          await page.locator("#volo-comments .js-vc-open").click();
          await page.locator('#volo-comments input[name="author_name"]').fill("S7 Guest");
          await page
            .locator("#volo-comments .js-vc-ta")
            .fill("Clean S7 guest feedback for moderation.");

          const okPromise = page.waitForResponse(
            (res) =>
              res.url().includes("/api/v1/comments") &&
              res.request().method() === "POST" &&
              res.status() === 201,
          );
          await page.locator("#volo-comments .js-vc-submit").click();
          await okPromise;
          await page
            .locator("#volo-comments .vc-item", { hasText: "Clean S7 guest feedback for moderation." })
            .waitFor({ state: "visible", timeout: 10000 });

          diag.assertClean();
        },
      );

      // 3. Admin moderates (approves if pending) & replies via #comments UI
      await withPage(
        browser,
        pageOpts({ width: 1280, height: 800 }),
        async (page, diag) => {
          await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#comments");

          const commentRow = page
            .locator("tr", { hasText: "Clean S7 guest feedback for moderation." })
            .first();
          await commentRow.waitFor({ state: "visible", timeout: 10000 });

          const approveBtn = commentRow.locator("td.col-act button.volo-act-btn", {
            hasText: /^Approve$/,
          });
          if ((await approveBtn.count()) > 0) {
            const appPromise = page.waitForResponse(
              (res) => res.url().includes("/api/admin/comments/") && res.status() === 200,
            );
            await approveBtn.click();
            await appPromise;
          }

          await commentRow.locator("td.col-act button.volo-act-btn", { hasText: /^Reply$/ }).click();
          const replyBox = commentRow.locator("textarea.volo-textarea");
          await replyBox.waitFor({ state: "visible", timeout: 5000 });
          await replyBox.fill("Official S7 staff reply to guest.");

          const replyPromise = page.waitForResponse(
            (res) => res.url().includes("/reply") && res.status() === 201,
          );
          await commentRow
            .locator("button.volo-btn.volo-btn-primary.volo-btn-sm", { hasText: /^Reply$/ })
            .click();
          await replyPromise;

          diag.assertClean();
        },
      );

      // 4. Guest sees official reply on public article
      await withPage(
        browser,
        pageOpts({ width: 1280, height: 800 }),
        async (page, diag) => {
          await page.goto(`${srv.baseUrl}/posts/s7-moderation-target`, {
            waitUntil: "domcontentloaded",
          });
          await page
            .locator("#volo-comments", { hasText: "Official S7 staff reply to guest." })
            .waitFor({ state: "visible", timeout: 10000 });
          diag.assertClean();
        },
      );
    });
  });
});
