package render

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestSanitizer_ParserDifferentialAndDangerousSubtrees(t *testing.T) {
	eng := NewEngine()

	// Security Finding 1: HTML5-vs-regex attribute parser differential (`title="x>y"` + spoofed data attr).
	differentialPayload := `<a title="x>y" href="https://volo-app-scheme.invalid/" data-volo-app-href="javascript://%0aalert(1)">xss</a>`
	gotDiff := eng.SanitizeHTML(differentialPayload)
	if strings.Contains(strings.ToLower(gotDiff), "javascript:") || strings.Contains(gotDiff, "data-volo-app-href") {
		t.Fatalf("parser-differential payload leaked dangerous scheme or attribute: %s", gotDiff)
	}
	if !strings.Contains(gotDiff, `href="https://volo-app-scheme.invalid/"`) ||
		!strings.Contains(gotDiff, `rel="nofollow noopener noreferrer"`) ||
		!strings.Contains(gotDiff, `title="x&gt;y"`) {
		t.Fatalf("unexpected sanitized anchor output: %s", gotDiff)
	}

	// Safe custom app deep-links (including slashless tg:resolve?...) vs blocked schemes & HTML-entity obfuscation.
	mixedLinks := strings.Join([]string{
		`<a href="tg:resolve?domain=volo&amp;post=7">Telegram</a>`,
		`<a href="obsidian://open?vault=notes&amp;file=arch">Obsidian</a>`,
		`<a href="huanai://open/article?id=42">AppLink</a>`,
		`<a href="/posts/hello" target="_self">Internal</a>`,
		`<a href="mailto:hi@example.com">Email</a>`,
		`<a href="https://example.com/docs" target="_blank">External</a>`,
		`<a href="j&#x61;vascript:alert(1)">EntityJS</a>`,
		`<a href="&#x09;java&#x0a;script:alert(1)">CtrlJS</a>`,
		`<a href="vbscript:msgbox(1)">VB</a>`,
		`<a href="data:text/html,&lt;script&gt;alert(1)&lt;/script&gt;">DataURI</a>`,
		`<a href="blob:https://evil.example/123">BlobURI</a>`,
		`<a href="file:///etc/passwd">FileURI</a>`,
		`<a href="ws://evil.example/ws">WS</a>`,
		`<a href="wss://evil.example/wss">WSS</a>`,
		`<a href="chrome-extension://abcd/page.html">ChromeExt</a>`,
		`<a href="moz-extension://abcd/page.html">MozExt</a>`,
		`<a href="view-source:https://example.com">ViewSource</a>`,
		`<a href="jar:https://example.com/app.jar!/index.html">JarURI</a>`,
		`<a href="filesystem:https://example.com/temporary/a.txt">FS</a>`,
		`<a href="about:blank">AboutBlank</a>`,
	}, "\n")

	gotLinks := eng.SanitizeHTML(mixedLinks)
	for _, want := range []string{
		`href="tg:resolve?domain=volo&amp;post=7"`,
		`href="obsidian://open?vault=notes&amp;file=arch"`,
		`href="huanai://open/article?id=42"`,
		`href="/posts/hello"`,
		`href="mailto:hi@example.com"`,
		`<a href="https://example.com/docs" target="_blank" rel="nofollow noopener noreferrer">External</a>`,
	} {
		if !strings.Contains(gotLinks, want) {
			t.Fatalf("expected safe link %s in sanitized output:\n%s", want, gotLinks)
		}
	}
	for _, blocked := range []string{
		"javascript:", "vbscript:", "data:", "blob:", "file:",
		"ws:", "wss:", "chrome-extension:", "moz-extension:",
		"view-source:", "jar:", "filesystem:", "about:",
	} {
		if strings.Contains(strings.ToLower(gotLinks), blocked) {
			t.Fatalf("blocked scheme %q leaked in:\n%s", blocked, gotLinks)
		}
	}

	// Untrusted comments must NOT allow custom app deep-link schemes.
	gotComment := eng.RenderComment(`[tg](tg:resolve?domain=volo) and [web](https://example.com)`)
	if strings.Contains(gotComment, "tg:resolve") {
		t.Fatalf("expected RenderComment to strip custom app scheme href, got: %s", gotComment)
	}
	if !strings.Contains(gotComment, `href="https://example.com"`) || !strings.Contains(gotComment, `rel="nofollow noopener noreferrer"`) {
		t.Fatalf("expected safe https link in comment output, got: %s", gotComment)
	}

	// Dangerous raw-text and foreign-namespace subtrees must drop both tags and inner payload text.
	subtrees := strings.Join([]string{
		`<p>safe_visible</p>`,
		`<script>evil_script()</script>`,
		`<style>body{display:none}/*evil_style*/</style>`,
		`<object data="//evil/x">evil_object</object>`,
		`<embed src="//evil/y">`,
		`<applet>evil_applet</applet>`,
		`<base href="https://evil.example/">`,
		`<meta http-equiv="refresh" content="0;url=javascript:alert(1)">`,
		`<link rel="stylesheet" href="https://evil.example/x.css">`,
		`<form action="https://evil.example"><button>evil_form</button></form>`,
		`<textarea>evil_textarea</textarea>`,
		`<title>evil_title</title>`,
		`<noscript><img src="x" onerror="alert(1)">evil_noscript</noscript>`,
		`<template><script>evil_template()</script>evil_tpl</template>`,
		`<svg onload="alert(1)"><desc>evil_svg</desc></svg>`,
		`<math><mtext>evil_mathml</mtext></math>`,
	}, "")
	gotSubtrees := eng.SanitizeHTML(subtrees)
	if gotSubtrees != `<p>safe_visible</p>` {
		t.Fatalf("expected dangerous subtrees to be completely stripped, got: %s", gotSubtrees)
	}
}

