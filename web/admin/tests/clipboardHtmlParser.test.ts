import test from "node:test";
import assert from "node:assert/strict";
import { parseClipboardHtml } from "../src/components/clipboardHtmlParser.ts";

test("parseClipboardHtml extracts head meta and body elements without DOMParser", () => {
  const doc = parseClipboardHtml(
    '<html><head><meta name="generator" content="Visual Studio Code"></head><body><h1>Hello &amp; Welcome</h1><p>World</p></body></html>'
  );
  const metas = doc.querySelectorAll("meta");
  assert.equal(metas.length, 1);
  assert.equal(metas[0].getAttribute("content"), "Visual Studio Code");
  assert.equal(doc.body.querySelector("h1")?.textContent, "Hello & Welcome");
  assert.equal(doc.body.textContent, "Hello & WelcomeWorld");
});

test("parseClipboardHtml handles void tags, comments, entities, and raw script/style blocks", () => {
  const doc = parseClipboardHtml(
    '<!-- cmt --><script>const x = "<b>not a tag</b>";</script><style>.a{color:red}</style><p>Pic <img src="file:///C:/a.png" alt="A &quot;1&quot;"> &#x27;ok&#x27;</p>'
  );
  assert.equal(doc.body.querySelectorAll("script").length, 1);
  assert.equal(doc.body.querySelectorAll("b").length, 0);
  const imgs = doc.body.querySelectorAll("img");
  assert.equal(imgs.length, 1);
  assert.equal(imgs[0].getAttribute("src"), "file:///C:/a.png");
  assert.equal(imgs[0].getAttribute("alt"), 'A "1"');
  doc.body.querySelectorAll("script, style").forEach((n) => n.remove());
  assert.equal(doc.body.textContent.trim(), "Pic  'ok'");
});

test("parseClipboardHtml supports Yuque, Feishu, KaTeX, :scope selectors, style parsing, and DOM mutation", () => {
  const doc = parseClipboardHtml(`
    <ne-h2>Heading</ne-h2>
    <div data-lake-card="codeblock" data-language="go" data-card-value="data:%7B%22code%22%3A%22fmt.Println(1)%22%7D"></div>
    <div class="ace-code-block" data-lang="ts"><div data-ace-line="1">const a = 1;</div></div>
    <span class="katex"><annotation encoding="application/x-tex">E=mc^2</annotation></span>
    <span style="font-weight: 700; font-style: italic; background-color: yellow !important;">Hi</span>
    <sup class="cite">[1]</sup>
    <div aria-hidden="true">secret-noise</div>
    <table><thead><tr><th>A</th><th>B</th></tr></thead><tbody><tr><td>1</td><td>2</td></tr></tbody></table>
  `);
  assert.equal(doc.body.querySelectorAll("ne-h2").length, 1);
  assert.equal(doc.body.querySelector('div[data-lake-card="codeblock"]')?.getAttribute("data-language"), "go");
  assert.equal(doc.body.querySelectorAll(".ace-code-block div[data-ace-line]").length, 1);
  assert.equal(doc.body.querySelector('.katex annotation[encoding="application/x-tex"]')?.textContent, "E=mc^2");

  const span = doc.body.querySelector("span[style]")!;
  assert.equal(span.style.fontWeight, "700");
  assert.equal(span.style.fontStyle, "italic");
  assert.equal(span.style.backgroundColor, "yellow");
  span.style.backgroundColor = "";
  assert.equal(span.style.backgroundColor, "");

  const tbl = doc.body.querySelector("table")!;
  assert.equal(tbl.querySelectorAll(":scope > thead > tr, :scope > tbody > tr").length, 2);
  assert.equal(tbl.querySelectorAll(":scope > tr").length, 0);

  doc.body.querySelector("[aria-hidden='true']")?.remove();
  assert.equal(doc.body.querySelectorAll("[aria-hidden='true']").length, 0);

  const sup = doc.body.querySelector("sup")!;
  assert.equal(sup.classList.contains("cite"), true);
  const replacement = doc.createElement("p");
  replacement.textContent = "replaced";
  sup.replaceWith(replacement);
  assert.equal(doc.body.querySelectorAll("sup").length, 0);
  assert.ok(doc.body.textContent.includes("replaced"));
});

test("parseClipboardHtml handles unclosed tags and lone < safely in linear time", () => {
  const weird = "<p>a < b && 3 < 5" + "<span>x".repeat(500);
  const doc = parseClipboardHtml(weird);
  assert.ok(doc.body.textContent.includes("a < b && 3 < 5"));
  assert.equal(doc.body.querySelectorAll("span").length, 500);
});
