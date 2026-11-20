import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { chromium } from "file:///C:/Users/92wil/AppData/Local/ms-playwright-go/1.50.1/package/index.mjs";

const DEFAULT_EDGE_PATH =
  "C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe";

/**
 * Launch Microsoft Edge via Playwright ESM.
 */
export async function launchEdge(opts = {}) {
  const {
    executablePath: customPath,
    headless = true,
    args = [],
    ...restOpts
  } = opts;

  const executablePath =
    customPath || process.env.VOLO_E2E_BROWSER || DEFAULT_EDGE_PATH;

  if (!existsSync(executablePath)) {
    throw new Error(
      `Edge browser executable not found at "${executablePath}". Set VOLO_E2E_BROWSER to a valid Chromium/Edge binary.`,
    );
  }

  const mergedArgs = ["--no-sandbox", ...args.filter((a) => a !== "--no-sandbox")];

  return chromium.launch({
    executablePath,
    headless,
    args: mergedArgs,
    ...restOpts,
  });
}

/**
 * Launch Edge, pass the Browser instance to fn(browser), and always close it.
 */
export async function withEdge(optsOrFn, maybeFn) {
  let opts = {};
  let fn = optsOrFn;
  if (typeof optsOrFn !== "function") {
    opts = optsOrFn || {};
    fn = maybeFn;
  }
  if (typeof fn !== "function") {
    throw new TypeError("withEdge requires a callback function");
  }

  const browser = await launchEdge(opts);
  try {
    return await fn(browser);
  } finally {
    await browser.close();
  }
}

export function normalizePageOpts(viewportOrOpts) {
  if (!viewportOrOpts || typeof viewportOrOpts !== "object") {
    return {
      viewport: { width: 1280, height: 800 },
      allowPageErrors: false,
      failOnConsoleError: false,
      ignoreConsolePatterns: [],
    };
  }
  const {
    width,
    height,
    viewport,
    allowPageErrors,
    failOnConsoleError,
    ignoreConsolePatterns,
    ...rest
  } = viewportOrOpts;

  const resolvedViewport =
    viewport ||
    (typeof width === "number" && typeof height === "number"
      ? { width, height }
      : { width: 1280, height: 800 });

  return {
    ...rest,
    viewport: resolvedViewport,
    allowPageErrors: Boolean(allowPageErrors),
    failOnConsoleError: Boolean(failOnConsoleError),
    ignoreConsolePatterns: Array.isArray(ignoreConsolePatterns)
      ? ignoreConsolePatterns
      : [],
  };
}

function matchesAnyPattern(text, patterns) {
  for (const pat of patterns) {
    if (pat instanceof RegExp && pat.test(text)) return true;
    if (typeof pat === "string" && text.includes(pat)) return true;
  }
  return false;
}

/**
 * Open an isolated browser context + page, collect pageerror and console errors,
 * run fn(page, diagnostics), and guarantee context cleanup in finally.
 */