func TestPreserveDirectives_LosslessRoundTripAndPublicStripping(t *testing.T) {
	eng := NewEngine()

	rawLock := ":::lock pwd=\"s3cr3t!\" hint=\"Ask author\" type=\"pwd\"\nProtected **markdown** content\n:::"
	rawBook := ":::book id=\"the-way-to-go\":::"
	rawShelf := ":::bookshelf limit=\"2\":::"
	rawCustom := ":::callout[kind=\"tip\"]\nInner custom body\n:::"
	rawTOC := "[toc]"

	md := strings.Join([]string{
		rawTOC,
		"",
		rawLock,
		"",
		rawBook,
		"",
		rawShelf,
		"",
		rawCustom,
	}, "\n")

	extractB64Payloads := func(htmlStr string) []string {
		re := regexp.MustCompile(`data-volo-md-b64="([^"]+)"`)
		matches := re.FindAllStringSubmatch(htmlStr, -1)
		out := make([]string, 0, len(matches))
		for _, m := range matches {
			dec, err := base64.StdEncoding.DecodeString(m[1])
			if err != nil {
				t.Fatalf("invalid base64 in data-volo-md-b64=%q: %v", m[1], err)
			}
			out = append(out, string(dec))
		}
		return out
	}

	// 1. PreserveDirectives: true with locked card (0 headings in document -> [toc] still emits <nav ...>TOC</nav>!).
	outLocked := eng.Render(md, Options{
		Mode:               RenderModeWeb,
		Locale:             "en-US",
		PreserveDirectives: true,
		LookupBook: func(slug string) (BookCardInfo, bool) {
			return BookCardInfo{Slug: slug, Title: "The Way to Go", ChapterCount: 10}, true
		},
		ListBooks: func() []BookCardInfo {
			return []BookCardInfo{{Slug: "the-way-to-go", Title: "The Way to Go", ChapterCount: 10}}
		},
	})

	wantTOCNav := `<nav class="volo-inline-toc" data-md-toc="1" contenteditable="false" data-volo-md-b64="` + base64.StdEncoding.EncodeToString([]byte(rawTOC)) + `">TOC</nav>`
	if !strings.Contains(outLocked.HTML, wantTOCNav) {
		t.Fatalf("expected 0-heading [toc] to emit non-editable TOC nav placeholder, got:\n%s", outLocked.HTML)
	}

	gotPayloads := extractB64Payloads(outLocked.HTML)
	wantPayloads := []string{rawTOC, rawLock, rawBook, rawShelf, rawCustom}
	if len(gotPayloads) != len(wantPayloads) {
		t.Fatalf("expected %d preserved directive payloads, got %d (%v)\nHTML:\n%s", len(wantPayloads), len(gotPayloads), gotPayloads, outLocked.HTML)
	}
	for idx, want := range wantPayloads {
		if gotPayloads[idx] != want {
			t.Fatalf("directive[%d] mismatch:\ngot:  %q\nwant: %q", idx, gotPayloads[idx], want)
		}
	}

	// 2. PreserveDirectives: true with AuthorOrShareBypass: true (admin editor preview path) & CustomDirective callback.
	outBypass := eng.Render(md, Options{
		Mode:                RenderModeWeb,
		Locale:              "en-US",
		Unlocked:            true,
		AuthorOrShareBypass: true,
		PreserveDirectives:  true,
		CustomDirective: func(name string, attrs map[string]string, body string) (string, bool) {
			if name == "callout" {
				return `<aside class="custom-callout"><p>` + body + `</p></aside>`, true
			}
			return "", false
		},
	})
	bypassPayloads := extractB64Payloads(outBypass.HTML)
	if len(bypassPayloads) != len(wantPayloads) {
		t.Fatalf("expected %d preserved directive payloads in bypass preview, got %d\nHTML:\n%s", len(wantPayloads), len(bypassPayloads), outBypass.HTML)
	}
	if bypassPayloads[1] != rawLock {
		t.Fatalf("unlocked bypass lock wrapper lost full raw fence: got %q, want %q", bypassPayloads[1], rawLock)
	}

	// 3. PreserveDirectives: false and SanitizeHTML MUST strip spoofed contenteditable & data-volo-md-b64!
	spoofMD := md + "\n\n" + `<div contenteditable="false" data-volo-md-b64="YWJj">spoof</div>`
	outPublic := eng.Render(spoofMD, Options{Mode: RenderModeWeb, Locale: "en-US"})
	if strings.Contains(outPublic.HTML, "data-volo-md-b64") || strings.Contains(outPublic.HTML, "contenteditable") {
		t.Fatalf("public render leaked editor directive attributes:\n%s", outPublic.HTML)
	}
	sanDirect := eng.SanitizeHTML(`<div contenteditable="false" data-volo-md-b64="YWJj">spoof</div>`)
	if strings.Contains(sanDirect, "data-volo-md-b64") || strings.Contains(sanDirect, "contenteditable") {
		t.Fatalf("SanitizeHTML leaked editor directive attributes: %s", sanDirect)
	}
}

