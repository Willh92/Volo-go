package render

import (
	"strings"
	"testing"
)

func TestEngineRenderFeaturesXSSAndFailClosedLocks(t *testing.T) {
	eng := NewEngine()

	md := strings.Join([]string{
		"# Architecture Overview",
		"",
		"Listen at [03:25] for the WAL benchmark and see [[go-sqlite-wal]].",
		"",
		"![Diagram](/uploads/arch.png)",
		"",
		"```go",
		"package main",
		"// :::lock inside a code fence MUST NOT be treated as a lock directive!",
		"const secretInCodeSample = \"VISIBLE_CODE_LITERAL\"",
		"```",
		"",
		"```mermaid",
		"graph LR; A-->B;",
		"```",
		"",
		"Inline math $E = mc^2$ and raw XSS <script>alert(1)</script><img src=x onerror=alert(2)>.",
		"",
		":::lock type=\"pwd\" pwd=\"topsecret\" hint=\"Passcode needed\"",
		"HIDDEN_LOCKED_PARAGRAPH_1",
		":::",
		"",
		":::lock type=\"user\"",
		"READER_ONLY_PARAGRAPH_2",
		":::",
		"",
		":::lock type=\"pwd\"",
		"UNCLOSED_EOF_SECRET_3",
	}, "\n")

	// 1. Anonymous un-unlocked render
	outAnon := eng.Render(md, Options{
		Mode:     RenderModeWeb,
		Locale:   "en-US",
		HasAudio: true,
		LookupMediaDims: func(src string) (int, int, bool) {
			if src == "/uploads/arch.png" {
				return 1280, 720, true
			}
			return 0, 0, false
		},
	})

	if !outAnon.HasLockBlock {
		t.Fatalf("expected HasLockBlock=true")
	}
	if !outAnon.NeedsMermaid || !outAnon.NeedsMath {
		t.Fatalf("expected NeedsMermaid=true and NeedsMath=true, got mermaid=%v math=%v", outAnon.NeedsMermaid, outAnon.NeedsMath)
	}
	if !strings.Contains(outAnon.HTML, "VISIBLE_CODE_LITERAL") {
		t.Fatalf("fenced code sample should remain visible: %s", outAnon.HTML)
	}
	for _, secret := range []string{"HIDDEN_LOCKED_PARAGRAPH_1", "READER_ONLY_PARAGRAPH_2", "UNCLOSED_EOF_SECRET_3"} {
		if strings.Contains(outAnon.HTML, secret) || strings.Contains(outAnon.PlainText, secret) {
			t.Fatalf("secret %q leaked into un-unlocked render!", secret)
		}
	}
	if strings.Contains(outAnon.HTML, "<script>") || strings.Contains(outAnon.HTML, "onerror") {
		t.Fatalf("XSS payload was not stripped by bluemonday: %s", outAnon.HTML)
	}
	if !strings.Contains(outAnon.HTML, `class="volo-audio-ts" data-audio-seek="205">▶ 03:25</button>`) {
		t.Fatalf("expected [03:25] -> unified volo-audio-ts ▶ 03:25 button in HTML: %s", outAnon.HTML)
	}
	if !strings.Contains(outAnon.HTML, `width="1280"`) || !strings.Contains(outAnon.HTML, `height="720"`) {
		t.Fatalf("expected intrinsic width/height injected onto image: %s", outAnon.HTML)
	}
	if len(outAnon.TOC) == 0 || outAnon.TOC[0].Text != "Architecture Overview" {
		t.Fatalf("expected heading TOC extracted, got %+v", outAnon.TOC)
	}

	// 2. Signed-in Reader unlocks `type="user"` ONLY, while `type="pwd"` and unclosed EOF remain locked!
	outReader := eng.Render(md, Options{
		Mode:             RenderModeWeb,
		IsReaderLoggedIn: true,
	})
	if !strings.Contains(outReader.HTML, "READER_ONLY_PARAGRAPH_2") {
		t.Fatalf("expected signed-in reader to see type=user block")
	}
	if strings.Contains(outReader.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || strings.Contains(outReader.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("reader without password must NOT see type=pwd blocks")
	}

	// 3. Password-unlocked Web render reveals pwd blocks (NOT type="user"), whereas Feed mode NEVER reveals them even if Unlocked=true!
	outUnlockedWeb := eng.Render(md, Options{Mode: RenderModeWeb, Unlocked: true})
	if !strings.Contains(outUnlockedWeb.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || !strings.Contains(outUnlockedWeb.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("unlocked web mode should reveal locked blocks")
	}
	if strings.Contains(outUnlockedWeb.HTML, "READER_ONLY_PARAGRAPH_2") {
		t.Fatalf("password unlock without reader login MUST NOT reveal type=user block!")
	}
	outFeed := eng.Render(md, Options{Mode: RenderModeWeb, AuthorOrShareBypass: true})
	if !strings.Contains(outFeed.HTML, "READER_ONLY_PARAGRAPH_2") || !strings.Contains(outFeed.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") {
		t.Fatalf("AuthorOrShareBypass should reveal all lock blocks in web mode")
	}
	outFeedMode := eng.Render(md, Options{Mode: RenderModeFeed, Unlocked: true, AuthorOrShareBypass: true})
	if strings.Contains(outFeedMode.HTML, "HIDDEN_LOCKED_PARAGRAPH_1") || strings.Contains(outFeedMode.HTML, "UNCLOSED_EOF_SECRET_3") {
		t.Fatalf("RenderModeFeed must NEVER emit locked content even if Unlocked/Bypass=true!")
	}

	// 3b. Multi-lock isolation: unlocking pwd="A" reveals ONLY block A, NOT pwd="B" and NOT type="user"!
	multiMD := ":::" + "lock pwd=\"A\"\nSECRET_ALPHA\n:::\n\n:::lock pwd=\"B\"\nSECRET_BRAVO\n:::\n\n:::lock type=\"user\"\nSECRET_READER\n:::"
	outOnlyA := eng.Render(multiMD, Options{
		Mode:              RenderModeWeb,
		UnlockedPasswords: map[string]bool{"A": true},
	})
	if !strings.Contains(outOnlyA.HTML, "SECRET_ALPHA") {
		t.Fatalf("expected SECRET_ALPHA unlocked: %s", outOnlyA.HTML)
	}
	if strings.Contains(outOnlyA.HTML, "SECRET_BRAVO") || strings.Contains(outOnlyA.HTML, "SECRET_READER") {
		t.Fatalf("unlocking pwd=A leaked sibling blocks: %s", outOnlyA.HTML)
	}
	if !outOnlyA.HasUnresolvedLock {
		t.Fatalf("expected HasUnresolvedLock=true while block B and user block remain locked")
	}

	// 4. Outbound post slug extraction ignores links inside locked blocks and code fences!
	slugs := ExtractOutboundPostSlugs(md + "\n\n:::lock\n[[secret-outbound-target]]\n:::\n")
	if len(slugs) != 1 || slugs[0] != "go-sqlite-wal" {
		t.Fatalf("expected only public outbound slug 'go-sqlite-wal', got %v", slugs)
	}
	if gotPwd := ExtractFirstLockPassword(md); gotPwd != "topsecret" {
		t.Fatalf("expected ExtractFirstLockPassword='topsecret', got %q", gotPwd)
	}
}

func TestLegacyBookshelfDirectivesAndWideTable(t *testing.T) {
	eng := NewEngine()

	books := []BookCardInfo{
		{
			Slug:         "the-way-to-go",
			Title:        "Go 语言入门指南",
			Description:  "The Way to Go 中文译本与实战笔记",
			ChapterCount: 42,
		},
		{
			Slug:         "gopl-notes",
			Title:        "Go 程序设计语言导读",
			CoverURL:     "/uploads/gopl.png",
			ChapterCount: 13,
		},
	}

	// 1. Legacy single-line `:::bookshelf:::` must render bookshelf grid, NOT literal `<p>:::bookshelf:::</p>`!
	outShelf := eng.Render(":::bookshelf:::", Options{
		Mode:   RenderModeWeb,
		Locale: "zh-CN",
		ListBooks: func() []BookCardInfo {
			return books
		},
	})
	if strings.Contains(outShelf.HTML, ":::bookshelf") {
		t.Fatalf("raw :::bookshelf::: directive leaked into HTML: %s", outShelf.HTML)
	}
	if !strings.Contains(outShelf.HTML, `class="volo-embed-bookshelf"`) ||
		!strings.Contains(outShelf.HTML, `<a class="volo-book-tile" href="/books/the-way-to-go"`) ||
		!strings.Contains(outShelf.HTML, `42 章`) ||
		strings.Contains(outShelf.HTML, `开始阅读`) ||
		!strings.Contains(outShelf.HTML, `volo-embed-book__cover--fallback`) {
		t.Fatalf("unexpected bookshelf HTML: %s", outShelf.HTML)
	}
	if outShelf.WordCount < 10 {
		t.Fatalf("expected rendered bookshelf WordCount >= 10, got %d", outShelf.WordCount)
	}

	// 2. Single-line `:::book id='the-way-to-go':::` and unquoted `:::book slug=gopl-notes:::`
	outBook := eng.Render(":::book id='the-way-to-go':::\n\n:::book slug=gopl-notes:::", Options{
		Mode:   RenderModeWeb,
		Locale: "en-US",
		LookupBook: func(slug string) (BookCardInfo, bool) {
			for _, b := range books {
				if b.Slug == slug {
					return b, true
				}
			}
			return BookCardInfo{}, false
		},
	})
	if strings.Contains(outBook.HTML, ":::book") {
		t.Fatalf("raw :::book directive leaked into HTML: %s", outBook.HTML)
	}
	if !strings.Contains(outBook.HTML, "42 chapters") || !strings.Contains(outBook.HTML, `<a class="volo-embed-book" href="/books/the-way-to-go"`) {
		t.Fatalf("expected localized chapter badge & full-card anchor in book card: %s", outBook.HTML)
	}

	// 3. Wide 20-column GFM table wraps cleanly in `.volo-table-scroll`
	var cols []string
	var seps []string
	var vals []string
	for i := 1; i <= 20; i++ {
		cols = append(cols, "C")
		seps = append(seps, "---")
		vals = append(vals, "11%")
	}
	wideTableMD := "| " + strings.Join(cols, " | ") + " |\n| " + strings.Join(seps, " | ") + " |\n| " + strings.Join(vals, " | ") + " |"
	outTbl := eng.Render(wideTableMD, Options{Mode: RenderModeWeb})
	if !strings.Contains(outTbl.HTML, `<div class="volo-table-scroll"><table>`) {
		t.Fatalf("expected wide table wrapped in .volo-table-scroll, got: %s", outTbl.HTML)
	}

	// 4. Hard line breaks (<br>), literal `--flag` double-hyphens, and `<font color=#ff0000>` preservation
	techMD := "> 最新的PHP版本，需要使用参数--with-libzip系统库，并且版本需要>=1.11，[libzip官网](https://libzip.org/)\n> <font  color=#ff0000> 注意libzip编译需要cmake如果没有要先安装cmake</font>\n> libzip安装如下：\n\n安装完成之后，执行配置：\n<font color=#ff0000>注意--prefix参数为安装位置，这里为/usr/php</font>"
	outTech := eng.Render(techMD, Options{Mode: RenderModeWeb})
	if !strings.Contains(outTech.HTML, "--with-libzip") || !strings.Contains(outTech.HTML, "--prefix") || strings.Contains(outTech.HTML, "&ndash;") || strings.Contains(outTech.HTML, "–") {
		t.Fatalf("expected literal --with-libzip and --prefix without en-dash mutation, got: %s", outTech.HTML)
	}
	if !strings.Contains(outTech.HTML, `<font color="#ff0000">`) || strings.Count(outTech.HTML, "<br") < 3 {
		t.Fatalf("expected <font color=\"#ff0000\"> and hard-wrap <br> tags preserved, got: %s", outTech.HTML)
	}
}

func TestExtendedMarkdownInlineTOCMediaAndSafety(t *testing.T) {
	eng := NewEngine()

	md := strings.Join([]string{
		"[TOC]",
		"",
		"## 化学与公式",
		"",
		"水分子 H~2~O 与面积 x^2^，对比 ~~删除线~~、脚注引用[^1]、路径 `/home/~alice` 与算式 2^3 + 2^4 = 24。",
		"",
		"这是 ==单段高亮==，以及 ==跨标签 **加粗** 与 `a == b` 代码高亮==，但普通比较 a == b == c 不应误判。",
		"",
		"### 媒体内嵌测试",
		"",
		"[演示视频](/uploads/demo.mp4)",
		"",
		"[播客音频](https://cdn.example.com/audio/ep01.mp3)",
		"",
		"[B站视频](https://www.bilibili.com/video/BV1xx411c7mD)",
		"",
		"[YouTube视频](https://youtu.be/dQw4w9WgXcQ)",
		"",
		`<iframe src="https://evil.example.com/phish"></iframe>`,
		"",
		`[恶意链接](javascript:alert(1)//test.mp4)`,
		"",
		"[^1]: 脚注正文说明",
	}, "\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb, Locale: "zh-CN"})

	// 1. Inline [TOC]
	if !strings.Contains(out.HTML, `<nav class="volo-inline-toc" data-md-toc="1"`) ||
		!strings.Contains(out.HTML, `href="#化学与公式"`) ||
		!strings.Contains(out.HTML, `href="#媒体内嵌测试"`) {
		t.Fatalf("expected inline TOC rendered with heading anchors, got: %s", out.HTML)
	}

	// 2. Subscript, Superscript, Strikethrough, Footnotes, and False-Positive Guards
	if !strings.Contains(out.HTML, "H<sub>2</sub>O") || !strings.Contains(out.HTML, "x<sup>2</sup>") {
		t.Fatalf("expected H<sub>2</sub>O and x<sup>2</sup>, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, "<del>删除线</del>") {
		t.Fatalf("expected <del>删除线</del>, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, "2^3 + 2^4 = 24") {
		t.Fatalf("expected spaced arithmetic 2^3 + 2^4 untouched, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, "a == b == c") {
		t.Fatalf("expected spaced comparison a == b == c untouched, got: %s", out.HTML)
	}

	// 3. Highlight ==...== (plain and cross-tag)
	if !strings.Contains(out.HTML, "<mark>单段高亮</mark>") {
		t.Fatalf("expected <mark>单段高亮</mark>, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, "<mark>跨标签 <strong>加粗</strong> 与 <code>a == b</code> 代码高亮</mark>") {
		t.Fatalf("expected cross-tag <mark> preserving inner strong & code, got: %s", out.HTML)
	}

	// 4. Standalone media players & allowlisted video embeds
	if !strings.Contains(out.HTML, `<video controls="" preload="metadata" playsinline="" src="/uploads/demo.mp4"></video>`) &&
		!strings.Contains(out.HTML, `<video controls preload="metadata" playsinline src="/uploads/demo.mp4"></video>`) {
		t.Fatalf("expected HTML5 <video> player for standalone .mp4 link, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `src="https://cdn.example.com/audio/ep01.mp3"`) || !strings.Contains(out.HTML, "<audio") {
		t.Fatalf("expected HTML5 <audio> player for standalone .mp3 link, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `src="https://player.bilibili.com/player.html?bvid=BV1xx411c7mD&amp;autoplay=0"`) {
		t.Fatalf("expected allowlisted Bilibili iframe embed, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `src="https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"`) {
		t.Fatalf("expected allowlisted YouTube privacy-enhanced iframe embed, got: %s", out.HTML)
	}

	// 5. Security: untrusted iframe & javascript: media link must NEVER emit evil src or script!
	if strings.Contains(out.HTML, "evil.example.com") || strings.Contains(out.HTML, "javascript:") {
		t.Fatalf("untrusted iframe or javascript: link leaked through sanitizer: %s", out.HTML)
	}
}

func TestCustomAppDeepLinkAnchorSanitization(t *testing.T) {
	eng := NewEngine()
	md := strings.Join([]string{
		`<a href="huanai://open/article?id=99&amp;tab=1">Open App</a>`,
		`[Obsidian](obsidian://open?vault=Notes)`,
		`<a href="javascript://%0aalert(1)">EvilJS</a>`,
		`<a href="j&#x61;vascript://alert(1)">EntityEvilJS</a>`,
		`<a href="file:///etc/passwd">LocalFile</a>`,
		`<a href="https://example.com" data-volo-app-href="javascript://evil">SpoofAttr</a>`,
	}, "\n\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb, Locale: "zh-CN"})
	if !strings.Contains(out.HTML, `href="huanai://open/article?id=99&amp;tab=1"`) {
		t.Fatalf("expected huanai:// anchor href preserved, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `href="obsidian://open?vault=Notes"`) {
		t.Fatalf("expected obsidian:// markdown link preserved, got: %s", out.HTML)
	}
	for _, forbidden := range []string{"javascript:", "file:", "data-volo-app-href"} {
		if strings.Contains(strings.ToLower(out.HTML), forbidden) {
			t.Fatalf("forbidden scheme/attr %q leaked in output: %s", forbidden, out.HTML)
		}
	}
}

func TestCodeHighlightAutoDetectedLangAndLexerEdgeCases(t *testing.T) {
	eng := NewEngine()
	md := strings.Join([]string{
		"```",
		"server {",
		"    listen 8080;",
		"    proxy_pass http://127.0.0.1:8080;",
		"    proxy_read_timeout 500ms;",
		"}",
		"```",
		"",
		"```css",
		"a:hover { display:flex; color:#fff; margin:0 auto; }",
		"```",
		"",
		"    SELECT id, title FROM posts WHERE id > 0;",
		"",
		`<pre><code class="language-go">func main() { println(len("hi")) }</code></pre>`,
	}, "\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb, ShowLineNumbers: true})

	// 1. Unlabeled Nginx block emits data-detected-lang="nginx" (no data-lang) & keeps http:// URI intact
	if !strings.Contains(out.HTML, `<figure class="volo-codeblock" data-detected-lang="nginx">`) {
		t.Fatalf("expected auto-detected data-detected-lang=\"nginx\", got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<span class="s2">http://127.0.0.1:8080</span>`) {
		t.Fatalf("expected intact Nginx URI token, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<span class="m">500ms</span>`) {
		t.Fatalf("expected Nginx ms duration unit token, got: %s", out.HTML)
	}

	// 2. Compact CSS properties without space after colon classify value keywords & pseudo-classes accurately
	if !strings.Contains(out.HTML, `<span class="nd">hover</span>`) {
		t.Fatalf("expected CSS :hover pseudo-class as nd, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<span class="na">display</span><span class="p">:</span><span class="kc">flex</span>`) {
		t.Fatalf("expected compact CSS display:flex classified as na + kc, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<span class="mh">#fff</span>`) {
		t.Fatalf("expected CSS #fff classified as mh, got: %s", out.HTML)
	}

	// 3. 4-space indented SQL block & raw <pre><code class="language-go"> upgraded to .volo-codeblock + .chroma
	if !strings.Contains(out.HTML, `data-detected-lang="sql"`) {
		t.Fatalf("expected 4-space indented SQL block to be highlighted and auto-detected, got: %s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<figure class="volo-codeblock" data-lang="go">`) || !strings.Contains(out.HTML, `<span class="nb">len</span>`) {
		t.Fatalf("expected raw <pre><code class=\"language-go\"> upgraded with Go builtin nb, got: %s", out.HTML)
	}
}

func TestQuoteDepthIsolationAndLockFailClosed(t *testing.T) {
	eng := NewEngine()

	t.Run("top-level :::lock containing > ::: never leaks in Render, RenderSummary, or StripAllLockBlocks", func(t *testing.T) {
		md := strings.Join([]string{
			"Public intro.",
			`:::lock pwd="topsecret"`,
			"SECRET_BEFORE_QUOTE_FENCE",
			"> :::",
			"SECRET_AFTER_QUOTE_FENCE",
			":::",
			"Public outro.",
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !out.HasLockBlock || !out.HasUnresolvedLock {
			t.Fatalf("expected unresolved lock block, got HasLock=%v HasUnresolved=%v", out.HasLockBlock, out.HasUnresolvedLock)
		}
		for _, secret := range []string{"SECRET_BEFORE_QUOTE_FENCE", "SECRET_AFTER_QUOTE_FENCE"} {
			if strings.Contains(out.HTML, secret) || strings.Contains(out.PlainText, secret) {
				t.Fatalf("Render() leaked %q:\nHTML: %s\nPlain: %s", secret, out.HTML, out.PlainText)
			}
		}
		if !strings.Contains(out.HTML, "Public intro.") || !strings.Contains(out.HTML, "Public outro.") {
			t.Fatalf("Render() dropped public text:\n%s", out.HTML)
		}

		summary := eng.RenderSummary(md)
		for _, secret := range []string{"SECRET_BEFORE_QUOTE_FENCE", "SECRET_AFTER_QUOTE_FENCE"} {
			if strings.Contains(summary, secret) {
				t.Fatalf("RenderSummary() leaked %q:\n%s", secret, summary)
			}
		}
		if !strings.Contains(summary, "Public intro.") || !strings.Contains(summary, "Public outro.") {
			t.Fatalf("RenderSummary() dropped public text:\n%s", summary)
		}

		stripped := StripAllLockBlocks(md)
		for _, secret := range []string{"SECRET_BEFORE_QUOTE_FENCE", "SECRET_AFTER_QUOTE_FENCE"} {
			if strings.Contains(stripped, secret) {
				t.Fatalf("StripAllLockBlocks() leaked %q:\n%s", secret, stripped)
			}
		}
		if !strings.Contains(stripped, "Public intro.") || !strings.Contains(stripped, "Public outro.") {
			t.Fatalf("StripAllLockBlocks() dropped public text:\n%s", stripped)
		}

		// Unlocked rendering preserves inner "> :::" blockquote content cleanly.
		unlocked := eng.Render(md, Options{
			Mode:              RenderModeWeb,
			UnlockedPasswords: map[string]bool{"topsecret": true},
		})
		if !strings.Contains(unlocked.HTML, "SECRET_BEFORE_QUOTE_FENCE") || !strings.Contains(unlocked.HTML, "SECRET_AFTER_QUOTE_FENCE") {
			t.Fatalf("unlocked Render() lost inner content:\n%s", unlocked.HTML)
		}
	})

	t.Run("blockquote > :::lock terminated by unquoted outer line fails closed before outer line", func(t *testing.T) {
		md := strings.Join([]string{
			`> :::lock pwd="qpwd"`,
			"> QUOTED_SECRET_LINE_1",
			"> > DEEPLY_QUOTED_SECRET_LINE_2",
			"Unquoted public outer paragraph.",
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !out.HasLockBlock || !out.HasUnresolvedLock {
			t.Fatalf("expected unresolved lock block, got HasLock=%v HasUnresolved=%v", out.HasLockBlock, out.HasUnresolvedLock)
		}
		for _, secret := range []string{"QUOTED_SECRET_LINE_1", "DEEPLY_QUOTED_SECRET_LINE_2"} {
			if strings.Contains(out.HTML, secret) || strings.Contains(out.PlainText, secret) {
				t.Fatalf("Render() leaked blockquote lock secret %q:\n%s", secret, out.HTML)
			}
		}
		if !strings.Contains(out.HTML, "<p>Unquoted public outer paragraph.</p>") {
			t.Fatalf("expected unquoted outer paragraph outside blockquote:\n%s", out.HTML)
		}

		summary := eng.RenderSummary(md)
		if strings.Contains(summary, "QUOTED_SECRET_LINE_1") || strings.Contains(summary, "DEEPLY_QUOTED_SECRET_LINE_2") {
			t.Fatalf("RenderSummary() leaked blockquote lock secret:\n%s", summary)
		}
		if !strings.Contains(summary, "Unquoted public outer paragraph.") {
			t.Fatalf("RenderSummary() lost outer paragraph:\n%s", summary)
		}

		stripped := StripAllLockBlocks(md)
		if strings.Contains(stripped, "QUOTED_SECRET_LINE_1") || strings.Contains(stripped, "DEEPLY_QUOTED_SECRET_LINE_2") {
			t.Fatalf("StripAllLockBlocks() leaked blockquote lock secret:\n%s", stripped)
		}
		if !strings.Contains(stripped, "Unquoted public outer paragraph.") {
			t.Fatalf("StripAllLockBlocks() lost outer paragraph:\n%s", stripped)
		}
	})

	t.Run("top-level fenced code and $$ math blocks do not close prematurely on > fence", func(t *testing.T) {
		md := strings.Join([]string{
			"```markdown",
			"before quoted fence",
			"> ```",
			"after quoted fence",
			"```",
			"",
			"$$",
			"x + y",
			"> $$",
			"z = 1",
			"$$",
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if strings.Count(out.HTML, `class="volo-codeblock"`) != 1 {
			t.Fatalf("expected single code block, got:\n%s", out.HTML)
		}
		if !strings.Contains(out.HTML, "before quoted fence") || !strings.Contains(out.HTML, "after quoted fence") {
			t.Fatalf("expected lines before and after > ``` inside same code block:\n%s", out.HTML)
		}
		if strings.Count(out.HTML, `class="volo-math-block"`) != 1 {
			t.Fatalf("expected single math block, got:\n%s", out.HTML)
		}
		if !strings.Contains(out.HTML, "z = 1") {
			t.Fatalf("expected z = 1 inside math block:\n%s", out.HTML)
		}
	})

	t.Run("blockquote > ```go followed by unquoted paragraph closes code block at blockquote end", func(t *testing.T) {
		md := strings.Join([]string{
			"> ```go",
			"> x := 42",
			"> > inner > keep",
			"Unquoted paragraph after quote.",
		}, "\n")

		out := eng.Render(md, Options{Mode: RenderModeWeb})
		if !strings.Contains(out.HTML, "<blockquote>") {
			t.Fatalf("expected blockquote wrapper:\n%s", out.HTML)
		}
		if !strings.Contains(out.HTML, `data-lang="go"`) || !strings.Contains(out.PlainText, "> inner > keep") {
			t.Fatalf("expected Go code block inside blockquote preserving extra > prefix:\n%s", out.HTML)
		}
		bqClose := strings.Index(out.HTML, "</blockquote>")
		outerPar := strings.Index(out.HTML, "<p>Unquoted paragraph after quote.</p>")
		if bqClose < 0 || outerPar < bqClose {
			t.Fatalf("expected unquoted paragraph after </blockquote>:\n%s", out.HTML)
		}
	})
}

func TestBarePreCodeEntitiesMermaidAndAlignmentRawState(t *testing.T) {
	eng := NewEngine()

	md := strings.Join([]string{
		`<pre data-title="a>b"><code class="language-sh">curl "https://api.example.com?a=1&copy=2&reg=3&times=4&amp;ok=5" &amp;&amp; echo &apos;done&apos;</code></pre>`,
		"",
		`<pre><code class="language-mermaid">graph TD`,
		`  A -->|<label>| B`,
		`  B -->|&lt;step&gt;| C</code></pre>`,
		"",
		"<pre>",
		`<p align="center">`,
		"raw pre line",
		"</p>",
		"</pre>",
		"",
		"<!--",
		`<p align="center">`,
		"commented line",
		"</p>",
		"-->",
	}, "\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb})
	if strings.Contains(out.HTML, "©") || strings.Contains(out.HTML, "®") || strings.Contains(out.HTML, "×") {
		t.Fatalf("semicolon-less query params were corrupted into entities:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, `?a=1&amp;copy=2&amp;reg=3&amp;times=4&amp;ok=5`) {
		t.Fatalf("expected intact query string inside highlighted code block:\n%s", out.HTML)
	}
	if !out.NeedsMermaid || !strings.Contains(out.HTML, `<pre class="mermaid">`) || !strings.Contains(out.HTML, `&lt;label&gt;`) || !strings.Contains(out.HTML, `&lt;step&gt;`) {
		t.Fatalf("expected bare language-mermaid block upgraded to <pre class=\"mermaid\"> with escaped <label>:\n%s", out.HTML)
	}
	if strings.Contains(out.HTML, `<div align="center">`) {
		t.Fatalf("<p align=\"center\"> inside <pre> or HTML comment was wrongly promoted to <div align=\"center\">:\n%s", out.HTML)
	}

	// Stress-check linear O(L) highlight scanning on repeated unclosed `==a ==b ...`
	repeatedMarks := strings.Repeat("==word ", 4000)
	markOut := eng.Render(repeatedMarks, Options{Mode: RenderModeWeb})
	if strings.Contains(markOut.HTML, "<mark>") {
		t.Fatalf("expected no <mark> for unclosed == tokens")
	}
}

func TestExpandedLexersAndLanguageAliases(t *testing.T) {
	eng := NewEngine()

	t.Run("php splits $class as nv variable and highlights #, //, /* */ comments", func(t *testing.T) {
		phpHTML := HighlightCodeHTML("<?php\n# hash comment\n// slash comment\n/* block */\nnamespace App;\nreadonly class Demo {\n    public function run($class): void {\n        echo match ($class) { default => 'ok' };\n    }\n}\n", "php", false)
		if !strings.Contains(phpHTML, `<span class="nv">$class</span>`) {
			t.Fatalf("expected PHP $class highlighted as nv variable without splitting keyword:\n%s", phpHTML)
		}
		if !strings.Contains(phpHTML, `<span class="k">class</span>`) || !strings.Contains(phpHTML, `<span class="k">readonly</span>`) || !strings.Contains(phpHTML, `<span class="k">match</span>`) {
			t.Fatalf("expected PHP keywords highlighted:\n%s", phpHTML)
		}
		if strings.Count(phpHTML, `<span class="c1">`) < 2 || !strings.Contains(phpHTML, `<span class="cm">`) {
			t.Fatalf("expected PHP #, //, and /* */ comments highlighted:\n%s", phpHTML)
		}
	})

	t.Run("ruby, lua, graphql, proto, genericLex -- comments, and aliases", func(t *testing.T) {
		rbHTML := HighlightCodeHTML("# comment\nclass User\n  attr_accessor :role\n  def admin?\n    @role == :admin || @@default == $GLOBAL_ROLE\n  end\nend\n", "rb", false)
		if !strings.Contains(rbHTML, `<span class="ss">:role</span>`) || !strings.Contains(rbHTML, `<span class="nv">@role</span>`) || !strings.Contains(rbHTML, `<span class="nv">@@default</span>`) || !strings.Contains(rbHTML, `<span class="nv">$GLOBAL_ROLE</span>`) {
			t.Fatalf("expected Ruby symbols and @/@@/$ variables highlighted:\n%s", rbHTML)
		}

		luaHTML := HighlightCodeHTML("--[[ multi\ncomment ]]\n-- line comment\nlocal function add(a, b)\n  if a == nil then return 0 end\n  return a + b\nend\n", "lua", false)
		if !strings.Contains(luaHTML, `<span class="cm">--[[ multi`) || !strings.Contains(luaHTML, `<span class="c1">-- line comment</span>`) || !strings.Contains(luaHTML, `<span class="k">local</span>`) {
			t.Fatalf("expected Lua comments and keywords highlighted:\n%s", luaHTML)
		}

		gqlHTML := HighlightCodeHTML("# query doc\nquery GetUser($id: ID!) @cached {\n  user(id: $id) { name }\n}\n", "gql", false)
		if !strings.Contains(gqlHTML, `<span class="c1"># query doc</span>`) || !strings.Contains(gqlHTML, `<span class="nv">$id</span>`) || !strings.Contains(gqlHTML, `<span class="nd">@cached</span>`) || !strings.Contains(gqlHTML, `<span class="k">query</span>`) {
			t.Fatalf("expected GraphQL comment, $var, @directive, and query keyword highlighted:\n%s", gqlHTML)
		}

		protoHTML := HighlightCodeHTML("// proto3\nsyntax = \"proto3\";\nmessage User { int64 id = 1; repeated string tags = 2; }\nservice Api { rpc Get(User) returns (User); }\n", "protobuf", false)
		if !strings.Contains(protoHTML, `<span class="c1">// proto3</span>`) || !strings.Contains(protoHTML, `<span class="k">message</span>`) || !strings.Contains(protoHTML, `<span class="kt">int64</span>`) || !strings.Contains(protoHTML, `<span class="k">returns</span>`) {
			t.Fatalf("expected Protobuf comment, keywords, and scalar types highlighted:\n%s", protoHTML)
		}

		genHTML := HighlightCodeHTML("-- dash comment\nval x = compute(42);\n", "hs", false)
		if !strings.Contains(genHTML, `<span class="c1">-- dash comment</span>`) {
			t.Fatalf("expected -- comment highlighted in genericLex:\n%s", genHTML)
		}

		// Verify heuristic auto-detection for idiomatic Ruby and Lua
		autoMD := "```\ndef greet(name)\n  puts name\nend\n```\n\n```\nlocal function fib(n)\n  return n\nend\n```\n"
		autoOut := eng.Render(autoMD, Options{Mode: RenderModeWeb})
		if !strings.Contains(autoOut.HTML, `data-detected-lang="ruby"`) || !strings.Contains(autoOut.HTML, `data-detected-lang="lua"`) {
			t.Fatalf("expected Ruby and Lua auto-detection:\n%s", autoOut.HTML)
		}

		// Verify requested aliases resolve to non-nil lexers with matching comment styles
		for _, alias := range []string{"scss", "sass", "less", "json5", "mjs", "cjs", "mts", "cts", "zsh", "fish", "console", "shellsession", "hcl", "tf", "terraform", "prisma", "dart", "scala", "zig", "elixir", "ex", "r", "perl", "pl"} {
			if resolveCodeLexer(alias) == nil {
				t.Fatalf("expected non-nil lexer for alias %q", alias)
			}
		}
		if !strings.Contains(HighlightCodeHTML("# tf comment\nresource \"x\" \"y\" {}", "terraform", false), `<span class="c1"># tf comment</span>`) {
			t.Fatal("expected # comment in terraform")
		}
		if !strings.Contains(HighlightCodeHTML("# ex comment\n[1, 2] -- [2]", "elixir", false), `<span class="c1"># ex comment</span>`) ||
			strings.Contains(HighlightCodeHTML("[1, 2] -- [2]", "elixir", false), `class="c1"`) {
			t.Fatal("expected # comment in elixir without misclassifying -- list operator as comment")
		}
		if !strings.Contains(HighlightCodeHTML("// zig comment\nconst x = 1;", "zig", false), `<span class="c1">// zig comment</span>`) {
			t.Fatal("expected // comment in zig")
		}
	})
}