export async function withPage(browser, viewportOrOpts, fn) {
  let rawOpts = viewportOrOpts;
  let callback = fn;
  if (typeof viewportOrOpts === "function") {
    rawOpts = {};
    callback = viewportOrOpts;
  }
  if (typeof callback !== "function") {
    throw new TypeError("withPage requires a callback function");
  }

  const {
    viewport,
    colorScheme,
    locale,
    allowPageErrors,
    failOnConsoleError,
    ignoreConsolePatterns,
    ...contextRest
  } = normalizePageOpts(rawOpts);

  delete contextRest.width;
  delete contextRest.height;

  const context = await browser.newContext({
    viewport,
    ...(colorScheme ? { colorScheme } : {}),
    ...(locale ? { locale } : {}),
    ...contextRest,
  });

  const page = await context.newPage();
  const pageErrors = [];
  const consoleErrors = [];

  page.on("pageerror", (err) => {
    pageErrors.push(err?.message || String(err));
  });
  page.on("console", (msg) => {
    if (msg.type() === "error") {
      consoleErrors.push({
        text: msg.text(),
        location: msg.location(),
      });
    }
  });

  const diagnostics = {
    pageErrors,
    consoleErrors,
    clear() {
      pageErrors.length = 0;
      consoleErrors.length = 0;
    },
    assertNoConsoleErrors(extraIgnore = []) {
      const patterns = [...ignoreConsolePatterns, ...extraIgnore];
      const unexpected = consoleErrors.filter(
        (entry) => !matchesAnyPattern(entry.text, patterns),
      );
      if (unexpected.length > 0) {
        assert.fail(
          `Unexpected browser console errors (${unexpected.length}):\n` +
            unexpected
              .map(
                (e) =>
                  `- ${e.text} (${e.location?.url || "unknown"}:${e.location?.lineNumber ?? 0})`,
              )
              .join("\n"),
        );
      }
    },
    assertClean(extraIgnore = []) {
      assert.equal(
        pageErrors.length,
        0,
        `Unexpected uncaught page errors (${pageErrors.length}):\n${pageErrors.join("\n")}`,
      );
      diagnostics.assertNoConsoleErrors(extraIgnore);
    },
  };

  try {
    const result = await callback(page, diagnostics);
    if (!allowPageErrors) {
      assert.equal(
        pageErrors.length,
        0,
        `Unexpected uncaught page errors (${pageErrors.length}):\n${pageErrors.join("\n")}`,
      );
    }
    if (failOnConsoleError) {
      diagnostics.assertNoConsoleErrors();
    }
    return result;
  } finally {
    await context.close();
  }
}

/**
 * Verify that the document has no horizontal overflow beyond tolerance (default 1px).
 */
export async function checkNoHorizontalOverflow(page, opts = {}) {
  const tolerance = opts.tolerance ?? 1;
  const shouldAssert = opts.assert !== false;

  const metrics = await page.evaluate(() => {
    const doc = document.documentElement;
    const body = document.body;
    return {
      docScrollWidth: doc.scrollWidth,
      docClientWidth: doc.clientWidth,
      bodyScrollWidth: body ? body.scrollWidth : 0,
      bodyClientWidth: body ? body.clientWidth : 0,
      innerWidth: window.innerWidth,
    };
  });

  const docOverflow = metrics.docScrollWidth - metrics.docClientWidth;
  const bodyOverflow = metrics.bodyScrollWidth - metrics.bodyClientWidth;
  const ok = docOverflow <= tolerance && bodyOverflow <= tolerance;

  const result = { ok, tolerance, docOverflow, bodyOverflow, ...metrics };
  if (!ok && shouldAssert) {
    assert.fail(
      `Horizontal overflow detected (tolerance=${tolerance}px): ${JSON.stringify(result)}`,
    );
  }
  return result;
}

/**
 * Verify that multiple elements share aligned X geometry (left, right, width, or bounds).
 */
