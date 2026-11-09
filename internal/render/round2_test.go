package render

import (
	"strings"
	"testing"
)

func TestMultipleUnlockedLockBlocksPreserveDistinctInnerCodePlaceholders(t *testing.T) {
	eng := NewEngine()
	md := strings.Join([]string{
		":::lock type=\"user\"",
		"First unlocked section with $a_1 + b_1$:",
		"",
		"```go",
		"const FirstBlock = \"ALPHA_DISTINCT_TOKEN\"",
		"```",
		":::",
		"",
		":::lock type=\"user\"",
		"Second unlocked section with $x_2 + y_2$:",
		"",
		"```go",
		"const SecondBlock = \"BETA_DISTINCT_TOKEN\"",
		"```",
		":::",
	}, "\n")

	out := eng.Render(md, Options{
		Mode:             RenderModeWeb,
		Locale:           "en-US",
		IsReaderLoggedIn: true,
	})
	if !strings.Contains(out.HTML, "ALPHA_DISTINCT_TOKEN") || !strings.Contains(out.HTML, "BETA_DISTINCT_TOKEN") {
		t.Fatalf("INNER placeholder collision lost unlocked lock block code content:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, "a_1 + b_1") || !strings.Contains(out.HTML, "x_2 + y_2") {
		t.Fatalf("INNER placeholder collision lost unlocked lock block math content:\n%s", out.HTML)
	}
}

func TestIsSafeImageSrcBlocksProtocolRelativeURLs(t *testing.T) {
	if isSafeImageSrc("//evil.example/tracker.png") {
		t.Fatal("expected protocol-relative //evil.example URL to be rejected")
	}
	if isSafeImageSrc(`/\evil.example/tracker.png`) {
		t.Fatal(`expected /\evil.example URL to be rejected`)
	}
	if !isSafeImageSrc("/uploads/2026/11/photo.webp") {
		t.Fatal("expected local /uploads/ path to be accepted")
	}
	if !isSafeImageSrc("https://cdn.example.com/photo.webp") {
		t.Fatal("expected https:// URL to be accepted")
	}
}

func TestRenderComment_StrictSanitizerNoRawHTMLNoFootnotesAndChroma(t *testing.T) {
	eng := NewEngine()
	commentMD := strings.Join([]string{
		`Hello **reader**! Check https://example.com/docs and footnote[^1].`,
		``,
		`<div id="volo-lightbox">spoof</div>`,
		`<input type="password" value="trap">`,
		`<audio controls><source src="javascript:alert(1)"></audio>`,
		``,
		"```go",
		`func Add(a, b int) int { return a + b }`,
		"```",
		``,
		`[^1]: hidden footnote definition`,
	}, "\n")

	got := eng.RenderComment(commentMD)
	for _, forbidden := range []string{
		`id="`,
		`volo-lightbox`,
		`<input`,
		`<button`,
		`<audio`,
		`<source`,
		`javascript:`,
		`tabindex=`,
		`data-`,
		`fn:1`,
		`fnref:1`,
	} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(forbidden)) {
			t.Fatalf("RenderComment leaked forbidden token %q in:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "nofollow") || !strings.Contains(got, "noreferrer") || !strings.Contains(got, `target="_blank"`) {
		t.Fatalf("expected external comment links to carry nofollow noreferrer and target=_blank:\n%s", got)
	}
	if !strings.Contains(got, `class="chroma"`) {
		t.Fatalf("expected Chroma syntax highlighting in RenderComment:\n%s", got)
	}

	// Direct policy defense-in-depth check if raw HTML ever reached commentSanitizer:
	san := eng.ensureCommentSanitizer().Sanitize(`<div id="volo-lightbox" data-x="1" tabindex="0"><li id="fn:1">x</li><input type="password"><button>x</button><audio controls><source src="javascript:alert(1)"></audio><span class="chroma evil-class">ok</span></div>`)
	for _, forbidden := range []string{`id=`, `<input`, `<button`, `<audio`, `<source`, `javascript:`, `tabindex`, `data-`, `evil-class`} {
		if strings.Contains(strings.ToLower(san), forbidden) {
			t.Fatalf("commentSanitizer failed to strip %q: %s", forbidden, san)
		}
	}
}

func TestAuthorSanitizer_InputCheckboxOnlyAndSafeSourceSrc(t *testing.T) {
	eng := NewEngine()
	out := eng.Render(strings.Join([]string{
		`- [x] Done task`,
		``,
		`<input type="password" name="pw">`,
		`<input type="text" value="spoof">`,
		`<audio controls src="/uploads/ep1.mp3"><source src="javascript:alert(1)"><source src="//evil.example/x.mp3"><source src="https://cdn.example.com/ep1.ogg" type="audio/ogg"></audio>`,
	}, "\n"), Options{Mode: RenderModeWeb, Locale: "en-US"})

	if strings.Contains(out.HTML, `type="password"`) || strings.Contains(out.HTML, `type="text"`) {
		t.Fatalf("author sanitizer failed to reject non-checkbox input types:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, `type="checkbox"`) {
		t.Fatalf("expected GFM task list checkbox input to remain intact:\n%s", out.HTML)
	}
	if strings.Contains(out.HTML, "javascript:") || strings.Contains(out.HTML, "evil.example") {
		t.Fatalf("author sanitizer failed to strip unsafe <source src>:\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, `src="/uploads/ep1.mp3"`) || !strings.Contains(out.HTML, `src="https://cdn.example.com/ep1.ogg"`) {
		t.Fatalf("expected safe local and https audio/source src to survive:\n%s", out.HTML)
	}
}

func TestCommonMarkIndentedBlockquoteNestedLockAndOrderedListFences(t *testing.T) {
	eng := NewEngine()

	// 1. 4-space indented code block must NOT be misparsed as :::lock directive!
	indentedCode := "Intro paragraph:\n\n    :::lock pwd=\"not-a-lock\"\n    indented literal line\n    :::\n"
	if ContainsLockDirective(indentedCode) {
		t.Fatal("4-space indented code block must NOT be detected as lock directive")
	}
	if len(ExtractAllLockPasswords(indentedCode)) != 0 {
		t.Fatalf("expected zero passwords from 4-space indented code block, got %v", ExtractAllLockPasswords(indentedCode))
	}
	indentedOut := eng.Render(indentedCode, Options{Mode: RenderModeWeb})
	if indentedOut.HasLockBlock || !strings.Contains(indentedOut.HTML, ":::lock") {
		t.Fatalf("expected 4-space indented :::lock to render as literal code block, got:\n%s", indentedOut.HTML)
	}

	// 2. Blockquote `> :::lock` MUST be caught and stripped/locked!
	quotedLock := "> :::lock pwd=\"qsecret\"\n> TOP_SECRET_QUOTED_PAYLOAD\n> :::\n\nPublic footer."
	if !ContainsLockDirective(quotedLock) {
		t.Fatal("expected blockquote `> :::lock` to be detected as lock directive")
	}
	if stripped := StripLockedMarkdownBlocks(quotedLock); strings.Contains(stripped, "TOP_SECRET_QUOTED_PAYLOAD") {
		t.Fatalf("StripLockedMarkdownBlocks leaked quoted lock payload:\n%s", stripped)
	}
	quotedOut := eng.Render(quotedLock, Options{Mode: RenderModeWeb})
	if strings.Contains(quotedOut.HTML, "TOP_SECRET_QUOTED_PAYLOAD") || !quotedOut.HasUnresolvedLock {
		t.Fatalf("Render leaked quoted lock payload:\n%s", quotedOut.HTML)
	}

	// 3. Nested `:::tip ... :::` and nested `:::lock` inside `:::lock` must NOT prematurely close outer lock!
	nestedLock := strings.Join([]string{
		`:::lock pwd="outer"`,
		`BEFORE_INNER_DIRECTIVE`,
		`:::tip`,
		`INNER_TIP_BODY`,
		`:::`,
		`AFTER_INNER_DIRECTIVE_SECRET`,
		`:::`,
		`Public visible tail.`,
	}, "\n")
	if stripped := StripAllLockBlocks(nestedLock); strings.Contains(stripped, "AFTER_INNER_DIRECTIVE_SECRET") || strings.Contains(stripped, "INNER_TIP_BODY") {
		t.Fatalf("StripAllLockBlocks prematurely closed outer :::lock on inner ::::\n%s", stripped)
	}
	lockedNestedOut := eng.Render(nestedLock, Options{Mode: RenderModeWeb})
	if strings.Contains(lockedNestedOut.HTML, "AFTER_INNER_DIRECTIVE_SECRET") || strings.Contains(lockedNestedOut.HTML, "INNER_TIP_BODY") {
		t.Fatalf("Render prematurely closed outer :::lock on inner ::::\n%s", lockedNestedOut.HTML)
	}

	// 4. Indented fenced code block inside `<ol>` must NOT break list numbering into two `<ol>` elements!
	listMD := strings.Join([]string{
		"1. First step",
		"   ```go",
		"   x := 42",
		"   ```",
		"2. Second step",
	}, "\n")
	listOut := eng.Render(listMD, Options{Mode: RenderModeWeb})
	if strings.Count(listOut.HTML, "<ol") != 1 {
		t.Fatalf("indented list code fence broke <ol> into %d lists:\n%s", strings.Count(listOut.HTML, "<ol"), listOut.HTML)
	}
	if !strings.Contains(listOut.HTML, "First step") || !strings.Contains(listOut.HTML, "Second step") {
		t.Fatalf("list items missing from rendered HTML:\n%s", listOut.HTML)
	}
}

func TestGitHubAlertsCJKH5H6HeroLCPAndMarkdownLexer(t *testing.T) {
	eng := NewEngine()
	md := strings.Join([]string{
		`![Hero Banner](/uploads/hero.webp)`,
		``,
		`> [!NOTE]`,
		`> Useful background context.`,
		``,
		`> [!WARNING]`,
		`> Watch out for breaking changes.`,
		``,
		`##### 深入理解核心存储`,
		`##### 深入理解核心存储`,
		`###### 第六级小标题`,
		``,
		`![Secondary Diagram](/uploads/diagram.webp)`,
		``,
		"```markdown",
		"- list item one",
		"- list item two",
		"```",
	}, "\n")

	out := eng.Render(md, Options{Mode: RenderModeWeb, Locale: "en-US"})

	if !strings.Contains(out.HTML, `volo-callout--note`) || !strings.Contains(out.HTML, `volo-callout--warning`) {
		t.Fatalf("expected GitHub alerts to transform into .volo-callout blocks:\n%s", out.HTML)
	}
	if strings.Contains(out.HTML, `id="heading"`) {
		t.Fatalf("expected CJK h5/h6 headings not to fall back to generic id=\"heading\":\n%s", out.HTML)
	}
	if !strings.Contains(out.HTML, `<h5 id="深入理解核心存储">`) || !strings.Contains(out.HTML, `<h5 id="深入理解核心存储-2">`) || !strings.Contains(out.HTML, `<h6 id="第六级小标题">`) {
		t.Fatalf("expected unique readable IDs on CJK h5/h6 headings:\n%s", out.HTML)
	}
	// First prose image must omit loading="lazy" while keeping decoding="async"; second image must have loading="lazy".
	heroIdx := strings.Index(out.HTML, `/uploads/hero.webp`)
	diagIdx := strings.Index(out.HTML, `/uploads/diagram.webp`)
	if heroIdx < 0 || diagIdx < 0 {
		t.Fatalf("missing prose images in output:\n%s", out.HTML)
	}
	heroTag := out.HTML[strings.LastIndex(out.HTML[:heroIdx], "<img") : heroIdx+strings.Index(out.HTML[heroIdx:], ">")+1]
	diagTag := out.HTML[strings.LastIndex(out.HTML[:diagIdx], "<img") : diagIdx+strings.Index(out.HTML[diagIdx:], ">")+1]
	if strings.Contains(heroTag, `loading="lazy"`) || !strings.Contains(heroTag, `decoding="async"`) {
		t.Fatalf("expected first prose image to skip loading=lazy and include decoding=async, got %s", heroTag)
	}
	if !strings.Contains(diagTag, `loading="lazy"`) || !strings.Contains(diagTag, `decoding="async"`) {
		t.Fatalf("expected second prose image to include loading=lazy and decoding=async, got %s", diagTag)
	}
	// Markdown `- list item` must NOT be highlighted as deleted diff line (`class="gd"`).
	if strings.Contains(out.HTML, `class="gd"`) {
		t.Fatalf("markdown code block `- list item` was mis-highlighted as diff deletion:\n%s", out.HTML)
	}
}