func TestIndentedListLockIsolationAndRawHTMLCommentPreTracking(t *testing.T) {
	eng := NewEngine()

	// 4+ space indented `:::lock` blocks and fenced code blocks nested inside unordered/ordered list items.
	md := strings.Join([]string{
		"- item one",
		"    :::lock pwd=\"list_secret_1\"",
		"    hidden_tight_list_secret",
		"    :::",
		"- item two",
		"",
		"    :::lock pwd=\"list_secret_2\"",
		"    hidden_loose_list_secret",
		"    :::",
		"  - nested item",
		"      :::lock pwd=\"list_secret_3\"",
		"      hidden_nested_list_secret",
		"      :::",
		"10. ordered item",
		"    ```go",
		"    func OrderedFence() int { return 42 }",
		"    ```",
	}, "\n")

	outAnon := eng.Render(md, Options{Mode: RenderModeWeb, Locale: "en-US"})
	for _, secret := range []string{
		"list_secret_1", "hidden_tight_list_secret",
		"list_secret_2", "hidden_loose_list_secret",
		"list_secret_3", "hidden_nested_list_secret",
		":::lock",
	} {
		if strings.Contains(outAnon.HTML, secret) || strings.Contains(outAnon.PlainText, secret) {
			t.Fatalf("CRITICAL LEAK: indented list lock leaked %q!\nHTML:\n%s\nPlain:\n%s", secret, outAnon.HTML, outAnon.PlainText)
		}
	}
	if !outAnon.HasLockBlock || !outAnon.HasUnresolvedLock {
		t.Fatalf("expected HasLockBlock=true and HasUnresolvedLock=true, got %+v", outAnon)
	}
	if !strings.Contains(outAnon.HTML, `data-lang="go"`) || !strings.Contains(outAnon.HTML, "OrderedFence") {
		t.Fatalf("expected 4-space indented code fence inside 10. ordered list item to highlight as Go:\n%s", outAnon.HTML)
	}
	if !ContainsLockDirective(md) {
		t.Fatalf("ContainsLockDirective missed indented list lock")
	}
	pwds := ExtractAllLockPasswords(md)
	if strings.Join(pwds, ",") != "list_secret_1,list_secret_2,list_secret_3" {
		t.Fatalf("ExtractAllLockPasswords mismatch: %v", pwds)
	}
	stripped := StripAllLockBlocks(md)
	if strings.Contains(stripped, "hidden_") || strings.Contains(stripped, "list_secret_") {
		t.Fatalf("StripAllLockBlocks leaked indented list lock:\n%s", stripped)
	}

	// Multi-line HTML comments and raw <pre>...</pre> must not rewrite inner math, subscripts, or directives.
	rawCommentAndPre := strings.Join([]string{
		"<!--",
		":::lock pwd=\"comment_pwd\"",
		"$x + y$ and H~2~O",
		":::",
		"-->",
		"<pre>",
		":::custom_demo",
		"price $a + b$ and path ~sub~",
		":::",
		"</pre>",
	}, "\n")
	outRaw := eng.Render(rawCommentAndPre, Options{
		Mode:               RenderModeWeb,
		PreserveDirectives: true,
	})
	if outRaw.NeedsMath || outRaw.HasLockBlock {
		t.Fatalf("math or lock inside HTML comment / <pre> should not trigger: NeedsMath=%v HasLock=%v", outRaw.NeedsMath, outRaw.HasLockBlock)
	}
	if strings.Contains(outRaw.HTML, "comment_pwd") || strings.Contains(outRaw.HTML, "volo-math-inline") || strings.Contains(outRaw.HTML, "<sub>") || strings.Contains(outRaw.HTML, "volo-custom-directive") {
		t.Fatalf("unexpected rewriting inside comment or <pre>:\n%s", outRaw.HTML)
	}
	if !strings.Contains(outRaw.HTML, "<pre>") || !strings.Contains(outRaw.HTML, "$a + b$") || !strings.Contains(outRaw.HTML, "~sub~") {
		t.Fatalf("expected <pre> inner text to survive verbatim:\n%s", outRaw.HTML)
	}

	// Unclosed <pre> earlier in an article must NEVER disable downstream :::lock fail-closed protection!
	unclosedPreWithLock := "<pre>\nunclosed pre block\n\n:::lock pwd=\"real_pwd\"\nmust_stay_hidden\n:::"
	outUnclosedPre := eng.Render(unclosedPreWithLock, Options{Mode: RenderModeWeb})
	if strings.Contains(outUnclosedPre.HTML, "real_pwd") || strings.Contains(outUnclosedPre.HTML, "must_stay_hidden") {
		t.Fatalf("unclosed <pre> bypassed downstream :::lock protection:\n%s", outUnclosedPre.HTML)
	}
}