export async function checkElementsAlignedX(page, selectors, opts = {}) {
  assert.ok(
    Array.isArray(selectors) && selectors.length >= 2,
    `checkElementsAlignedX requires an array of at least 2 selectors (got ${Array.isArray(selectors) ? selectors.length : typeof selectors})`,
  );
  const tolerance = opts.tolerance ?? 2;
  const mode = opts.mode ?? "bounds";
  const requireAll = opts.requireAll ?? true;
  const shouldAssert = opts.assert !== false;

  const measurements = await page.evaluate((sels) => {
    return sels.map((selector) => {
      const el = document.querySelector(selector);
      if (!el) {
        return { selector, found: false, visible: false };
      }
      const style = getComputedStyle(el);
      const rect = el.getBoundingClientRect();
      const visible =
        style.display !== "none" &&
        style.visibility !== "hidden" &&
        style.opacity !== "0" &&
        rect.width > 0 &&
        rect.height > 0;
      return {
        selector,
        found: true,
        visible,
        left: rect.left,
        right: rect.right,
        width: rect.width,
        top: rect.top,
        bottom: rect.bottom,
      };
    });
  }, selectors);

  const visibleItems = measurements.filter((m) => m.visible);
  const errors = [];

  if (requireAll) {
    const missing = measurements.filter((m) => !m.visible);
    if (missing.length > 0) {
      errors.push(
        `Missing or hidden elements: ${missing.map((m) => m.selector).join(", ")}`,
      );
    }
  } else if (visibleItems.length < 2) {
    errors.push(
      `Expected at least 2 visible elements, found ${visibleItems.length}`,
    );
  }

  let spreads = {};
  if (visibleItems.length >= 2) {
    const lefts = visibleItems.map((m) => m.left);
    const rights = visibleItems.map((m) => m.right);
    const widths = visibleItems.map((m) => m.width);

    const leftSpread = Math.max(...lefts) - Math.min(...lefts);
    const rightSpread = Math.max(...rights) - Math.min(...rights);
    const widthSpread = Math.max(...widths) - Math.min(...widths);
    spreads = { leftSpread, rightSpread, widthSpread };

    if ((mode === "left" || mode === "bounds") && leftSpread > tolerance) {
      errors.push(`Left edge spread ${leftSpread.toFixed(2)}px > ${tolerance}px`);
    }
    if ((mode === "right" || mode === "bounds") && rightSpread > tolerance) {
      errors.push(`Right edge spread ${rightSpread.toFixed(2)}px > ${tolerance}px`);
    }
    if ((mode === "width" || mode === "bounds") && widthSpread > tolerance) {
      errors.push(`Width spread ${widthSpread.toFixed(2)}px > ${tolerance}px`);
    }
  }

  const ok = errors.length === 0;
  const result = { ok, mode, tolerance, spreads, measurements, errors };
  if (!ok && shouldAssert) {
    assert.fail(
      `X alignment check failed (${errors.join("; ")}):\n${JSON.stringify(measurements, null, 2)}`,
    );
  }
  return result;
}

/**
 * Verify that two visible elements do not visually overlap.
 */
export async function checkNoOverlap(page, selA, selB, opts = {}) {
  const margin = opts.margin ?? 0;
  const requireBothVisible = opts.requireBothVisible ?? false;
  const shouldAssert = opts.assert !== false;

  const data = await page.evaluate(
    ([aSel, bSel]) => {
      function inspect(selector) {
        const el = document.querySelector(selector);
        if (!el) return { selector, found: false, visible: false };
        const style = getComputedStyle(el);
        const rect = el.getBoundingClientRect();
        const visible =
          style.display !== "none" &&
          style.visibility !== "hidden" &&
          style.opacity !== "0" &&
          rect.width > 0 &&
          rect.height > 0;
        return {
          selector,
          found: true,
          visible,
          left: rect.left,
          right: rect.right,
          top: rect.top,
          bottom: rect.bottom,
          width: rect.width,
          height: rect.height,
        };
      }
      return { a: inspect(aSel), b: inspect(bSel) };
    },
    [selA, selB],
  );

  if (!data.a.visible || !data.b.visible) {
    if (requireBothVisible) {
      const msg = `Expected both "${selA}" and "${selB}" to be visible: ${JSON.stringify(data)}`;
      if (shouldAssert) assert.fail(msg);
      return { ok: false, ...data, reason: msg };
    }
    return { ok: true, skipped: "not-visible", ...data };
  }

  const overlapX =
    Math.min(data.a.right, data.b.right) - Math.max(data.a.left, data.b.left);
  const overlapY =
    Math.min(data.a.bottom, data.b.bottom) - Math.max(data.a.top, data.b.top);
  const overlaps = overlapX > -margin && overlapY > -margin;

  const result = {
    ok: !overlaps,
    margin,
    overlapX,
    overlapY,
    ...data,
  };
  if (overlaps && shouldAssert) {
    assert.fail(
      `Elements "${selA}" and "${selB}" overlap (overlapX=${overlapX.toFixed(2)}, overlapY=${overlapY.toFixed(2)}, margin=${margin}):\n${JSON.stringify(data, null, 2)}`,
    );
  }
  return result;
}

