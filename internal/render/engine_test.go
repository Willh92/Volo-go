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

