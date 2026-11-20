import test from "node:test";
import assert from "node:assert/strict";
import { extractColonDetail, inferToastKind, trimEndChars } from "../src/toastKind.ts";

test("inferToastKind classifies err, warn, and ok messages accurately and runs in linear time", () => {
  assert.equal(inferToastKind("HTTP 500 Internal Server Error"), "err");
  assert.equal(inferToastKind("Operation failed"), "err");
  assert.equal(inferToastKind("Saved successfully"), "ok");

  // Positive warn matches
  assert.equal(inferToastKind("Webcam not detected"), "warn");
  assert.equal(inferToastKind("No USB device connected"), "warn");
  assert.equal(inferToastKind("no  device"), "warn");
  assert.equal(inferToastKind("NO audio DEVICE"), "warn");
  assert.equal(inferToastKind("未检测到可用设备"), "warn");
  assert.equal(inferToastKind("未偵測到裝置"), "warn");
  assert.equal(inferToastKind("no \nno  device"), "warn");

  // Negative checks preserving exact `no .* device` single-line semantics
  assert.equal(inferToastKind("no device"), "ok");
  assert.equal(inferToastKind("no \n device"), "ok");
  assert.equal(inferToastKind("no \r device"), "ok");
  assert.equal(inferToastKind("no \u2028 device"), "ok");

  for (const [label, input] of [
    ["no_repeat_20k", "no ".repeat(20000)],
    ["no_nl_repeat_10k_device", "no \n".repeat(10000) + " device"],
  ] as const) {
    const t0 = performance.now();
    const kind = inferToastKind(input);
    const elapsedMs = performance.now() - t0;
    assert.equal(kind, "ok");
    assert.ok(elapsedMs < 25, `inferToastKind (${label}) took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }
});

test("trimEndChars, trimChars, and extractColonDetail match legacy semantics in linear time", () => {
  assert.equal(trimEndChars("/console///", "/"), "/console");
  assert.equal(trimEndChars("/", "/"), "");
  assert.equal(trimEndChars("hello.!", ".!"), "hello");
  assert.equal(trimEndChars("YWJj==", "="), "YWJj");
  assert.equal(trimEndChars("Title ###", "#"), "Title ");
  assert.equal(trimEndChars("\n\nhi\n", "\n", true), "hi");
  assert.equal(trimEndChars("\n\n", "\n", true), "");

  assert.deepEqual(extractColonDetail("auth: invalid credentials"), ["auth", "invalid credentials"]);
  assert.deepEqual(extractColonDetail("Settings:  saved"), ["settings", "saved"]);
  assert.deepEqual(extractColonDetail("Validation   failed: bad slug"), ["validation", "bad slug"]);
  assert.deepEqual(extractColonDetail("Validation\nfailed: bad"), ["validation", "bad"]);
  assert.deepEqual(extractColonDetail("database:\n  locked"), ["database", "locked"]);
  assert.equal(extractColonDetail("auth:"), null);
  assert.equal(extractColonDetail("auth:   "), null);
  assert.equal(extractColonDetail("auth: line1\nline2"), null);
  assert.equal(extractColonDetail("theme: a\u2028b"), null);
  assert.equal(extractColonDetail("no colon"), null);

  for (const [label, fn] of [
    ["trimEnd_slash_20k", () => trimEndChars("/".repeat(20000) + "!", "/")],
    ["trimChars_nl_20k", () => trimEndChars("\n".repeat(20000) + "!", "\n", true)],
    ["colon_spaces_20k", () => extractColonDetail("auth:" + " ".repeat(20000) + "\n")],
  ] as const) {
    const t0 = performance.now();
    fn();
    const elapsedMs = performance.now() - t0;
    assert.ok(elapsedMs < 25, `${label} took ${elapsedMs.toFixed(2)}ms (limit 25ms)`);
  }
});
