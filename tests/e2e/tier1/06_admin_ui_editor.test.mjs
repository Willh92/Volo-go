import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import { assertStatus } from "../lib/assertions.mjs";
import {
  checkNoHorizontalOverflow,
  checkNotClipped,
  launchEdge,
  withPage,
} from "../lib/browser.mjs";
import { createClient } from "../lib/client.mjs";
import { startVoloServer } from "../lib/server.mjs";

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
  await page.locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit').click();
  await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
  await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
}

describe("Tier 1 — Admin SPA UI & CM6 Markdown Editor (F16)", () => {
  let srv;
  let browser;

  before(async () => {
    srv = await startVoloServer();
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

  it("SetupWizard BootConfig version & fresh setup UI flow", async () => {
    const freshSrv = await startVoloServer({ skipSetup: true });
    try {
      await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
        await page.goto(`${freshSrv.baseUrl}/setup`, { waitUntil: "domcontentloaded" });
        await page.locator(".volo-center-stage.is-setup .volo-wizard-box").waitFor({
          state: "visible",
          timeout: 15000,
        });
        await page.locator('input[name="setup_blog_title"]').waitFor({
          state: "visible",
          timeout: 15000,
        });

        const boot = await readBootConfig(page);
        assert.equal(boot.mode, "setup");
        assert.match(String(boot.version || ""), /^v?\d+\.\d+\.\d+/);

        await page.locator('input[name="setup_blog_title"]').fill("E2E Fresh Studio");
        await page.locator('input[name="setup_blog_subtitle"]').fill("Fresh setup subtitle");
        await page.locator('input[name="setup_public_url"]').fill(freshSrv.baseUrl);
        await page.locator(".volo-accent-dot-btn").nth(1).click();
        await page.locator('input[name="setup_owner_handle"]').fill("freshadmin");
        await page.locator('input[name="setup_owner_nick"]').fill("Fresh Owner");
        await page.locator('input[name="setup_owner_email"]').fill("fresh@e2e.local");
        await page.locator('input[name="setup_owner_new_pwd"]').fill("FreshPass!123");
        await page.locator('input[name="setup_owner_confirm_pwd"]').fill("FreshPass!123");

        diag.clear();
        await page.locator('form button[type="submit"].volo-btn.volo-btn-primary').click();
        await page.waitForURL(/\/console/, { timeout: 15000 });
        await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
        await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });

        const railVer = (await page.locator(".volo-rail-footer").textContent()) || "";
        assert.ok(railVer.includes(boot.version), `Expected "${railVer}" to include "${boot.version}"`);

        await checkNoHorizontalOverflow(page);
        diag.assertClean();
      });
    } finally {
      await freshSrv.stop();
    }
  });

  it("LoginView version display, staff login, sidebar .volo-rail-footer & navigation", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await page.goto(`${srv.baseUrl}/console`, { waitUntil: "domcontentloaded" });
      await page.locator(".volo-auth-version").waitFor({ state: "visible", timeout: 15000 });

      const boot = await readBootConfig(page);
      assert.match(String(boot.version || ""), /^v?\d+\.\d+\.\d+/);
      const authVer = ((await page.locator(".volo-auth-version").textContent()) || "").trim();
      assert.equal(authVer, `VOLO ${boot.version}`);

      await checkNoHorizontalOverflow(page);

      await page.locator('input[name="volo_staff_ident"]').fill(srv.adminCreds.username);
      await page.locator('input[name="volo_staff_secret"]').fill(srv.adminCreds.password);
      diag.clear();
      await page.locator('button[type="submit"].volo-btn.volo-btn-primary.volo-auth-submit').click();

      await page.locator(".volo-shell").waitFor({ state: "visible", timeout: 15000 });
      await page.locator(".volo-rail-footer").waitFor({ state: "visible", timeout: 15000 });
      const railFooter = (await page.locator(".volo-rail-footer").textContent()) || "";
      assert.ok(railFooter.includes(`VOLO ${boot.version}`));

      await page.locator("nav.volo-nav-scroll button.volo-nav-btn", { hasText: /About|关于/ }).click();
      await page.waitForFunction(() => location.hash === "#about");
      await page.locator(".volo-about-hero").waitFor({ state: "visible", timeout: 10000 });

      await page
        .locator("nav.volo-nav-scroll button.volo-nav-group-btn", { hasText: /General Settings|通用设置/ })
        .click();
      await page
        .locator("nav.volo-nav-scroll button.volo-nav-btn", { hasText: /Site & Reading|站点与阅读偏好/ })
        .click();
      await page.waitForFunction(() => location.hash === "#settings_site");
      await page.locator('input[name="site_main_title"]').waitFor({ state: "visible", timeout: 10000 });

      await checkNoHorizontalOverflow(page);
      diag.assertNoConsoleErrors();
    });
  });

  it("About workspace (#about) telemetry fields & Refresh button", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      const infoPromise = page.waitForResponse(
        (res) => res.url().includes("/api/admin/about/project-info") && res.status() === 200,
      );
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#about");
      const infoRes = await infoPromise;
      const info = await infoRes.json();

      await page.locator(".volo-about-hero .volo-badge").waitFor({ state: "visible", timeout: 10000 });
      const badgeText = (await page.locator(".volo-about-hero .volo-badge").textContent()) || "";
      assert.ok(badgeText.includes(info.version), `Expected badge "${badgeText}" to include "${info.version}"`);

      const statusPill = page.locator(".volo-status-pill").first();
      await statusPill.waitFor({ state: "visible" });
      if (info.online) {
        assert.ok((await statusPill.getAttribute("class"))?.includes("ok"));
      }

      const repoCardText = (await page.locator(".volo-grid-2 .volo-card").first().textContent()) || "";
      assert.ok(repoCardText.includes(info.repo.full_name));
      assert.ok(info.admin_gzip_kb > 0);
      assert.ok(repoCardText.includes(`${info.admin_gzip_kb} KB (gzip)`));

      const releaseChip = (await page.locator(".volo-subcard .volo-code-chip").first().textContent()) || "";
      assert.ok(releaseChip.includes(info.latest_release?.tag_name || info.version));

      const refreshPromise = page.waitForResponse(
        (res) => res.url().includes("/api/admin/about/project-info?refresh=1") && res.status() === 200,
      );
      await page.locator(".volo-card .volo-form-actions button.volo-btn.volo-btn-sm").first().click();
      await refreshPromise;
      await page.locator(".volo-toast.is-ok").waitFor({ state: "visible", timeout: 10000 });

      await checkNotClipped(page, ".volo-about-hero .volo-badge");
      await checkNoHorizontalOverflow(page);
      diag.assertNoConsoleErrors();
    });
  });

  it("About workspace transient API failure shows persistent error banner & Retry recovers", async () => {
    await withPage(
      browser,
      {
        width: 1280,
        height: 800,
        ignoreConsolePatterns: [/500/, /Internal Server Error/, /Failed to load resource/],
      },
      async (page, diag) => {
        let failedOnce = false;
        await page.route("**/api/admin/about/project-info*", async (route) => {
          if (!failedOnce) {
            failedOnce = true;
            await route.fulfill({
              status: 500,
              contentType: "application/json",
              body: JSON.stringify({ error: "upstream timeout" }),
            });
            return;
          }
          await route.continue();
        });

        await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#about");

        const alertBanner = page.locator('[role="alert"].volo-draft-banner');
        await alertBanner.waitFor({ state: "visible", timeout: 10000 });
        const retryBtn = alertBanner.locator("button.volo-btn.volo-btn-sm");
        await retryBtn.waitFor({ state: "visible" });

        const recoveryPromise = page.waitForResponse(
          (res) => res.url().includes("/api/admin/about/project-info") && res.status() === 200,
        );
        await retryBtn.click();
        const recRes = await recoveryPromise;
        const info = await recRes.json();

        await alertBanner.waitFor({ state: "hidden", timeout: 10000 });
        const badgeText = (await page.locator(".volo-about-hero .volo-badge").textContent()) || "";
        assert.ok(badgeText.includes(info.version));
        const kvText = (await page.locator(".volo-about-kv-grid").textContent()) || "";
        assert.ok(kvText.includes(`${info.admin_gzip_kb} KB (gzip)`));

        diag.assertClean();
      },
    );
  });

  it("Settings UI (#settings_site) save & browser reload persistence", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#settings_site");

      const titleInput = page.locator('input[name="site_main_title"]');
      const subInput = page.locator('input[name="site_sub_tagline"]');
      await titleInput.waitFor({ state: "visible", timeout: 10000 });
      await page.waitForFunction(() => {
        const el = document.querySelector('input[name="site_main_title"]');
        return el && el.value.length > 0;
      });

      await titleInput.fill("Volo E2E Site Title");
      await subInput.fill("E2E Tagline Persisted");

      const siteForm = page.locator("form.volo-card").filter({ has: titleInput });
      const patchPromise = page.waitForResponse(
        (res) =>
          res.url().includes("/api/admin/settings/site") &&
          res.request().method() === "PATCH" &&
          res.status() === 200,
      );
      await siteForm.locator('button[type="submit"].volo-btn.volo-btn-primary').click();
      await patchPromise;
      await page.locator(".volo-toast.is-ok").waitFor({ state: "visible", timeout: 10000 });

      await page.reload({ waitUntil: "domcontentloaded" });
      await titleInput.waitFor({ state: "visible", timeout: 10000 });
      await page.waitForFunction(() => {
        const el = document.querySelector('input[name="site_main_title"]');
        return el && el.value === "Volo E2E Site Title";
      });
      assert.equal(await titleInput.inputValue(), "Volo E2E Site Title");
      assert.equal(await subInput.inputValue(), "E2E Tagline Persisted");

      const anon = createClient(srv.baseUrl);
      const homeRes = await anon.get("/");
      assertStatus(homeRes, 200);
      assert.ok(homeRes.text.includes("Volo E2E Site Title"));

      diag.assertNoConsoleErrors();
    });
  });

  it("CM6 editor (#editor): ir/wysiwyg/sv mode switching, preview, publish & public /posts/{slug}", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor");

      await page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 });
      const titleInput = page.locator("input.volo-editor-title-input");
      await titleInput.fill("E2E CM6 Full Flow");

      const slugInput = page
        .locator(".volo-field")
        .filter({ has: page.locator("label", { hasText: /Slug/i }) })
        .locator("input.volo-input");
      await slugInput.fill("e2e-cm6-full-flow");

      const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
      assert.equal(await modeTabs.nth(0).getAttribute("aria-selected"), "true");
      await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });

      const cmContent = page.locator(".volo-cm-host .cm-editor .cm-content");
      await cmContent.click();
      await page.keyboard.insertText(
        '# CM6 Heading\n\nHello **Playwright**!\n\n```go\nfunc hello() {}\n```\n',
      );

      const svPreviewPromise = page.waitForResponse(
        (res) => res.url().includes("/api/admin/render/preview") && res.status() === 200,
      );
      await modeTabs.nth(2).click();
      await svPreviewPromise;
      assert.equal(await modeTabs.nth(2).getAttribute("aria-selected"), "true");
      await page.locator(".volo-ed-body.mode-sv").waitFor({ state: "visible" });
      assert.equal(
        await page.evaluate(() => localStorage.getItem("volo_editor_mode")),
        "sv",
      );

      const svPane = page.locator(".volo-ed-body.mode-sv .volo-preview-pane");
      await svPane.locator("h1", { hasText: "CM6 Heading" }).waitFor({ state: "visible", timeout: 10000 });
      assert.ok(((await svPane.locator("strong").textContent()) || "").includes("Playwright"));

      await modeTabs.nth(1).click();
      assert.equal(await modeTabs.nth(1).getAttribute("aria-selected"), "true");
      await page.locator(".volo-ed-body.mode-wysiwyg").waitFor({ state: "visible" });
      const wysiwygSurface = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
      await wysiwygSurface.waitFor({ state: "visible", timeout: 10000 });
      await page.waitForFunction(() => {
        const el = document.querySelector(".volo-wysiwyg-surface");
        return el && el.textContent && el.textContent.includes("CM6 Heading");
      });
      const cmHostDisplay = await page
        .locator(".volo-cm-host")
        .evaluate((el) => getComputedStyle(el).display);
      assert.equal(cmHostDisplay, "none");

      await modeTabs.nth(0).click();
      await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
      assert.ok(((await cmContent.textContent()) || "").includes("CM6 Heading"));

      const modalPreviewPromise = page.waitForResponse(
        (res) => res.url().includes("/api/admin/render/preview") && res.status() === 200,
      );
      await page.locator(".volo-card-head button.volo-btn.volo-btn-sm", { hasText: /Preview|预览/ }).click();
      await modalPreviewPromise;
      const modalPane = page.locator(".volo-modal-backdrop .volo-preview-pane");
      await modalPane.waitFor({ state: "visible", timeout: 10000 });
      assert.ok(((await modalPane.textContent()) || "").includes("CM6 Heading"));
      await page.keyboard.press("Escape");
      await modalPane.waitFor({ state: "hidden", timeout: 10000 });

      const savePromise = page.waitForResponse(
        (res) =>
          res.url().includes("/api/admin/articles") &&
          res.request().method() === "POST" &&
          (res.status() === 200 || res.status() === 201),
      );
      await page
        .locator(".volo-card-head button.volo-btn.volo-btn-sm.volo-btn-primary", {
          hasText: /Publish|发布文章/,
        })
        .click();
      const saveRes = await savePromise;
      const savedData = await saveRes.json();
      assert.equal(savedData.article.status, "published");
      assert.equal(savedData.article.slug, "e2e-cm6-full-flow");

      await page.goto(`${srv.baseUrl}/posts/e2e-cm6-full-flow`, { waitUntil: "domcontentloaded" });
      const bodyText = (await page.locator("body").textContent()) || "";
      assert.ok(bodyText.includes("CM6 Heading"));
      assert.ok(bodyText.includes("Playwright"));
      assert.ok(bodyText.includes("func hello()"));
      await checkNoHorizontalOverflow(page);

      diag.assertNoConsoleErrors();
    });
  });

  it("CM6 editor (#editor): strips unsafe javascript: links on WYSIWYG paste and includes synced code languages", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor");
      await page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 });

      const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
      await modeTabs.nth(1).click();
      const wysiwygSurface = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
      await wysiwygSurface.waitFor({ state: "visible", timeout: 10000 });
      await wysiwygSurface.click();

      await page.evaluate(() => {
        const el = document.querySelector('.volo-wysiwyg-surface[contenteditable="true"]');
        if (!el) throw new Error("missing wysiwyg surface");
        const dt = new DataTransfer();
        dt.setData(
          "text/html",
          '<p>SafeText <a href="javascript:alert(1)">EvilLink</a> <img src="javascript:alert(2)" alt="EvilImg"></p>',
        );
        dt.setData("text/plain", "SafeText EvilLink");
        const evt = new ClipboardEvent("paste", {
          bubbles: true,
          cancelable: true,
          clipboardData: dt,
        });
        el.dispatchEvent(evt);
      });

      await page.waitForTimeout(250);
      await modeTabs.nth(0).click();
      await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
      const cmText = (await page.locator(".volo-cm-host .cm-editor .cm-content").textContent()) || "";
      assert.ok(cmText.includes("SafeText"), `Expected pasted SafeText in editor: ${cmText}`);
      assert.ok(!/javascript\s*:/i.test(cmText), `Unsafe javascript: URL leaked into editor: ${cmText}`);

      const codeBtn = page.locator(
        '.volo-ed-toolbar button[aria-label="Code Block"], .volo-ed-toolbar button[aria-label="代码块"]',
      );
      await codeBtn.waitFor({ state: "visible", timeout: 5000 });
      await codeBtn.click();
      const popMenu = page.locator(".volo-ed-pop");
      await popMenu.waitFor({ state: "visible", timeout: 5000 });
      const popText = (await popMenu.textContent()) || "";
      assert.ok(
        popText.includes("Dockerfile") && popText.includes("Haskell") && popText.includes("Prisma"),
        `Expected synced code languages (Dockerfile, Haskell, Prisma) in popup: ${popText}`,
      );

      diag.assertNoConsoleErrors();
    });
  });

  it("CM6 editor (#editor): prevents IR -> WYSIWYG hydration wipe race under delayed preview", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor");
      await page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 });

      const cmContent = page.locator(".volo-cm-host .cm-editor .cm-content");
      await cmContent.click();
      await page.keyboard.type("Alpha Beta");
      const irBefore = (await cmContent.textContent()) || "";
      assert.ok(irBefore.includes("Alpha Beta"), `Expected initial IR text: ${irBefore}`);

      // Delay preview endpoint 300ms to force an active hydration window
      await page.route("**/api/*/render/preview", async (route) => {
        await new Promise((r) => setTimeout(r, 300));
        await route.continue();
      });

      const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
      await modeTabs.nth(1).click();
      const surface = page.locator(".volo-wysiwyg-surface");
      await surface.waitFor({ state: "visible", timeout: 5000 });

      // Attempt immediate keypress & paste while hydrating
      await page.keyboard.type("!");
      await page.evaluate(() => {
        const el = document.querySelector(".volo-wysiwyg-surface");
        if (!el) return;
        const dt = new DataTransfer();
        dt.setData("text/plain", "WIPE");
        el.dispatchEvent(new ClipboardEvent("paste", { bubbles: true, cancelable: true, clipboardData: dt }));
      });

      await page.locator('.volo-wysiwyg-surface[contenteditable="true"]').waitFor({ state: "visible", timeout: 10000 });
      await page.unroute("**/api/*/render/preview");

      const hydratedText = (await surface.textContent()) || "";
      assert.ok(hydratedText.includes("Alpha Beta"), `Expected hydrated WYSIWYG text to preserve Alpha Beta: ${hydratedText}`);

      await modeTabs.nth(0).click();
      await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
      const irAfter = (await cmContent.textContent()) || "";
      assert.ok(irAfter.includes("Alpha Beta"), `IR content wiped during hydration race: ${irAfter}`);

      diag.assertNoConsoleErrors();
    });
  });

  it("CM6 editor (#editor): WYSIWYG Control+Z / Control+Shift+Z undo-redo roundtrips to IR", async () => {
    await withPage(browser, { width: 1280, height: 800 }, async (page, diag) => {
      await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#editor");
      await page.locator(".volo-studio-editor").waitFor({ state: "visible", timeout: 15000 });

      const cmContent = page.locator(".volo-cm-host .cm-editor .cm-content");
      await cmContent.click();
      await page.keyboard.type("BaseLine");

      const modeTabs = page.locator('.volo-ed-modes[role="tablist"] button.volo-ed-mode-btn[role="tab"]');
      await modeTabs.nth(1).click();
      const wysiwygSurface = page.locator('.volo-wysiwyg-surface[contenteditable="true"]');
      await wysiwygSurface.waitFor({ state: "visible", timeout: 10000 });
      await wysiwygSurface.click();

      // Wait >400ms so pushWysiwygHistory records a separate undo frame instead of coalescing with initial hydration
      await page.waitForTimeout(450);
      await page.keyboard.press("End");
      await page.keyboard.type(" UndoTarget");
      await page.waitForTimeout(100);
      assert.ok(((await wysiwygSurface.textContent()) || "").includes("UndoTarget"));

      await page.keyboard.press("Control+z");
      await page.waitForTimeout(100);
      const afterUndo = (await wysiwygSurface.textContent()) || "";
      assert.ok(!afterUndo.includes("UndoTarget"), `Expected Control+Z to undo UndoTarget, got: ${afterUndo}`);
      assert.ok(afterUndo.includes("BaseLine"), `Expected BaseLine to remain after undo, got: ${afterUndo}`);

      await page.keyboard.press("Control+Shift+z");
      await page.waitForTimeout(100);
      const afterRedo = (await wysiwygSurface.textContent()) || "";
      assert.ok(afterRedo.includes("UndoTarget"), `Expected Control+Shift+Z to redo UndoTarget, got: ${afterRedo}`);

      await modeTabs.nth(0).click();
      await page.locator(".volo-ed-body.mode-ir").waitFor({ state: "visible" });
      const irText = (await cmContent.textContent()) || "";
      assert.ok(irText.includes("BaseLine") && irText.includes("UndoTarget"), `Expected IR roundtrip text: ${irText}`);

      diag.assertNoConsoleErrors();
    });
  });

  it("AboutHelpWorkspace (#about): shows error banner on /api/admin/about/project-info 500 and recovers on Retry", async () => {
    await withPage(
      browser,
      { width: 1280, height: 800, ignoreConsolePatterns: [/status of 500|Failed to load resource/i] },
      async (page, diag) => {
        await loginConsoleViaUI(page, diag, srv.baseUrl, srv.adminCreds, "#overview");

        await page.route("**/api/admin/about/project-info*", async (route) => {
          await route.fulfill({
            status: 500,
            contentType: "application/json",
            body: JSON.stringify({ error: "simulated project-info failure" }),
          });
        });

        await page.locator("nav.volo-nav-scroll button.volo-nav-btn", { hasText: /About|关于/ }).click();
        await page.waitForFunction(() => location.hash === "#about");
        await page.locator(".volo-about-hero").waitFor({ state: "visible", timeout: 10000 });

        const errBanner = page.locator('.volo-draft-banner[role="alert"]');
        await errBanner.waitFor({ state: "visible", timeout: 10000 });
        const retryBtn = errBanner.locator("button");
        await retryBtn.waitFor({ state: "visible", timeout: 5000 });

        await page.unroute("**/api/admin/about/project-info*");
        const recoveryPromise = page.waitForResponse(
          (res) => res.url().includes("/api/admin/about/project-info") && res.status() === 200,
        );
        await retryBtn.click();
        await recoveryPromise;
        await errBanner.waitFor({ state: "hidden", timeout: 10000 });

        const aboutText = (await page.locator(".volo-about-kv-grid").textContent()) || "";
        assert.ok(aboutText.includes("KB") || aboutText.includes("go1."), `Expected recovered system info: ${aboutText}`);

        diag.assertNoConsoleErrors();
      },
    );
  });
});
