import test from "node:test";
import assert from "node:assert/strict";
import { safeHttpUrl, safeHref, safeMediaSrc, cleanClipboardUrl } from "../src/urlSanitize.ts";

test("safeHttpUrl accepts http/https URLs and rejects unsafe/relative schemes", () => {
  assert.equal(safeHttpUrl("https://example.com/path?a=1#h"), "https://example.com/path?a=1#h");
  assert.equal(safeHttpUrl("  http://localhost:8080/health  "), "http://localhost:8080/health");
  assert.equal(safeHttpUrl("javascript:alert(1)"), "");
  assert.equal(safeHttpUrl("data:text/html,<script>alert(1)</script>"), "");
  assert.equal(safeHttpUrl("/relative/path"), "");
  assert.equal(safeHttpUrl("//evil.example/x"), "");
  assert.equal(safeHttpUrl("http:evil.com"), "");
  assert.equal(safeHttpUrl("https:evil.com"), "");
  assert.equal(safeHttpUrl("http:///evil.com"), "");
  assert.equal(safeHttpUrl("https://example.com/\"onclick=x"), "");
  assert.equal(safeHttpUrl(null), "");
});

test("safeHref allows safe http/https/relative/hash/optional-mailto and blocks obfuscated XSS vectors", () => {
  assert.equal(safeHref("https://volo.dev/docs?v=1"), "https://volo.dev/docs?v=1");
  assert.equal(safeHref("/posts/hello-world"), "/posts/hello-world");
  assert.equal(safeHref("./img.png"), "./img.png");
  assert.equal(safeHref("../assets/doc.pdf"), "../assets/doc.pdf");
  assert.equal(safeHref("#section-1"), "#section-1");
  assert.equal(safeHref("mailto:admin@example.com"), "");
  assert.equal(safeHref("mailto:admin@example.com", { allowMailto: true }), "mailto:admin@example.com");

  // Control characters, null bytes, zero-width obfuscation, protocol-relative, backslash bypass, opaque http:
  assert.equal(safeHref("java\u0000script:alert(1)"), "");
  assert.equal(safeHref("\u200bjavascript:alert(1)"), "");
  assert.equal(safeHref("JAVASCRIPT:alert(1)"), "");
  assert.equal(safeHref("VBSCRIPT:msgbox(1)"), "");
  assert.equal(safeHref("DATA:image/svg+xml;base64,PHN2Zz4="), "");
  assert.equal(safeHref("java\tscript:alert(1)"), "");
  assert.equal(safeHref("java\nscript:alert(1)"), "");
  assert.equal(safeHref("java script:alert(1)"), "");
  assert.equal(safeHref("  javascript:alert(1)"), "");
  assert.equal(safeHref("vbscript:msgbox(1)"), "");
  assert.equal(safeHref("data:text/html;base64,PHNjcmlwdD4="), "");
  assert.equal(safeHref("//evil.example.com/steal"), "");
  assert.equal(safeHref("/\\evil.example.com"), "");
  assert.equal(safeHref("\\evil.example.com"), "");
  assert.equal(safeHref("http:evil.com"), "");
  assert.equal(safeHref("https:evil.com"), "");
  assert.equal(safeHref("http:///evil.com"), "");
  assert.equal(safeHref("https://example.com/<script>"), "");
});

test("safeMediaSrc allows safe media paths and base64 raster data images only when opted in", () => {
  const pngB64 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR4nGNgYAAAAAMAASsJTYQAAAAASUVORK5CYII=";
  const svgB64 = "data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+PC9zdmc+";

  assert.equal(safeMediaSrc("/media/cover.webp"), "/media/cover.webp");
  assert.equal(safeMediaSrc("https://cdn.example.com/v.mp4"), "https://cdn.example.com/v.mp4");
  assert.equal(safeMediaSrc(pngB64), "");
  assert.equal(safeMediaSrc(pngB64, { allowDataImage: true }), pngB64);
  assert.equal(safeMediaSrc("data:image/png;base64,iVBOR w0K", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc(svgB64, { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("http:evil.com"), "");
  assert.equal(safeMediaSrc("javascript:alert(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("JAVASCRIPT:alert(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("VBSCRIPT:msgbox(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("DATA:image/svg+xml;base64,PHN2Zz4=", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("java\tscript:alert(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("java\nscript:alert(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("  javascript:alert(1)", { allowDataImage: true }), "");
  assert.equal(safeMediaSrc("blob:https://example.com/1234", { allowBlob: true }), "blob:https://example.com/1234");
  assert.equal(safeMediaSrc("blob:javascript:1234", { allowBlob: true }), "");
});

test("cleanClipboardUrl unescapes safe &amp; entities without falling back to unsafe raw strings", () => {
  assert.equal(
    cleanClipboardUrl("https://cdn.example.com/img?w=800&amp;amp;fit=cover", "src"),
    "https://cdn.example.com/img?w=800&fit=cover"
  );
  assert.equal(cleanClipboardUrl("/media/pic.jpg?v=1&amp;s=2", "src"), "/media/pic.jpg?v=1&s=2");
  assert.equal(cleanClipboardUrl("data:image/svg+xml;base64,PHN2Zz4=", "src"), "");
  assert.equal(cleanClipboardUrl("javascript:alert(document.cookie)", "href"), "");
  assert.equal(cleanClipboardUrl("mailto:team@example.com", "href"), "mailto:team@example.com");
});
