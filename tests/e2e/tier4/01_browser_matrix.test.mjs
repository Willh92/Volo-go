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

const THEMES = [
  { id: "volo-studio", skinAttr: "studio", normalMax: 1140 },
  { id: "volo-journal", skinAttr: "volo-journal", normalMax: 1180 },
];

const MODES = ["light", "dark"];

const VIEWPORTS = [
  { name: "mobile-375", width: 375, height: 812 },
  { name: "tablet-768", width: 768, height: 1024 },
  { name: "desktop-1280", width: 1280, height: 800 },
  { name: "wide-1440", width: 1440, height: 900 },
  { name: "wide-1920", width: 1920, height: 1080 },
];

const LONG_UNBROKEN_TOKEN = "VoloMatrixUnbrokenIdentifier_".repeat(6) + "End";

const RICH_MARKDOWN = [
  "## Overview",
  "",
  "This matrix article validates layout alignment, wide tables, code wrapping/scrolling, and interactive drawers across viewports and themes.",
  "",
  "### Architecture",
  "",
  "Nested section content for Table of Contents verification.",
  "",
  "## Wide Table",
  "",
  "| ColumnOne | ColumnTwo | ColumnThree | ColumnFour | ColumnFive | ColumnSix | ColumnSeven | ColumnEight |",
  "| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |",
  "| AlphaValue | BetaValue | GammaValue | DeltaValue | EpsilonValue | ZetaValue | EtaValue | ThetaLongestCellValueHere |",
  "| RowTwoA | RowTwoB | RowTwoC | RowTwoD | RowTwoE | RowTwoF | RowTwoG | RowTwoH |",
  "",
  "## Long Code Block",
  "",
  "```go",
  "package matrix",
  "",
  `const LongUnbroken = "${LONG_UNBROKEN_TOKEN}"`,
  "",
  "func RenderMatrixSample() string {",
  "\treturn LongUnbroken",
  "}",
  "```",
].join("\n");

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

async function assertPluginsHealthy(admin, expectedIds, contextMsg) {
  const res = await admin.get("/api/admin/plugins");
  assertStatus(res, 200, `GET /api/admin/plugins (${contextMsg})`);
  const items = res.data?.items || res.data?.plugins || [];
  for (const id of expectedIds) {
    const p = items.find((item) => item.id === id);
    assert.ok(p, `${contextMsg}: expected plugin "${id}" in /api/admin/plugins`);
    assert.equal(
      p.is_enabled,
      true,
      `${contextMsg}: plugin "${id}" disabled (circuit_state=${p.circuit_state}, failure_count=${p.failure_count}, last_error=${p.last_error})`,
    );
    assert.equal(
      p.circuit_state,
      "closed",
      `${contextMsg}: plugin "${id}" circuit_state="${p.circuit_state}" (failure_count=${p.failure_count}, last_error=${p.last_error})`,
    );
  }
}

async function waitForPageReady(page, expectedSkin, expectedTheme) {
  await page.locator("#studio-main").waitFor({ state: "visible", timeout: 15000 });
  await page.waitForFunction(
    ({ skin, mode }) => {
      const root = document.documentElement;
      return (
        root.getAttribute("data-volo-skin") === skin &&
        root.getAttribute("data-theme") === mode
      );
    },
    { skin: expectedSkin, mode: expectedTheme },
    { timeout: 10000 },
  );
  await page.evaluate(
    () =>
      new Promise((resolve) =>
        requestAnimationFrame(() => requestAnimationFrame(resolve)),
      ),
  );
}

async function getMainWidth(page) {
  return page.evaluate(() => {
    const el = document.querySelector("#studio-main");
    return el ? el.getBoundingClientRect().width : 0;
  });
}

async function getArticleTemplateWidths(page, expectedTemplate) {
  return page.evaluate((tpl) => {
    const layout = document.querySelector(".studio-layout--article");
    const article = document.querySelector(`article[data-page-template="${tpl}"]`);
    return {
      layoutWidth: layout ? layout.getBoundingClientRect().width : 0,
      articleWidth: article ? article.getBoundingClientRect().width : 0,
    };
  }, expectedTemplate);
}