func TestMathURLCurrencySubscriptNFDHeadingAndMediaFixes(t *testing.T) {
	eng := NewEngine()

	// 1. URLs and HTML attributes with `$` must not trigger math spans.
	urlMathMD := strings.Join([]string{
		`[Wiki](https://en.wikipedia.org/wiki/Item_($1)_($2))`,
		`Bare https://example.com/api/$1/view/$2 link.`,
		`<div title="a > b $1 $2">attr_text</div>`,
	}, "\n\n")
	outURLMath := eng.Render(urlMathMD, Options{Mode: RenderModeWeb})
	if outURLMath.NeedsMath || strings.Contains(outURLMath.HTML, "volo-math") {
		t.Fatalf("dollar signs inside URLs or HTML attributes triggered math:\n%s", outURLMath.HTML)
	}
	if !strings.Contains(outURLMath.HTML, `href="https://en.wikipedia.org/wiki/Item_($1)_($2)"`) ||
		!strings.Contains(outURLMath.HTML, `href="https://example.com/api/$1/view/$2"`) ||
		!strings.Contains(outURLMath.HTML, `title="a &gt; b $1 $2"`) {
		t.Fatalf("corrupted URL or attribute containing $:\n%s", outURLMath.HTML)
	}

	// 2. CJK currency bridging guard: `"预算$100，公式$\alpha$"`
	outCJK := eng.Render(`预算$100，公式$\alpha$`, Options{Mode: RenderModeWeb, Locale: "zh-CN"})
	if !outCJK.NeedsMath {
		t.Fatalf("expected NeedsMath=true for CJK mixed currency + formula")
	}
	if !strings.Contains(outCJK.HTML, "预算$100，公式") ||
		!strings.Contains(outCJK.HTML, `<span class="volo-math-inline">$\alpha$</span>`) {
		t.Fatalf("CJK currency bridged into math or corrupted formula:\n%s", outCJK.HTML)
	}

	// 3. Tilde/caret path & URL protection vs prose subscript/superscript/highlight.
	tildeMD := strings.Join([]string{
		`Relative user path: /~user/file~1~2 and home path ~/dir/file~1~ and nested dir/~bar~baz~`,
		`Protocol-less www.example.com/~user/file~1~2`,
		`Custom scheme [Open](huanai://open/~u/a~1~) and bare huanai://open/~u/a~1~`,
		`[ref-tilde][id]`,
		``,
		`[id]: /~user/a~1~`,
		``,
		`Formula H~2~O and E=mc^2^ with ==cross **tag** highlight==.`,
	}, "\n")
	outTilde := eng.Render(tildeMD, Options{Mode: RenderModeWeb})
	if strings.Contains(outTilde.HTML, "<del>") {
		t.Fatalf("tilde path was corrupted into <del> strikethrough:\n%s", outTilde.HTML)
	}
	for _, wantLiteral := range []string{
		`/~user/file~1~2`,
		`~/dir/file~1~`,
		`dir/~bar~baz~`,
		`www.example.com/~user/file~1~2`,
		`href="huanai://open/~u/a~1~"`,
		`<a href="/~user/a~1~">ref-tilde</a>`,
		`H<sub>2</sub>O`,
		`mc<sup>2</sup>`,
		`<mark>cross <strong>tag</strong> highlight</mark>`,
	} {
		if !strings.Contains(outTilde.HTML, wantLiteral) {
			t.Fatalf("missing expected output %q in:\n%s", wantLiteral, outTilde.HTML)
		}
	}

	// 4. NFD combining marks (`\p{M}`) in heading IDs & case-normalized existingID deduplication.
	nfdMD := "<h2 id=\"Intro\">Intro</h2>\n\n## intro\n\n## か\u3099き\u3099\n\n## Cafe\u0301\n"
	outNFD := eng.Render(nfdMD, Options{Mode: RenderModeWeb})
	for _, wantID := range []string{
		`id="intro"`,
		`id="intro-2"`,
		"id=\"か\u3099き\u3099\"",
		"id=\"cafe\u0301\"",
	} {
		if !strings.Contains(outNFD.HTML, wantID) {
			t.Fatalf("missing expected heading %s in:\n%s", wantID, outNFD.HTML)
		}
	}

	// 5. Standalone `.weba` audio and `.m4v` video link upgrades.
	mediaMD := "[Podcast Clip](https://cdn.example.com/audio/ep1.weba)\n\n[Keynote](https://cdn.example.com/video/talk.m4v)"
	outMedia := eng.Render(mediaMD, Options{Mode: RenderModeWeb})
	if !strings.Contains(outMedia.HTML, `<audio controls="" preload="metadata" src="https://cdn.example.com/audio/ep1.weba"></audio>`) &&
		!strings.Contains(outMedia.HTML, `<audio src="https://cdn.example.com/audio/ep1.weba"`) {
		t.Fatalf("expected .weba link to upgrade to <audio>, got:\n%s", outMedia.HTML)
	}
	if !strings.Contains(outMedia.HTML, `<video`) || !strings.Contains(outMedia.HTML, `https://cdn.example.com/video/talk.m4v`) {
		t.Fatalf("expected .m4v link to upgrade to <video>, got:\n%s", outMedia.HTML)
	}
}