/**
 * Verify that matched element(s) are not clipped by the viewport,
 * overflow-hidden/clip ancestors, or their own overflow bounds.
 */
export async function checkNotClipped(page, selector, opts = {}) {
  const tolerance = opts.tolerance ?? 2;
  const all = opts.all ?? false;
  const allowEllipsis = opts.allowEllipsis ?? true;
  const checkAncestorVertical = opts.checkAncestorVertical !== false;
  const shouldAssert = opts.assert !== false;

  const report = await page.evaluate(
    ({ sel, tol, checkAll, ellipsisAllowed, checkAncV }) => {
      const nodes = Array.from(document.querySelectorAll(sel));
      const docClientWidth = document.documentElement.clientWidth;
      const items = [];

      for (let i = 0; i < nodes.length; i += 1) {
        const el = nodes[i];
        const style = getComputedStyle(el);
        const rect = el.getBoundingClientRect();
        const visible =
          style.display !== "none" &&
          style.visibility !== "hidden" &&
          style.opacity !== "0" &&
          rect.width > 0 &&
          rect.height > 0;

        if (!visible) continue;

        const issues = [];
        if (rect.left < -tol || rect.right > docClientWidth + tol) {
          issues.push(
            `viewport-x clipped (left=${rect.left.toFixed(1)}, right=${rect.right.toFixed(1)}, docClientWidth=${docClientWidth})`,
          );
        }

        let anc = el.parentElement;
        while (anc && anc !== document.documentElement) {
          const ancStyle = getComputedStyle(anc);
          const ancRect = anc.getBoundingClientRect();
          const ox = ancStyle.overflowX;
          const oy = ancStyle.overflowY;
          if (ox === "hidden" || ox === "clip") {
            if (rect.left < ancRect.left - tol || rect.right > ancRect.right + tol) {
              issues.push(
                `ancestor-x clipped by <${anc.tagName.toLowerCase()} class="${anc.className}">`,
              );
            }
          }
          if (checkAncV && (oy === "hidden" || oy === "clip")) {
            if (rect.top < ancRect.top - tol || rect.bottom > ancRect.bottom + tol) {
              issues.push(
                `ancestor-y clipped by <${anc.tagName.toLowerCase()} class="${anc.className}">`,
              );
            }
          }
          anc = anc.parentElement;
        }

        const selfOX = style.overflowX;
        const selfOY = style.overflowY;
        const usesEllipsis = ellipsisAllowed && style.textOverflow === "ellipsis";
        if ((selfOX === "hidden" || selfOX === "clip") && !usesEllipsis) {
          if (el.scrollWidth > el.clientWidth + tol) {
            issues.push(
              `self-x clipped (scrollWidth=${el.scrollWidth} > clientWidth=${el.clientWidth})`,
            );
          }
        }
        if (selfOY === "hidden" || selfOY === "clip") {
          if (el.scrollHeight > el.clientHeight + tol) {
            issues.push(
              `self-y clipped (scrollHeight=${el.scrollHeight} > clientHeight=${el.clientHeight})`,
            );
          }
        }

        items.push({ index: i, rect: rect.toJSON(), issues });
        if (!checkAll) break;
      }

      return { totalMatched: nodes.length, checkedCount: items.length, items };
    },
    {
      sel: selector,
      tol: tolerance,
      checkAll: all,
      ellipsisAllowed: allowEllipsis,
      checkAncV: checkAncestorVertical,
    },
  );

  const failures = [];
  if (report.checkedCount === 0) {
    failures.push(`No visible element matched selector "${selector}" (matched=${report.totalMatched})`);
  } else {
    for (const item of report.items) {
      if (item.issues.length > 0) {
        failures.push(`Element[${item.index}]: ${item.issues.join("; ")}`);
      }
    }
  }

  const ok = failures.length === 0;
  const result = { ok, selector, tolerance, ...report, failures };
  if (!ok && shouldAssert) {
    assert.fail(
      `Clipping check failed for "${selector}":\n${failures.join("\n")}`,
    );
  }
  return result;
}