async function waitForAnimations(page, selector) {
  await page.locator(selector).evaluate(async (el) => {
    await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
    if (typeof el.getAnimations === "function") {
      await Promise.all(el.getAnimations().map((a) => a.finished.catch(() => {})));
    }
  });
}

describe("Tier 4 — Real-Browser Viewport & Theme Matrix (S2, S8)", () => {
  let srv;
  let admin;
  let browser;
  const createdNavIds = [];

  before(async () => {
    srv = await startVoloServer({ defaultLocale: "en-US" });
    admin = createClient(srv.baseUrl);
    assertStatus(
      await admin.login(srv.adminCreds.username, srv.adminCreds.password),
      200,
      "Admin login",
    );

    assertStatus(
      await admin.patch("/api/admin/settings/reading", {
        show_code_line_numbers: true,
      }),
      200,
      "Enable code line numbers",
    );

    const catIds = [];
    for (const [idx, name] of ["Systems", "Design", "Guides"].entries()) {
      const cRes = await admin.post("/api/admin/categories", {
        name,
        slug: `matrix-cat-${name.toLowerCase()}`,
        description: `${name} category description`,
        sort_order: idx + 1,
      });
      assertStatus(cRes, 201, `Create category ${name}`);
      catIds.push(cRes.data.category.id);
    }

    for (let i = 1; i <= 4; i += 1) {
      const lRes = await admin.post("/api/admin/links", {
        title: `Matrix Friend ${i}`,
        url: `https://example.com/matrix-friend-${i}`,
        description: `Matrix link description ${i}`,
        category_name: i <= 2 ? "Engineering" : "References",
      });
      assertStatus(lRes, 201, `Create link ${i}`);
    }

    const richArtRes = await admin.post("/api/admin/articles", {
      title: "Matrix Rich Technical Article",
      slug: "matrix-rich-article",
      summary_md: "Rich matrix test summary.",
      content_md: RICH_MARKDOWN,
      status: "published",
      category_id: catIds[0],
      allow_comment: true,
      tags: ["matrix", "layout", "code"],
    });
    assertStatus(richArtRes, 201, "Create matrix-rich-article");

    const secondArtRes = await admin.post("/api/admin/articles", {
      title: "Matrix Secondary Article",
      slug: "matrix-secondary-article",
      summary_md: "Secondary article for home listing.",
      content_md: "# Secondary Heading\n\nBody copy for matrix home feed.",
      status: "published",
      category_id: catIds[1],
      tags: ["matrix"],
    });
    assertStatus(secondArtRes, 201, "Create matrix-secondary-article");

    const defaultPageRes = await admin.post("/api/admin/pages", {
      title: "Matrix Default Standalone Page",
      slug: "matrix-default-page",
      template: "default",
      show_toc: true,
      is_published: true,
      allow_comment: false,
      content_md: RICH_MARKDOWN,
    });
    assertStatus(defaultPageRes, 201, "Create default standalone page");

    const widePageRes = await admin.post("/api/admin/pages", {
      title: "Matrix Wide Standalone Page",
      slug: "matrix-wide-page",
      template: "wide",
      show_toc: true,
      is_published: true,
      allow_comment: false,
      content_md: RICH_MARKDOWN,
    });
    assertStatus(widePageRes, 201, "Create wide standalone page");

    const bookChapters = [
      { title: "Volume I — Foundations", url: "", depth: 0 },
      { title: "Chapter 1: Rich Reference", url: "/posts/matrix-rich-article", depth: 1 },
      { title: "Chapter 2: Secondary Rules", url: "/posts/matrix-secondary-article", depth: 1 },
      { title: "Volume II — Verification", url: "", depth: 0 },
      { title: "Chapter 3: Wide Screens", url: "/posts/matrix-rich-article", depth: 1 },
    ];
    const bookRes = await admin.post("/api/admin/books", {
      title: "Matrix Verification Book",
      slug: "matrix-book",
      description: "Multi-volume book for matrix inspection.",
      source_type: "manual",
      embed_mode: "inline",
      reading_status: "reading",
      chapters: bookChapters,
    });
    assertStatus(bookRes, 201, "Create matrix-book");

    for (let i = 1; i <= 4; i += 1) {
      const navRes = await admin.post("/api/admin/nav", {
        title: `MatrixNav${i}`,
        url: `/matrix-nav-${i}`,
        is_enabled: true,
        sort_order: 20 + i,
      });
      assertStatus(navRes, 201, `Create nav item ${i}`);
      createdNavIds.push(navRes.data.item.id);
    }

    const headerNavItems = [
      "home",
      "archives",
      "categories",
      "tags",
      "books",
      "links",
      ...createdNavIds.map((id) => `custom:${id}`),
    ];

    for (const themeId of ["volo-studio", "volo-journal"]) {
      assertStatus(
        await admin.put(`/api/admin/themes/${themeId}/config`, {
          header_nav_items: headerNavItems,
          header_nav_primary_count: 4,
        }),
        200,
        `Configure header_nav_items for ${themeId}`,
      );
    }

    const headerPluginZip = buildStoreZip([
      {
        name: "manifest.json",
        data: JSON.stringify({
          id: "e2e-header-widget",
          version: "1.0.0",
          min_host_version: "0.1.0",
          runtime: "goja",
          entry: "main.js",
          name: { "en-US": "E2E Header Widget" },
          permissions: ["slots:public.header"],
          ui_slots: [
            {
              slot: "public.header",
              dynamic: true,
            },
          ],
        }),
      },
      {
        name: "main.js",
        data: [
          "function renderSlot(slot) {",
          '  if (slot === "public.header") {',
          '    return \'<div class="studio-card-pad" style="padding:8px 14px;border:1px solid var(--volo-line);border-radius:10px;background:var(--volo-surface);">E2E Matrix Header Banner</div>\';',
          "  }",
          '  return "";',
          "}",
          "module.exports = { renderSlot: renderSlot };",
          "",
        ].join("\n"),
      },
    ]);
    const pluginForm = new FormData();
    pluginForm.append(
      "file",
      new Blob([headerPluginZip], { type: "application/zip" }),
      "e2e-header-widget.zip",
    );
    const instRes = await admin.post("/api/admin/plugins/install?enable=1", pluginForm);
    assertStatus(instRes, 201, "Install synthetic public.header plugin");

    assertStatus(
      await admin.post("/api/admin/plugins/kanbanniang/enable"),
      200,
      "Enable kanbanniang",
    );
    assertStatus(
      await admin.patch("/api/admin/plugins/kanbanniang/config", {
        position: "right",
        model: "Potion-Maker/Pio",
        hide_on_mobile: true,
      }),
      200,
      "Configure kanbanniang right",
    );
    await assertPluginsHealthy(admin, ["kanbanniang", "e2e-header-widget"], "initial setup");

    assertStatus(
      await admin.patch("/api/admin/settings/appearance", {
        default_mode: "system",
        accent: "forest",
      }),
      200,
      "Set appearance default_mode=system",
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

  for (const theme of THEMES) {
    describe(`Theme ${theme.id} across 5 viewports x 2 color modes`, () => {
      before(async () => {
        assertStatus(
          await admin.post("/api/admin/themes/activate", {
            theme_id: theme.id,
            target: "both",
          }),
          200,
          `Activate ${theme.id}`,
        );
      });

      for (const mode of MODES) {
        for (const vp of VIEWPORTS) {
          it(`${theme.id} | ${mode} | ${vp.name} (${vp.width}x${vp.height}) renders cleanly without overflow, clipping, or overlap`, async () => {
            await withPage(
              browser,
              {
                width: vp.width,
                height: vp.height,
                colorScheme: mode,
                failOnConsoleError: true,
              },
              async (page, diag) => {
                const routesToCheck = [
                  "/",
                  "/posts/matrix-rich-article",
                  "/matrix-default-page",
                  "/matrix-wide-page",
                  "/categories",
                  "/links",
                  "/books/matrix-book",
                ];

                let normalMainWidth = 0;
                let wideMainWidth = 0;
                let defaultTplWidths = null;
                let wideTplWidths = null;

                for (const route of routesToCheck) {
                  await page.goto(`${srv.baseUrl}${route}`, {
                    waitUntil: "domcontentloaded",
                  });
                  await waitForPageReady(page, theme.skinAttr, mode);

                  await checkNoHorizontalOverflow(page);
                  await checkNotClipped(page, "#volo-theme-btn");
                  await checkNotClipped(page, ".studio-brand");
                  await page
                    .locator(".studio-header-widgets")
                    .waitFor({ state: "visible", timeout: 5000 });
                  await checkNotClipped(page, ".studio-header-widgets");

                  if (vp.width <= 760) {
                    await page
                      .locator("#volo-nav-more-btn")
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, "#volo-nav-more-btn");
                    await page
                      .locator(".studio-nav__link")
                      .first()
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, ".studio-nav__link");
                  } else {
                    await page
                      .locator(".studio-nav__link")
                      .first()
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, ".studio-nav__link", {
                      all: true,
                    });
                    await page
                      .locator("#volo-nav-more-btn")
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, "#volo-nav-more-btn");
                  }

                  if (
                    route === "/posts/matrix-rich-article" ||
                    route === "/matrix-default-page" ||
                    route === "/matrix-wide-page"
                  ) {
                    await page
                      .locator(".volo-copy-btn")
                      .first()
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, ".volo-copy-btn");

                    if (theme.id === "volo-studio" && vp.width >= 1280) {
                      await page
                        .locator(".volo-toc-card")
                        .waitFor({ state: "visible", timeout: 5000 });
                      await checkNotClipped(page, ".volo-toc-card");
                    } else {
                      await page
                        .locator("#volo-mobile-toc-btn:not([hidden])")
                        .waitFor({ state: "visible", timeout: 5000 });
                      await checkNotClipped(page, "#volo-mobile-toc-btn");
                    }
                  }

                  if (route === "/books/matrix-book") {
                    await page
                      .locator(".book-reader-bar__left")
                      .waitFor({ state: "visible", timeout: 5000 });
                    await checkNotClipped(page, ".book-reader-bar__left");
                    if (vp.width >= 1024) {
                      await page
                        .locator(".book-reader-toc")
                        .waitFor({ state: "visible", timeout: 5000 });
                      await checkNotClipped(page, ".book-reader-toc");
                    }
                  }

                  if (
                    vp.width >= 1440 &&
                    (route === "/" ||
                      route === "/matrix-default-page" ||
                      route === "/matrix-wide-page")
                  ) {
                    await checkElementsAlignedX(
                      page,
                      [
                        ".studio-header__inner",
                        ".studio-header-widgets",
                        "#studio-main",
                        ".studio-footer",
                      ],
                      {
                        mode: "bounds",
                        tolerance: 2,
                        requireAll: true,
                      },
                    );
                    if (route === "/") {
                      normalMainWidth = await getMainWidth(page);
                    } else if (route === "/matrix-default-page") {
                      defaultTplWidths = await getArticleTemplateWidths(page, "default");
                    } else if (route === "/matrix-wide-page") {
                      wideMainWidth = await getMainWidth(page);
                      wideTplWidths = await getArticleTemplateWidths(page, "wide");
                    }
                  }

                  if (route === "/posts/matrix-rich-article") {
                    if (vp.width <= 768) {
                      const mobileTocBtn = page.locator("#volo-mobile-toc-btn:not([hidden])");
                      await mobileTocBtn.waitFor({ state: "visible", timeout: 10000 });
                      await checkNotClipped(page, "#volo-mobile-toc-btn");

                      await mobileTocBtn.click();
                      await page
                        .locator("#volo-toc-drawer.is-open:not([hidden]) .volo-toc-drawer__panel")
                        .waitFor({ state: "visible", timeout: 10000 });
                      await waitForAnimations(page, ".volo-toc-drawer__panel");
                      await checkNotClipped(page, ".volo-toc-drawer__panel");
                      await checkNoHorizontalOverflow(page);

                      await page
                        .locator("#volo-toc-drawer [data-toc-close]")
                        .click();
                      await page
                        .locator("#volo-toc-drawer")
                        .waitFor({ state: "hidden", timeout: 10000 });
                    }

                    if (vp.width >= 1280) {
                      const kbnRoot = page.locator("#volo-kanbanniang-root:not([hidden])");
                      try {
                        await kbnRoot.waitFor({ state: "visible", timeout: 15000 });
                      } catch (err) {
                        await assertPluginsHealthy(
                          admin,
                          ["kanbanniang", "e2e-header-widget"],
                          `after kbn timeout on ${theme.id} ${mode} ${vp.name}`,
                        );
                        throw err;
                      }
                      await checkNotClipped(page, "#volo-kanbanniang-root");

                      if (theme.id === "volo-journal") {
                        const tocBtn = page.locator("#volo-mobile-toc-btn:not([hidden])");
                        await tocBtn.waitFor({ state: "visible", timeout: 10000 });

                        await checkNoOverlap(
                          page,
                          "#volo-mobile-toc-btn",
                          "#volo-kanbanniang-root",
                          { requireBothVisible: true },
                        );

                        await tocBtn.click();
                        await page
                          .locator("#volo-toc-drawer.is-open:not([hidden]) .volo-toc-drawer__panel")
                          .waitFor({ state: "visible", timeout: 10000 });
                        await waitForAnimations(page, ".volo-toc-drawer__panel");
                        assert.equal(await kbnRoot.isVisible(), true);

                        await checkNoOverlap(
                          page,
                          ".volo-toc-drawer__panel",
                          ".solo-kanbanniang",
                          { requireBothVisible: true },
                        );
                        await checkNotClipped(page, ".volo-toc-drawer__panel");

                        await page.keyboard.press("Escape");
                        await page
                          .locator("#volo-toc-drawer")
                          .waitFor({ state: "hidden", timeout: 10000 });

                        // Closed-state summon pill coexistence with #volo-mobile-toc-btn & .volo-toc-drawer__panel
                        await kbnRoot.hover();
                        const closeBtn = kbnRoot.locator('[data-kbn-tool="close"]');
                        await closeBtn.waitFor({ state: "visible", timeout: 5000 });
                        await closeBtn.click();

                        const summonPill = page.locator(
                          "#volo-kanbanniang-summon.solo-kanbanniang-summon:not([hidden])",
                        );
                        await summonPill.waitFor({ state: "visible", timeout: 5000 });
                        await checkNotClipped(page, "#volo-kanbanniang-summon");
                        await checkNoOverlap(
                          page,
                          "#volo-mobile-toc-btn",
                          "#volo-kanbanniang-summon",
                          { requireBothVisible: true },
                        );

                        await tocBtn.click();
                        await page
                          .locator("#volo-toc-drawer.is-open:not([hidden]) .volo-toc-drawer__panel")
                          .waitFor({ state: "visible", timeout: 10000 });
                        await waitForAnimations(page, ".volo-toc-drawer__panel");
                        await checkNoOverlap(
                          page,
                          ".volo-toc-drawer__panel",
                          "#volo-kanbanniang-summon",
                          { requireBothVisible: true },
                        );
                        await checkNotClipped(page, ".volo-toc-drawer__panel");

                        await page.keyboard.press("Escape");
                        await page
                          .locator("#volo-toc-drawer")
                          .waitFor({ state: "hidden", timeout: 10000 });

                        await summonPill.click();
                        await kbnRoot.waitFor({ state: "visible", timeout: 10000 });
                      }
                    }
                  }
                }

                if (vp.width >= 1440) {
                  assert.ok(
                    normalMainWidth >= theme.normalMax - 10 &&
                      normalMainWidth <= theme.normalMax + 10,
                    `Expected normal #studio-main width near ${theme.normalMax}px on ${theme.id}, got ${normalMainWidth}`,
                  );
                  assert.ok(
                    wideMainWidth >= 1300 && wideMainWidth > normalMainWidth + 100,
                    `Expected wide page #studio-main (~1320px) to exceed normal (${normalMainWidth}px), got ${wideMainWidth}px`,
                  );

                  assert.ok(
                    defaultTplWidths && defaultTplWidths.articleWidth > 0,
                    `Expected default standalone page widths on ${theme.id}`,
                  );
                  assert.ok(
                    wideTplWidths && wideTplWidths.articleWidth > 0,
                    `Expected wide standalone page widths on ${theme.id}`,
                  );
                  assert.ok(
                    defaultTplWidths.articleWidth <= 855,
                    `Expected default article width <= 855px on ${theme.id}, got ${defaultTplWidths.articleWidth}`,
                  );
                  assert.ok(
                    wideTplWidths.layoutWidth >= 1300 &&
                      wideTplWidths.layoutWidth > defaultTplWidths.layoutWidth + 180,
                    `Expected wide .studio-layout--article (~1320px) to exceed default (${defaultTplWidths.layoutWidth}px) on ${theme.id}, got ${wideTplWidths.layoutWidth}px`,
                  );
                  const expectedMinWideArticle = theme.id === "volo-studio" ? 1040 : 1300;
                  assert.ok(
                    wideTplWidths.articleWidth >= expectedMinWideArticle &&
                      wideTplWidths.articleWidth > defaultTplWidths.articleWidth + 180,
                    `Expected article[data-page-template="wide"] (>= ${expectedMinWideArticle}px) to exceed default (${defaultTplWidths.articleWidth}px) on ${theme.id}, got ${wideTplWidths.articleWidth}px`,
                  );
                }

                diag.assertClean();
              },
            );
          });
        }
      }
    });
  }

  describe("Kanbanniang left-docked, theme toggle persistence & mobile nav menu", () => {
    after(async () => {
      await admin
        .patch("/api/admin/plugins/kanbanniang/config", {
          position: "right",
        })
        .catch(() => {});
      await admin
        .post("/api/admin/themes/activate", {
          theme_id: "volo-studio",
          target: "both",
        })
        .catch(() => {});
    });

    it("renders kanbanniang position='left' (.volo-kbn-left) cleanly in both volo-studio and volo-journal", async () => {
      assertStatus(
        await admin.patch("/api/admin/plugins/kanbanniang/config", {
          position: "left",
          model: "Potion-Maker/Pio",
          hide_on_mobile: true,
        }),
        200,
        "Set kanbanniang position=left",
      );

      for (const theme of THEMES) {
        assertStatus(
          await admin.post("/api/admin/themes/activate", {
            theme_id: theme.id,
            target: "both",
          }),
          200,
          `Activate ${theme.id}`,
        );

        await withPage(
          browser,
          { width: 1440, height: 900, colorScheme: "light", failOnConsoleError: true },
          async (page, diag) => {
            await page.goto(`${srv.baseUrl}/posts/matrix-rich-article`, {
              waitUntil: "domcontentloaded",
            });
            await waitForPageReady(page, theme.skinAttr, "light");

            const kbnLeft = page.locator("#volo-kanbanniang-root.volo-kbn-left:not([hidden])");
            try {
              await kbnLeft.waitFor({ state: "visible", timeout: 15000 });
            } catch (err) {
              await assertPluginsHealthy(
                admin,
                ["kanbanniang", "e2e-header-widget"],
                `after left kbn timeout on ${theme.id}`,
              );
              throw err;
            }
            await checkNoHorizontalOverflow(page);
            await checkNotClipped(page, "#volo-kanbanniang-root.volo-kbn-left");

            if (theme.id === "volo-journal") {
              await page.locator("#volo-mobile-toc-btn:not([hidden])").click();
              await page
                .locator("#volo-toc-drawer:not([hidden]) .volo-toc-drawer__panel")
                .waitFor({ state: "visible", timeout: 10000 });
              await checkNoOverlap(
                page,
                ".volo-toc-drawer__panel",
                ".solo-kanbanniang.volo-kbn-left",
                { requireBothVisible: true },
              );
            }

            diag.assertClean();
          },
        );
      }

      await assertPluginsHealthy(
        admin,
        ["kanbanniang", "e2e-header-widget"],
        "post left-docked check",
      );
    });

    it("toggles light/dark theme mode via #volo-theme-btn -> #volo-theme-menu and persists across reload", async () => {
      assertStatus(
        await admin.post("/api/admin/themes/activate", {
          theme_id: "volo-studio",
          target: "both",
        }),
        200,
      );

      await withPage(
        browser,
        { width: 1280, height: 800, colorScheme: "light", failOnConsoleError: true },
        async (page, diag) => {
          await page.goto(`${srv.baseUrl}/`, { waitUntil: "domcontentloaded" });
          await waitForPageReady(page, "studio", "light");

          const lightColors = await page.evaluate(() => {
            const st = getComputedStyle(document.body);
            return { bg: st.backgroundColor, fg: st.color };
          });

          await page.locator("#volo-theme-btn").click();
          const themeMenu = page.locator("#volo-theme-menu:not([hidden])");
          await themeMenu.waitFor({ state: "visible", timeout: 5000 });
          await themeMenu.locator('button[data-volo-mode="dark"]').click();

          await page.waitForFunction(
            () =>
              document.documentElement.getAttribute("data-theme") === "dark" &&
              localStorage.getItem("volo_theme_mode") === "dark",
            null,
            { timeout: 5000 },
          );
          await page.waitForFunction(
            (prevBg) => getComputedStyle(document.body).backgroundColor !== prevBg,
            lightColors.bg,
            { timeout: 5000 },
          );
          await page.waitForTimeout(220);

          const darkColors = await page.evaluate(() => {
            const st = getComputedStyle(document.body);
            return { bg: st.backgroundColor, fg: st.color };
          });
          assert.notEqual(
            darkColors.bg,
            lightColors.bg,
            "Expected body backgroundColor to change between light and dark modes",
          );
          assert.notEqual(
            darkColors.fg,
            lightColors.fg,
            "Expected body foreground color to change between light and dark modes",
          );

          await page.reload({ waitUntil: "domcontentloaded" });
          await waitForPageReady(page, "studio", "dark");
          assert.equal(
            await page.evaluate(() => localStorage.getItem("volo_theme_mode")),
            "dark",
          );

          diag.assertClean();
        },
      );
    });

    it("opens mobile #volo-nav-more-btn / #volo-nav-more-menu cleanly at 375x812", async () => {
      for (const theme of THEMES) {
        assertStatus(
          await admin.post("/api/admin/themes/activate", {
            theme_id: theme.id,
            target: "both",
          }),
          200,
        );

        await withPage(
          browser,
          { width: 375, height: 812, colorScheme: "light", failOnConsoleError: true },
          async (page, diag) => {
            await page.goto(`${srv.baseUrl}/`, { waitUntil: "domcontentloaded" });
            await waitForPageReady(page, theme.skinAttr, "light");

            const moreBtn = page.locator("#volo-nav-more-btn");
            await moreBtn.waitFor({ state: "visible", timeout: 10000 });
            await checkNotClipped(page, "#volo-nav-more-btn");

            await moreBtn.click();
            const moreMenu = page.locator("#volo-nav-more-menu:not([hidden])");
            await moreMenu.waitFor({ state: "visible", timeout: 5000 });
            const overflowCount = await moreMenu.locator("a.studio-nav-more__item").count();
            assert.ok(
              overflowCount >= 2,
              `Expected >= 2 overflow nav links inside #volo-nav-more-menu, got ${overflowCount}`,
            );
            const menuText = (await moreMenu.textContent()) || "";
            assert.ok(
              menuText.includes("MatrixNav"),
              `Expected custom overflow nav items inside #volo-nav-more-menu, got: ${menuText}`,
            );

            await checkNotClipped(page, "#volo-nav-more-menu");
            await checkNoHorizontalOverflow(page);

            diag.assertClean();
          },
        );
      }
    });
  });
});