func TestLexers_RustLifetimesCppSeparatorsPythonFStringsAndHTMLProse(t *testing.T) {
	// 1. Rust lifetimes vs single-char literals.
	rustSrc := "fn foo<'a, 'b>(x: &'a str) -> char {\n    let ch = 'z';\n    let nl = '\\n';\n}\n"
	rustHTML := HighlightCodeHTML(rustSrc, "rust", false)
	if strings.Contains(rustHTML, `<span class="s1">&#39;a, &#39;</span>`) {
		t.Fatalf("Rust lifetimes mis-highlighted as string literal:\n%s", rustHTML)
	}
	if strings.Count(rustHTML, `<span class="kt">&#39;a</span>`) != 2 ||
		!strings.Contains(rustHTML, `<span class="kt">&#39;b</span>`) {
		t.Fatalf("expected Rust lifetimes 'a and 'b to highlight as kt:\n%s", rustHTML)
	}
	if !strings.Contains(rustHTML, `<span class="s1">&#39;z&#39;</span>`) ||
		!strings.Contains(rustHTML, `<span class="s1">&#39;\n&#39;</span>`) {
		t.Fatalf("expected Rust char literals to highlight as s1:\n%s", rustHTML)
	}

	// 2. C++14 digit separators and removal of JS backtick template strings.
	cppSrc := "uint64_t a = 1'000'000;\nuint32_t b = 0xFF'FF;\n// `not_template`\n`raw_tick`\n"
	cppHTML := HighlightCodeHTML(cppSrc, "cpp", false)
	if !strings.Contains(cppHTML, `<span class="mi">1&#39;000&#39;000</span>`) {
		t.Fatalf("expected C++ 1'000'000 to tokenize as single mi number:\n%s", cppHTML)
	}
	if !strings.Contains(cppHTML, `<span class="mh">0xFF&#39;FF</span>`) {
		t.Fatalf("expected C++ 0xFF'FF to tokenize as single mh hex number:\n%s", cppHTML)
	}
	if strings.Contains(cppHTML, `class="s1"`) || strings.Contains(cppHTML, `class="sb"`) {
		t.Fatalf("C++ digit separators or backticks mis-tokenized as string:\n%s", cppHTML)
	}

	// 3. Python multi-line triple-quoted f-strings and raw f-strings.
	pySrc := "msg = f\"\"\"line1\n{user_name}\nline3\"\"\"\npat = rf'\\d+'\n"
	pyHTML := HighlightCodeHTML(pySrc, "python", true)
	if strings.Count(pyHTML, `<span class="s">`) < 4 {
		t.Fatalf("expected multi-line Python f\"\"\"...\"\"\" and rf'...' to highlight as string across lines:\n%s", pyHTML)
	}

	// 4. HTML/Vue tag-state-aware tokenization: prose outside tags stays plain escaped text!
	htmlSrc := `<div class="hero" data-id='42'>We don't color English prose words &amp; apostrophes!</div>`
	htmlOut := HighlightCodeHTML(htmlSrc, "html", false)
	if !strings.Contains(htmlOut, `<span class="nt">&lt;div</span>`) ||
		!strings.Contains(htmlOut, `<span class="na">class</span>`) ||
		!strings.Contains(htmlOut, `<span class="s">&#34;hero&#34;</span>`) ||
		!strings.Contains(htmlOut, `<span class="na">data-id</span>`) ||
		!strings.Contains(htmlOut, `<span class="s">&#39;42&#39;</span>`) ||
		!strings.Contains(htmlOut, `<span class="ni">&amp;amp;</span>`) ||
		!strings.Contains(htmlOut, `<span class="nt">&lt;/div&gt;</span>`) {
		t.Fatalf("expected HTML tags, attributes, strings, and entities to highlight cleanly:\n%s", htmlOut)
	}
	if !strings.Contains(htmlOut, `We don&#39;t color English prose words `) {
		t.Fatalf("expected HTML prose outside tags to be plain escaped text without na/s spans:\n%s", htmlOut)
	}
}

func BenchmarkHighlightCodeHTML_MultiLang(b *testing.B) {
	snippet := strings.Join([]string{
		"fn process<'a, 'b>(input: &'a str, fallback: &'b str) -> Result<&'a str, String> {",
		"    if input.is_empty() {",
		"        return Ok(fallback);",
		"    }",
		"    let sep = '\\n';",
		"    println!(\"sep={}\", sep);",
		"    Ok(input)",
		"}",
	}, "\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := HighlightCodeHTML(snippet, "rust", true); out == "" {
			b.Fatal("empty")
		}
	}
}
